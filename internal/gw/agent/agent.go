// Package agent is the loop of gwagent: long-poll the desired routes from cpd, apply them to
// HAProxy at runtime, acknowledge the generation, and report the VRRP state.
//
// Readiness: the ready file (read by keepalived's chk_gateway) is removed at start and created
// after the first successful sync, so a stale gateway cannot take the VIP. If cpd cannot be
// reached within ReadyAfter, the file is created anyway with the last saved routes: a slightly
// stale gateway beats no gateway, and the IPAM does not reuse an IP before every live gateway
// acked its removal.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/CSTAM-well-contained/monorepo/internal/gw/haproxy"
	"github.com/CSTAM-well-contained/monorepo/internal/gw/keepalived"
	"github.com/CSTAM-well-contained/monorepo/internal/model"
)

// Applier is what the agent needs from applier.Applier.
type Applier interface {
	SyncRoutes(ctx context.Context, generation uint64, routes []haproxy.Route) error
	Generation() uint64
	Settings() haproxy.Settings
}

// Config of the agent.
type Config struct {
	Name      string // gw1 or gw2
	CPD       string // base URL of cpd, e.g. http://10.20.0.5:8080
	Token     string // gateway bearer token
	RunDir    string // holds ready and vrrp.state (/run/cstam)
	StateFile string // last applied routes, survives restarts (/var/lib/cstam/desired.json)

	Wait       time.Duration // long-poll wait asked from cpd (default 25 s)
	ReadyAfter time.Duration // create the ready file anyway after this (default 10 s)
	VRRPPoll   time.Duration // how often vrrp.state is read (default 500 ms)
	MaxBackoff time.Duration // between failed calls to cpd (default 15 s)
	Log        *slog.Logger
}

// Agent runs one gateway.
type Agent struct {
	cfg  Config
	app  Applier
	http *http.Client

	mu      sync.Mutex
	synced  bool // applied a generation from cpd since start
	ready   bool
	vrrp    string
	ackSent int64 // last generation acked, for logs
}

// New checks the config and returns an agent.
func New(cfg Config, app Applier) (*Agent, error) {
	u, err := url.Parse(cfg.CPD)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("agent: cpd url %q must be http(s)://host[:port]", cfg.CPD)
	}
	if cfg.Name == "" || cfg.Token == "" || cfg.RunDir == "" || cfg.StateFile == "" {
		return nil, errors.New("agent: name, token, run dir and state file are required")
	}
	if cfg.Wait <= 0 {
		cfg.Wait = 25 * time.Second
	}
	if cfg.ReadyAfter <= 0 {
		cfg.ReadyAfter = 10 * time.Second
	}
	if cfg.VRRPPoll <= 0 {
		cfg.VRRPPoll = 500 * time.Millisecond
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 15 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &Agent{cfg: cfg, app: app, http: &http.Client{Timeout: cfg.Wait + 10*time.Second}}, nil
}

func (a *Agent) readyPath() string { return filepath.Join(a.cfg.RunDir, "ready") }

// Run loops until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	if err := os.Remove(a.readyPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("agent: remove ready file: %w", err)
	}
	a.restore(ctx)
	start := time.Now()

	go a.watchVRRP(ctx)

	after := int64(-1) // first call: answer at once
	backoff := time.Second
	for ctx.Err() == nil {
		if !a.isReady() && time.Since(start) >= a.cfg.ReadyAfter {
			a.markReady("stale")
		}
		d, err := a.fetch(ctx, after)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			a.cfg.Log.Warn("fetch desired state", "err", err.Error(), "retry_in", backoff.String())
			if !sleep(ctx, min(backoff, max(time.Until(start.Add(a.cfg.ReadyAfter)), 50*time.Millisecond))) {
				break
			}
			backoff = min(2*backoff, a.cfg.MaxBackoff)
			continue
		}
		if err := a.apply(ctx, d); err != nil {
			a.cfg.Log.Error("apply routes", "generation", d.Generation, "err", err.Error())
			if !sleep(ctx, backoff) {
				break
			}
			backoff = min(2*backoff, a.cfg.MaxBackoff)
		} else {
			backoff = time.Second
		}
		after = int64(a.app.Generation()) //nolint:gosec // generations are small
		if err := a.ack(ctx); err != nil && ctx.Err() == nil {
			a.cfg.Log.Warn("ack", "err", err.Error())
		}
	}
	return nil
}

// apply syncs HAProxy to d unless d is older than what serves.
func (a *Agent) apply(ctx context.Context, d model.Desired) error {
	cur := a.app.Generation()
	if d.Generation < 0 {
		return fmt.Errorf("negative generation %d", d.Generation)
	}
	gen := uint64(d.Generation)
	a.mu.Lock()
	synced := a.synced
	a.mu.Unlock()
	switch {
	case gen < cur:
		a.cfg.Log.Warn("cpd is behind this gateway, not applying", "cpd_generation", gen, "applied", cur)
		a.markReady("synced")
		return nil
	case gen == cur && synced:
		return nil
	}
	routes, err := Convert(d.Routes)
	if err == nil {
		err = a.app.SyncRoutes(ctx, gen, routes)
	}
	if err != nil {
		return err
	}
	a.cfg.Log.Info("routes applied", "generation", gen, "routes", len(routes))
	if err := save(a.cfg.StateFile, d); err != nil {
		a.cfg.Log.Warn("save state", "err", err.Error())
	}
	a.mu.Lock()
	a.synced = true
	a.mu.Unlock()
	a.markReady("synced")
	return nil
}

// restore applies the routes saved before a restart (HAProxy may be up already).
func (a *Agent) restore(ctx context.Context) {
	data, err := os.ReadFile(a.cfg.StateFile)
	if err != nil {
		return
	}
	var d model.Desired
	if err := json.Unmarshal(data, &d); err != nil {
		a.cfg.Log.Warn("saved state unreadable", "err", err.Error())
		return
	}
	routes, err := Convert(d.Routes)
	if err == nil && d.Generation >= 0 {
		err = a.app.SyncRoutes(ctx, uint64(d.Generation), routes)
	}
	if err != nil {
		a.cfg.Log.Warn("restore saved routes", "err", err.Error())
		return
	}
	a.cfg.Log.Info("saved routes restored", "generation", d.Generation, "routes", len(routes))
}

// Convert checks the routes from cpd and turns them into HAProxy routes.
func Convert(in []model.Route) ([]haproxy.Route, error) {
	out := make([]haproxy.Route, 0, len(in))
	for _, r := range in {
		addr, err := netip.ParseAddr(r.Addr)
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", r.Host, err)
		}
		if r.Port < 1 || r.Port > 65535 {
			return nil, fmt.Errorf("route %q: port %d", r.Host, r.Port)
		}
		out = append(out, haproxy.Route{Host: r.Host, Slot: r.Slot, Addr: addr, Port: uint16(r.Port)})
	}
	return out, nil
}

func (a *Agent) isReady() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ready
}

func (a *Agent) markReady(how string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ready {
		return
	}
	if err := os.WriteFile(a.readyPath(), []byte(how+"\n"), 0o644); err != nil { //nolint:gosec // read by keepalived
		a.cfg.Log.Error("write ready file", "err", err.Error())
		return
	}
	a.ready = true
	a.cfg.Log.Info("gateway ready", "how", how)
}

// watchVRRP acks at once when the VRRP state changes (the dashboard shows failovers live).
func (a *Agent) watchVRRP(ctx context.Context) {
	t := time.NewTicker(a.cfg.VRRPPoll)
	defer t.Stop()
	for {
		s := a.readVRRP()
		a.mu.Lock()
		changed := s != a.vrrp
		a.vrrp = s
		a.mu.Unlock()
		if changed {
			a.cfg.Log.Info("vrrp state", "state", s)
			if err := a.ack(ctx); err != nil && ctx.Err() == nil {
				a.cfg.Log.Warn("ack vrrp change", "err", err.Error())
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *Agent) readVRRP() string {
	st, err := keepalived.ReadState(filepath.Join(a.cfg.RunDir, "vrrp.state"))
	if err != nil {
		return ""
	}
	switch st.Name {
	case "MASTER", "BACKUP", "FAULT", "STOP":
		return st.Name
	}
	return ""
}

func (a *Agent) fetch(ctx context.Context, after int64) (model.Desired, error) {
	q := url.Values{"wait": {a.cfg.Wait.String()}}
	if after >= 0 {
		q.Set("after", strconv.FormatInt(after, 10))
	}
	var d model.Desired
	err := a.call(ctx, http.MethodGet, "/v1/gateways/"+a.cfg.Name+"/desired?"+q.Encode(), nil, &d)
	return d, err
}

func (a *Agent) ack(ctx context.Context) error {
	a.mu.Lock()
	vrrp := a.vrrp
	a.mu.Unlock()
	gen := int64(a.app.Generation()) //nolint:gosec // generations are small
	body := model.Ack{Generation: gen, ConfigVersion: a.app.Settings().Version, VRRPState: vrrp}
	if err := a.call(ctx, http.MethodPost, "/v1/gateways/"+a.cfg.Name+"/ack", body, nil); err != nil {
		return err
	}
	a.mu.Lock()
	if gen != a.ackSent {
		a.cfg.Log.Info("acked", "generation", gen)
	}
	a.ackSent = gen
	a.mu.Unlock()
	return nil
}

func (a *Agent) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.cfg.CPD+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: status %d: %.200s", method, path, resp.StatusCode, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	return nil
}

func save(path string, d model.Desired) error {
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return haproxy.WriteFileAtomic(path, data)
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Canary answers the HAProxy canary backend (be_slot000) that the applier checks after
// every reload.
func Canary() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "canary\n")
	})
}
