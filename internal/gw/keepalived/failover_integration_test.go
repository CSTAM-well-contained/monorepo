//go:build integration

package keepalived

import (
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Two containers play gw1 and gw2 on a Docker bridge, share a VIP with VRRP unicast, and run
// HAProxy behind the chk_gateway script. The test measures how long the VIP is unreachable.

const labImage = "cstam-lab-gw:test"

var (
	labNet  = "cstam-lab-vrrp"
	labCIDR = "172.31.250.0/24"
	vip     = netip.MustParseAddr("172.31.250.10")
	gwIPs   = []netip.Addr{netip.MustParseAddr("172.31.250.11"), netip.MustParseAddr("172.31.250.12")}
)

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

const haproxyCfg = `global
    master-worker
defaults
    mode http
    timeout connect 1s
    timeout client 5s
    timeout server 5s
frontend fe
    bind :80
    http-request return status 200 content-type text/plain lf-string "%[env(GW_NAME)]\n" if { path /__gw/healthz }
    http-request return status 200 content-type text/plain lf-string "%[env(GW_NAME)]\n"
`

type gw struct{ name, ctr string }

func startPair(t *testing.T, trackProcess bool) []gw {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if exec.Command("docker", "image", "inspect", labImage).Run() != nil {
		run(t, "docker", "build", "-q", "-t", labImage, "testdata")
	}
	_ = exec.Command("docker", "network", "rm", labNet).Run()
	run(t, "docker", "network", "create", "--subnet", labCIDR, labNet)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", labNet).Run() })

	var gws []gw
	for i, name := range []string{"gw1", "gw2"} {
		dir := t.TempDir()
		s := Settings{
			RouterID: "cstam-" + name, Interface: "eth0", VRID: 51,
			Priority: []int{150, 100}[i], AdvertInt: 500 * time.Millisecond,
			SrcIP: gwIPs[i], PeerIP: gwIPs[1-i], VIP: vip, ScriptUser: "root",
			CheckScript: "/usr/local/libexec/cstam/chk_gateway", NotifyScript: "/usr/local/libexec/cstam/vrrp-notify",
			ForceBackupFile: "/run/cstam/force-backup", TrackProcess: trackProcess,
		}
		conf, err := Render(s)
		if err != nil {
			t.Fatal(err)
		}
		files := map[string][]byte{"keepalived.conf": conf, "haproxy.cfg": []byte(haproxyCfg), "chk_gateway": CheckScript, "vrrp-notify": NotifyScript}
		for f, data := range files {
			if err := os.WriteFile(filepath.Join(dir, f), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		ctr := fmt.Sprintf("cstam-lab-%s-%d", name, time.Now().UnixNano())
		run(t, "docker", "run", "-d", "--name", ctr, "--network", labNet, "--ip", gwIPs[i].String(),
			"--cap-add", "NET_ADMIN", "--cap-add", "NET_RAW", "--cap-add", "NET_BROADCAST",
			"-e", "GW_NAME="+name, "-v", dir+":/lab:ro", "--entrypoint", "sleep", labImage, "infinity")
		t.Cleanup(func() {
			if t.Failed() {
				out, _ := exec.Command("docker", "logs", ctr).CombinedOutput()
				t.Logf("%s logs:\n%s", name, out)
			}
			_ = exec.Command("docker", "rm", "-f", ctr).Run()
		})
		// scripts must be root-owned and not writable by others (enable_script_security)
		run(t, "docker", "exec", ctr, "sh", "-ec", `
			install -d /usr/local/libexec/cstam /run/cstam /etc/keepalived
			install -m 0755 /lab/chk_gateway /lab/vrrp-notify /usr/local/libexec/cstam/
			install -m 0644 /lab/keepalived.conf /etc/keepalived/keepalived.conf
			install -m 0644 /lab/haproxy.cfg /etc/haproxy.cfg
			touch /run/cstam/ready`)
		run(t, "docker", "exec", "-d", "-e", "GW_NAME="+name, ctr, "haproxy", "-W", "-db", "-f", "/etc/haproxy.cfg")
		run(t, "docker", "exec", ctr, "keepalived", "-t", "-f", "/etc/keepalived/keepalived.conf")
		run(t, "docker", "exec", "-d", ctr, "keepalived", "-n", "-l", "-D", "-f", "/etc/keepalived/keepalived.conf")
		gws = append(gws, gw{name, ctr})
	}
	// gw1 has the higher priority: it must own the VIP once both are up
	waitServedBy(t, "gw1", 15*time.Second)
	return gws
}

// probe returns which gateway answered on the VIP, or "" if none did within 100 ms.
var client = &http.Client{Timeout: 100 * time.Millisecond, Transport: &http.Transport{DisableKeepAlives: true}}

func probe() string {
	resp, err := client.Get("http://" + vip.String() + "/")
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b))
}

func waitServedBy(t *testing.T, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if probe() == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("VIP never served by %s within %v (now: %q)", want, within, probe())
}

// outage probes every 20 ms after fail() and returns how long the VIP did not answer
// before gw2 served it.
func outage(t *testing.T, fail func()) time.Duration {
	t.Helper()
	fail()
	start := time.Now()
	var firstDown time.Time
	for time.Since(start) < 15*time.Second {
		got := probe()
		switch {
		case got == "gw2":
			if firstDown.IsZero() {
				return 0
			}
			return time.Since(firstDown)
		case firstDown.IsZero():
			firstDown = time.Now()
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("gw2 never took the VIP")
	return 0
}

// track_process is not tested here: it needs netlink process events, which the kernel only
// sends to the initial PID namespace, so it stays in FAULT inside a container. Test it on the VMs.
func TestFailover(t *testing.T) {
	cases := []struct {
		name  string
		track bool
		fail  func(t *testing.T, gw1 gw)
		max   time.Duration
	}{
		{"haproxy killed, caught by chk_gateway", false, func(t *testing.T, g gw) {
			run(t, "docker", "exec", g.ctr, "pkill", "-9", "haproxy")
		}, 3500 * time.Millisecond},
		{"manual failover via force-backup file", false, func(t *testing.T, g gw) {
			run(t, "docker", "exec", g.ctr, "sh", "-c", "echo 1 > /run/cstam/force-backup")
		}, time.Second},
		{"clean keepalived stop", false, func(t *testing.T, g gw) {
			run(t, "docker", "exec", g.ctr, "pkill", "-TERM", "keepalived")
		}, time.Second},
		{"silent failure: gw1 frozen", false, func(t *testing.T, g gw) {
			run(t, "docker", "pause", g.ctr)
		}, MasterDownInterval(500*time.Millisecond, 100) + 700*time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gws := startPair(t, tc.track)
			d := outage(t, func() { tc.fail(t, gws[0]) })
			t.Logf("VIP unreachable for %v (limit %v)", d.Round(time.Millisecond), tc.max)
			if d > tc.max {
				t.Fatalf("outage %v > %v", d, tc.max)
			}
			// nopreempt: gw2 keeps the VIP; the notify script recorded MASTER on gw2
			if st := run(t, "docker", "exec", gws[1].ctr, "cat", "/run/cstam/vrrp.state"); !strings.HasPrefix(st, "MASTER") {
				t.Fatalf("gw2 state file: %q", st)
			}
			_ = exec.Command("docker", "unpause", gws[0].ctr).Run()
		})
	}
}

func TestRenderAndState(t *testing.T) {
	if d := MasterDownInterval(500*time.Millisecond, 100); d.Round(time.Millisecond) != 1805*time.Millisecond {
		t.Fatalf("master down interval %v", d)
	}
	st, err := ParseState("MASTER 1760000000.5 150\n")
	if err != nil || st.Name != "MASTER" || st.Priority != 150 {
		t.Fatalf("%+v %v", st, err)
	}
}
