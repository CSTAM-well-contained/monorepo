package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CSTAM-well-contained/monorepo/internal/ipam"
	"github.com/CSTAM-well-contained/monorepo/internal/lifecycle"
	"github.com/CSTAM-well-contained/monorepo/internal/model"
)

const (
	adminTok = "admin-token-0123456789"
	gwTok    = "gateway-token-0123456789"
)

// stub records the last call and returns err.
type stub struct {
	err       error
	last      string
	after     int64
	wait      time.Duration
	ack       model.Ack
	createdIn [3]any
}

func (s *stub) CreateHackathon(_ context.Context, name, domain string, ttl time.Duration) (lifecycle.Hackathon, error) {
	s.last, s.createdIn = "CreateHackathon", [3]any{name, domain, ttl}
	return lifecycle.Hackathon{ID: 1, Name: name, Domain: domain}, s.err
}
func (s *stub) Hackathons(context.Context) ([]lifecycle.Hackathon, error) {
	s.last = "Hackathons"
	return []lifecycle.Hackathon{}, s.err
}
func (s *stub) Hackathon(_ context.Context, name string) (lifecycle.Hackathon, error) {
	s.last = "Hackathon " + name
	return lifecycle.Hackathon{Name: name}, s.err
}
func (s *stub) EndHackathon(_ context.Context, name string) error {
	s.last = "EndHackathon " + name
	return s.err
}
func (s *stub) CreateTeam(_ context.Context, h, name string, port int, image string) (lifecycle.Team, error) {
	s.last, s.createdIn = "CreateTeam "+h, [3]any{name, port, image}
	return lifecycle.Team{ID: 7, Name: name, State: "provisioning"}, s.err
}
func (s *stub) Teams(_ context.Context, h string) ([]lifecycle.Team, error) {
	s.last = "Teams " + h
	return nil, s.err
}
func (s *stub) Team(_ context.Context, id int64) (lifecycle.Team, error) {
	s.last = fmt.Sprint("Team ", id)
	return lifecycle.Team{ID: id}, s.err
}
func (s *stub) DeleteTeam(_ context.Context, id int64) error {
	s.last = fmt.Sprint("DeleteTeam ", id)
	return s.err
}
func (s *stub) Renew(_ context.Context, id int64) (time.Time, error) {
	s.last = fmt.Sprint("Renew ", id)
	return time.Time{}, s.err
}
func (s *stub) Status(context.Context) (lifecycle.Status, error) {
	s.last = "Status"
	return lifecycle.Status{}, s.err
}
func (s *stub) Desired(_ context.Context, after int64, wait time.Duration) (model.Desired, error) {
	s.last, s.after, s.wait = "Desired", after, wait
	return model.Desired{Generation: 3, Routes: []model.Route{}}, s.err
}
func (s *stub) Ack(_ context.Context, gw string, a model.Ack) error {
	s.last, s.ack = "Ack "+gw, a
	return s.err
}
func (s *stub) AllTeams(context.Context) ([]lifecycle.Team, error) {
	s.last = "AllTeams"
	return []lifecycle.Team{}, s.err
}
func (s *stub) Events(_ context.Context, after int64, limit int) ([]lifecycle.Event, error) {
	s.last = fmt.Sprint("Events ", after, " ", limit)
	return []lifecycle.Event{}, s.err
}
func (s *stub) Addresses(context.Context) ([]lifecycle.Address, error) {
	s.last = "Addresses"
	return []lifecycle.Address{}, s.err
}
func (s *stub) Routes(context.Context) (model.Desired, error) {
	s.last = "Routes"
	return model.Desired{Routes: []model.Route{}}, s.err
}

func do(t *testing.T, st *stub, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	srv := &Server{C: st, AdminToken: adminTok, GatewayToken: gwTok, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := srv.Validate(); err != nil {
		t.Fatal(err)
	}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestRoutes(t *testing.T) {
	cases := []struct {
		name, method, path, token, body string
		code                            int
		call                            string
	}{
		{"healthz needs no token", "GET", "/healthz", "", "", 200, ""},
		{"no token", "GET", "/v1/status", "", "", 401, ""},
		{"wrong token", "GET", "/v1/status", "nope", "", 401, ""},
		{"gateway token on admin route", "GET", "/v1/status", gwTok, "", 401, ""},
		{"admin token on gateway route", "GET", "/v1/gateways/gw1/desired", adminTok, "", 401, ""},
		{"status", "GET", "/v1/status", adminTok, "", 200, "Status"},
		{"all teams", "GET", "/v1/teams", adminTok, "", 200, "AllTeams"},
		{"all teams needs admin", "GET", "/v1/teams", gwTok, "", 401, ""},
		{"last events", "GET", "/v1/events", adminTok, "", 200, "Events -1 100"},
		{"events after", "GET", "/v1/events?after=7&limit=20", adminTok, "", 200, "Events 7 20"},
		{"events bad after", "GET", "/v1/events?after=-3", adminTok, "", 400, ""},
		{"events bad limit", "GET", "/v1/events?limit=9999", adminTok, "", 400, ""},
		{"ipam", "GET", "/v1/ipam", adminTok, "", 200, "Addresses"},
		{"routes", "GET", "/v1/routes", adminTok, "", 200, "Routes"},
		{"create hackathon", "POST", "/v1/hackathons", adminTok, `{"name":"h1","domain":"h1.example.org"}`, 201, "CreateHackathon"},
		{"unknown field", "POST", "/v1/hackathons", adminTok, `{"name":"h1","bogus":1}`, 400, ""},
		{"negative ttl", "POST", "/v1/hackathons", adminTok, `{"name":"h1","lease_ttl_seconds":-1}`, 400, ""},
		{"list hackathons", "GET", "/v1/hackathons", adminTok, "", 200, "Hackathons"},
		{"end hackathon", "DELETE", "/v1/hackathons/h1", adminTok, "", 202, "Hackathon h1"},
		{"create team", "POST", "/v1/hackathons/h1/teams", adminTok, `{"name":"t1","app_port":8080}`, 202, "CreateTeam h1"},
		{"list teams", "GET", "/v1/hackathons/h1/teams", adminTok, "", 200, "Teams h1"},
		{"get team", "GET", "/v1/teams/7", adminTok, "", 200, "Team 7"},
		{"bad team id", "GET", "/v1/teams/abc", adminTok, "", 400, ""},
		{"renew", "POST", "/v1/teams/7/renew", adminTok, "", 200, "Renew 7"},
		{"delete team", "DELETE", "/v1/teams/7", adminTok, "", 202, "Team 7"},
		{"desired", "GET", "/v1/gateways/gw1/desired?after=2&wait=1s", gwTok, "", 200, "Desired"},
		{"desired bad after", "GET", "/v1/gateways/gw1/desired?after=x", gwTok, "", 400, ""},
		{"ack", "POST", "/v1/gateways/gw1/ack", gwTok, `{"generation":3,"config_version":"v1","vrrp_state":"MASTER"}`, 200, "Ack gw1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &stub{}
			w := do(t, st, tc.method, tc.path, tc.token, tc.body)
			if w.Code != tc.code {
				t.Fatalf("code %d, want %d: %s", w.Code, tc.code, w.Body)
			}
			if st.last != tc.call {
				t.Fatalf("called %q, want %q", st.last, tc.call)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("content type %q", ct)
			}
		})
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		code int
	}{
		{fmt.Errorf("x: %w", lifecycle.ErrInvalidSpec), 400},
		{fmt.Errorf("x: %w", lifecycle.ErrNotFound), 404},
		{fmt.Errorf("x: %w", lifecycle.ErrConflict), 409},
		{fmt.Errorf("x: %w", lifecycle.ErrNotActive), 409},
		{ipam.ErrLeaseExpired, 409},
		{ipam.ErrPoolExhausted, 503},
		{errors.New("db password=secret broke"), 500},
	}
	for _, tc := range cases {
		t.Run(tc.err.Error(), func(t *testing.T) {
			w := do(t, &stub{err: tc.err}, "POST", "/v1/hackathons/h1/teams", adminTok, `{"name":"t1"}`)
			if w.Code != tc.code {
				t.Fatalf("code %d, want %d", w.Code, tc.code)
			}
			if tc.code == 500 && strings.Contains(w.Body.String(), "secret") {
				t.Fatal("internal error leaked to the client")
			}
			if tc.code == 503 && w.Header().Get("Retry-After") == "" {
				t.Fatal("no Retry-After")
			}
		})
	}
}

func TestDesiredParams(t *testing.T) {
	st := &stub{}
	do(t, st, "GET", "/v1/gateways/gw1/desired", gwTok, "")
	if st.after != -1 || st.wait != MaxWait {
		t.Fatalf("defaults: after %d wait %s", st.after, st.wait)
	}
	do(t, st, "GET", "/v1/gateways/gw1/desired?after=5&wait=10m", gwTok, "")
	if st.after != 5 || st.wait != MaxWait {
		t.Fatalf("capped: after %d wait %s", st.after, st.wait)
	}
}

func TestValidateTokens(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, s := range []Server{
		{C: &stub{}, AdminToken: "short", GatewayToken: gwTok, Log: log},
		{C: &stub{}, AdminToken: adminTok, GatewayToken: adminTok, Log: log},
	} {
		if err := s.Validate(); err == nil {
			t.Fatalf("accepted %+v", s)
		}
	}
}
