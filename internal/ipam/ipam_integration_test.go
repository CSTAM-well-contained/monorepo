//go:build integration

package ipam_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CSTAM-well-contained/monorepo/internal/config"
	"github.com/CSTAM-well-contained/monorepo/internal/ipam"
	"github.com/CSTAM-well-contained/monorepo/internal/store"
	"github.com/CSTAM-well-contained/monorepo/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

// poolSize is the number of leasable team IPs: .32-.191 minus the reserved ones.
var poolSize = 160 - len(config.ReservedIPs)

type env struct {
	ctx  context.Context
	pool *pgxpool.Pool
	m    *ipam.IPAM
	hack int64
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := store.Open(ctx, testutil.Postgres(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	m := ipam.New()
	m.Quarantine = 0
	for range 2 { // idempotent
		if err := m.Seed(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	var hack int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO hackathons (name, domain, lease_ttl) VALUES ('h1', 'h1.example.org', '1 hour') RETURNING id`).
		Scan(&hack); err != nil {
		t.Fatal(err)
	}
	return &env{ctx: ctx, pool: pool, m: m, hack: hack}
}

func newTeam(ctx context.Context, tx pgx.Tx, hack int64, name string) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx,
		`INSERT INTO teams (hackathon_id, name, state, lease_expires_at)
		 VALUES ($1, $2, 'provisioning', now() + interval '1 hour') RETURNING id`, hack, name).Scan(&id)
	return id, err
}

// create makes a team and leases its IP in one transaction, as cpd does.
func (e *env) create(t *testing.T, name string) (int64, ipam.Lease) {
	t.Helper()
	var id int64
	var lease ipam.Lease
	err := store.InTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		var err error
		if id, err = newTeam(e.ctx, tx, e.hack, name); err != nil {
			return err
		}
		lease, err = e.m.Allocate(e.ctx, tx, id)
		return err
	})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return id, lease
}

func (e *env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// release takes the team out of the routes and quarantines its IP, as teardown does.
func (e *env) release(t *testing.T, id int64) int64 {
	t.Helper()
	var gen int64
	err := store.InTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(e.ctx, `UPDATE teams SET state = 'deleting' WHERE id = $1`, id); err != nil {
			return err
		}
		var err error
		gen, err = e.m.Release(e.ctx, tx, id)
		return err
	})
	if err != nil {
		t.Fatalf("release %d: %v", id, err)
	}
	return gen
}

func (e *env) counts(t *testing.T) map[string]int {
	t.Helper()
	c, err := e.m.Counts(e.ctx, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSeed(t *testing.T) {
	e := setup(t)
	c := e.counts(t)
	if c["free"] != poolSize || c["reserved"] != len(config.ReservedIPs) {
		t.Fatalf("counts = %v, want %d free and %d reserved", c, poolSize, len(config.ReservedIPs))
	}
	// A reserved address that leaves the list becomes free again; a new one is reserved.
	e.m.Reserved = []netip.Addr{netip.MustParseAddr("10.20.0.40")}
	if err := e.m.Seed(e.ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	var state string
	for ip, want := range map[string]string{"10.20.0.68": "free", "10.20.0.40": "reserved"} {
		if err := e.pool.QueryRow(e.ctx, `SELECT state FROM ip_pool WHERE ip = $1::inet`, ip).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != want {
			t.Errorf("%s is %s, want %s", ip, state, want)
		}
	}
}

func TestReservedNeverLeased(t *testing.T) {
	e := setup(t)
	e.m.Slots = 999
	seen := map[netip.Addr]bool{}
	for i := range poolSize {
		_, lease := e.create(t, fmt.Sprintf("t%d", i))
		seen[lease.IP] = true
	}
	for _, ip := range config.ReservedIPs {
		if seen[ip] {
			t.Errorf("reserved %s was leased", ip)
		}
	}
	if len(seen) != poolSize {
		t.Errorf("%d distinct IPs, want %d", len(seen), poolSize)
	}
}

func TestParallelAllocation(t *testing.T) {
	tests := []struct {
		name     string
		slots    int
		attempts int
		want     int // successful allocations
	}{
		{"slots run out first", 128, 1000, 128},
		{"IPs run out first", 999, 300, poolSize},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			e.m.Slots = tc.slots
			var (
				mu        sync.Mutex
				ips       = map[netip.Addr]int64{}
				slots     = map[int]int64{}
				exhausted int
				wg        sync.WaitGroup
			)
			for i := range tc.attempts {
				wg.Add(1)
				go func() {
					defer wg.Done()
					var id int64
					var lease ipam.Lease
					err := store.InTx(e.ctx, e.pool, func(tx pgx.Tx) error {
						var err error
						if id, err = newTeam(e.ctx, tx, e.hack, fmt.Sprintf("t%d", i)); err != nil {
							return err
						}
						lease, err = e.m.Allocate(e.ctx, tx, id)
						return err
					})
					mu.Lock()
					defer mu.Unlock()
					switch {
					case errors.Is(err, ipam.ErrPoolExhausted):
						exhausted++
					case err != nil:
						t.Errorf("team %d: %v", i, err)
					default:
						if other, dup := ips[lease.IP]; dup {
							t.Errorf("IP %s given to teams %d and %d", lease.IP, other, id)
						}
						if other, dup := slots[lease.Slot]; dup {
							t.Errorf("slot %d given to teams %d and %d", lease.Slot, other, id)
						}
						ips[lease.IP], slots[lease.Slot] = id, id
					}
				}()
			}
			wg.Wait()
			if len(ips) != tc.want || exhausted != tc.attempts-tc.want {
				t.Fatalf("%d allocated, %d exhausted; want %d and %d", len(ips), exhausted, tc.want, tc.attempts-tc.want)
			}
			// The database agrees with what the callers saw.
			var dbTeams int
			if err := e.pool.QueryRow(e.ctx,
				`SELECT count(*) FROM teams t JOIN ip_pool p ON p.team_id = t.id AND p.ip = t.ip
				 WHERE p.state = 'allocated'`).Scan(&dbTeams); err != nil {
				t.Fatal(err)
			}
			if dbTeams != tc.want {
				t.Errorf("database has %d allocated teams, want %d", dbTeams, tc.want)
			}
		})
	}
}

func TestReleaseRules(t *testing.T) {
	e := setup(t)
	id, _ := e.create(t, "team1")
	e.exec(t, `UPDATE teams SET state = 'ready' WHERE id = $1`, id)
	err := store.InTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		_, err := e.m.Release(e.ctx, tx, id)
		return err
	})
	if !errors.Is(err, ipam.ErrStillRouted) {
		t.Fatalf("release of a ready team: %v, want ErrStillRouted", err)
	}
	gen := e.release(t, id)
	if gen != 1 {
		t.Errorf("first release generation = %d, want 1", gen)
	}
	if again := e.release(t, id); again != 0 {
		t.Errorf("second release generation = %d, want 0 (idempotent)", again)
	}
	if c := e.counts(t); c["quarantine"] != 1 || c["allocated"] != 0 {
		t.Errorf("counts = %v, want 1 quarantine", c)
	}
}

func TestQuarantineSweep(t *testing.T) {
	const gwSeen, gwStale = "now()", "now() - interval '10 minutes'"
	type gw struct {
		name     string
		ackDelta int64 // acked_gen = released generation + ackDelta
		seen     string
	}
	tests := []struct {
		name       string
		gateways   []gw
		quarantine time.Duration
		inUse      bool
		wantFreed  bool
	}{
		{"no gateway registered", nil, 0, false, true},
		{"both gateways acked", []gw{{"gw1", 0, gwSeen}, {"gw2", 1, gwSeen}}, 0, false, true},
		{"one gateway behind", []gw{{"gw1", 0, gwSeen}, {"gw2", -1, gwSeen}}, 0, false, false},
		{"stale gateway behind is ignored", []gw{{"gw1", 0, gwSeen}, {"gw2", -1, gwStale}}, 0, false, true},
		{"every gateway stale", []gw{{"gw1", 0, gwStale}, {"gw2", 0, gwStale}}, 0, false, false},
		{"quarantine time not over", []gw{{"gw1", 0, gwSeen}}, time.Hour, false, false},
		{"neutron port still exists", []gw{{"gw1", 0, gwSeen}}, 0, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			e.m.Quarantine = tc.quarantine
			id, lease := e.create(t, "team1")
			gen := e.release(t, id)
			for _, g := range tc.gateways {
				e.exec(t, `INSERT INTO gateways (name, acked_gen, last_seen) VALUES ($1, $2, `+g.seen+`)`,
					g.name, gen+g.ackDelta)
			}
			inUse := func(context.Context, netip.Addr) (bool, error) { return tc.inUse, nil }
			freed, err := e.m.Sweep(e.ctx, e.pool, inUse)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(freed) == 1 && freed[0] == lease.IP; got != tc.wantFreed {
				t.Fatalf("freed = %v, want freed %v", freed, tc.wantFreed)
			}
		})
	}
}

// A freed IP can go to a new team while the old team row is still being deleted.
func TestReuseWhileOldTeamDeleting(t *testing.T) {
	e := setup(t)
	e.m.Slots = 1
	old, first := e.create(t, "team1")
	e.release(t, old)
	if _, err := e.m.Sweep(e.ctx, e.pool, nil); err != nil {
		t.Fatal(err)
	}
	// The old team (state deleting) still holds slot 1: no slot left.
	err := store.InTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		id, err := newTeam(e.ctx, tx, e.hack, "team2")
		if err != nil {
			return err
		}
		_, err = e.m.Allocate(e.ctx, tx, id)
		return err
	})
	if !errors.Is(err, ipam.ErrPoolExhausted) {
		t.Fatalf("allocate with the only slot held: %v, want ErrPoolExhausted", err)
	}
	e.m.Slots = 2
	_, second := e.create(t, "team2")
	if second.IP != first.IP {
		t.Errorf("new team got %s, want the recycled %s (lowest free)", second.IP, first.IP)
	}
}

func TestLease(t *testing.T) {
	e := setup(t)
	id, _ := e.create(t, "team1")
	until, err := e.m.Renew(e.ctx, e.pool, id)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(until); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("renewed until %v, want about now + 1h (lease_ttl)", until)
	}
	e.exec(t, `UPDATE teams SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, id)
	if _, err := e.m.Renew(e.ctx, e.pool, id); !errors.Is(err, ipam.ErrLeaseExpired) {
		t.Errorf("renew of an expired lease: %v, want ErrLeaseExpired", err)
	}
	expired, err := e.m.Expired(e.ctx, e.pool, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0] != id {
		t.Errorf("expired = %v, want [%d]", expired, id)
	}
}
