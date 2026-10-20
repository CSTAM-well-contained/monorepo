// Package api is the REST API of cpd: admin endpoints for hackathons and teams, and the
// gateway endpoints (desired state long-poll and acks). Plain net/http, JSON bodies.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/CSTAM-well-contained/monorepo/internal/ipam"
	"github.com/CSTAM-well-contained/monorepo/internal/lifecycle"
	"github.com/CSTAM-well-contained/monorepo/internal/model"
)

// Controller is what the API needs from lifecycle.Controller.
type Controller interface {
	CreateHackathon(ctx context.Context, name, domain string, ttl time.Duration) (lifecycle.Hackathon, error)
	Hackathons(ctx context.Context) ([]lifecycle.Hackathon, error)
	Hackathon(ctx context.Context, name string) (lifecycle.Hackathon, error)
	EndHackathon(ctx context.Context, name string) error
	CreateTeam(ctx context.Context, hackathon, name string, appPort int, image string) (lifecycle.Team, error)
	Teams(ctx context.Context, hackathon string) ([]lifecycle.Team, error)
	Team(ctx context.Context, id int64) (lifecycle.Team, error)
	DeleteTeam(ctx context.Context, id int64) error
	Renew(ctx context.Context, id int64) (time.Time, error)
	Status(ctx context.Context) (lifecycle.Status, error)
	Desired(ctx context.Context, after int64, wait time.Duration) (model.Desired, error)
	Ack(ctx context.Context, gateway string, a model.Ack) error
	AllTeams(ctx context.Context) ([]lifecycle.Team, error)
	Events(ctx context.Context, after int64, limit int) ([]lifecycle.Event, error)
	Addresses(ctx context.Context) ([]lifecycle.Address, error)
	Routes(ctx context.Context) (model.Desired, error)
}

// MaxWait caps the desired-state long-poll (below common proxy idle timeouts).
const MaxWait = 25 * time.Second

const maxBody = 64 << 10

// Server holds the handlers. Both tokens are required and must differ.
type Server struct {
	C            Controller
	AdminToken   string
	GatewayToken string
	Log          *slog.Logger
}

// token says which bearer token a route needs.
type token int

const (
	public token = iota
	adminOnly
	gatewayOnly
)

type route struct {
	pattern string // "METHOD /path"
	token   token
	h       http.HandlerFunc
}

// routes is the whole API. docs/official/openapi.yaml must list every one (tested).
func (s *Server) routes() []route {
	return []route{
		{"GET /healthz", public, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		}},
		{"POST /v1/hackathons", adminOnly, s.createHackathon},
		{"GET /v1/hackathons", adminOnly, s.listHackathons},
		{"GET /v1/hackathons/{h}", adminOnly, s.getHackathon},
		{"DELETE /v1/hackathons/{h}", adminOnly, s.endHackathon},
		{"POST /v1/hackathons/{h}/teams", adminOnly, s.createTeam},
		{"GET /v1/hackathons/{h}/teams", adminOnly, s.listTeams},
		{"GET /v1/teams/{id}", adminOnly, s.getTeam},
		{"POST /v1/teams/{id}/renew", adminOnly, s.renewTeam},
		{"DELETE /v1/teams/{id}", adminOnly, s.deleteTeam},
		{"GET /v1/status", adminOnly, s.status},
		{"GET /v1/teams", adminOnly, s.allTeams},
		{"GET /v1/events", adminOnly, s.events},
		{"GET /v1/ipam", adminOnly, s.addresses},
		{"GET /v1/routes", adminOnly, s.routeTable},
		{"GET /v1/gateways/{gw}/desired", gatewayOnly, s.desired},
		{"POST /v1/gateways/{gw}/ack", gatewayOnly, s.ack},
	}
}

// Handler returns the routes of the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, r := range s.routes() {
		switch r.token {
		case adminOnly:
			mux.Handle(r.pattern, s.auth(s.AdminToken, r.h))
		case gatewayOnly:
			mux.Handle(r.pattern, s.auth(s.GatewayToken, r.h))
		default:
			mux.Handle(r.pattern, r.h)
		}
	}
	return mux
}

func (s *Server) auth(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if token == "" || subtle.ConstantTimeCompare(got, want) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or wrong bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type hackathonIn struct {
	Name            string `json:"name"`
	Domain          string `json:"domain"`
	LeaseTTLSeconds int64  `json:"lease_ttl_seconds"`
}

func (s *Server) createHackathon(w http.ResponseWriter, r *http.Request) {
	var in hackathonIn
	if !s.decode(w, r, &in) {
		return
	}
	if in.LeaseTTLSeconds < 0 || in.LeaseTTLSeconds > 30*24*3600 {
		writeError(w, http.StatusBadRequest, "lease_ttl_seconds out of range")
		return
	}
	h, err := s.C.CreateHackathon(r.Context(), in.Name, in.Domain, time.Duration(in.LeaseTTLSeconds)*time.Second)
	s.reply(w, r, http.StatusCreated, h, err)
}

func (s *Server) listHackathons(w http.ResponseWriter, r *http.Request) {
	hs, err := s.C.Hackathons(r.Context())
	s.reply(w, r, http.StatusOK, hs, err)
}

func (s *Server) getHackathon(w http.ResponseWriter, r *http.Request) {
	h, err := s.C.Hackathon(r.Context(), r.PathValue("h"))
	s.reply(w, r, http.StatusOK, h, err)
}

func (s *Server) endHackathon(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("h")
	if err := s.C.EndHackathon(r.Context(), name); err != nil {
		s.reply(w, r, 0, nil, err)
		return
	}
	h, err := s.C.Hackathon(r.Context(), name)
	s.reply(w, r, http.StatusAccepted, h, err)
}

type teamIn struct {
	Name    string `json:"name"`
	AppPort int    `json:"app_port"`
	Image   string `json:"image"`
}

func (s *Server) createTeam(w http.ResponseWriter, r *http.Request) {
	var in teamIn
	if !s.decode(w, r, &in) {
		return
	}
	t, err := s.C.CreateTeam(r.Context(), r.PathValue("h"), in.Name, in.AppPort, in.Image)
	if err == nil {
		w.Header().Set("Location", "/v1/teams/"+strconv.FormatInt(t.ID, 10))
	}
	s.reply(w, r, http.StatusAccepted, t, err)
}

func (s *Server) listTeams(w http.ResponseWriter, r *http.Request) {
	ts, err := s.C.Teams(r.Context(), r.PathValue("h"))
	s.reply(w, r, http.StatusOK, ts, err)
}

func (s *Server) getTeam(w http.ResponseWriter, r *http.Request) {
	id, ok := teamID(w, r)
	if !ok {
		return
	}
	t, err := s.C.Team(r.Context(), id)
	s.reply(w, r, http.StatusOK, t, err)
}

func (s *Server) renewTeam(w http.ResponseWriter, r *http.Request) {
	id, ok := teamID(w, r)
	if !ok {
		return
	}
	until, err := s.C.Renew(r.Context(), id)
	s.reply(w, r, http.StatusOK, map[string]time.Time{"lease_expires_at": until}, err)
}

func (s *Server) deleteTeam(w http.ResponseWriter, r *http.Request) {
	id, ok := teamID(w, r)
	if !ok {
		return
	}
	if err := s.C.DeleteTeam(r.Context(), id); err != nil {
		s.reply(w, r, 0, nil, err)
		return
	}
	t, err := s.C.Team(r.Context(), id)
	s.reply(w, r, http.StatusAccepted, t, err)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	st, err := s.C.Status(r.Context())
	s.reply(w, r, http.StatusOK, st, err)
}

func (s *Server) allTeams(w http.ResponseWriter, r *http.Request) {
	ts, err := s.C.AllTeams(r.Context())
	s.reply(w, r, http.StatusOK, ts, err)
}

// events: ?after=<id> returns newer events (oldest first); without it, the last ones.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, limit := int64(-1), 100
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "after must be an event id")
			return
		}
		after = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeError(w, http.StatusBadRequest, "limit must be 1 to 500")
			return
		}
		limit = n
	}
	evs, err := s.C.Events(r.Context(), after, limit)
	s.reply(w, r, http.StatusOK, evs, err)
}

func (s *Server) addresses(w http.ResponseWriter, r *http.Request) {
	as, err := s.C.Addresses(r.Context())
	s.reply(w, r, http.StatusOK, as, err)
}

func (s *Server) routeTable(w http.ResponseWriter, r *http.Request) {
	d, err := s.C.Routes(r.Context())
	s.reply(w, r, http.StatusOK, d, err)
}

func (s *Server) desired(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after := int64(-1) // no "after": answer at once
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "after must be a generation number")
			return
		}
		after = n
	}
	wait := MaxWait
	if v := q.Get("wait"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			writeError(w, http.StatusBadRequest, "wait must be a duration like 10s")
			return
		}
		wait = min(d, MaxWait)
	}
	d, err := s.C.Desired(r.Context(), after, wait)
	if r.Context().Err() != nil {
		return // the gateway hung up
	}
	s.reply(w, r, http.StatusOK, d, err)
}

func (s *Server) ack(w http.ResponseWriter, r *http.Request) {
	var in model.Ack
	if !s.decode(w, r, &in) {
		return
	}
	err := s.C.Ack(r.Context(), r.PathValue("gw"), in)
	s.reply(w, r, http.StatusOK, map[string]int64{"acked_generation": in.Generation}, err)
}

func teamID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "team id must be a positive number")
		return 0, false
	}
	return id, true
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "body must be application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad json body: "+err.Error())
		return false
	}
	return true
}

// reply writes v with status, or maps err to an HTTP error.
func (s *Server) reply(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err == nil {
		writeJSON(w, status, v)
		return
	}
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, lifecycle.ErrInvalidSpec):
		code = http.StatusBadRequest
	case errors.Is(err, lifecycle.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, lifecycle.ErrConflict), errors.Is(err, lifecycle.ErrNotActive),
		errors.Is(err, ipam.ErrLeaseExpired):
		code = http.StatusConflict
	case errors.Is(err, ipam.ErrPoolExhausted):
		code = http.StatusServiceUnavailable
		w.Header().Set("Retry-After", "60")
	}
	if code == http.StatusInternalServerError {
		s.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, code, "internal error")
		return
	}
	writeError(w, code, err.Error())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client may be gone; nothing to do
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// Validate checks the tokens before serving.
func (s *Server) Validate() error {
	if len(s.AdminToken) < 16 || len(s.GatewayToken) < 16 {
		return errors.New("api: admin and gateway tokens must be at least 16 characters")
	}
	if s.AdminToken == s.GatewayToken {
		return errors.New("api: admin and gateway tokens must differ")
	}
	if s.C == nil || s.Log == nil {
		return fmt.Errorf("api: controller and logger are required")
	}
	return nil
}
