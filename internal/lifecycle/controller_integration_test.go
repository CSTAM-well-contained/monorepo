//go:build integration

package lifecycle_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CSTAM-well-contained/monorepo/internal/config"
	"github.com/CSTAM-well-contained/monorepo/internal/ipam"
	"github.com/CSTAM-well-contained/monorepo/internal/lifecycle"
	"github.com/CSTAM-well-contained/monorepo/internal/model"
	"github.com/CSTAM-well-contained/monorepo/internal/openstack/fake"
	"github.com/CSTAM-well-contained/monorepo/internal/store"
	"github.com/CSTAM-well-contained/monorepo/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

type env struct {
	ctx  context.Context
	pool *pgxpool.Pool
	m    *ipam.IPAM
	sbx  *fake.Sandboxes
	c    *lifecycle.Controller
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
	if err := m.Seed(ctx, pool); err != nil {
		t.Fatal(err)
	}
	e := &env{ctx: ctx, pool: pool, m: m, sbx: fake.New()}
	e.c = e.controller(ctx)
	if _, err := e.c.CreateHackathon(ctx, "h1", "h1.example.org", time.Hour); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) controller(ctx context.Context) *lifecycle.Controller {
	c := lifecycle.NewController(ctx, e.pool, e.m, e.sbx, slog.New(slog.NewTextHandler(io.Discard, nil)), 4)
	c.FailedRetention = 0
	return c
}

func (e *env) team(t *testing.T, id int64) lifecycle.Team {
	t.Helper()
	tm, err := e.c.Team(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func (e *env) reconcile(t *testing.T) {
	t.Helper()
	if err := e.c.Reconcile(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.c.Wait()
}

func (e *env) desired(t *testing.T) model.Desired {
	t.Helper()
	d, err := e.c.Desired(e.ctx, -1, 0)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *env) counts(t *testing.T) map[string]int {
	t.Helper()
	n, err := e.m.Counts(e.ctx, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTeamLifecycle(t *testing.T) {
	e := setup(t)
	tm, err := e.c.CreateTeam(e.ctx, "h1", "t1", 8080, "")
	if err != nil {
		t.Fatal(err)
	}
	if tm.State != "provisioning" || tm.IP != "10.20.0.32" || tm.Slot != 1 || tm.FQDN != "t1.h1.example.org" {
		t.Fatalf("new team = %+v", tm)
	}
	e.c.Wait()
	if got := e.team(t, tm.ID); got.State != "ready" {
		t.Fatalf("state %s, error %q", got.State, got.Error)
	}
	d := e.desired(t)
	want := model.Route{Host: "t1.h1.example.org", Slot: 1, Addr: "10.20.0.32", Port: 8080}
	if len(d.Routes) != 1 || d.Routes[0] != want {
		t.Fatalf("routes = %+v", d.Routes)
	}
	if _, err := e.c.CreateTeam(e.ctx, "h1", "t1", 80, ""); !errors.Is(err, lifecycle.ErrConflict) {
		t.Fatalf("duplicate team: %v", err)
	}
	if _, err := e.c.Renew(e.ctx, tm.ID); err != nil {
		t.Fatal(err)
	}

	if err := e.c.DeleteTeam(e.ctx, tm.ID); err != nil {
		t.Fatal(err)
	}
	e.c.Wait()
	if got := e.team(t, tm.ID); got.State != "deleted" {
		t.Fatalf("state after delete: %s", got.State)
	}
	d2 := e.desired(t)
	if len(d2.Routes) != 0 || d2.Generation <= d.Generation {
		t.Fatalf("after delete: %+v (was gen %d)", d2, d.Generation)
	}
	if refs, _ := e.sbx.List(e.ctx, "cstam-"); len(refs) != 0 {
		t.Fatalf("sandboxes left: %v", refs)
	}
	if n := e.counts(t); n["quarantine"] != 1 {
		t.Fatalf("pool after delete: %v", n)
	}
	// no gateway registered: the sweep frees it
	e.reconcile(t)
	if n := e.counts(t); n["quarantine"] != 0 || n["allocated"] != 0 {
		t.Fatalf("pool after sweep: %v", n)
	}
	if err := e.c.DeleteTeam(e.ctx, tm.ID); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestCreateFailureCleansUp(t *testing.T) {
	e := setup(t)
	e.sbx.FailCreate = func(lifecycle.Spec) error { return lifecycle.ErrQuota }
	e.sbx.CreatedAnyway = true // half-created: the port exists
	tm, err := e.c.CreateTeam(e.ctx, "h1", "t1", 80, "")
	if err != nil {
		t.Fatal(err)
	}
	e.c.Wait()
	got := e.team(t, tm.ID)
	if got.State != "failed" || got.Error == "" || got.IP != "" {
		t.Fatalf("team = %+v", got)
	}
	if refs, _ := e.sbx.List(e.ctx, "cstam-"); len(refs) != 0 {
		t.Fatalf("sandboxes left: %v", refs)
	}
	if len(e.desired(t).Routes) != 0 {
		t.Fatal("failed team is routed")
	}
	// retention 0: the next pass deletes the failed team and frees name and slot
	e.reconcile(t)
	e.reconcile(t)
	if got := e.team(t, tm.ID); got.State != "deleted" {
		t.Fatalf("state = %s", got.State)
	}
	e.sbx.FailCreate = nil
	if _, err := e.c.CreateTeam(e.ctx, "h1", "t1", 80, ""); err != nil {
		t.Fatalf("name not reusable: %v", err)
	}
}

func TestIPTakenStaysQuarantined(t *testing.T) {
	e := setup(t)
	e.sbx.HoldForeign(config.TeamPoolLo)
	tm, err := e.c.CreateTeam(e.ctx, "h1", "t1", 80, "")
	if err != nil {
		t.Fatal(err)
	}
	e.c.Wait()
	if got := e.team(t, tm.ID); got.State != "failed" {
		t.Fatalf("state = %s", got.State)
	}
	e.reconcile(t)
	if n := e.counts(t); n["quarantine"] != 1 {
		t.Fatalf("held IP was freed: %v", n)
	}
}

func TestLeaseExpiry(t *testing.T) {
	e := setup(t)
	tm, err := e.c.CreateTeam(e.ctx, "h1", "t1", 80, "")
	if err != nil {
		t.Fatal(err)
	}
	e.c.Wait()
	if _, err := e.pool.Exec(e.ctx, `UPDATE teams SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, tm.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Renew(e.ctx, tm.ID); !errors.Is(err, ipam.ErrLeaseExpired) {
		t.Fatalf("renew expired: %v", err)
	}
	e.reconcile(t)
	if got := e.team(t, tm.ID); got.State != "deleted" {
		t.Fatalf("state = %s", got.State)
	}
}

func TestRestartRecovery(t *testing.T) {
	e := setup(t)
	dead, cancel := context.WithCancel(e.ctx)
	cancel()
	crashed := e.controller(dead) // registers the team, but its worker never runs
	tm, err := crashed.CreateTeam(e.ctx, "h1", "t1", 80, "")
	if err != nil {
		t.Fatal(err)
	}
	if e.sbx.Creates != 0 {
		t.Fatal("worker ran on a dead controller")
	}
	e.reconcile(t)
	if got := e.team(t, tm.ID); got.State != "ready" {
		t.Fatalf("state = %s", got.State)
	}
}

// slowCloud blocks Create until released, to delete a team mid-provisioning.
type slowCloud struct {
	*fake.Sandboxes
	entered, release chan struct{}
}

func (s slowCloud) Create(ctx context.Context, spec lifecycle.Spec) (lifecycle.Ref, error) {
	close(s.entered)
	<-s.release
	return s.Sandboxes.Create(ctx, spec)
}

func TestDeleteWhileProvisioning(t *testing.T) {
	e := setup(t)
	slow := slowCloud{Sandboxes: e.sbx, entered: make(chan struct{}), release: make(chan struct{})}
	c := lifecycle.NewController(e.ctx, e.pool, e.m, slow, slog.New(slog.NewTextHandler(io.Discard, nil)), 4)
	tm, err := c.CreateTeam(e.ctx, "h1", "t1", 80, "")
	if err != nil {
		t.Fatal(err)
	}
	<-slow.entered
	if err := c.DeleteTeam(e.ctx, tm.ID); err != nil {
		t.Fatal(err)
	}
	close(slow.release)
	c.Wait()
	if got := e.team(t, tm.ID); got.State != "deleted" {
		t.Fatalf("state = %s", got.State)
	}
	if refs, _ := e.sbx.List(e.ctx, "cstam-"); len(refs) != 0 {
		t.Fatalf("sandbox leaked: %v", refs)
	}
}

func TestDesiredLongPoll(t *testing.T) {
	e := setup(t)
	gen := e.desired(t).Generation
	done := make(chan model.Desired, 1)
	go func() {
		d, err := e.c.Desired(e.ctx, gen, 20*time.Second)
		if err != nil {
			t.Error(err)
		}
		done <- d
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if _, err := e.c.CreateTeam(e.ctx, "h1", "t1", 80, ""); err != nil {
		t.Fatal(err)
	}
	d := <-done
	if d.Generation <= gen || len(d.Routes) != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("long-poll returned %+v after %s", d, time.Since(start))
	}
	e.c.Wait()
}

func TestGatewayAckGatesRecycling(t *testing.T) {
	e := setup(t)
	tm, err := e.c.CreateTeam(e.ctx, "h1", "t1", 80, "")
	if err != nil {
		t.Fatal(err)
	}
	e.c.Wait()
	gen := e.desired(t).Generation
	for _, gw := range []string{"gw1", "gw2"} {
		if err := e.c.Ack(e.ctx, gw, model.Ack{Generation: gen, ConfigVersion: "v1", VRRPState: "BACKUP"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.c.Ack(e.ctx, "gw1", model.Ack{Generation: gen + 5}); !errors.Is(err, lifecycle.ErrInvalidSpec) {
		t.Fatalf("ack ahead of generation: %v", err)
	}
	if err := e.c.Ack(e.ctx, "gw1", model.Ack{Generation: gen, VRRPState: "bogus"}); !errors.Is(err, lifecycle.ErrInvalidSpec) {
		t.Fatalf("bad vrrp state: %v", err)
	}
	if err := e.c.DeleteTeam(e.ctx, tm.ID); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	if n := e.counts(t); n["quarantine"] != 1 {
		t.Fatalf("freed before the gateways acked: %v", n)
	}
	newGen := e.desired(t).Generation
	for _, gw := range []string{"gw1", "gw2"} {
		if err := e.c.Ack(e.ctx, gw, model.Ack{Generation: newGen, ConfigVersion: "v1", VRRPState: "MASTER"}); err != nil {
			t.Fatal(err)
		}
	}
	e.reconcile(t)
	if n := e.counts(t); n["quarantine"] != 0 {
		t.Fatalf("not freed after both acks: %v", n)
	}
	s, err := e.c.Status(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Gateways) != 2 || s.Gateways[0].VRRPState != "MASTER" || s.Teams["deleted"] != 1 {
		t.Fatalf("status = %+v", s)
	}
}

func TestEndHackathon(t *testing.T) {
	e := setup(t)
	for _, n := range []string{"a", "b", "c"} {
		if _, err := e.c.CreateTeam(e.ctx, "h1", n, 80, ""); err != nil {
			t.Fatal(err)
		}
	}
	e.c.Wait()
	if err := e.c.EndHackathon(e.ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.CreateTeam(e.ctx, "h1", "late", 80, ""); !errors.Is(err, lifecycle.ErrNotActive) {
		t.Fatalf("create after end: %v", err)
	}
	e.c.Wait()
	e.reconcile(t)
	h, err := e.c.Hackathon(e.ctx, "h1")
	if err != nil {
		t.Fatal(err)
	}
	if h.State != "ended" {
		t.Fatalf("hackathon state = %s", h.State)
	}
	if refs, _ := e.sbx.List(e.ctx, "cstam-"); len(refs) != 0 {
		t.Fatalf("sandboxes left: %v", refs)
	}
	if err := e.c.EndHackathon(e.ctx, "h1"); err != nil {
		t.Fatalf("end twice: %v", err)
	}
}

func TestValidation(t *testing.T) {
	e := setup(t)
	cases := []struct {
		name, hack, team, image string
		port                    int
		want                    error
	}{
		{"bad team name", "h1", "Bad_Name", "", 80, lifecycle.ErrInvalidSpec},
		{"bad port", "h1", "t1", "", 70000, lifecycle.ErrInvalidSpec},
		{"bad image", "h1", "t1", "x; rm -rf /", 80, lifecycle.ErrInvalidSpec},
		{"unknown hackathon", "nope", "t1", "", 80, lifecycle.ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := e.c.CreateTeam(e.ctx, tc.hack, tc.team, tc.port, tc.image); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := e.c.CreateHackathon(e.ctx, "h1", "x.example.org", 0); !errors.Is(err, lifecycle.ErrConflict) {
		t.Fatalf("duplicate hackathon: %v", err)
	}
	if _, err := e.c.CreateHackathon(e.ctx, "h2", "not a domain", 0); !errors.Is(err, lifecycle.ErrInvalidSpec) {
		t.Fatalf("bad domain: %v", err)
	}
}
