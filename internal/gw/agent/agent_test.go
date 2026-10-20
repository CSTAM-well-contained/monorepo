package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/CSTAM-well-contained/monorepo/internal/gw/haproxy"
	"github.com/CSTAM-well-contained/monorepo/internal/model"
)

const authz = "gw-for-tests"

// fakeApplier records what the agent applied.
type fakeApplier struct {
	mu     sync.Mutex
	gen    uint64
	routes []haproxy.Route
	calls  int
	fail   error
}

func (f *fakeApplier) SyncRoutes(_ context.Context, gen uint64, routes []haproxy.Route) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail != nil {
		return f.fail
	}
	f.gen, f.routes = gen, routes
	return nil
}

func (f *fakeApplier) Generation() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gen
}

func (f *fakeApplier) Settings() haproxy.Settings { return haproxy.Settings{Version: "v7"} }

// fakeCPD serves desired states and records acks.
type fakeCPD struct {
	mu      sync.Mutex
	desired model.Desired
	acks    []model.Ack
	down    bool
	hold    time.Duration // long-poll hold when nothing changed (default 100 ms)
}

func (c *fakeCPD) set(d model.Desired) {
	c.mu.Lock()
	c.desired = d
	c.mu.Unlock()
}

func (c *fakeCPD) lastAck() (model.Ack, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.acks) == 0 {
		return model.Ack{Generation: -1}, 0
	}
	return c.acks[len(c.acks)-1], len(c.acks)
}

func (c *fakeCPD) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	down := c.down
	c.mu.Unlock()
	if down {
		http.Error(w, "down", http.StatusBadGateway)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+authz {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/gateways/gw1/desired":
		after := int64(-1)
		if v := r.URL.Query().Get("after"); v != "" {
			after, _ = strconv.ParseInt(v, 10, 64)
		}
		c.mu.Lock()
		hold := c.hold
		c.mu.Unlock()
		if hold == 0 {
			hold = 100 * time.Millisecond
		}
		deadline := time.Now().Add(hold)
		for {
			c.mu.Lock()
			d := c.desired
			c.mu.Unlock()
			if d.Generation != after || time.Now().After(deadline) {
				_ = json.NewEncoder(w).Encode(d)
				return
			}
			select {
			case <-r.Context().Done(): // the agent stopped: let the test server close
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	case r.Method == http.MethodPost && r.URL.Path == "/v1/gateways/gw1/ack":
		var a model.Ack
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.acks = append(c.acks, a)
		c.mu.Unlock()
		_, _ = fmt.Fprint(w, "{}")
	default:
		http.NotFound(w, r)
	}
}

type rig struct {
	app    *fakeApplier
	cpd    *fakeCPD
	agent  *Agent
	run    string
	state  string
	cancel context.CancelFunc
	done   chan struct{}
}

func newRig(t *testing.T, app *fakeApplier, cpd *fakeCPD) *rig {
	t.Helper()
	srv := httptest.NewServer(cpd)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	r := &rig{app: app, cpd: cpd, run: filepath.Join(dir, "run"), state: filepath.Join(dir, "lib", "desired.json")}
	if err := os.MkdirAll(r.run, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.run, "ready"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Name: "gw1", CPD: srv.URL, Token: authz, RunDir: r.run, StateFile: r.state,
		Wait: 100 * time.Millisecond, ReadyAfter: 300 * time.Millisecond, VRRPPoll: 20 * time.Millisecond,
		MaxBackoff: 50 * time.Millisecond}, app)
	if err != nil {
		t.Fatal(err)
	}
	r.agent = a
	return r
}

func (r *rig) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.done = cancel, make(chan struct{})
	go func() {
		defer close(r.done)
		_ = r.agent.Run(ctx)
	}()
	t.Cleanup(r.stop)
}

func (r *rig) stop() {
	r.cancel()
	<-r.done
}

func (r *rig) readyFile() string {
	b, err := os.ReadFile(filepath.Join(r.run, "ready"))
	if err != nil {
		return ""
	}
	return string(b)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func desired(gen int64, hosts ...string) model.Desired {
	d := model.Desired{Generation: gen, Routes: []model.Route{}}
	for i, h := range hosts {
		d.Routes = append(d.Routes, model.Route{Host: h, Slot: i + 1, Addr: fmt.Sprintf("10.20.0.%d", 32+i), Port: 80})
	}
	return d
}

func TestSyncAckAndReady(t *testing.T) {
	cpd := &fakeCPD{desired: desired(3, "a.h.example.org")}
	r := newRig(t, &fakeApplier{}, cpd)
	r.start(t)
	eventually(t, "first ack", func() bool { a, _ := cpd.lastAck(); return a.Generation == 3 })
	if a, _ := cpd.lastAck(); a.ConfigVersion != "v7" {
		t.Fatalf("ack = %+v", a)
	}
	if got := r.readyFile(); got != "synced\n" {
		t.Fatalf("ready file = %q", got)
	}
	cpd.set(desired(4, "a.h.example.org", "b.h.example.org"))
	eventually(t, "second ack", func() bool { a, _ := cpd.lastAck(); return a.Generation == 4 })
	if n := len(r.app.routes); n != 2 {
		t.Fatalf("applied %d routes", n)
	}
	var saved model.Desired
	b, err := os.ReadFile(r.state)
	if err == nil {
		err = json.Unmarshal(b, &saved)
	}
	if err != nil || saved.Generation != 4 {
		t.Fatalf("saved state: %+v %v", saved, err)
	}
}

func TestNeverAppliesOlderGeneration(t *testing.T) {
	app := &fakeApplier{gen: 9}
	cpd := &fakeCPD{desired: desired(5, "a.h.example.org")}
	r := newRig(t, app, cpd)
	r.start(t)
	eventually(t, "ack", func() bool { _, n := cpd.lastAck(); return n > 0 })
	if app.calls != 0 || app.Generation() != 9 {
		t.Fatalf("applied an older generation: calls %d gen %d", app.calls, app.Generation())
	}
	if a, _ := cpd.lastAck(); a.Generation != 9 {
		t.Fatalf("ack = %+v", a)
	}
}

func TestFailedApplyKeepsOldAck(t *testing.T) {
	app := &fakeApplier{gen: 2, fail: errors.New("haproxy down")}
	cpd := &fakeCPD{desired: desired(3, "a.h.example.org")}
	r := newRig(t, app, cpd)
	r.start(t)
	eventually(t, "retries", func() bool { app.mu.Lock(); defer app.mu.Unlock(); return app.calls >= 2 })
	if a, _ := cpd.lastAck(); a.Generation == 3 {
		t.Fatal("acked a generation that was not applied")
	}
	app.mu.Lock()
	app.fail = nil
	app.mu.Unlock()
	eventually(t, "ack after recovery", func() bool { a, _ := cpd.lastAck(); return a.Generation == 3 })
}

// At start the gateway already serves the generation of cpd (0) but the first apply failed:
// the agent must retry without waiting for a long-poll that only ends when cpd changes.
func TestFailedApplyRetriedWhileCPDIsIdle(t *testing.T) {
	app := &fakeApplier{fail: errors.New("canary status 503")}
	cpd := &fakeCPD{desired: desired(0, "a.h.example.org"), hold: 20 * time.Second}
	r := newRig(t, app, cpd)
	r.start(t)
	eventually(t, "first try", func() bool { app.mu.Lock(); defer app.mu.Unlock(); return app.calls >= 1 })
	app.mu.Lock()
	app.fail = nil
	app.mu.Unlock()
	eventually(t, "routes applied", func() bool { app.mu.Lock(); defer app.mu.Unlock(); return len(app.routes) == 1 })
}

func TestBadRouteIsNotApplied(t *testing.T) {
	app := &fakeApplier{}
	d := desired(1, "a.h.example.org")
	d.Routes[0].Addr = "not-an-ip"
	r := newRig(t, app, &fakeCPD{desired: d})
	r.start(t)
	eventually(t, "a try", func() bool { return r.readyFile() != "" })
	if app.calls != 0 {
		t.Fatal("bad route reached HAProxy")
	}
}

func TestStaleReadyWhenCPDDown(t *testing.T) {
	app := &fakeApplier{}
	cpd := &fakeCPD{down: true}
	r := newRig(t, app, cpd)
	if err := save(r.state, desired(6, "a.h.example.org")); err != nil {
		t.Fatal(err)
	}
	r.start(t)
	eventually(t, "stale ready", func() bool { return r.readyFile() == "stale\n" })
	if app.Generation() != 6 || len(app.routes) != 1 {
		t.Fatalf("saved routes not restored: gen %d routes %d", app.Generation(), len(app.routes))
	}
}

func TestVRRPChangeIsReported(t *testing.T) {
	cpd := &fakeCPD{desired: desired(1)}
	r := newRig(t, &fakeApplier{}, cpd)
	r.start(t)
	eventually(t, "ack", func() bool { _, n := cpd.lastAck(); return n > 0 })
	line := fmt.Sprintf("MASTER %d.5 150\n", time.Now().Unix())
	if err := os.WriteFile(filepath.Join(r.run, "vrrp.state"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "vrrp ack", func() bool { a, _ := cpd.lastAck(); return a.VRRPState == "MASTER" })
}

func TestConvert(t *testing.T) {
	cases := []struct {
		name string
		in   model.Route
		ok   bool
	}{
		{"ok", model.Route{Host: "a.b.org", Slot: 1, Addr: "10.20.0.40", Port: 8080}, true},
		{"bad addr", model.Route{Host: "a.b.org", Slot: 1, Addr: "x", Port: 80}, false},
		{"port 0", model.Route{Host: "a.b.org", Slot: 1, Addr: "10.20.0.40"}, false},
		{"port too big", model.Route{Host: "a.b.org", Slot: 1, Addr: "10.20.0.40", Port: 70000}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Convert([]model.Route{tc.in}); (err == nil) != tc.ok {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	for _, cfg := range []Config{
		{Name: "gw1", CPD: "ftp://x", Token: "t", RunDir: "/r", StateFile: "/s"},
		{Name: "gw1", CPD: "http://x", RunDir: "/r", StateFile: "/s"},
	} {
		if _, err := New(cfg, &fakeApplier{}); err == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
}

func TestForceBackupFollowsCPD(t *testing.T) {
	app := &fakeApplier{fail: errors.New("haproxy down")} // a failover must not wait for routes
	d := desired(2)
	d.ForceBackup = []string{"gw2"}
	cpd := &fakeCPD{desired: d}
	r := newRig(t, app, cpd)
	fb := filepath.Join(r.run, ForceBackupFile)
	read := func() string { b, _ := os.ReadFile(filepath.Clean(fb)); return string(b) }
	r.start(t)
	eventually(t, "0 for another gateway", func() bool { return read() == "0\n" })

	d = desired(3)
	d.ForceBackup = []string{"gw1", "gw2"}
	cpd.set(d)
	eventually(t, "1 when cpd names gw1", func() bool { return read() == "1\n" })

	// keepalived restarted and reset the file (as root, so the agent replaces it)
	if err := os.WriteFile(fb, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "1 re-asserted", func() bool { return read() == "1\n" })

	cpd.set(desired(4))
	eventually(t, "0 after failback", func() bool { return read() == "0\n" })
}

func TestForceBackupUntouchedUntilKnown(t *testing.T) {
	r := newRig(t, &fakeApplier{}, &fakeCPD{down: true})
	fb := filepath.Join(r.run, ForceBackupFile)
	if err := os.WriteFile(fb, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.start(t)
	eventually(t, "stale ready", func() bool { return r.readyFile() == "stale\n" })
	if st, err := os.Stat(fb); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file rewritten before cpd or a saved state said anything: %v %v", st, err)
	}
}
