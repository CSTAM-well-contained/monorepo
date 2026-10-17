# 8. The build flow at a glance

This chapter is the map for Parts II and III. It shows the order of the steps, what can run in parallel, the three go/no-go gates, the critical path, and what to cut if you run late. Each step later has its own detailed section.

![Figure 8.1 – Phases, parallel tracks and gates. Red arrows mark the critical path.](diagrams/f05-build-flow.png)

## 8.1 Principles behind the order

1. **Burn down the cloud risk first.** The two riskiest assumptions (the VIP on an OVN virtual port, and how a team IP reaches a host) are tested by hand in Step 1, before any Go code depends on them.
2. **Build what does not depend on the cloud in parallel.** The control plane (IPAM, controller, teardown) is pure logic plus Postgres. It is built and tested against fakes from day 3, while the experiments run.
3. **Integrate early, once.** Gate G2 requires one team working end to end on FelCloud before any hardening work. Integration problems found late are the most expensive.
4. **Evidence is a feature.** The benchmark report, the security table and the video are generated from tools you build along the way.
5. **Highest points first; cut from the bottom.**

## 8.2 Phases, tracks and gates

| Phase | Days (part-time, estimate) | Content | Ends with |
|---|---|---|---|
| **Phase 0 – Setup** | Day 1 | Step 0: repository, tooling, `CLAUDE.md`, CI, `cstamctl doctor`; **email the mentors** | Green CI, no secrets in git |
| **Phase 1 – Risk burn-down** | Days 2–5 | Step 1: experiments E1–E10, H1, H2 on real VMs | **Gate G1** |
| **Phase 2 – Core build** | Days 3–16 | Three tracks in parallel (below) | **Gate G2** |
| **Phase 3 – Hardening and scoring** | Days 13–24 | Security, stress, failover benchmarks, generation fencing, static stability, dashboard wired, bonuses 1–3 | **Gate G3** |
| **Phase 4 – Differentiators and deliverables** | Days 20–35 | Optional VM driver, re-home, TLS and custom domains; reports, video, pitch, rehearsals | Submission |

The three tracks of Phase 2:

| Track | Steps | Starts | Depends on |
|---|---|---|---|
| **C – Control plane** (pure Go + Postgres, against fakes) | 6 → 7 → 8 | Day 3 | Step 0 only |
| **N – Network and agents** (on FelCloud) | 2 → 3 → 4, and 5 | After G1 (day 5–6) | G1 decisions |
| **U – Dashboard** (against a mock API generated from OpenAPI) | 12 (first half) | Day 5–8 | OpenAPI file from Step 6 |

If you work alone, interleave: mornings on track N (cloud, needs focus and your hands on the CLI), evenings on track C (code and tests). Track U waits until G2 unless a teammate takes it.

## 8.3 The gates

::: gate
**Gate G1 – end of Phase 1 (about day 5)**

Decide, from measured results written in `docs/notebook/experiments.md`:

| Decision | GO (primary) | NO-GO → fallback |
|---|---|---|
| HA mechanism | E1/E6: failover gap < 5 s on every injection, target < 2 s; floating IP follows the VIP every time | Floating IP fails but VIP works from inside → **F1** (notify script moves the floating IP). VIP itself fails → **F2** (Octavia `ovn` L4 load balancer, run E7 immediately) |
| Team networking | H1: ≥ 12 NICs hot-plug into a host VM, < 10 s per attach → **pod-NIC** | **AAP**, if E2 passes (ARP answered by the host's MAC, 10/10). If H2 shows a limit of 10 → CIDR mode (/27 per host) or one host per 10 teams. If E2 fails too → CIDR mode without reserved-port binding |
| Team pool | E3: port with fixed IP .192 created → pool .32–.191 | Pre-reserved warm ports in .200–.250, target reduced to 40 teams |
| MTU | E5: 100 MB download through a 1442-MTU network completes | MSS clamping on every path |

After G1, delete all lab resources (`openstack port list --tags cstam-lab` must be empty) and **do not revisit these decisions** without new facts.
:::

::: gate
**Gate G2 – one team end to end on FelCloud (about day 12)**

`POST /v1/hackathons/{id}/teams` → the subdomain answers through the floating IP from both gateways (failover in between); `DELETE` → route gone on both gateways (generation acknowledged) → port gone (`openstack port list --tags cstam-team:<id>` empty) → IP in QUARANTINE → FREE after 60 s.

NO-GO: stop all new features and fix integration. **Cut line 1** applies: the VM driver, host re-homing and DNS delegation are postponed.
:::

::: gate
**Gate G3 – every scored item demonstrable (about day 20)**

Each row of the requirement coverage table (chapter 5) has a working demo and a measurement. Feature freeze for scored items.

NO-GO: drop the remaining Phase 4 differentiators. **Cut line 2** applies: TLS, custom domains and re-homing are dropped if not started.
:::

## 8.4 The step list

| Step | Title | Goal (what exists at the end) | Points served | Depends on | Hours (estimate) |
|---|---|---|---|---|---|
| **0** | Setup | Repository, Go module, tooling, CI, `CLAUDE.md`, `cstamctl doctor`, mentor emails sent | – | – | 4 |
| **1** | Risk experiments | Measured answers to E1–E10, H1, H2; gate G1 decided | De-risks 40+ | 0 | 10 |
| **2** | OpenStack foundation as code | Pulumi program `infra/pulumi` (Go): router, 5 security groups, ports (VIP first), server group, keypair, VMs, floating IPs, cloud-init | All (prerequisite) | G1 | 5 |
| **3** | Gateway pair | HAProxy (slot backends) and keepalived configured on gw1/gw2; failover by hand measured | HA gateways (15) | 2 | 8 |
| **4** | Gateway agent (`gwagent`) | Watch desired state, runtime route updates, validated reload, verify, rollback, acks with digest, readiness gate, force-backup, logs | HA (15), reload/rollback (10) | 3, agent protocol from 6 | 14 |
| **5** | Team networking and host agent (`hostagent`) | By hand for one team, then the `NetDriver` (pod-NIC or AAP), hardened containers, firewall, reboot restore | Provisioning (15), isolation (5) | 2, G1 | 14 |
| **6** | Control plane skeleton, database and IPAM | `cpd` with config, migrations, schema, IPAM with concurrency tests, OpenAPI skeleton, agent endpoint and PKI | IPAM (10) | 0 | 12 |
| **7** | Provisioning controller | Claims with fencing, step pipeline, Neutron adapter with limiter, AAP syncer or warm pool, publisher, `POST /teams` | Provisioning (15), stress (15) | 6 | 14 |
| **8** | Teardown, generations and reconciler | Strict teardown, quarantine gated by acks, fence/rejoin, sweepers, drift scan, GC with circuit breaker, `edges.md` property test | Teardown & lifecycle (10), IPAM (10) | 7 | 12 |
| **9** | Security | Security groups verified, namespace/host firewall, hardening, mTLS checks, admin auth, audit chain, `cstamctl sectest` attack suite | Isolation (5), security docs | 5, 7 | 10 |
| **10** | End-to-end integration (gate G2) | One team end to end on FelCloud; e2e test suite; crash recovery of `cpd` | All | 4, 5, 8 | 6 |
| **11** | Stress, chaos and benchmark report | `cstam-bench` provisioning waves, failover and reload benchmarks, chaos scenarios, generated report | Stress (15), deliverable | 10 | 12 |
| **12** | Admin dashboard | All Phase 3 pages and controls against the real API, SSE live updates | Dashboard (10) | OpenAPI (6), 10 for wiring | 14 |
| **13** | Bonuses | Maintenance page, rate/bandwidth limits, Heat templates, TLS, custom domains | +2 each | 10 (G2) | 4–16 |
| **14** | Documentation, video and pitch | Technical report, API and security docs, diagrams, 5-minute video, pitch deck, rehearsals, backup clips | Deliverables, pitch (10) | 11, 12 | 12 |

Total core estimate (Steps 0–12, 14): about 150 hours. With a 5-week part-time schedule (about 30 hours per week), that leaves roughly one week of buffer, which you will need.

## 8.5 Dependencies and the critical path

```text
S0 ──► S1 (E1..E10, H1, H2) ──► G1 ──► S2 ──► S3 ──► S4 ─┐
 │                                     └────► S5 ─────────┤
 └──► S6 ──► S7 ──► S8 ────────────────────────────────────┼──► S10 (G2) ──► S11 ──► S14
          (OpenAPI) └──────────────► S12 (mock) ───────────┘          │       │
                                                                      ├──► S9 ┤
                                                                      ├──► S13┘
                                                                      └──► S12 (wired) ──► G3
```

**Critical path:** E1/H1 → G1 → S3/S4 and S5 → S10 (G2) → S11 → benchmark report → video. Any delay on this chain delays submission; everything else has slack.

Things that are easy to forget but sit on the critical path:

- **Mentor answers** (DNS delegation, quota, ownership of `adam` / `sbx-f0699704`). Sent on day 1.
- **The agent protocol** (`control.proto`) is needed by Step 4 and Step 5. Write it at the start of Step 6, even though most of Step 6 is IPAM.
- **Pre-pulled images** on hosts before any stress test.

## 8.6 What to cut if you are late

Keep, never cut: the VRRP pair and the live failover demo, reload/rollback with validation, the provisioning API, IPAM with quarantine, the 10-concurrent stress test, ordered teardown, the benchmark report and the security table.

Cut in this order (first = first to go):

1. gVisor strict mode (0 points)
2. VM-per-team driver and host re-homing (differentiators only)
3. Custom domain binding, then TLS (+2 each, highest effort)
4. Audit hash chain → plain append-only audit table
5. Own Go CA → shared token over TLS with a pinned server certificate (document as future work)
6. Second hackathon host → one host
7. Dashboard polish: Audit page, config diff viewer (keep the IP grid if at all possible: it is the best visual)
8. React → templ + htmx if the frontend blocks you
9. Chaos scenarios beyond the three scored failure types

## 8.7 Working rhythm with Claude Code

For every step:

1. Open plan mode for anything touching more than one package. Prompt: *"Read the guide section for Step N and the relevant ADRs. Explain the mechanism in your own words, then propose a plan: files, interfaces, tests, risks, and the Done-when checks. Do not write code yet."* Approve or correct the plan.
2. Run the by-hand part yourself; paste outputs into `docs/notebook/experiments.md`.
3. Ask for one small change at a time. After each diff: *"Explain line by line what this does, and give one input that would make it fail."*
4. Run the Done-when checks. Break the logic on purpose (invert a condition) and check that a test fails.
5. Run `/code-review` for the large steps (4, 5, 7, 8).
6. Answer the learning checkpoint in `docs/notebook/learning.md`. Commit.

::: learn
**Learning checkpoint**

1. Which steps can start before gate G1, and why is that safe?
2. Name the three gates and the fallback attached to each NO-GO.
3. Why is the agent protocol written at the start of Step 6 rather than in Step 4?
4. If you lose a week, which three items do you cut first, and how many points does that cost?
:::
