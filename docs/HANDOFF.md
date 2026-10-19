# Handoff: where the work stands (2026-10-19)

Written for the next teammate **and their Claude Code**, which will work mostly on its own.
First prompt to give Claude:

> Read docs/HANDOFF.md, CLAUDE.md, docs/README.md and docs/challenge/brief.md.
> Then do the setup check in section 1 and continue with section 5, task by task.

## 1. Setup check (Claude can run all of this)
The human only has to put two secrets in place (sent privately by Ahmed), as described in
[`team/README.md`](team/README.md): the OpenStack application credential in
`~/.config/openstack/clouds.yaml` (cloud name `cstam`) and their own S3 key in `~/.aws/credentials`
(profile `cstam`, created with `openstack ec2 credentials create`). Then:
```bash
export OS_CLOUD=cstam PULUMI_CONFIG_PASSPHRASE=""
openstack server list -f value -c Name -c Status     # cstam-cp, cstam-gw1, cstam-gw2: ACTIVE
make lint test build                                 # green
make test-int                                        # green, needs Docker (HAProxy, keepalived labs, postgres:18-alpine)
                                                     # CSTAM_TEST_PG_DSN=postgres://... reuses an existing Postgres instead
cd infra/pulumi && pulumi stack select prod && pulumi preview   # "no changes" (floating IPs pending, see 4)
```

## 2. Rules that still apply with full access (from CLAUDE.md)
- Cloud changes only through Pulumi, `pulumi preview` shown to the human before `pulumi up`.
  Never `pulumi destroy` / `state unprotect`. The VIP port and the public floating IP are protected.
- The only resources created outside Pulumi are created **by our own program `cpd`** at runtime
  (team ports and sandbox VMs), named `cstam-<hackathon>-<team>` and tagged `cstam`. Manual
  `openstack create/delete` only for lab resources named `cstam-lab-*`.
- The OpenStack project is **shared with other people**: never touch a resource that is not ours
  (not named `cstam-*`). Never delete the OVN metadata port 10.20.0.200.
- Secrets: may be read, copied and installed when needed; never committed, never printed in chat.
- Commit messages short, no em-dashes. Small commits.
- Dependencies **already approved** by Ahmed (2026-10-18), pin these versions because newer ones
  require Go 1.26 and the module is Go 1.25:
  `github.com/gophercloud/gophercloud/v2@v2.15.0`, `github.com/gophercloud/utils/v2@v2.0.0-20260922174841-b4759ea0529b`,
  `github.com/jackc/pgx/v5@v5.11.0`, `github.com/pressly/goose/v3@v3.26.0`,
  `golang.org/x/text@v0.30.0`, `golang.org/x/sync@v0.17.0`. Anything else: ask.
  After `go get`, check `go.mod` still says `go 1.25` (`go mod edit -go=1.25` if not).

## 3. The challenge and our status
Scoring is in [`challenge/brief.md`](challenge/brief.md). Phase 2 (stress test, teardown, isolation)
builds on Phase 1, so Phase 1 comes first.

| Phase 1 part | Pts | Status |
|---|---|---|
| Zero-downtime reload, validation, sub-second rollback | 10 | **done, tested** (local Docker) |
| Dual HA gateways, subdomain routing, failover | 15 | **done, tested** (local Docker): HAProxy + keepalived + `gwagent`; FelCloud run waits for floating IPs |
| Automated IPAM and reclamation, short leases | 10 | **done, tested**: IPAM wired into cpd (lease expiry, ack-gated recycling) |
| API-triggered OpenStack sandbox provisioning | 15 | **done**: REST API `cpd` + VM driver tested on FelCloud; full run on FelCloud waits for floating IPs |
| Deliverables: code, architecture/data-flow diagram, demo video | | **official docs in `docs/official/`** (Task F); local demo stack (`deploy/dev/demo.sh`, Task E); video recording missing |

### What exists and is tested
- `internal/gw/haproxy`: renders `haproxy.cfg` (128 slot backends `be_slot001..`, canary `be_slot000`,
  `/__gw/healthz` returning `X-Config-Version` and `X-Routes-Generation`), map files, runtime API
  client (set server, atomic versioned map swap), master CLI `reload` (`Success=1`), `haproxy -c`.
- `internal/gw/applier`: render -> `haproxy -c` -> swap -> hitless reload -> verify (healthz + canary)
  -> rollback to last good. `SyncRoutes(generation, routes)` changes routes at runtime, no reload.
- `internal/gw/keepalived`: renders `keepalived.conf` + `chk_gateway` + `vrrp-notify`, reads the state file.
- Measurements (notebook entries L1, L2 in `notebook/experiments.md`): 0 failed requests over 20
  reloads (96,606 requests); broken config rolled back in 73 ms (0.72 s total); failover 0.2 s
  (manual / clean stop), 1.5 s (HAProxy killed), 1.7 s (frozen master).
- Infra (Pulumi stack `prod`): router, 5 security groups, VIP port .10, gw1 .11, gw2 .12, cp .5.
- Proof: `go test -tags integration -v ./internal/gw/...`

## 4. Blockers outside the code
- **No floating IPs**: FelCloud had no free public IP (`ExternalIpAddressExhausted`). Without them
  nothing can reach our VMs (no SSH, no deploy, no public demo). Ahmed asked the mentors for 2.
  When they exist, `pulumi up` in `infra/pulumi` creates them (code already there, protected).
  Check from time to time: `openstack floating ip list` / ask Ahmed.
- What still works without them: `cpd` can run **on the laptop** and call the FelCloud API
  (Keystone/Nova/Neutron are public), so provisioning can be built and tested for real.
  The VMs have outbound internet (router SNAT), but inbound is impossible.
- Fallback if the floating IPs never come (ask Ahmed first): add a self-updater to the gateway
  cloud-init (systemd timer pulling binaries + sha256 from the S3 bucket) and rebuild gw1/gw2 once
  through Pulumi.
- gw1 and gw2 share one hypervisor (soft anti-affinity only works on FelCloud).

## 5. Next tasks, in order
Work like CLAUDE.md says: explain the plan, small diffs, tests, `make lint test build`, commit.

### Task A: IPAM + store (`internal/store`, `internal/ipam`): DONE (2026-10-19)
What exists: `store.Open/Migrate/InTx` (goose provider on the embedded FS, retry on 40001/40P01),
`ipam.New()` with `Seed`, `Allocate(tx, team)`, `Release(tx, team) -> generation`, `Sweep(db, inUse)`,
`Renew`, `Expired`, `Counts`. Every IP change writes an `events` row (`ip.allocated/quarantined/freed`).
`internal/testutil.Postgres(t)` gives each test a fresh database (Docker, or `CSTAM_TEST_PG_DSN`).
Proof: `go test -race -tags integration -v ./internal/ipam/` (7 tests, incl. 1000 parallel allocations).

Decisions taken while building it (change them if you disagree):
- Allocation takes a transaction-scoped advisory lock: the lowest free slot is computed from
  committed rows, so without it two transactions could pick the same slot. Allocations are fast,
  so serializing them costs nothing at our scale (128 slots).
- `Release` refuses a team still in state `ready` (`ErrStillRouted`): the caller takes the route out
  in the same transaction first. It also clears `teams.ip` (history is in `events`), otherwise the
  unique index `teams_ip_live` would block reuse while the old team is still `deleting`.
- `Sweep` frees an IP when it has been quarantined >= `Quarantine` (60 s default), every gateway
  seen in the last `GatewayWindow` (5 min) acked `released_gen`, and the `inUse` callback (Neutron
  port check, Task B) says no. If gateways are registered but none was seen recently, nothing is
  freed (safer). No gateway registered at all (local dev) means free.
- `go.mod` says `go 1.25.0`, not `go 1.25`: pgx v5.11.0 itself declares `go 1.25.0` (same Go line).
  `go mod tidy` was not run: it wants the test-only SQLite driver of goose (a large download) that
  we never build. `go build ./...`, `go vet` and golangci-lint are clean.

Original design notes:
Schema is in `migrations/00001_init.sql` (embedded by package `migrations`, apply with goose at cpd
start). Design:
- Pool = team pool .32-.191 from `internal/config/topology.go`, seeded idempotently
  (`INSERT ... ON CONFLICT DO NOTHING`); `config.ReservedIPs` (.68) seeded as `reserved`.
- Allocate in the same transaction as the team row: `SELECT ip FROM ip_pool WHERE state='free'
  ORDER BY ip LIMIT 1 FOR UPDATE SKIP LOCKED`, also the lowest free slot 1..128.
- Lease = `teams.lease_expires_at` (hackathon `lease_ttl`, renew endpoint extends it).
- Release: the route is removed first (bump `routes_generation`), IP goes to `quarantine` with
  `released_gen` = that generation. It becomes `free` only when every gateway seen in the last
  5 minutes has `acked_gen >= released_gen` (and the Neutron port is deleted). This is what makes
  recycling safe: no gateway can still send traffic to a reused IP.
- Tests (tag `integration`, Postgres in Docker via `docker run`, no testcontainers dependency):
  1000 parallel allocations never give the same IP twice; pool exhaustion returns a clear error;
  quarantine is not freed before both gateways ack.

### Task B: OpenStack sandbox driver (`internal/openstack`): DONE (2026-10-19)
What exists: `lifecycle.Sandboxes` interface (`Create`, `Delete`, `InUse`, `List`), `Spec` with
validation (dns labels, team/lab pool IP, image ref), `Ref` stored as JSON in `teams.sandbox_ref`.
`openstack.Driver` (gophercloud): port with fixed IP + VM, idempotent by name, quota and IP-taken
errors mapped to `lifecycle.ErrQuota` / `ErrIPTaken`. `openstack/fake` for unit tests.
Proof: `go test ./internal/lifecycle/ ./internal/openstack/...` and the e2e test
(notebook L4: 23 s create, 14 s delete).

Original plan:
- Interface (put it in `internal/lifecycle`): `Create(ctx, Spec{Hackathon, Team, IP, Port, Image}) (ref, error)`,
  `Delete(ctx, ref) error`, both idempotent (find by name first).
- Phase 1 sandbox = **one small VM per team** (flavor `G0.basic.1c1g`, image `Debian 12 - Bookworm`)
  on a Neutron port with the fixed IP chosen by IPAM, security group `cstam-team` (only the
  gateways can reach it; teams cannot reach each other). cloud-init starts the team app on
  `app_port` (default: a tiny page saying team name + IP; with `image`: install docker and run it).
  Network/SG IDs come from `pulumi stack output` (`networkId`, `subnetId`, `teamSecGroupId`).
- Quota: 10 instances total, 3 used, so about 6 teams at a time. For Phase 2's 10+ teams, add a
  second driver later: containers on on-demand hosts (.21-.31) with pod-NIC (guide ch. 14).
- `internal/openstack/fake` for unit tests; an `e2e` test (tag `e2e`) creates one real team and
  deletes it, then lists that nothing `cstam-e2e-*` is left.

### Task C: `cpd` (control plane): DONE (2026-10-19)
What exists:
- `internal/lifecycle/controller.go`, `records.go`: team states provisioning -> ready | failed ->
  deleting -> deleted. One worker per team (bounded by `-workers`), reconciler every 2 s (lease
  expiry, teams stuck after a restart, failed-team cleanup after 10 min, IP sweep with the
  driver's in-use check, hackathon ending -> ended). `Desired` long-poll, `Ack` refuses a
  generation ahead of the DB (it would free IPs too early).
- `internal/model`: JSON wire types shared with gwagent (no pgx dependency).
- `internal/api`: REST API, bearer tokens (admin and gateway, distinct, constant-time compare),
  errors mapped to 400/404/409/503, internal errors never leaked.
  Endpoints: `POST/GET /v1/hackathons`, `GET/DELETE /v1/hackathons/{h}`,
  `POST/GET /v1/hackathons/{h}/teams`, `GET/DELETE /v1/teams/{id}`, `POST /v1/teams/{id}/renew`,
  `GET /v1/status`, `GET /v1/gateways/{gw}/desired?after=N[&wait=25s]`, `POST /v1/gateways/{gw}/ack`.
- `cmd/cpd`: env config (`CSTAM_DB_DSN`, `CSTAM_ADMIN_TOKEN`, `CSTAM_GATEWAY_TOKEN`,
  `CSTAM_DRIVER=openstack|fake`, `CSTAM_NETWORK_ID/SUBNET_ID/TEAM_SG_ID`), migrates and seeds at start.
Proof: `go test -race -tags integration -v ./internal/lifecycle/` (10 tests), `go test ./internal/api/`,
and `deploy/dev/cpd-smoke.sh` (real cpd + Postgres in Docker + fake cloud, through curl).
Decisions: a failed team keeps its name and slot for 10 min so people can read the error, then it
is deleted. Deleting a team that is mid-creation waits for the create call, then deletes the VM.

### Task D: `cmd/gwagent`: DONE (2026-10-19)
What exists: `internal/gw/agent` (the loop) and `cmd/gwagent` (flags, canary on 127.0.0.1:9102,
`-bootstrap` for systemd ExecStartPre). The agent removes `/run/cstam/ready` at start, restores the
routes saved in `/var/lib/cstam/desired.json`, long-polls `cpd`, applies with `applier.SyncRoutes`,
saves, writes `ready` (`synced`, or `stale` after 10 s without cpd), acks. It never applies an
older generation, does not ack a generation it failed to apply (backoff 1 s -> 15 s), and acks at
once when `/run/cstam/vrrp.state` changes (polled every 500 ms: fsnotify is not an approved dep).
Token from `CSTAM_GATEWAY_TOKEN` only.
Proof: `go test -race ./internal/gw/agent/` (8 unit tests) and
`go test -tags integration -v -run TestAgentDrivesRealHAProxy ./internal/gw/agent/` (fake cpd ->
agent -> real HAProxy 3.2 in Docker -> team apps; a new route serves 24 ms after cpd publishes it).
Not done: force-backup flag from cpd (manual failover is the file `/run/cstam/force-backup`),
traffic log streaming (guide ch. 13, optional), systemd units in `deploy/systemd`.

### Task E: local demo stack: DONE (2026-10-19), video still to record
What exists:
- `internal/docker`: `lifecycle.Sandboxes` driver, one container per team on a Docker network at
  its IPAM IP (docker CLI with argv, label `cstam.sandbox`, memory/CPU/pids limits). Default page:
  the `haproxy:3.2` image answering "team X of H at IP" (no extra download); `image` runs that image
  with `PORT`/`APP_PORT` set. `cpd` uses it with `CSTAM_DRIVER=docker` (`CSTAM_DOCKER_NETWORK`).
- `gwagent -bootstrap -vrrp-iface ens3` also writes keepalived.conf and the VRRP scripts
  (`keepalived.ForGateway`: gw1 priority 150, gw2 100, peers and VIP from the IP plan). Use it in the
  gateway systemd units too (Task G).
- `deploy/dev/demo.sh up | teams N | failover pause|kill | reload | teardown | status | down` and
  `deploy/dev/gw-entrypoint.sh`. Plain `docker run` (no compose plugin to download), bridge
  `cstam-demo` = 10.20.0.0/24 with the host as .1, cpd on 10.20.0.1:8080, gateways .11/.12, VIP .10.
  `HACK=name demo.sh teams` uses another hackathon (an ended one keeps its name).
Proof: `go test -tags integration ./internal/docker/` and the run in notebook L6 (team routed
0.7-2.1 s after the API call, failover 1.3-2.3 s, IPs freed 61 s after teardown and reused).
Two gateway bugs found by the demo and fixed (L6): slow retry of a failed first apply, and a
gateway that took the VIP while serving 503 after a failed reload.
Left for a human: **record the video** (screen capture of `demo.sh up`, `teams 4`, a browser or
curl on a team, `failover pause`, `reload`, `teardown`). The local network is not isolated
between teams (Docker bridge); isolation is the job of `cstam-team` on FelCloud.
When floating IPs exist, redo the demo on FelCloud.

### Task F: official docs: DONE (2026-10-19)
`docs/official/`: `README.md` (requirement -> implementation -> evidence table), `architecture.md`
(components, HA gateway data flow, reload/rollback, lifecycle and IPAM, security; reuses the guide
PNG diagrams and says where the built system differs from them), `api.md`, `benchmark.md`
(B1-B7 from notebook L1-L7, incl. the stress test `demo.sh stress N`: 50 teams p95 4.9 s).
Keep `benchmark.md` up to date when FelCloud numbers arrive (failover with OVN, VM stress).

### Task H: reject app ports the security group blocks
`cstam-team` only opens TCP 80 and 8000-8099 from the gateways, but `CreateTeam` accepts any port:
on FelCloud a team on port 3000 would be `ready` but unreachable. Validate `app_port` against the
same ranges for the openstack driver (keep the ranges in one place shared with `infra/pulumi`),
with a table test. Documented in `docs/official/api.md` meanwhile.

### Task G: systemd units for the FelCloud VMs (needed once floating IPs exist)
`deploy/systemd/`: `gwagent.service` (`ExecStartPre=gwagent -bootstrap -vrrp-iface ens3`, ordered
before `haproxy.service` for the files and started after it for the sockets, the same order as
`deploy/dev/gw-entrypoint.sh`), `cpd.service` (env file `/etc/cstam/cpd.env`, 0600). Delete stale
HAProxy sockets before start (L6 bug 2 came from a stale socket). Test what can be tested without
the VMs: `systemd-analyze verify`.

## 6. Good to know
- Guide errata found by testing, already fixed in ch. 12: keepalived 2.3 refuses `delay 0` in
  `vrrp_track_process` and an explicit `track_interface` on the instance's own interface.
- `vrrp_track_process` does not work in a container (process events only reach the initial PID
  namespace): test it on the real gateways.
- The guide (`docs/guide/chapters/`) is the long reference: 12 gateways, 13 gwagent, 15 control
  plane, 16 provisioning, 17 teardown and IPAM. Where it says "Connect-RPC", use plain HTTP JSON
  for now (that dependency is not approved). Where it says sqlc, plain pgx queries are fine.
- FelCloud quirks are in `notebook/experiments.md` (F1, D1). Add every new finding there.
