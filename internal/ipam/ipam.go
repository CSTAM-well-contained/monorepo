// Package ipam leases the private IPs of team sandboxes and recycles them safely.
//
// An address moves free -> allocated -> quarantine -> free. Allocation and release run in
// the caller's transaction, so the team row and its address always change together.
// A released address is only reused once every live gateway has acknowledged a routes
// generation that no longer contains it (see docs/guide/chapters/17-step8-teardown.md).
package ipam

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/CSTAM-well-contained/monorepo/internal/config"
)

// Errors returned by the IPAM.
var (
	ErrPoolExhausted = errors.New("ipam: no free IP or slot left")
	ErrTeamNotFound  = errors.New("ipam: team not found, deleted or already holding an IP")
	ErrStillRouted   = errors.New("ipam: team is still published (state ready), remove its route first")
	ErrLeaseExpired  = errors.New("ipam: lease expired or team not active")
)

// DB is what the IPAM needs from a pgx pool or transaction.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// IPAM holds the pool bounds and the recycling rules.
type IPAM struct {
	Lo, Hi   netip.Addr   // team pool, inclusive
	Reserved []netip.Addr // addresses inside the pool that are never leased
	Slots    int          // HAProxy slot backends be_slot001..be_slotN
	// Quarantine is the minimum time between release and reuse (ARP caches, keep-alive).
	Quarantine time.Duration
	// GatewayWindow: gateways seen within this window must have acked the release.
	GatewayWindow time.Duration
}

// New returns the IPAM of the platform topology (internal/config).
func New() *IPAM {
	return &IPAM{
		Lo:            config.TeamPoolLo,
		Hi:            config.TeamPoolHi,
		Reserved:      config.ReservedIPs,
		Slots:         128,
		Quarantine:    60 * time.Second,
		GatewayWindow: 5 * time.Minute,
	}
}

// Lease is what a team gets from Allocate.
type Lease struct {
	IP   netip.Addr
	Slot int
}

// Seed inserts every pool address (idempotent) and applies the reserved list: listed free
// addresses become reserved, reserved addresses no longer listed become free.
func (m *IPAM) Seed(ctx context.Context, db DB) error {
	if !m.Lo.Is4() || !m.Hi.Is4() || m.Hi.Less(m.Lo) {
		return fmt.Errorf("ipam: bad pool %s-%s", m.Lo, m.Hi)
	}
	var all []string
	for ip := m.Lo; ip.Compare(m.Hi) <= 0; ip = ip.Next() {
		all = append(all, ip.String())
	}
	reserved := make([]string, 0, len(m.Reserved))
	for _, ip := range m.Reserved {
		reserved = append(reserved, ip.String())
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO ip_pool (ip, state) SELECT unnest($1::text[])::inet, 'free' ON CONFLICT (ip) DO NOTHING`,
		all); err != nil {
		return fmt.Errorf("ipam: seed pool: %w", err)
	}
	if _, err := db.Exec(ctx,
		`UPDATE ip_pool SET state = 'reserved', updated_at = now()
		 WHERE state = 'free' AND ip = ANY($1::text[]::inet[])`, reserved); err != nil {
		return fmt.Errorf("ipam: reserve: %w", err)
	}
	if _, err := db.Exec(ctx,
		`UPDATE ip_pool SET state = 'free', updated_at = now()
		 WHERE state = 'reserved' AND NOT ip = ANY($1::text[]::inet[])`, reserved); err != nil {
		return fmt.Errorf("ipam: unreserve: %w", err)
	}
	return nil
}

// Allocate gives the lowest free IP and the lowest free slot to a team that has neither.
// It must run in the transaction that created the team row.
func (m *IPAM) Allocate(ctx context.Context, tx pgx.Tx, teamID int64) (Lease, error) {
	// One allocation at a time: the slot is chosen from committed rows, so two concurrent
	// transactions could otherwise pick the same slot. The lock is released at commit.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('cstam.ipam.allocate'))`); err != nil {
		return Lease{}, fmt.Errorf("ipam: lock: %w", err)
	}
	var slot int
	err := tx.QueryRow(ctx,
		`SELECT s FROM generate_series(1, $1::int) AS s
		 WHERE NOT EXISTS (SELECT 1 FROM teams WHERE slot = s AND state <> 'deleted')
		 ORDER BY s LIMIT 1`, m.Slots).Scan(&slot)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrPoolExhausted
	}
	if err != nil {
		return Lease{}, fmt.Errorf("ipam: pick slot: %w", err)
	}
	var ipText string
	err = tx.QueryRow(ctx,
		`SELECT host(ip) FROM ip_pool WHERE state = 'free'
		 ORDER BY ip LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&ipText)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrPoolExhausted
	}
	if err != nil {
		return Lease{}, fmt.Errorf("ipam: pick ip: %w", err)
	}
	ip, err := netip.ParseAddr(ipText)
	if err != nil {
		return Lease{}, fmt.Errorf("ipam: parse %q: %w", ipText, err)
	}
	tag, err := tx.Exec(ctx,
		`UPDATE teams SET ip = $2::inet, slot = $3, updated_at = now()
		 WHERE id = $1 AND state <> 'deleted' AND ip IS NULL AND slot IS NULL`, teamID, ipText, slot)
	if err != nil {
		return Lease{}, fmt.Errorf("ipam: assign to team %d: %w", teamID, err)
	}
	if tag.RowsAffected() != 1 {
		return Lease{}, ErrTeamNotFound
	}
	if _, err := tx.Exec(ctx,
		`UPDATE ip_pool SET state = 'allocated', team_id = $2, released_gen = NULL, released_at = NULL,
		        updated_at = now()
		 WHERE ip = $1::inet`, ipText, teamID); err != nil {
		return Lease{}, fmt.Errorf("ipam: mark %s allocated: %w", ip, err)
	}
	if err := event(ctx, tx, "ip.allocated", teamID, ip, map[string]any{"slot": slot}); err != nil {
		return Lease{}, err
	}
	return Lease{IP: ip, Slot: slot}, nil
}

// Release puts the team's IP in quarantine. It bumps the routes generation in the same
// transaction: the new generation is the first one without the team's route, and the IP
// stays in quarantine until every live gateway has acknowledged it. The caller must have
// taken the team out of the published routes (state not ready) earlier in tx.
// Release is idempotent: a team without an IP returns generation 0 and no error.
func (m *IPAM) Release(ctx context.Context, tx pgx.Tx, teamID int64) (int64, error) {
	var state string
	var ipText *string
	err := tx.QueryRow(ctx, `SELECT state, host(ip) FROM teams WHERE id = $1 FOR UPDATE`, teamID).
		Scan(&state, &ipText)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrTeamNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("ipam: read team %d: %w", teamID, err)
	}
	if state == "ready" {
		return 0, ErrStillRouted
	}
	if ipText == nil {
		return 0, nil
	}
	var gen int64
	if err := tx.QueryRow(ctx, `UPDATE routes_generation SET gen = gen + 1 RETURNING gen`).Scan(&gen); err != nil {
		return 0, fmt.Errorf("ipam: bump generation: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE ip_pool SET state = 'quarantine', team_id = NULL, released_gen = $2, released_at = now(),
		        updated_at = now()
		 WHERE team_id = $1 AND state = 'allocated'`, teamID, gen); err != nil {
		return 0, fmt.Errorf("ipam: quarantine: %w", err)
	}
	// The address leaves the team row so that it can be leased again while this team is
	// still being deleted (teams_ip_live is unique over non-deleted teams). The event log
	// keeps the history.
	if _, err := tx.Exec(ctx, `UPDATE teams SET ip = NULL, updated_at = now() WHERE id = $1`, teamID); err != nil {
		return 0, fmt.Errorf("ipam: clear team ip: %w", err)
	}
	ip, err := netip.ParseAddr(*ipText)
	if err != nil {
		return 0, fmt.Errorf("ipam: parse %q: %w", *ipText, err)
	}
	if err := event(ctx, tx, "ip.quarantined", teamID, ip, map[string]any{"released_gen": gen}); err != nil {
		return 0, err
	}
	return gen, nil
}

// InUse reports whether something outside the database still uses ip (a Neutron port).
type InUse func(ctx context.Context, ip netip.Addr) (bool, error)

// Sweep frees quarantined IPs that are safe to reuse: quarantined for at least
// m.Quarantine, acknowledged by every gateway seen within m.GatewayWindow, and not in use
// according to inUse (nil skips that check). If gateways are registered but none was seen
// recently, nothing is freed: the control plane cannot prove any gateway dropped the route.
// It returns the freed addresses.
func (m *IPAM) Sweep(ctx context.Context, db DB, inUse InUse) ([]netip.Addr, error) {
	candidates, err := m.sweepable(ctx, db, nil)
	if err != nil {
		return nil, err
	}
	var ok []string
	for _, ip := range candidates {
		if inUse != nil {
			used, err := inUse(ctx, ip)
			if err != nil {
				return nil, fmt.Errorf("ipam: check %s: %w", ip, err)
			}
			if used {
				continue
			}
		}
		ok = append(ok, ip.String())
	}
	if len(ok) == 0 {
		return nil, nil
	}
	// The conditions are checked again: only rows still eligible are freed.
	return m.sweepable(ctx, db, ok)
}

// sweepable lists eligible quarantined IPs, or frees them when only is non-nil.
func (m *IPAM) sweepable(ctx context.Context, db DB, only []string) ([]netip.Addr, error) {
	const eligible = `
		state = 'quarantine'
		AND released_at <= now() - $1 * interval '1 millisecond'
		AND (NOT EXISTS (SELECT 1 FROM gateways)
		     OR released_gen <= (SELECT min(acked_gen) FROM gateways
		                         WHERE last_seen > now() - $2 * interval '1 millisecond'))`
	args := []any{m.Quarantine.Milliseconds(), m.GatewayWindow.Milliseconds()}
	sql := `SELECT host(ip) FROM ip_pool WHERE ` + eligible + ` ORDER BY ip`
	if only != nil {
		sql = `UPDATE ip_pool SET state = 'free', released_gen = NULL, released_at = NULL, updated_at = now()
		       WHERE ip = ANY($3::text[]::inet[]) AND ` + eligible + ` RETURNING host(ip)`
		args = append(args, only)
	}
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("ipam: sweep: %w", err)
	}
	texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("ipam: sweep: %w", err)
	}
	ips := make([]netip.Addr, 0, len(texts))
	for _, s := range texts {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("ipam: parse %q: %w", s, err)
		}
		ips = append(ips, ip)
	}
	if only != nil {
		for _, ip := range ips {
			if err := event(ctx, db, "ip.freed", 0, ip, nil); err != nil {
				return nil, err
			}
		}
	}
	return ips, nil
}

// Renew extends a live team's lease by its hackathon's lease_ttl, counted from now.
// An expired lease cannot be renewed: the reconciler is already tearing it down.
func (m *IPAM) Renew(ctx context.Context, db DB, teamID int64) (time.Time, error) {
	var until time.Time
	err := db.QueryRow(ctx,
		`UPDATE teams t SET lease_expires_at = now() + h.lease_ttl, updated_at = now()
		 FROM hackathons h
		 WHERE t.id = $1 AND t.hackathon_id = h.id AND h.state = 'active'
		   AND t.state IN ('provisioning', 'ready') AND t.lease_expires_at > now()
		 RETURNING t.lease_expires_at`, teamID).Scan(&until)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrLeaseExpired
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("ipam: renew team %d: %w", teamID, err)
	}
	return until, nil
}

// Expired lists live teams whose lease has run out, oldest first, at most limit.
func (m *IPAM) Expired(ctx context.Context, db DB, limit int) ([]int64, error) {
	if limit <= 0 || limit > math.MaxInt32 {
		limit = 100
	}
	rows, err := db.Query(ctx,
		`SELECT id FROM teams WHERE state IN ('provisioning', 'ready') AND lease_expires_at <= now()
		 ORDER BY lease_expires_at LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("ipam: expired: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("ipam: expired: %w", err)
	}
	return ids, nil
}

// Counts returns the number of pool addresses per state.
func (m *IPAM) Counts(ctx context.Context, db DB) (map[string]int, error) {
	rows, err := db.Query(ctx, `SELECT state, count(*)::int FROM ip_pool GROUP BY state`)
	if err != nil {
		return nil, fmt.Errorf("ipam: counts: %w", err)
	}
	defer rows.Close()
	out := map[string]int{"free": 0, "allocated": 0, "quarantine": 0, "reserved": 0}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, fmt.Errorf("ipam: counts: %w", err)
		}
		out[state] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ipam: counts: %w", err)
	}
	return out, nil
}

func event(ctx context.Context, db DB, kind string, teamID int64, ip netip.Addr, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["ip"] = ip.String()
	var team *int64
	if teamID != 0 {
		team = &teamID
	}
	if _, err := db.Exec(ctx, `INSERT INTO events (kind, team_id, detail) VALUES ($1, $2, $3)`,
		kind, team, detail); err != nil {
		return fmt.Errorf("ipam: event %s: %w", kind, err)
	}
	return nil
}
