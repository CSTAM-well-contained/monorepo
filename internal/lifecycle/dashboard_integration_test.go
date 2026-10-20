//go:build integration

package lifecycle_test

import (
	"strings"
	"testing"

	"github.com/CSTAM-well-contained/monorepo/internal/model"
)

// TestDashboardQueries checks the read endpoints of the dashboard against a real database.
func TestDashboardQueries(t *testing.T) {
	e := setup(t)
	tm, err := e.c.CreateTeam(e.ctx, "h1", "t1", 80, "")
	if err != nil {
		t.Fatal(err)
	}
	e.c.Wait()

	all, err := e.c.AllTeams(e.ctx)
	if err != nil || len(all) != 1 || all[0].Name != "t1" || all[0].State != "ready" {
		t.Fatalf("all teams = %+v, %v", all, err)
	}

	addrs, err := e.c.Addresses(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 160 || addrs[0].IP != "10.20.0.32" || addrs[0].State != "allocated" ||
		addrs[0].Team != "t1" || addrs[0].Hackathon != "h1" {
		t.Fatalf("addresses: %d, first %+v", len(addrs), addrs[0])
	}
	if addrs[1].IP != "10.20.0.33" || addrs[1].State != "free" {
		t.Fatalf("second address %+v (want numeric order)", addrs[1])
	}

	routes, err := e.c.Routes(e.ctx)
	if err != nil || len(routes.Routes) != 1 || routes.Routes[0].Addr != tm.IP {
		t.Fatalf("routes = %+v, %v", routes, err)
	}

	last, err := e.c.Events(e.ctx, -1, 100)
	if err != nil || len(last) == 0 {
		t.Fatalf("events = %v, %v", last, err)
	}
	kinds := ""
	for i, ev := range last {
		kinds += ev.Kind + " "
		if i > 0 && ev.ID <= last[i-1].ID {
			t.Fatalf("events not oldest first: %+v", last)
		}
	}
	if !strings.Contains(kinds, "ip.allocated") || !strings.Contains(kinds, "team.ready") {
		t.Fatalf("event kinds: %s", kinds)
	}
	newest := last[len(last)-1]
	if newest.Team != "t1" && newest.Kind == "team.ready" {
		t.Fatalf("team name not joined: %+v", newest)
	}
	if two, err := e.c.Events(e.ctx, -1, 2); err != nil || len(two) != 2 || two[1].ID != newest.ID {
		t.Fatalf("last 2 events = %+v, %v", two, err)
	}
	if none, err := e.c.Events(e.ctx, newest.ID, 10); err != nil || len(none) != 0 {
		t.Fatalf("events after the newest = %+v, %v", none, err)
	}

	// Gateway health: gw1 MASTER in sync, gw2 BACKUP one generation behind.
	gen := routes.Generation
	if err := e.c.Ack(e.ctx, "gw1", model.Ack{Generation: gen, ConfigVersion: "v1", VRRPState: "MASTER"}); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Ack(e.ctx, "gw2", model.Ack{Generation: gen - 1, ConfigVersion: "v1", VRRPState: "BACKUP"}); err != nil {
		t.Fatal(err)
	}
	st, err := e.c.Status(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Gateways) != 2 || !st.Gateways[0].Alive || !st.Gateways[0].InSync || st.Gateways[1].InSync {
		t.Fatalf("gateways = %+v", st.Gateways)
	}
	if len(st.Alerts) != 1 || !strings.Contains(st.Alerts[0], "gw2 is behind") {
		t.Fatalf("alerts = %q", st.Alerts)
	}
	vrrp, err := e.c.Events(e.ctx, newest.ID, 10)
	if err != nil || len(vrrp) != 2 || vrrp[0].Kind != "gateway.vrrp" {
		t.Fatalf("vrrp events = %+v, %v", vrrp, err)
	}
}
