package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CSTAM-well-contained/monorepo/internal/ipam"
	"github.com/CSTAM-well-contained/monorepo/internal/store"
)

// Controller drives teams through their states:
//
//	provisioning -> ready -> deleting -> deleted
//	provisioning -> failed -> deleting -> deleted
//
// The database is the truth. Workers run in this process; the reconciler picks up any team
// left in a transitional state (after a crash, a cloud error, or a restart).
type Controller struct {
	db   *pgxpool.Pool
	ipam *ipam.IPAM
	sbx  Sandboxes
	log  *slog.Logger
	base context.Context // lifetime of the workers

	CreateTimeout time.Duration // one sandbox creation (default 10 min)
	DeleteTimeout time.Duration // one sandbox deletion (default 5 min)
	// FailedRetention: failed teams stay visible this long, then are deleted (default 10 min).
	FailedRetention time.Duration

	sem     chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
	busy    map[int64]bool // teams with a worker running
	changed chan struct{}  // closed and replaced on every routes change
}

// NewController returns a controller whose workers stop when ctx ends. workers bounds the
// sandbox operations in flight (the cloud API and the instance quota are small).
func NewController(ctx context.Context, db *pgxpool.Pool, m *ipam.IPAM, sbx Sandboxes, log *slog.Logger, workers int) *Controller {
	if workers < 1 {
		workers = 4
	}
	return &Controller{
		db: db, ipam: m, sbx: sbx, log: log, base: ctx,
		CreateTimeout: 10 * time.Minute, DeleteTimeout: 5 * time.Minute, FailedRetention: 10 * time.Minute,
		sem: make(chan struct{}, workers), busy: map[int64]bool{}, changed: make(chan struct{}),
	}
}

// Wait blocks until every worker started so far has returned.
func (c *Controller) Wait() { c.wg.Wait() }

func (c *Controller) notify() {
	c.mu.Lock()
	close(c.changed)
	c.changed = make(chan struct{})
	c.mu.Unlock()
}

func (c *Controller) changes() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changed
}

// spawn runs fn for team id in a worker, unless a worker already handles that team.
func (c *Controller) spawn(id int64, fn func(context.Context, int64)) {
	if c.base.Err() != nil {
		return
	}
	c.mu.Lock()
	if c.busy[id] {
		c.mu.Unlock()
		return
	}
	c.busy[id] = true
	c.mu.Unlock()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() {
			c.mu.Lock()
			delete(c.busy, id)
			c.mu.Unlock()
		}()
		select {
		case c.sem <- struct{}{}:
		case <-c.base.Done():
			return
		}
		defer func() { <-c.sem }()
		fn(c.base, id)
	}()
}

// CreateTeam registers a team, leases its IP and slot, and starts provisioning.
// It returns at once with the team in state provisioning.
func (c *Controller) CreateTeam(ctx context.Context, hackathon, name string, appPort int, image string) (Team, error) {
	if appPort == 0 {
		appPort = 80
	}
	if err := validTeam(hackathon, name, appPort, image); err != nil {
		return Team{}, err
	}
	var id int64
	err := store.InTx(ctx, c.db, func(tx pgx.Tx) error {
		var hid int64
		var state string
		err := tx.QueryRow(ctx, `SELECT id, state FROM hackathons WHERE name = $1 FOR SHARE`, hackathon).Scan(&hid, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("hackathon %s: %w", hackathon, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("create team: %w", err)
		}
		if state != "active" {
			return fmt.Errorf("hackathon %s: %w", hackathon, ErrNotActive)
		}
		err = tx.QueryRow(ctx,
			`INSERT INTO teams (hackathon_id, name, state, app_port, image, lease_expires_at)
			 SELECT $1, $2, 'provisioning', $3, $4, now() + lease_ttl FROM hackathons WHERE id = $1
			 RETURNING id`, hid, name, appPort, image).Scan(&id)
		if pgCode(err) == "23505" {
			return fmt.Errorf("team %s in %s: %w", name, hackathon, ErrConflict)
		}
		if err != nil {
			return fmt.Errorf("create team: %w", err)
		}
		if _, err := c.ipam.Allocate(ctx, tx, id); err != nil {
			return err
		}
		return c.event(ctx, tx, "team.created", id, map[string]any{"hackathon": hackathon, "team": name})
	})
	if err != nil {
		return Team{}, err
	}
	c.spawn(id, c.provision)
	return c.Team(ctx, id)
}

func specOf(t Team) Spec {
	ip, _ := netip.ParseAddr(t.IP) // empty after release: only Name is used then
	return Spec{Hackathon: t.Hackathon, Team: t.Name, IP: ip, AppPort: t.AppPort, Image: t.Image}
}

func refOf(t Team) Ref {
	if r, err := ParseRef(t.ref); err == nil {
		return r
	}
	return Ref{Name: specOf(t).Name()}
}

// provision creates the sandbox of a team in state provisioning and publishes its route.
func (c *Controller) provision(ctx context.Context, id int64) {
	t, err := c.Team(ctx, id)
	if err != nil || t.State != "provisioning" {
		return
	}
	spec := specOf(t)
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, c.CreateTimeout)
	ref, err := c.sbx.Create(cctx, spec)
	cancel()
	if ref.Name == "" {
		ref.Name = spec.Name()
	}
	if err != nil {
		c.fail(ctx, id, ref, err)
		return
	}
	published := false
	err = store.InTx(ctx, c.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE teams SET state = 'ready', sandbox_ref = $2, error = '', updated_at = now()
			 WHERE id = $1 AND state = 'provisioning'`, id, ref.String())
		if err != nil {
			return err
		}
		published = tag.RowsAffected() == 1
		if !published { // deleted while we were creating: keep the ref for the teardown
			_, err = tx.Exec(ctx, `UPDATE teams SET sandbox_ref = $2 WHERE id = $1`, id, ref.String())
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE routes_generation SET gen = gen + 1`); err != nil {
			return err
		}
		return c.event(ctx, tx, "team.ready", id, map[string]any{"seconds": time.Since(start).Seconds()})
	})
	if err != nil {
		c.log.Error("publish team", "team", id, "err", err) // still provisioning: the reconciler retries
		return
	}
	if published {
		c.notify()
		c.log.Info("team ready", "team", id, "name", ref.Name, "ip", t.IP, "took", time.Since(start).String())
		return
	}
	c.teardown(ctx, id)
}

// fail marks a team failed and releases its IP; the sandbox is cleaned up afterwards.
func (c *Controller) fail(ctx context.Context, id int64, ref Ref, cause error) {
	msg := cause.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	c.log.Warn("provisioning failed", "team", id, "err", cause)
	err := store.InTx(ctx, c.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE teams SET state = 'failed', error = $2, sandbox_ref = $3, updated_at = now()
			 WHERE id = $1 AND state = 'provisioning'`, id, msg, ref.String())
		if err != nil || tag.RowsAffected() == 0 {
			return err // already deleting: the teardown does the rest
		}
		// The IP goes to quarantine. If the cloud said it is taken, the sweep's in-use
		// check keeps it there until whoever holds it lets go.
		if _, err := c.ipam.Release(ctx, tx, id); err != nil {
			return err
		}
		return c.event(ctx, tx, "team.failed", id, map[string]any{"error": msg})
	})
	if err != nil {
		c.log.Error("mark team failed", "team", id, "err", err)
		return
	}
	c.notify()
	c.cleanupFailed(ctx, id)
}

// cleanupFailed deletes what a failed creation may have left in the cloud.
func (c *Controller) cleanupFailed(ctx context.Context, id int64) {
	t, err := c.Team(ctx, id)
	if err != nil || t.State != "failed" || t.ref == "" {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, c.DeleteTimeout)
	defer cancel()
	if err := c.sbx.Delete(dctx, refOf(t)); err != nil {
		c.log.Warn("clean up failed team", "team", id, "err", err)
		return
	}
	if _, err := c.db.Exec(ctx, `UPDATE teams SET sandbox_ref = '' WHERE id = $1 AND state = 'failed'`, id); err != nil {
		c.log.Error("clean up failed team", "team", id, "err", err)
	}
}

// DeleteTeam takes the team's route out, quarantines its IP and starts the teardown.
// Deleting a team that is already deleting or deleted succeeds.
func (c *Controller) DeleteTeam(ctx context.Context, id int64) error {
	err := store.InTx(ctx, c.db, func(tx pgx.Tx) error {
		var state string
		err := tx.QueryRow(ctx, `SELECT state FROM teams WHERE id = $1 FOR UPDATE`, id).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("team %d: %w", id, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("delete team: %w", err)
		}
		if state == "deleting" || state == "deleted" {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE teams SET state = 'deleting', updated_at = now() WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete team: %w", err)
		}
		if _, err := c.ipam.Release(ctx, tx, id); err != nil {
			return err
		}
		return c.event(ctx, tx, "team.deleting", id, map[string]any{"from": state})
	})
	if err != nil {
		return err
	}
	c.notify()
	c.spawn(id, c.teardown)
	return nil
}

// teardown deletes the sandbox of a team in state deleting, then marks it deleted.
func (c *Controller) teardown(ctx context.Context, id int64) {
	t, err := c.Team(ctx, id)
	if err != nil || t.State != "deleting" {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, c.DeleteTimeout)
	err = c.sbx.Delete(dctx, refOf(t))
	cancel()
	if err != nil {
		c.log.Warn("teardown", "team", id, "err", err) // the reconciler retries
		return
	}
	err = store.InTx(ctx, c.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE teams SET state = 'deleted', updated_at = now() WHERE id = $1 AND state = 'deleting'`, id)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return c.event(ctx, tx, "team.deleted", id, nil)
	})
	if err != nil {
		c.log.Error("mark team deleted", "team", id, "err", err)
	}
}

// EndHackathon stops new registrations and tears down every team. The reconciler marks
// the hackathon ended once no team is left. Ending twice succeeds.
func (c *Controller) EndHackathon(ctx context.Context, name string) error {
	h, err := c.Hackathon(ctx, name)
	if err != nil {
		return err
	}
	if _, err := c.db.Exec(ctx, `UPDATE hackathons SET state = 'ending' WHERE id = $1 AND state = 'active'`, h.ID); err != nil {
		return fmt.Errorf("end hackathon: %w", err)
	}
	if err := c.event(ctx, c.db, "hackathon.ending", 0, map[string]any{"hackathon": name}); err != nil {
		return err
	}
	return c.deleteWhere(ctx, `hackathon_id = $1 AND state IN ('provisioning', 'ready', 'failed')`, h.ID)
}

func (c *Controller) deleteWhere(ctx context.Context, where string, args ...any) error {
	rows, err := c.db.Query(ctx, `SELECT id FROM teams WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return fmt.Errorf("select teams: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return fmt.Errorf("select teams: %w", err)
	}
	var errs []error
	for _, id := range ids {
		if err := c.DeleteTeam(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Renew extends a team's lease by its hackathon's lease TTL.
func (c *Controller) Renew(ctx context.Context, id int64) (time.Time, error) {
	until, err := c.ipam.Renew(ctx, c.db, id)
	if errors.Is(err, ipam.ErrLeaseExpired) {
		if _, gerr := c.Team(ctx, id); gerr != nil {
			return time.Time{}, gerr
		}
	}
	return until, err
}

// Reconcile runs one pass: expired leases and old failures are torn down, teams stuck in
// a transitional state are driven again, safe quarantined IPs are freed, and finished
// hackathons are closed.
func (c *Controller) Reconcile(ctx context.Context) error {
	var errs []error
	expired, err := c.ipam.Expired(ctx, c.db, 100)
	errs = append(errs, err)
	for _, id := range expired {
		c.log.Info("lease expired", "team", id)
		errs = append(errs, c.DeleteTeam(ctx, id))
	}
	errs = append(errs, c.deleteWhere(ctx,
		`state = 'failed' AND sandbox_ref = '' AND updated_at < now() - $1 * interval '1 millisecond'`,
		c.FailedRetention.Milliseconds()))

	rows, err := c.db.Query(ctx,
		`SELECT id, state FROM teams
		 WHERE state IN ('provisioning', 'deleting') OR (state = 'failed' AND sandbox_ref <> '') ORDER BY id`)
	if err == nil {
		type pending struct {
			ID    int64
			State string
		}
		var ps []pending
		ps, err = pgx.CollectRows(rows, pgx.RowToStructByPos[pending])
		for _, p := range ps {
			switch p.State {
			case "provisioning":
				c.spawn(p.ID, c.provision)
			case "deleting":
				c.spawn(p.ID, c.teardown)
			case "failed":
				c.spawn(p.ID, c.cleanupFailed)
			}
		}
	}
	errs = append(errs, err)

	freed, err := c.ipam.Sweep(ctx, c.db, c.sbx.InUse)
	errs = append(errs, err)
	if len(freed) > 0 {
		c.log.Info("ips freed", "count", len(freed))
	}
	_, err = c.db.Exec(ctx,
		`UPDATE hackathons h SET state = 'ended', ended_at = now()
		 WHERE state = 'ending' AND NOT EXISTS (SELECT 1 FROM teams t WHERE t.hackathon_id = h.id AND t.state <> 'deleted')`)
	errs = append(errs, err)
	return errors.Join(errs...)
}

// Run reconciles every interval until ctx ends.
func (c *Controller) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := c.Reconcile(ctx); err != nil && ctx.Err() == nil {
			c.log.Error("reconcile", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
