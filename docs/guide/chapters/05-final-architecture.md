# 5. The final architecture

This chapter describes the system you will build, component by component, then follows a request, a registration, a gateway crash and an expiry through it. Chapter 6 justifies each choice; chapter 7 describes the Go code.

![Figure 5.1 – The final architecture: one floating IP, a redundant gateway pair, Docker hosts with one Neutron port per team, and a control plane that is not on the data path.](diagrams/f03-final-architecture.png)

## 5.1 The solution in one paragraph

One public floating IP points to a virtual IP (VIP) shared by two gateways running **HAProxy and keepalived**. Keepalived moves the VIP to the standby gateway within about two seconds if the active one dies. HAProxy routes each team's subdomain to that team's **own Neutron-managed private IP** on a Docker host VM (one host per hackathon). There, the team's containers share a private network namespace that owns the team's IP. A Go **control plane** (API, IPAM, controller, reconciler, dashboard) provisions teams, validates and rolls back gateway configuration, and tears everything down in a safe order. Small Go **agents** on the gateways and hosts apply what the control plane wants. The control plane is not on the request path: if it dies, traffic keeps flowing.

## 5.2 Components

### Network (FelCloud, tenant-level only)

| Element | Configuration |
|---|---|
| `cstam-router` | External gateway on `INTERNET`, interface 10.20.0.1 on `cstam-sandbox-subnet` |
| `cstam-sandbox-net` | 10.20.0.0/24, MTU 1442, OVN. IP plan as in chapter 1: VIP .10, gateways .11/.12, control plane .5, hosts .21/.22, **team pool .32–.191** (.68 reserved), lab pool .192–.199, Neutron's own pool .200–.250 untouched |
| Floating IPs | **One** for the platform (→ `cstam-vip`), one for the control plane (SSH bastion and dashboard) |
| Security groups (5, about 36 rules) | `cstam-gw` (80/443 from anywhere; SSH and stats port 8404 from `cstam-cp`), `cstam-vrrp` (IP protocol 112 between members only), `cstam-cp` (SSH and 443 from your admin IP; agent port 8443 from `cstam-gw` and `cstam-host`), `cstam-host` (SSH and ICMP from `cstam-cp`), `cstam-team` (TCP 80 and 8000–8099 **from `cstam-gw` only**; no rule for its own members, so team-to-team traffic is dropped by Neutron) |
| Server groups | `cstam-gw-ag` (anti-affinity for the two gateways), `cstam-host-ag` (for the hosts) |

### Gateways (2 × G0.basic.1c2g, Ubuntu 24.04)

| Piece | What it does |
|---|---|
| **HAProxy 3.2 LTS** | Frontend on :80 and :443. Reads the `Host` header, looks it up in `hosts.map` (hostname → *slot backend*), and forwards to that slot's single server = the team's IP and port. Unknown hosts get a 404 page. Each slot has health checks; a slot without a healthy server shows a maintenance page. Per-team rate limits (stick tables). JSON traffic logs to the local agent. Master-worker mode for hitless reloads. |
| **keepalived 2.2** | VRRPv3, unicast between the two gateways, `advert_int 0.5`. Both start as BACKUP with `nopreempt`: the first elected master keeps the VIP, so a returning gateway does not cause a second outage. Tracks the HAProxy process (instant), a gateway check (agent synced and HAProxy answering), and a "force backup" file for manual failover. Sends gratuitous ARPs so OVN moves the VIP. |
| **`gwagent`** (Go) | Dials out to the control plane over mTLS and watches its desired state (routes, configuration version, force-backup flag), tagged with a **generation** number. Applies changes through two safety gates: validate before (`haproxy -c`) and verify after (health check and canary route), with automatic rollback to the last good version. Acknowledges each generation with a digest of what is really loaded. Reports VRRP state and streams traffic logs. Blocks the gateway from taking the VIP until it has synced (boot rule). |

### Hackathon hosts (1–2 × G0.basic.4c8g, Ubuntu 24.04, Docker)

The host agent supports two network drivers behind one Go interface. Gate G1 picks one from experiment results.

| | **pod-NIC (primary)** | **AAP (fallback)** |
|---|---|---|
| Team IP lives on | A real Neutron port, hot-plugged into the host VM as a NIC | A reserved, unbound Neutron port; its IP is an allowed-address-pair on the host port |
| Inside the VM | The NIC is moved into the network namespace of a `pause` container (`<team>-pod`), renamed `eth0`, MTU 1442, default route via 10.20.0.1. Team containers join with `--network container:<team>-pod` and listen on the team IP directly | The IP is bound on the host's interface (`ip addr add` + gratuitous ARP). A Docker bridge per team (`cst-tNNNN`, MTU 1442). DNAT team-IP:port → container, SNAT bridge → team IP |
| Team-to-team isolation | **Neutron security group `cstam-team`** (no self rule), plus an nftables table inside each namespace | Host iptables chains (`CSTAM-*`, jumped from `DOCKER-USER`) |
| Metadata (169.254.169.254) | Dropped by the namespace's nftables table | Dropped by the host iptables chains |
| Speed | Warm pool of pre-attached ports per host → about 1 s per team | Port create + allowed-address-pair update (coalesced per host) → 1–3 s per team |
| Limits | Hot-plug slots per VM (measured in H1) | `max_allowed_address_pair` per port (default 10, measured in H2) |

Common to both: container hardening (`--cap-drop ALL`, `no-new-privileges`, read-only root, memory/CPU/PID limits, a systemd slice per team), Docker `live-restore`, and the **`hostagent`** (Go), which dials out to the control plane, applies team specs idempotently, restores everything after a reboot from its desired state, and cleans up in order on teardown.

### Control plane (1 × G0.basic.2c4g, Ubuntu 24.04)

| Piece | What it does |
|---|---|
| **`cpd`** (Go, one binary) | Role `api`: public REST API under `/v1` (OpenAPI), the agents' mTLS endpoint, SSE streams, the embedded dashboard. Role `controller`: team workers (provision and teardown), the per-host allowed-address-pair syncer (AAP mode), the warm-pool filler (pod-NIC mode), the gateway publisher (generations), the sweepers (lease expiry, quarantine → free), the Neutron drift scanner and garbage collector. |
| **PostgreSQL 16** | Source of truth: hackathons, hosts, teams, IP addresses, leases, routes, generations, gateways, config versions, operations, events, audit log. Unix socket only. Backed up to Swift every 5 minutes. |
| **Dashboard** | React + Vite + TypeScript + Tailwind, built to static files embedded in `cpd`. Overview, Teams, Gateways, Routing & Config, Traffic, IPAM grid, Audit. Live updates by Server-Sent Events. No business logic: every button is an API call. |
| **OpenStack credential** | An application credential restricted by access rules to the Neutron and Nova calls the control plane needs. Lives only on this VM. |

### Tools

`cstamctl` (Go CLI): PKI (create the CA, issue agent certificates), node enrollment over SSH, `doctor` (read-only checks of the cloud), benchmarks, the security test suite, `gc --dry-run`.

## 5.3 The request path

A visitor opens `http://team5.hacka.cstam.felcloud.tn/`:

1. **DNS**: the wildcard record (or `sslip.io`) resolves to the platform's floating IP, 197.5.133.x.
2. **Floating IP → VIP**: the OVN logical router `cstam-router` translates the floating IP to 10.20.0.10 (the VIP).
3. **VIP → master gateway**: OVN delivers traffic for 10.20.0.10 to whichever gateway last announced it with a gratuitous ARP (the VRRP master, say gw1).
4. **HAProxy on gw1**: the `Host` header `team5.hacka…` is looked up in `hosts.map` → `be_slot007`. That slot's server is `10.20.0.57:80`.
5. **gw1 → team IP**: HAProxy opens a connection from gw1's own address 10.20.0.11 to 10.20.0.57:80. The `cstam-team` security group on team 5's port allows it because the source belongs to `cstam-gw`.
6. **Team IP → app**: in pod-NIC mode, the packet arrives on team 5's NIC inside its namespace, where the web container listens on :80. (In AAP mode, the host DNATs 10.20.0.57:80 to the container on team 5's bridge.)
7. The response flows back the same way. HAProxy adds `X-Served-By: gw1` and logs one JSON line, which reaches the dashboard's live traffic view.

The control plane appears nowhere in this list. That is the "static stability" property: **the data path does not depend on the control plane.**

## 5.4 Four end-to-end walkthroughs

### A team registers

1. `POST /v1/hackathons/{id}/teams {"name":"team5"}` with an `Idempotency-Key`. The API inserts the team (`desired = PRESENT`, phase `REQUESTED`) and notifies the controller in the same transaction. It answers `202 Accepted`.
2. A controller worker claims the team (`FOR UPDATE SKIP LOCKED`, with a fencing epoch). In one transaction it **leases an IP** (least recently released free row, `SKIP LOCKED`) and picks a host.
3. **Neutron port**: pod-NIC takes a warm, already attached port (rename and re-tag); otherwise it creates `cstam-team-<id>` with the leased fixed IP and tags. In AAP mode, the per-host syncer adds the IP to the host port's allowed-address-pairs with compare-and-swap.
4. **Host agent**: the team spec appears in the host's desired state; the agent builds the namespace, firewall and containers and reports ready.
5. **Route**: the controller inserts the route and bumps the global **generation** in one transaction. Both gateway agents receive the new snapshot, apply it at runtime (`set server addr`, `add map`), verify it, and acknowledge the generation with a digest.
6. **Health**: a probe through the gateway path succeeds. The lease becomes ACTIVE with its TTL, the team becomes ACTIVE, and the dashboard shows it live. Expected total: a few seconds (to be measured).

### gw1 crashes

1. gw1 stops sending VRRP advertisements (kernel panic, VM stopped).
2. After `3 × 0.5 s + skew` ≈ 1.8 s, keepalived on gw2 declares itself MASTER, adds 10.20.0.10, and sends gratuitous ARPs.
3. OVN rebinds the virtual VIP port to gw2's port. The floating IP still maps to 10.20.0.10, which now lands on gw2.
4. gw2 already holds the same configuration generation (it acknowledged every change), so routing is identical. New connections succeed. Connections that were open on gw1 are reset.
5. gw2's agent reports MASTER; the dashboard flips the cards and records "failover 1.8 s". When gw1 returns, it stays BACKUP (`nopreempt`) and must sync before it is even eligible.

If only HAProxy dies on gw1, `vrrp_track_process` notices immediately and gw1 goes to FAULT: failover in well under a second.

### A sandbox expires (or is deleted)

1. The expiry sweeper sees `expires_at ≤ now()` and sets `desired = ABSENT`.
2. A worker runs the **teardown steps in strict order**:
   1. remove the route (new generation G);
   2. wait until the master gateway acknowledges G (the subdomain now returns 404);
   3. host cleanup: containers, namespace or bridge, firewall rules, conntrack entries;
   4. detach the port from the host (pod-NIC) or remove the allowed-address-pair (AAP);
   5. delete the Neutron port;
   6. move the IP to **QUARANTINE**, recording that generation G must be acknowledged.
3. The IP sweeper frees the IP only when the quarantine (60 s) has elapsed, **every member gateway has acknowledged generation ≥ G**, and Neutron confirms no port holds the IP. Only then can another team get it.

This order is what prevents the `edges.md` bug. A backup gateway that is down simply keeps the IP in quarantine until it comes back and syncs, or until an admin fences it.

### The control plane dies

Gateways keep their last-good configuration on disk and in memory. Hosts keep their containers and rules. Failover still works (VRRP is between gateways only). Local rollback still works (it is done by the gateway agent). Only registrations, teardowns and the dashboard pause. When `cpd` restarts, workers find teams whose `generation` differs from what was observed and continue where they stopped.

## 5.5 Requirement coverage

| Challenge item | Points | Where in the design | Where in the guide |
|---|---|---|---|
| Automated OpenStack sandbox provisioning | 15 | Real Neutron port per team, host agent, controller | ADR-12/13, Steps 5, 7 |
| Dual redundant HA gateways | 15 | HAProxy + keepalived VRRPv3 on two anti-affine VMs, VIP on OVN virtual port, boot rule | ADR-07/08, Steps 1, 3, 4 |
| Automated IPAM & reclamation | 10 | Pool table with SKIP LOCKED, leases with TTL, quarantine gated by acknowledgements | ADR-05/06, Steps 6, 8 |
| Zero-downtime reload & auto-rollback | 10 | Runtime map/server updates, validated hitless reloads, health gate, last-good rollback | ADR-10/11, Step 4 |
| Concurrent provisioning stress test (10+) | 15 | No VM boot per team, warm pool, coalesced Neutron updates, compare-and-swap | ADR-04/05/13, Steps 7, 11 |
| Automated teardown & lifecycle security | 10 | Strict teardown order, generations, reconciler, garbage collector | ADR-06, Step 8 |
| Network isolation & access safeguards | 5 | Neutron security groups, namespace nftables, container hardening, attack test suite | ADR-14/15, Step 9 |
| Admin dashboard | 10 | React dashboard over REST + SSE, all four control types | ADR-18, Step 12 |
| Pitch and live demo | 10 | Demo script, chaos panel, backup clips | Step 14 |
| Bonus: smart health monitoring & fallback | +2 | Slot health checks + maintenance page | Step 13 |
| Bonus: rate limiting & bandwidth throttling | +2 | HAProxy stick tables, `bwlim` filters | Step 13 |
| Bonus: OpenStack native orchestration | +2 | Heat templates for the infrastructure (and optional VM driver) | Step 13 |
| Bonus: automated SSL/TLS | +2 | lego DNS-01 (or HTTP-01) + HAProxy crt-list hot update | Step 13 |
| Bonus: custom domain binding | +2 | Verified CNAME/TXT, extra map entry, certificate | Step 13 |

## 5.6 Locked decisions and decisions validated in Step 1

**Locked** (do not revisit them without new facts):

1. Go for the whole backend, one module, four binaries.
2. HAProxy as the L7 gateway with slot backends; files on disk always describe the full state; runtime API as the fast path; validated hitless reloads for structural changes.
3. Two gateways, Active/Standby; keepalived VRRPv3 unicast as the primary HA mechanism.
4. Container sandboxes on Docker hosts by default, one real Neutron port per team.
5. PostgreSQL is the source of truth. The IPAM owns .32–.191, outside Neutron's allocation pool.
6. Strict teardown order with generation acknowledgements before an IP is reused.
7. Agents dial out and pull full desired-state snapshots tagged with a generation. Static stability.
8. One floating IP for all teams.
9. The dashboard has no logic of its own.

**Validated in Step 1** (gate G1):

| Experiment | Question | Decides |
|---|---|---|
| E1 | Does the floating IP on an unbound VIP port follow keepalived failover, and how fast? | VRRP (primary) vs fallback F1 (move the floating IP from the notify script) vs F2 (OVN load balancer) |
| E2 | Does a reserved team port + allowed-address-pair make the team IP reachable, with ARP answered by the host's MAC? | Whether the AAP fallback is usable |
| E3 | Can we create ports with explicit fixed IPs outside the allocation pool? | Team pool .32–.191 |
| E4 | Do concurrent allowed-address-pair updates lose writes, and does `If-Match` return 412? | Evidence for the AAP syncer |
| E5 | Large transfers through MTU 1442 vs 1500 | Docker and namespace MTU settings |
| E6 | Failback, split-brain and a 1-hour soak at `advert_int 0.5` | keepalived settings |
| E7 | Octavia `ovn` provider with a health monitor | Whether fallback F2 is real |
| E9 | DNS: delegated zone, wildcard, or `sslip.io` | Domain and TLS approach |
| E10 | Is 169.254.169.254 and the management range blocked from a team? | Isolation rules |
| H1 | How many NICs hot-plug into a host VM, and how fast? | pod-NIC (≥ 12 NICs, < 10 s per attach) vs AAP |
| H2 | `max_allowed_address_pair` | AAP capacity per host (10 → CIDR mode or more hosts) |

::: learn
**Learning checkpoint**

1. Walk the request path out loud, naming every IP address the packet goes through.
2. Which component would a judge call a single point of failure, and why is it not on the data path?
3. In the teardown walkthrough, what would go wrong if step 5 (delete port) ran before step 1 (remove route)?
4. What exactly does a gateway acknowledge, and why does the acknowledgement include a digest?
5. Name two things that keep working when `cpd` is dead, and two that stop.
:::
