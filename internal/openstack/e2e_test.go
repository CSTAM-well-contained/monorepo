//go:build e2e

package openstack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/CSTAM-well-contained/monorepo/internal/config"
	"github.com/CSTAM-well-contained/monorepo/internal/lifecycle"
)

// stackIDs reads the network IDs from the env, or from `pulumi stack output` of infra/pulumi.
func stackIDs(t *testing.T) Config {
	t.Helper()
	cfg := Config{
		NetworkID:      os.Getenv("CSTAM_NETWORK_ID"),
		SubnetID:       os.Getenv("CSTAM_SUBNET_ID"),
		TeamSecGroupID: os.Getenv("CSTAM_TEAM_SG_ID"),
	}
	if cfg.NetworkID != "" {
		return cfg
	}
	out, err := exec.Command("pulumi", "stack", "output", "--json", "-s", "prod", "--cwd", "../../infra/pulumi").Output()
	if err != nil {
		t.Skipf("no CSTAM_NETWORK_ID and pulumi stack output failed: %v", err)
	}
	var o struct{ NetworkID, SubnetID, TeamSecGroupID string }
	if err := json.Unmarshal(out, &o); err != nil {
		t.Fatalf("pulumi stack output: %v", err)
	}
	cfg.NetworkID, cfg.SubnetID, cfg.TeamSecGroupID = o.NetworkID, o.SubnetID, o.TeamSecGroupID
	return cfg
}

// TestE2ESandbox creates one real team VM on a free lab-pool IP and deletes it.
func TestE2ESandbox(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	d, err := New(ctx, stackIDs(t))
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	spec := lifecycle.Spec{Hackathon: "e2e", Team: "t" + hex.EncodeToString(b), AppPort: 80}
	for ip := config.LabPoolHi; ip.Compare(config.LabPoolLo) >= 0; ip = ip.Prev() {
		used, err := d.InUse(ctx, ip)
		if err != nil {
			t.Fatal(err)
		}
		if !used {
			spec.IP = ip
			break
		}
	}
	if !spec.IP.IsValid() {
		t.Skip("no free lab-pool IP")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := d.Delete(ctx, lifecycle.Ref{Name: spec.Name()}); err != nil {
			t.Errorf("cleanup of %s: %v", spec.Name(), err)
		}
	})

	start := time.Now()
	ref, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("create %s on %s: %v", spec.Name(), spec.IP, err)
	}
	t.Logf("created %s on %s in %v (server %s, port %s)", ref.Name, spec.IP, time.Since(start).Round(time.Second), ref.Server, ref.Port)
	if ref.Server == "" || ref.Port == "" {
		t.Errorf("ref %+v lacks IDs", ref)
	}
	if again, err := d.Create(ctx, spec); err != nil || again != ref {
		t.Errorf("second Create = %+v, %v; want the same %+v", again, err, ref)
	}
	if used, err := d.InUse(ctx, spec.IP); err != nil || !used {
		t.Errorf("InUse(%s) = %v, %v; want true", spec.IP, used, err)
	}

	start = time.Now()
	if err := d.Delete(ctx, ref); err != nil {
		t.Fatalf("delete: %v", err)
	}
	t.Logf("deleted in %v", time.Since(start).Round(time.Second))
	left, err := d.List(ctx, lifecycle.NamePrefix+"e2e-")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("left after delete: %+v", left)
	}
	if used, err := d.InUse(ctx, spec.IP); err != nil || used {
		t.Errorf("InUse(%s) after delete = %v, %v; want false", spec.IP, used, err)
	}
}
