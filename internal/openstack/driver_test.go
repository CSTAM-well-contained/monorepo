package openstack

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/CSTAM-well-contained/monorepo/internal/lifecycle"
)

func TestUserData(t *testing.T) {
	base := lifecycle.Spec{Hackathon: "h1", Team: "team1", IP: netip.MustParseAddr("10.20.0.40"), AppPort: 8080}
	tests := []struct {
		name    string
		image   string
		want    []string
		notWant []string
	}{
		{"default page", "",
			[]string{"#cloud-config\n", "python3 -m http.server 8080", "<h1>team team1</h1>", "sandbox 10.20.0.40"},
			[]string{"docker"}},
		{"container image", "ghcr.io/team1/app:v1",
			[]string{"#cloud-config\n", "packages: [docker.io]", `"8080:8080", "ghcr.io/team1/app:v1"]`},
			[]string{"http.server"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			s.Image = tc.image
			b, err := UserData(s)
			if err != nil {
				t.Fatal(err)
			}
			out := string(b)
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("user-data lacks %q:\n%s", w, out)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(out, w) {
					t.Errorf("user-data contains %q:\n%s", w, out)
				}
			}
			if strings.Contains(out, "\t") {
				t.Error("user-data contains a tab (invalid YAML indentation)")
			}
		})
	}
	bad := base
	bad.Image = "x\n  - [reboot]"
	if _, err := UserData(bad); !errors.Is(err, lifecycle.ErrInvalidSpec) {
		t.Errorf("UserData with an injected image: %v, want ErrInvalidSpec", err)
	}
}

func httpErr(code int, body string) error {
	return fmt.Errorf("wrapped: %w", gophercloud.ErrUnexpectedResponseCode{Actual: code, Body: []byte(body)})
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"ip taken", httpErr(409, `{"NeutronError": {"type": "IpAddressAlreadyAllocated"}}`), lifecycle.ErrIPTaken},
		{"other conflict", httpErr(409, `{"NeutronError": {"type": "MacAddressInUse"}}`), nil},
		{"neutron quota", httpErr(409, `{"NeutronError": {"type": "OverQuota"}}`), lifecycle.ErrQuota},
		{"nova quota", httpErr(403, `{"forbidden": {"message": "Quota exceeded for instances"}}`), lifecycle.ErrQuota},
		{"not found", httpErr(404, ``), nil},
		{"plain error", errors.New("boom"), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.err)
			if !errors.Is(got, tc.err) {
				t.Errorf("classify lost the original error: %v", got)
			}
			for _, sentinel := range []error{lifecycle.ErrIPTaken, lifecycle.ErrQuota} {
				if errors.Is(got, sentinel) != errors.Is(tc.want, sentinel) {
					t.Errorf("classify(%v) = %v, want %v", tc.err, got, tc.want)
				}
			}
		})
	}
}

func TestRetryable(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{httpErr(429, ""), true},
		{httpErr(503, ""), true},
		{httpErr(500, ""), true},
		{httpErr(404, ""), false},
		{httpErr(409, ""), false},
		{errors.New("boom"), false},
	}
	for _, tc := range tests {
		if got := retryable(tc.err); got != tc.want {
			t.Errorf("retryable(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
