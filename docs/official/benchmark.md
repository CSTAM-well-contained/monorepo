# Benchmark report

All numbers are measured, not estimated. Each line names the command that reproduces it and the
lab notebook entry with the details ([experiments.md](../notebook/experiments.md)).

**Where:** a laptop (WSL2 Debian 13, Docker 29) running real HAProxy 3.2, keepalived 2.3,
PostgreSQL 18 and our binaries, on a Docker bridge with the FelCloud IP plan (10.20.0.0/24); and
FelCloud for the VM driver (B4). The full FelCloud run is pending floating IPs: on OVN, failover
adds the virtual-port rebinding time (expected 10-300 ms), to be measured there.

## Summary

| # | Metric | Result | Target |
|---|---|---|---|
| B1 | Requests dropped by hot reloads | **0 of 96,606** over 20 reloads; 0 of 40,667 in a later run | zero downtime |
| B1 | Broken config: detection + rollback | rejected by `haproxy -c` before touching HAProxy; a config that loads but fails verification is rolled back in **73-142 ms**, 0.72-0.97 s end to end | sub-second |
| B2 | New team routed after cpd publishes it | **24 ms** (runtime API, no reload) | |
| B3 | 1000 parallel allocations, 128 slots | 128 allocated, 872 clean `pool exhausted`, **no IP or slot given twice** | no conflicts |
| B4 | FelCloud VM sandbox create / delete | **23 s** to ACTIVE, **14 s** to delete server + port | |
| B5 | 50 simultaneous registrations | **50/50 routed, p50 3.4 s, p95 4.9 s**, 0 errors, 50 distinct IPs and slots | 10+ teams, error-free |
| B6 | Gateway failover (takeover through the VIP) | **1.3-1.9 s** frozen master, **1.5-2.3 s** HAProxy killed, 0.2 s clean stop | seamless |
| B7 | Teardown of 50 teams | sandboxes and routes gone in **7 s**; IPs back in the pool at **63 s** (60 s quarantine by design) | immediate release |

## B1 - Zero-downtime reload and rollback
`go test -tags integration -v -run 'TestBrokenConfigRollsBack|TestReloadDropsNoRequest' ./internal/gw/applier/`
(or `deploy/dev/demo.sh reload`). Notebook L1, L6.
- 20 reloads under continuous load: 96,606 requests, 0 failed. Slowest apply 356 ms.
- Broken config that passes `haproxy -c` but fails the post-reload verification: check 151 ms,
  reload 175 ms, verify timeout 501 ms, **rollback 142 ms**, total 0.97 s. (L1: rollback 73 ms,
  total 0.72 s.) A config with a syntax error never reaches HAProxy.

## B2 - Route propagation
`go test -tags integration -run TestAgentDrivesRealHAProxy -v ./internal/gw/agent/` (L5).
cpd publishes generation 2 with a new team; the team answers through HAProxy 24 ms later.

## B3 - IPAM under contention
`go test -race -tags integration -v ./internal/ipam/` (L3). 1000 concurrent "create team +
allocate" transactions on 128 slots: 128 allocated, 872 `ErrPoolExhausted`, no duplicate,
3.8 s total with the race detector. With 999 slots: the 159 pool IPs minus the reserved .68.

## B4 - OpenStack provisioning (FelCloud)
`OS_CLOUD=cstam go test -tags e2e -run TestE2ESandbox -v ./internal/openstack/` (L4).
One `G0.basic.1c1g` Debian 12 VM on a Neutron port with a fixed IP: ACTIVE in 23 s, server and
port deleted in 14 s. The instance quota (10 for the project, 3 used by the platform) limits a
FelCloud stress test to about 6 VM teams; more teams need the shared-host driver (Phase 2 design).

## B5 - Concurrent provisioning stress test
`CPD_WORKERS=16 deploy/dev/demo.sh up; HACK=s50 deploy/dev/demo.sh stress 50` (L7).
All registrations are sent at the same time; each team is "done" when its own page (team name
and IP) answers through the VIP.

| Teams | cpd workers | API answers (all) | Routed | p50 | p95 | max | Distinct IPs / slots |
|---|---|---|---|---|---|---|---|
| 20 | 4 | 0.32 s | 20/20 | 2.37 s | 4.02 s | 4.16 s | 20 / 20 |
| 20 | 16 | 0.40 s | 20/20 | 2.11 s | 2.42 s | 3.55 s | 20 / 20 |
| 50 | 16 | 0.51 s | 50/50 | 3.39 s | 4.93 s | 5.10 s | 50 / 50 |

The API answers every registration in under 0.6 s (IP and slot are allocated in that call); the
rest is sandbox creation, bounded by `-workers`. Route propagation itself is milliseconds (B2).

## B6 - Gateway failover
`deploy/dev/demo.sh failover pause|kill`: a probe requests a team through the VIP every 20 ms
(200 ms timeout) while the MASTER is broken (L2, L6).

| Failure | Takeover (6+ runs) | Mechanism |
|---|---|---|
| keepalived stopped cleanly | 0.20 s | priority-0 advert |
| HAProxy killed | 1.5, 1.57, 1.86, 2.31 s | `chk_gateway` fails twice (interval 1 s) |
| master frozen (`docker pause`) | 1.32, 1.62, 1.7, 1.85 s | 3 x 0.5 s + skew = 1.8 s in theory |

On the VMs, `vrrp_track_process` should bring the HAProxy-killed case under 1 s (cannot be measured
in containers).

## B7 - Teardown and IP recycling
`deploy/dev/demo.sh teardown` after B5 (L6, L7). 50 teams: routes removed at once, all sandboxes
deleted in 7 s; IPs stay in quarantine until both gateways acked the route removal and 60 s
passed, then all 50 are free at 63 s. The next hackathon received 10.20.0.32 again.
The 60 s quarantine is a deliberate safety margin (`ipam.Quarantine`, for ARP caches and
keep-alive connections); the gateway-ack condition alone is met within milliseconds.
