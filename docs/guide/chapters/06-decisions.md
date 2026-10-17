# 6. Architecture decision records

An architecture decision record (ADR) captures one decision: the context, the options, the choice and its consequences. Each ADR below follows the same structure, so you can find the "why" quickly when a judge asks. The comparison tables use these criteria:

- **Performance**: latency and throughput for our scale (≤ 50 teams, ≤ thousands of requests per second).
- **Complexity**: how much code and how many moving parts you must understand and debug.
- **FelCloud risk**: the chance that it does not work on this cloud (OVN, quotas, missing services).
- **Demo value**: how clearly the jury can see it working and see that it is your work.
- **Score impact**: effect on the points it serves.

| # | Decision | Choice |
|---|---|---|
| ADR-01 | Deployables | Modular monolith control plane + two agents + CLI |
| ADR-02 | Agent communication | Connect-RPC over mTLS, agents dial out and watch full desired state |
| ADR-03 | Event bus | None besides Postgres `LISTEN/NOTIFY` |
| ADR-04 | Orchestration | Desired-state controller with claims and fencing |
| ADR-05 | IPAM allocator | Own pool table + `SKIP LOCKED`, Neutron as second lock |
| ADR-06 | Generations and acknowledgements | Commit-ordered generation counter, digest-checked acks gate IP reuse |
| ADR-07 | HA mechanism | keepalived VRRP; floating-IP move and OVN load balancer as fallbacks |
| ADR-08 | Active/Standby vs Active/Active | Active/Standby, `nopreempt` |
| ADR-09 | Reverse proxy | HAProxy |
| ADR-10 | HAProxy routing strategy | Slot backends + host map |
| ADR-11 | Reload vs runtime API | Runtime for routes, validated hitless reload for structure, files always first |
| ADR-12 | Team runtime | Docker containers on a host VM per hackathon |
| ADR-13 | Team networking | pod-NIC primary, AAP fallback (gate G1) |
| ADR-14 | Firewall tool | nftables inside team namespaces; iptables `DOCKER-USER` chains in AAP mode |
| ADR-15 | Container hardening | Hardened runc by default; gVisor optional |
| ADR-16 | Secrets, OpenStack credential, agent PKI | Restricted application credential; own small Go CA |
| ADR-17 | Admin authentication | argon2id password + server-side sessions, two roles |
| ADR-18 | Dashboard technology and realtime | React + Vite + TypeScript + Tailwind, SSE |
| ADR-19 | DNS and TLS | Tiered: delegated zone → wildcard → sslip.io; DNS-01 wildcard per hackathon |
| ADR-20 | Deployment | Static binaries + systemd, cloud-init without secrets |
| ADR-21 | Control-plane HA | Static stability + backups; active/standby `cpd` demonstrated |

---

## ADR-01 Deployables: microservices or not

**Context.** Code must physically run in three places: on each gateway (only a local process can run `haproxy -c`, talk to the local admin socket and roll back without a network round-trip), on each host (only a local process can create namespaces, Docker networks and firewall rules), and somewhere central (database, IPAM, OpenStack credentials, API, dashboard). That already makes a distributed system. The real question is whether the central part should itself be split.

| | Plain monolith | **Modular monolith** | Microservices (api, ipam, provisioner, gateway-svc, log-svc + bus) |
|---|---|---|---|
| Deployables | 1 | 1 binary, roles chosen by flag | 4–6 plus a message bus |
| Internal boundaries | Packages, often leaky | Packages with explicit interfaces; `internal/` and lint rules prevent cross-imports | Network APIs |
| Consistency (lease IP + team + route) | One transaction | **One transaction** | Distributed saga; double allocation possible on partial failure |
| Latency per internal call | ns | ns | ms + serialization + retries |
| Failure modes | Process dies | Process dies | Every hop can time out |
| Fits 10-VM quota | Yes | Yes | Needs more RAM/VMs plus a broker |
| Demo/jury perception | "Messy?" | "They know where the boundaries are and why" | "Resume-driven; where is the IPAM transaction?" |
| **Verdict** | Weak structure | **Chosen** | Rejected |

**Decision.** **A modular monolith control plane `cpd` (roles `api` and `controller`), plus `gwagent`, `hostagent` and the `cstamctl` CLI.** IPAM is a package used inside transactions, not a service.

**Consequences.** The core invariant ("one IP belongs to one team, and a route points at it only while the lease is active") is enforced by database transactions and constraints. Several `cpd` instances can run (stateless API; one leader for sweepers). A package can be extracted later because its interface already exists. Pitch line: *"We have three deployable components because the code must run in three places. Inside the control plane, our key invariant needs a database transaction, so we did not build a distributed transaction to protect it."* (Kubernetes does the same: `kube-controller-manager` is one binary with about 30 controllers.)

## ADR-02 Agent communication

**Context.** The control plane must tell agents what to do (routes, configs, team specs, manual failover) and learn what they did (acknowledgements, VRRP state, health, logs). Agents restart, hosts reboot, the control plane can be down.

Wire protocol:

| | REST/JSON hand-written | gRPC (grpc-go) | **Connect-RPC** (`connectrpc.com/connect`) |
|---|---|---|---|
| Schema / codegen | Optional | `.proto` mandatory | `.proto` via `buf` (lint + breaking-change check) |
| Transport | HTTP/1.1 | HTTP/2 only, own stack | Standard `net/http`, HTTP/1.1 and 2; speaks Connect, gRPC and gRPC-Web |
| Debug with `curl` | Yes | No | **Yes** (JSON mode) |
| Streaming | SSE by hand | Yes | Yes |
| **Verdict** | No contract | Heavier, harder to debug | **Chosen** |

Direction:

| | Push (control plane calls agents) | **Dial-out watch (agents connect to the control plane)** |
|---|---|---|
| Inbound port on gateways/hosts | Yes (attack surface, SG rule) | **None** |
| Agent rebooted while control plane down | Stays empty until pushed | Restores last-good from disk, then syncs when the control plane returns |
| Missed message | Lost unless retried | Impossible: the agent always fetches the **full snapshot** for the latest generation |
| Latency | One RPC | Stream notification (ms) |
| **Verdict** | | **Chosen** |

**Decision.** **Connect-RPC over mutual TLS. Agents dial out** to `cpd`'s agent endpoint (:8443) and call `Watch(after_generation)` (a server stream that sends a new full snapshot whenever the generation changes), `Ack(generation, digest, result, timings)`, `Report(status)` every 2 s, and `PushTrafficLogs` (a client stream). Everything an agent must do is part of its desired state, including manual failover (a `force_backup` flag) and rollback (a structural config version).

**Consequences.** Level-triggered and idempotent by construction: applying snapshot 42 twice is harmless, and a lost notification is fixed by the next one. Hosts and gateways expose no management port besides SSH from the control plane. The public API stays REST (ADR-18 and chapter 7).

## ADR-03 Event bus

**Context.** API writes must wake controller workers; state changes must reach dashboards (SSE) and gateway watchers.

| | In-process channels | **Postgres `LISTEN/NOTIFY`** | NATS JetStream | Redis Streams |
|---|---|---|---|---|
| Works across 2 `cpd` processes | No | Yes | Yes | Yes |
| Delivered only if the transaction commits | n/a | **Yes** | No (dual write → needs an outbox) | No |
| Extra daemon / VM | No | No | Yes | Yes |
| Durability | No | No (it is only a hint; data is in tables) | Yes | Yes |
| **Verdict** | Too weak | **Chosen** | Not needed | Not needed |

**Decision.** **No broker.** `pg_notify` in the same transaction as the state change, plus a 1-second poll as a safety net.

**Consequences.** No dual-write problem: if the process dies after writing the row, there is no "event published but row missing" state. Payloads are tiny (IDs only); consumers read tables.

## ADR-04 Orchestration of provisioning and teardown

**Context.** Provisioning touches Postgres, Neutron (and Nova in pod-NIC mode), a host agent and two gateway agents. Teardown must run in a strict order. Any process can crash between two steps; a Neutron call can time out after the port was actually created.

| | Hand-written saga + job queue | Temporal | River (Postgres job queue) | **Desired-state controller (claims + fencing)** |
|---|---|---|---|---|
| Idempotency | Design every step and every compensation | Activities must be idempotent anyway | Same as saga | Built in: each step = "is it done? if not, make it so" |
| Crash mid-way | Resume from saved step; compensation may crash too | Excellent (replay) | Resume job | Excellent: next claim re-runs steps; done steps are skipped |
| Drift (port deleted by hand, host reboot) | Separate repair code | Separate | Separate | **Same code path** |
| Teardown order | Separate pipeline | Separate workflow | Separate | Same mechanism; teardown **is** the compensation |
| Extra infrastructure | None | Temporal server + DB (~2 GB RAM, a VM) | None | None |
| Pitfall | Compensations rarely run, so rarely tested | Determinism rules, versioning | Unique-job states can drop a change that arrives while a job runs | One new idea to learn: level-triggered |
| Demo value | "We undo in reverse order" | "We use Temporal" | – | "Kill the control plane mid-provisioning; restart; the team finishes" |
| **Verdict** | | Overkill | Optional for one-shot jobs only | **Chosen** |

**Decision.** **Teams carry `desired` (PRESENT/ABSENT) and an observed `phase`.** Workers claim teams with `UPDATE … WHERE id IN (SELECT … FOR UPDATE SKIP LOCKED)`, which increments a **fencing epoch**; every write a worker makes includes `claim_epoch = $epoch`, so a stalled worker that lost its claim cannot write anymore. Each pipeline is an ordered list of `Step{Name, Done, Apply}`. A failed provisioning sets `desired = ABSENT`, and the teardown pipeline cleans up.

**Consequences.** One mechanism handles crashes, retries, manual teardown during provisioning, and drift. The `operations` table logs every step with its duration (for the dashboard and the benchmark breakdown). Leader election (a Postgres advisory lock on a dedicated connection) is used only for sweepers; safety never depends on it.

## ADR-05 IPAM allocator

**Context.** 10–50 concurrent registrations must never get the same IP. A released IP must not be reused while anything still references it (`edges.md`). Neutron auto-allocates only from .200–.250.

| | Neutron picks (`port create` without IP) | **Own pool table + `FOR UPDATE SKIP LOCKED`** | Postgres advisory locks | Allocation-pool split |
|---|---|---|---|---|
| Correct under 50 parallel requests | Yes | Yes, without blocking | Yes, hand-built | Not an allocator: a partition |
| Quarantine / no early reuse | **Impossible**: Neutron reuses a freed IP at once (the `edges.md` bug) | Yes | Yes | Needed so Neutron never auto-allocates our range |
| Visible lifecycle for the dashboard | No | Yes, one row per IP | Partial | – |
| Complexity | Lowest | Moderate | Higher (key encoding, pooler pitfalls) | Already done on FelCloud |
| Score (IPAM + lifecycle, 20 pts) | Weak: "OpenStack does it" | Strong | Same, less clean | Supports the chosen option |
| **Verdict** | Rejected | **Chosen** | Leader election only | **Chosen (already in place)** |

**Decision.** **One row per usable IP (.32–.191)**. Allocation picks the least recently released `FREE` row with `FOR UPDATE SKIP LOCKED` inside the claim transaction. States: `FREE → LEASED → ACTIVE → RELEASING → QUARANTINE → FREE`, plus `RESERVED` (infrastructure, the existing .68), `FOREIGN` (Neutron says someone else holds it) and `WARM` (pre-attached pod-NIC ports). Neutron's per-subnet fixed-IP uniqueness is a second, independent lock: a duplicate create fails with 409 instead of silently choosing another IP.

**Consequences.** Partial unique indexes make illegal states unrepresentable (one live lease per IP and per team). Leases have a provisioning deadline (120 s) and a TTL (capped at the hackathon end); renewal cannot resurrect an expired lease. All times come from the database clock, never from Go processes or agents.

::: why
**Why `SKIP LOCKED`**

With plain `FOR UPDATE`, fifty transactions all try to lock the same "first free" row; forty-nine wait, then re-check and lose. With `SKIP LOCKED`, each transaction skips rows locked by others and takes the next free one. Fifty registrations proceed in parallel and each gets a different IP.
:::

## ADR-06 Generations and acknowledgements

**Context.** Teardown removes a route on **two** gateways. The backup may be down or slow. If an IP were reused while any gateway that can take the VIP still routes the old hostname to it, the old team's URL would serve the new team's app.

| | Push to master, then to backup (v1) | Time-based quarantine only | **Generation numbers + digest-checked acks gate reuse** |
|---|---|---|---|
| Backup down during teardown | Comes back stale; a later failover serves old routes | Unsafe if the outage is longer than the quarantine | IP stays in quarantine until the backup acks or is fenced |
| Proof of what is loaded | None | None | Digest of the actual map |
| Rebooted gateway with old map | Can take the VIP | Can take the VIP | **Not eligible for VRRP until synced** |
| **Verdict** | Unsafe | Unsafe | **Chosen** |

**Decision.** A single-row `config_head` table holds the global generation, bumped with `UPDATE … RETURNING` in the same transaction as each route change; the row lock means generation n+1 can never commit before n. Routes record `added_generation` and `removed_generation`. Gateway agents acknowledge `(generation, sha256 of the sorted map)`; the control plane rejects a digest that does not match. An IP leaves quarantine only when the quarantine has elapsed **and** `min(acked_generation of member gateways) ≥ removal generation` **and** Neutron shows no port with that IP. A long-dead gateway can be **fenced** (VIP pair removed from its port, port disabled, removed from membership) and later **rejoined**.

**Consequences.** A formal safety argument exists (Step 8). The dashboard shows each gateway's acknowledged generation, which makes "both gateways are in sync" visible. Many teardowns at once share one generation (a publisher batches changes within 50 ms).

## ADR-07 HA mechanism for the gateways

**Context.** The brief requires dual redundant gateways with instant failover, demonstrated by crashing the primary live.

| | **keepalived VRRP + VIP on OVN virtual port** | F1: keepalived notify moves the floating IP through Neutron | F2: Octavia `ovn` L4 load balancer in front of both HAProxys | Octavia amphora Active-Standby | Two floating IPs + DNS |
|---|---|---|---|---|---|
| Failover (estimate) | HAProxy killed < 1 s; VM crash ≈ 1.8 s + OVN rebind | 1.5–4 s (+ Neutron API call) | 1–3 s (health monitor) | Seconds; creation takes minutes | Minutes |
| Our own visible work | High: `keepalived.conf`, kill gw1 live | High | Medium (keepalived not needed) | Black box | None |
| FelCloud risk | Floating IP on virtual port (test E1) | Gateway needs a restricted credential | HM support in OVN provider (test E7) | Admin-only failover, no Barbican | – |
| Matches brief wording | "Active/Standby", "primary crash" | Yes | "Active/Active" | Yes | No |
| Effort | Low | ~1 hour on top | Low–medium | Medium | Trivial |
| **Verdict** | **Primary** | **Built anyway as safety net** | **Plan B** | Rejected | Rejected |

**Decision.** **keepalived VRRPv3, unicast, `advert_int 0.5`, VIP 10.20.0.10 on an unbound port listed in both gateway ports' allowed-address-pairs (exact IP, not CIDR), floating IP on the VIP port.** The VIP port is created first, then the gateway ports that reference it.

**Consequences.** OVN moves the VIP when it sees a gratuitous ARP from the new master, so keepalived's GARP settings matter (`vrrp_garp_master_delay`, `repeat`, `refresh`) and the notify script sends an extra `arping -U`. Gateways use `arp_ignore=1` and `arp_announce=2`. The decision is confirmed or reversed at gate G1.

## ADR-08 Active/Standby or Active/Active

See chapter 4, section 4.4.

**Decision.** **Active/Standby.** Both gateways start as `BACKUP` with `nopreempt` and priorities 150/100: the first elected master keeps the VIP, so a returning gateway does not take it back and cause a second outage. Manual failover uses a tracked file (`/run/cstam/force-backup`) that puts the master into FAULT. Split-brain (both MASTER) is detected by the control plane from agent reports and resolved by forcing one node into FAULT.

**Consequences.** One gateway carries all traffic (capacity is far above need). The dashboard shows "Primary vs Standby" exactly as the brief describes. Optional later: the OVN load balancer "mode switch" to show active/active.

## ADR-09 Reverse proxy

| | **HAProxy** | Nginx (open source) | Traefik | Envoy |
|---|---|---|---|---|
| Config validation | `haproxy -c` | `nginx -t` | No real test step; bad dynamic config is silently skipped | Through a control plane |
| Change routes without reload | Runtime API: maps, `set server addr` | Only in paid Plus | File/HTTP provider | xDS |
| Hitless reload | master-worker, socket passing | Graceful reload | n/a | Hot restart |
| Demonstrable rollback | Explicit | Explicit | Hard to show | Complex |
| Per-team rate limit (bonus) | Stick tables, very strong | `limit_req` | Middleware | Yes |
| Health checks per backend + maintenance page | Yes | Passive only (OSS) | Yes | Yes |
| **Verdict** | **Chosen** | Good fallback | Rejected | Overkill |

**Decision.** **HAProxy 3.2 LTS** from `ppa:vbernat/haproxy-3.2` (the Ubuntu 24.04 package, 2.8 LTS, also works).

## ADR-10 HAProxy routing strategy

**Context.** Adding or removing a team must not require a reload, but we still want per-team health checks, a maintenance page, statistics and rate limits.

| Strategy | Team add/remove | Health checks per team | Maintenance page | Per-team stats / limits | Reloads |
|---|---|---|---|---|---|
| One backend per team, rendered into the config | Reload | Yes | Yes | Yes | **Every change** |
| Map host → `ip:port` + one generic backend using `set-dst` | `add map` | **No** | Only on connect error | By host key only | None |
| `server-template` slots in one backend | Runtime | Yes | Partly | Per server | None, but host → server needs static `use-server` rules |
| **Pre-declared slot backends `be_slot001…128` (one server each) + map host → slot** | Runtime (`set server addr`, `add map`) | **Yes** | **Yes** | **Yes** | None (a reload only to resize the pool) |

**Decision.** **Slot backends.** `be_slot000` is a canary served by the gateway agent; `be_slot001` to `be_slot128` are pre-declared, each with one server parked at a placeholder address in `disabled` state. `hosts.map` maps each hostname to a slot name; unknown hosts go to `be_404`. One slot per exposed app (a team with a web app and an API uses two slots).

**Consequences.** Team changes never reload HAProxy. Order matters: on add, the server is set and enabled **before** the map entry; on remove, the map entry goes first, then sessions are shut down, then the slot is parked. Resizing the slot pool is the natural "structural reload" demo.

## ADR-11 Reload vs runtime API

**Context.** Runtime API changes live only in memory; a later reload re-reads files and would silently drop them.

| | Reload for every change | Runtime only | **Files first, runtime as the fast path** |
|---|---|---|---|
| Team add latency | 50–300 ms | < 5 ms | < 5 ms |
| Changes survive reload/restart | Yes | **No** | Yes |
| Validation gate | `haproxy -c` | Values validated by our code | Both, depending on change class |
| **Verdict** | Slower, more churn | Loses routes | **Chosen** |

**Decision.** **Two change classes, both through two safety gates (validate before, verify after):**

- **Route changes** (99 %): write the new map and slot state to disk atomically (temp file + `rename`), then apply in memory (`set server addr`, `add/del map`, or a versioned `prepare map` / `commit map` for a full replacement), then verify the canary and the changed route. On failure, push version N−1 back (< 10 ms).
- **Structural changes** (slot count, TLS settings, template version): render a candidate, `haproxy -c`, atomically swap files, `reload` through the master CLI (which answers `Success=1` or `Success=0`), verify `/__gw/healthz` returns the new `X-Config-Version` and the canary works, then commit as last-good. On failure, restore last-good, reload, verify the old version (≈ 300–800 ms, estimate).

**Consequences.** After any reload, memory equals files again. Rollback of structural changes never touches routes: routes always come from the current route table, so a rollback can never resurrect a deleted team's route.

## ADR-12 Team runtime

| | VM per team | k3s, namespace per team | **Docker on one host VM per hackathon** |
|---|---|---|---|
| Isolation | Strongest (own kernel) | Namespaces + NetworkPolicy | Namespaces + Neutron SG (pod-NIC) or bridges + iptables + hardening |
| Time to provision | 30–90 s | Seconds | Seconds (about 1 s with a warm pool) |
| Fits quota (10 VMs) | **No** (5–7 teams) | Yes | Yes: dozens of teams per host |
| Per-team OpenStack IP | Native | Needs MetalLB tricks | Native Neutron port per team |
| Our scored work visible | Yes | Less | Yes |
| RAM overhead | High per team | +0.5–1 GB per node | Minimal |
| **Verdict** | Optional premium driver | Viable, not chosen | **Chosen** |

**Decision.** **One Docker host VM per hackathon** (G0.basic.4c8g), teams as groups of containers. A host is a failure domain for its own hackathon only; the platform-wide HA requirement is about the shared gateways. An optional `vm` sandbox driver (Heat stack per team) can be added later behind the same interface.

## ADR-13 Team networking

**Context.** The team IP must be a real Neutron IP, reachable only from the gateways, isolated from other teams, from the host and from the metadata service, provisioned quickly and in parallel.

| Criterion | **pod-NIC: hot-plugged Neutron port moved into the team namespace** | **AAP: reserved port + allowed-address-pair + bridge + NAT** | netns + veth + policy routing | VM per team |
|---|---|---|---|---|
| Team IP is a bound Neutron port | Yes (ACTIVE, device = host VM) | Unbound "virtual" port | Unbound | Yes |
| Team-to-team isolation | **Neutron SG `cstam-team`** + namespace nftables | Host iptables only | Host nftables only | Neutron SG |
| Anti-spoofing | OVN port security per team NIC | Host-wide | Host-wide | Per VM |
| NAT | None | DNAT + SNAT per team | SNAT / policy routing | None |
| OVN gotchas | None for teams | GARP needed; virtual-port binding must be verified (E2) | Same as AAP | None |
| Neutron limits | Ports quota (500) | `max_allowed_address_pair` (default 10); whole-list updates race | Same as AAP | Instances quota (10) |
| Per-host limit | Free hot-plug slots (~25 on `pc`, maybe very few on `q35`) | AAP limit | AAP limit | – |
| Latency per team | Warm pool: ~1 s; cold: port create + attach 2–6 s (serialized per VM) | 1–3 s (coalesced AAP updates) | 1–2 s | 30–90 s |
| Complexity | Medium (namespace plumbing, re-plumb after restarts) | High (NAT, Docker rules, GARP, syncer) | High | Low but quota-bound |
| Demo value | "Isolation enforced by OpenStack itself" | OK | OK | Strong but cannot reach 10 |
| **Verdict** | **Primary if H1 shows ≥ 12 NICs and attach < 10 s** | **Fallback** | Rejected | Optional premium |

**Decision.** Build the host agent with a **`NetDriver` interface** and two implementations. **Gate G1 picks pod-NIC if experiment H1 passes, otherwise AAP** (with CIDR mode or ≤ 10 teams per host if H2 shows the default pair limit). No per-team Caddy router: HAProxy's map already routes each hostname to `team-IP:port`, so an extra app is just an extra slot.

**Consequences.** In pod-NIC mode a **warm pool** of pre-attached ports per host keeps provisioning fast; the host agent watches Docker events and re-plumbs a namespace if the pause container restarts, and restores all teams after a reboot. In AAP mode, a **per-host syncer** is the only writer of the host port's allowed-address-pairs: it computes the full list from the database, writes it in one update with `If-Match: revision_number=N`, and retries on 412; ten registrations become one or two Neutron calls.

## ADR-14 Firewall tool

| | iptables `DOCKER-USER` chains | **nftables tables** | eBPF / XDP |
|---|---|---|---|
| Coexists with Docker | Yes; Docker evaluates `DOCKER-USER` first and never flushes it | Yes, as separate tables (a drop in any base chain wins); never `flush ruleset` | Custom programs |
| Atomic apply + dry run | `iptables-restore --noflush --test` | `nft -c -f`, one kernel transaction | n/a |
| Readability for the jury | Good, counters with `-v` | Good | Hard |
| Effort | Low | Low | High |
| **Verdict** | **AAP mode** (team traffic crosses the host's forward path) | **pod-NIC mode** (rules inside each team namespace + a small host table) | Optional bonus only |

**Decision.** Same pattern as HAProxy: **render → check → atomic apply → verify drift periodically.** In pod-NIC mode, each namespace gets `table inet pod` (input only from the gateways to 80 and 8000–8099; output drops 169.254.0.0/16, 10.20.0.0/24 and private ranges); the host has `table inet cstam_host` (SSH and nothing else from the control plane). In AAP mode, `CSTAM-*` chains jumped from `DOCKER-USER` and `INPUT`, applied with `iptables-restore --noflush`.

## ADR-15 Container hardening

| | **Hardened runc (default)** | gVisor (`runsc`) |
|---|---|---|
| Kernel attack surface | Host kernel, seccomp-filtered | User-space kernel; host sees ~70 syscalls |
| Overhead | ~0 | Syscall-heavy apps 2–10× slower; +~50 MB per sandbox |
| Compatibility | Everything | Most web stacks; some fail |
| **Verdict** | **Default** | Optional per-hackathon "strict" mode after the core is done |

**Decision.** Every team container: `--cap-drop ALL` (plus `NET_BIND_SERVICE` only if needed), `--security-opt no-new-privileges`, read-only root with small `tmpfs`, `--pids-limit`, `--memory` = `--memory-swap`, `--cpus`, a non-root user, a per-team systemd slice for aggregate limits, json-file log rotation. Docker daemon: `userns-remap`, no `docker0`, `live-restore`, MTU 1442. Teams submit an app spec validated against an allowlist (`DisallowUnknownFields`): "privileged", host mounts and the Docker socket simply do not exist in the API.

## ADR-16 Secrets, OpenStack credential and agent PKI

| | Shared bearer token over TLS | **Own small Go CA** (`cstamctl pki`) | smallstep `step-ca` | WireGuard mesh |
|---|---|---|---|---|
| Mutual authentication | No | Yes | Yes | Yes |
| Moving parts | 0 | One CLI command | One daemon | Kernel config per node |
| Explainability | Low | **High (~150 lines you understand)** | Medium | Medium |
| **Verdict** | Cut-list fallback | **Chosen** | Fine alternative | Rejected |

**Decision.**

- **Agent PKI:** ECDSA P-256 CA created by `cstamctl pki init` (CA key kept offline or encrypted), leaf certificates with IP SANs and a URI SAN identity (`spiffe://cstam/gateway/gw1`), TLS 1.3, `RequireAndVerifyClientCert`. The control plane checks that `Ack(gateway=gw1)` really comes from gw1's certificate.
- **OpenStack credential:** an application credential with **access rules** limited to the port, attach-interface and server-read calls, with an expiry, not "unrestricted" (it cannot create other credentials). Only on the control plane, loaded with systemd `LoadCredential=`. Fallback F1 would need a second credential on the gateways limited to updating one floating IP.
- **Never** in cloud-init user-data (readable from the metadata service for the life of the VM). Agents receive keys after boot through `cstamctl node enroll` over SSH.
- Repository hygiene: `.gitignore`, a `gitleaks` pre-commit hook.

## ADR-17 Admin authentication

| | Static admin token | **Password (argon2id) + server-side sessions** | JWT | OIDC (Keycloak/Dex) |
|---|---|---|---|---|
| Revocation | Rotate token | Delete row | Hard | Yes |
| Browser safety | Token in localStorage | HttpOnly, SameSite=Strict cookie | Usually localStorage | Cookie |
| Effort | 10 min | ~1 day | ~1 day | 2–3 days + an IdP VM |
| **Verdict** | Too weak | **Chosen** | No need | Overkill |

**Decision.** Two roles, `admin` and `viewer` (give the jury a viewer account), hashed personal API tokens for the CLI and benchmarks, login rate limiting, CSRF protection (SameSite=Strict + Origin check + custom header). Every mutating call writes the append-only, hash-chained `audit_log`.

## ADR-18 Dashboard technology and realtime

| | **React + Vite + TypeScript + Tailwind + TanStack Query** | Go `templ` + htmx |
|---|---|---|
| Rich widgets (IP grid, charts, log table, diff viewer) | Huge ecosystem | More hand-made JS |
| Type safety with the API | Types generated from OpenAPI | Shared Go types |
| Deployment | `npm run build` → embedded with `go:embed` → still one binary | One binary natively |
| Listed in the brief's skills | Yes | No |
| **Verdict** | **Chosen** | Fallback if behind schedule |

| Realtime | **SSE** | WebSocket | Polling |
|---|---|---|---|
| Direction needed | Server → browser only | Both | – |
| Reconnect and resume | Built in (`Last-Event-ID`) | By hand | n/a |
| Through HAProxy | Plain HTTP | Upgrade handling | Plain |
| **Verdict** | **Chosen** | Not needed | Too laggy |

**Decision.** The SSE broker never blocks on a slow browser (drop and count). Traffic logs flow HAProxy → local UDP → `gwagent` → `PushTrafficLogs` → in-memory ring buffers in `cpd` → SSE; only aggregates (every 10 s) go to Postgres.

## ADR-19 DNS and TLS

**Context.** No Designate. A TLS wildcard covers exactly one label: `*.cstam.felcloud.tn` does **not** cover `team5.hacka.cstam.felcloud.tn`. Let's Encrypt allows 50 certificates per registered domain per week, shared with all of `felcloud.tn`.

| DNS tier | Requires from FelCloud | Gives us |
|---|---|---|
| 1. Delegated zone `cstam.felcloud.tn` → our small authoritative DNS (`miekg/dns`) | NS records | Per-team records created and purged at teardown ("purge DNS rules" becomes real); DNS-01 for wildcards |
| 2. Wildcard `*.cstam.felcloud.tn` → floating IP | One A record | Working hostnames; nothing to purge |
| 3. **`sslip.io`** | Nothing | Always works; test on day 1 so the demo never depends on FelCloud |

| TLS option | Requires | Verdict |
|---|---|---|
| **DNS-01 wildcard per hackathon** (`*.hacka.cstam.felcloud.tn`) via one CNAME `_acme-challenge…` → `acme-dns` we run, `go-acme/lego` | One CNAME from FelCloud | **Preferred** |
| HTTP-01 per hostname | Wildcard A record | Acceptable; watch the rate limit; staging endpoint during tests |
| Self-signed CA | Nothing | Demo fallback |

**Decision.** Configurable `BASE_DOMAIN`, tiers tried in order. Certificates are pushed to both gateways like routes and hot-swapped through the runtime API (`set ssl cert` + `commit ssl cert`). Always use the Let's Encrypt **staging** endpoint until the final demo.

## ADR-20 Deployment

| | **Static binaries + systemd, cloud-init installs packages** | Agents in containers | Ansible |
|---|---|---|---|
| Fit for agents | Agents need host networking and capabilities anyway | Needs `--privileged`, Docker socket: no isolation gained | Fine, more tooling |
| Transparency | High | Medium | High |
| **Verdict** | **Chosen** | Rejected | Optional for the report |

**Decision.** `CGO_ENABLED=0` static builds; systemd units with `Type=notify` (an agent is "started" only once synced), `Restart=always`, sandboxing options. cloud-init only installs packages and system settings, never secrets. `cstamctl node enroll` copies certificates and env files over SSH through the control plane (bastion). Rolling upgrades: backup gateway first, manual failover, then the other. Docker `live-restore` so restarting dockerd does not kill team containers.

## ADR-21 Control-plane HA

| Option | Effort | Recovery time | Worth it? |
|---|---|---|---|
| **Single VM + `Restart=always` + `pg_dump` to Swift every 5 min + rebuild script** | Low | Minutes | **Deploy this** |
| Two `cpd` processes sharing Postgres (stateless API, leader-elected sweepers) | Low (already designed for it) | Seconds for `cpd`; Postgres still single | **Demonstrate it** (kill the leader mid-stress test) |
| + Postgres streaming replica, manual or `pg_auto_failover` promotion | Medium–high | ~30 s | Roadmap |
| Patroni + etcd (3 nodes) | High, 3 more VMs | Seconds | No (quota, time) |

**Decision.** **Static stability first**: the control plane is not on the data path, so its failure stops provisioning and the dashboard, not traffic. Agents persist last-good state and restore it at boot without the control plane.

**Consequences.** A judge's "isn't the control plane a single point of failure?" gets a measured answer: "We stopped it for five minutes during the load test: zero failed requests on existing teams; failover and rollback still worked."

## ADR-22 Infrastructure as code for the permanent platform

**Context.** The router, 5 security groups, VIP and infrastructure ports, server group, VMs and floating IPs must be created once, rebuilt identically, changed safely and removed without touching FelCloud's own resources. Team ports are out of scope: `cpd` manages them at runtime with gophercloud.

| | Bash + `openstack` CLI | Heat (HOT YAML) | Terraform / OpenTofu | **Pulumi (Go)** |
|---|---|---|---|---|
| Plan before change | No | `stack update --dry-run` (weak) | `plan` | **`preview`** |
| State / drift | None; hand-written guards | In Heat | State file; `plan` shows drift | State file; `refresh` shows drift |
| Language | Bash | YAML | HCL | **Go, same as the project; `go build` catches errors** |
| Protect critical resources | Manual | No | `prevent_destroy` | `Protect(true)` |
| FelCloud risk | None | Heat version unknown | None (OpenStack provider) | None (same provider underneath) |
| Score impact | – | Heat bonus (+2) | – | – (Heat bonus still earned by the VM-per-team driver, Step 13) |
| **Verdict** | Step 1 lab only | Bonus driver only | Good alternative | **Chosen** |

**Decision.** `infra/pulumi` is a Pulumi Go program in its own Go module (Pulumi's large dependency tree stays out of `cpd` and the agents). Existing FelCloud objects are **lookups**, never resources, so `pulumi destroy` cannot touch them. The VIP port and the public floating IP are `Protect`ed. VMs ignore changes to user data and image so editing cloud-init never rebuilds a running VM by surprise. State lives on the laptop (`pulumi login file://~/.pulumi-cstam`), backed up, never in git. You run `preview`, `up` and `destroy`; the coding agent only reads and edits the program.

**Consequences.** One more CLI and a state file to back up. In return, every infrastructure change is reviewed as a diff in git and as a plan before it happens, which is exactly the "understand every step" rule of this project.

::: learn
**Learning checkpoint**

1. Pick three ADRs and explain, without notes, why the second-best option lost.
2. Why does Postgres `NOTIFY` avoid the dual-write problem that a separate broker has?
3. What is a fencing epoch, and what bad thing does it prevent?
4. Why must files on disk be written before the runtime API change, and not after?
5. Why is `*.cstam.felcloud.tn` not enough for TLS, and what certificate do we need instead?
:::
