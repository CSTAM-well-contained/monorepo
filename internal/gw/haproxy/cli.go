package haproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Socket is one HAProxy CLI socket (runtime API or master CLI). Each command opens a
// connection, sends one line (plus an optional payload) and reads until HAProxy closes.
type Socket struct {
	Path    string
	Timeout time.Duration // per command when ctx has no deadline; default 2 s
}

// Do runs one command and returns HAProxy's answer without the trailing newlines.
func (s Socket) Do(ctx context.Context, cmd string) (string, error) {
	if strings.ContainsAny(cmd, "\r\n") {
		return "", fmt.Errorf("haproxy cli: command contains a newline: %q", cmd)
	}
	return s.do(ctx, cmd+"\n")
}

func (s Socket) do(ctx context.Context, raw string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		t := s.Timeout
		if t == 0 {
			t = 2 * time.Second
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t)
		defer cancel()
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", s.Path)
	if err != nil {
		return "", fmt.Errorf("haproxy cli %s: %w", s.Path, err)
	}
	defer func() { _ = conn.Close() }()
	dl, _ := ctx.Deadline()
	if err := conn.SetDeadline(dl); err != nil {
		return "", err
	}
	if _, err := io.WriteString(conn, raw); err != nil {
		return "", fmt.Errorf("haproxy cli write: %w", err)
	}
	out, err := io.ReadAll(conn)
	if err != nil {
		return "", fmt.Errorf("haproxy cli read: %w", err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// Runtime wraps the commands the applier needs on the runtime API socket.
type Runtime struct{ Socket }

func (r Runtime) expectEmpty(ctx context.Context, cmd string) error {
	out, err := r.Do(ctx, cmd)
	if err != nil {
		return err
	}
	if out != "" {
		return fmt.Errorf("haproxy %q: %s", cmd, out)
	}
	return nil
}

// SetServer points slot's server to ep and puts it in ready state.
func (r Runtime) SetServer(ctx context.Context, slot int, ep Endpoint) error {
	srv := SlotName(slot) + "/s"
	cmd := fmt.Sprintf("set server %s addr %s port %d", srv, ep.Addr, ep.Port)
	out, err := r.Do(ctx, cmd)
	if err != nil {
		return err
	}
	if !strings.Contains(out, "changed") && !strings.Contains(out, "no need to change") {
		return fmt.Errorf("haproxy %q: %s", cmd, out)
	}
	return r.expectEmpty(ctx, "set server "+srv+" state ready")
}

// ParkServer stops sending traffic to slot, cuts its sessions and parks its address.
func (r Runtime) ParkServer(ctx context.Context, slot int) error {
	srv := SlotName(slot) + "/s"
	if err := r.expectEmpty(ctx, "set server "+srv+" state maint"); err != nil {
		return err
	}
	if err := r.expectEmpty(ctx, "shutdown sessions server "+srv); err != nil {
		return err
	}
	out, err := r.Do(ctx, fmt.Sprintf("set server %s addr %s port 80", srv, ParkAddr))
	if err != nil {
		return err
	}
	if !strings.Contains(out, "changed") && !strings.Contains(out, "no need to change") {
		return fmt.Errorf("haproxy park %s: %s", srv, out)
	}
	return nil
}

// ReplaceMap swaps the whole content of a loaded map atomically: prepare a new version,
// fill it, commit. A failure before the commit leaves the live map untouched.
func (r Runtime) ReplaceMap(ctx context.Context, path string, entries []MapEntry) error {
	out, err := r.Do(ctx, "prepare map "+path)
	if err != nil {
		return err
	}
	const prefix = "New version created: "
	if !strings.HasPrefix(out, prefix) {
		return fmt.Errorf("haproxy prepare map %s: %s", path, out)
	}
	ver, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(out, prefix)), 10, 32)
	if err != nil {
		return fmt.Errorf("haproxy prepare map %s: version in %q: %w", path, out, err)
	}
	at := fmt.Sprintf("@%d", ver)
	if len(entries) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "add map %s %s <<\n", at, path)
		for _, e := range entries {
			if strings.ContainsAny(e.Key+e.Value, " \t\r\n") {
				return fmt.Errorf("map entry %q %q contains white space", e.Key, e.Value)
			}
			b.WriteString(e.Key + " " + e.Value + "\n")
		}
		b.WriteString("\n")
		if out, err := r.do(ctx, b.String()); err != nil || out != "" {
			_, _ = r.Do(ctx, fmt.Sprintf("clear map %s %s", at, path))
			if err == nil {
				err = errors.New(out)
			}
			return fmt.Errorf("haproxy add map %s: %w", path, err)
		}
	}
	return r.expectEmpty(ctx, fmt.Sprintf("commit map %s %s", at, path))
}

// Master is the master CLI socket (haproxy -S).
type Master struct{ Socket }

// Reload asks the master for a hitless reload and waits for its verdict.
// HAProxy >= 2.7 answers "Success=1" once the new worker is ready, "Success=0" if it failed;
// the startup log follows a "--" line.
func (m Master) Reload(ctx context.Context) error {
	out, err := m.Do(ctx, "reload")
	if err != nil {
		return err
	}
	first, rest, _ := strings.Cut(out, "\n")
	switch strings.TrimSpace(first) {
	case "Success=1":
		return nil
	case "Success=0":
		return fmt.Errorf("haproxy reload failed: %s", strings.TrimSpace(rest))
	default:
		return fmt.Errorf("haproxy reload: unexpected answer %q", out)
	}
}

// Checker runs `haproxy -c -f <file>`. Argv is the command prefix: ["haproxy"] on a gateway,
// ["docker", "exec", "<ctr>", "haproxy"] in the integration tests.
type Checker struct{ Argv []string }

// Check returns nil if HAProxy accepts the config file.
func (c Checker) Check(ctx context.Context, path string) error {
	if len(c.Argv) == 0 {
		return errors.New("haproxy checker: empty argv")
	}
	args := append(append([]string{}, c.Argv[1:]...), "-c", "-f", path)
	out, err := exec.CommandContext(ctx, c.Argv[0], args...).CombinedOutput() //nolint:gosec // argv comes from agent config, not from requests
	if err != nil {
		return fmt.Errorf("haproxy -c: %w: %s", err, CheckErrors(string(out)))
	}
	return nil
}

// CheckErrors keeps the [ALERT] lines of `haproxy -c` output (the reason a config is refused),
// without the version banner and the [NOTICE] lines. With no alert it returns the whole output.
func CheckErrors(out string) string {
	var alerts []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "[ALERT]") {
			alerts = append(alerts, strings.TrimSpace(l))
		}
	}
	if len(alerts) == 0 {
		return strings.TrimSpace(out)
	}
	return strings.Join(alerts, " | ")
}
