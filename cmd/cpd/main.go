// Command cpd is the CSTAM control-plane daemon: REST API, IPAM, controller and reconciler.
//
// Configuration comes from the environment (secrets never go on the command line):
//
//	CSTAM_DB_DSN         PostgreSQL DSN (required)
//	CSTAM_ADMIN_TOKEN    bearer token of the admin API (required, >= 16 chars)
//	CSTAM_GATEWAY_TOKEN  bearer token of the gateway agents (required, >= 16 chars)
//	CSTAM_DRIVER         openstack (default), docker (containers, local demo) or fake (in-memory)
//	CSTAM_DOCKER_NETWORK Docker network of the docker driver (default cstam-demo)
//	OS_CLOUD             clouds.yaml entry, for the openstack driver
//	CSTAM_NETWORK_ID, CSTAM_SUBNET_ID, CSTAM_TEAM_SG_ID
//	                     from `pulumi stack output` (networkId, subnetId, teamSecGroupId)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/CSTAM-well-contained/monorepo/internal/api"
	"github.com/CSTAM-well-contained/monorepo/internal/config"
	"github.com/CSTAM-well-contained/monorepo/internal/docker"
	"github.com/CSTAM-well-contained/monorepo/internal/ipam"
	"github.com/CSTAM-well-contained/monorepo/internal/lifecycle"
	"github.com/CSTAM-well-contained/monorepo/internal/openstack"
	"github.com/CSTAM-well-contained/monorepo/internal/openstack/fake"
	"github.com/CSTAM-well-contained/monorepo/internal/store"
	"github.com/CSTAM-well-contained/monorepo/web"
)

var version = "dev" // set by -ldflags "-X main.version=..."

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	listen := flag.String("listen", ":"+strconv.Itoa(config.CPDPort), "address of the REST API")
	workers := flag.Int("workers", 4, "sandbox operations in flight")
	every := flag.Duration("reconcile", 2*time.Second, "reconciler interval")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("app", "cpd", "version", version)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log, *listen, *workers, *every); err != nil {
		log.Error("cpd stopped", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, listen string, workers int, every time.Duration) error {
	dsn := os.Getenv("CSTAM_DB_DSN")
	if dsn == "" {
		return errors.New("CSTAM_DB_DSN is not set")
	}
	pool, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool); err != nil {
		return err
	}
	m := ipam.New()
	if err := m.Seed(ctx, pool); err != nil {
		return err
	}
	sbx, err := driver(ctx, log)
	if err != nil {
		return err
	}
	ctl := lifecycle.NewController(ctx, pool, m, sbx, log, workers)
	srv := &api.Server{C: ctl, AdminToken: os.Getenv("CSTAM_ADMIN_TOKEN"),
		GatewayToken: os.Getenv("CSTAM_GATEWAY_TOKEN"), ViewerToken: os.Getenv("CSTAM_VIEWER_TOKEN"),
		Log: log, UI: web.Handler()}
	if err := srv.Validate(); err != nil {
		return err
	}
	hs := &http.Server{
		Addr:              listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      api.MaxWait + 15*time.Second, // long-poll plus margin
		IdleTimeout:       90 * time.Second,
	}
	go ctl.Run(ctx, every)
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("cpd listening", "addr", listen)

	select {
	case err := <-errc:
		return fmt.Errorf("http: %w", err)
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	err = hs.Shutdown(sctx)
	ctl.Wait() // workers stop with ctx; teams left mid-way are resumed by the next start
	return err
}

func driver(ctx context.Context, log *slog.Logger) (lifecycle.Sandboxes, error) {
	switch d := os.Getenv("CSTAM_DRIVER"); d {
	case "fake":
		log.Warn("using the in-memory fake sandbox driver: nothing is created in the cloud")
		return fake.New(), nil
	case "docker":
		network := os.Getenv("CSTAM_DOCKER_NETWORK")
		if network == "" {
			network = "cstam-demo"
		}
		log.Info("using the docker sandbox driver: one container per team", "network", network)
		return docker.New(docker.Config{Network: network})
	case "", "openstack":
		return openstack.New(ctx, openstack.Config{
			NetworkID:      os.Getenv("CSTAM_NETWORK_ID"),
			SubnetID:       os.Getenv("CSTAM_SUBNET_ID"),
			TeamSecGroupID: os.Getenv("CSTAM_TEAM_SG_ID"),
		})
	default:
		return nil, fmt.Errorf("CSTAM_DRIVER %q: want openstack, docker or fake", d)
	}
}
