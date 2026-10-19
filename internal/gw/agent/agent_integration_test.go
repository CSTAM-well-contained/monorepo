//go:build integration

package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CSTAM-well-contained/monorepo/internal/gw/applier"
	"github.com/CSTAM-well-contained/monorepo/internal/gw/haproxy"
	"github.com/CSTAM-well-contained/monorepo/internal/model"
)

const haproxyImage = "haproxy:3.2"

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func dockerRun(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// app is a team app on 127.0.0.1 that answers its name.
func app(t *testing.T, name string) model.Route {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, name) }))
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	var p int
	fmt.Sscan(port, &p)
	return model.Route{Addr: host, Port: p}
}

// TestAgentDrivesRealHAProxy: fake cpd -> agent -> applier -> HAProxy in Docker -> team apps.
func TestAgentDrivesRealHAProxy(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	dir, err := os.MkdirTemp("", "cstam-gwagent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	canary := httptest.NewServer(Canary())
	t.Cleanup(canary.Close)
	port := freePort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	s := haproxy.Settings{
		Version: "v1", GWName: "gw1", Dir: dir, AdminSocket: filepath.Join(dir, "admin.sock"), SocketMode: "600",
		HTTPBind: fmt.Sprintf("127.0.0.1:%d", port), CanaryAddr: strings.TrimPrefix(canary.URL, "http://"),
		InternalSources: []string{"127.0.0.1"}, Slots: 8, RateLimit: 100000,
	}
	ctr := fmt.Sprintf("cstam-it-gwagent-%d", time.Now().UnixNano())
	dockerRun(t, "run", "-d", "--rm", "--name", ctr, "--network", "host",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-v", dir+":"+dir, "--entrypoint", "sleep", haproxyImage, "infinity")
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", ctr).Run() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ap := applier.New(applier.Config{
		Runtime:   haproxy.Runtime{Socket: haproxy.Socket{Path: s.AdminSocket}},
		Master:    haproxy.Master{Socket: haproxy.Socket{Path: filepath.Join(dir, "master.sock"), Timeout: 10 * time.Second}},
		Checker:   haproxy.Checker{Argv: []string{"docker", "exec", ctr, "haproxy"}},
		HealthURL: base + "/__gw/healthz",
	}, s)
	if err := ap.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	dockerRun(t, "exec", "-d", ctr, "haproxy", "-W", "-db",
		"-S", filepath.Join(dir, "master.sock")+",mode,600", "-f", haproxy.CfgPath(dir))

	a, b := app(t, "team-a"), app(t, "team-b")
	a.Host, a.Slot = "a.h1.example.org", 1
	b.Host, b.Slot = "b.h1.example.org", 2
	cpd := &fakeCPD{desired: model.Desired{Generation: 1, Routes: []model.Route{a}}}
	srv := httptest.NewServer(cpd)
	t.Cleanup(srv.Close)
	ag, err := New(Config{Name: "gw1", CPD: srv.URL, Token: authz, RunDir: dir, StateFile: filepath.Join(dir, "desired.json"),
		Wait: 200 * time.Millisecond}, ap)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = ag.Run(ctx) }()
	defer func() { cancel(); <-done }()

	get := func(host string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, base+"/", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	serves := func(host, want string) func() bool {
		return func() bool { code, body := get(host); return code == 200 && body == want }
	}

	eventually10 := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	eventually10("team a routed", serves(a.Host, "team-a"))
	eventually10("ack 1", func() bool { x, _ := cpd.lastAck(); return x.Generation == 1 })

	start := time.Now()
	cpd.set(model.Desired{Generation: 2, Routes: []model.Route{a, b}})
	eventually10("team b routed", serves(b.Host, "team-b"))
	t.Logf("new team routed %s after the cpd change (no reload)", time.Since(start).Round(time.Millisecond))

	cpd.set(model.Desired{Generation: 3, Routes: []model.Route{b}})
	eventually10("team a unrouted", func() bool { code, _ := get(a.Host); return code != 200 })
	eventually10("ack 3", func() bool { x, _ := cpd.lastAck(); return x.Generation == 3 })
	if code, body := get(b.Host); code != 200 || body != "team-b" {
		t.Fatalf("team b after removing a: %d %q", code, body)
	}
	if ready, err := os.ReadFile(filepath.Join(dir, "ready")); err != nil || string(ready) != "synced\n" {
		t.Fatalf("ready file %q %v", ready, err)
	}
}
