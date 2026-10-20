package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/CSTAM-well-contained/monorepo/internal/gw/logs"
	"github.com/CSTAM-well-contained/monorepo/internal/model"
)

// TrafficEntry is one traffic line as the dashboard reads it.
type TrafficEntry struct {
	ID int64     `json:"id"`
	At time.Time `json:"at"` // when cpd received it (lines come in batches about once a second)
	model.TrafficLine
}

// GatewayTraffic sums the last 10 seconds of one gateway.
type GatewayTraffic struct {
	Requests int     `json:"requests_10s"` // dropped lines included
	RPS      float64 `json:"rps"`
	Errors   int     `json:"errors_5xx_10s"`
	Dropped  int64   `json:"dropped_total"` // lines lost before reaching cpd since it started
}

// TrafficPage answers GET /v1/traffic.
type TrafficPage struct {
	Lines    []TrafficEntry            `json:"lines"` // oldest first
	LastID   int64                     `json:"last_id"`
	Gateways map[string]GatewayTraffic `json:"gateways"`
}

const statWindow = 10 // seconds

type bucket struct {
	sec     int64
	n, errs int
}

// TrafficRing keeps the last lines in memory (best effort, lost on restart) and per-second
// counters per gateway, so the request rate stays right even when lines are dropped.
type TrafficRing struct {
	max int
	now func() time.Time

	mu      sync.Mutex
	lines   []TrafficEntry
	nextID  int64
	buckets map[string]*[statWindow]bucket
	dropped map[string]int64
}

// NewTrafficRing keeps up to size lines (default 2000).
func NewTrafficRing(size int) *TrafficRing {
	if size <= 0 {
		size = 2000
	}
	return &TrafficRing{max: size, now: time.Now, buckets: map[string]*[statWindow]bucket{}, dropped: map[string]int64{}}
}

// Add stores one batch of gateway gw.
func (r *TrafficRing) Add(gw string, b model.TrafficBatch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	sec := now.Unix()
	bs := r.buckets[gw]
	if bs == nil {
		bs = &[statWindow]bucket{}
		r.buckets[gw] = bs
	}
	cur := &bs[sec%statWindow]
	if cur.sec != sec {
		*cur = bucket{sec: sec}
	}
	cur.n += len(b.Lines) + int(b.Dropped)
	r.dropped[gw] += b.Dropped
	for _, l := range b.Lines {
		l = logs.Clip(l)
		l.GW = gw
		if l.Status >= 500 {
			cur.errs++
		}
		r.nextID++
		r.lines = append(r.lines, TrafficEntry{ID: r.nextID, At: now, TrafficLine: l})
	}
	if over := len(r.lines) - r.max; over > 0 {
		r.lines = append(r.lines[:0], r.lines[over:]...)
	}
}

// Page returns up to limit lines with an id above after (the newest ones when there are
// more), only for host when it is not empty, and the per-gateway stats.
func (r *TrafficRing) Page(after int64, host string, limit int) TrafficPage {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := TrafficPage{Lines: []TrafficEntry{}, LastID: r.nextID, Gateways: map[string]GatewayTraffic{}}
	for i := len(r.lines) - 1; i >= 0 && len(p.Lines) < limit; i-- {
		e := r.lines[i]
		if e.ID <= after {
			break
		}
		if host == "" || e.Host == host {
			p.Lines = append(p.Lines, e)
		}
	}
	for i, j := 0, len(p.Lines)-1; i < j; i, j = i+1, j-1 {
		p.Lines[i], p.Lines[j] = p.Lines[j], p.Lines[i]
	}
	sec := r.now().Unix()
	for gw, bs := range r.buckets {
		var g GatewayTraffic
		for _, b := range bs {
			if b.sec > sec-statWindow && b.sec <= sec {
				g.Requests += b.n
				g.Errors += b.errs
			}
		}
		g.RPS = float64(g.Requests) / statWindow
		g.Dropped = r.dropped[gw]
		p.Gateways[gw] = g
	}
	return p
}

var gwNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// maxTrafficBody: a full batch with every field at its clipped size and JSON escapes.
const maxTrafficBody = 512 << 10

func (s *Server) pushTraffic(w http.ResponseWriter, r *http.Request) {
	gw := r.PathValue("gw")
	if !gwNameRE.MatchString(gw) {
		writeError(w, http.StatusBadRequest, "bad gateway name")
		return
	}
	var b model.TrafficBatch
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTrafficBody))
	if err := dec.Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "bad json body: "+err.Error())
		return
	}
	if len(b.Lines) > model.MaxTrafficBatch || b.Dropped < 0 {
		writeError(w, http.StatusBadRequest, "at most "+strconv.Itoa(model.MaxTrafficBatch)+" lines, dropped >= 0")
		return
	}
	s.Traffic.Add(gw, b)
	writeJSON(w, http.StatusOK, map[string]int{"accepted": len(b.Lines)})
}

// traffic: ?after=<id>&host=<fqdn>&limit=<n> (default 200, at most 500).
func (s *Server) trafficPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var after int64
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "after must be a line id")
			return
		}
		after = n
	}
	limit := 200
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeError(w, http.StatusBadRequest, "limit must be 1 to 500")
			return
		}
		limit = n
	}
	writeJSON(w, http.StatusOK, s.Traffic.Page(after, q.Get("host"), limit))
}
