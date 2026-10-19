//go:build integration

package docker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/CSTAM-well-contained/monorepo/internal/lifecycle"
)

// TestDockerSandbox creates a team container on a real Docker network, fetches its page,
// checks idempotency and IP conflicts, then deletes it.
func TestDockerSandbox(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if exec.Command("docker", "image", "inspect", DefaultImage).Run() != nil {
		t.Skipf("image %s not present (docker pull it first)", DefaultImage)
	}
	const network = "cstam-it-sandbox"
	_ = exec.Command("docker", "network", "rm", network).Run()
	// The team pool lives in 10.20.0.0/24; this fails if the demo network already uses it.
	if out, err := exec.Command("docker", "network", "create", "--subnet", "10.20.0.0/24", network).CombinedOutput(); err != nil {
		t.Skipf("cannot create the test network (is deploy/dev/demo.sh running?): %s", out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })

	ctx := context.Background()
	d, err := New(Config{Network: network, AppDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s := lifecycle.Spec{Hackathon: "it", Team: "alpha", IP: netip.MustParseAddr("10.20.0.190"), AppPort: 80}
	t.Cleanup(func() { _ = d.Delete(ctx, lifecycle.Ref{Name: s.Name()}) })

	start := time.Now()
	ref, err := d.Create(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("container running %v after Create", time.Since(start).Round(time.Millisecond))
	again, err := d.Create(ctx, s)
	if err != nil || again != ref {
		t.Fatalf("second Create = %+v, %v; want %+v", again, err, ref)
	}

	var body string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if resp, err := (&http.Client{Timeout: time.Second}).Get("http://10.20.0.190/"); err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			body = string(b)
			break
		}
	}
	if want := "team alpha of it at 10.20.0.190\n"; body != want {
		t.Fatalf("page = %q, want %q", body, want)
	}

	other := s
	other.Team = "beta"
	if _, err := d.Create(ctx, other); !errors.Is(err, lifecycle.ErrIPTaken) {
		t.Fatalf("Create on a used IP: %v, want ErrIPTaken", err)
	}
	_ = d.Delete(ctx, lifecycle.Ref{Name: other.Name()})

	if used, err := d.InUse(ctx, s.IP); err != nil || !used {
		t.Fatalf("InUse = %v, %v", used, err)
	}
	if refs, _ := d.List(ctx, "cstam-it-"); len(refs) != 1 || refs[0].Name != s.Name() {
		t.Fatalf("List = %+v", refs)
	}
	for range 2 {
		if err := d.Delete(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	if used, _ := d.InUse(ctx, s.IP); used {
		t.Fatal("IP still in use after Delete")
	}
	if out, _ := exec.Command("docker", "ps", "-a", "-q", "--filter", "label="+Label+"=it").Output(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("containers left: %s", out)
	}
}
