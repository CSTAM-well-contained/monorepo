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
	cases := []struct {
		name string
		gws  []Gateway
		want []string // substrings, in order
	}{
		{"healthy pair", []Gateway{gw("gw1", "MASTER", 5, true), gw("gw2", "BACKUP", 5, true)}, nil},
		{"no gateway registered", nil, nil},
		{"split brain", []Gateway{gw("gw1", "MASTER", 5, true), gw("gw2", "MASTER", 5, true)}, []string{"split brain"}},
		{"no master", []Gateway{gw("gw1", "BACKUP", 5, true), gw("gw2", "FAULT", 5, true)},
			[]string{"gw2 is in FAULT", "no gateway holds the VIP"}},
		{"backup behind", []Gateway{gw("gw1", "MASTER", 5, true), gw("gw2", "BACKUP", 4, true)},
			[]string{"gw2 is behind: generation 4 of 5"}},
		{"silent master is not counted", []Gateway{gw("gw1", "MASTER", 5, false), gw("gw2", "BACKUP", 5, true)},
			[]string{"gw1: no report since", "no gateway holds the VIP"}},
		{"all silent: no extra alert", []Gateway{gw("gw1", "MASTER", 5, false)}, []string{"gw1: no report"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := alerts(5, tc.gws)
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
