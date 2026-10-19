// Package fake is an in-memory lifecycle.Sandboxes for tests: idempotent like the real
// driver, with fixed-IP uniqueness and fault injection.
package fake

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"

	"github.com/CSTAM-well-contained/monorepo/internal/lifecycle"
)

// Sandboxes keeps the sandboxes in a map. The zero value is not usable: call New.
type Sandboxes struct {
	mu      sync.Mutex
	byName  map[string]entry
	foreign map[netip.Addr]bool // IPs held by resources that are not ours
	next    int

	// FailCreate, when set, is called before each Create; a non-nil error is returned
	// as is. With CreatedAnyway the sandbox is created first ("created, then timed out").
	FailCreate    func(s lifecycle.Spec) error
	CreatedAnyway bool
	// FailDelete, when set, is called before each Delete.
	FailDelete func(ref lifecycle.Ref) error

	Creates, Deletes int // calls, for assertions
}

type entry struct {
	ref  lifecycle.Ref
	spec lifecycle.Spec
}

var _ lifecycle.Sandboxes = (*Sandboxes)(nil)

// New returns an empty fake cloud.
func New() *Sandboxes {
	return &Sandboxes{byName: map[string]entry{}, foreign: map[netip.Addr]bool{}}
}

// HoldForeign marks ip as used by someone else (Create fails with ErrIPTaken, InUse is true).
func (f *Sandboxes) HoldForeign(ip netip.Addr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.foreign[ip] = true
}

// Create makes the sandbox, or returns the existing one with the same name.
func (f *Sandboxes) Create(_ context.Context, s lifecycle.Spec) (lifecycle.Ref, error) {
	if err := s.Validate(); err != nil {
		return lifecycle.Ref{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Creates++
	ref := lifecycle.Ref{Name: s.Name()}
	if f.FailCreate != nil {
		if err := f.FailCreate(s); err != nil {
			if f.CreatedAnyway {
				if _, err := f.createLocked(s); err != nil {
					return ref, err
				}
			}
			return ref, err
		}
	}
	return f.createLocked(s)
}

func (f *Sandboxes) createLocked(s lifecycle.Spec) (lifecycle.Ref, error) {
	if e, ok := f.byName[s.Name()]; ok {
		if e.spec.IP != s.IP {
			return e.ref, fmt.Errorf("port %s exists with %s: %w", s.Name(), e.spec.IP, lifecycle.ErrIPTaken)
		}
		return e.ref, nil
	}
	if f.foreign[s.IP] || f.ipUsedLocked(s.IP) {
		return lifecycle.Ref{Name: s.Name()}, fmt.Errorf("%s: %w", s.IP, lifecycle.ErrIPTaken)
	}
	f.next++
	ref := lifecycle.Ref{Name: s.Name(), Server: fmt.Sprintf("server-%d", f.next), Port: fmt.Sprintf("port-%d", f.next)}
	f.byName[s.Name()] = entry{ref: ref, spec: s}
	return ref, nil
}

func (f *Sandboxes) ipUsedLocked(ip netip.Addr) bool {
	for _, e := range f.byName {
		if e.spec.IP == ip {
			return true
		}
	}
	return false
}

// Delete removes the sandbox named ref.Name; a missing one is not an error.
func (f *Sandboxes) Delete(_ context.Context, ref lifecycle.Ref) error {
	if !strings.HasPrefix(ref.Name, lifecycle.NamePrefix) {
		return fmt.Errorf("fake: refusing to delete %q: not ours", ref.Name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Deletes++
	if f.FailDelete != nil {
		if err := f.FailDelete(ref); err != nil {
			return err
		}
	}
	delete(f.byName, ref.Name)
	return nil
}

// InUse reports whether a sandbox or a foreign resource holds ip.
func (f *Sandboxes) InUse(_ context.Context, ip netip.Addr) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.foreign[ip] || f.ipUsedLocked(ip), nil
}

// List returns the sandboxes whose name starts with prefix, sorted by name.
func (f *Sandboxes) List(_ context.Context, prefix string) ([]lifecycle.Ref, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []lifecycle.Ref
	for name, e := range f.byName {
		if strings.HasPrefix(name, prefix) {
			out = append(out, e.ref)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
