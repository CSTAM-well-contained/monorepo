package fake

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/CSTAM-well-contained/monorepo/internal/lifecycle"
)

func spec(team, ip string) lifecycle.Spec {
	return lifecycle.Spec{Hackathon: "h1", Team: team, IP: netip.MustParseAddr(ip), AppPort: 80}
}

func TestFake(t *testing.T) {
	ctx := context.Background()
	f := New()
	ref, err := f.Create(ctx, spec("team1", "10.20.0.40"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.Create(ctx, spec("team1", "10.20.0.40"))
	if err != nil || again != ref {
		t.Fatalf("second Create = %+v, %v; want the same %+v", again, err, ref)
	}
	if _, err := f.Create(ctx, spec("team2", "10.20.0.40")); !errors.Is(err, lifecycle.ErrIPTaken) {
		t.Errorf("Create on a used IP: %v, want ErrIPTaken", err)
	}
	f.HoldForeign(netip.MustParseAddr("10.20.0.41"))
	if _, err := f.Create(ctx, spec("team3", "10.20.0.41")); !errors.Is(err, lifecycle.ErrIPTaken) {
		t.Errorf("Create on a foreign IP: %v, want ErrIPTaken", err)
	}
	if used, _ := f.InUse(ctx, netip.MustParseAddr("10.20.0.40")); !used {
		t.Error("InUse(.40) = false")
	}
	if list, _ := f.List(ctx, "cstam-h1-"); len(list) != 1 || list[0] != ref {
		t.Errorf("List = %+v", list)
	}
	for range 2 { // idempotent
		if err := f.Delete(ctx, lifecycle.Ref{Name: ref.Name}); err != nil {
			t.Fatal(err)
		}
	}
	if used, _ := f.InUse(ctx, netip.MustParseAddr("10.20.0.40")); used {
		t.Error("InUse(.40) after Delete = true")
	}
	if err := f.Delete(ctx, lifecycle.Ref{Name: "other-vm"}); err == nil {
		t.Error("Delete of a resource that is not ours succeeded")
	}
}

func TestFakeCreatedThenTimedOut(t *testing.T) {
	ctx := context.Background()
	f := New()
	timeout := errors.New("timeout")
	f.FailCreate = func(lifecycle.Spec) error { return timeout }
	f.CreatedAnyway = true
	if _, err := f.Create(ctx, spec("team1", "10.20.0.40")); !errors.Is(err, timeout) {
		t.Fatalf("Create: %v, want the injected timeout", err)
	}
	f.FailCreate = nil
	ref, err := f.Create(ctx, spec("team1", "10.20.0.40"))
	if err != nil || ref.Server == "" {
		t.Fatalf("retry after a lost response: %+v, %v; want the existing sandbox", ref, err)
	}
}
