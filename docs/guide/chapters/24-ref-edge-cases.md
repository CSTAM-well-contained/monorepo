# 24. Reference A – Edge-case catalogue

This chapter lists every edge case we know about, in one place. Use it in three ways:

1. **While building:** before you call a step "done", check the rows that touch it.
2. **While testing:** each row names the test that proves it. A row without a passing test is an open risk.
3. **Before the pitch:** jury members love "what happens if...". Every answer is here.

Each row has four parts: the **scenario** (what happens), the **consequence** if we did nothing, the **prevention or detection** in our design, and the **test** that proves it. Test names refer to Go tests (`go test -run <Name>`), chaos scenarios (`CH-xx`, chapter 20) or security probes (`Sxx`, chapter 18).

::: note
**How to read "pod-NIC" and "AAP" in this chapter**

Some rows only apply to one team-network model. They are marked **[pod-NIC]** (the primary model: a real Neutron port hot-plugged into the host and moved into the team's network namespace) or **[AAP]** (the fallback: a reserved port plus an allowed-address-pair on the host port). Rows without a mark apply to both.
:::

## 24.1 IP allocation and recycling

| # | Scenario | Consequence if unhandled | Prevention / detection | Test |
|---|---|---|---|---|
| 1 | **The `edges.md` bug:** the port is deleted before the route, Neutron (or our IPAM) gives the IP to team B while gw1 still routes `team-a.…` to it | Team A's URL serves team B's app. A security bug with no crash and no log line | Strict teardown order (route first, port last); IP leaves QUARANTINE only when every member gateway acked a generation ≥ the removal generation; team pool is outside Neutron's auto pool; LRU reuse | `TestEdgesMdScenario`, `TestLifecycleProperties` (property S checked after every random action), probe S21 |
| 2 | Two concurrent registrations get the same IP | Two teams on one address, ARP conflicts, misrouting | `FOR UPDATE SKIP LOCKED` on one row per IP; partial unique index `lease_live_ip`; Neutron's own fixed-IP uniqueness as a third lock | `TestConcurrentLease` (100 goroutines → 100 distinct IPs, `-race -count=10`) |
| 3 | Pool exhausted (160 IPs leased or in quarantine) | 500 errors, half-built teams | `ErrPoolExhausted` → `503` with `Retry-After` = seconds until the next quarantine ends; admission check against `max_teams`; metric `cstam_ipam_free` | `TestLeaseExhaustion` |
| 4 | The IP we lease is already held by a port that is not ours (someone ran `openstack port create --fixed-ip`) | Provisioning loops on 409 forever | 409 `IpAddressAlreadyAllocated` → look the port up by IP; not tagged ours → mark `FOREIGN`, lease another IP (max 3 tries) | fake Neutron "foreign port" fault; chaos "orphan + foreign port" |
| 5 | `10.20.0.68` (the existing `sbx-f0699704` VM) sits inside our team range | Our IPAM hands out a live address | Seeded as `RESERVED` in the migration; the reconciler also detects any foreign port in the range | seed check in `TestMigrations`; `SELECT state FROM ip_addresses WHERE ip='10.20.0.68'` |
| 6 | A `FOREIGN` IP becomes unused again | The pool shrinks forever | Reconciler: if two consecutive scans show no port with that IP → `FREE` | `TestReconcileDiff` (row "foreign no longer used") |
| 7 | Pool range edited by migration while IPs are leased | IPs vanish from management, rows orphaned | Migrations may only shrink a pool over `FREE`/`RESERVED` rows (checked in the migration itself); `EXCLUDE` constraint prevents overlapping pools | migration test |
| 8 | Stale ARP cache / conntrack entry for a reused IP | The new owner gets a 30 s black hole, or old flows reach the old host | Gateway agent runs `ip neigh del <ip>` on removal; host agent flushes conntrack on teardown; new owner sends GARP **[AAP]** / OVN binds a real port **[pod-NIC]**; 60 s quarantine; LRU reuse | measure "first request after reuse" in Step 11 |
| 9 | Warm-pool port leaks (created, attached, never used, then the hackathon ends) **[pod-NIC]** | Ports and IPs held forever | Warm ports are tagged `cstam-warm` and belong to a hackathon; hackathon close drains the pool through the normal teardown; reconciler counts warm ports vs target | `TestWarmPoolDrain` (Step 7 tests) |
| 10 | Warm pool empty during a burst of registrations **[pod-NIC]** | Registrations wait for a cold attach (2–6 s each, serialized per VM by Nova) | Refill worker keeps `warm_target` per host; cold path still works, only slower; the dashboard shows pool level | stress test at concurrency > warm target (chapter 20) |

## 24.2 Provisioning under failures

| # | Scenario | Consequence if unhandled | Prevention / detection | Test |
|---|---|---|---|---|
| 11 | Two concurrent registrations with the **same team name** | Two teams for one subdomain | Partial unique indexes `team_slug_live`, `team_fqdn_live` → SQLSTATE `23505` → `409` | 20 goroutines POST the same slug: exactly 1 × `202`, 19 × `409` |
| 12 | Client retries `POST /teams` after its own timeout | Duplicate team | `Idempotency-Key`: same key + same body → original response; same key + different body → `422` | replay test in Step 7 |
| 13 | `cpd` crashes right after the lease commit, before the port exists | IP stuck in LEASED, team stuck | Claim expires after 30 s → another worker (or the restarted process) resumes; PENDING deadline (120 s) is the backstop | crash hook after step 0; `systemctl kill -s KILL cstam-cp` during `bench -n 20` |
| 14 | Neutron **creates the port but the HTTP call times out** | A retry creates a second port, or leaks one | The fixed IP is the idempotency key: retry → 409 → lookup by IP → tag matches our lease → adopt | fake Neutron `TimeoutAfterCommit` |
| 15 | Neutron returns 5xx / is slow | Teams stuck or error storms | Retry with full jitter (3 tries at call site), then work-queue backoff; global limiter (10 rps, 8 in flight); condition `PortReady=False reason=NeutronUnavailable` | fake Neutron `Err500`, `Latency` |
| 16 | Nova interface attach is slow or fails **[pod-NIC]** | Provisioning latency explodes, or a port stays half-attached | Warm pool absorbs bursts; attach is idempotent (port `device_id` checked first); timeout → detach + retry; "no more PCI slots" → host marked full, fatal-for-team on that host | H1 experiment; fake Nova fault |
| 17 | Concurrent updates to the host port's `allowed_address_pairs` **[AAP]** | Lost update: one team's IP silently dropped, "1 in 20 teams unreachable" | One AAP syncer per host computes the full list from the DB and writes it with `If-Match: revision_number`; 412 → re-read and retry; coalesces many teams into one PUT | `TestAAPSyncerConcurrent` (fake enforces revisions, 50 teams on one host); E4 experiment |
| 18 | AAP limit (default 10 per port) reached **[AAP]** | The 11th team on a host fails | H2 experiment tells the real limit; capacity per host ≤ limit; or CIDR mode (/27 per host); error classified fatal-for-team on that host | fake returns the limit error |
| 19 | Host agent down during provisioning | Team stuck in PROVISIONING | Backoff within the PENDING deadline, then teardown → FAILED; host marked `DOWN` after 3 missed heartbeats, scheduler skips it | stop the fake agent; `CH-04` |
| 20 | Health check fails because the team has not deployed an app yet | Team never becomes ACTIVE | Health probe targets the pod's own health endpoint (`/.cstam/health` served by a tiny responder) or a TCP check on the netns, not the team's app | fresh team without app reaches ACTIVE |
| 21 | Admin deletes a team while it is still provisioning | Half-built leftovers | `desired=ABSENT`; the worker switches to the teardown pipeline at the next step boundary; every teardown step is idempotent | property test action "delete at random step" |
| 22 | Two workers on one team (a claim expired during a GC pause) | Double side effects, conflicting writes | Fencing: every write carries `claim_epoch`; a stale worker gets 0 rows and abandons; external steps are idempotent | pause a worker past its claim TTL |
| 23 | Keystone token expires in the middle of a run | 401 storm | gophercloud re-auth (`AllowReauth`); 401 **after** re-auth = fatal-for-system → stop workers, alert | short-TTL token in a staging test |
| 24 | Neutron quota exceeded (ports, SG rules) | Infinite retries | `OverQuota` classified fatal-for-team; `cstamctl doctor` checks the budget before an event | fake returns `OverQuota` |
| 25 | Malicious slug (`a;b`, newline, `../`, Unicode look-alike) | Config injection into HAProxy maps, nft or Docker names | `slug` domain regex in the DB, the same regex in the API, and in the agents before rendering | `go test -fuzz=FuzzSlug`; probe S19 |

## 24.3 Teardown, gateways and generations

| # | Scenario | Consequence if unhandled | Prevention / detection | Test |
|---|---|---|---|---|
| 26 | **Backup gateway down during teardown** | gw2 keeps the old route; if gw2 later takes the VIP after the IP was reused → case 1 | IP stays in QUARANTINE until gw2 acks ≥ Gr or is fenced (VIP pair removed + port disabled); dashboard shows "blocked by gw2 ack" | `TestTeardownWithBackupDown`; chaos "kill gw2 during mass teardown" |
| 27 | Generation commit-order inversion (tx with gen 6 commits before tx with gen 5) | A gateway acks 6 while the gen-5 removal is not yet visible → false safety | Single-row `config_head` bumped with `UPDATE … RETURNING`; its row lock serializes commits | `TestGenerationCommitOrder` |
| 28 | Late or out-of-order ack (old HTTP response arrives after a newer one) | Ack goes backwards, IPs blocked or wrongly freed | `acked_generation = GREATEST(...)`; the agent never applies a lower generation | `TestAckNeverGoesBack` |
| 29 | Agent bug acks a generation it did not really apply | False safety | Ack carries the sha256 digest of the live map; `cpd` rejects a digest that does not match `config_versions` | unit test with a wrong digest |
| 30 | Gateway restored from an old image / old disk | Serves an old map that includes routes to reused IPs | Boot rule: `/run/cstam/ready` missing → `chk_gateway` fails → keepalived stays in FAULT until the agent synced to head | start the agent with an old map file, assert `eligible=0` |
| 31 | VRRP split brain (both MASTER) | OVN binding flaps with every GARP; traffic jumps between gateways | Unicast VRRP, SG `cstam-vrrp`, `track_interface`; both gateways have acked the same generation, so routes are identical (safe); the control plane sees two MASTERs > 2 s and forces the lower one to FAULT via `force-backup` | `CH-05` (drop proto 112 on gw2) |
| 32 | Manual config rollback while routes changed meanwhile | Deleted routes come back ("resurrection") | Rollback restores only the structural HAProxy template; routes always come from the current table | `TestNoResurrectOnRollback` |
| 33 | WebSocket or SSE connection stays open to a team after its removal | Data keeps flowing to a deleted team's backend | Gateway agent runs `shutdown sessions server <slot>/s` before acking; `timeout tunnel` caps the rest | open a WebSocket, delete the team, assert it closes |
| 34 | 50 teams torn down at hackathon end | 50 generations, 50 map rewrites, reload storm | Publisher batches route changes within a 50 ms window into one generation; Neutron semaphore protects the API | measure generations created per teardown wave |
| 35 | Host agent down during teardown | The team IP stays bound on the host ("zombie") | **[AAP]** AAP removal fences the host at the Neutron level. **[pod-NIC]** detaching the port removes the NIC from the VM. Either way the agent purges leftovers when it returns (desired state has no team) | teardown with agent stopped, then started: agent ends with no team |
| 36 | Team's port deleted by a human while the team is ACTIVE | Team unreachable; IP free in Neutron | Drift scan sees "lease ACTIVE, no port" → recreate with the same fixed IP and re-attach **[pod-NIC]** / re-add AAP **[AAP]** | chaos: delete the port by hand, it returns within one scan |
| 37 | Neutron says "deleted" but OVN northbound lags | IP still answered briefly | `neutron_absent_at` is set from a fresh GET; plus quarantine time | none |

## 24.4 Hosts, control plane and data

| # | Scenario | Consequence if unhandled | Prevention / detection | Test |
|---|---|---|---|---|
| 38 | Host VM reboots | **[pod-NIC]** NICs come back in the root netns, unconfigured; **[AAP]** IP bindings, NAT and rules are lost | Agent starts, pulls desired state, re-plumbs every team (idempotent), restarts pods; Docker `live-restore` keeps containers across a `dockerd` restart | `openstack server reboot --hard cstam-host-a` → all teams HEALTHY ≤ 90 s with no control-plane action |
| 39 | A team's pause container restarts **[pod-NIC]** | Its netns is new; the NIC fell back to the root netns | Agent watches Docker events for `*-pod` containers and re-runs the plumbing | `docker restart team1-pod` → team reachable again in < 5 s |
| 40 | Host VM deleted / rebuilt | Every team on it is dead | Host `RETIRED`; teams `DEGRADED`; admin chooses re-provision on another host or teardown. Optional differentiator: re-home with the same IPs | manual drill |
| 41 | Control plane (`cpd`) dies | No new registrations, no teardown, no dashboard | Data plane is statically stable: gateways and hosts keep last-good state; failover and local rollback still work; on restart, resync continues every unfinished team | `CH-06`: stop `cpd`, 0 failed probes for 5 min |
| 42 | Database restored from an old backup | Everything looks orphaned; reconciler could delete live ports | Circuit breaker: if a scan would delete > max(5, 10 %) of our ports, it deletes nothing and alerts; orphans need two scans | restore an old dump in staging, run reconciler in dry-run |
| 43 | Leader loses its DB session mid-cycle | Two leaders for a second | Leadership is an optimization only: every leader action is a guarded SQL update or an idempotent, tag-checked Neutron call | `pg_terminate_backend` on the leader connection |
| 44 | Clock skew between VMs | Early or late expiry | All TTLs use Postgres `now()` only; Go uses monotonic time only for timeouts | fake clock in the agent has no effect |
| 45 | Renew races with expiry | A deleted team comes back | Renew SQL requires `expires_at > now()` and `desired='PRESENT'` in the same UPDATE | race test |
| 46 | Teardown requested twice (TTL expiry + admin delete at the same time) | Double release | `BeginRelease` is idempotent; `desired` already ABSENT | parallel DELETE test |
| 47 | Both gateways down | Total public outage | Out of scope for safety (nothing routes, so nothing misroutes); anti-affinity reduces the risk; alert; fast rebuild from scripts | chaos (documented, not demoed) |
| 48 | Team container tries to reach metadata `169.254.169.254` | It reads the **host VM's** user-data and keys | nft output drop in the team netns **[pod-NIC]** / `CSTAM-TEAM` chain **[AAP]**; no secrets in user-data anyway | probe S06 |
| 49 | Team traffic with MTU 1500 over the 1442 network | Large responses and `git clone` hang | Team NIC MTU 1442 **[pod-NIC]**; Docker `mtu 1442` + MSS clamping **[AAP]** | E5; 100 MB download from inside a team container |
| 50 | Custom domain (bonus) already used by another team | Domain hijack | `team_fqdn_live` unique index covers custom domains too; ownership verified by TXT/CNAME before routing | insert duplicate → `23505` |

## 24.5 Weaknesses of design v1 and how this guide closes them

The critical review (chapter 4) found weaknesses in the first design. This table shows where each one is closed.

| Id | Weakness | Closed by | Where |
|---|---|---|---|
| W1 | Control plane is a single VM | Static stability + pg_dump to Swift + restore drill; optional second `cpd` with leader election | Steps 4, 5, 10; chapter 25 |
| W2 | Backup gateway can hold stale config | Agents pull full snapshots by generation; ack with digest; boot readiness gate | Step 4, Step 8 |
| W3 | An unreachable gateway blocks or breaks IP release | Ack gate + fence/rejoin procedure | Step 8; runbook 25.4 |
| W4 | IPAM pool overlapped Neutron's auto allocations | Team pool .32–.191, outside .200–.250; FOREIGN detection | Step 6 |
| W5 | Lost updates on `allowed_address_pairs` | pod-NIC removes the problem; AAP fallback uses a per-host syncer with `If-Match` | Steps 5, 7 |
| W6 | OVN behaviour of unbound team ports unverified | Experiments E2/H1 and gate G1; pod-NIC uses bound ports | Step 1 |
| W7 | MTU 1442 not handled | NIC/Docker MTU 1442, MSS clamp, E5 | Steps 1, 5 |
| W8 | Host VM is a SPOF for its hackathon | Maintenance page bonus; reboot restore; optional re-home | Steps 5, 13 |
| W9 | Runtime API changes lost on reload | Files on disk always written first; runtime is only the fast path | Steps 3, 4 |
| W10 | Wildcard DNS depends on FelCloud | Three tiers: delegation, wildcard record, sslip.io fallback | Steps 0, 1, 13 |
| W11 | TLS could burn FelCloud's Let's Encrypt quota | Per-hackathon wildcard via DNS-01; staging endpoint during tests | Step 13 |
| W12 | Quotas not budgeted | Quota budget table + `cstamctl doctor` | Step 0, Step 2 |
| W13 | Agent protocol and security unclear | Agents dial out over mTLS; no inbound agent port | Steps 4, 6, 9 |
| W14 | Old plan used Python tools | Whole stack in Go | Chapter 7 |
| W15 | Live demo is fragile | `make demo`, preflight checklist, backup clips per step | Step 14 |
| W16 | "Short leases" were only a TTL | PENDING deadline, ACTIVE TTL, renew, expiry sweeper | Step 6, Step 8 |
| W17 | Team egress uncontrolled | Egress policy and rate limits; bandwidth bonus | Steps 9, 13 |

::: learn
**Learning checkpoint**

1. Pick three rows from 24.1–24.3 and explain each one aloud without reading: scenario, consequence, prevention.
2. Which two rows would still be dangerous if the generation acks were removed and only the 60 s quarantine remained?
3. Why does row 14 ("created but timed out") not create a duplicate port, step by step?
4. Which rows exist only in the AAP model, and why does pod-NIC remove them?
5. For row 42, why is "delete nothing and alert" safer than "delete what looks orphaned"?
:::
