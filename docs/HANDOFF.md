# Handoff: where the work stands (2026-10-18)

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
make test-int                                        # green, needs Docker (HAProxy, keepalived labs)
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
| Dual HA gateways, subdomain routing, failover | 15 | **core done, tested** (local Docker); agent binary missing |
| Automated IPAM and reclamation, short leases | 10 | **schema done** (`migrations/00001_init.sql`, verified on Postgres 18); code missing |
| API-triggered OpenStack sandbox provisioning | 15 | missing |
| Deliverables: code, architecture/data-flow diagram, demo video | | diagrams in the guide; official docs and video missing |

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

### Task A: IPAM + store (`internal/store`, `internal/ipam`)
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

### Task B: OpenStack sandbox driver (`internal/openstack`)
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

### Task C: `cpd` (control plane, `cmd/cpd`, `internal/api`, `internal/lifecycle`)
- REST (Go 1.22 `net/http` patterns, bearer admin token from env):
  `POST /v1/hackathons`, `DELETE /v1/hackathons/{h}` (tears down all teams),
  `POST /v1/hackathons/{h}/teams` -> 202 + team (state `provisioning`), `GET` list/show,
  `POST /v1/teams/{id}/renew`, `DELETE /v1/teams/{id}`.
- Worker: provisioning goroutines (bounded), on success `state=ready` + generation bump (route
  published `<team>.<domain> -> slot -> ip:app_port`). On failure: clean up, `failed`, IP released.
- Reconciler every 2 s: expired leases -> teardown; teams stuck in `provisioning`/`deleting` after a
  restart are driven again; eligible quarantine -> free. Log events to `events`.
- Gateway endpoints (separate gateway token): `GET /v1/gateways/{gw}/desired?after=<gen>`
  (long-poll 25 s, returns `{generation, routes}`), `POST /v1/gateways/{gw}/ack`
  `{generation, config_version, vrrp_state}`.

### Task D: `cmd/gwagent`
Wire the tested packages: poll `desired`, `applier.SyncRoutes`, write `/run/cstam/ready` after the
first successful sync, ack, report VRRP state from `/run/cstam/vrrp.state`, serve the canary on
127.0.0.1:9102. Flags for every path and URL (no hardcoded IPs, use `internal/config`).
Test: gwagent + HAProxy in Docker against a fake desired-state server.

### Task E: local demo stack + video
`docker compose` with Postgres, cpd, gw1/gw2 (HAProxy + keepalived + gwagent), and a Docker sandbox
driver, using the same IP plan (10.20.0.0/24) as FelCloud. Record: create teams -> routing works,
hot reload with a broken config -> rollback, `docker pause gw1` -> failover. When floating IPs exist,
redo it on FelCloud.

### Task F: official docs (deliverable, not team notes)
`docs/official/`: architecture, data-flow diagram of the HA gateway (Excalidraw sources are in
`docs/guide/diagrams/`), API reference, IPAM lifecycle, measured results from the notebook.

## 6. Good to know
- Guide errata found by testing, already fixed in ch. 12: keepalived 2.3 refuses `delay 0` in
  `vrrp_track_process` and an explicit `track_interface` on the instance's own interface.
- `vrrp_track_process` does not work in a container (process events only reach the initial PID
  namespace): test it on the real gateways.
- The guide (`docs/guide/chapters/`) is the long reference: 12 gateways, 13 gwagent, 15 control
  plane, 16 provisioning, 17 teardown and IPAM. Where it says "Connect-RPC", use plain HTTP JSON
  for now (that dependency is not approved). Where it says sqlc, plain pgx queries are fine.
- FelCloud quirks are in `notebook/experiments.md` (F1, D1). Add every new finding there.
