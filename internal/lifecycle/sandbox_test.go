package lifecycle

import (
	"errors"
	"net/netip"
	"testing"
)

func TestSpecValidate(t *testing.T) {
	ok := Spec{Hackathon: "h1", Team: "team-1", IP: netip.MustParseAddr("10.20.0.40"), AppPort: 80}
	tests := []struct {
		name  string
		edit  func(s *Spec)
		valid bool
	}{
		{"valid", func(*Spec) {}, true},
		{"valid with image", func(s *Spec) { s.Image = "ghcr.io/team-1/app:v1.2" }, true},
		{"valid lab ip", func(s *Spec) { s.IP = netip.MustParseAddr("10.20.0.199") }, true},
		{"upper case team", func(s *Spec) { s.Team = "Team1" }, false},
		{"team too long", func(s *Spec) { s.Team = "a234567890123456789012345678901234" }, false},
		{"hackathon with dot", func(s *Spec) { s.Hackathon = "h.1" }, false},
		{"gateway ip", func(s *Spec) { s.IP = netip.MustParseAddr("10.20.0.11") }, false},
		{"neutron pool ip", func(s *Spec) { s.IP = netip.MustParseAddr("10.20.0.200") }, false},
		{"zero ip", func(s *Spec) { s.IP = netip.Addr{} }, false},
		{"port 0", func(s *Spec) { s.AppPort = 0 }, false},
		{"port too big", func(s *Spec) { s.AppPort = 70000 }, false},
		{"port 8080", func(s *Spec) { s.AppPort = 8080 }, true},
		{"port 8099", func(s *Spec) { s.AppPort = 8099 }, true},
		{"port 3000 blocked by the security group", func(s *Spec) { s.AppPort = 3000 }, false},
		{"port 8100 blocked by the security group", func(s *Spec) { s.AppPort = 8100 }, false},
		{"image with shell", func(s *Spec) { s.Image = "nginx; rm -rf /" }, false},
		{"image with newline", func(s *Spec) { s.Image = "nginx\n  - [reboot]" }, false},
		{"image with quote", func(s *Spec) { s.Image = `nginx"` }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := ok
			tc.edit(&s)
			err := s.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("Validate() = %v, want valid %v", err, tc.valid)
			}
			if err != nil && !errors.Is(err, ErrInvalidSpec) {
				t.Errorf("error %v does not wrap ErrInvalidSpec", err)
			}
		})
	}
}

func TestRef(t *testing.T) {
	s := Spec{Hackathon: "h1", Team: "team1"}
	if got := s.Name(); got != "cstam-h1-team1" {
		t.Fatalf("Name() = %q", got)
	}
	in := Ref{Name: s.Name(), Server: "srv-id", Port: "port-id"}
	out, err := ParseRef(in.String())
	if err != nil || out != in {
		t.Fatalf("round trip: %+v, %v; want %+v", out, err, in)
	}
	for _, bad := range []string{"", "{", `{"name":"someone-else"}`} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) accepted", bad)
		}
	}
}
