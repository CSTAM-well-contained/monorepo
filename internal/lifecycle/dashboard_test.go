package lifecycle

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAlerts(t *testing.T) {
	gw := func(name, vrrp string, gen int64, alive bool) Gateway {
		return Gateway{Name: name, VRRPState: vrrp, AckedGen: gen, Alive: alive, LastSeen: time.Unix(0, 0)}
	}
	cfg := func(g Gateway, serving, tried, outcome string) Gateway {
		g.ConfigVersion, g.ConfigTried, g.ConfigOutcome = serving, tried, outcome
		return g
	}
	cases := []struct {
		name   string
		config string
		gws    []Gateway
		want   []string // substrings, in order
	}{
		{"healthy pair", "", []Gateway{gw("gw1", "MASTER", 5, true), gw("gw2", "BACKUP", 5, true)}, nil},
		{"no gateway registered", "", nil, nil},
		{"split brain", "", []Gateway{gw("gw1", "MASTER", 5, true), gw("gw2", "MASTER", 5, true)}, []string{"split brain"}},
		{"no master", "", []Gateway{gw("gw1", "BACKUP", 5, true), gw("gw2", "FAULT", 5, true)},
			[]string{"gw2 is in FAULT", "no gateway holds the VIP"}},
		{"backup behind", "", []Gateway{gw("gw1", "MASTER", 5, true), gw("gw2", "BACKUP", 4, true)},
			[]string{"gw2 is behind: generation 4 of 5"}},
		{"silent master is not counted", "", []Gateway{gw("gw1", "MASTER", 5, false), gw("gw2", "BACKUP", 5, true)},
			[]string{"gw1: no report since", "no gateway holds the VIP"}},
		{"all silent: no extra alert", "", []Gateway{gw("gw1", "MASTER", 5, false)}, []string{"gw1: no report"}},
		{"config rolled back", "c3", []Gateway{
			cfg(gw("gw1", "MASTER", 5, true), "c2", "c3", "rolled_back"),
			cfg(gw("gw2", "BACKUP", 5, true), "c3", "c3", "applied")},
			[]string{"gw1 did not take config c3 (rolled_back), still serves c2"}},
		{"old failure is not news", "c4", []Gateway{
			cfg(gw("gw1", "MASTER", 5, true), "c2", "c3", "rejected"), gw("gw2", "BACKUP", 5, true)}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := alerts(5, tc.config, tc.gws)
			if len(got) != len(tc.want) {
				t.Fatalf("alerts %q, want %d matching %q", got, len(tc.want), tc.want)
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i], w) {
					t.Fatalf("alert %d = %q, want it to contain %q", i, got[i], w)
				}
			}
			if got == nil || slices.Contains(got, "") {
				t.Fatalf("alerts must be a non-nil list of messages: %#v", got)
			}
		})
	}
}

func TestConfigInValidate(t *testing.T) {
	long := strings.Repeat("x", MaxExtraLineLen+1)
	cases := []struct {
		name string
		in   ConfigIn
		ok   bool
	}{
		{"rate limit only", ConfigIn{RateLimit: 2000}, true},
		{"extra lines", ConfigIn{RateLimit: 100, Extra: []string{"http-request deny if { path /x }"}, Reason: "block /x"}, true},
		{"zero rate", ConfigIn{RateLimit: 0}, false},
		{"huge rate", ConfigIn{RateLimit: 2000000}, false},
		{"newline injection", ConfigIn{RateLimit: 1, Extra: []string{"ok\nbackend evil"}}, false},
		{"empty line", ConfigIn{RateLimit: 1, Extra: []string{""}}, false},
		{"long line", ConfigIn{RateLimit: 1, Extra: []string{long}}, false},
		{"non ascii", ConfigIn{RateLimit: 1, Extra: []string{"deny é"}}, false},
		{"too many lines", ConfigIn{RateLimit: 1, Extra: slices.Repeat([]string{"x"}, MaxExtraLines+1)}, false},
		{"control char in reason", ConfigIn{RateLimit: 1, Reason: "a\x1bb"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.in.Validate(); (err == nil) != tc.ok {
				t.Fatalf("Validate() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestCleanAckError(t *testing.T) {
	if got := cleanAckError("line1\nline2\x00"); got != "line1 line2 " {
		t.Fatalf("got %q", got)
	}
	if got := cleanAckError(strings.Repeat("e", 500)); len(got) != 300 {
		t.Fatalf("len %d", len(got))
	}
}
