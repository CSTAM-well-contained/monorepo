package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/CSTAM-well-contained/monorepo/internal/config"
	"github.com/CSTAM-well-contained/monorepo/internal/ipam"
	"github.com/CSTAM-well-contained/monorepo/internal/model"
	"github.com/CSTAM-well-contained/monorepo/internal/store"
)

// Errors of the controller, mapped to HTTP statuses by internal/api.
var (
	ErrNotFound  = errors.New("not found")
	ErrConflict  = errors.New("already exists")
	ErrNotActive = errors.New("hackathon is not active")
)

var (
	domainRE  = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}$`)
	versionRE = regexp.MustCompile(`^[A-Za-z0-9._-]{0,32}$`)
	vrrpRE    = regexp.MustCompile(`^(|MASTER|BACKUP|FAULT|STOP)$`)
	outcomeRE = regexp.MustCompile(`^(|applied|rejected|rolled_back|failed)$`)
)

// Hackathon is one event; its teams are published under its domain.
type Hackathon struct {
	ID              int64     `json:"id"`
	Name            string    `json:"name"`
	Domain          string    `json:"domain"`
	State           string    `json:"state"`
	LeaseTTLSeconds int64     `json:"lease_ttl_seconds"`
	CreatedAt       time.Time `json:"created_at"`
}

// Team is one sandbox with one private IP, published at FQDN when ready.
type Team struct {
	ID             int64     `json:"id"`
	Hackathon      string    `json:"hackathon"`
	Name           string    `json:"name"`
	FQDN           string    `json:"fqdn"`
	State          string    `json:"state"`
	IP             string    `json:"ip,omitempty"`
	Slot           int       `json:"slot,omitempty"`
	AppPort        int       `json:"app_port"`
	Image          string    `json:"image,omitempty"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	Error          string    `json:"error,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	ref            string
}

// Gateway is what cpd knows about one gateway agent.
type Gateway struct {
	Name          string     `json:"name"`
	AckedGen      int64      `json:"acked_generation"`
	ConfigVersion string     `json:"config_version"`
	VRRPState     string     `json:"vrrp_state"`
	VRRPSince     *time.Time `json:"vrrp_since,omitempty"`
	LastSeen      time.Time  `json:"last_seen"`
	Alive         bool       `json:"alive"`   // acked within GatewayAlive
	InSync        bool       `json:"in_sync"` // serves the current generation
	ForceBackup   bool       `json:"force_backup"`
	ConfigTried   string     `json:"config_tried"`   // last config version this gateway tried
	ConfigOutcome string     `json:"config_outcome"` // applied, rejected, rolled_back, failed
	ConfigError   string     `json:"config_error,omitempty"`
}

// Status is the platform overview (dashboard, demo).
type Status struct {
	Generation int64          `json:"generation"`
	Pool       map[string]int `json:"ip_pool"`
	Teams      map[string]int `json:"teams"`
	Gateways   []Gateway      `json:"gateways"`
	Alerts     []string       `json:"alerts"` // split brain, no master, gateway behind or silent
	Config     string         `json:"config_version"` // newest config version, "" before the first push
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// CreateHackathon registers an event. ttl 0 means 2 hours.
func (c *Controller) CreateHackathon(ctx context.Context, name, domain string, ttl time.Duration) (Hackathon, error) {
	domain = strings.ToLower(domain)
	if ttl == 0 {
		ttl = 2 * time.Hour
	}
	var errs []error
	if !labelRE.MatchString(name) {
		errs = append(errs, fmt.Errorf("name %q is not a dns label", name))
	}
	if len(domain) > 200 || !domainRE.MatchString(domain) {
		errs = append(errs, fmt.Errorf("domain %q is not a dns name", domain))
	}
	if ttl < 10*time.Second || ttl > 30*24*time.Hour {
		errs = append(errs, fmt.Errorf("lease ttl %s outside 10s..720h", ttl))
	}
	if len(errs) > 0 {
		return Hackathon{}, fmt.Errorf("%w: %w", ErrInvalidSpec, errors.Join(errs...))
	}
	h := Hackathon{Name: name, Domain: domain, LeaseTTLSeconds: int64(ttl / time.Second)}
	err := c.db.QueryRow(ctx,
		`INSERT INTO hackathons (name, domain, lease_ttl) VALUES ($1, $2, $3 * interval '1 second')
		 RETURNING id, state, created_at`, name, domain, h.LeaseTTLSeconds).Scan(&h.ID, &h.State, &h.CreatedAt)
	switch {
	case pgCode(err) == "23505":
		return Hackathon{}, fmt.Errorf("hackathon %s: %w", name, ErrConflict)
	case err != nil:
		return Hackathon{}, fmt.Errorf("create hackathon: %w", err)
	}
	return h, c.event(ctx, c.db, "hackathon.created", 0, map[string]any{"hackathon": name})
}

const hackathonSelect = `SELECT id, name, domain, state, extract(epoch FROM lease_ttl)::bigint, created_at FROM hackathons`

func scanHackathon(row pgx.CollectableRow) (Hackathon, error) {
	var h Hackathon
	err := row.Scan(&h.ID, &h.Name, &h.Domain, &h.State, &h.LeaseTTLSeconds, &h.CreatedAt)
	return h, err
}

// Hackathons lists every event, newest first.
func (c *Controller) Hackathons(ctx context.Context) ([]Hackathon, error) {
	rows, err := c.db.Query(ctx, hackathonSelect+` ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list hackathons: %w", err)
	}
	return pgx.CollectRows(rows, scanHackathon)
}

// Hackathon returns one event by name.
func (c *Controller) Hackathon(ctx context.Context, name string) (Hackathon, error) {
	rows, err := c.db.Query(ctx, hackathonSelect+` WHERE name = $1`, name)
	if err != nil {
		return Hackathon{}, fmt.Errorf("get hackathon: %w", err)
	}
	h, err := pgx.CollectExactlyOneRow(rows, scanHackathon)
	if errors.Is(err, pgx.ErrNoRows) {
		return Hackathon{}, fmt.Errorf("hackathon %s: %w", name, ErrNotFound)
	}
	return h, err
}

const teamSelect = `SELECT t.id, h.name, h.domain, t.name, t.state, coalesce(host(t.ip), ''), coalesce(t.slot, 0),
	t.app_port, t.image, t.lease_expires_at, t.error, t.created_at, t.sandbox_ref
	FROM teams t JOIN hackathons h ON h.id = t.hackathon_id`

func scanTeam(row pgx.CollectableRow) (Team, error) {
	var t Team
	var domain string
	err := row.Scan(&t.ID, &t.Hackathon, &domain, &t.Name, &t.State, &t.IP, &t.Slot,
		&t.AppPort, &t.Image, &t.LeaseExpiresAt, &t.Error, &t.CreatedAt, &t.ref)
	t.FQDN = t.Name + "." + domain
	return t, err
}

// Team returns one team, deleted ones included.
func (c *Controller) Team(ctx context.Context, id int64) (Team, error) {
	rows, err := c.db.Query(ctx, teamSelect+` WHERE t.id = $1`, id)
	if err != nil {
		return Team{}, fmt.Errorf("get team: %w", err)
	}
	t, err := pgx.CollectExactlyOneRow(rows, scanTeam)
	if errors.Is(err, pgx.ErrNoRows) {
		return Team{}, fmt.Errorf("team %d: %w", id, ErrNotFound)
	}
	return t, err
}

// Teams lists the teams of a hackathon that are not deleted, in creation order.
func (c *Controller) Teams(ctx context.Context, hackathon string) ([]Team, error) {
	if _, err := c.Hackathon(ctx, hackathon); err != nil {
		return nil, err
	}
	rows, err := c.db.Query(ctx, teamSelect+` WHERE h.name = $1 AND t.state <> 'deleted' ORDER BY t.id`, hackathon)
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	return pgx.CollectRows(rows, scanTeam)
}

// Desired returns the routing table of the gateways. If the generation equals after, it
// waits up to wait for a change (long-poll) and returns the table as it is then.
func (c *Controller) Desired(ctx context.Context, after int64, wait time.Duration) (model.Desired, error) {
	deadline := time.Now().Add(wait)
	for {
		changed := c.changes()
		d, err := c.desired(ctx)
		if err != nil || d.Generation != after || !time.Now().Before(deadline) {
			return d, err
		}
		// The in-process signal wakes us at once; the timer also catches other writers.
		t := time.NewTimer(min(time.Until(deadline), time.Second))
		select {
		case <-changed:
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return d, fmt.Errorf("desired: %w", ctx.Err())
		}
		t.Stop()
	}
}

func (c *Controller) desired(ctx context.Context) (model.Desired, error) {
	d := model.Desired{Routes: []model.Route{}}
	err := pgx.BeginTxFunc(ctx, c.db, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT gen FROM routes_generation`).Scan(&d.Generation); err != nil {
				return err
			}
			rows, err := tx.Query(ctx,
				`SELECT t.name || '.' || h.domain, t.slot, host(t.ip), t.app_port
				 FROM teams t JOIN hackathons h ON h.id = t.hackathon_id
				 WHERE t.state = 'ready' ORDER BY t.slot`)
			if err != nil {
				return err
			}
			d.Routes, err = pgx.CollectRows(rows, pgx.RowToStructByPos[model.Route])
			if err != nil {
				return err
			}
			rows, err = tx.Query(ctx, `SELECT name FROM gateways WHERE force_backup ORDER BY name`)
			if err != nil {
				return err
			}
			d.ForceBackup, err = pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			d.Config, err = currentConfig(ctx, tx)
			return err
		})
	if err != nil {
		return model.Desired{}, fmt.Errorf("desired state: %w", err)
	}
	return d, nil
}

// Ack records what a gateway applied. A generation above the current one is refused:
// it would let the IPAM free addresses that some gateway may still route to.
func (c *Controller) Ack(ctx context.Context, gateway string, a model.Ack) error {
	if !labelRE.MatchString(gateway) || !versionRE.MatchString(a.ConfigVersion) || !versionRE.MatchString(a.ConfigTried) ||
		!vrrpRE.MatchString(a.VRRPState) || !outcomeRE.MatchString(a.ConfigOutcome) || a.Generation < 0 {
		return fmt.Errorf("%w: bad gateway name, config version, config outcome or vrrp state", ErrInvalidSpec)
	}
	a.ConfigError = cleanAckError(a.ConfigError)
	return store.InTx(ctx, c.db, func(tx pgx.Tx) error {
		var gen int64
		if err := tx.QueryRow(ctx, `SELECT gen FROM routes_generation`).Scan(&gen); err != nil {
			return fmt.Errorf("ack: %w", err)
		}
		if a.Generation > gen {
			return fmt.Errorf("%w: generation %d is ahead of %d", ErrInvalidSpec, a.Generation, gen)
		}
		var old *string
		var oldTried, oldOutcome string
		err := tx.QueryRow(ctx, `SELECT vrrp_state, config_tried, config_outcome FROM gateways WHERE name = $1 FOR UPDATE`,
			gateway).Scan(&old, &oldTried, &oldOutcome)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("ack: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO gateways (name, acked_gen, config_version, vrrp_state, vrrp_since, last_seen,
			   config_tried, config_outcome, config_error)
			 VALUES ($1, $2, $3, $4, now(), now(), $5, $6, $7)
			 ON CONFLICT (name) DO UPDATE SET acked_gen = EXCLUDED.acked_gen,
			   config_version = EXCLUDED.config_version, vrrp_state = EXCLUDED.vrrp_state,
			   vrrp_since = CASE WHEN gateways.vrrp_state = EXCLUDED.vrrp_state THEN gateways.vrrp_since
			                ELSE now() END,
			   last_seen = now(), config_tried = EXCLUDED.config_tried,
			   config_outcome = EXCLUDED.config_outcome, config_error = EXCLUDED.config_error`,
			gateway, a.Generation, a.ConfigVersion, a.VRRPState, a.ConfigTried, a.ConfigOutcome, a.ConfigError); err != nil {
			return fmt.Errorf("ack: %w", err)
		}
		if a.ConfigOutcome != "" && (a.ConfigTried != oldTried || a.ConfigOutcome != oldOutcome) {
			if err := c.event(ctx, tx, "config."+a.ConfigOutcome, 0, map[string]any{"gateway": gateway,
				"version": a.ConfigTried, "serving": a.ConfigVersion, "error": a.ConfigError}); err != nil {
				return err
			}
		}
		if old == nil || *old != a.VRRPState {
			return c.event(ctx, tx, "gateway.vrrp", 0, map[string]any{"gateway": gateway, "state": a.VRRPState})
		}
		return nil
	})
}

// Status returns the generation, the IP pool and team counts, and the gateways.
func (c *Controller) Status(ctx context.Context) (Status, error) {
	var s Status
	if err := c.db.QueryRow(ctx, `SELECT gen FROM routes_generation`).Scan(&s.Generation); err != nil {
		return s, fmt.Errorf("status: %w", err)
	}
	var err error
	if s.Pool, err = c.ipam.Counts(ctx, c.db); err != nil {
		return s, err
	}
	s.Teams = map[string]int{}
	rows, err := c.db.Query(ctx, `SELECT state, count(*)::int FROM teams GROUP BY state`)
	if err != nil {
		return s, fmt.Errorf("status: %w", err)
	}
	type count struct {
		State string
		N     int
	}
	counts, err := pgx.CollectRows(rows, pgx.RowToStructByPos[count])
	if err != nil {
		return s, fmt.Errorf("status: %w", err)
	}
	for _, n := range counts {
		s.Teams[n.State] = n.N
	}
	rows, err = c.db.Query(ctx,
		`SELECT name, acked_gen, config_version, vrrp_state, vrrp_since, last_seen,
		   last_seen > now() - $1 * interval '1 millisecond', acked_gen >= $2, force_backup,
		   config_tried, config_outcome, config_error
		 FROM gateways ORDER BY name`, GatewayAlive.Milliseconds(), s.Generation)
	if err != nil {
		return s, fmt.Errorf("status: %w", err)
	}
	s.Gateways, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Gateway])
	if err != nil {
		return s, fmt.Errorf("status: %w", err)
	}
	var cfgID *int64
	if err := c.db.QueryRow(ctx, `SELECT max(id) FROM config_versions`).Scan(&cfgID); err != nil {
		return s, fmt.Errorf("status: %w", err)
	}
	if cfgID != nil {
		s.Config = versionName(*cfgID)
	}
	s.Alerts = alerts(s.Generation, s.Config, s.Gateways)
	return s, nil
}

func (c *Controller) event(ctx context.Context, db ipam.DB, kind string, teamID int64, detail map[string]any) error {
	var team *int64
	if teamID != 0 {
		team = &teamID
	}
	if detail == nil {
		detail = map[string]any{}
	}
	if _, err := db.Exec(ctx, `INSERT INTO events (kind, team_id, detail) VALUES ($1, $2, $3)`, kind, team, detail); err != nil {
		return fmt.Errorf("event %s: %w", kind, err)
	}
	return nil
}

// validTeam checks the user input of a new team before anything is written.
func validTeam(hackathon, name string, appPort int, image string) error {
	// Any team-pool address passes; the real one comes from the IPAM.
	return Spec{Hackathon: hackathon, Team: name, IP: config.TeamPoolLo, AppPort: appPort, Image: image}.Validate()
}
