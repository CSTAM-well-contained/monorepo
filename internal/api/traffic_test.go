package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/CSTAM-well-contained/monorepo/internal/model"
)

func lines(host string, statuses ...int) []model.TrafficLine {
	out := make([]model.TrafficLine, len(statuses))
	for i, s := range statuses {
		out[i] = model.TrafficLine{Host: host, Status: s, GW: "spoofed"}
	}
	return out
}

func TestTrafficRing(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	r := NewTrafficRing(5)
	r.now = func() time.Time { return clock }

	r.Add("gw1", model.TrafficBatch{Lines: lines("a.h.test", 200, 502, 200)})
	r.Add("gw2", model.TrafficBatch{Lines: lines("b.h.test", 200), Dropped: 4})
	p := r.Page(0, "", 100)
	if p.LastID != 4 || len(p.Lines) != 4 || p.Lines[0].ID != 1 || p.Lines[3].Host != "b.h.test" {
		t.Fatalf("page = %+v", p)
	}
	if p.Lines[0].GW != "gw1" || p.Lines[3].GW != "gw2" {
		t.Fatal("the gateway name must come from the URL, not from the line")
	}
	if g := p.Gateways["gw1"]; g.Requests != 3 || g.Errors != 1 || g.RPS != 0.3 {
		t.Fatalf("gw1 stats %+v", g)
	}
	if g := p.Gateways["gw2"]; g.Requests != 5 || g.Dropped != 4 {
		t.Fatalf("gw2 stats %+v (dropped lines count as requests)", g)
	}

	// capacity 5: the oldest lines go, ids keep growing
	r.Add("gw1", model.TrafficBatch{Lines: lines("a.h.test", 200, 200, 200)})
	p = r.Page(0, "", 100)
	if len(p.Lines) != 5 || p.Lines[0].ID != 3 || p.LastID != 7 {
		t.Fatalf("after overflow: ids %d..%d last %d", p.Lines[0].ID, p.Lines[len(p.Lines)-1].ID, p.LastID)
	}
	if p := r.Page(6, "", 100); len(p.Lines) != 1 || p.Lines[0].ID != 7 {
		t.Fatalf("after=6: %+v", p.Lines)
	}
	if p := r.Page(0, "", 2); len(p.Lines) != 2 || p.Lines[1].ID != 7 {
		t.Fatalf("limit keeps the newest: %+v", p.Lines)
	}
	if p := r.Page(0, "b.h.test", 100); len(p.Lines) != 1 || p.Lines[0].Host != "b.h.test" {
		t.Fatalf("host filter: %+v", p.Lines)
	}

	// 11 s later the window is empty
	clock = clock.Add(11 * time.Second)
	if g := r.Page(0, "", 1).Gateways["gw1"]; g.Requests != 0 || g.RPS != 0 {
		t.Fatalf("old buckets counted: %+v", g)
	}
}

func TestTrafficEndpoints(t *testing.T) {
	st := &stub{}
	body, _ := json.Marshal(model.TrafficBatch{Lines: lines("a.h.test", 200, 404)})
	w := do(t, st, "POST", "/v1/gateways/gw1/traffic", gwTok, string(body))
	if w.Code != 200 {
		t.Fatalf("push: %d %s", w.Code, w.Body)
	}
	if w := do(t, st, "POST", "/v1/gateways/gw1/traffic", adminTok, string(body)); w.Code != 401 {
		t.Fatalf("admin token on the gateway route: %d", w.Code)
	}
	tooMany, _ := json.Marshal(model.TrafficBatch{Lines: make([]model.TrafficLine, model.MaxTrafficBatch+1)})
	if w := do(t, st, "POST", "/v1/gateways/gw1/traffic", gwTok, string(tooMany)); w.Code != 400 {
		t.Fatalf("too many lines: %d", w.Code)
	}
	if w := do(t, st, "POST", "/v1/gateways/GW1/traffic", gwTok, string(body)); w.Code != 400 {
		t.Fatalf("bad gateway name: %d", w.Code)
	}
	for _, q := range []string{"?after=-1", "?limit=0", "?limit=501"} {
		if w := do(t, st, "GET", "/v1/traffic"+q, adminTok, ""); w.Code != 400 {
			t.Fatalf("GET /v1/traffic%s: %d", q, w.Code)
		}
	}
	if w := do(t, st, "GET", "/v1/traffic", gwTok, ""); w.Code != 401 {
		t.Fatalf("gateway token reads traffic: %d", w.Code)
	}
	// each do() builds a new server, so only the shape is checked here (the ring has its own test)
	w = do(t, st, "GET", "/v1/traffic?after=0&limit=10&host=a.h.test", adminTok, "")
	var p TrafficPage
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || w.Code != 200 || p.Lines == nil || p.Gateways == nil {
		t.Fatalf("page: %d %s", w.Code, w.Body)
	}
	huge := fmt.Sprintf(`{"lines":[{"host":%q}],"dropped":0}`, strings.Repeat("h", maxTrafficBody))
	if w := do(t, st, "POST", "/v1/gateways/gw1/traffic", gwTok, huge); w.Code != 400 {
		t.Fatalf("oversized body: %d", w.Code)
	}
}
