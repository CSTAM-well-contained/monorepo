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

### D3 - Router lost its public IP  (2026-10-20 00:10, Emna's PC)
- `pulumi preview --refresh`: `cstam-router` no longer has an external gateway. Its public IP
  197.5.133.180 (set by Pulumi on 2026-10-17) was removed outside our code (no commit, no Pulumi
  run). `private_router` (not ours) lost its gateway too: most likely FelCloud reclaimed router IPs
  to refill the exhausted pool.
- Effect: the VMs have **no outbound internet** any more (no SNAT), on top of no inbound.
- `pulumi up` retried: both floating IPs still `ExternalIpAddressExhausted`. Re-adding the router
  gateway was not forced: it needs a pool IP too and could fight FelCloud's cleanup.
- FelCloud console checked through the browser (dashboard.felcloud.io): quota 0/50 floating IPs,
  3/10 instances, one public network only (`INTERNET`, 197.5.133.0/24, pool .1-.230). No other way in.
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
