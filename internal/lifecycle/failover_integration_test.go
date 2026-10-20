//go:build integration

package lifecycle_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/CSTAM-well-contained/monorepo/internal/lifecycle"
	"github.com/CSTAM-well-contained/monorepo/internal/model"
)

func TestFailover(t *testing.T) {
	e := setup(t)
	ack := func(gw, vrrp string) {
		t.Helper()
		if err := e.c.Ack(e.ctx, gw, model.Ack{Generation: e.desired(t).Generation, VRRPState: vrrp}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.c.Failover(e.ctx, "gw1", false); !errors.Is(err, lifecycle.ErrNotFound) {
		t.Fatalf("unknown gateway: %v", err)
	}
	if err := e.c.Failover(e.ctx, "GW1;", false); !errors.Is(err, lifecycle.ErrInvalidSpec) {
		t.Fatalf("bad name: %v", err)
	}
	ack("gw1", "MASTER")
	if err := e.c.Failover(e.ctx, "gw1", false); !errors.Is(err, lifecycle.ErrNoPeer) {
		t.Fatalf("no peer registered: %v", err)
	}
	ack("gw2", "FAULT")
	if err := e.c.Failover(e.ctx, "gw1", false); !errors.Is(err, lifecycle.ErrNoPeer) {
		t.Fatalf("peer in FAULT: %v", err)
	}
	ack("gw2", "BACKUP")

	before := e.desired(t)
	if err := e.c.Failover(e.ctx, "gw1", false); err != nil {
		t.Fatal(err)
	}
	after := e.desired(t)
	if after.Generation != before.Generation+1 || !slices.Equal(after.ForceBackup, []string{"gw1"}) {
		t.Fatalf("desired after failover: %+v (before gen %d)", after, before.Generation)
	}
	if err := e.c.Failover(e.ctx, "gw1", false); err != nil || e.desired(t).Generation != after.Generation {
		t.Fatalf("second failover must be a no-op: %v", err)
	}
	// gw2 cannot be failed over too: its only peer is forced down (and behind)
	ack("gw2", "MASTER")
	if err := e.c.Failover(e.ctx, "gw2", false); !errors.Is(err, lifecycle.ErrNoPeer) {
		t.Fatalf("failover of the last gateway: %v", err)
	}
	st, err := e.c.Status(e.ctx)
	if err != nil || !st.Gateways[0].ForceBackup || st.Gateways[1].ForceBackup {
		t.Fatalf("status = %+v, %v", st.Gateways, err)
	}

	if err := e.c.Failback(e.ctx, "gw1"); err != nil {
		t.Fatal(err)
	}
	if d := e.desired(t); len(d.ForceBackup) != 0 || d.Generation != after.Generation+1 {
		t.Fatalf("desired after failback: %+v", d)
	}
	// force skips the peer check (gw1 is now behind)
	if err := e.c.Failover(e.ctx, "gw2", true); err != nil {
		t.Fatalf("forced failover: %v", err)
	}
	evs, err := e.c.Events(e.ctx, -1, 20)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, ev := range evs {
		kinds = append(kinds, ev.Kind)
	}
	if n := len(slices.DeleteFunc(slices.Clone(kinds), func(k string) bool { return k != "gateway.failover" })); n != 2 {
		t.Fatalf("failover events: %v", kinds)
	}
	if !slices.Contains(kinds, "gateway.failback") {
		t.Fatalf("no failback event: %v", kinds)
	}
}
