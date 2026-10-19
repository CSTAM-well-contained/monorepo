// Package testutil starts what integration tests need: a PostgreSQL server in Docker.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// PostgresImage is the server used by the tests (the schema is verified on 18).
const PostgresImage = "postgres:18-alpine"

// EnvPostgresDSN points the tests at an existing server instead of Docker. Its user must be
// allowed to CREATE DATABASE, e.g. postgres://postgres:pw@127.0.0.1:5432/postgres.
const EnvPostgresDSN = "CSTAM_TEST_PG_DSN"

var (
	pgOnce      sync.Once
	pgAdminDSN  string
	pgContainer string
	pgErr       error
)

// Main runs the tests of a package and removes the Postgres container afterwards.
// Use it from TestMain: func TestMain(m *testing.M) { testutil.Main(m) }.
func Main(m *testing.M) {
	code := m.Run()
	if pgContainer != "" {
		// Best effort: the container was started with --rm, stopping it removes it.
		_ = exec.Command("docker", "stop", "-t", "1", pgContainer).Run() //nolint:gosec // id printed by docker run
	}
	os.Exit(code)
}

// Postgres returns the DSN of a new empty database, dropped when the test ends.
func Postgres(t *testing.T) string {
	t.Helper()
	pgOnce.Do(func() { pgAdminDSN, pgErr = startPostgres() })
	if pgErr != nil {
		t.Skipf("postgres not available: %v", pgErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, pgAdminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	name := "t_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if c, err := pgx.Connect(ctx, pgAdminDSN); err == nil {
			_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(ctx)
		}
	})
	u, err := url.Parse(pgAdminDSN)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

func startPostgres() (string, error) {
	if dsn := os.Getenv(EnvPostgresDSN); dsn != "" {
		return dsn, nil
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return "", fmt.Errorf("docker not installed and %s not set", EnvPostgresDSN)
	}
	out, err := exec.Command("docker", "run", "-d", "--rm",
		"-e", "POSTGRES_PASSWORD=cstam", "-p", "127.0.0.1::5432", PostgresImage).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker run %s: %w: %s", PostgresImage, err, out)
	}
	pgContainer = strings.TrimSpace(string(out))
	out, err = exec.Command("docker", "port", pgContainer, "5432/tcp").Output() //nolint:gosec // id printed by docker run
	if err != nil {
		return "", fmt.Errorf("docker port: %w", err)
	}
	// "127.0.0.1:49153" (one line per address family)
	hostPort := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	dsn := "postgres://postgres:cstam@" + hostPort + "/postgres?sslmode=disable"
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		c, err := pgx.Connect(ctx, dsn)
		if err == nil {
			_ = c.Close(ctx)
			cancel()
			return dsn, nil
		}
		cancel()
		if time.Now().After(deadline) {
			return "", fmt.Errorf("postgres not ready after 60 s: %w", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
