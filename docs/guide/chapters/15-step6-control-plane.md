# 15. Step 6 – Control plane skeleton, database and IPAM

## Step 6 – Control plane skeleton, database and IPAM

::: note
**Goal:** the `cpd` binary starts on the control-plane VM `cstam-cp` (10.20.0.5), connects to PostgreSQL 16, runs its migrations, and exposes `/healthz`, `/readyz`, `GET /v1/hackathons` and `GET /v1/pools/{id}`. The IP address manager (IPAM) can lease, activate, release, quarantine and free addresses in the team pool 10.20.0.32–10.20.0.191, with 100 parallel leases and never the same IP twice. All of this is proven by tests that run on your laptop.
**Points served:** IPAM & reclamation (10), stress test (15, the correctness half), teardown & lifecycle security (10, the data half).
**Time estimate:** 2–3 days part-time.
**Depends on:** Step 0 (repo, Go module, CI). This step does **not** need the cloud: you can start it on day 3, in parallel with the network experiments (track C in Figure 5.1).
:::

This step builds the "brain" of the system: the place where the truth is written down. Everything later (provisioning, teardown, the dashboard) reads and writes these tables. If the tables are right, most bugs become impossible instead of merely unlikely.

### Understand first

#### What the control plane is, and what it is not

The control plane (`cpd`, "control-plane daemon") is **one Go binary**. Inside it there are two *roles*:

| Role | What runs | How many |
|---|---|---|
| `api` | Public REST API `/v1`, SSE stream, embedded dashboard, and the agent-facing Connect-RPC listener (agents dial in) | 1..N, stateless |
| `controller` | Workers that drive teams to their desired state, the IP sweeper, the expiry sweeper, the reconciler | workers on every instance; sweepers and reconciler only on the leader |

By default one process runs both roles (`CSTAM_ROLES=api,controller`). IPAM is **not** a separate service. It is a Go package (`internal/ipam`) whose functions run *inside* the same database transaction as the team change. That is the whole reason we chose a modular monolith (Chapter 6): "this IP belongs to this team" must be one `BEGIN … COMMIT`, not a conversation between two services.

The control plane is **not on the data path**. Visitor traffic goes FIP → VIP → HAProxy → team IP. If `cpd` or PostgreSQL stops, every existing team keeps working, gateway failover keeps working, and local rollback on the gateways keeps working. Only new registrations, teardowns and the dashboard pause.

#### Desired state, observed phase, health

Every team row has three separate columns. Mixing them is the classic source of bugs such as "an unhealthy team got deleted":

| Column | Values | Who writes it | Meaning |
|---|---|---|---|
| `desired` | `PRESENT`, `ABSENT` | API, expiry sweeper | What the user (or the TTL) asked for |
| `phase` | `REQUESTED`, `PROVISIONING`, `ACTIVE`, `EXPIRING`, `DELETING`, `DELETED`, `FAILED` | controller only | How far the system has got |
| `health` | `UNKNOWN`, `HEALTHY`, `DEGRADED`, `DOWN` | health checker, reconciler | Is the app answering? Never triggers deletion |

#### The IP pool: one row per address

The IP address manager (IPAM) keeps **one row per usable IP** in the table `ip_addresses`. That row *is* the lock. To lease an IP, a transaction locks one free row with `SELECT … FOR UPDATE SKIP LOCKED`:

- `FOR UPDATE` locks the row until the transaction commits.
- `SKIP LOCKED` tells PostgreSQL: if another transaction already locked a row, do not wait for it, skip it and take the next one.

So 50 concurrent registrations each grab a *different* free row at the same time, instead of all queuing on "the first free row" and failing one after another. Plain `FOR UPDATE` would serialise them. `SERIALIZABLE` isolation without locks would abort most of them with error `40001`.

Neutron is a second, independent lock: we always ask Neutron for a *specific* IP (`--fixed-ip ip-address=10.20.0.57`). If somebody else holds it, Neutron answers `409 IpAddressAlreadyAllocated`. Our DB picks *which* IP; Neutron confirms nobody else has it.

![Figure 15.1 – Lifecycle of one IP address in the team pool. QUARANTINE → FREE is the only way back, and it is guarded by one SQL statement.](diagrams/f10-ip-states.png)

#### IP states

| State | Meaning | Has a lease? |
|---|---|---|
| `FREE` | Can be leased | no |
| `WARM` | Pod-NIC mode only: a Neutron port with this IP already exists and is already hot-plugged on a host, waiting for a team (warm pool) | no (owned by a host) |
| `LEASED` | Taken by a team that is being provisioned (lease `PENDING`) | yes |
| `ACTIVE` | The team is live and routed | yes |
| `RELEASING` | Teardown started; route/host/port cleanup in progress | yes |
| `QUARANTINE` | Everything is cleaned up; waiting for time **and** gateway acknowledgements | no |
| `RESERVED` | Never lease (infrastructure or the foreign VM at .68) | no |
| `FOREIGN` | Neutron says someone else holds it; never lease until it is unused for 2 scans | no |

Why `WARM` exists: in the pod-NIC model (Chapter 14) attaching a NIC to a VM takes 2–6 s and Nova does it one at a time per VM. Ten simultaneous registrations would wait behind each other. So when a hackathon is created we pre-create and pre-attach a few ports per host. Their IPs are in state `WARM`. A registration then *takes* a warm IP (milliseconds) instead of attaching a new NIC.

#### Two timers, one clock

All times come from PostgreSQL `now()`, never from Go or from agents. Then two control-plane replicas or a skewed agent clock can never disagree about expiry.

| Lease state | `expires_at` means | Default |
|---|---|---|
| `PENDING` | provisioning deadline | `now() + 120 s` |
| `ACTIVE` | sandbox time-to-live (TTL) | `least(now() + hackathon.default_ttl, hackathon.ends_at)` |

Go uses `time.Since` (monotonic clock) only for timeouts of single calls.

### Do it by hand

#### 6.1 Install PostgreSQL 16 on the control-plane VM

On `cstam-cp` (Ubuntu 24.04 ships PostgreSQL 16):

```bash
sudo apt-get update
sudo apt-get install -y postgresql-16 postgresql-client-16
sudo systemctl enable --now postgresql

# OS user that runs cpd, and a DB role with the same name (peer auth over the unix socket)
sudo useradd -r -m -d /var/lib/cstam -s /usr/sbin/nologin cstam
sudo -u postgres psql <<'SQL'
CREATE ROLE cstam LOGIN;                       -- owner, runs migrations
CREATE ROLE cstam_app NOLOGIN;                 -- privileges used at runtime
GRANT cstam_app TO cstam;
CREATE DATABASE cstam OWNER cstam;
SQL
```

Restrict access to the unix socket only, with *peer* authentication (the OS user name must equal the DB role, so there is **no password to leak**):

```bash
PGHBA=/etc/postgresql/16/main/pg_hba.conf
sudo cp $PGHBA $PGHBA.orig
sudo tee $PGHBA >/dev/null <<'EOF'
# TYPE  DATABASE  USER      METHOD
local   all       postgres  peer
local   cstam     cstam     peer
# no "host" lines: PostgreSQL is not reachable over TCP at all
EOF
sudo sed -i "s/^#\?listen_addresses.*/listen_addresses = ''/" /etc/postgresql/16/main/postgresql.conf
sudo systemctl restart postgresql
```

::: done
**Done when**
```bash
sudo -u cstam psql -d cstam -c 'select current_user, version();'   # cstam | PostgreSQL 16.x
ss -ltnp | grep 5432                                                # prints nothing: no TCP listener
ls -l /var/run/postgresql/.s.PGSQL.5432                             # the unix socket exists
```
:::

The connection string for `cpd` is then `postgres:///cstam?host=/var/run/postgresql&pool_max_conns=30` (empty host and user: the socket directory and the OS user are used).

#### 6.2 Local development database

On your laptop, run the same version in Docker for development and tests (tests use testcontainers, which starts its own container automatically):

```bash
docker run -d --name cstam-pg -e POSTGRES_USER=cstam -e POSTGRES_PASSWORD=dev -e POSTGRES_DB=cstam \
  -p 127.0.0.1:55432:5432 postgres:16-alpine
export CSTAM_DATABASE_URL='postgres://cstam:dev@127.0.0.1:55432/cstam?sslmode=disable'
```

### Build it in Go

#### 6.3 Package layout for this step

```text
cmd/cpd/main.go                     # wiring, roles, "migrate" subcommand
internal/config/cpd.go              # env struct
internal/store/                     # pgx pool, InTx helper, sqlc-generated code
internal/store/queries/*.sql        # sqlc input: ipam.sql, teams.sql, hackathons.sql
internal/ipam/                      # IPAM: Lease, Activate, BeginRelease, Quarantine, Warm*, sweeper
internal/lifecycle/fsm.go           # team + IP transition tables
internal/leader/leader.go           # advisory-lock leader election
internal/api/                       # REST handlers (generated strict server), middleware
api/openapi/cstam.v1.yaml           # contract (source of truth)
migrations/00001_init.sql           # schema
migrations/00002_seed_pool.sql      # pool rows
internal/testutil/                  # testcontainers Postgres, invariants
```

#### 6.4 Configuration and `main.go`

All configuration comes from environment variables (12-factor style). The env names *are* the documentation. Use `github.com/caarlos0/env/v11`.

```go
// internal/config/cpd.go
package config

import (
	"log/slog"
	"time"
)

type CPD struct {
	Roles              []string      `env:"CSTAM_ROLES" envDefault:"api,controller"`
	DatabaseURL        string        `env:"CSTAM_DATABASE_URL,required"`
	PublicAddr         string        `env:"CSTAM_PUBLIC_ADDR" envDefault:":443"`      // admin API + dashboard (TLS)
	AgentAddr          string        `env:"CSTAM_AGENT_ADDR" envDefault:":8443"`      // agents dial in here (mTLS)
	MetricsAddr        string        `env:"CSTAM_METRICS_ADDR" envDefault:"127.0.0.1:9090"`
	Env                string        `env:"CSTAM_ENV" envDefault:"dev"`               // goes into Neutron tag cstam-env:<env>
	OSCloud            string        `env:"OS_CLOUD" envDefault:"cstam"`
	NetworkID          string        `env:"CSTAM_NETWORK_ID"`
	SubnetID           string        `env:"CSTAM_SUBNET_ID"`
	BaseDomain         string        `env:"CSTAM_BASE_DOMAIN" envDefault:"cstam.felcloud.tn"`
	NeutronRPS         float64       `env:"CSTAM_NEUTRON_RPS" envDefault:"10"`
	NeutronMaxInFlight int           `env:"CSTAM_NEUTRON_MAX_INFLIGHT" envDefault:"8"`
	Workers            int           `env:"CSTAM_WORKERS" envDefault:"16"`
	PendingTTL         time.Duration `env:"CSTAM_PENDING_TTL" envDefault:"120s"`
	QuarantineTTL      time.Duration `env:"CSTAM_QUARANTINE" envDefault:"60s"`
	TLSCert            string        `env:"CSTAM_TLS_CERT" envDefault:"/etc/cstam/pki/cpd.crt"`
	TLSKey             string        `env:"CSTAM_TLS_KEY" envDefault:"/etc/cstam/pki/cpd.key"`
	TLSCA              string        `env:"CSTAM_TLS_CA" envDefault:"/etc/cstam/pki/ca.crt"`
	LogLevel           slog.Level    `env:"CSTAM_LOG_LEVEL" envDefault:"INFO"`
}

func (c CPD) HasRole(r string) bool {
	for _, x := range c.Roles {
		if x == r || x == "all" {
			return true
		}
	}
	return false
}
```

```go
// cmd/cpd/main.go
package main

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	var cfg config.CPD
	if err := env.Parse(&cfg); err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if len(args) > 0 && args[0] == "migrate" { // cpd migrate
		return store.MigrateUp(ctx, pool) // goose, migrations embedded with embed.FS
	}

	st := store.New(pool)
	ipm := &ipam.IPAM{Pool: pool, PendingTTL: cfg.PendingTTL, QuarantineTTL: cfg.QuarantineTTL}

	g, ctx := errgroup.WithContext(ctx)
	if cfg.HasRole("api") {
		srv := api.NewServer(api.Deps{Store: st, IPAM: ipm, Log: log})
		g.Go(func() error { return platform.ServeHTTPS(ctx, cfg.PublicAddr, srv.Handler(), cfg.TLSCert, cfg.TLSKey) })
		// Step 7 adds: the agent listener on cfg.AgentAddr (Connect-RPC, mTLS)
	}
	if cfg.HasRole("controller") {
		// Step 7 adds: controller workers. Step 8 adds: sweepers + reconciler under leader.Run.
		g.Go(func() error {
			leader.Run(ctx, cfg.DatabaseURL, log, func(lctx context.Context) {
				ipam.RunSweeper(lctx, ipm, log) // QUARANTINE -> FREE, every 2 s
			})
			return nil
		})
	}
	g.Go(func() error { return platform.ServeMetrics(ctx, cfg.MetricsAddr, pool) }) // /metrics /healthz /readyz
	return g.Wait()
}
```

`/healthz` answers 200 if the process is alive. `/readyz` answers 200 only if `SELECT 1` works on the pool. Bind metrics to 127.0.0.1: they are scraped locally or through an SSH tunnel.

#### 6.5 The schema (`migrations/00001_init.sql`)

This is the complete schema for Steps 6–9. Read it slowly: most rules of the system are written here as constraints, so a bug in Go cannot break them.

```sql
-- +goose Up
CREATE EXTENSION IF NOT EXISTS btree_gist;   -- EXCLUDE constraints
CREATE EXTENSION IF NOT EXISTS pgcrypto;     -- gen_random_uuid(), digest()

CREATE TYPE inetrange AS RANGE (subtype = inet);

CREATE TYPE hackathon_status AS ENUM ('DRAFT','OPEN','CLOSED','ARCHIVED');
CREATE TYPE host_state       AS ENUM ('READY','DRAINING','DOWN','RETIRED');
CREATE TYPE net_mode         AS ENUM ('PODNIC','AAP');
CREATE TYPE aap_mode         AS ENUM ('EXACT','CIDR');
CREATE TYPE ip_state         AS ENUM ('FREE','WARM','LEASED','ACTIVE','RELEASING','QUARANTINE','RESERVED','FOREIGN');
CREATE TYPE lease_state      AS ENUM ('PENDING','ACTIVE','RELEASING','RELEASED');
CREATE TYPE desired_state    AS ENUM ('PRESENT','ABSENT');
CREATE TYPE team_phase       AS ENUM ('REQUESTED','PROVISIONING','ACTIVE','EXPIRING','DELETING','DELETED','FAILED');
CREATE TYPE team_health      AS ENUM ('UNKNOWN','HEALTHY','DEGRADED','DOWN');
CREATE TYPE vrrp_state       AS ENUM ('MASTER','BACKUP','FAULT','UNKNOWN');
CREATE TYPE op_state         AS ENUM ('RUNNING','SUCCEEDED','FAILED','ABANDONED');

-- Slugs end up in HAProxy maps, container names, firewall comments: whitelist them in the DB.
CREATE DOMAIN slug AS text CHECK (VALUE ~ '^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$');

CREATE TABLE hackathons (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  slug          slug NOT NULL UNIQUE,
  name          text NOT NULL,
  status        hackathon_status NOT NULL DEFAULT 'DRAFT',
  starts_at     timestamptz NOT NULL,
  ends_at       timestamptz NOT NULL,
  max_teams     int  NOT NULL CHECK (max_teams BETWEEN 1 AND 160),
  default_ttl   interval NOT NULL DEFAULT '4 hours',
  warm_per_host int  NOT NULL DEFAULT 6 CHECK (warm_per_host BETWEEN 0 AND 20),
  created_at    timestamptz NOT NULL DEFAULT now(),
  CHECK (ends_at > starts_at)
);

CREATE TABLE hosts (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id       uuid NOT NULL REFERENCES hackathons(id),
  name               slug NOT NULL UNIQUE,                 -- 'cstam-host-a'
  nova_server_id     uuid NOT NULL UNIQUE,
  neutron_port_id    uuid NOT NULL UNIQUE,                 -- its primary port (10.20.0.21)
  mgmt_ip            inet NOT NULL UNIQUE,
  state              host_state NOT NULL DEFAULT 'READY',
  net_mode           net_mode NOT NULL DEFAULT 'PODNIC',   -- decided at gate G1
  aap_mode           aap_mode,                             -- only for net_mode = AAP
  aap_cidr           cidr,                                 -- only for aap_mode = CIDR
  capacity           int  NOT NULL CHECK (capacity BETWEEN 1 AND 64),
  agent_last_seen_at timestamptz,
  desired_rev        bigint NOT NULL DEFAULT 0,            -- bumped when a team is added/removed on this host
  applied_rev        bigint NOT NULL DEFAULT 0,            -- what the host agent acknowledged
  CHECK ((net_mode = 'AAP') = (aap_mode IS NOT NULL)),
  CHECK ((aap_mode = 'CIDR') = (aap_cidr IS NOT NULL))
);

CREATE TABLE ip_pools (
  id        serial PRIMARY KEY,
  name      text NOT NULL UNIQUE,
  range     inetrange NOT NULL,                  -- '[10.20.0.32,10.20.0.191]'
  host_id   uuid REFERENCES hosts(id),           -- set only in AAP CIDR mode (one block per host)
  EXCLUDE USING gist (range WITH &&)             -- two pools can never overlap
);

CREATE TABLE ip_addresses (
  ip                       inet PRIMARY KEY CHECK (family(ip) = 4 AND masklen(ip) = 32),
  pool_id                  int  NOT NULL REFERENCES ip_pools(id),
  state                    ip_state NOT NULL DEFAULT 'FREE',
  current_lease_id         uuid UNIQUE,
  warm_host_id             uuid REFERENCES hosts(id),     -- WARM: host the NIC is attached to
  warm_port_id             uuid UNIQUE,                   -- WARM: the pre-created Neutron port
  state_changed_at         timestamptz NOT NULL DEFAULT now(),
  last_released_at         timestamptz,                   -- LRU ordering
  quarantine_until         timestamptz,
  required_ack_generation  bigint,                        -- every member gateway must ack >= this
  neutron_absent_at        timestamptz,                   -- a fresh Neutron GET found no port with this IP
  foreign_clean_scans      int NOT NULL DEFAULT 0,        -- FOREIGN -> FREE after 2
  note                     text,
  version                  bigint NOT NULL DEFAULT 0,
  CHECK ((state IN ('LEASED','ACTIVE','RELEASING')) = (current_lease_id IS NOT NULL)),
  CHECK ((state = 'QUARANTINE') = (quarantine_until IS NOT NULL AND required_ack_generation IS NOT NULL)),
  CHECK ((state = 'WARM') = (warm_host_id IS NOT NULL)),                -- WARM with NULL port = "warming"
  CHECK (warm_port_id IS NULL OR state = 'WARM')
);
CREATE INDEX ip_free_lru   ON ip_addresses (pool_id, last_released_at NULLS FIRST, ip) WHERE state = 'FREE';
CREATE INDEX ip_warm       ON ip_addresses (warm_host_id) WHERE state = 'WARM';
CREATE INDEX ip_quarantine ON ip_addresses (quarantine_until) WHERE state = 'QUARANTINE';

CREATE TABLE teams (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id     uuid NOT NULL REFERENCES hackathons(id),
  slug             slug NOT NULL,                        -- immutable
  fqdn             text NOT NULL,                        -- team1.hacka.cstam.felcloud.tn
  display_name     text NOT NULL,
  host_id          uuid REFERENCES hosts(id),
  desired          desired_state NOT NULL DEFAULT 'PRESENT',
  phase            team_phase NOT NULL DEFAULT 'REQUESTED',
  health           team_health NOT NULL DEFAULT 'UNKNOWN',
  spec             jsonb NOT NULL DEFAULT '{}',          -- apps: [{name, image, port, cpu, mem_mib}]
  spec_version     bigint NOT NULL DEFAULT 1,
  expires_at       timestamptz,
  max_expires_at   timestamptz NOT NULL,                 -- = hackathon.ends_at
  claimed_by       text,                                 -- work claim (fencing), Step 7
  claim_epoch      bigint NOT NULL DEFAULT 0,
  claim_expires_at timestamptz,
  next_attempt_at  timestamptz NOT NULL DEFAULT now(),
  attempts         int NOT NULL DEFAULT 0,
  last_error       text,
  failure_reason   text,
  created_at       timestamptz NOT NULL DEFAULT now(),
  deleted_at       timestamptz,
  CHECK (fqdn = lower(fqdn) AND fqdn ~ '^[a-z0-9.-]+$'),
  CHECK (phase <> 'ACTIVE' OR (host_id IS NOT NULL AND expires_at IS NOT NULL)),
  CHECK ((phase IN ('DELETED','FAILED')) = (deleted_at IS NOT NULL))
);
CREATE UNIQUE INDEX team_slug_live ON teams (hackathon_id, slug) WHERE phase NOT IN ('DELETED','FAILED');
CREATE UNIQUE INDEX team_fqdn_live ON teams (fqdn)               WHERE phase NOT IN ('DELETED','FAILED');
CREATE INDEX team_work ON teams (next_attempt_at) WHERE phase NOT IN ('DELETED','FAILED');

CREATE TABLE leases (
  id              uuid PRIMARY KEY,
  ip              inet NOT NULL REFERENCES ip_addresses(ip),
  team_id         uuid NOT NULL REFERENCES teams(id),
  state           lease_state NOT NULL DEFAULT 'PENDING',
  neutron_port_id uuid UNIQUE,
  created_at      timestamptz NOT NULL DEFAULT now(),
  activated_at    timestamptz,
  expires_at      timestamptz NOT NULL,
  released_at     timestamptz,
  CHECK ((state = 'RELEASED') = (released_at IS NOT NULL))
);
-- THE core invariants: one live lease per IP, one live lease per team.
CREATE UNIQUE INDEX lease_live_ip   ON leases (ip)      WHERE state <> 'RELEASED';
CREATE UNIQUE INDEX lease_live_team ON leases (team_id) WHERE state <> 'RELEASED';
CREATE INDEX lease_expiry ON leases (expires_at) WHERE state IN ('PENDING','ACTIVE');

ALTER TABLE ip_addresses ADD CONSTRAINT ip_current_lease_fk
  FOREIGN KEY (current_lease_id) REFERENCES leases(id) DEFERRABLE INITIALLY DEFERRED;

-- Single-row generation counter. Its row lock serialises generation bumps in COMMIT order (Step 8).
CREATE TABLE config_head (
  id                 boolean PRIMARY KEY DEFAULT true CHECK (id),
  generation         bigint NOT NULL DEFAULT 0,
  structural_version int    NOT NULL DEFAULT 1
);
INSERT INTO config_head DEFAULT VALUES;

CREATE TABLE config_versions (
  generation         bigint PRIMARY KEY,
  kind               text NOT NULL CHECK (kind IN ('ROUTES','STRUCTURAL','ROLLBACK')),
  structural_version int NOT NULL,
  map_digest         bytea NOT NULL,       -- sha256 of the sorted route snapshot at this generation
  reason             text NOT NULL,
  created_by         text NOT NULL,
  created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE haproxy_templates (           -- structural configs (manual rollback target)
  version     int PRIMARY KEY,
  body        text NOT NULL,
  sha256      bytea NOT NULL,
  status      text NOT NULL DEFAULT 'proposed' CHECK (status IN ('proposed','active','rejected','rolled_back','superseded')),
  created_by  text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE routes (
  id                 bigserial PRIMARY KEY,
  team_id            uuid NOT NULL REFERENCES teams(id),
  lease_id           uuid NOT NULL REFERENCES leases(id),
  fqdn               text NOT NULL,
  target_ip          inet NOT NULL,
  target_port        int  NOT NULL DEFAULT 80 CHECK (target_port = 80 OR target_port BETWEEN 8000 AND 8099),
  slot               int  NOT NULL CHECK (slot BETWEEN 1 AND 128),   -- be_slotNNN on the gateways
  added_generation   bigint NOT NULL,
  removed_generation bigint,
  CHECK (removed_generation IS NULL OR removed_generation > added_generation)
);
CREATE UNIQUE INDEX route_live_fqdn ON routes (fqdn) WHERE removed_generation IS NULL;
CREATE UNIQUE INDEX route_live_slot ON routes (slot) WHERE removed_generation IS NULL;
CREATE INDEX route_gen ON routes (added_generation, removed_generation);

CREATE TABLE gateways (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name              slug NOT NULL UNIQUE,                -- 'gw1'
  neutron_port_id   uuid NOT NULL UNIQUE,
  mgmt_ip           inet NOT NULL UNIQUE,
  member            boolean NOT NULL DEFAULT true,       -- part of the ack quorum (ALL members must ack)
  fenced_at         timestamptz,
  acked_generation  bigint NOT NULL DEFAULT 0,
  acked_digest      bytea,
  acked_at          timestamptz,
  vrrp              vrrp_state NOT NULL DEFAULT 'UNKNOWN',
  haproxy_ok        boolean NOT NULL DEFAULT false,
  synced            boolean NOT NULL DEFAULT false,
  force_backup      boolean NOT NULL DEFAULT false,      -- manual failover flag, part of desired state
  last_seen_at      timestamptz,
  CHECK (member OR fenced_at IS NOT NULL)
);

CREATE TABLE operations (
  id          bigserial PRIMARY KEY,
  team_id     uuid REFERENCES teams(id),
  kind        text NOT NULL,                 -- PROVISION / TEARDOWN / REPAIR / GC / FAILOVER / ROLLBACK
  step        text NOT NULL,
  claim_epoch bigint NOT NULL DEFAULT 0,
  state       op_state NOT NULL DEFAULT 'RUNNING',
  started_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
  finished_at timestamptz,
  duration_ms int GENERATED ALWAYS AS ((extract(epoch FROM finished_at - started_at) * 1000)::int) STORED,
  error       text,
  detail      jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX op_team ON operations (team_id, started_at DESC);

CREATE TABLE events (                        -- state transitions; feeds SSE (id = Last-Event-ID)
  id          bigserial PRIMARY KEY,
  at          timestamptz NOT NULL DEFAULT clock_timestamp(),
  actor       text NOT NULL,
  entity_type text NOT NULL,
  entity_id   text NOT NULL,
  type        text NOT NULL,
  from_state  text,
  to_state    text,
  data        jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX events_entity ON events (entity_type, entity_id, id);

CREATE TABLE audit_log (                     -- who did what (Step 9 hash chain)
  id          bigserial PRIMARY KEY,
  at          timestamptz NOT NULL DEFAULT now(),
  actor       text NOT NULL,                 -- 'admin:alice', 'viewer:jury1', 'system:reconciler', 'token:bench'
  actor_ip    inet,
  action      text NOT NULL,                 -- 'team.create', 'gateway.failover', 'config.rollback', 'auth.login'
  target      text,
  request_id  uuid NOT NULL,
  outcome     text NOT NULL CHECK (outcome IN ('ok','denied','error')),
  details     jsonb NOT NULL DEFAULT '{}',
  prev_hash   bytea,
  hash        bytea NOT NULL
);
CREATE INDEX audit_at ON audit_log (at DESC);

CREATE TABLE idempotency_keys (
  principal   text        NOT NULL,
  key         text        NOT NULL,
  method      text        NOT NULL,
  path        text        NOT NULL,
  request_hash bytea      NOT NULL,          -- same key + different body => 422
  status_code int,
  response    jsonb,
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (principal, key)
);

CREATE TABLE admins (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  username      text NOT NULL UNIQUE,
  role          text NOT NULL CHECK (role IN ('admin','viewer')),
  password_hash text NOT NULL,               -- $argon2id$...
  must_change   boolean NOT NULL DEFAULT true,
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE sessions (
  token_sha256  bytea PRIMARY KEY,
  admin_id      uuid NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_seen_at  timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL
);
CREATE TABLE api_tokens (
  token_sha256  bytea PRIMARY KEY,
  name          text NOT NULL,
  role          text NOT NULL CHECK (role IN ('admin','viewer','bench')),
  expires_at    timestamptz NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now()
);

-- Runtime role: no UPDATE/DELETE on history tables.
GRANT USAGE ON SCHEMA public TO cstam_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO cstam_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO cstam_app;
REVOKE UPDATE, DELETE, TRUNCATE ON events, audit_log FROM cstam_app;

-- +goose Down
-- development only
DROP SCHEMA public CASCADE; CREATE SCHEMA public;
```

`cpd` runs `SET ROLE cstam_app` on every pooled connection (pgx `AfterConnect` hook) after migrations, so the runtime code cannot edit history even by mistake.

::: why
**What each constraint rules out**

| Illegal state | Prevented by |
|---|---|
| Two teams hold the same IP | `lease_live_ip` + `ip_addresses.current_lease_id UNIQUE` + Neutron fixed-IP uniqueness (third lock) |
| A team holds two IPs | `lease_live_team` |
| IP marked LEASED/ACTIVE without a lease, or FREE with one | `CHECK ((state IN ...) = (current_lease_id IS NOT NULL))` |
| IP in quarantine without a release time or required ack generation | `CHECK` on QUARANTINE |
| WARM IP without its host, or a warm port on a non-WARM IP | the two `CHECK`s on WARM |
| Two live teams with the same name or FQDN (race between two registrations) | `team_slug_live`, `team_fqdn_live` → SQLSTATE `23505` → HTTP 409 |
| Two live routes for one FQDN, or one gateway slot used twice | `route_live_fqdn`, `route_live_slot` |
| Overlapping pools (a typo makes the team pool cover the VIP) | `EXCLUDE USING gist (range WITH &&)` |
| A team name containing `;`, spaces or newlines (config injection into HAProxy) | `slug` domain regex |
| A gateway silently dropped from the ack quorum | `CHECK (member OR fenced_at IS NOT NULL)` |
| Rewriting history | `REVOKE UPDATE, DELETE` on `events`, `audit_log` |
:::

#### 6.6 Seed the pool (`migrations/00002_seed_pool.sql`)

```sql
-- +goose Up
INSERT INTO ip_pools (name, range) VALUES ('teams', '[10.20.0.32,10.20.0.191]');
INSERT INTO ip_addresses (ip, pool_id)
SELECT ('10.20.0.' || n)::inet, (SELECT id FROM ip_pools WHERE name = 'teams')
FROM generate_series(32, 191) AS n;
UPDATE ip_addresses SET state = 'RESERVED', note = 'sbx-f0699704 (existing VM, fixed IP)' WHERE ip = '10.20.0.68';

INSERT INTO ip_pools (name, range) VALUES ('lab', '[10.20.0.192,10.20.0.199]');
INSERT INTO ip_addresses (ip, pool_id)
SELECT ('10.20.0.' || n)::inet, (SELECT id FROM ip_pools WHERE name = 'lab')
FROM generate_series(192, 199) AS n;
-- +goose Down
DELETE FROM ip_addresses; DELETE FROM ip_pools;
```

Why these ranges work: Neutron auto-allocates **only** from the subnet's allocation pool (.200–.250, already configured on FelCloud). A port created with an explicit `ip-address` may use any address of the CIDR, and our project owns the network, so it may do that. Neutron's own allocations (new VMs, DHCP/metadata ports) can therefore never collide with our pool. **No subnet change is needed.**

::: done
**Done when**
```bash
go run ./cmd/cpd migrate
psql "$CSTAM_DATABASE_URL" -c "SELECT p.name, a.state, count(*) FROM ip_addresses a JOIN ip_pools p ON p.id=a.pool_id GROUP BY 1,2 ORDER BY 1,2;"
#  lab   | FREE     |   8
#  teams | FREE     | 159
#  teams | RESERVED |   1
openstack subnet show cstam-sandbox-subnet -c allocation_pools -f value   # still 10.20.0.200-10.20.0.250
```
:::

#### 6.7 sqlc configuration

`sqlc` reads your SQL files and generates typed Go functions. You write real SQL (with `SKIP LOCKED`, advisory locks, `pg_notify`), and you still get compile-time types.

```yaml
# sqlc.yaml
version: "2"
sql:
  - engine: postgresql
    schema: migrations
    queries: internal/store/queries
    gen:
      go:
        package: db
        out: internal/store/db
        sql_package: pgx/v5
        emit_pointers_for_null_types: true
        overrides:
          - db_type: inet
            go_type: net/netip.Addr
          - db_type: uuid
            go_type: github.com/google/uuid.UUID
```

Run `sqlc generate` (add it to `make gen`). Commit the generated code.

#### 6.8 The IPAM queries (`internal/store/queries/ipam.sql`)

```sql
-- name: LeaseIP :one
-- Takes one FREE IP (skipping rows locked by other transactions), least recently released first,
-- marks it LEASED and creates the PENDING lease. Returns no row when the pool is empty.
WITH cand AS (
  SELECT ip FROM ip_addresses
  WHERE pool_id = sqlc.arg(pool_id) AND state = 'FREE'
  ORDER BY last_released_at NULLS FIRST, ip
  LIMIT 1
  FOR UPDATE SKIP LOCKED
), upd AS (
  UPDATE ip_addresses a
  SET state = 'LEASED', current_lease_id = sqlc.arg(lease_id), state_changed_at = now(),
      neutron_absent_at = NULL, version = version + 1
  FROM cand WHERE a.ip = cand.ip
  RETURNING a.ip
)
INSERT INTO leases (id, ip, team_id, state, expires_at)
SELECT sqlc.arg(lease_id), upd.ip, sqlc.arg(team_id), 'PENDING',
       now() + sqlc.arg(pending_secs)::int * interval '1 second'
FROM upd
RETURNING ip;

-- name: TakeWarmIP :one
-- Pod-NIC: take a pre-attached port on the chosen host. Same SKIP LOCKED pattern.
WITH cand AS (
  SELECT ip, warm_port_id FROM ip_addresses
  WHERE state = 'WARM' AND warm_host_id = sqlc.arg(host_id) AND warm_port_id IS NOT NULL
  ORDER BY state_changed_at, ip
  LIMIT 1
  FOR UPDATE SKIP LOCKED
), upd AS (
  UPDATE ip_addresses a
  SET state = 'LEASED', current_lease_id = sqlc.arg(lease_id), warm_host_id = NULL, warm_port_id = NULL,
      state_changed_at = now(), version = version + 1
  FROM cand WHERE a.ip = cand.ip
  RETURNING a.ip, cand.warm_port_id
)
INSERT INTO leases (id, ip, team_id, state, expires_at, neutron_port_id)
SELECT sqlc.arg(lease_id), upd.ip, sqlc.arg(team_id), 'PENDING',
       now() + sqlc.arg(pending_secs)::int * interval '1 second', upd.warm_port_id
FROM upd
RETURNING ip, neutron_port_id;

-- name: ReserveForWarm :one
-- Refiller step 1: claim a FREE IP for host_id. State WARM with warm_port_id NULL means "warming":
-- nobody else can lease it, and TakeWarmIP ignores it until the port exists.
WITH cand AS (
  SELECT ip FROM ip_addresses
  WHERE pool_id = sqlc.arg(pool_id) AND state = 'FREE'
  ORDER BY last_released_at NULLS FIRST, ip
  LIMIT 1
  FOR UPDATE SKIP LOCKED
)
UPDATE ip_addresses a SET state = 'WARM', warm_host_id = sqlc.arg(host_id), state_changed_at = now(),
       version = version + 1
FROM cand WHERE a.ip = cand.ip
RETURNING a.ip;

-- name: MarkWarm :execrows
-- Refiller step 3: the port exists and is attached to the host.
UPDATE ip_addresses SET warm_port_id = sqlc.arg(port_id), state_changed_at = now(), version = version + 1
WHERE ip = sqlc.arg(ip) AND state = 'WARM' AND warm_host_id = sqlc.arg(host_id) AND warm_port_id IS NULL;

-- name: CountWarm :one
SELECT count(*) FROM ip_addresses WHERE state = 'WARM' AND warm_host_id = sqlc.arg(host_id);

-- name: ActivateLease :execrows
WITH l AS (
  UPDATE leases SET state = 'ACTIVE', activated_at = now(),
         expires_at = least(now() + sqlc.arg(ttl_secs)::int * interval '1 second', sqlc.arg(max_expires_at)::timestamptz)
  WHERE id = sqlc.arg(lease_id) AND state = 'PENDING'
  RETURNING ip
)
UPDATE ip_addresses a SET state = 'ACTIVE', state_changed_at = now(), version = version + 1
FROM l WHERE a.ip = l.ip AND a.state = 'LEASED' AND a.current_lease_id = sqlc.arg(lease_id);

-- name: BeginRelease :execrows
WITH l AS (
  UPDATE leases SET state = 'RELEASING'
  WHERE id = sqlc.arg(lease_id) AND state IN ('PENDING','ACTIVE')
  RETURNING ip
)
UPDATE ip_addresses a SET state = 'RELEASING', state_changed_at = now(), version = version + 1
FROM l WHERE a.ip = l.ip AND a.current_lease_id = sqlc.arg(lease_id) AND a.state IN ('LEASED','ACTIVE');

-- name: QuarantineIP :execrows
-- After: route removed (generation Gr), host cleaned, NIC detached / AAP removed, Neutron port deleted.
-- Guard: refuse if any live route still targets this IP.
WITH l AS (
  UPDATE leases SET state = 'RELEASED', released_at = now()
  WHERE id = sqlc.arg(lease_id) AND state = 'RELEASING'
  RETURNING ip
)
UPDATE ip_addresses a
SET state = 'QUARANTINE', current_lease_id = NULL, state_changed_at = now(), last_released_at = now(),
    quarantine_until = now() + sqlc.arg(quarantine_secs)::int * interval '1 second',
    required_ack_generation = sqlc.arg(route_removed_generation), version = version + 1
FROM l
WHERE a.ip = l.ip AND a.state = 'RELEASING' AND a.current_lease_id = sqlc.arg(lease_id)
  AND NOT EXISTS (SELECT 1 FROM routes r WHERE r.target_ip = a.ip AND r.removed_generation IS NULL);

-- name: MarkNeutronAbsent :exec
UPDATE ip_addresses SET neutron_absent_at = now() WHERE ip = sqlc.arg(ip) AND state = 'QUARANTINE';

-- name: FreeEligibleIPs :many
-- Leader sweeper, every 2 s. The ONLY way an IP becomes FREE again after use.
UPDATE ip_addresses a
SET state = 'FREE', quarantine_until = NULL, required_ack_generation = NULL,
    state_changed_at = now(), version = version + 1
WHERE a.state = 'QUARANTINE'
  AND a.quarantine_until <= now()
  AND a.neutron_absent_at IS NOT NULL AND a.neutron_absent_at >= a.state_changed_at
  AND a.required_ack_generation <= (SELECT coalesce(min(g.acked_generation), -1) FROM gateways g WHERE g.member)
  AND NOT EXISTS (SELECT 1 FROM routes r WHERE r.target_ip = a.ip AND r.removed_generation IS NULL)
RETURNING a.ip;

-- name: MarkForeign :exec
-- Neutron said the IP is held by a port that is not ours. Must run in the same tx that releases the lease.
UPDATE ip_addresses SET state = 'FOREIGN', current_lease_id = NULL, note = sqlc.arg(note),
       foreign_clean_scans = 0, state_changed_at = now(), version = version + 1
WHERE ip = sqlc.arg(ip);

-- name: PoolCounts :many
SELECT state, count(*)::int AS n FROM ip_addresses WHERE pool_id = sqlc.arg(pool_id) GROUP BY state;

-- name: PoolGrid :many
SELECT a.ip, a.state, a.state_changed_at, a.quarantine_until, t.slug AS team_slug
FROM ip_addresses a
LEFT JOIN leases l ON l.id = a.current_lease_id
LEFT JOIN teams t  ON t.id = l.team_id
WHERE a.pool_id = sqlc.arg(pool_id)
ORDER BY a.ip;
```

Three details to understand:

1. `coalesce(min(...), -1)`: with **zero** member gateways nothing is freed. This is *fail-safe*: when in doubt, keep the IP locked.
2. `neutron_absent_at >= state_changed_at`: the "Neutron has no port with this IP" check must be **newer** than the moment the IP entered quarantine. An old answer does not count.
3. Lock ordering rule (prevents deadlocks): any transaction that touches several of these rows locks them in the order **team row → ip_addresses row → config_head row**. Write this rule at the top of `internal/store/doc.go`.

::: warn
**The warm-pool reservation gap**

Between "pick an IP for a warm port" and "port created and attached" there are seconds of Neutron and Nova calls. You must **not** hold a database transaction open across those calls (it would keep locks and a pooled connection for seconds). The refiller therefore works in three short steps:

1. `ReserveForWarm` (short transaction): the IP goes FREE → WARM with `warm_port_id = NULL`, which means "warming". Nobody else can lease it, and `TakeWarmIP` ignores it because the port is still NULL.
2. Create the port with that fixed IP and tags `cstam`, `cstam-env:<env>`, `cstam-warm:<host>`, then attach it to the host (no transaction open).
3. `MarkWarm` (short transaction): store the port ID. Now the IP can be taken by a team.

If `cpd` crashes between 1 and 3, the row stays "warming" with a NULL port. The reconciler (Step 8) handles it: after 2 minutes it looks in Neutron for a port with that fixed IP and our tags. If found, it adopts it (`MarkWarm`); if not, it puts the IP through quarantine like any released IP. Correctness never depends on the gap being short.
:::

#### 6.9 The Go IPAM package

```go
// internal/ipam/ipam.go
package ipam

var (
	ErrPoolExhausted = errors.New("ipam: no free IP in pool")
	ErrNoWarm        = errors.New("ipam: no warm port on host")
	ErrBadTransition = errors.New("ipam: illegal state transition")
)

type IPAM struct {
	Pool          *pgxpool.Pool
	PendingTTL    time.Duration // 120 s
	QuarantineTTL time.Duration // 60 s (demo: 20 s)
}

// InTx runs fn in a READ COMMITTED transaction and retries serialization/deadlock errors.
func InTx(ctx context.Context, p *pgxpool.Pool, fn func(q *db.Queries, tx pgx.Tx) error) error {
	for attempt := 0; ; attempt++ {
		err := pgx.BeginTxFunc(ctx, p, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
			return fn(db.New(tx), tx)
		})
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01") && attempt < 5 {
			time.Sleep(time.Duration(10*(attempt+1)) * time.Millisecond)
			continue
		}
		return err
	}
}

// Lease must run inside the caller's transaction that also moves the team to PROVISIONING,
// so "team has a lease" and "team is provisioning" commit together or not at all.
func (a *IPAM) Lease(ctx context.Context, q *db.Queries, teamID uuid.UUID, poolID int32) (uuid.UUID, netip.Addr, error) {
	leaseID := uuid.New()
	ip, err := q.LeaseIP(ctx, db.LeaseIPParams{
		PoolID: poolID, LeaseID: leaseID, TeamID: teamID, PendingSecs: int32(a.PendingTTL.Seconds()),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, netip.Addr{}, ErrPoolExhausted
	}
	return leaseID, ip, err
}

// TakeWarm leases a pre-attached port on hostID. Caller falls back to Lease when ErrNoWarm.
func (a *IPAM) TakeWarm(ctx context.Context, q *db.Queries, teamID, hostID uuid.UUID) (uuid.UUID, netip.Addr, uuid.UUID, error) {
	leaseID := uuid.New()
	row, err := q.TakeWarmIP(ctx, db.TakeWarmIPParams{
		HostID: hostID, LeaseID: leaseID, TeamID: teamID, PendingSecs: int32(a.PendingTTL.Seconds()),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, netip.Addr{}, uuid.Nil, ErrNoWarm
	}
	return leaseID, row.Ip, *row.NeutronPortID, err
}

func mustOne(n int64, err error) error {
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrBadTransition // someone else moved it: re-read, never "force"
	}
	return nil
}

func (a *IPAM) Activate(ctx context.Context, q *db.Queries, leaseID uuid.UUID, ttl time.Duration, max time.Time) error {
	return mustOne(q.ActivateLease(ctx, db.ActivateLeaseParams{LeaseID: leaseID, TtlSecs: int32(ttl.Seconds()), MaxExpiresAt: max}))
}

// BeginRelease is idempotent: 0 rows means "already releasing or released".
func (a *IPAM) BeginRelease(ctx context.Context, q *db.Queries, leaseID uuid.UUID) error {
	_, err := q.BeginRelease(ctx, leaseID)
	return err
}

func (a *IPAM) Quarantine(ctx context.Context, q *db.Queries, leaseID uuid.UUID, removedGen int64) error {
	return mustOne(q.QuarantineIP(ctx, db.QuarantineIPParams{
		LeaseID: leaseID, QuarantineSecs: int32(a.QuarantineTTL.Seconds()), RouteRemovedGeneration: removedGen,
	}))
}
```

```go
// internal/ipam/sweeper.go: runs only on the leader
func RunSweeper(ctx context.Context, a *IPAM, log *slog.Logger) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ips, err := db.New(a.Pool).FreeEligibleIPs(ctx)
			if err != nil {
				log.Error("sweeper", "err", err)
				continue
			}
			for _, ip := range ips {
				log.Info("ip freed", "ip", ip) // + events row "ip.state QUARANTINE->FREE"
			}
		}
	}
}
```

The sweeper is idempotent: two leaders for a second would run the same guarded `UPDATE` and the second one would find nothing to do.

#### 6.10 State machines as data

Transitions are written once, as data, and tested exhaustively. The SQL that changes a phase repeats the guard, so a Go bug cannot bypass it.

| From | To | Trigger / guard | Actor |
|---|---|---|---|
| (none) | `REQUESTED` | `POST /teams`; unique indexes pass; hackathon `OPEN`; live teams < `max_teams` | API |
| `REQUESTED` | `PROVISIONING` | worker claims the team; same tx: IP leased (or warm taken), host chosen | controller |
| `PROVISIONING` | `ACTIVE` | all provision steps done; master gateway acked; health OK; lease ACTIVE | controller |
| `ACTIVE` | `EXPIRING` | `expires_at - now() < 10 min` | expiry sweeper |
| `EXPIRING` | `ACTIVE` | successful renew | API → controller |
| `REQUESTED`/`PROVISIONING`/`ACTIVE`/`EXPIRING` | `DELETING` | `desired = 'ABSENT'` (user, admin, TTL, hackathon end, PENDING deadline, fatal error) | controller |
| `DELETING` | `DELETED` | teardown done, IP in QUARANTINE, no `failure_reason` | controller |
| `DELETING` | `FAILED` | same but `failure_reason IS NOT NULL` | controller |

Forbidden (and unit-tested): `ACTIVE → PROVISIONING`, `DELETING → ACTIVE` (no undelete: create a new team), anything out of a terminal phase, `REQUESTED → ACTIVE`. `FAILED` is reached **only after cleanup**, so "FAILED" never means "something may still be half-built".

IP transitions (Figure 15.1):

| From | To | Guard (enforced in SQL) | Actor |
|---|---|---|---|
| FREE | LEASED | `SKIP LOCKED` pick; creates PENDING lease | controller |
| FREE | WARM (warming, no port yet) | `ReserveForWarm` | warm refiller |
| WARM (warming) | WARM (ready) | port created + attached → `MarkWarm` | warm refiller |
| WARM | LEASED | `TakeWarmIP` | controller |
| WARM | QUARANTINE | warm port removed (host drained, pool shrunk) – same cleanup path as a team | controller |
| LEASED | ACTIVE | lease PENDING → ACTIVE | controller |
| LEASED/ACTIVE | RELEASING | lease → RELEASING | controller |
| RELEASING | QUARANTINE | no live route on IP; lease RELEASED; `required_ack_generation = Gr` | controller |
| QUARANTINE | FREE | time passed ∧ all member gateways acked ≥ Gr ∧ fresh Neutron-absent | sweeper (leader) |
| LEASED | FOREIGN | Neutron 409 and the existing port is not ours | controller |
| FOREIGN | FREE | reconciler saw no port with that IP in two consecutive scans | reconciler |
| RESERVED ↔ FREE | | manual | admin API |

```go
// internal/lifecycle/fsm.go
package lifecycle

type Phase string

const (
	Requested    Phase = "REQUESTED"
	Provisioning Phase = "PROVISIONING"
	Active       Phase = "ACTIVE"
	Expiring     Phase = "EXPIRING"
	Deleting     Phase = "DELETING"
	Deleted      Phase = "DELETED"
	Failed       Phase = "FAILED"
)

var teamTransitions = map[Phase][]Phase{
	Requested:    {Provisioning, Deleting},
	Provisioning: {Active, Deleting},
	Active:       {Expiring, Deleting},
	Expiring:     {Active, Deleting},
	Deleting:     {Deleted, Failed},
}

func CanTransition(from, to Phase) bool {
	for _, p := range teamTransitions[from] {
		if p == to {
			return true
		}
	}
	return false
}

// AllowedFrom returns the phases from which `to` is reachable (used as SQL guard argument).
func AllowedFrom(to Phase) []Phase {
	var out []Phase
	for from, tos := range teamTransitions {
		for _, t := range tos {
			if t == to {
				out = append(out, from)
			}
		}
	}
	return out
}
```

```sql
-- name: SetPhase :execrows
UPDATE teams SET phase = sqlc.arg(to_phase), last_error = NULL,
       deleted_at = CASE WHEN sqlc.arg(to_phase) IN ('DELETED','FAILED') THEN now() ELSE deleted_at END
WHERE id = sqlc.arg(id) AND claim_epoch = sqlc.arg(epoch)          -- fencing (Step 7)
  AND phase = ANY(sqlc.arg(allowed_from)::team_phase[]);
```

#### 6.11 Leader election

Some loops must run on one instance only (they are cheap but noisy): the IP sweeper, the expiry sweeper, the Neutron drift scan. We use a PostgreSQL **advisory lock**: a named lock that lives as long as the database session that took it.

```go
// internal/leader/leader.go
package leader

const lockKey int64 = 0x63737461_6d4c4452 // "cstamLDR"

// Run blocks. It calls lead(ctx) while holding the lock; ctx is cancelled as soon as leadership is in doubt.
func Run(ctx context.Context, dsn string, log *slog.Logger, lead func(context.Context)) {
	for ctx.Err() == nil {
		conn, err := pgx.Connect(ctx, dsn) // dedicated session: NOT from the pool
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		var ok bool
		_ = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockKey).Scan(&ok)
		if !ok {
			conn.Close(ctx)
			time.Sleep(3 * time.Second)
			continue
		}
		log.Info("became leader")
		lctx, cancel := context.WithCancel(ctx)
		go func() { // if our session dies, Postgres already released the lock: stop leading now
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-lctx.Done():
					return
				case <-t.C:
					c, cc := context.WithTimeout(lctx, 800*time.Millisecond)
					err := conn.Ping(c)
					cc()
					if err != nil {
						cancel()
						return
					}
				}
			}
		}()
		lead(lctx)
		cancel()
		conn.Close(context.Background())
		log.Info("lost leadership")
	}
}
```

::: why
**Leadership is an optimisation, not a safety mechanism**

Two leaders for one second (the old one has not yet noticed its session died) must be harmless. It is, because every leader action is either a guarded SQL `UPDATE … WHERE state = 'QUARANTINE' AND …` or an idempotent, tag-checked Neutron call. Never write leader code like "I am the leader, so I can just delete it".
:::

Gotcha: a session-level advisory lock belongs to *that* connection. Never return it to a pool, and add `keepalives_idle=5` to the DSN so a dead leader's lock is released quickly by PostgreSQL.

#### 6.12 OpenAPI skeleton and the first endpoints

Write the contract first (`api/openapi/cstam.v1.yaml`), then generate the Go server interface with `oapi-codegen` (std `net/http`, strict server) and, later, the TypeScript types for the dashboard.

```yaml
openapi: 3.1.0
info: { title: CSTAM control plane, version: 1.0.0 }
servers: [{ url: /v1 }]
components:
  securitySchemes:
    session: { type: apiKey, in: cookie, name: cstam_session }
    token:   { type: http, scheme: bearer }
  schemas:
    Problem:
      type: object
      properties: { type: {type: string}, title: {type: string}, status: {type: integer}, detail: {type: string} }
    Hackathon:
      type: object
      required: [id, slug, name, status, starts_at, ends_at, max_teams]
      properties:
        id: { type: string, format: uuid }
        slug: { type: string, pattern: '^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$' }
        name: { type: string }
        status: { type: string, enum: [DRAFT, OPEN, CLOSED, ARCHIVED] }
        starts_at: { type: string, format: date-time }
        ends_at: { type: string, format: date-time }
        max_teams: { type: integer, minimum: 1, maximum: 160 }
        default_ttl: { type: string, example: "4h" }
    PoolView:
      type: object
      properties:
        counts: { type: object, additionalProperties: { type: integer } }
        addresses:
          type: array
          items:
            type: object
            properties:
              ip: { type: string }
              state: { type: string }
              team: { type: string, nullable: true }
              quarantine_until: { type: string, format: date-time, nullable: true }
security: [{ session: [] }, { token: [] }]
paths:
  /hackathons:
    get:  { operationId: listHackathons, responses: { '200': { description: ok, content: { application/json: { schema: { type: array, items: { $ref: '#/components/schemas/Hackathon' } } } } } } }
    post: { operationId: createHackathon, requestBody: { required: true, content: { application/json: { schema: { $ref: '#/components/schemas/Hackathon' } } } },
            responses: { '201': { description: created }, '409': { description: conflict, content: { application/problem+json: { schema: { $ref: '#/components/schemas/Problem' } } } } } }
  /pools/{id}:
    get:
      operationId: getPool
      parameters: [{ name: id, in: path, required: true, schema: { type: integer } }]
      responses: { '200': { description: ok, content: { application/json: { schema: { $ref: '#/components/schemas/PoolView' } } } } }
```

```yaml
# oapi-codegen.yaml
package: gen
generate: { std-http-server: true, strict-server: true, models: true }
output: internal/api/gen/server.gen.go
```

```go
// internal/api/server.go  (excerpt)
type Server struct{ deps Deps }

func (s *Server) GetPool(ctx context.Context, req gen.GetPoolRequestObject) (gen.GetPoolResponseObject, error) {
	q := db.New(s.deps.Store.Pool)
	counts, err := q.PoolCounts(ctx, int32(req.Id))
	if err != nil {
		return nil, err
	}
	grid, err := q.PoolGrid(ctx, int32(req.Id))
	if err != nil {
		return nil, err
	}
	return gen.GetPool200JSONResponse(toPoolView(counts, grid)), nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	strict := gen.NewStrictHandler(s, nil)
	gen.HandlerFromMuxWithBaseURL(strict, mux, "/v1")
	var h http.Handler = mux
	h = middleware.Auth(s.deps.Auth)(h)      // placeholder in this step; real auth in Step 9
	h = middleware.RequestID(h)
	h = middleware.Recover(s.deps.Log)(h)
	h = middleware.AccessLog(s.deps.Log)(h)
	return h
}
```

Errors use RFC 9457 `application/problem+json`. A unique violation (`23505`) on `hackathons.slug` becomes `409 {"type":"https://cstam/errors/conflict","title":"slug already exists"}`.

The auth middleware in this step only checks a static bootstrap token from `CSTAM_BOOTSTRAP_TOKEN` (so nobody can call the API while you develop on the cloud). Step 9 replaces it with argon2id sessions, roles and audit.

#### 6.13 Tests

**Transition table (unit, no DB):**

```go
func TestTeamTransitions(t *testing.T) {
	all := []Phase{Requested, Provisioning, Active, Expiring, Deleting, Deleted, Failed}
	allowed := map[[2]Phase]bool{
		{Requested, Provisioning}: true, {Requested, Deleting}: true, {Provisioning, Active}: true,
		{Provisioning, Deleting}: true, {Active, Expiring}: true, {Active, Deleting}: true,
		{Expiring, Active}: true, {Expiring, Deleting}: true, {Deleting, Deleted}: true, {Deleting, Failed}: true,
	}
	for _, f := range all {
		for _, to := range all {
			if got := CanTransition(f, to); got != allowed[[2]Phase{f, to}] {
				t.Errorf("%s -> %s: got %v", f, to, got)
			}
		}
	}
}
```

**Test database (testcontainers-go):**

```go
// internal/testutil/pg.go
func NewDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	c, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("cstam"), postgres.WithUsername("t"), postgres.WithPassword("t"),
		postgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Terminate(ctx) })
	dsn, _ := c.ConnectionString(ctx, "sslmode=disable")
	if err := store.MigrateDSN(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	cfg, _ := pgxpool.ParseConfig(dsn)
	cfg.MaxConns = 120
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
```

**100 goroutines leasing at the same moment:**

```go
func TestConcurrentLease(t *testing.T) {
	pool := testutil.NewDB(t)
	teams := testutil.SeedTeams(t, pool, 100) // 100 team rows in REQUESTED
	a := &ipam.IPAM{Pool: pool, PendingTTL: 2 * time.Minute, QuarantineTTL: time.Minute}

	var mu sync.Mutex
	seen := map[netip.Addr]uuid.UUID{}
	start := make(chan struct{})
	g, ctx := errgroup.WithContext(context.Background())
	for _, team := range teams {
		g.Go(func() error {
			<-start // release all goroutines together: maximum contention
			return ipam.InTx(ctx, pool, func(q *db.Queries, _ pgx.Tx) error {
				_, ip, err := a.Lease(ctx, q, team, testutil.TeamPoolID)
				if err != nil {
					return err
				}
				mu.Lock()
				defer mu.Unlock()
				if other, dup := seen[ip]; dup {
					return fmt.Errorf("ip %s given to %s and %s", ip, other, team)
				}
				seen[ip] = team
				return nil
			})
		})
	}
	close(start)
	require.NoError(t, g.Wait())
	require.Len(t, seen, 100)
	require.NotContains(t, seen, netip.MustParseAddr("10.20.0.68")) // RESERVED is never leased
	testutil.AssertInvariants(t, pool)
}
```

**Exhaustion:** lease 159 times (all FREE team IPs), then the 160th call must return `ErrPoolExhausted`, and the API turns that into `503` with `Retry-After` = seconds until the next quarantine expiry.

**Invariants** (`internal/testutil/invariants.sql`, every query must return zero rows; run after every DB test and, in production, every minute by the reconciler with an alert):

```sql
-- 1. one live lease per IP
SELECT ip FROM leases WHERE state <> 'RELEASED' GROUP BY ip HAVING count(*) > 1;
-- 2. ip row points at a lease that is live and for the same IP
SELECT a.ip FROM ip_addresses a JOIN leases l ON l.id = a.current_lease_id
 WHERE l.state = 'RELEASED' OR l.ip <> a.ip;
-- 3. no live route through a released lease
SELECT r.fqdn FROM routes r JOIN leases l ON l.id = r.lease_id
 WHERE r.removed_generation IS NULL AND l.state = 'RELEASED';
-- 4. no live route to an IP that is FREE / QUARANTINE / WARM
SELECT a.ip FROM ip_addresses a JOIN routes r ON r.target_ip = a.ip
 WHERE r.removed_generation IS NULL AND a.state IN ('FREE','QUARANTINE','WARM');
-- 5. no FREE IP whose route removal is not yet acked by every member gateway
SELECT a.ip FROM ip_addresses a
 WHERE a.state = 'FREE'
   AND a.ip IN (SELECT target_ip FROM routes
                WHERE removed_generation > (SELECT coalesce(min(acked_generation),0) FROM gateways WHERE member));
-- 6. ACTIVE team has exactly one ACTIVE lease
SELECT t.id FROM teams t LEFT JOIN leases l ON l.team_id = t.id AND l.state = 'ACTIVE'
 WHERE t.phase = 'ACTIVE' GROUP BY t.id HAVING count(l.id) <> 1;
```

```go
func AssertInvariants(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	for i, q := range splitSQL(invariantsSQL) {
		rows, err := pool.Query(context.Background(), q)
		require.NoError(t, err)
		n := 0
		for rows.Next() {
			n++
		}
		rows.Close()
		if n > 0 {
			t.Fatalf("invariant %d violated (%d rows): %s", i+1, n, q)
		}
	}
}
```

### Done when

::: done
**Done when**
```bash
make gen && go build ./... && golangci-lint run ./...
go test -race ./internal/lifecycle/...                                     # transition table green
go test -race -count=5 ./internal/ipam -run 'TestConcurrentLease|TestLeaseExhaustion'
#   ok  ... (5 runs, 100 distinct IPs each time, .68 never leased, invariants hold)

# on cstam-cp, with the bootstrap token
sudo -u cstam /usr/local/bin/cpd migrate
curl -sk -H "Authorization: Bearer $CSTAM_BOOTSTRAP_TOKEN" https://10.20.0.5/v1/pools/1 | jq .counts
#   {"FREE":159,"RESERVED":1}
curl -sk https://10.20.0.5/readyz        # 200
```
:::

### Pitfalls

::: warn
**Do not hold a transaction across a network call**

Never call Neutron, Nova or an agent while a database transaction is open. The transaction would keep row locks and a pooled connection for seconds, and 20 concurrent registrations would exhaust the pool. Pattern: short transaction → network call → short transaction. Idempotent steps (Step 7) make this safe.
:::

::: warn
**`SKIP LOCKED` needs `LIMIT` inside the locked subquery**

If you write `UPDATE … WHERE ip = (SELECT … FOR UPDATE SKIP LOCKED)` without `LIMIT 1` in the subquery, or if you put `ORDER BY` outside it, PostgreSQL may lock more rows than you think, and concurrency drops. Keep the exact `WITH cand AS (… LIMIT 1 FOR UPDATE SKIP LOCKED)` shape.
:::

::: warn
**Time from Go is wrong time**

Do not pass `time.Now()` from Go into expiry columns. Use `now()` in SQL. A control plane with a skewed clock would otherwise expire leases early or never.
:::

::: warn
**Advisory locks and connection pools**

`pg_try_advisory_lock` on a pooled connection is a classic bug: the connection goes back to the pool, another goroutine uses it, and "you" are still the leader through somebody else's session. Use a dedicated `pgx.Connect` for the leader.
:::

### Learning checkpoint

::: learn
**Answer these yourself, in `docs/notebook/learning.md`, before Step 7**

1. Why does `SKIP LOCKED` give 50 different IPs to 50 concurrent transactions, while plain `FOR UPDATE` would make them wait? Draw two transactions on a timeline.
2. Which three independent mechanisms prevent two teams from holding 10.20.0.57 at the same time?
3. Why is `QUARANTINE → FREE` allowed only in one SQL statement, and what are its four conditions?
4. What would go wrong if Neutron's allocation pool were 10.20.0.2–.250 instead of .200–.250?
5. Why is the leader election "only an optimisation"? Give an example of a leader action and explain why running it twice is harmless.
:::

### Driving Claude Code for this step

::: claude
**Prompts that work well**

- Plan mode first: *"Read docs/guide chapter 15 and migrations/00001_init.sql. Propose the package layout for internal/store, internal/ipam and internal/lifecycle, the sqlc queries, and the tests. Do not write code yet. List risks."*
- Then one package at a time: *"Implement internal/ipam exactly as in chapter 15 §6.9, with TestConcurrentLease and TestLeaseExhaustion using testcontainers. Do not change the SQL in ipam.sql without asking me."*
- Ask for proof, not code: *"Show me a sequence of two concurrent transactions that would break invariant 5 if FreeEligibleIPs did not check min(acked_generation). Then show which line prevents it."*

**What to review in the diff:** every transaction is short (no network calls inside), every state-changing SQL has a `WHERE state = …` guard, `mustOne` is used for transitions that must affect exactly one row, and no `time.Now()` is written into the database.
:::
