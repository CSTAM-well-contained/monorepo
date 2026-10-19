// Package docker is the local sandbox driver: one container per team on a Docker network
// that uses the platform IP plan (10.20.0.0/24), at the team's IPAM address. It lets the
// whole stack (cpd, gateways, teams) run on one laptop. It implements lifecycle.Sandboxes
// by calling the docker CLI with argv slices (no shell).
package docker

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/CSTAM-well-contained/monorepo/internal/lifecycle"
)

// Label put on every team container: List and the GC only look at labelled containers.
const Label = "cstam.sandbox"

// DefaultImage serves the default team page. HAProxy is already on every dev machine
// (integration tests), so the demo downloads nothing more.
const DefaultImage = "haproxy:3.2"

// defaultApp answers every request with the team name and address. HAProxy expands the
// environment variables set by Create.
const defaultApp = `global
    maxconn 512
defaults
    mode http
    timeout connect 2s
    timeout client 10s
    timeout server 10s
frontend app
    bind ":${APP_PORT}"
    http-request return status 200 content-type text/plain lf-string "team %[env(TEAM)] of %[env(HACKATHON)] at %[env(TEAM_IP)]\n"
`

// Runner runs the docker CLI and returns its combined output. Tests replace it.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// CLI runs the real docker binary.
func CLI(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "docker", args...).CombinedOutput() //nolint:gosec // argv, validated names
}

// Config of the driver.
type Config struct {
	Network string // Docker network with the platform subnet (required)
	Image   string // image of the default page (default DefaultImage)
	// AppDir is where the default page config is written (default: a dir in os.TempDir).
	AppDir string
	Memory string        // per team container, default 128m
	CPUs   string        // per team container, default 0.5
	Run    Runner        // default CLI
	Wait   time.Duration // how long Create waits for the container to run (default 20 s)
}

// Driver creates team containers.
type Driver struct {
	cfg     Config
	appConf string // host path of the default page config
}

var _ lifecycle.Sandboxes = (*Driver)(nil)

// New checks the config and writes the default page config.
func New(cfg Config) (*Driver, error) {
	if cfg.Network == "" {
		return nil, errors.New("docker driver: network is required")
	}
	if cfg.Image == "" {
		cfg.Image = DefaultImage
	}
	if cfg.Memory == "" {
		cfg.Memory = "128m"
	}
	if cfg.CPUs == "" {
		cfg.CPUs = "0.5"
	}
	if cfg.Run == nil {
		cfg.Run = CLI
	}
	if cfg.Wait == 0 {
		cfg.Wait = 20 * time.Second
	}
	if cfg.AppDir == "" {
		cfg.AppDir = filepath.Join(os.TempDir(), "cstam-docker-sandbox")
	}
	if err := os.MkdirAll(cfg.AppDir, 0o755); err != nil { //nolint:gosec // read by the container user
		return nil, fmt.Errorf("docker driver: %w", err)
	}
	conf := filepath.Join(cfg.AppDir, "team-app.cfg")
	if err := os.WriteFile(conf, []byte(defaultApp), 0o644); err != nil { //nolint:gosec // public page config
		return nil, fmt.Errorf("docker driver: %w", err)
	}
	return &Driver{cfg: cfg, appConf: conf}, nil
}

// container is what docker inspect tells us about a team container.
type container struct {
	ID, IP string
	State  string
}

// inspect returns the container named name, or ok=false if there is none.
func (d *Driver) inspect(ctx context.Context, name string) (c container, ok bool, err error) {
	format := `{{.Id}} {{.State.Status}} {{with index .NetworkSettings.Networks "` + d.cfg.Network + `"}}{{.IPAddress}}{{end}}`
	out, err := d.cfg.Run(ctx, "container", "inspect", "--format", format, name)
	if err != nil {
		if strings.Contains(string(out), "No such container") || strings.Contains(string(out), "No such object") {
			return container{}, false, nil
		}
		return container{}, false, fmt.Errorf("docker inspect %s: %w: %s", name, err, firstLine(out))
	}
	f := strings.Fields(string(out))
	if len(f) < 2 {
		return container{}, false, fmt.Errorf("docker inspect %s: unexpected output %q", name, out)
	}
	c = container{ID: f[0], State: f[1]}
	if len(f) > 2 {
		c.IP = f[2]
	}
	return c, true, nil
}

// Create runs the team container, or returns the existing one.
func (d *Driver) Create(ctx context.Context, s lifecycle.Spec) (lifecycle.Ref, error) {
	if err := s.Validate(); err != nil {
		return lifecycle.Ref{}, err
	}
	name := s.Name()
	ref := lifecycle.Ref{Name: name}
	c, ok, err := d.inspect(ctx, name)
	if err != nil {
		return ref, err
	}
	if ok && c.IP != "" && c.IP != s.IP.String() {
		return ref, fmt.Errorf("container %s exists with %s: %w", name, c.IP, lifecycle.ErrIPTaken)
	}
	if !ok {
		out, err := d.cfg.Run(ctx, d.runArgs(s)...)
		if err != nil {
			msg := string(out)
			switch {
			case strings.Contains(msg, "Address already in use"), strings.Contains(msg, "address already in use"):
				return ref, fmt.Errorf("%s: %w", s.IP, lifecycle.ErrIPTaken)
			case strings.Contains(msg, "is already in use by container"): // lost a race with ourselves
			default:
				return ref, fmt.Errorf("docker run %s: %w: %s", name, err, firstLine(out))
			}
		}
	}
	return d.waitRunning(ctx, name)
}

func (d *Driver) runArgs(s lifecycle.Spec) []string {
	port := strconv.Itoa(s.AppPort)
	args := []string{"run", "-d", "--name", s.Name(),
		"--network", d.cfg.Network, "--ip", s.IP.String(),
		"--hostname", s.Team,
		"--label", Label + "=" + s.Hackathon, "--label", "cstam.team=" + s.Team,
		"--restart", "unless-stopped",
		"--memory", d.cfg.Memory, "--cpus", d.cfg.CPUs, "--pids-limit", "256",
		"--security-opt", "no-new-privileges",
		// the app may listen on a port below 1024 without any capability
		"--sysctl", "net.ipv4.ip_unprivileged_port_start=0",
		"-e", "TEAM=" + s.Team, "-e", "HACKATHON=" + s.Hackathon,
		"-e", "TEAM_IP=" + s.IP.String(), "-e", "APP_PORT=" + port, "-e", "PORT=" + port,
	}
	if s.Image != "" { // custom images (nginx...) may need CHOWN or SETUID at start
		return append(args, "--cap-drop", "NET_RAW", s.Image)
	}
	return append(args, "--cap-drop", "ALL", "-v", d.appConf+":/usr/local/etc/haproxy/haproxy.cfg:ro", d.cfg.Image)
}

func (d *Driver) waitRunning(ctx context.Context, name string) (lifecycle.Ref, error) {
	ref := lifecycle.Ref{Name: name}
	ctx, cancel := context.WithTimeout(ctx, d.cfg.Wait)
	defer cancel()
	for {
		c, ok, err := d.inspect(ctx, name)
		if err != nil {
			return ref, err
		}
		if !ok {
			return ref, fmt.Errorf("container %s disappeared", name)
		}
		ref.Server = c.ID
		switch c.State {
		case "running":
			return ref, nil
		case "exited", "dead":
			logs, _ := d.cfg.Run(ctx, "logs", "--tail", "5", name)
			return ref, fmt.Errorf("container %s %s: %s", name, c.State, firstLine(logs))
		}
		select {
		case <-ctx.Done():
			return ref, fmt.Errorf("container %s not running (%s): %w", name, c.State, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Delete removes the container named ref.Name; a missing one is not an error.
func (d *Driver) Delete(ctx context.Context, ref lifecycle.Ref) error {
	if !strings.HasPrefix(ref.Name, lifecycle.NamePrefix) {
		return fmt.Errorf("docker driver: refusing to delete %q: not ours", ref.Name)
	}
	out, err := d.cfg.Run(ctx, "rm", "-f", ref.Name)
	if err != nil && !strings.Contains(string(out), "No such container") {
		return fmt.Errorf("docker rm %s: %w: %s", ref.Name, err, firstLine(out))
	}
	return nil
}

// InUse reports whether any container on the network holds ip.
func (d *Driver) InUse(ctx context.Context, ip netip.Addr) (bool, error) {
	out, err := d.cfg.Run(ctx, "network", "inspect", "--format",
		`{{range .Containers}}{{.IPv4Address}} {{end}}`, d.cfg.Network)
	if err != nil {
		return false, fmt.Errorf("docker network inspect %s: %w: %s", d.cfg.Network, err, firstLine(out))
	}
	for _, f := range strings.Fields(string(out)) {
		if p, err := netip.ParsePrefix(f); err == nil && p.Addr() == ip {
			return true, nil
		}
	}
	return false, nil
}

// List returns the team containers whose name starts with prefix, sorted by name.
func (d *Driver) List(ctx context.Context, prefix string) ([]lifecycle.Ref, error) {
	out, err := d.cfg.Run(ctx, "ps", "-a", "--filter", "label="+Label, "--format", "{{.Names}} {{.ID}}")
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w: %s", err, firstLine(out))
	}
	var refs []lifecycle.Ref
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.HasPrefix(f[0], prefix) {
			refs = append(refs, lifecycle.Ref{Name: f[0], Server: f[1]})
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
