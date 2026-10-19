package docker

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/CSTAM-well-contained/monorepo/internal/lifecycle"
)

// script answers docker commands by their first two args.
type script struct {
	calls   [][]string
	answers map[string]func(args []string) (string, error)
}

func (s *script) run(_ context.Context, args ...string) ([]byte, error) {
	s.calls = append(s.calls, args)
	key := strings.Join(args[:min(2, len(args))], " ")
	if f, ok := s.answers[key]; ok {
		out, err := f(args)
		return []byte(out), err
	}
	return nil, errors.New("unexpected call: " + strings.Join(args, " "))
}

func (s *script) called(prefix string) bool {
	return slices.ContainsFunc(s.calls, func(a []string) bool { return strings.HasPrefix(strings.Join(a, " "), prefix) })
}

var errExit = errors.New("exit status 1")

func spec() lifecycle.Spec {
	return lifecycle.Spec{Hackathon: "h1", Team: "alpha", IP: netip.MustParseAddr("10.20.0.32"), AppPort: 8080}
}

func newDriver(t *testing.T, s *script) *Driver {
	t.Helper()
	d, err := New(Config{Network: "cstam-demo", Run: s.run, AppDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCreate(t *testing.T) {
	notFound := func([]string) (string, error) { return "Error: No such container: cstam-h1-alpha", errExit }
	running := func([]string) (string, error) { return "abc123 running 10.20.0.32\n", nil }
	tests := []struct {
		name    string
		inspect []func([]string) (string, error) // successive answers
		run     func([]string) (string, error)
		wantErr error
		wantRun bool
	}{
		{"new container", []func([]string) (string, error){notFound, running},
			func([]string) (string, error) { return "abc123\n", nil }, nil, true},
		{"already exists", []func([]string) (string, error){running, running}, nil, nil, false},
		{"exists but stopped", []func([]string) (string, error){
			func([]string) (string, error) { return "abc123 exited 10.20.0.32\n", nil }, running}, nil, nil, false},
		{"exists with another IP", []func([]string) (string, error){
			func([]string) (string, error) { return "abc123 running 10.20.0.40\n", nil }}, nil, lifecycle.ErrIPTaken, false},
		{"IP held by someone else", []func([]string) (string, error){notFound},
			func([]string) (string, error) {
				return "docker: Error response from daemon: Address already in use.", errExit
			}, lifecycle.ErrIPTaken, true},
		{"app crashes", []func([]string) (string, error){notFound,
			func([]string) (string, error) { return "abc123 exited \n", nil }},
			func([]string) (string, error) { return "abc123\n", nil }, errAny, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i := 0
			s := &script{answers: map[string]func([]string) (string, error){
				"container inspect": func(a []string) (string, error) {
					f := tt.inspect[min(i, len(tt.inspect)-1)]
					i++
					return f(a)
				},
				"run -d":               tt.run,
				"logs --tail":          func([]string) (string, error) { return "boom", nil },
				"start cstam-h1-alpha": func([]string) (string, error) { return "cstam-h1-alpha", nil },
			}}
			ref, err := newDriver(t, s).Create(context.Background(), spec())
			switch {
			case errors.Is(tt.wantErr, errAny) && err == nil, !errors.Is(tt.wantErr, errAny) && !errors.Is(err, tt.wantErr):
				t.Fatalf("Create: %v, want %v", err, tt.wantErr)
			}
			if ref.Name != "cstam-h1-alpha" {
				t.Errorf("ref name %q", ref.Name)
			}
			if err == nil && ref.Server != "abc123" {
				t.Errorf("ref server %q", ref.Server)
			}
			if s.called("run -d") != tt.wantRun {
				t.Errorf("docker run called = %v, want %v", !tt.wantRun, tt.wantRun)
			}
		})
	}
}

var errAny = errors.New("any error")

func TestRunArgs(t *testing.T) {
	d := newDriver(t, &script{})
	args := strings.Join(d.runArgs(spec()), " ")
	for _, want := range []string{"--network cstam-demo", "--ip 10.20.0.32", "--name cstam-h1-alpha",
		"--label cstam.sandbox=h1", "-e APP_PORT=8080", "--cap-drop ALL", "haproxy:3.2"} {
		if !strings.Contains(args, want) {
			t.Errorf("default app args lack %q: %s", want, args)
		}
	}
	s := spec()
	s.Image = "nginx:1.27"
	args = strings.Join(d.runArgs(s), " ")
	if !strings.HasSuffix(args, " nginx:1.27") || strings.Contains(args, "haproxy.cfg") {
		t.Errorf("custom image args: %s", args)
	}
}

func TestCreateRejectsInvalidSpec(t *testing.T) {
	s := &script{}
	bad := spec()
	bad.Team = "Bad Name; rm -rf /"
	if _, err := newDriver(t, s).Create(context.Background(), bad); !errors.Is(err, lifecycle.ErrInvalidSpec) {
		t.Fatalf("Create: %v, want ErrInvalidSpec", err)
	}
	if len(s.calls) != 0 {
		t.Fatalf("docker was called: %v", s.calls)
	}
}

func TestDelete(t *testing.T) {
	s := &script{answers: map[string]func([]string) (string, error){
		"rm -f": func(a []string) (string, error) {
			if a[2] == "cstam-h1-gone" {
				return "Error response from daemon: No such container: cstam-h1-gone", errExit
			}
			return a[2], nil
		},
	}}
	d := newDriver(t, s)
	for _, name := range []string{"cstam-h1-alpha", "cstam-h1-gone"} {
		if err := d.Delete(context.Background(), lifecycle.Ref{Name: name}); err != nil {
			t.Errorf("Delete %s: %v", name, err)
		}
	}
	if err := d.Delete(context.Background(), lifecycle.Ref{Name: "postgres"}); err == nil {
		t.Error("Delete of a container that is not ours succeeded")
	}
}

func TestInUseAndList(t *testing.T) {
	s := &script{answers: map[string]func([]string) (string, error){
		"network inspect": func([]string) (string, error) { return "10.20.0.11/24 10.20.0.32/24 ", nil },
		"ps -a": func([]string) (string, error) {
			return "cstam-h2-b 222\ncstam-h1-b 111\ncstam-h1-a 000\n", nil
		},
	}}
	d := newDriver(t, s)
	for ip, want := range map[string]bool{"10.20.0.32": true, "10.20.0.33": false, "10.20.0.11": true} {
		got, err := d.InUse(context.Background(), netip.MustParseAddr(ip))
		if err != nil || got != want {
			t.Errorf("InUse(%s) = %v, %v; want %v", ip, got, err, want)
		}
	}
	refs, err := d.List(context.Background(), "cstam-h1-")
	if err != nil {
		t.Fatal(err)
	}
	want := []lifecycle.Ref{{Name: "cstam-h1-a", Server: "000"}, {Name: "cstam-h1-b", Server: "111"}}
	if !slices.Equal(refs, want) {
		t.Errorf("List = %+v, want %+v", refs, want)
	}
}
