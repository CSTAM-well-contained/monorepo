> **Superseded.** This is the first plan (v1, Python era). The current plan is the implementation guide in `docs/guide/` (Go, Pulumi). Kept for history.

# CSTAM x FelCloud – Implementation Plan

How we turn `docs/solution-design.html` into working code, one step at a time, with agentic coding
but without "vibe coding": every step is understood before it is committed.

---

## How we work on every step

Each step goes through the same loop:

1. **Explain** – Claude explains what we are building, which OpenStack / Linux pieces are involved,
   and what can go wrong. No code yet.
2. **By hand first** – anything touching infrastructure is first done manually (`openstack`, `ip`,
   `iptables`, `docker` commands) so you see it working before it is automated.
3. **Small diff** – Claude writes one focused change. You read it, ask questions, and get any
   line explained.
4. **Prove it** – every step ends with a test or check with a clear "done when" result.
5. **You commit** – a step is finished only when you can explain it in your own words
   (this is also preparation for the jury Q&A).

Rules:

- Claude never creates or deletes OpenStack resources without asking first.
- Use plan mode for the bigger steps (3, 5, 6, 7) so the plan is approved before code is written.
- Run `/code-review` before committing a large step.
- Keep `docs/notebook/experiments.md` as a lab notebook: commands run, results, measurements.
  The benchmark report and technical report will mostly come from it.

---

## Overview

| # | Step | Points served | Status |
|---|------|---------------|--------|
| 0 | Repo setup | – | ☐ |
| 1 | Test the risky parts by hand | prerequisite for all | ☐ |
| 2 | Gateway pair as code | HA gateways (15) | ☐ |
| 3 | Reload and rollback pipeline | Reload & rollback (10) | ☐ |
| 4 | Team network on one host → host agent | Provisioning (15), isolation (5) | ☐ |
| 5 | Control plane skeleton + IPAM | IPAM (10) | ☐ |
| 6 | Provisioning (`POST /teams`) | Provisioning (15), stress test (15) | ☐ |
| 7 | Teardown, reconciler, quarantine | Teardown & lifecycle security (10) | ☐ |
| 8 | Security and isolation | Isolation & safeguards (5) | ☐ |
| 9 | Stress test and benchmark report | Stress test (15), deliverable | ☐ |
| 10 | Admin dashboard | Dashboard (10) | ☐ |
| 11 | Bonuses, docs, demo video | +2 each, deliverables, pitch | ☐ |

**Why this order:** steps 1–3 cover 25 points and carry the biggest risk on FelCloud's network
(floating IP on an unbound VIP port, VRRP, allowed-address-pairs). We check the network design
works on FelCloud before building the control plane on top of it.

---

## Step 0 – Repo setup

**Goal:** a clean, safe project structure.

Tasks:
- `git init`
- `.gitignore` that excludes `.secrets/`, `.env`, `clouds.yaml`, `node_modules/`, `__pycache__/`
- Folder layout:
  ```
  infra/          OpenStack scripts (router, ports, VMs, security groups), later Heat templates
  gateway/        HAProxy + Keepalived configs, gateway agent
  host-agent/     per-hackathon host agent (Docker networks, NAT, iptables)
  control-plane/  FastAPI API, IPAM, workers/reconciler, DB migrations
  dashboard/      React admin dashboard
  bench/          stress tests and benchmark scripts
  docs/           design, plan, experiments notebook, reports
  ```
- `CLAUDE.md` with the working rules above.
- `docs/notebook/experiments.md` (empty notebook template).

**You will understand:** how the project is organised and where each component lives.

**Done when:** first commit exists and `git status` / `git log -p` show no secrets.

---

## Step 1 – Test the risky parts by hand (no code)

**Goal:** check the three assumptions from the design's "Risks" section before writing code.

Tasks:
1. Router: `openstack router create cstam-router --external-gateway INTERNET` and attach
   `cstam-sandbox-subnet`.
2. VIP: create `vip-port` on `cstam-sandbox-net`, attach a floating IP to it.
3. Two small test VMs; add the VIP to both ports' `allowed-address-pairs`.
4. Security group allowing VRRP (IP protocol 112) only between the two VMs.
5. Keepalived with unicast peers on both; a tiny web server on each saying "I am VM1/VM2".
6. Kill VM1 (or stop keepalived) and watch the floating IP keep answering from VM2.
7. Separately: add a secondary IP to a VM via allowed-address-pairs + `ip addr add`, reach it
   from another VM.

**You will understand:** VRRP, VIPs, why Neutron anti-spoofing needs allowed-address-pairs,
how a floating IP maps to a private IP (and that FelCloud runs OVN).

**Done when:** `curl` to the floating IP survives killing VM1; failover time and every command
are recorded in `docs/notebook/experiments.md`. If any test fails, we revise the design before step 2.

---

## Step 2 – Gateway pair as code

**Goal:** the two real gateways, reproducible from scripts.

Tasks:
- `infra/` script creating: anti-affinity server group, 2 gateway VMs (`G0.basic.1c2g`),
  gateway security group, VIP pair, floating IP.
- HAProxy config: one HTTP frontend, routing by `Host` header via a map file
  (`hosts.map`: `team1.hackA.cstam.felcloud.tn 10.20.0.55:80`), 404 for unknown hosts,
  master-worker mode, admin socket bound locally.
- Keepalived config: unicast peers, `track_script` checking HAProxy every second.
- Cloud-init / provisioning script to install and configure both.

**You will understand:** HAProxy map-based routing, `track_script` (VIP moves even if the VM
is alive but HAProxy is dead), anti-affinity.

**Done when:** a test subdomain routes to a dummy backend through either gateway; killing
HAProxy on gw1 moves the VIP to gw2; failover time measured.

---

## Step 3 – Reload and rollback pipeline (gateway agent)

**Goal:** change routes while traffic flows; a bad config can never stay live.

Pipeline:
1. **Render** config from a template; names sanitized to `[a-z0-9-]`.
2. **Validate** with `haproxy -c -f new.cfg`; invalid → reject, live config untouched.
3. **Apply**: route add/remove via Runtime API (`add map` / `del map`, no reload);
   structural changes via hitless reload.
4. **Health check**: `/healthz` + a known route; failure → restore last-good and reload (< 1 s).
5. **Commit**: save as last-good (versioned) and push the same change to the peer gateway.

Gateway agent: small Python service on each gateway exposing these operations to the control
plane, plus health and VRRP state (master/backup).

**You will understand:** the two safety gates (before and after apply), Runtime API vs reload,
how master-worker mode keeps listening sockets during reload, why both gateways must stay in sync.

**Done when:**
- broken config → rejected at validation;
- valid-but-broken config → rolled back automatically in < 1 s;
- a load generator running during all of this shows 0 failed requests.

---

## Step 4 – Team network on one host → host agent

**Goal:** a team gets its own Neutron IP and isolated bridge on the hackathon host.

First by hand for `team1`:
1. Neutron port with a fixed IP, no VM attached (e.g. `10.20.0.55`).
2. Add that IP to the host VM port's `allowed-address-pairs`.
3. `ip addr add 10.20.0.55/24 dev eth0` on the host.
4. Docker network `br-team1` (`172.20.1.0/24`) + Caddy router container on it.
5. NAT: DNAT `10.20.0.55:80 → team1-router`, SNAT `br-team1 → 10.20.0.55`.
6. Hostname routing in Caddy: `web.team1… → web:3000`, `api.team1… → api:8000`.

Then turn the recipe into the **host agent** (Python service, mTLS): `create_team`,
`delete_team`, `status`, all idempotent.

**You will understand:** the core of the design – how a Neutron IP, a Docker bridge and NAT
combine so the gateway only ever routes to `team-IP:80`.

**Done when:** team1 reachable at `10.20.0.55:80` from the gateway; running `create_team`
twice changes nothing; `delete_team` leaves no rules, bridge or IP behind.

---

## Step 5 – Control plane skeleton + IPAM

**Goal:** the source of truth and a correct IP allocator.

Tasks:
- FastAPI app, PostgreSQL, migrations (Alembic).
- Tables: `hackathons`, `teams`, `ip_leases`, `routes`, `config_versions`, `events`.
- IPAM state machine: `FREE → LEASED → ACTIVE → RELEASING → QUARANTINE → FREE`.
- Lease allocation with `SELECT … FOR UPDATE SKIP LOCKED`; lease TTL; quarantine period (60 s).
- Clients for Neutron (openstacksdk), host agent, gateway agents.

**You will understand:** row locking and why 10+ parallel registrations never get the same IP;
why the DB, not Neutron, decides which IP to use next.

**Done when:** unit tests pass, including a concurrency test (20 parallel leases → 20 distinct
IPs) and tests for every allowed / forbidden state transition.

---

## Step 6 – Provisioning (`POST /teams`)

**Goal:** one API call → live, isolated sandbox in seconds.

Steps (each one undone in reverse order if a later one fails):
1. IPAM lease
2. Neutron reserved port with that fixed IP
3. Allowed-address-pair on the host port
4. Host agent: bind IP, bridge, router, NAT, isolation rules
5. Both gateways: map entry via the step 3 pipeline
6. Health check → `ACTIVE`, TTL starts

**You will understand:** compensation (undo in reverse order), idempotent steps, why no VM
boots during team provisioning (that is what makes it fast).

**Done when:** one call gives a working subdomain; injecting a failure at step 4 leaves nothing
behind (no port, no allowed pair, no IP leased).

---

## Step 7 – Teardown, reconciler, quarantine

**Goal:** free resources immediately and safely; repair drift automatically.

Strict teardown order:
1. Route removed on **both** gateways
2. Containers, bridge, NAT and isolation rules removed
3. Allowed-address-pair removed
4. Neutron port deleted
5. IP → `QUARANTINE` (60 s) → `FREE`

Reconciler (periodic worker): expires leases and triggers teardown; compares DB with Neutron,
gateways and Docker; removes orphan ports, stale routes and dead containers.

**You will understand:** the bug in `docs/design-v1/edge-cases.md` – deleting the port before the route
lets Neutron give the IP to another team while the old route still points at it, so one team's
URL serves another team's app.

**Done when:**
- the edges.md scenario is an **automated test** and it cannot happen;
- expired teams are cleaned up with no manual action;
- a manually created orphan port / stale route is repaired by the reconciler.

---

## Step 8 – Security and isolation

**Goal:** a team can reach only its own sandbox; nobody can reach the management layer.

Tasks:
- Security groups with explicit rules using `remote_group_id` (gateway SG, host SG, control-plane SG).
  Fix the empty "HTTP from gateways" group found on `sbx-f0699704`.
- Host iptables: `DOCKER-USER` drop rules between team bridges and towards gateway,
  control-plane and metadata (`169.254.169.254`) IPs; `INPUT` protection for the host.
- Container hardening: `cap-drop ALL`, `no-new-privileges`, read-only root, userns remap,
  CPU/RAM/PID limits per team cgroup slice.
- HAProxy admin socket local only; gateway SSH / agent API only from the control-plane SG.

**You will understand:** the 5 defence layers and which attack each one blocks.

**Done when:** an automated attack test suite fails every attack: team1 → team2,
team → metadata, team → gateway admin, team → control plane, team → host.

---

## Step 9 – Stress test and benchmark report

**Goal:** numbers for the Phase 2 deliverable.

Measure:
- Provisioning latency for 10, 20 (and max possible) concurrent registrations
- Gateway failover time (kill VM, kill HAProxy, kill Keepalived)
- IP recycling time (teardown → IP reusable)
- Reload / rollback time; requests lost during reload (should be 0)
- Load test throughput and latency through the gateways

**Done when:** `docs/benchmark-report.md` with tables/charts and reproducible scripts in `bench/`.

---

## Step 10 – Admin dashboard (React)

**Goal:** the Phase 3 dashboard. It contains no logic of its own; it only shows API data and
calls API endpoints.

Shows / does:
- Active team sandboxes and their OpenStack private IPs
- Real-time gateway health (master vs backup)
- Mapped subdomains and live traffic logs
- Buttons: spin-up, teardown, manual gateway failover, manual config rollback

**Done when:** every item of the Phase 3 list is visible or clickable and works against the
real system.

---

## Step 11 – Bonuses, documentation, demo

Bonuses (+2 each), in order of effort:
- Maintenance page when a sandbox is down (HAProxy `errorfile` / backup server)
- Rate limiting per team (HAProxy stick-tables)
- Wildcard TLS (needs DNS-01 access from FelCloud)
- Custom domain binding
- Heat templates for the gateway pair and host VMs

Deliverables:
- Architecture & HA gateway data-flow diagram (already in `docs/diagrams/`)
- API integration and security architecture documentation
- Technical report
- Demo video: subdomain routing, hot reload + rollback, live gw1 crash and failover
- Pitch deck and rehearsed live demo

---

## Open questions for the FelCloud mentors

- Wildcard DNS record `*.cstam.felcloud.tn`, and API access for Let's Encrypt DNS-01?
- Quota increase possible?
- Who created `sbx-f0699704`; is there an existing CSTAM tool to build on?
