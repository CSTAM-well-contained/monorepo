// Command gwagent is the CSTAM gateway agent: keeps HAProxy at the desired routes from cpd,
// reports the generation it serves and the VRRP state, and answers the HAProxy canary.
//
// The gateway token comes from the environment (CSTAM_GATEWAY_TOKEN), never from a flag.
// `gwagent -bootstrap` writes haproxy.cfg and the static files, checks them and exits: run it
// before HAProxy starts (systemd ExecStartPre), then run gwagent itself. With -vrrp-iface it
// also writes keepalived.conf and the VRRP scripts (start keepalived after it).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/CSTAM-well-contained/monorepo/internal/config"
	"github.com/CSTAM-well-contained/monorepo/internal/gw/agent"
	"github.com/CSTAM-well-contained/monorepo/internal/gw/applier"
	"github.com/CSTAM-well-contained/monorepo/internal/gw/haproxy"
	"github.com/CSTAM-well-contained/monorepo/internal/gw/keepalived"
	"github.com/CSTAM-well-contained/monorepo/internal/gw/logs"
)

var version = "dev" // set by -ldflags "-X main.version=..."

type flags struct {
	name, cpd, runDir, stateFile                  string
	dir, adminSock, masterSock, haproxyCmd        string
	httpBind, statsBind, canary, cfgVersion, user string
	trafficLog                                    string
	vrrpIface, kaConf, kaScriptUser               string
	kaTrackProcess                                bool
	slots, rateLimit                              int
	bootstrap                                     bool
}

func main() {
	var f flags
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.StringVar(&f.name, "name", "", "gateway name: gw1 or gw2 (required)")
	flag.StringVar(&f.cpd, "cpd", "http://"+config.ControlIP.String()+":"+strconv.Itoa(config.CPDPort), "cpd base URL")
	flag.StringVar(&f.runDir, "run-dir", "/run/cstam", "ready file and vrrp.state")
	flag.StringVar(&f.stateFile, "state-file", "/var/lib/cstam/desired.json", "last applied routes")
	flag.StringVar(&f.dir, "haproxy-dir", "/etc/haproxy", "haproxy.cfg, maps/ and errors/")
	flag.StringVar(&f.adminSock, "admin-socket", "/run/haproxy/admin.sock", "HAProxy runtime API socket")
	flag.StringVar(&f.masterSock, "master-socket", "/run/haproxy/master.sock", "HAProxy master CLI socket")
	flag.StringVar(&f.haproxyCmd, "haproxy-cmd", "haproxy", "command that runs haproxy (split on spaces, no shell)")
	flag.StringVar(&f.httpBind, "http-bind", ":80", "HAProxy HTTP listener")
	flag.StringVar(&f.statsBind, "stats-bind", "", "HAProxy stats listener, e.g. 10.20.0.11:8404 (empty: none)")
	flag.StringVar(&f.canary, "canary", "127.0.0.1:9102", "canary responder address")
	flag.StringVar(&f.trafficLog, "traffic-log", "127.0.0.1:5514", "UDP address HAProxy sends its traffic log to, read by the agent for the dashboard (empty: none)")
	flag.StringVar(&f.cfgVersion, "config-version", "v1", "HAProxy config version, served in X-Config-Version")
	flag.StringVar(&f.user, "haproxy-user", "", "user/group HAProxy drops to (empty: keep)")
	flag.IntVar(&f.slots, "slots", 128, "team slot backends")
	flag.IntVar(&f.rateLimit, "rate-limit", 2000, "requests per 10 s per Host")
	flag.BoolVar(&f.bootstrap, "bootstrap", false, "write and check the HAProxy files, then exit")
	flag.StringVar(&f.vrrpIface, "vrrp-iface", "", "with -bootstrap: VRRP interface, e.g. ens3; also writes the keepalived files")
	flag.StringVar(&f.kaConf, "keepalived-conf", keepalived.DefaultConf, "where to write keepalived.conf")
	flag.StringVar(&f.kaScriptUser, "vrrp-script-user", "keepalived_script", "user running the VRRP check script")
	flag.BoolVar(&f.kaTrackProcess, "vrrp-track-process", true, "watch the haproxy process (off in containers)")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("app", "gwagent", "gw", f.name, "version", version)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log, f); err != nil {
		log.Error("gwagent stopped", "err", err.Error())
		os.Exit(1)
	}
}

func settings(f flags) haproxy.Settings {
	sources := []string{"127.0.0.1", config.ControlIP.String()}
	for _, gw := range config.Gateways {
		sources = append(sources, gw.String())
	}
	return haproxy.Settings{
		Version: f.cfgVersion, GWName: f.name,
		Dir: f.dir, AdminSocket: f.adminSock, SocketMode: "660",
		User: f.user, Group: f.user, SocketGroup: f.user, // the agent (group haproxy) uses the socket
		HTTPBind: f.httpBind, StatsBind: f.statsBind, CanaryAddr: f.canary, TrafficLog: f.trafficLog,
		InternalSources: sources, Slots: f.slots, RateLimit: f.rateLimit,
	}
}

func run(ctx context.Context, log *slog.Logger, f flags) error {
	s := settings(f)
	if err := s.Validate(); err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	argv := strings.Fields(f.haproxyCmd)
	app := applier.New(applier.Config{
		Runtime:   haproxy.Runtime{Socket: haproxy.Socket{Path: f.adminSock}},
		Master:    haproxy.Master{Socket: haproxy.Socket{Path: f.masterSock, Timeout: 10 * time.Second}},
		Checker:   haproxy.Checker{Argv: argv},
		HealthURL: "http://" + healthAddr(f.httpBind) + "/__gw/healthz",
		Log:       log,
	}, s)
	if err := app.Bootstrap(ctx); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if f.bootstrap && f.vrrpIface != "" { // root only: the agent itself runs unprivileged
		ka, err := keepalived.ForGateway(f.name, f.vrrpIface)
		if err != nil {
			return fmt.Errorf("keepalived: %w", err)
		}
		ka.ScriptUser, ka.TrackProcess = f.kaScriptUser, f.kaTrackProcess
		if err := keepalived.Install(ka, f.kaConf); err != nil {
			return err
		}
		log.Info("keepalived files written", "conf", f.kaConf, "priority", ka.Priority)
	}
	if f.bootstrap {
		log.Info("haproxy files written", "dir", f.dir)
		return nil
	}
	token := os.Getenv("CSTAM_GATEWAY_TOKEN")
	var traffic *logs.Collector
	if f.trafficLog != "" {
		traffic = logs.New(0)
	}
	ag, err := agent.New(agent.Config{Name: f.name, CPD: strings.TrimRight(f.cpd, "/"), Token: token,
		RunDir: f.runDir, StateFile: f.stateFile, Log: log, Traffic: traffic}, app)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(f.runDir, 0o755); err != nil { //nolint:gosec // keepalived scripts read it
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.stateFile), 0o750); err != nil {
		return err
	}

	canary := &http.Server{Addr: f.canary, Handler: agent.Canary(), ReadHeaderTimeout: 2 * time.Second}
	errc := make(chan error, 1)
	go func() {
		if err := canary.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("canary: %w", err)
		}
	}()
	if traffic != nil {
		go func() {
			// best effort: without it the gateway still serves, only the dashboard misses lines
			if err := traffic.Listen(ctx, f.trafficLog); err != nil {
				log.Error("traffic log listener stopped", "err", err.Error())
			}
		}()
	}
	go func() { errc <- ag.Run(ctx) }()
	log.Info("gwagent started", "cpd", f.cpd)

	select {
	case err = <-errc:
	case <-ctx.Done():
		err = <-errc // the agent returns once ctx is done
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_ = canary.Shutdown(sctx)
	return err
}

// healthAddr turns the HAProxy bind into the address the applier probes locally.
func healthAddr(bind string) string {
	host, port, err := net.SplitHostPort(bind)
	if err != nil || host == "" || host == "0.0.0.0" {
		return "127.0.0.1:" + port
	}
	return net.JoinHostPort(host, port)
}
