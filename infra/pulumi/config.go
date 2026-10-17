package main

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// Topology is the fixed IP plan of cstam-sandbox-net (see the guide, chapter 1.4).
// It must stay identical to internal/config/topology.go in the main module.
type Topology struct {
	RouterIP, CPIP, VIPIP, GW1IP, GW2IP, HostAIP, HostBIP string
}

var topo = Topology{
	RouterIP: "10.20.0.1",
	CPIP:     "10.20.0.5",
	VIPIP:    "10.20.0.10",
	GW1IP:    "10.20.0.11",
	GW2IP:    "10.20.0.12",
	HostAIP:  "10.20.0.21",
	HostBIP:  "10.20.0.22",
}

// Settings are the per-stack values from Pulumi.<stack>.yaml.
type Settings struct {
	Env               string // stack name, used in the cstam-env:<env> tag
	Network, Subnet   string // existing FelCloud network and subnet (looked up, never managed)
	ExternalNetwork   string // external network for the router gateway and floating IPs
	Image             string
	GWFlavor          string
	CPFlavor          string
	HostFlavor        string
	ServerGroupPolicy string // "anti-affinity" or "soft-anti-affinity"
	TeamNetMode       string // "podnic", "aap" or "aap-cidr"
	WithHostB         bool
	BootVolumeGB      int // 0 = boot from image; > 0 = boot from a volume of this size
	AdminCIDR         string   // who may reach SSH (22) and the dashboard (443) on the control plane
	SSHPublicKeys     []string // every teammate's public key, from infra/ssh-keys/*.pub
}

func loadSettings(ctx *pulumi.Context) (Settings, error) {
	c := config.New(ctx, "")
	s := Settings{
		Env:               ctx.Stack(),
		Network:           orDefault(c.Get("network"), "cstam-sandbox-net"),
		Subnet:            orDefault(c.Get("subnet"), "cstam-sandbox-subnet"),
		ExternalNetwork:   orDefault(c.Get("externalNetwork"), "INTERNET"),
		Image:             orDefault(c.Get("image"), "Ubuntu 24.04 LTS - Noble Numbat"),
		GWFlavor:          orDefault(c.Get("gwFlavor"), "G0.basic.1c2g"),
		CPFlavor:          orDefault(c.Get("cpFlavor"), "G0.basic.2c4g"),
		HostFlavor:        orDefault(c.Get("hostFlavor"), "G0.basic.4c8g"),
		ServerGroupPolicy: orDefault(c.Get("serverGroupPolicy"), "anti-affinity"),
		TeamNetMode:       orDefault(c.Get("teamNetMode"), "podnic"),
		WithHostB:         c.GetBool("withHostB"),
		BootVolumeGB:      c.GetInt("bootVolumeGB"),
		// Default: open to the internet. Safe only because SSH accepts keys only (no
		// passwords) and the dashboard has its own login; fail2ban runs on the control plane.
		AdminCIDR: orDefault(c.Get("adminCidr"), "0.0.0.0/0"),
	}

	switch s.ServerGroupPolicy {
	case "anti-affinity", "soft-anti-affinity":
	default:
		return s, fmt.Errorf("serverGroupPolicy must be anti-affinity or soft-anti-affinity, got %q", s.ServerGroupPolicy)
	}
	switch s.TeamNetMode {
	case "podnic", "aap", "aap-cidr":
	default:
		return s, fmt.Errorf("teamNetMode must be podnic, aap or aap-cidr, got %q", s.TeamNetMode)
	}
	if _, err := netip.ParsePrefix(s.AdminCIDR); err != nil {
		return s, fmt.Errorf("adminCidr must be a CIDR like 0.0.0.0/0 or 203.0.113.7/32: %w", err)
	}

	// Public keys are not secrets: they are committed in infra/ssh-keys/<name>.pub so that
	// every teammate's key reaches every VM. Add yours there (see docs/team/README.md).
	dir := orDefault(c.Get("sshKeysDir"), "../ssh-keys")
	files, err := filepath.Glob(filepath.Join(dir, "*.pub"))
	if err != nil {
		return s, err
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return s, fmt.Errorf("read %s: %w", f, err)
		}
		k := strings.TrimSpace(string(b))
		if !strings.HasPrefix(k, "ssh-") && !strings.HasPrefix(k, "ecdsa-") {
			return s, fmt.Errorf("%s does not look like an SSH public key", f)
		}
		s.SSHPublicKeys = append(s.SSHPublicKeys, k)
	}
	if len(s.SSHPublicKeys) == 0 {
		return s, fmt.Errorf("no SSH public key in %s: without one nobody can log in", dir)
	}
	return s, nil
}

// tags returns the tags every CSTAM resource carries. The destroy path and the
// reconciler only ever touch resources that have both of them.
func (s Settings) tags(extra ...string) pulumi.StringArray {
	out := pulumi.StringArray{pulumi.String("cstam"), pulumi.String("cstam-env:" + s.Env)}
	for _, t := range extra {
		out = append(out, pulumi.String(t))
	}
	return out
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
