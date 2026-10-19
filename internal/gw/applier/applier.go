// Package applier keeps the local HAProxy at the desired state.
//
// Settings change with a reload: render, check with `haproxy -c`, swap the file, reload through
// the master CLI, verify that the new version serves (healthz + canary). Any failure restores
// the last good settings. Routes change at runtime, without a reload.
package applier

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/CSTAM-well-contained/monorepo/internal/gw/haproxy"
)

// Outcome of an apply.
type Outcome string

const (
	// Applied means the new config serves.
	Applied Outcome = "applied"
	// Rejected means haproxy -c refused it; nothing changed.
	Rejected Outcome = "rejected"
	// RolledBack means it was loaded, failed verification, and the last good config serves again.
	RolledBack Outcome = "rolled_back"
)

// Result reports what happened and how long each phase took.
type Result struct {
	Version  string
	Outcome  Outcome
	Err      error
	Check    time.Duration
	Reload   time.Duration
	Verify   time.Duration
	Rollback time.Duration // from the failed verification to the old version serving again
	Total    time.Duration
}

// Config wires the applier to one HAProxy.
type Config struct {
	Runtime haproxy.Runtime
	Master  haproxy.Master
	Checker haproxy.Checker
	// HealthURL is http://127.0.0.1:<port>/__gw/healthz; the canary uses the same address.
	HealthURL     string
	VerifyTimeout time.Duration // default 500 ms
	Log           *slog.Logger
}

// Applier serialises all changes to one HAProxy.
type Applier struct {
	cfg  Config
	http *http.Client

	mu         sync.Mutex
	lastGood   haproxy.Settings
	routes     []haproxy.Route
	slots      map[int]haproxy.Endpoint
	generation uint64
}

// New returns an applier whose last good settings are s. Call Bootstrap before starting HAProxy.
func New(cfg Config, s haproxy.Settings) *Applier {
	if cfg.VerifyTimeout == 0 {
		cfg.VerifyTimeout = 500 * time.Millisecond
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &Applier{
		cfg:      cfg,
		lastGood: s,
		slots:    map[int]haproxy.Endpoint{},
		// a fresh connection per request: after a reload it must reach the new worker
		http: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
	}
}

// Bootstrap writes the static files, the maps and haproxy.cfg for the initial settings, and
// checks the result. HAProxy (systemd) is started afterwards.
func (a *Applier) Bootstrap(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.lastGood
	if err := haproxy.InstallStatic(s.Dir); err != nil {
		return fmt.Errorf("install static files: %w", err)
	}
	if err := haproxy.WriteMaps(s.Dir, a.generation, a.routes); err != nil {
		return fmt.Errorf("write maps: %w", err)
	}
	if err := a.writeChecked(ctx, s, haproxy.CfgPath(s.Dir)); err != nil {
		return err
	}
	return nil
}

// Settings returns the settings that currently serve.
func (a *Applier) Settings() haproxy.Settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastGood
}

// ApplySettings loads new settings with a hitless reload and rolls back on any failure.
func (a *Applier) ApplySettings(ctx context.Context, s haproxy.Settings) Result {
	a.mu.Lock()
	defer a.mu.Unlock()
	start := time.Now()
	res := Result{Version: s.Version}
	done := func(o Outcome, err error) Result {
		res.Outcome, res.Err, res.Total = o, err, time.Since(start)
		a.cfg.Log.Info("haproxy apply", "version", s.Version, "outcome", o, "err", errString(err),
			"check_ms", ms(res.Check), "reload_ms", ms(res.Reload), "verify_ms", ms(res.Verify),
			"rollback_ms", ms(res.Rollback), "total_ms", ms(res.Total))
		return res
	}

	// 1. render and check a candidate file; the live file is untouched until this passes
	live := haproxy.CfgPath(s.Dir)
	cand := live + ".new"
	t := time.Now()
	err := a.writeChecked(ctx, s, cand)
	res.Check = time.Since(t)
	if err != nil {
		_ = os.Remove(cand)
		return done(Rejected, err)
	}

	// 2. swap and reload
	if err := os.Rename(cand, live); err != nil {
		return done(Rejected, fmt.Errorf("swap config: %w", err))
	}
	t = time.Now()
	err = a.cfg.Master.Reload(ctx)
	res.Reload = time.Since(t)

	// 3. verify the new version really serves
	if err == nil {
		t = time.Now()
		err = a.verify(ctx, s.Version, a.generation)
		res.Verify = time.Since(t)
	}
	if err == nil {
		a.lastGood = s
		return done(Applied, nil)
	}

	// 4. rollback to the last good settings
	t = time.Now()
	if rbErr := a.restore(ctx); rbErr != nil {
		res.Rollback = time.Since(t)
		return done(RolledBack, fmt.Errorf("%w; ROLLBACK FAILED: %w", err, rbErr))
	}
	res.Rollback = time.Since(t)
	return done(RolledBack, err)
}

// restore re-renders the last good settings with the current routes, reloads and verifies.
func (a *Applier) restore(ctx context.Context) error {
	s := a.lastGood
	data, err := haproxy.Render(s, a.routes)
	if err != nil {
		return err
	}
	if err := haproxy.WriteFileAtomic(haproxy.CfgPath(s.Dir), data); err != nil {
		return err
	}
	if err := a.cfg.Master.Reload(ctx); err != nil {
		return err
	}
	return a.verify(ctx, s.Version, a.generation)
}

// SyncRoutes makes HAProxy route exactly these hosts, at runtime, without a reload.
// Order: new endpoints ready, then the atomic map swap, then park the freed slots, so no
// request ever reaches a placeholder or a stale team.
func (a *Applier) SyncRoutes(ctx context.Context, generation uint64, routes []haproxy.Route) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.lastGood
	table, err := haproxy.SlotTable(routes, s.Slots)
	if err != nil {
		return err
	}
	routes = append([]haproxy.Route(nil), routes...)

	err = a.runtimeSync(ctx, s, generation, routes, table)
	if err != nil {
		// the runtime state is now unknown: write files and reload, which is always consistent
		a.cfg.Log.Warn("runtime route sync failed, reloading", "generation", generation, "err", err.Error())
		if err := haproxy.WriteMaps(s.Dir, generation, routes); err != nil {
			return fmt.Errorf("write maps: %w", err)
		}
		prev := a.generation
		a.routes, a.slots, a.generation = routes, table, generation
		if err := a.restore(ctx); err != nil {
			// HAProxy may still run the old servers: forget the slot table so the next sync
			// sets every server again, and do not claim a generation HAProxy never confirmed.
			a.slots, a.generation = nil, prev
			return fmt.Errorf("route sync by reload: %w", err)
		}
		return nil
	}
	a.routes, a.slots, a.generation = routes, table, generation

	// persist, so a later reload or restart serves the same routes
	if err := haproxy.WriteMaps(s.Dir, generation, routes); err != nil {
		return fmt.Errorf("write maps: %w", err)
	}
	data, err := haproxy.Render(s, routes)
	if err != nil {
		return err
	}
	if err := haproxy.WriteFileAtomic(haproxy.CfgPath(s.Dir), data); err != nil {
		return err
	}
	return a.verify(ctx, s.Version, generation)
}

func (a *Applier) runtimeSync(ctx context.Context, s haproxy.Settings, gen uint64, routes []haproxy.Route, table map[int]haproxy.Endpoint) error {
	rt := a.cfg.Runtime
	for slot, ep := range table {
		if old, ok := a.slots[slot]; ok && old == ep {
			continue
		}
		if err := rt.SetServer(ctx, slot, ep); err != nil {
			return err
		}
	}
	if err := rt.ReplaceMap(ctx, haproxy.HostsPath(s.Dir), haproxy.HostsMap(routes)); err != nil {
		return err
	}
	if err := rt.ReplaceMap(ctx, haproxy.MetaPath(s.Dir), haproxy.MetaMap(gen)); err != nil {
		return err
	}
	for slot := range a.slots {
		if _, still := table[slot]; !still {
			if err := rt.ParkServer(ctx, slot); err != nil {
				return err
			}
		}
	}
	return nil
}

// Generation is the last route generation HAProxy confirmed.
func (a *Applier) Generation() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.generation
}

func (a *Applier) writeChecked(ctx context.Context, s haproxy.Settings, path string) error {
	data, err := haproxy.Render(s, a.routes)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	if err := haproxy.WriteFileAtomic(path, data); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := a.cfg.Checker.Check(ctx, path); err != nil {
		return err
	}
	return nil
}

// verify polls healthz until it reports version and generation, then requests the canary.
func (a *Applier) verify(ctx context.Context, version string, gen uint64) error {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.VerifyTimeout)
	defer cancel()
	var last error
	for {
		last = a.probe(ctx, version, gen)
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("verify %s: %w", version, last)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (a *Applier) probe(ctx context.Context, version string, gen uint64) error {
	code, hdr, err := a.get(ctx, a.cfg.HealthURL, "")
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("healthz status %d", code)
	}
	if v := hdr.Get("X-Config-Version"); v != version {
		return fmt.Errorf("healthz serves version %q, want %q", v, version)
	}
	if g := hdr.Get("X-Routes-Generation"); g != strconv.FormatUint(gen, 10) {
		return fmt.Errorf("healthz serves generation %q, want %d", g, gen)
	}
	code, _, err = a.get(ctx, a.cfg.HealthURL, haproxy.CanaryHost)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("canary status %d", code)
	}
	return nil
}

// get returns the status and headers; the body is drained and closed.
func (a *Applier) get(ctx context.Context, url, host string) (int, http.Header, error) {
	if host != "" {
		url = urlRoot(url) + "/"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	if host != "" {
		req.Host = host
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, resp.Header, nil
}

// urlRoot turns http://h:p/__gw/healthz into http://h:p.
func urlRoot(u string) string {
	n := 0
	for i, c := range u {
		if c == '/' {
			n++
			if n == 3 {
				return u[:i]
			}
		}
	}
	return u
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
