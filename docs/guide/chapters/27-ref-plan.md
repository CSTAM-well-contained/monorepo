# 27. Reference D – Schedule, milestones and cut lines

This chapter turns the step list of chapter 8 into a calendar. It assumes **5 weeks, part-time, about 30 hours per week**, working alone. Adjust the dates, not the order.

![Figure 27.1 – The five-week plan: milestones, gates and cumulative points.](diagrams/f16-timeline.png)

## 27.1 Assumptions

- About 150 hours for the core (Steps 0–12 and 14), plus 4–16 hours of bonuses. Week 5 is buffer and polish.
- Cloud work (Steps 1–5) needs your full attention and your hands on the CLI. Pure code work (Steps 6–8) can be done in shorter sessions.
- Every hour estimate is a guess. Record real hours in `docs/notebook/experiments.md`; after week 1, rescale the rest of the plan.

## 27.2 Milestones

| Milestone | Week | Steps | Hours (est.) | Points unlocked | Definition of done |
|---|---|---|---|---|---|
| **M0 – Setup** | W1, day 1 | 0 | 4 | – | CI green; `gitleaks detect` clean; `CLAUDE.md` read back by Claude; mentor email sent |
| **M1 – Risks burned down** | W1, days 2–5 | 1 | 10 | de-risks 40+ | E1–E10, H1, H2 recorded; **gate G1** decided and written in the decisions log; lab resources deleted |
| **M2 – Control plane core** (track C, starts day 3) | W1–W2 | 6 | 12 | IPAM (10) | `TestConcurrentLease` passes 10× with `-race`; schema + seed applied; OpenAPI skeleton; `control.proto` written |
| **M3 – Gateways** | W2 | 2, 3, 4 | 27 | HA (15), reload/rollback (10) | Failover measured through either gateway; broken config rejected; bad-valid config rolled back < 1 s; 0 failed requests over 20 reloads |
| **M4 – Host agent** | W2–W3 | 5 | 14 | part of provisioning (15), isolation (5) | One team by hand, then by agent; `Ensure` twice = no change; reboot restore works |
| **M5 – Provisioning and teardown** | W3 | 7, 8 | 26 | provisioning (15), teardown (10) | `POST /teams` → ACTIVE; crash tests pass; `TestEdgesMdScenario`, `TestTeardownWithBackupDown` green |
| **M6 – Gate G2** | end of W3 | 10 | 6 | all | One team end to end on FelCloud through both gateways; e2e suite green; no leftovers after DELETE |
| **M7 – Scoring** | W4 | 9, 11, 12 | 36 | isolation (5), stress (15), dashboard (10) | `cstamctl sectest` all blocked; `make bench-all` generates the report; every dashboard control works; **gate G3** |
| **M8 – Deliverables** | W5 | 13, 14 | 16–28 | bonuses, pitch (10) | Technical report, API and security docs, ≤ 5-min video, pitch deck, 5 timed rehearsals, backup clips |

## 27.3 Week by week (one person)

| Week | Mornings (cloud, focus) | Evenings (code) | End-of-week check |
|---|---|---|---|
| **W1** | Step 0; Step 1 experiments E3, E1, E6, H1, H2, E2, E4, E5, E9, E10 | From day 3: Step 6 schema, IPAM, tests against testcontainers | **G1** decided; IPAM concurrency test green |
| **W2** | Step 2 foundation script; Step 3 gateways by hand; Step 4 gateway agent | Step 6 finish (OpenAPI, agent endpoint, PKI); Step 7 controller against fakes | Failover and reload numbers in `experiments.md` |
| **W3** | Step 5 host networking by hand, then host agent | Step 7 Neutron adapter + warm pool; Step 8 teardown, generations, reconciler | **G2**: one team end to end |
| **W4** | Step 11 stress + failover benchmarks, chaos | Step 9 security suite; Step 12 dashboard | **G3**: every scored item demoable; feature freeze |
| **W5** | Bonuses 1–3 (maintenance page, rate limit, Heat); rehearsals on the real cloud | Step 14: report, docs, video editing, deck | Submission |

## 27.4 With a team of two or three

| Person | Weeks 1–2 | Weeks 3–4 | Week 5 |
|---|---|---|---|
| **A – network** | Steps 1, 2, 3, 4 | Step 5, chaos, failover benchmark | Demo script, live failover rehearsal |
| **B – control plane** | Steps 6, 7 | Steps 8, 10, stress benchmark | Technical report, API docs |
| **C – dashboard and security** (optional third person) | Dashboard against the mock API (from the OpenAPI file); `cstam-sectest` harness | Step 9, dashboard wired to the real API | Video, slides, security doc |

The contract between people is the OpenAPI file and the `.proto` files. Change them only through a reviewed pull request.

## 27.5 Gates and what they decide

| Gate | When | Question | If NO-GO |
|---|---|---|---|
| **G1** | End of W1 | Does the VIP + floating IP fail over on OVN? Can we hot-plug ≥ 12 NICs? | HA: F1 (notify moves the FIP) or F2 (Octavia `ovn` L4 LB). Team network: AAP model, CIDR mode if the AAP limit is 10 |
| **G2** | End of W3 | Does one team work end to end on FelCloud? | Stop new features, fix integration. Apply cut line 1 |
| **G3** | End of W4 | Is every scored item demonstrable with a measurement? | Drop the remaining differentiators. Apply cut line 2 |

## 27.6 Risk buffer

- Week 5 is not planned work. If it is free, spend it on bonuses and rehearsals.
- Each milestone already has about 20 % slack in its hours.
- The three biggest schedule risks and their early warning signs:

| Risk | Early sign | Response |
|---|---|---|
| OVN does not move the VIP / FIP | E1 fails on day 2–3 | Switch to F1 the same day; F2 by the end of W1 |
| Hot-plug NICs limited (q35) | H1 fails after 1–2 NICs | AAP model; budget 1 extra day in W3 |
| Mentors do not answer about DNS | No reply by day 4 | sslip.io for everything; TLS bonus becomes self-signed or is cut |

## 27.7 What to cut, by points per hour

Never cut (they carry most of the points and the required deliverables):

- the VRRP pair and the live failover demo (15 + pitch)
- reload/rollback with validation (10)
- the provisioning API on real Neutron ports (15)
- IPAM with quarantine (10) and ordered teardown (10)
- the 10-concurrent stress test (15)
- the benchmark report and the security table

Cut in this order:

| Order | Item | Points lost | Hours saved (est.) |
|---|---|---|---|
| 1 | gVisor strict mode | 0 | 3 |
| 2 | VM-per-team driver, host re-homing | 0 (+2 Heat stays possible via infra stack) | 10 |
| 3 | Custom domain binding | 2 | 4 |
| 4 | TLS bonus | 2 | 8 |
| 5 | Audit hash chain → plain append-only table | 0 | 2 |
| 6 | Own Go CA → token over TLS with pinned server certificate | small (security doc) | 4 |
| 7 | Second hackathon host | 0 (S03 tested by reasoning only) | 3 |
| 8 | Dashboard polish (Audit page, diff viewer; keep the IPAM grid) | small | 6 |
| 9 | React → templ + htmx | small | varies |
| 10 | Chaos scenarios beyond the three scored failure types | 0 | 4 |

::: learn
**Learning checkpoint**

1. Which milestone is on the critical path in each week?
2. You are at the end of W3 and G2 is not reached. What exactly do you stop doing?
3. Which two cut-list items cost real points, and why are they still cut before the dashboard?
:::
