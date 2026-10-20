//go:build integration

package applier

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CSTAM-well-contained/monorepo/internal/gw/haproxy"
)

const image = "haproxy:3.2"

// lab runs a real HAProxy in Docker (host network, same paths inside and outside).
type lab struct {
	t        *testing.T
	ctr      string
	settings haproxy.Settings
	app      *Applier
	base     string // http://127.0.0.1:<port>
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func newLab(t *testing.T) *lab { return newLabWith(t, nil) }

// newLabWith lets a test change the settings before HAProxy starts.
func newLabWith(t *testing.T, mod func(*haproxy.Settings)) *lab {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	dir, err := os.MkdirTemp("", "cstam-hap-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	canary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "canary\n") }))
	t.Cleanup(canary.Close)

	port := freePort(t)
	s := haproxy.Settings{
		Version: "v000001", GWName: "gw1",
		Dir: dir, AdminSocket: filepath.Join(dir, "admin.sock"), SocketMode: "600",
		HTTPBind: fmt.Sprintf("127.0.0.1:%d", port), CanaryAddr: strings.TrimPrefix(canary.URL, "http://"),
		InternalSources: []string{"127.0.0.1"}, Slots: 8, RateLimit: 100000,
	}
	if mod != nil {
		mod(&s)
	}
	ctr := fmt.Sprintf("cstam-it-hap-%d", time.Now().UnixNano())
	docker(t, "run", "-d", "--rm", "--name", ctr, "--network", "host",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-v", dir+":"+dir, "--entrypoint", "sleep", image, "infinity")
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", ctr).Run() })

	l := &lab{t: t, ctr: ctr, settings: s, base: fmt.Sprintf("http://127.0.0.1:%d", port)}
	l.app = New(Config{
		Runtime:   haproxy.Runtime{Socket: haproxy.Socket{Path: s.AdminSocket}},
		Master:    haproxy.Master{Socket: haproxy.Socket{Path: filepath.Join(dir, "master.sock"), Timeout: 10 * time.Second}},
		Checker:   haproxy.Checker{Argv: []string{"docker", "exec", ctr, "haproxy"}},
		HealthURL: l.base + "/__gw/healthz",
	}, s)

	ctx := context.Background()
	if err := l.app.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	docker(t, "exec", "-d", ctr, "haproxy", "-W", "-db",
		"-S", filepath.Join(dir, "master.sock")+",mode,600", "-f", haproxy.CfgPath(dir))
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := l.app.verify(ctx, "v000001", 0); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("haproxy never became healthy: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return l
}

func (l *lab) get(host string) (int, string, error) {
	req, _ := http.NewRequest(http.MethodGet, l.base+"/", nil)
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func teamApp(t *testing.T, body string) haproxy.Endpoint {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
	t.Cleanup(srv.Close)
	ap := netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "http://"))
	return haproxy.Endpoint{Addr: ap.Addr(), Port: ap.Port()}
}

func route(host string, slot int, ep haproxy.Endpoint) haproxy.Route {
	return haproxy.Route{Host: host, Slot: slot, Addr: ep.Addr, Port: ep.Port}
}

func TestRoutesAtRuntime(t *testing.T) {
	l := newLab(t)
	ctx := context.Background()
	a, b := teamApp(t, "team-a"), teamApp(t, "team-b")

	if err := l.app.SyncRoutes(ctx, 1, []haproxy.Route{route("a.hacka.test", 1, a), route("b.hacka.test", 2, b)}); err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]string{"a.hacka.test": "team-a", "b.hacka.test": "team-b"} {
		if code, body, err := l.get(host); err != nil || code != 200 || body != want {
			t.Fatalf("%s: %d %q %v, want 200 %q", host, code, body, err, want)
		}
	}
	if code, _, _ := l.get("nobody.hacka.test"); code != 404 {
		t.Fatalf("unknown host: %d, want 404", code)
	}

	// remove b, move a to another slot
	if err := l.app.SyncRoutes(ctx, 2, []haproxy.Route{route("a.hacka.test", 3, a)}); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := l.get("a.hacka.test"); code != 200 || body != "team-a" {
		t.Fatalf("a after move: %d %q", code, body)
	}
	if code, _, _ := l.get("b.hacka.test"); code != 404 {
		t.Fatalf("b after removal: %d, want 404", code)
	}

	// the routes survive a reload (they are in the rendered file and the map files)
	s := l.app.Settings()
	s.Version = "v000002"
	if r := l.app.ApplySettings(ctx, s); r.Outcome != Applied {
		t.Fatalf("reload: %s %v", r.Outcome, r.Err)
	}
	if code, body, _ := l.get("a.hacka.test"); code != 200 || body != "team-a" {
		t.Fatalf("a after reload: %d %q", code, body)
	}
}

// getFrom sends GET path with Host from a given loopback source address (127.0.0.0/8 all
// reaches the listener on 127.0.0.1), so a test can play an outside client.
func (l *lab) getFrom(src, host, path string) (int, string, error) {
	d := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(src)}, Timeout: 2 * time.Second}
	c := &http.Client{Transport: &http.Transport{DialContext: d.DialContext, DisableKeepAlives: true}}
	req, _ := http.NewRequest(http.MethodGet, l.base+path, nil)
	req.Host = host
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

// TestGatewayGuards checks the protections of the public frontend: the management endpoints
// are invisible from outside, and the per-Host rate limit stops one team's flood without
// touching the others.
func TestGatewayGuards(t *testing.T) {
	const limit = 20 // requests per 10 s per Host
	l := newLabWith(t, func(s *haproxy.Settings) { s.RateLimit = limit })
	ctx := context.Background()
	a, b := teamApp(t, "team-a"), teamApp(t, "team-b")
	if err := l.app.SyncRoutes(ctx, 1, []haproxy.Route{route("a.hacka.test", 1, a), route("b.hacka.test", 2, b)}); err != nil {
		t.Fatal(err)
	}

	// internal source: healthz answers; outside source: 404, same as any unknown path
	if code, body, err := l.getFrom("127.0.0.1", "x", "/__gw/healthz"); err != nil || code != 200 || !strings.HasPrefix(body, "ok gw1") {
		t.Fatalf("healthz from inside: %d %q %v", code, body, err)
	}
	for _, path := range []string{"/__gw/healthz", "/__gw/", "/__gw/anything"} {
		if code, body, err := l.getFrom("127.0.0.2", "a.hacka.test", path); err != nil || code != 404 || strings.Contains(body, "gw1") {
			t.Fatalf("%s from outside: %d %q %v, want a 404 that says nothing", path, code, body, err)
		}
	}

	// flood one Host from outside: over the limit it gets 429, the other team still answers
	var ok, limited int
	for range 3 * limit {
		code, _, err := l.getFrom("127.0.0.2", "a.hacka.test", "/")
		switch {
		case err != nil:
			t.Fatal(err)
		case code == 200:
			ok++
		case code == 429:
			limited++
		default:
			t.Fatalf("flood: unexpected %d", code)
		}
	}
	if ok > limit+1 || limited < limit {
		t.Fatalf("flood of %d requests: %d ok, %d limited; want at most %d ok", 3*limit, ok, limited, limit+1)
	}
	if code, body, err := l.getFrom("127.0.0.2", "b.hacka.test", "/"); err != nil || code != 200 || body != "team-b" {
		t.Fatalf("other team during the flood: %d %q %v", code, body, err)
	}
	// the Host header is matched case-insensitively and without the port: no bypass that way
	if code, _, _ := l.getFrom("127.0.0.2", "A.HACKA.TEST:80", "/"); code != 429 {
		t.Fatalf("flooded host written differently: %d, want 429", code)
	}
}

func TestInvalidConfigIsRejected(t *testing.T) {
	l := newLab(t)
	s := l.app.Settings()
	s.Version = "v000002"
	s.Extra = []string{"this-is-not-a-directive"}
	r := l.app.ApplySettings(context.Background(), s)
	if r.Outcome != Rejected {
		t.Fatalf("outcome %s, want rejected (err %v)", r.Outcome, r.Err)
	}
	t.Logf("rejected in %v: %v", r.Check, r.Err)
	if err := l.app.verify(context.Background(), "v000001", 0); err != nil {
		t.Fatalf("old version no longer serves: %v", err)
	}
}

func TestBrokenConfigRollsBack(t *testing.T) {
	l := newLab(t)
	s := l.app.Settings()
	s.Version = "v000002"
	// valid for haproxy -c, but healthz now fails: the agent must notice and roll back
	s.Extra = []string{"http-request return status 500 if { path /__gw/healthz }"}
	r := l.app.ApplySettings(context.Background(), s)
	if r.Outcome != RolledBack || r.Err == nil {
		t.Fatalf("outcome %s, want rolled_back (err %v)", r.Outcome, r.Err)
	}
	t.Logf("check %v, reload %v, verify %v (timeout), rollback %v, total %v",
		r.Check, r.Reload, r.Verify, r.Rollback, r.Total)
	if r.Rollback >= time.Second {
		t.Fatalf("rollback took %v, want < 1s", r.Rollback)
	}
	if err := l.app.verify(context.Background(), "v000001", 0); err != nil {
		t.Fatalf("last good version does not serve: %v", err)
	}
	if got := l.app.Settings().Version; got != "v000001" {
		t.Fatalf("last good = %s", got)
	}
}

func TestReloadDropsNoRequest(t *testing.T) {
	l := newLab(t)
	ctx := context.Background()
	if err := l.app.SyncRoutes(ctx, 1, []haproxy.Route{route("a.hacka.test", 1, teamApp(t, "team-a"))}); err != nil {
		t.Fatal(err)
	}

	var ok, failed atomic.Int64
	var firstErr atomic.Value
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(keepAlive bool) {
			defer wg.Done()
			c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: !keepAlive}}
			for {
				select {
				case <-stop:
					return
				default:
				}
				req, _ := http.NewRequest(http.MethodGet, l.base+"/", nil)
				req.Host = "a.hacka.test"
				resp, err := c.Do(req)
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode != 200 {
						err = fmt.Errorf("status %d", resp.StatusCode)
					}
				}
				if err != nil {
					failed.Add(1)
					firstErr.CompareAndSwap(nil, err.Error())
				} else {
					ok.Add(1)
				}
			}
		}(w%2 == 0)
	}

	const reloads = 20
	var worst time.Duration
	for i := 0; i < reloads; i++ {
		s := l.app.Settings()
		s.Version = "v" + strconv.Itoa(100+i)
		r := l.app.ApplySettings(ctx, s)
		if r.Outcome != Applied {
			t.Fatalf("reload %d: %s %v", i, r.Outcome, r.Err)
		}
		worst = max(worst, r.Total)
		time.Sleep(50 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	t.Logf("%d reloads (slowest apply %v): %d requests ok, %d failed", reloads, worst, ok.Load(), failed.Load())
	if failed.Load() > 0 {
		t.Fatalf("%d requests failed during reloads, first: %v", failed.Load(), firstErr.Load())
	}
}

// TestSyncAfterFailedReload: HAProxy unreachable (no sockets) makes both the runtime sync and
// the fallback reload fail. Once HAProxy is back, the same routes must really be served, and
// Generation must not claim the failed generation in between.
func TestSyncAfterFailedReload(t *testing.T) {
	l := newLab(t)
	ctx := context.Background()
	a := teamApp(t, "team-a")
	routes := []haproxy.Route{route("a.hacka.test", 1, a)}

	socks := []string{l.settings.AdminSocket, filepath.Join(l.settings.Dir, "master.sock")}
	for _, s := range socks {
		if err := os.Rename(s, s+".away"); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.app.SyncRoutes(ctx, 1, routes); err == nil {
		t.Fatal("SyncRoutes succeeded without HAProxy")
	}
	if g := l.app.Generation(); g != 0 {
		t.Fatalf("Generation = %d after a failed sync, want 0", g)
	}
	for _, s := range socks {
		if err := os.Rename(s+".away", s); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.app.SyncRoutes(ctx, 1, routes); err != nil {
		t.Fatal(err)
	}
	if code, body, err := l.get("a.hacka.test"); err != nil || code != 200 || body != "team-a" {
		t.Fatalf("a after recovery: %d %q %v, want 200 team-a", code, body, err)
	}
}

// TestApplyWithFailedRollback: HAProxy unreachable during a config apply makes the reload and
// the rollback fail. The outcome must say so, and once HAProxy is back a route sync must
// really set the servers (the runtime state was unknown).
func TestApplyWithFailedRollback(t *testing.T) {
	l := newLab(t)
	ctx := context.Background()
	a := teamApp(t, "team-a")
	routes := []haproxy.Route{route("a.hacka.test", 1, a)}
	if err := l.app.SyncRoutes(ctx, 1, routes); err != nil {
		t.Fatal(err)
	}
	socks := []string{l.settings.AdminSocket, filepath.Join(l.settings.Dir, "master.sock")}
	for _, s := range socks {
		if err := os.Rename(s, s+".away"); err != nil {
			t.Fatal(err)
		}
	}
	s := l.app.Settings()
	s.Version = "v000002"
	if r := l.app.ApplySettings(ctx, s); r.Outcome != Failed || r.Err == nil {
		t.Fatalf("outcome %s (err %v), want failed", r.Outcome, r.Err)
	}
	for _, s := range socks {
		if err := os.Rename(s+".away", s); err != nil {
			t.Fatal(err)
		}
	}
	b := teamApp(t, "team-b")
	routes = append(routes, route("b.hacka.test", 2, b))
	if err := l.app.SyncRoutes(ctx, 2, routes); err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]string{"a.hacka.test": "team-a", "b.hacka.test": "team-b"} {
		if code, body, err := l.get(host); err != nil || code != 200 || body != want {
			t.Fatalf("%s: %d %q %v, want 200 %q", host, code, body, err, want)
		}
	}
}
