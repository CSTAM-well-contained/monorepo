package main

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// gateway answers like the demo gateway: the page of the team named by the Host header.
func gateway(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL + "/"
}

func teamPage(w http.ResponseWriter, r *http.Request) {
	team, _, _ := strings.Cut(r.Host, ".")
	w.Header().Set("Content-Type", "text/plain")
	_, _ = fmt.Fprintf(w, "team %s of hack at 10.20.0.32\n", html.EscapeString(team))
}

func TestRunLoadCountsSuccess(t *testing.T) {
	cfg := loadConfig{target: gateway(t, teamPage), hosts: []string{"t1.hack.test", "t2.hack.test"},
		conns: 4, dur: 300 * time.Millisecond, timeout: time.Second, keepAlive: true, checkBody: true}
	r := runLoad(context.Background(), cfg)
	if r.OK == 0 || r.Errors != 0 || r.OK != r.Requests {
		t.Fatalf("want only successes, got %+v", r)
	}
	if r.P50 <= 0 || r.P99 < r.P50 || r.Max < r.P99 || r.RPS <= 0 {
		t.Fatalf("bad latency summary: %+v", r)
	}
}

func TestRunLoadDetectsMisrouting(t *testing.T) {
	wrong := func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "team other of hack at 10.20.0.33\n") }
	cfg := loadConfig{target: gateway(t, wrong), hosts: []string{"t1.hack.test"},
		conns: 2, dur: 100 * time.Millisecond, timeout: time.Second, keepAlive: true, checkBody: true}
	r := runLoad(context.Background(), cfg)
	if r.OK != 0 || r.Misrouted == 0 || r.Misrouted != r.Errors {
		t.Fatalf("want every answer counted as misrouted, got %+v", r)
	}
}

func TestRunLoadCountsStatusAndTimeouts(t *testing.T) {
	var n atomic.Int64
	h := func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) % 3 {
		case 0:
			http.Error(w, "down", http.StatusServiceUnavailable)
		case 1:
			time.Sleep(200 * time.Millisecond) // longer than the timeout
		default:
			teamPage(w, r)
		}
	}
	cfg := loadConfig{target: gateway(t, h), hosts: []string{"t1.hack.test"},
		conns: 3, dur: 500 * time.Millisecond, timeout: 50 * time.Millisecond, keepAlive: true, checkBody: true}
	r := runLoad(context.Background(), cfg)
	if r.Status["503"] == 0 || r.Status["timeout"] == 0 || r.OK == 0 {
		t.Fatalf("want 503s, timeouts and successes, got %v", r.Status)
	}
	if r.OK+r.Errors != r.Requests {
		t.Fatalf("counts do not add up: %+v", r)
	}
}

func TestRunLoadKeepsTheRate(t *testing.T) {
	cfg := loadConfig{target: gateway(t, teamPage), hosts: []string{"t1.hack.test"},
		conns: 4, dur: time.Second, timeout: time.Second, keepAlive: true, checkBody: true, rate: 200}
	r := runLoad(context.Background(), cfg)
	if r.Requests < 170 || r.Requests > 210 {
		t.Fatalf("rate 200/s for 1 s sent %d requests, want about 200", r.Requests)
	}
}

func TestSummarizeGap(t *testing.T) {
	s := func(sec int, ok bool) sample {
		return sample{at: time.Duration(sec)*time.Second + time.Millisecond, ok: ok, status: "x"}
	}
	per := [][]sample{{s(0, true), s(1, false), s(2, false), s(3, true), s(4, false)}}
	r := summarize(per, 5*time.Second)
	if r.Gap != 2 {
		t.Fatalf("gap = %d, want 2 (seconds 1-2)", r.Gap)
	}
	if want := 3 * time.Second; r.Stall != want {
		t.Fatalf("stall = %v, want %v (success at 0 s then 3 s)", r.Stall, want)
	}
}

func TestPercentile(t *testing.T) {
	d := []time.Duration{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if pct(d, 0.5) != 6 || pct(d, 0.99) != 10 || pct(nil, 0.5) != 0 {
		t.Fatalf("pct: %v %v", pct(d, 0.5), pct(d, 0.99))
	}
}
