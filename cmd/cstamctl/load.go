package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"time"
)

// loadConfig is one load test: conns workers send GET target with each Host in turn.
type loadConfig struct {
	target    string
	hosts     []string
	conns     int
	dur       time.Duration
	timeout   time.Duration
	keepAlive bool
	rate      float64 // total requests per second, 0 = as fast as possible
	checkBody bool    // the demo page starts with "team <first label of Host> "
}

// second counts the answers that completed during one second of the run.
type second struct {
	OK     int `json:"ok"`
	Errors int `json:"errors"`
}

type loadResult struct {
	Requests  int            `json:"requests"`
	OK        int            `json:"ok"`
	Errors    int            `json:"errors"`
	Misrouted int            `json:"misrouted"` // 200 but the page of another team
	Status    map[string]int `json:"status"`    // "200", "503", "timeout", "conn" ...
	Elapsed   time.Duration  `json:"elapsed_ns"`
	RPS       float64        `json:"rps"`
	P50       time.Duration  `json:"p50_ns"`
	P90       time.Duration  `json:"p90_ns"`
	P99       time.Duration  `json:"p99_ns"`
	Max       time.Duration  `json:"max_ns"`
	Timeline  []second       `json:"timeline"`
	// Gap is the longest run of seconds with errors and no success (0 when none).
	Gap int `json:"gap_seconds"`
	// Stall is the longest time between two successful answers (or from the start / to the
	// end of the run): the outage seen by clients, even when requests hang instead of failing.
	Stall time.Duration `json:"stall_ns"`
}

type sample struct {
	at     time.Duration // completion time since start
	lat    time.Duration
	status string
	ok     bool
	wrong  bool
}

func runLoad(ctx context.Context, cfg loadConfig) loadResult {
	tr := &http.Transport{
		MaxIdleConns:        cfg.conns,
		MaxIdleConnsPerHost: cfg.conns,
		DisableKeepAlives:   !cfg.keepAlive,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: cfg.timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	ctx, cancel := context.WithTimeout(ctx, cfg.dur)
	defer cancel()
	start := time.Now()
	per := make([][]sample, cfg.conns)
	var wg sync.WaitGroup
	for w := range cfg.conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// With a rate, each worker sends one request every conns/rate seconds on a fixed
			// schedule (offset per worker); a late request is sent at once, never in a burst.
			var every time.Duration
			if cfg.rate > 0 {
				every = time.Duration(float64(cfg.conns) / cfg.rate * float64(time.Second))
			}
			next := start.Add(every * time.Duration(w) / time.Duration(cfg.conns))
			for i := w; ctx.Err() == nil; i += cfg.conns {
				if every > 0 {
					if d := time.Until(next); d > 0 {
						select {
						case <-ctx.Done():
							return
						case <-time.After(d):
						}
					}
					next = maxTime(next.Add(every), time.Now())
				}
				host := cfg.hosts[i%len(cfg.hosts)]
				s := probe(ctx, client, cfg, host)
				if ctx.Err() != nil && !s.ok {
					return // cut by the end of the run, not a failure
				}
				s.at = time.Since(start)
				per[w] = append(per[w], s)
			}
		}()
	}
	wg.Wait()
	return summarize(per, time.Since(start))
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func probe(ctx context.Context, client *http.Client, cfg loadConfig, host string) sample {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.target, nil)
	if err != nil {
		return sample{status: "request"}
	}
	req.Host = host
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		st := "conn"
		var te interface{ Timeout() bool }
		if errors.As(err, &te) && te.Timeout() {
			st = "timeout"
		}
		return sample{lat: time.Since(t0), status: st}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	s := sample{lat: time.Since(t0), status: fmt.Sprint(resp.StatusCode)}
	if err != nil {
		s.status = "body"
		return s
	}
	s.ok = resp.StatusCode == http.StatusOK
	if s.ok && cfg.checkBody {
		team, _, _ := strings.Cut(host, ".")
		if !strings.HasPrefix(string(body), "team "+team+" ") {
			s.ok, s.wrong = false, true
		}
	}
	return s
}

func summarize(per [][]sample, elapsed time.Duration) loadResult {
	r := loadResult{Status: map[string]int{}, Elapsed: elapsed}
	var lats, oks []time.Duration
	r.Timeline = make([]second, int(elapsed/time.Second)+1)
	for _, ss := range per {
		for _, s := range ss {
			r.Requests++
			r.Status[s.status]++
			sec := &r.Timeline[min(int(s.at/time.Second), len(r.Timeline)-1)]
			switch {
			case s.ok:
				r.OK++
				sec.OK++
				lats = append(lats, s.lat)
				oks = append(oks, s.at)
			default:
				r.Errors++
				sec.Errors++
				if s.wrong {
					r.Misrouted++
				}
			}
		}
	}
	if elapsed > 0 {
		r.RPS = float64(r.OK) / elapsed.Seconds()
	}
	slices.Sort(lats)
	slices.Sort(oks)
	prev := time.Duration(0)
	for _, at := range append(oks, elapsed) {
		r.Stall = max(r.Stall, at-prev)
		prev = at
	}
	r.P50, r.P90, r.P99 = pct(lats, 0.50), pct(lats, 0.90), pct(lats, 0.99)
	if n := len(lats); n > 0 {
		r.Max = lats[n-1]
	}
	run := 0
	for _, s := range r.Timeline {
		if s.Errors > 0 && s.OK == 0 {
			run++
			r.Gap = max(r.Gap, run)
		} else {
			run = 0
		}
	}
	return r
}

// pct is the nearest-rank percentile of sorted durations.
func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(float64(len(sorted)-1)*p+0.5)]
}

func loadCmd(args []string) error {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	var cfg loadConfig
	hosts := fs.String("hosts", "", "comma-separated Host headers, used in turn (required)")
	fs.StringVar(&cfg.target, "url", "http://10.20.0.10/", "URL to request (the VIP)")
	fs.IntVar(&cfg.conns, "c", 32, "concurrent workers")
	fs.DurationVar(&cfg.dur, "d", 20*time.Second, "duration")
	fs.DurationVar(&cfg.timeout, "timeout", 2*time.Second, "per-request timeout")
	fs.BoolVar(&cfg.keepAlive, "keepalive", true, "reuse connections")
	fs.Float64Var(&cfg.rate, "rate", 0, "total requests per second (0 = as fast as possible)")
	fs.BoolVar(&cfg.checkBody, "check-body", true, `200 must be the page of the requested team ("team <name> ...")`)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	timeline := fs.Bool("timeline", false, "print requests per second")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for _, h := range strings.Split(*hosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			cfg.hosts = append(cfg.hosts, h)
		}
	}
	if len(cfg.hosts) == 0 || cfg.conns < 1 || cfg.dur <= 0 || cfg.rate < 0 {
		return errors.New("load: -hosts, -c >= 1, -d > 0 and -rate >= 0 are required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	r := runLoad(ctx, cfg)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	ms := func(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000) }
	fmt.Printf("%d hosts, %d workers, %s, keep-alive %v, target rate %v/s (0 = max)\n", len(cfg.hosts), cfg.conns, r.Elapsed.Round(time.Millisecond), cfg.keepAlive, cfg.rate)
	fmt.Printf("requests %d, ok %d, errors %d (misrouted %d), %.0f req/s\n", r.Requests, r.OK, r.Errors, r.Misrouted, r.RPS)
	fmt.Printf("latency p50 %s, p90 %s, p99 %s, max %s\n", ms(r.P50), ms(r.P90), ms(r.P99), ms(r.Max))
	keys := make([]string, 0, len(r.Status))
	for k := range r.Status {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Printf("  %s: %d\n", k, r.Status[k])
	}
	if *timeline || r.Errors > 0 {
		fmt.Printf("longest time without a successful answer: %s\n", ms(r.Stall))
		for i, s := range r.Timeline {
			fmt.Printf("  t=%2ds ok %6d errors %5d\n", i, s.OK, s.Errors)
		}
	}
	if r.Errors > 0 {
		return fmt.Errorf("load: %d errors", r.Errors)
	}
	return nil
}
