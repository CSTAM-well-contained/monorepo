# Lab notebook

Every experiment, measurement and decision: command, output, date, conclusion.
Benchmark and technical reports are written from this file.

## Template
### E<n> – <title>  (date, who)
- Question:
- Commands (exact):
- Output (trimmed):
- Measurement (median / max, n runs):
- Conclusion / decision:
- Cleanup done: yes/no (resources deleted: ...)

---

### F1 – Project facts at start  (2026-10-16, read-only)
- Subnet `cstam-sandbox-subnet` 10.20.0.0/24, Neutron allocation pool only .200–.250 (.200 = OVN metadata port).
- MTU 1442 (Geneve/OVN). Quota: 10 instances, 20 vCPU, 50 GB RAM, 10 SG, 100 SG rules, 50 FIPs.
- Object storage is Ceph RGW with an S3 API at https://object-store.api.felcloud.io (used for Pulumi state).

### C1 – Clean-up of pre-existing VMs  (2026-10-17, team confirmed unused)
- Deleted VMs `adam` (SHUTOFF, 4c4g) and `sbx-f0699704` (10.20.0.68), their 2 boot volumes,
  the sbx port and its security group. Kept the OVN metadata port (10.20.0.200) and all FelCloud networks.
- 10.20.0.68 stays RESERVED in our IPAM until a Neutron scan confirms nobody uses it.

### D1 – First `pulumi up` of the platform  (2026-10-17)
- Router: `enable_snat` in external_gateway_info → **403 PolicyNotAuthorized** for non-admins.
  Fix: do not send it (SNAT is on by default). Router outside IP: 197.5.133.180.
- Gateways with **anti-affinity**: gw2 → ERROR "Exhausted all hosts available for retrying build failures".
  Switched to **soft-anti-affinity**: both ACTIVE but **same hostId** (same hypervisor).
  Consequence: HA covers VM / HAProxy / keepalived failures, not a hypervisor failure. Asked FelCloud.
- Floating IPs: **ExternalIpAddressExhausted** on INTERNET (197.5.133.0/24, pool .1–.230).
  No way in from outside until FelCloud frees IPs. Asked mentors for 2 floating IPs.
- Serial console log is empty on FelCloud (`openstack console log show`): use the VNC console URL instead.

### L1 – Gateway agent: reload and rollback against real HAProxy  (2026-10-17, laptop, Docker)
- Test: `go test -tags integration ./internal/gw/applier/` (haproxy:3.2 = 3.2.26, host network).
- 20 hitless reloads under 8 client loops (half keep-alive): **96,606 requests, 0 failed**. Slowest apply 211 ms.
- Invalid config (`haproxy -c` fails): rejected in 71 ms, old version keeps serving.
- Valid but broken config (healthz returns 500): check 85 ms, reload 61 ms, verify gave up after 500 ms,
  **rollback 73 ms**, total 0.72 s. Last good version serves again.
- Routes added, moved and removed at runtime (no reload); they survive a later reload.

### L2 – keepalived failover between two containers  (2026-10-17, laptop, Docker bridge)
- Test: `go test -tags integration ./internal/gw/keepalived/` (keepalived 2.3.3, VRRPv3 unicast, advert 0.5 s, nopreempt).
- VIP unreachable (client probing every 20 ms, 100 ms timeout):
  | Failure on the master | Outage |
  |---|---|
  | manual failover (`force-backup` = 1) | 0.20 s |
  | clean keepalived stop (priority-0 advert) | 0.20 s |
  | HAProxy killed, caught by `chk_gateway` (interval 1, fall 2) | 1.5 s |
  | master frozen (`docker pause`, silent failure) | 1.7 s (theory 1.8 s) |
- Two guide bugs found and fixed: `delay 0` in `vrrp_track_process` is refused (must be > 0, omit it);
  `track_interface` on the instance's own interface is refused (it is tracked implicitly).
- `vrrp_track_process` cannot be tested in a container (process events only reach the initial PID
  namespace). **To do on the real gateways**: expect HAProxy-kill failover well under 1 s with it.
- Not covered by Docker: OVN GARP/rebinding time on FelCloud. Re-measure on the VMs (Phase 1 demo).

### L3 – IPAM against real PostgreSQL  (2026-10-19, Emna's PC, WSL Debian 13, Docker)
- Test: `go test -race -count=1 -tags integration -v ./internal/ipam/` (postgres:18-alpine, one fresh DB per test).
- 1000 concurrent "create team + allocate" transactions, 128 slots: **128 allocated, 872 `ErrPoolExhausted`,
  no IP or slot given twice**, 3.8 s total including the team inserts (race detector on).
- 300 concurrent with 999 slots: 159 allocated (pool .32-.191 minus reserved .68), 141 exhausted; .68 never leased.
- Quarantine sweep, 7 cases: freed only when every gateway seen in the last 5 min acked the release
  generation; a gateway behind blocks it; a stale gateway (10 min) is ignored; all gateways stale blocks;
  quarantine time and the Neutron in-use callback block it.
- Cleanup done: yes (containers run with `--rm`, test databases dropped).

### L4 - Sandbox VM driver against FelCloud  (2026-10-19, Emna's PC, WSL Debian 13)
- Test: `OS_CLOUD=cstam go test -tags e2e -run TestE2ESandbox -v ./internal/openstack/`
  (one G0.basic.1c1g Debian 12 VM on a Neutron port with a fixed lab-pool IP, SG `cstam-team`).
- `cstam-e2e-t8ff67f` on 10.20.0.199: **created and ACTIVE in 23 s, deleted (server + port) in 14 s**.
- Cleanup done: yes, `openstack server list` shows only cp/gw1/gw2, no `cstam-e2e-*` port left.

### L5 - Gateway agent end to end with real HAProxy  (2026-10-19, Emna's PC, WSL Debian 13, Docker)
- Test: `go test -tags integration -v -run TestAgentDrivesRealHAProxy ./internal/gw/agent/` (haproxy:3.2).
- Fake cpd publishes generation 2 with a new team: **routed 24 ms later**, by the runtime API
  (no reload). Removing a team: its host stops answering 200, the other team keeps serving, ack follows.
- Cleanup done: yes (container `--rm`, temp dir removed).

### L6 - Whole platform on one machine (local demo)  (2026-10-19, Emna's PC, WSL Debian 13, Docker)
- Run: `deploy/dev/demo.sh up`, `teams 4`, `failover pause|kill` x 6, `reload`, `teardown`. Real cpd
  (docker sandbox driver) + Postgres, gw1/gw2 containers (HAProxy 3.2 + keepalived + gwagent) on a
  Docker bridge `10.20.0.0/24` with the FelCloud IP plan, team containers at their IPAM addresses.
- API call to first 200 through the VIP: **0.7-2.1 s per team** (upper bound: teams are polled one
  after the other; see L7 for the parallel measurement. Container start ~0.6 s, then the
  route push to both gateways). The IPAM gives .32, .33, ... and slots 1, 2, ... as designed.
- Failover, takeover time measured with a 20 ms probe through the VIP (6 runs, alternating gateways):
  | failure | takeover |
  |---|---|
  | master frozen (`docker pause`) | 1.32, 1.62, 1.85 s |
  | HAProxy killed (`pkill -KILL haproxy`) | 1.57, 1.86, 2.31 s |
  Same range as L2. After `docker unpause` the frozen master still believes it is MASTER with the
  higher priority and takes the VIP back (a second, short switch); a restarted gateway comes back
  as BACKUP (`nopreempt`).
- Teardown of 4 teams: containers gone in 0.7 s; the 4 IPs stay in quarantine and are freed at 61 s
  (60 s quarantine, both gateways acked). The next hackathon got 10.20.0.32 again.
- **Two bugs found by this run, fixed with regression tests:**
  1. `gwagent`: a failed first apply (HAProxy canary not up yet) was retried only after the 25 s
     long-poll, so a fresh gateway stayed FAULT for ~26 s. Now it fetches again at once
     (`TestFailedApplyRetriedWhileCPDIsIdle`).
  2. `applier.SyncRoutes`: when the runtime sync and the fallback reload both failed (gateway
     restarted, HAProxy not listening yet), the new slot table and generation were recorded anyway.
     The next sync skipped the servers, said "applied", the gateway became ready and took the VIP
     **while answering 503** for every team. Now a failed reload forgets the slot table and keeps the
     old generation (`TestSyncAfterFailedReload`).
- Cleanup done: yes (`demo.sh down` removes containers and the network).

### L7 - Concurrent registrations stress test  (2026-10-19, Emna's PC, WSL Debian 13, Docker)
- Run: `CPD_WORKERS=<n> deploy/dev/demo.sh up`, then `HACK=<name> deploy/dev/demo.sh stress <N>`:
  N `POST /teams` sent at the same time; a team counts as done when its own page (name + IP)
  answers through the VIP.
  | teams | workers | all API answers | routed | p50 | p95 | max |
  |---|---|---|---|---|---|---|
  | 20 | 4 | 0.32 s | 20/20 | 2.37 s | 4.02 s | 4.16 s |
  | 20 | 16 | 0.40 s | 20/20 | 2.11 s | 2.42 s | 3.55 s |
  | 50 | 16 | 0.51 s | 50/50 | 3.39 s | 4.93 s | 5.10 s |
- 0 API errors, all IPs and slots distinct, every team `ready`. Sandbox creation (bounded by
  `-workers`) dominates; with 4 workers the teardown also goes by batches of 4 (~0.65 s each).
- Teardown of 50: sandboxes gone in 7 s, all 50 IPs free at 63 s.
- Cleanup done: yes (`demo.sh down`).

### D3 - Where the public IPs went  (2026-10-20, Emna's PC, read-only checks)
Timeline (UTC), from the FelCloud client area (felcloud.io/console > Real Time Billing > Floating
IP), `openstack router show` (`updated_at`) and `pulumi stack history`:
| When | What |
|---|---|
| 10-10 14:07-14:08 | FIP 197.5.133.147 allocated and released (1 min) |
| 10-10 14:11-19:41 | FIPs **197.5.133.185 -> 10.20.0.10 (VIP)** and 197.5.133.75 -> 10.20.0.100, then **released** (before this repo existed: earlier lab) |
| 10-16 | facts read: `private_router` (not ours) never had a gateway (guide ch. 3) |
| 10-17 10:39 | `pulumi up` creates `cstam-router`, gateway IP **197.5.133.180**; both FIPs already `ExternalIpAddressExhausted` (D1) |
| 10-17 11:14 | last successful `pulumi up` (teammate, deleted 2 lab resources, router untouched) |
| 10-17 12:09 | **router gateway removed by hand** (router `updated_at`, revision 6), not by Pulumi |
| 10-19, 10-20 | `pulumi up` retries: FIPs still exhausted |

What went wrong:
1. The project held public IPs on 10-10 (one already on the VIP .10) and released them; in a
   full pool a released IP is gone for good.
2. The router's own public IP (.180) was removed on 10-17 by a manual action (dashboard or
   FelCloud). Pulumi does not refresh by default, so `pulumi preview` kept saying "unchanged" and
   HANDOFF wrongly said the VMs had outbound internet. Since 10-17 the VMs have **no internet at all**.
   (`pulumi preview --refresh` shows it.) An earlier version of this entry said FelCloud also
   reclaimed `private_router`'s IP: wrong, it never had one.

Self-service options checked (none frees a 197.5.133.x address):
- Neutron: one external network with a public range (`INTERNET`, pool .1-.230); `Rancher-mgmt`
  and `lb-mgmt-net` are internal. No IPv6. `ip availability` and specific-IP requests are admin-only.
  An Octavia load balancer with a public VIP draws from the same pool.
- Client area: Load Balancer (same pool), Pack Offer (not open yet), **VPS** ("1 IPv4", Starter
  2 vCPU/4 GB, 14.76 TND/month, balance 174 TND). Not ordered: it is a purchase, and the VPS is
  not on `cstam-sandbox-net`, so it cannot reach our VMs while the router has no gateway.
- Cleanup done: nothing created.

### D2 - Floating IP retry  (2026-10-19 21:54, Emna's PC)
- `pulumi preview`: only `cstam-public-fip` (on the VIP port) and `cstam-cp-fip` to create, 30
  unchanged. `pulumi up`: both fail with **ExternalIpAddressExhausted** (pool 197.5.133.1-.230 on
  `INTERNET` is still full). Project quota is 50 FIPs, so the pool, not the quota, is the limit.
  The dashboard uses the same Neutron API: it cannot do better.
- Nothing partial created (`openstack floating ip list` empty), state unchanged.
- The noVNC console works (`openstack console url show`), but the VMs are key-only with no
  password, so it gives no login. Other external networks (`Rancher-mgmt`, `lb-mgmt-net`) are
  FelCloud management networks, not internet access.
- Retry: `cd infra/pulumi && pulumi preview && pulumi up` (same two creates).

### L8 - systemd units on systemd gateways  (2026-10-19, Emna's PC, WSL Debian 13, Docker)
- Run: `GW_MODE=systemd deploy/dev/demo.sh up`: gw1/gw2 boot systemd (lab image + systemd, stand-in
  for the haproxy package unit) and are installed by `deploy/systemd/install-gateway.sh`, the same
  script as on the VMs. Only differences: `eth0` and `-vrrp-track-process=false`.
- Result: all four units active on both gateways; HAProxy master root, workers `haproxy`, gwagent
  `cstam:haproxy`; sockets `root:haproxy 0660`; `/run/cstam` recreated by tmpfiles after a reboot.
  `docker restart` of a gateway (full boot): BACKUP again after ~4 s, no failed CSTAM unit.
- **HAProxy killed: no failover any more**, systemd restarts it (`Restart=always`) before
  `chk_gateway` fails twice: VIP down **0.45, 0.46, 0.59, 0.61 s** (was 1.5-2.3 s by failover).
  Frozen master: failover in 1.58, 1.83 s as before. 20 parallel teams: 20/20, p95 3.3 s.
- Found while writing them: the HAProxy admin socket had no group, so the unprivileged agent could
  not use it; gwagent now sets `group haproxy` on it (`SocketGroup`).
- `cpd.service`: `systemd-analyze verify` only reports the missing `postgresql.service` of the
  test container. Not run for real (needs the cp VM).
- Cleanup done: yes (`demo.sh down`).

### L9 - HTTP load test, failover under load, flood isolation  (2026-10-20, Emna's PC, Docker)
- Tool: `cstamctl load` (new, stdlib only): fixed total rate, checks each 200 is the requested
  team's page, per-second timeline, longest time without a successful answer. Run with
  `deploy/dev/demo.sh load [SECONDS] [WORKERS] [pause|kill]`, 20 teams.
- First run without a rate: every answer 429 (the per-Host limit, 2000 per 10 s, works; refused
  requests count, so a flooder stays blocked). The gateway answered ~95,000 req/s of 429s.
- Steady 3,000 req/s (150 per team): 0 errors, 0 misrouted, p99 1.6 ms (keep-alive), 4.5 ms (new
  connection each time), 1.2 ms with systemd gateways.
- Failover under load (0.3 s client timeout): frozen master 1.51 s x3 without any success; HAProxy
  killed 1.91 s (plain), 0.46 / 0.55 / 0.69 s (systemd restart, no failover). 0 misrouted. With a
  2 s client timeout the frozen case reads 2.0 s: requests hang on the frozen gateway until the
  timeout, so the client timeout bounds what a user sees.
- Flood: team01 at ~79,000 req/s for 20 s: 1,968 served, 1,584,829 refused (429); 19 other teams at
  2,850 req/s: 0 errors, p99 3.7 ms.
- Seen: right after the flood, team01 was still limited in the next run until a failover moved the
  traffic to the other gateway (rate counters are per gateway, no `peers`): written as a known gap.
- Found while writing security.md: `cpd` listened on 8080 but `cstam-cp` only opens 8443 to the
  agents, so on FelCloud the gateways could not have reached `cpd`. Fixed in code (no cloud change):
  `config.CPDPort = 8443` is the default of `cpd -listen` and `gwagent -cpd`, env examples and demo.
- New test `TestGatewayGuards` (real HAProxy): `/__gw/*` is a silent 404 from an outside source
  address, a flooded Host gets 429 while another team answers, and `A.HACKA.TEST:80` does not
  bypass the limit. `TestOpenAPIListsEveryRoute` keeps `docs/official/openapi.yaml` in step.
- Cleanup done: yes (`demo.sh down`).

### L10 - Planned failover through the API  (2026-10-20, Emna's PC, WSL Debian 13, Docker)
- Setup: `GW_MODE=systemd demo.sh up` (agent unprivileged as on the VMs), 2 teams, then
  `demo.sh failover planned`: `POST /v1/gateways/<master>/failover`, probe every 20 ms, failback.
- Result (twice): gw2 took over **304-310 ms** after the API call, **1 of ~175 probes** failed
  (200 ms probe timeout). Event log: `gateway.failover gw1`, `gateway.vrrp gw1 FAULT`,
  `gateway.vrrp gw2 MASTER`, `gateway.failback gw1`, `gateway.vrrp gw1 BACKUP` (nopreempt).
- Refusal: with gw2 forced down, `failover gw1` is refused (409 `no healthy peer gateway`).
- **Bug found (there since Task G)**: on systemd gateways cpd always saw `vrrp_state: ""`.
  keepalived runs the notify script with umask 077, so `/run/cstam/vrrp.state` was root 0600 and
  the unprivileged agent could not read it (the plain Docker lab runs the agent as root, so the
  tests missed it). Fix: `umask 022` in `vrrp-notify.sh`; MASTER/BACKUP now reach cpd.
- The force-backup file is created by keepalived as root (`init_file 0 overwrite`); the agent
  replaces it by rename (it owns `/run/cstam`) and keepalived picks the new file up.
- Cleanup done: no (demo stack kept running for the dashboard work).

### L11 - Config versions and manual rollback through cpd  (2026-10-20, Emna's PC, Docker)
- Setup: `GW_MODE=systemd demo.sh up`, 2 teams, `demo.sh config` (probe on team01 every 20 ms).
- Per gateway, from the `haproxy apply` log lines (both gateways within a few ms of each other):

  | Version | What | Outcome | check | reload | verify | rollback | total |
  |---|---|---|---|---|---|---|---|
  | c1 | rate limit 1000 | applied | 45 ms | 118 ms | 2 ms | - | 165 ms |
  | c2 | healthz answers 500 (valid syntax) | rolled_back | 43 ms | 119 ms | 500 ms (timeout) | **139 ms** | **0.80 s** |
  | c3 | unknown keyword | rejected | 23 ms | - | - | - | 23 ms |
  | c4 | rollback to c1 (copy) | applied | 39 ms | 117 ms | 2 ms | - | 158 ms |
- Team traffic during the whole sequence: **316 x 200, 0 errors**.
- Events: `config.created`, `config.applied/rolled_back/rejected` per gateway with the error,
  `config.rollback`; `/v1/status.alerts` says "gw1 did not take config c2 (rolled_back), still
  serves c1" until a newer version applies.
- Change on the way: `haproxy -c` errors now keep only the `[ALERT]` lines (the version banner
  filled the 300 characters stored per gateway).
- Cleanup done: no (demo stack kept running for the dashboard work).

### L12 - Live traffic log path  (2026-10-20, Emna's PC, Docker, systemd gateways)
- Path: HAProxy JSON log-format -> UDP syslog 127.0.0.1:5514 (`gwagent -traffic-log`, default on)
  -> agent buffer (2000 lines, oldest dropped and counted) -> `POST /v1/gateways/{gw}/traffic`
  every 1 s (<= 150 lines per batch, <= 10 batches per second) -> cpd ring (2000 lines) and
  per-second counters -> `GET /v1/traffic?after=`.
- 30 requests on 3 teams + one unknown host: all lines arrived within ~1 s with host, path, status,
  backend slot and timings; `/__gw/healthz` (keepalived check every second) is filtered out.
- `demo.sh load 12 32` (450 req/s, 5,400 requests, 0 errors): `/v1/traffic` reported 379 req/s
  over its 10 s window 9 s into the run (window still holds the ramp-up), 0 lines dropped.
- Cleanup done: no (demo stack kept running for the dashboard work).

### L13 - Dashboard in a real browser  (2026-10-20, Emna's PC, headless Edge over CDP)
- Setup: demo stack (systemd gateways, 4 teams), cpd serving `web/` at `/`; a local TCP forwarder
  127.0.0.1:18443 -> 10.20.0.1:8443 so the Windows browser reaches cpd in WSL; Edge headless
  driven over the DevTools protocol from PowerShell (the Chrome extension was not connected).
- Checked: login with the admin token, every tab renders, "live" pill green, no alert when
  healthy. Clicking **Planned failover** on gw1 + confirm: gw2 MASTER and first card within
  ~1 s, gw1 "FAULT, forced backup", red banner, timeline "planned failover of gw1 / gw1 FAULT /
  gw2 MASTER". Spin-up form: `jury01` ready on 10.20.0.36 (slot 5) ~1.5 s after submit.
  Teardown with the typed name: `team04` deleting at once, toast "teardown started".
- Bugs found and fixed before commit: `replaceChildren(null)` printed "null" on the Traffic tab
  (now a `put()` helper skips null); unbounded traffic table (now a scroll box); low contrast of
  white text on the light accent in dark mode.
- Cleanup done: demo stack still up; forwarder is a throwaway script outside the repo.

## Decisions log
| Date | Decision | Based on | Alternatives rejected |
|---|---|---|---|
| 2026-10-16 | Backend in Go, modular monolith + 2 agents | guide ch. 6 ADR-01 | microservices |
| 2026-10-17 | IaC = Pulumi in Go, shared state in FelCloud S3 bucket `cstam-pulumi-state` | team of several people | local state, Pulumi Cloud, Heat, Terraform |
| 2026-10-17 | Admin access: SSH/dashboard open to any IP, key-only, fail2ban on cp | teammates on changing networks | IP allowlist, WireGuard |
| 2026-10-17 | soft-anti-affinity for the gateway server group | D1 | strict anti-affinity (fails on FelCloud) |
| 2026-10-19 | IPAM allocation serialized by a transaction advisory lock | L3 (slot uniqueness from committed rows) | retry on unique violation |
| 2026-10-19 | Sweep frees nothing when gateways exist but none was seen in 5 min | ch. 17 safety rule | free on timeout only |
| 2026-10-19 | Local demo = plain `docker run` script, team sandboxes as containers (docker driver) | no floating IPs yet; saves the compose plugin download | docker compose, kind |
| 2026-10-19 | gwagent `-bootstrap -vrrp-iface` also renders keepalived.conf and the VRRP scripts | one source of truth for the IP plan (gw1 prio 150) | hand-written keepalived.conf in cloud-init |
| 2026-10-20 | Planned failover: cpd publishes `force_backup`, the agent writes keepalived's track file; refused without a healthy in-sync peer | guide ch. 21 section 12.6, L10 | SSH to the gateway, killing a process |
| 2026-10-20 | Dashboard in plain HTML/JS embedded in cpd, polling the REST API | no Node on the machines, metered network, one binary | React + Vite (guide ch. 21), htmx (extra download) |
