// Package lifecycle drives teams from creation to teardown. This file defines the sandbox
// drivers it uses: one sandbox per team, reachable at the team's IPAM address.
package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/CSTAM-well-contained/monorepo/internal/config"
)

// Errors shared by every sandbox driver.
var (
	// ErrIPTaken: another resource already holds the team's fixed IP. Quarantine the IP.
	ErrIPTaken = errors.New("sandbox: fixed IP already in use")
	// ErrQuota: the cloud refused for lack of quota. Retrying will not help soon.
	ErrQuota = errors.New("sandbox: cloud quota exceeded")
	// ErrInvalidSpec: the spec failed validation; nothing was sent to the cloud.
	ErrInvalidSpec = errors.New("sandbox: invalid spec")
)

// NamePrefix starts the name of every sandbox resource the platform creates.
const NamePrefix = "cstam-"

var (
	labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	// imageRE accepts container image references (registry/repo:tag@digest) and nothing
	// that could break out of cloud-init YAML or a shell argument.
	imageRE = regexp.MustCompile(`^[a-z0-9]+([._/:@-][a-zA-Z0-9]+)*$`)
)

// Spec describes the sandbox of one team.
type Spec struct {
	Hackathon string     // dns label
	Team      string     // dns label
	IP        netip.Addr // from the IPAM, inside the team pool
	AppPort   int        // the team app listens here
	Image     string     // optional container image; empty serves a default page
}

// Name is the deterministic name of the team's resources: cstam-<hackathon>-<team>.
func (s Spec) Name() string { return NamePrefix + s.Hackathon + "-" + s.Team }

// Validate rejects a spec that must not reach the cloud.
func (s Spec) Validate() error {
	var errs []error
	if !labelRE.MatchString(s.Hackathon) {
		errs = append(errs, fmt.Errorf("hackathon %q is not a dns label", s.Hackathon))
	}
	if !labelRE.MatchString(s.Team) {
		errs = append(errs, fmt.Errorf("team %q is not a dns label", s.Team))
	}
	if !config.InTeamPool(s.IP) && !inLabPool(s.IP) {
		errs = append(errs, fmt.Errorf("ip %s is outside the team and lab pools", s.IP))
	}
	if !config.TeamPortAllowed(s.AppPort) {
		errs = append(errs, fmt.Errorf("app port %d: the gateways only reach team ports %v", s.AppPort, config.TeamPorts))
	}
	if s.Image != "" && (len(s.Image) > 255 || !imageRE.MatchString(s.Image)) {
		errs = append(errs, fmt.Errorf("image %q is not a valid image reference", s.Image))
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrInvalidSpec, errors.Join(errs...))
	}
	return nil
}

// The lab pool is used by the e2e test, never by the IPAM.
func inLabPool(ip netip.Addr) bool {
	return ip.Compare(config.LabPoolLo) >= 0 && ip.Compare(config.LabPoolHi) <= 0
}

// Ref identifies a created sandbox. It is stored in teams.sandbox_ref.
// Name alone is enough to find everything again (after a crash before the IDs were saved).
type Ref struct {
	Name   string `json:"name"`
	Server string `json:"server,omitempty"` // Nova server ID, or container ID
	Port   string `json:"port,omitempty"`   // Neutron port ID
}

// String encodes the ref for the database.
func (r Ref) String() string {
	b, _ := json.Marshal(r) // three strings: cannot fail
	return string(b)
}

// ParseRef decodes a ref stored by String.
func ParseRef(s string) (Ref, error) {
	var r Ref
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return Ref{}, fmt.Errorf("sandbox ref %q: %w", s, err)
	}
	if !strings.HasPrefix(r.Name, NamePrefix) {
		return Ref{}, fmt.Errorf("sandbox ref %q: name must start with %s", s, NamePrefix)
	}
	return r, nil
}

// Sandboxes creates and deletes team sandboxes. Implementations are idempotent: Create of
// an existing sandbox returns it, Delete of a missing one succeeds.
type Sandboxes interface {
	// Create makes the sandbox and waits until it runs. On error the returned ref names
	// what may have been created, so the caller can Delete it.
	Create(ctx context.Context, s Spec) (Ref, error)
	// Delete removes everything named ref.Name (IDs are a shortcut, the name is the truth).
	Delete(ctx context.Context, ref Ref) error
	// InUse reports whether any resource still holds ip (the IPAM sweep asks before freeing).
	InUse(ctx context.Context, ip netip.Addr) (bool, error)
	// List returns the sandboxes whose name starts with prefix (garbage collection).
	List(ctx context.Context, prefix string) ([]Ref, error)
}
