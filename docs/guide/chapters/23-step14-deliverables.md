# 23. Step 14 – Documentation, demo video and pitch

::: note
**Goal** · Every deliverable the brief asks for exists, is consistent with the running system, and the demo is rehearsed until it is boring.
**Points served** · Selection for the final (decided on what you submit), Phase 3 documentation and video, Phase 4 pitch (10), and indirectly every scored item, because the jury only scores what it sees.
**Time estimate** · 15–20 hours spread over the last week (reports 6 h, video 4 h, deck 3 h, rehearsals 5 × 1 h).
**Depends on** · Gate G3 (Chapter 22's bonuses are optional). The benchmark report comes from Step 11, the security table from Step 9.
:::

Most teams treat documentation and the video as an afterthought. The brief says: **only selected teams advance to the final pitch**, and selection is made on what you **submit**. A clear report, real numbers and a tight video are therefore not decoration. They are the gate to the 10 pitch points and to the jury seeing your live demo at all.

## 14.1 Understand first: what the jury will actually look at

Judges have little time. They will open the repository README, watch the first minute of the video, look at the architecture diagram and skim the benchmark summary table. Design every deliverable for that reading order:

1. **One sentence and one diagram** that explain the whole system (Figure 5.1 is ready).
2. **One table of numbers** with p50/p95 and the command that produced them.
3. **One video** whose chapters map one-to-one to the brief's demo list.
4. Everything else (API reference, security architecture, technical report) must exist and be correct, because a judge who checks one claim in depth must find it supported.

::: why
**Why "measured, not claimed" wins**
Every team will write "instant failover" and "zero downtime". Very few will write "failover 1.9 s p50 / 2.6 s p95 over 30 hard crashes, 0 failed requests over 60 000 requests during 20 reloads, script `bench/failover`". Numbers with a method are the single strongest differentiator you have (Chapter 4, section on first-place differentiators).
:::

## 14.2 Deliverables checklist mapped to the brief

| Brief phase | Deliverable | Where it comes from | Format | Done when |
|---|---|---|---|---|
| Phase 1 | Source code repository | the repo | GitHub (private until submission, then shared) | `make build test lint` green on a fresh clone; README quick start works |
| Phase 1 | System architecture & OpenStack HA gateway data-flow diagram | `docs/diagrams/` (Figures 5.1, 10.1, 13.1, 14.1) | PNG + editable `.excalidraw` | Every box and IP matches the deployed system (`topology.env`) |
| Phase 1 | Short video: subdomain routing, hot reload, primary gateway failover | section 14.6 | MP4, ≤ 5 min | Chapters match the brief's three demo items |
| Phase 2 | Benchmark report (provisioning latency, failover time, IP recycling, load) | Step 11 `make bench-all` | `docs/benchmark-report.md` + PDF | Every number has a script, a run count and raw data hash |
| Phase 2 | API integration documentation | OpenAPI spec (section 14.4) | `api/openapi/cstam.v1.yaml` + Redoc HTML | Every endpoint has an example request and response |
| Phase 2 | Security architecture documentation | Step 9 + `cstam-sectest` report | `docs/security.md` + PDF | Threat table, layer diagram, latest attack table (all PASS) |
| Phase 3 | Admin dashboard + frontend source | `web/`, embedded in `cpd` | source + screenshots | All four Phase-3 bullets visible and working (Step 12) |
| Phase 3 | Technical report | section 14.3 | PDF, 15–25 pages | Reviewed against this checklist |
| Phase 3 | Demo video (features and functionality) | same video, or a longer cut | MP4 | Covers every scored item |
| Phase 4 | Presentation | section 14.7 | PDF + source | 10–12 slides, one number per slide |
| Phase 4 | Live demo incl. simulated primary crash on OpenStack | section 14.8 | live + backup clips | 5 timed rehearsals done |

Keep this table in `docs/deliverables.md` and tick it as you go.

## 14.3 The technical report

Write it in Markdown in the repo (`docs/report/`) and build the PDF with the same pandoc + headless Chrome pipeline as this guide. Outline:

1. **Summary** (half a page): the problem, the one-sentence solution, the headline numbers.
2. **Context and requirements**: the brief's items and how you read them (Chapter 2 of this guide is your draft).
3. **The FelCloud environment**: verified facts and constraints (Chapter 3): quota, OVN, MTU 1442, allocation pool, missing services.
4. **Architecture**: Figure 5.1, the component list, the request path, the four end-to-end walkthroughs (Chapter 5).
5. **Key decisions**: 6–8 short decision records with the options and why the others lost (pick from Chapter 6). Include "why not Kubernetes", "why not Octavia L7", "why pod-NIC" (or "why AAP" if G1 chose the fallback), "why a modular monolith".
6. **Implementation**:
   - HA gateways: VRRP, VIP on OVN, readiness gate.
   - Reload and rollback: the two gates, timings.
   - Provisioning: controller, fencing, Neutron idempotency, warm pool.
   - IPAM and teardown: state machine, generations, the safety property and its proof sketch.
   - Security: the five layers.
7. **Results**: the benchmark summary table and three charts (provisioning by concurrency, failover timeline, reload under load), plus the security attack table.
8. **Experiments and what changed**: the Step 1 results from `docs/notebook/experiments.md`. Showing that you tested assumptions and changed the design when a test failed is a strong signal of engineering maturity.
9. **Limitations and future work**: be honest: in-flight connections on failover, host VM as a failure domain per hackathon, single control-plane database, split brain detection rather than prevention, what FelCloud could add (Designate, quota, Heat in production).
10. **Appendix**: how to reproduce (`make` targets), configuration reference, glossary (copy Chapter 28).

::: note
**Reuse, do not rewrite**
Most of this report already exists in `docs/notebook/experiments.md`, the benchmark report, the security report and this guide. Your job is to select, shorten and make it consistent with what is deployed. Ask Claude Code to draft each section from those sources, then edit it yourself so that every sentence is something you can defend.
:::

## 14.4 API documentation

The OpenAPI file (`api/openapi/cstam.v1.yaml`) is already the source of truth for the server stubs and the dashboard types (Steps 6, 7, 12). Make it good documentation too:

- Every operation has a `summary`, a `description` that explains the asynchronous model (202 + `phase` + `conditions`), and at least one `example` for request and response.
- Error responses use the RFC 9457 `application/problem+json` schema with examples (`pool-exhausted`, `name-taken`, `lease-expired`).
- Document the headers: `Idempotency-Key`, `X-CSRF`, `Last-Event-ID` for SSE.
- Add a short "Integration guide" section in `docs/api.md`: authenticate, create a hackathon, register a team, watch it become ACTIVE through SSE, renew, delete. Use `curl` examples that you actually ran.

Render a static HTML reference:

```bash
npx @redocly/cli build-docs api/openapi/cstam.v1.yaml -o docs/api/index.html
npx @redocly/cli lint api/openapi/cstam.v1.yaml      # no errors before submission
```

The agent protocol (Connect-RPC, `api/proto/`) is internal, but document it in one page: the services, who dials whom, the generation and digest rules, and the boot rule. It is the part a FelCloud engineer would need to operate the system.

## 14.5 Security architecture document

Build `docs/security.md` from Step 9:

1. Threat model: assets, actors, trust boundaries (section 9.1).
2. Figure 18.1 (defence in depth) and one paragraph per layer.
3. The threat table with test IDs.
4. **The latest `cstam-sectest` report pasted verbatim**, with the date, the git commit and the counter deltas.
5. Secrets inventory and the restricted application credential (show the access rules, not the secret).
6. Known limitations: shared kernel (and the optional gVisor mode), the host agent's privileges, split brain.

The attack table is the most convincing part. A judge who sees "S04 metadata: blocked, `pod` output rule 169.254/16 +3 packets" knows you tested it.

## 14.6 The demo video (≤ 5 minutes)

### 14.6.1 Preflight checklist (run 30 minutes before recording or presenting)

- `make preflight` (a script you write in Step 11 or now):
  - both gateways healthy and on the same generation;
  - cp up; host agents connected;
  - `chronyc tracking` offset < 5 ms on all VMs;
  - 3 demo teams ACTIVE (`team1`, `team2`, `demo-sec`);
  - ≥ 30 FREE IPs;
  - quarantine set to 20 s for the demo (say so on screen);
  - demo mode on (`CSTAM_DEMO_MODE=1` enables "simulate crash").
- Terminal layout (tmux, font 18 pt): pane 1 the load generator printing ok/error per second; pane 2 commands; pane 3 `journalctl -fu keepalived` on gw2.
- Browser: dashboard Overview, plus a tab on `team1.<hackathon>.<BASE_DOMAIN>`.
- Close notifications, use a clean browser profile, hide bookmarks.

### 14.6.2 Script

| Time | Show | Say (short) |
|---|---|---|
| 0:00–0:20 | Title, Figure 5.1 | "One public IP, two HA gateways, a real Neutron port per team, and a control plane that never lets a bad config or a stale route survive." |
| 0:20–0:50 | Browser: `team1…` and `team2…` show different apps; `nope…` gives 404 | Subdomain routing by `Host` header through the VIP. |
| 0:50–1:30 | Dashboard: spin up `team3`; the provisioning steps tick live; the IPAM grid cell turns green; URL works | API-triggered provisioning, the team's OpenStack port shown (also visible in Skyline). |
| 1:30–2:20 | Load generator at 300 req/s. Add and remove routes, 5 hot reloads. Push a broken config: "rejected by validation". Push a valid-but-wrong config: "rolled back in 0.4 s". Counter: **0 errors**. | The two safety gates. |
| 2:20–3:10 | Gateway card gw1 MASTER. "Simulate crash" (or kernel panic via SSH). Load generator shows a short gap, `X-Served-By` flips to gw2, the card flips, the timeline shows the measured failover time. | Failover measured, not claimed. Explain "new connections succeed; open ones on the dead gateway are reset". |
| 3:10–3:40 | `cstamctl bench register --n 10 --concurrency 10`: all ACTIVE, latency table | Stress test, 10+ concurrent registrations. |
| 3:40–4:10 | Delete team3. IPAM grid: ACTIVE → RELEASING → QUARANTINE (countdown) → FREE. Old URL → 404 | Safe recycling; the `edges.md` bug cannot happen. |
| 4:10–4:45 | `make sectest` summary (all PASS) and one live attempt: team1 → team2 and → metadata time out; counters | Isolation, proven by attacks. |
| 4:45–5:00 | Recap table: requirement → evidence; repo link | |

### 14.6.3 Recording tips

- Record each row as a **separate clip** (OBS Studio, 1080p, 30 fps). Clips are easier to redo, and the same clips are your live-demo backup (section 14.8).
- Record narration separately and calmly; cut pauses. Add chapter markers matching the brief's wording ("Active subdomain routing", "Dynamic hot-reloading", "Simulated primary gateway failover").
- Zoom the screen region that matters (the load generator counter, the gateway cards). A judge on a laptop cannot read a full 4K terminal.
- Show real timestamps; do not speed up the failover part.

## 14.7 The pitch deck (10–12 slides)

| # | Slide | Content |
|---|---|---|
| 1 | Title | Name, team, one-sentence solution |
| 2 | The problem | Figure 2.1 (today: one hand-edited proxy, static IPs); the three root causes |
| 3 | What FelCloud's cloud really offers | 4 facts that shaped the design (OVN, MTU 1442, allocation pool, quota) |
| 4 | Architecture | Figure 5.1, the request path in one line |
| 5 | HA gateways | VRRP + VIP on OVN, readiness gate; **number**: failover p50/p95 |
| 6 | Zero-downtime reload and rollback | Figure 13.1; **number**: 0 / 60 000 failed, rollback < 1 s |
| 7 | Provisioning and IPAM | pod-NIC (or AAP), warm pool, state machine; **number**: 10/20 concurrent p95 |
| 8 | Safe recycling | the `edges.md` timeline, generations and acks; **number**: IP reusable after X s, 0 leaks over 500 cycles |
| 9 | Isolation | Figure 18.1; **number**: 30/30 attacks blocked |
| 10 | Dashboard | one screenshot of Overview + IPAM grid |
| 11 | What we would do next for FelCloud | Heat, Designate, control-plane HA, quota |
| 12 | Thank you / Q&A | repo link, contact |

Rules: one message and one number per slide; diagrams from this guide; no code on slides; the same vocabulary as the dashboard.

## 14.8 The live pitch (about 10 minutes)

| Minutes | Content |
|---|---|
| 0–1 | Problem (slide 2) |
| 1–2.5 | Architecture and three decisions with "why" (slides 4–5) |
| 2.5–3 | Switch to live: load generator already running at 200 req/s, dashboard visible |
| 3–4 | Routing + spin up a team live (let a jury member choose the name; if they type something weird, the validation answers) |
| 4–5 | Broken config rejected, valid-but-wrong config rolled back, 0 errors |
| 5–6.5 | **The required moment: primary gateway crash on OpenStack.** Ask a jury member to say "now". `ssh gw1 'echo c \| sudo tee /proc/sysrq-trigger'` or `openstack server stop cstam-gw1`. Show the gap, the flip, the measured time. Then start gw1 again: it stays BACKUP (`nopreempt`), 0 errors |
| 6.5–7.5 | 10 concurrent registrations, then a teardown with the quarantine countdown on the IPAM grid |
| 7.5–8.5 | Security: two live attack attempts + the sectest summary; benchmark slide |
| 8.5–10 | Next steps for FelCloud, close, questions (Chapter 26) |

### 14.8.1 Backup plan

- **Decide the rule in advance**: if any live step fails twice or takes more than 30 s, say "let me show you the recording of exactly this step", play that clip, then return to live for the next step.
- `cstamctl demo reset` (idempotent, about 20 s): recreates the demo teams, clears force-backup flags, restores the tagged demo config version.
- If the FelCloud API is slow or down: everything except provisioning still works, because the data plane does not depend on the control plane or on Neutron. Say it: it is a feature, not an excuse.
- If the venue network blocks the floating IP: dashboard through the SSH tunnel over a mobile hotspot; worst case, the full video.
- Carry the video and the deck on the laptop **and** on a USB key, and the deck as PDF.

### 14.8.2 Rehearsals

Rehearse the full pitch **five times on the real system**, timed:
1. Alone, with notes, to fix the order.
2. Without notes.
3. Starting with gw2 as MASTER (the failover is then gw2 → gw1; the dashboard must still read correctly).
4. With one injected problem (stop a host agent before the demo) to practise the backup rule.
5. In front of a friend who asks the questions from Chapter 26.

After each rehearsal write down what went wrong in `docs/rehearsals.md` and fix the cause, not the symptom.

## Done when

::: done
**Done when**
- Every row of `docs/deliverables.md` is ticked, and each artefact has been opened from a fresh clone (not only from your working copy).
- The video is ≤ 5 minutes, chapters match the brief, numbers on screen match the benchmark report.
- `npx @redocly/cli lint` reports no errors; `docs/api/index.html` opens offline.
- `docs/security.md` contains a sectest report from the last 48 hours, all PASS.
- Five rehearsals are logged in `docs/rehearsals.md`; the last one was on time with no backup clip needed.
:::

## Pitfalls

::: warn
**Common ways to lose points at the end**
- Numbers in the slides that differ from the report (different runs). Generate both from the same `bench/results/<run-id>`.
- A diagram that still shows v1 (Caddy per team, AAP) while the system runs pod-NIC, or the reverse. Judges notice.
- A video recorded before the last bug fix, showing behaviour that no longer exists.
- Demoing on conference Wi-Fi without testing the floating IP from that network.
- Claiming "zero downtime failover". Say what you measured: the gap for new connections, and that open connections on the dead gateway reset.
- Pushing secrets in the final repository snapshot (run `gitleaks detect` on the whole history before sharing).
:::

## Learning checkpoint

::: learn
**Answer these yourself before moving on**
1. In one sentence, without notes: what does the system do and why is it safe?
2. Which three numbers will you say on stage, and which script produced each?
3. What do you say if the live failover takes 6 s instead of 2 s?
4. What is the honest limitation you will mention first, and why does mentioning it help you?
:::

## Driving Claude Code for this step

::: claude
- Drafting: *"Using docs/notebook/experiments.md, docs/benchmark-report.md and reports/security/latest.md, draft section 7 'Results' of the technical report. Every number must cite its source file and run ID. Do not invent any number; leave a TODO where data is missing."*
- Consistency check: *"Compare docs/report/*.md, the slide notes and topology.env. List every IP, name, port or number that differs between them."*
- Jury review: *"Act as a skeptical FelCloud engineer. Read the report and list the ten claims you would challenge first, with the evidence you would ask for."*
- API docs: *"Add examples and descriptions to every operation in api/openapi/cstam.v1.yaml using the real responses in docs/api-examples/. Run redocly lint and fix all errors."*
- Never let the agent record, upload or share anything; it writes text, you publish.
:::
