//go:build integration

package lifecycle_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/CSTAM-well-contained/monorepo/internal/lifecycle"
	"github.com/CSTAM-well-contained/monorepo/internal/model"
)

func TestConfigVersions(t *testing.T) {
	e := setup(t)
	if d := e.desired(t); d.Config != nil {
		t.Fatalf("config before any push: %+v", d.Config)
	}
	if _, err := e.c.PushConfig(e.ctx, lifecycle.ConfigIn{RateLimit: 0}); !errors.Is(err, lifecycle.ErrInvalidSpec) {
		t.Fatalf("invalid push: %v", err)
	}

	gen0 := e.desired(t).Generation
	c1, err := e.c.PushConfig(e.ctx, lifecycle.ConfigIn{RateLimit: 300, Reason: "first"})
	if err != nil || c1.Version != "c1" || !c1.Current || len(c1.Extra) != 0 {
		t.Fatalf("push c1 = %+v, %v", c1, err)
	}
	broken := []string{"http-request return status 500 if { path /__gw/healthz }"}
	c2, err := e.c.PushConfig(e.ctx, lifecycle.ConfigIn{RateLimit: 300, Extra: broken, Reason: "demo: valid but broken"})
	if err != nil || c2.Version != "c2" {
		t.Fatalf("push c2 = %+v, %v", c2, err)
	}
	d := e.desired(t)
	if d.Generation != gen0+2 || d.Config == nil || d.Config.Version != "c2" || !slices.Equal(d.Config.Extra, broken) {
		t.Fatalf("desired = %+v (config %+v)", d, d.Config)
	}

	// gw1 rolled c2 back and still serves c1: an alert and a config.rolled_back event
	ack := model.Ack{Generation: d.Generation, ConfigVersion: "c1", VRRPState: "MASTER",
		ConfigTried: "c2", ConfigOutcome: "rolled_back", ConfigError: "verify: canary status 500\nmore"}
	if err := e.c.Ack(e.ctx, "gw1", ack); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Ack(e.ctx, "gw1", ack); err != nil { // same report again: no second event
		t.Fatal(err)
	}
	st, err := e.c.Status(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	g := st.Gateways[0]
	if st.Config != "c2" || g.ConfigTried != "c2" || g.ConfigOutcome != "rolled_back" || strings.Contains(g.ConfigError, "\n") {
		t.Fatalf("status config %q, gateway %+v", st.Config, g)
	}
	if !slices.ContainsFunc(st.Alerts, func(a string) bool { return strings.Contains(a, "did not take config c2") }) {
		t.Fatalf("alerts = %q", st.Alerts)
	}
	evs, err := e.c.Events(e.ctx, -1, 50)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Kind == "config.rolled_back" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d config.rolled_back events", n)
	}
	if err := e.c.Ack(e.ctx, "gw1", model.Ack{Generation: d.Generation, ConfigOutcome: "exploded"}); !errors.Is(err, lifecycle.ErrInvalidSpec) {
		t.Fatalf("bad outcome: %v", err)
	}

	// manual rollback to c1 = new version c3 with c1 content
	c3, err := e.c.RollbackConfig(e.ctx, c1.ID)
	if err != nil || c3.Version != "c3" || c3.CopiedFrom == nil || *c3.CopiedFrom != c1.ID ||
		c3.RateLimit != 300 || len(c3.Extra) != 0 || c3.Reason != "rollback to c1" {
		t.Fatalf("rollback = %+v, %v", c3, err)
	}
	if _, err := e.c.RollbackConfig(e.ctx, 99); !errors.Is(err, lifecycle.ErrNotFound) {
		t.Fatalf("rollback to a missing version: %v", err)
	}
	vs, err := e.c.ConfigVersions(e.ctx)
	if err != nil || len(vs) != 3 || vs[0].Version != "c3" || !vs[0].Current || vs[1].Current {
		t.Fatalf("versions = %+v, %v", vs, err)
	}
	if d := e.desired(t); d.Config.Version != "c3" {
		t.Fatalf("desired config after rollback: %+v", d.Config)
	}
}
