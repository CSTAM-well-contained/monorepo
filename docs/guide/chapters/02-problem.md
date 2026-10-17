# 2. The problem and how it is scored

## 2.1 The problem in FelCloud's words

FelCloud hosts high-density hackathons on its OpenStack cloud. Today it uses **static subnets** and **manually edited reverse proxies**. When many teams deploy at the same time, or when someone breaks the proxy configuration, two things happen:

- **Network downtime.** One proxy carries every team. If it crashes, or if a typo is reloaded, every team goes offline at once.
- **IP conflicts.** IP addresses are chosen by hand and reused while something still points at them.

The brief asks for a system that:

1. provisions isolated team sandboxes automatically through an API, each with a dynamically assigned private OpenStack IP;
2. routes team subdomains (`team5.cstam.felcloud.tn`) through **redundant HA gateways** with instant failover;
3. **validates and hot-reloads** routing rules, and **rolls back** a bad configuration automatically in under a second;
4. manages the IP pool with **short leases** and **immediate, safe reuse**;
5. survives **10 or more simultaneous registrations**;
6. cleans up expired sandboxes, routes and IPs automatically;
7. isolates teams from each other and from the management layer;
8. offers an **admin dashboard**.

![Figure 2.1 – Today: every team depends on one proxy, edited by hand, in front of IPs nobody manages.](diagrams/f01-problem.png)

## 2.2 Reading between the lines

A brief is written by people with a specific pain. Each phrase below hides a concrete expectation that a judge will check.

| Brief phrase | What it really means for FelCloud | What a judge will check |
|---|---|---|
| "IPv4 address scarcity" | Public IPv4 is the expensive resource. They do not want one floating IP per team. They want **one public IP for the whole event** and routing by name behind it. Private IPs are scarce too: Neutron's automatic pool on our subnet has only 51 addresses. | Number of floating IPs used = 1. One private IP per team. The IP comes back when the team is gone. |
| "Gateway single points of failure" | Today one hand-edited proxy carries everything. | Kill the primary gateway live and nothing breaks. Then: "what about the control plane? the host?" They will ask. |
| "IP conflicts" | Static subnets, hand-picked IPs, IPs reused while still referenced. This is exactly the bug in `edges.md`: a stale route sends team A's subdomain to team B's app. | Concurrent registrations never get the same IP. A released IP cannot be reused while any route still points at it. |
| "Aggressively recycling private IPs" | Short leases and fast reuse, but reuse must be **safe**. | The "IP recycling speed" metric in the benchmark: time from teardown request to IP reusable. |
| "Without disrupting existing infrastructure" | This is a hosting company with real customers on a shared cloud. Do not break shared networks, do not need admin rights, do not burn their Let's Encrypt quota on `felcloud.tn`. Adding a team must not drop traffic for the other teams. | Every action is tenant-scoped and reversible. Zero failed requests during reloads. No admin-only APIs. A clean uninstall. |
| "Developer sandbox automation" (company context) | They already automate something (the existing `sbx-*` VM looks like their own tool). They want something they could **adopt**, not a toy. | Clean API, idempotency, a reconciler, documentation, maintainable code. |
| "Rollback capabilities" | A broken config must never stay live, and an operator must be able to return to version N−1 by hand. | A deliberately broken config is rejected; a valid-but-wrong config is rolled back automatically; a "rollback to vN" button exists. |

## 2.3 The score map: easy points and hard points

| Item | Points | Difficulty | Why | Where points get lost |
|---|---|---|---|---|
| Automated OpenStack sandbox provisioning | 15 | Medium | The plumbing is known; the cloud specifics (OVN, MTU) are the risk | Judges think "a container is not an OpenStack sandbox" if the team IP is not a real Neutron IP |
| Dual redundant HA gateways | 15 | **Hard** (FelCloud-specific risk) | VRRP, a VIP and a floating IP on OVN virtual ports is the riskiest network behavior in the project | Failover takes 10+ seconds; the backup has stale config; split-brain; failback causes a second outage |
| Automated IPAM & reclamation | 10 | Easy–medium | Postgres plus a state machine | Not linked to Neutron (judges want to see ports appear and disappear in the OpenStack dashboard); no real "short lease" semantics |
| Zero-downtime reload & auto-rollback | 10 | Medium | HAProxy does the heavy lifting | Runtime changes lost on the next reload; rollback slower than 1 s; dropped requests during reload |
| Concurrent provisioning stress test | 15 | **Hard** (correctness under concurrency) | Races in IPAM, in allowed-address-pair updates, in gateway updates | One duplicate IP or one failed team out of 20 means "not error-free" |
| Automated teardown & lifecycle security | 10 | Medium | Ordering plus a reconciler | The `edges.md` bug; leaked ports; forgotten DNS/proxy rules |
| Network isolation & access safeguards | 5 | Easy | Security groups plus host firewall | No *test* proving it; metadata service reachable from containers |
| Admin dashboard | 10 | Easy | A frontend over our API | One of the four required controls missing; "live traffic logs" faked |
| Pitch and live demo | 10 | **Hard** (fragility) | Live cloud, conference network, timing | The demo fails live; no numbers |
| Bonuses | +2 each | Varies | See Step 13 | |

Two lessons follow from this table:

1. **The hard items are about correctness and the cloud, not about code volume.** That is why the build flow (chapter 8) tests the cloud's network behavior first and puts concurrency tests at the center of the control plane.
2. **The easy items are lost by missing evidence, not by missing features.** Isolation is worth 5 points, but only if an automated attack suite proves it. The dashboard is worth 10, but only if every listed control works against the real system.

## 2.4 The selection gate

Only selected teams advance to the final pitch. Selection is based on what you **submit**: the source repository, the architecture and data-flow diagram, the video, the benchmark report, the API and security documentation, and the technical report.

Most teams treat these as an afterthought written in the last night. Treat them as features:

- The **benchmark report** is generated automatically from benchmark runs (Step 11).
- The **security documentation** is generated from the attack test suite (Step 9).
- The **API documentation** is the OpenAPI file you write anyway (chapter 7).
- The **video** follows a fixed script mapped one-to-one to the brief (Step 14).

## 2.5 Definitions we commit to

Vague words lose arguments with engineers. We define the important words now and use these definitions in the code, the benchmark report and the pitch.

| Word | Our definition | How we measure it |
|---|---|---|
| **Failover gap** | The longest run of consecutive failed or timed-out requests seen by an external prober that opens a **new connection every 10–50 ms** through the floating IP, from a laptop outside FelCloud | Go probe (Step 1 and Step 11), first `gw2 200` minus last `gw1 200` |
| **Seamless failover** | After a failover gap of X ms, **every new connection succeeds** without any human action, DNS change or client change. Connections that were in flight on the dead gateway are reset, because HAProxy is a full proxy and no TCP state is shared. We say this honestly. | Failover gap and lost-request count per failure type |
| **Zero-downtime reload** | 0 failed requests across a sustained load test (for example 60,000 requests) while routes are changed and the configuration is reloaded and rolled back many times | Load generator at constant rate, with and without keep-alive, cross-checked with HAProxy counters |
| **Sub-second rollback** | From the first failed post-apply health probe to the moment the previous configuration is verified live again: under 1 s | Timings recorded by the gateway agent |
| **Error-free stress test** | N simultaneous registrations, N teams reach ACTIVE, N distinct IPs, 0 errors, 0 leaked Neutron ports after cleanup | `cstam-bench` plus invariant checks |
| **IP recycling time** | (a) teardown request → IP safe to reuse (all gateways acknowledged the route removal); (b) → IP `FREE` (adds the quarantine) | Database timestamps on IP state changes |

Targets (to be confirmed by measurement):

| Event | Target p50 / p95 |
|---|---|
| HAProxy process killed on the master | < 1.0 s / < 1.5 s |
| Master VM hard crash | < 2.0 s / < 3.0 s |
| Hot reload (team added or removed) | 0 failed requests |
| Invalid config pushed | rejected in < 200 ms, 0 lost requests |
| Valid-but-broken config | rolled back in < 1 s |
| 10 simultaneous registrations | 10/10 ACTIVE, p95 < 10 s |
| 50 simultaneous registrations | 50/50 ACTIVE, p95 < 30 s |

::: learn
**Learning checkpoint**

1. Why does "IPv4 scarcity" lead to routing by hostname instead of one floating IP per team?
2. Write the `edges.md` bug as a timeline of four events. Which event must never happen before which other one?
3. Why is "seamless failover" defined with *new* connections, and what happens to an open WebSocket on the dead gateway?
4. Which two scored items are hard because of FelCloud's network rather than because of code?
:::
