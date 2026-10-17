# 7. Go software architecture

This chapter describes the code: which programs exist, how they talk, how the repository is organized, which libraries are used and why, the core interfaces, and how the system behaves under load and failure. Part II then builds it piece by piece.

![Figure 7.1 – The four Go programs, where they run, and how they communicate. Thick arrows are the data path; the control plane is not on it.](diagrams/f04-go-services.png)

## 7.1 Deployables and roles

| Program | Runs on | Responsibilities | Talks to |
|---|---|---|---|
| **`cpd`** (control-plane daemon) | `cstam-cp` | Role `api`: REST `/v1`, SSE, embedded dashboard, agent endpoint (Connect over mTLS, :8443). Role `controller`: team workers, AAP syncer or warm-pool filler, gateway publisher, sweepers, Neutron drift scan and garbage collector. Subcommand `migrate`. | PostgreSQL (pgx), Neutron and Nova (gophercloud), agents (they dial in) |
| **`gwagent`** | `cstam-gw1`, `cstam-gw2` | Watch desired gateway state; apply routes at runtime and structural changes by validated reload; verify; roll back; ack generations with digests; report VRRP state; manage the readiness file and force-backup file; stream traffic logs | Local HAProxy admin socket and master CLI, keepalived (files and notify script), `cpd` |
| **`hostagent`** | `cstam-host-a`, `cstam-host-b` | Watch desired team specs for its host; plumb team networking through a `NetDriver` (pod-NIC or AAP); run hardened containers; firewall; restore after reboot; clean up on removal; report status | Local Docker (moby client), netlink, nftables/iptables, `cpd` |
| **`cstamctl`** | Your laptop or the control plane | `pki init/issue`, `node enroll`, `doctor` (read-only cloud checks), `bench` (stress, failover, reload, recycle), `sectest` (attack suite), `gc --dry-run`, `audit verify` | `cpd` REST, SSH, OpenStack (read-only) |

`cpd` roles are chosen with `CSTAM_ROLES=api,controller` (default both). Two `cpd` processes can run at once: both serve the API; workers share work through database claims; only the leader (a Postgres advisory lock) runs sweepers and scans.

## 7.2 Bounded contexts

Each context is a Go package with an interface. Other packages never touch its tables directly; `depguard` in golangci-lint enforces the import rules.

| Context | Package | Owns |
|---|---|---|
| Tenancy | `internal/tenancy` | Hackathons, teams, names, quotas, app specs |
| IPAM | `internal/ipam` | IP rows, leases, state machine, quarantine |
| Cloud adapter | `internal/openstack` | Ports, tags, allowed-address-pairs, interface attach, security-group reads, floating IP (fallback F1) |
| Edge routing | `internal/edge` | Routes, generations, config versions, gateway membership and acks |
| Sandbox runtime | `internal/runtime` | Host desired state, warm pool, host capacity |
| Lifecycle | `internal/lifecycle` | Claims, fencing, step pipelines, sweepers, reconciler |
| Agent API | `internal/agentapi` | Connect service implementation for agents |
| Public API | `internal/api` | REST handlers, middleware, SSE broker |
| Access | `internal/auth`, `internal/audit` | Admins, sessions, tokens, agent identities, audit chain |
| Platform | `internal/platform` | TLS, HTTP servers, retry, metrics, systemd notify |

## 7.3 Repository layout

One Go module, `github.com/<you>/cstam`, Go 1.25.

```text
cstam/
├── go.mod  go.sum  Makefile  buf.yaml  buf.gen.yaml  sqlc.yaml  .golangci.yml  CLAUDE.md
├── api/
│   ├── openapi/cstam.v1.yaml          # public REST contract (source of truth)
│   └── proto/cstam/
│       ├── agent/v1/control.proto     # ControlService: hosted by cpd, called by agents
│       ├── gateway/v1/gateway.proto   # gateway desired state, status, logs
│       └── host/v1/host.proto         # host desired state, team status
├── gen/                               # generated code, committed (openapi server, proto, connect)
├── cmd/
│   ├── cpd/main.go
│   ├── gwagent/main.go
│   ├── hostagent/main.go
│   └── cstamctl/main.go
├── internal/
│   ├── model/        # domain types: Team, Lease, Route, Phase, Condition (no dependencies)
│   ├── config/       # env structs per binary; topology.go (IP plan, names)
│   ├── store/        # sqlc queries + generated code + tx helpers
│   ├── ipam/  tenancy/  edge/  runtime/  lifecycle/
│   ├── openstack/    # NeutronClient interface, gophercloud impl, fake/, limiter
│   ├── agentapi/     # Connect ControlService (inside cpd)
│   ├── api/          # REST handlers, middleware/, sse/
│   ├── auth/  audit/  pki/  platform/
│   ├── gw/           # gwagent internals: haproxy/ (runtime, render, apply), keepalived/, logs/
│   ├── host/         # hostagent internals: netdriver/podnic, netdriver/aap, firewall/, docker/, slices/
│   └── testutil/     # testcontainers Postgres, fakes, invariant checks
├── migrations/       # goose SQL files (embedded)
├── web/              # React + Vite dashboard; web/dist embedded by web/embed.go
├── deploy/
│   ├── systemd/  cloud-init/  haproxy/  keepalived/  env/
├── infra/            # pulumi/ (Go IaC, own go.mod), topology.env + lab/ (Step 1 shell experiments)
├── bench/            # stress, failover probe, loadgen, report generator
├── test/e2e/         # go test -tags e2e (real FelCloud)
└── docs/             # experiments.md, learning.md, reports
```

::: why
**One module, not one module per binary**

One version, one `go test ./...`, shared types (`model`, generated protobuf) without `replace` directives. The linker only includes what a binary imports, so `gwagent` does not contain pgx or gophercloud; check with `go version -m bin/gwagent`.
:::

## 7.4 The stack

| Concern | Choice (pin in `go.mod`) | Why |
|---|---|---|
| HTTP router | `net/http` ServeMux (method and `{id}` patterns) + `oapi-codegen` strict server | Zero dependencies; generated code targets it |
| Agent RPC | `connectrpc.com/connect` + `buf` (`protoc-gen-go`, `protoc-gen-connect-go`) | Typed contract, curl-able, gRPC-compatible |
| OpenStack | `github.com/gophercloud/gophercloud/v2` | Official Go SDK; re-authenticates; reads `clouds.yaml`. Calling the `openstack` CLI costs 1–2 s of Python start-up per call |
| Database | `github.com/jackc/pgx/v5` + `sqlc` | Real SQL (`SKIP LOCKED`, advisory locks, `pg_notify`), typed Go. ORMs hide exactly the locking you must explain |
| Migrations | `github.com/pressly/goose/v3`, embedded SQL | Plain SQL up/down |
| Config | `github.com/caarlos0/env/v11` + systemd `EnvironmentFile=` | One struct with tags |
| Logs | `log/slog` (JSON handler) | Standard library |
| Metrics | `github.com/prometheus/client_golang` | `/metrics` on every daemon |
| Tracing (optional) | OpenTelemetry (`otelhttp`, `otelconnect`) | One trace per provisioning for the benchmark breakdown |
| Docker | `github.com/moby/moby/client` (API version negotiation) | Typed, no output parsing, much faster than forking `docker` |
| Netlink | `github.com/vishvananda/netlink`, `github.com/vishvananda/netns` | Move links between namespaces, addresses, routes; idempotent checks |
| Firewall | Render text → `nft -c -f` then `nft -f` (pod-NIC); `iptables-restore --noflush --test` then apply (AAP) | Atomic, readable rules you can show the jury |
| HAProxy | Small raw runtime-socket client (~150 lines) + `haproxy -c` + master CLI | The protocol is plain text; no heavy dependency |
| systemd | `github.com/coreos/go-systemd/v22` | `READY=1`, watchdog, transient slices over D-Bus |
| Rate limit / concurrency | `golang.org/x/time/rate`, `golang.org/x/sync/errgroup`, `semaphore` | Protect Neutron; bounded parallelism |
| Auth | `golang.org/x/crypto/argon2`; opaque tokens hashed with SHA-256 | No JWT needed |
| ACME (bonus) | `github.com/go-acme/lego/v4` | DNS-01 / HTTP-01 |
| Tests | `testing`, `testify`, `testcontainers-go` (Postgres module), `httptest`, `pgregory.net/rapid`, `goleak` | Real Postgres for concurrency tests (SKIP LOCKED cannot be faked) |
| Load | `github.com/tsenart/vegeta/v12/lib` | Constant-rate open-loop load, every request recorded |
| Lint | golangci-lint v2: `govet, staticcheck, errcheck, gosec, revive, depguard, errorlint, contextcheck, bodyclose, sqlclosecheck` | Catches the bugs that hurt network code |

All binaries build with `CGO_ENABLED=0` (pure Go dependencies).

## 7.5 Communication contracts

### Agent protocol (Connect, hosted by `cpd`)

```proto
// api/proto/cstam/agent/v1/control.proto
syntax = "proto3";
package cstam.agent.v1;

service ControlService {
  // Server stream: sends a full snapshot now, then again whenever the generation changes.
  rpc WatchGateway(WatchRequest) returns (stream GatewayDesired);
  rpc WatchHost(WatchRequest)    returns (stream HostDesired);
  rpc Ack(AckRequest)            returns (AckResponse);       // what was applied, with digest and timings
  rpc Report(AgentReport)        returns (ReportResponse);    // heartbeat every 2 s
  rpc PushTrafficLogs(stream LogBatch) returns (PushAck);     // gateways only
}

message WatchRequest { string agent_id = 1; int64 after_generation = 2; }

message Route { string host = 1; string slot = 2; string ip = 3; uint32 port = 4; uint32 rate_limit_rps = 5; }
message GatewayDesired {
  int64 generation = 1;            // global routing generation (config_head)
  repeated Route routes = 2;       // FULL table, never a delta
  bytes map_digest = 3;            // sha256 of the sorted "host slot ip:port" lines
  int32 structural_version = 4;    // HAProxy template version to run
  bytes structural_config = 5;     // rendered template for that version
  uint32 slot_count = 6;
  bool force_backup = 7;           // manual failover: agent writes /run/cstam/force-backup
}

message AckRequest {
  string agent_id = 1; int64 generation = 2; bytes digest = 3;
  enum Result { RESULT_UNSPECIFIED = 0; APPLIED = 1; REJECTED_VALIDATION = 2; ROLLED_BACK = 3; }
  Result result = 4; string detail = 5;
  int64 validate_ms = 6; int64 apply_ms = 7; int64 verify_ms = 8; int64 rollback_ms = 9;
}

message AgentReport {
  string agent_id = 1;
  enum Vrrp { VRRP_UNSPECIFIED = 0; MASTER = 1; BACKUP = 2; FAULT = 3; }
  Vrrp vrrp = 2; bool haproxy_up = 3; int64 applied_generation = 4; bool ready = 5;
  int32 structural_version = 6; repeated TeamStatus teams = 7;   // hosts fill teams
}
```

`HostDesired` carries the host's `desired_rev` and the full list of team specs on that host (team ID, IP, MAC of its port in pod-NIC mode, apps with image, port, env, limits). The host agent converges to it and acknowledges `desired_rev`.

### Public REST API (`/v1`, OpenAPI 3.1)

| Resource | Endpoints | Notes |
|---|---|---|
| Hackathons | `POST /v1/hackathons`, `GET /v1/hackathons`, `GET/PATCH/DELETE /v1/hackathons/{id}` | Domain, host(s), pool, default TTL, join code, max teams |
| Teams | `POST /v1/hackathons/{hid}/teams` (202), `GET /v1/teams?hackathon_id=&phase=`, `GET /v1/teams/{id}`, `DELETE /v1/teams/{id}` (202, sets desired = ABSENT), `POST /v1/teams/{id}:renew`, `POST /v1/teams/{id}:retry` | The team is the long-running operation: poll it or watch SSE |
| Apps | `PUT /v1/teams/{id}/apps/{name}` (app spec), `DELETE …` | Spec allowlist; env values are write-only |
| Self-registration | `POST /v1/register {join_code, team_name}` | Rate limited; returns a team-scoped token |
| IPAM (read-only) | `GET /v1/ipam/pool`, `GET /v1/leases?state=` | Feeds the IP grid |
| Routes (read-only) | `GET /v1/routes` | Derived from teams |
| Gateways | `GET /v1/gateways`, `POST /v1/gateways/{name}:failover`, `:failback`, `:fence`, `:rejoin`, `:simulate-crash` (demo mode) | Status from heartbeats |
| Config versions | `GET /v1/config-versions`, `GET /v1/config-versions/{v}`, `GET …/{v}/diff?against=`, `POST /v1/config-versions` (propose, 202), `POST /v1/config-versions/{v}:rollback` (202) | Rollback creates a **new** version with old content |
| Operations | `GET /v1/operations?team_id=` | Every step with its duration |
| Events and streams | `GET /v1/events?after=`, `GET /v1/stream?topics=team,gateway,ipam,config,traffic` (SSE) | `Last-Event-ID` resume |
| Auth | `POST /v1/auth/login`, `POST /v1/auth/logout`, `POST /v1/api-tokens`, `DELETE /v1/api-tokens/{id}` | |
| Audit | `GET /v1/audit?cursor=`, `POST /v1/audit:verify` | |
| System | `GET /healthz`, `GET /readyz`, `GET /metrics` (metrics port) | |

Conventions: JSON with `snake_case`, RFC 3339 times, UUIDv7 IDs, keyset pagination (`page_token`), errors as RFC 9457 `application/problem+json`, `Idempotency-Key` on every POST (same key + same body → original response; different body → 422), custom verbs in the `:verb` style.

Example:

```http
POST /v1/hackathons/0190a1b2-.../teams
Authorization: Bearer cstam_7Qk...
Idempotency-Key: 6f1c2b8e-1d2a-4e0b-9a7e-3c1f0b7e9d11
Content-Type: application/json

{"name":"team5","ttl":"6h"}
```
```http
HTTP/1.1 202 Accepted
Location: /v1/teams/0190a1c3-...

{"id":"0190a1c3-...","name":"team5","fqdn":"team5.hacka.cstam.felcloud.tn",
 "desired":"PRESENT","phase":"REQUESTED","ip":null,"conditions":[],
 "lease":null}
```

## 7.6 Core interfaces

```go
// internal/ipam/ipam.go: runs inside the caller's transaction.
type IPAM interface {
	Lease(ctx context.Context, q *store.Queries, teamID uuid.UUID, poolID int32) (leaseID uuid.UUID, ip netip.Addr, err error) // FREE -> LEASED (SKIP LOCKED, LRU)
	Activate(ctx context.Context, q *store.Queries, leaseID uuid.UUID, ttl time.Duration, max time.Time) error            // LEASED -> ACTIVE
	BeginRelease(ctx context.Context, q *store.Queries, leaseID uuid.UUID) error                                          // -> RELEASING (idempotent)
	Quarantine(ctx context.Context, q *store.Queries, leaseID uuid.UUID, removedGen int64) error                          // RELEASING -> QUARANTINE
	MarkForeign(ctx context.Context, q *store.Queries, ip netip.Addr, note string) error
}
var ErrPoolExhausted = errors.New("ipam: no free IP in pool")

// internal/openstack/neutron.go: our vocabulary, not gophercloud's.
type NeutronClient interface {
	PortByFixedIP(ctx context.Context, ip netip.Addr) (*Port, error)                 // nil, nil when absent
	CreatePort(ctx context.Context, spec PortSpec) (*Port, error)                    // fixed IP, name, tags, SG; 409 -> ErrIPAlreadyAllocated
	UpdatePortIdentity(ctx context.Context, portID, name string, tags []string) error // warm-pool claim (rename + retag)
	DeletePort(ctx context.Context, portID string) error                             // 404 -> nil
	ListPortsByTags(ctx context.Context, tags ...string) ([]Port, error)             // drift scan, GC
	// AAP mode: replace the whole list with compare-and-swap; HTTP 412 -> ErrRevisionConflict.
	SetAllowedAddressPairs(ctx context.Context, portID string, ips []netip.Prefix, revision int) (*Port, error)
	// pod-NIC mode: Nova interface attach/detach.
	AttachPort(ctx context.Context, serverID, portID string) error
	DetachPort(ctx context.Context, serverID, portID string) error
}
type Port struct {
	ID, Name, MAC, DeviceID, Status string
	FixedIP                         netip.Addr
	AllowedPairs                    []netip.Prefix
	Tags                            []string
	Revision                        int
}

// internal/lifecycle/step.go: one idempotent unit of work.
type Step interface {
	Name() string
	Done(ctx context.Context, t *Team) (bool, error) // observe the real world
	Apply(ctx context.Context, t *Team) error        // make Done true; may be called again
}

// internal/edge/publisher.go: the only writer of generations.
type Publisher interface {
	AddRoute(ctx context.Context, tx pgx.Tx, r model.Route) (gen int64, err error)
	RemoveRoutes(ctx context.Context, tx pgx.Tx, teamID uuid.UUID) (gen int64, err error)
	MinAckedGeneration(ctx context.Context) (int64, error) // across member gateways
}

// internal/host/netdriver/netdriver.go: inside hostagent.
type NetDriver interface {
	Ensure(ctx context.Context, spec model.TeamNetSpec) (model.TeamNetStatus, error) // idempotent
	Remove(ctx context.Context, teamID uuid.UUID) error                              // idempotent
	List(ctx context.Context) ([]model.TeamNetStatus, error)                         // for drift and reboot restore
}

// internal/gw/haproxy/runtime.go: inside gwagent.
type Runtime interface {
	Cmd(ctx context.Context, cmd string) (string, error)
	SetServer(ctx context.Context, slot string, ip netip.Addr, port uint16, ready bool) error
	MapAdd(ctx context.Context, mapPath, key, val string) error
	MapDel(ctx context.Context, mapPath, key string) error
	MapReplace(ctx context.Context, mapPath string, entries map[string]string) error // prepare/add @v/commit
	MasterReload(ctx context.Context) (ok bool, startupLog string, err error)
}

// internal/store: transactions with retry on 40001/40P01.
func InTx(ctx context.Context, p *pgxpool.Pool, fn func(q *Queries, tx pgx.Tx) error) error
```

Fakes for tests: `internal/openstack/fake` implements `NeutronClient` in memory with fixed-IP uniqueness (409), revision numbers (412 on stale revision), an allowed-address-pair limit, latency, and fault injection including the nasty "created, then timed out" case. Fake gateway and host agents record what they applied, so tests can assert the teardown order.

## 7.7 Status model and error classes

A team has:

| Field | Meaning | Written by |
|---|---|---|
| `desired` | `PRESENT` or `ABSENT` | API, expiry sweeper |
| `phase` | `REQUESTED → PROVISIONING → ACTIVE ⇄ EXPIRING → DELETING → DELETED` (or `FAILED` after cleanup of a failed provisioning) | Controller only |
| `health` | `UNKNOWN / HEALTHY / DEGRADED / DOWN`; never triggers deletion | Health checker, reconciler |
| `conditions` | Per-step status (`IPLeased`, `PortReady`, `HostReady`, `RoutesSynced`, `Healthy`) with reason and message | Controller |
| `spec_version`, `claim_epoch` | Change counter; fencing token | API; claim query |

Errors are classified once, in `internal/lifecycle/errors.go`:

| Class | Examples | Behavior |
|---|---|---|
| Retryable | Timeout, 5xx, 409 revision conflict, 412, connection refused | Release claim with exponential backoff and jitter (500 ms … 30 s) |
| Waiting | Gateway ack pending, warm port not yet attached | Requeue soon; not counted as failure |
| Fatal for the team | Over quota, pool exhausted, invalid spec, AAP limit exceeded | `desired = ABSENT`, `failure_reason` set → teardown → `FAILED` |
| Fatal for the system | 401 after re-authentication, network missing | Stop workers, alert |

## 7.8 Concurrency model and protecting Neutron

```text
API (one goroutine per request)              Controller
  tx: insert team, NOTIFY                       ├─ team workers: up to 16 teams in parallel (claims)
  202                                           ├─ AAP syncer: one goroutine per host port (AAP mode)
                                                ├─ warm-pool filler: per host (pod-NIC mode)
                                                ├─ gateway publisher: batches route changes (50 ms)
                                                └─ leader only: sweepers (2–5 s), drift scan (30 s), GC

All Neutron/Nova calls → openstack.Limiter: rate.Limiter(10 rps, burst 10) + semaphore(8 in flight)
                         + 15 s timeout per call + small retry (3 tries, full jitter) on 5xx/409/412/429
hostagent: semaphore(4) for Docker operations; per-team mutex
```

Rules:

- **Context deadlines everywhere**: API handlers 10 s, a step 15–30 s, a Neutron call 15 s, a health probe 2 s.
- **Never hold a database transaction open across a Neutron call.** Transactions are milliseconds; the claim protects the team between them.
- **One gophercloud `ProviderClient`** shared by all goroutines (it is concurrency-safe and re-authenticates under a lock).
- **pgx pool size** ≥ API concurrency + workers + 2 (leader and `LISTEN` connections). 30 is plenty.
- Retry small at the call site, then return the error and let the claim backoff handle the rest. Never retry at full depth in two layers.

## 7.9 Performance budget (estimates, to be replaced by measurements)

| Step | Mechanism | Estimate | Parallel? | Bottleneck |
|---|---|---|---|---|
| API transaction: team + idempotency row | Postgres | 2–10 ms | Yes | – |
| Claim + lease (SKIP LOCKED) | Postgres | 1–3 ms (< 20 ms p99 at 50) | Yes | – |
| pod-NIC: claim warm port (rename + retag) | 1–2 Neutron PUTs | 0.2–0.8 s | Yes (limiter) | Neutron |
| pod-NIC cold path: create port + attach | Neutron + Nova | 2–6 s, **serialized per VM** | No (per host) | Nova attach lock |
| AAP: create port | Neutron | 0.3–1.5 s | Yes | Neutron |
| AAP: host port pairs update | Neutron, coalesced | 0.3–1.5 s per update, 1–3 updates per wave | Per host: no | One shared port |
| Host agent: namespace, firewall, containers | Kernel + dockerd | 0.3–2 s | Partly | dockerd |
| Route + generation + both gateways ack | Postgres + runtime API | 50–200 ms | Yes | – |
| Health probe | HTTP | 0.1–1 s | Yes | Container start |
| **Total per team** | | **≈ 1–3 s warm (pod-NIC), 2–5 s (AAP)** | | |

Projected waves (estimates):

- **10 teams** (one host, warm pool ≥ 10): all ACTIVE in about 3–6 s.
- **50 teams** (two hosts): warm pools cover the first ~24, the rest attach cold at 2–6 s per NIC per host in parallel across hosts: all ACTIVE in about 30–60 s in pod-NIC mode, 12–30 s in AAP CIDR mode. This is why the warm-pool size is a configuration value and why Step 11 measures it.

Two cheap accelerators: pre-pull images (`pause`, demo apps) on hosts in cloud-init, and size the warm pool to the expected wave.

## 7.10 Deployment and operations

| Topic | How |
|---|---|
| Build | `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(git describe --tags)"` |
| Install | cloud-init installs packages (`haproxy`, `keepalived`, `docker`, `chrony`, `nftables`, `arping`, `socat`), sysctls, users. **No secrets.** `cstamctl node enroll gw1` then copies the binary, certificate, key and env file over SSH via the control plane, and enables the systemd unit |
| Units | `Type=notify` (an agent is ready only once synced), `Restart=always`, `WatchdogSec=30`, `NoNewPrivileges`, `ProtectSystem=strict`, minimal `ReadWritePaths` |
| Privileges | `gwagent` runs as user `cstam` in group `haproxy` with a sudo rule for exactly `systemctl reload haproxy` (or uses the master CLI socket); `hostagent` needs `CAP_NET_ADMIN` and Docker access (root-equivalent: say so in the security doc; it has no inbound port) |
| Config | `/etc/cstam/<daemon>.env` (mode 600). Runtime data (routes, teams, versions) lives in the database and is never edited on VMs |
| Upgrades | Backup gateway first → verify synced → manual failover → upgrade the other. Hosts: Docker `live-restore`. Agents accept protocol version N and N−1 (`buf breaking` in CI) |
| Backups | `pg_dump` every 5 minutes to a Swift container; a restore drill timed for the report |
| Static stability | Agents persist last-good state on disk and restore it at boot without the control plane |

Control-plane HA: see ADR-21. Deploy one VM; demonstrate two `cpd` processes (kill the leader during a stress test: the other takes the lock within about 5 s and finishes the work).

## 7.11 Observability

| Signal | Where | Examples |
|---|---|---|
| Logs | `slog` JSON → journald (`journalctl -u cpd -o cat \| jq`) | Every step with `team`, `step`, `duration_ms`, `epoch` |
| Metrics | `/metrics` on each daemon (bound to the private IP) | `cstam_teams{phase}`, `cstam_ips{state}`, `cstam_step_seconds{step}`, `cstam_neutron_request_seconds{op,code}`, `cstam_gateway_vrrp_state{gw}`, `cstam_gateway_acked_generation{gw}`, `cstam_apply_total{result}`, `cstam_rollback_seconds`, `cstam_warm_pool_size{host}`, `cstam_logs_dropped_total` |
| Traces (optional) | OTel → Jaeger during benchmarks only | One trace per provisioning |
| Audit / events | `events` table, `audit_log` (hash chain), SSE | `team.phase`, `ip.state`, `gateway.failover`, `config.rolled_back`, `gc.deleted_orphan_port` |
| HAProxy | Prometheus exporter on :8404 (from the control plane only), JSON traffic logs | Per-slot request rate, 5xx, latency |

::: learn
**Learning checkpoint**

1. Draw the four programs and the arrows between them from memory. Which arrows carry user traffic?
2. Why does the `Watch` stream always send a full snapshot instead of a delta?
3. Which interface would you fake to test the `edges.md` teardown order, and what would the test assert?
4. Why must a database transaction never stay open during a Neutron call?
5. Where does the time go when 10 teams register at once in AAP mode, and what does coalescing change?
:::
