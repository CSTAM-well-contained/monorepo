# 28. Reference E – Working with Claude Code, and glossary

## 28.1 The goal: agentic coding without "vibe coding"

Claude Code writes a lot of the code in this project. That is fine, on one condition: **you must be able to explain every line that reaches `main`**. The jury will test this (question 25 in chapter 26), and FelCloud will judge you as an engineer, not as a prompt writer. The rules below make that possible.

## 28.2 The final CLAUDE.md

This is the file from Step 0 (chapter 9), repeated here so the reference is complete. Keep it at the repository root; Claude Code reads it at the start of every session.

```markdown
# CSTAM project rules for Claude Code

## Who I am and how we work
- I am a student building this to learn. Explain before you code: the mechanism (Linux, OpenStack,
  HAProxy, Postgres), the plan, the files you will touch, and how we will verify it.
- Small steps: one package or one behaviour per change, under ~300 lines of diff.
- Every change comes with tests and a "done when" command I can run myself.
- When you are unsure about a FelCloud behaviour, say so and propose an experiment. Do not guess.

## Hard rules
- NEVER run commands that create, modify or delete OpenStack resources. Propose them; I run them.
  Read-only `openstack ... list/show` is fine.
- NEVER read, print or copy .secrets/, *.key, *.pem, clouds.yaml, *.env.
- No new Go dependency without asking me first.
- External commands: exec with an argv slice, never `sh -c` with string concatenation.
- All names that reach HAProxy, nftables, iptables or Docker are validated with
  ^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$ before use.

## Code conventions
- Go 1.25, one module. Binaries in cmd/, everything else in internal/.
- IPs and names come from internal/config/topology.go and infra/topology.env. No hardcoded IPs elsewhere.
- Errors: wrap with %w. No panics in agents. context.Context with a timeout on every network call.
- Logging: log/slog JSON. Never log request bodies sent to OpenStack.
- Tests: table-driven. Integration tests use testcontainers-go and the build tag `integration`.
  Tests needing root on a host use `hostint`; tests needing FelCloud use `e2e`.
- Agents (gwagent, hostagent) never import pgx or gophercloud (enforced by depguard).
- SQL lives in internal/store/queries/*.sql (sqlc). Real SQL, no ORM.

## Lab notebook
- Record every experiment, measurement and decision in docs/notebook/experiments.md
  (command, output, date, conclusion).
```

To make the cloud rule technical and not only a promise, add a deny rule in `.claude/settings.json` for write commands (`openstack * create*`, `openstack * delete*`, `openstack * set*`, `openstack * unset*`, `openstack server *`). Use the `/update-config` helper or edit the file yourself.

## 28.3 The per-step workflow

1. **Branch.** `git switch -c step7-provisioning`. One branch per step or sub-step.
2. **Plan mode** (Shift+Tab) for anything that touches more than one package. Ask for a plan, read it, correct it. No code yet.
3. **By hand first** for anything that touches the cloud or the kernel (Steps 1–5): run the commands yourself, paste the output into `docs/notebook/experiments.md`.
4. **Tests first** for logic (IPAM, state machines, teardown order): ask for the failing test, read it, then ask for the code.
5. **Small diffs.** One behaviour at a time. Ask for an explanation of every diff.
6. **Break it on purpose.** Invert a condition, remove a `WHERE` guard, swap two teardown steps. A test must fail. If none fails, the test is missing.
7. **Review.** Run `/code-review` for Steps 4, 5, 7, 8 and 9. Go through the checklist below.
8. **Learning checkpoint.** Answer the step's questions in `docs/notebook/learning.md` without help.
9. **Commit and merge.** The pull request description is your lab note: what, why, how verified.

## 28.4 Review checklist (before every merge)

- [ ] I can explain each new function in one or two sentences.
- [ ] Tests fail when I break the logic on purpose.
- [ ] No secrets, no hardcoded IPs outside `topology.go` / `topology.env`.
- [ ] Every network call has a context with a timeout; no DB transaction is open across a network call.
- [ ] Every state change in SQL has a `WHERE state = …` guard (and `claim_epoch` where the row is claimed).
- [ ] Every external effect is idempotent ("is it already done?" is checked first).
- [ ] Names reaching HAProxy, nft or Docker are validated.
- [ ] External commands use argv, never `sh -c`.
- [ ] `go vet`, `golangci-lint run`, `go test -race ./...` are green.
- [ ] Teardown order unchanged, or the change is explained and tested.

## 28.5 Prompt templates

**Explain first**
> Read section `<N.M>` of my guide and the files `<paths>`. Explain the mechanism in your own words: what happens in Linux / OpenStack / HAProxy / Postgres. Then propose a plan with files, interfaces, tests, risks and the Done-when commands. Do not write code.

**Test first**
> Write only the failing test for `<behaviour>`. Use a table-driven test. Show me which assertion fails today and why.

**Small change**
> Implement only `<function>` so that `<test>` passes. Do not change other files. Then explain the diff line by line and give one input that would make it fail.

**Infrastructure (no execution)**
> Write the exact `openstack` commands for `<task>`, in the order I must run them, with the expected output of each. Do not run anything. Mark which commands create or delete resources.

**Review as the jury**
> Act as a skeptical FelCloud engineer on the jury. Read `<package>`. List five questions this code would fail, and for each one the test or experiment that would answer it.

**Debug**
> Here is the symptom `<output>`. List the three most likely causes in order, and the read-only command that would confirm or rule out each. Do not change any code yet.

## 28.6 What you never delegate

- Running any command that changes the cloud (create, set, delete, attach, detach, reboot).
- Deciding gate G1, G2 or G3. The agent can summarize the evidence; you decide.
- The teardown order and the IP release rule. Review every diff that touches them line by line.
- Benchmark numbers in the report and the slides. You run the benchmarks and you read the raw data.
- Anything involving secrets: credentials, CA keys, passwords.
- The live demo. Rehearse it yourself, five times.

## 28.7 All learning checkpoints, collected

Answer these in `docs/notebook/learning.md`. Together they are your preparation for the jury.

**Understanding the design (chapters 2–8)**
1. Why is "seamless failover" defined with new connections only?
2. Which three FelCloud facts changed the original design, and how?
3. Why can each team not have its own security group, and what do we do instead?
4. Why did VM-per-team, k3s and Octavia L7 lose? One sentence each.
5. What exactly does a gateway acknowledge, and why does the ack include a digest?
6. Why must files on disk be written before the runtime API change?
7. What is a fencing epoch, and what does it prevent?

**Steps 0–5 (infrastructure and agents)**
8. Why does Neutron drop packets for the VIP unless it is an allowed address on the gateway port?
9. What does a gratuitous ARP do during failover, and what does OVN do with it?
10. Why does the backup take about 1.8 s to notice a crashed master with `advert_int 0.5`?
11. What happens to an open TCP connection during a hitless reload? During a failover?
12. Why is a gateway not allowed to take the VIP before its agent has synced?
13. Walk a packet from gw1 to a team container in the pod-NIC model. Where would team1 → team2 be dropped?
14. Why must the metadata service be blocked inside each team namespace?

**Steps 6–9 (control plane and security)**
15. Why `SKIP LOCKED` and not plain `FOR UPDATE`?
16. Why must a transaction never stay open during a Neutron call?
17. What happens if the control plane crashes between "port created" and "port ID saved"?
18. Draw the `edges.md` timeline and show which step of the teardown order prevents it.
19. Why are generations bumped on one row and not taken from a sequence?
20. Why does the reconciler delete an orphan only after two scans, and never more than max(5, 10 %)?
21. Which layer stops each of: team → team, team → metadata, team → gateway agent, fork bomb?

**Steps 10–14 (integration and delivery)**
22. Why open-loop load generation for failover tests?
23. Why is a p99 over 10 samples meaningless?
24. Why SSE and not WebSockets for the dashboard?
25. What do you say if the live failover demo fails on stage?

## 28.8 Glossary

| Term | Meaning in this project |
|---|---|
| **AAP (allowed-address-pairs)** | Neutron port attribute listing extra IP/MAC pairs the port may use. Port security drops other source addresses. Used for the VIP on both gateway ports, and for team IPs in the fallback model |
| **Ack (acknowledgement)** | Message from a gateway agent: "I applied and verified generation G, and my map digest is D" |
| **Agent** | Our Go program on a gateway (`gwagent`) or host (`hostagent`). It dials out to the control plane and applies desired state locally |
| **Anti-affinity** | Nova server-group policy: never place the members on the same hypervisor |
| **ARP** | Address Resolution Protocol: finds the MAC address for an IPv4 address on a local network |
| **Bounded context** | A part of the code that owns its own data and rules (IPAM, edge routing, tenancy...), exposed through an interface |
| **Canary route** | A fixed hostname (`canary.gw.internal`) routed to the agent itself, used to verify that routing works after a change |
| **Circuit breaker** | Reconciler safety: if a scan would delete too much, it deletes nothing and alerts |
| **Claim** | A worker's temporary ownership of a team row (30 s, extended every 10 s) |
| **Conntrack** | Linux connection-tracking table used by NAT and stateful firewalls |
| **Control plane (cp, `cpd`)** | The VM and Go binary holding the API, IPAM, controller, reconciler and database. Not on the data path |
| **Data path** | What visitor packets cross: floating IP → VIP → HAProxy → team IP |
| **Desired state** | What should exist (in the database). Agents and controllers make reality match it |
| **Digest** | SHA-256 of the sorted route map at a generation; proves what a gateway is serving |
| **Drift** | A difference between desired state and reality (orphan port, missing route, stale container) |
| **Fencing (gateway)** | Removing a dead gateway from the ack quorum safely: remove its VIP allowed address and disable its port |
| **Fencing token / epoch** | Number carried by every write of a claim owner; a stale owner's writes affect 0 rows |
| **FIP (floating IP)** | Public IPv4 address NATed by the router to a private port; ours points to the VIP |
| **FOREIGN** | IPAM state for an IP in our range held by a port we did not create |
| **GARP (gratuitous ARP)** | An unsolicited ARP announcing "this IP is at this MAC". Makes OVN rebind the VIP after failover |
| **Generation** | Monotonic number of the routing configuration; every route change creates a new one |
| **gophercloud** | The official Go SDK for OpenStack (`gophercloud/v2`) |
| **Hitless reload** | HAProxy reload where the new worker inherits listening sockets and old workers finish their requests, so no connection is refused |
| **Idempotency key** | Client-supplied header that makes a retried `POST` return the original result instead of creating a duplicate |
| **Idempotent** | Doing it twice has the same effect as doing it once |
| **IPAM** | IP Address Management: our pool table and state machine for team IPs |
| **Leader election** | One control-plane instance holds a Postgres advisory lock and runs the sweepers; safety does not depend on it |
| **LISTEN/NOTIFY** | PostgreSQL publish/subscribe; notifications are sent only when the transaction commits |
| **Map (HAProxy)** | A key/value file loaded by HAProxy; `hosts.map` maps a hostname to a slot backend |
| **Master-worker** | HAProxy mode where a master process manages workers and handles reloads |
| **mTLS** | Mutual TLS: both sides present certificates signed by our own CA |
| **MTU** | Maximum packet size on a link; 1442 bytes on FelCloud's Geneve overlay |
| **netns (network namespace)** | A Linux network stack of its own: interfaces, routes, firewall. One per team |
| **nftables / nft** | The Linux packet filter framework (successor of iptables) |
| **nopreempt** | keepalived option: a recovered gateway does not take the VIP back, avoiding a second outage |
| **OVN** | Open Virtual Network, the SDN backend of FelCloud's Neutron |
| **Pause container** | A tiny container that only holds a network namespace open for the team's other containers |
| **pod-NIC** | Our primary team-network model: a real Neutron port hot-plugged into the host and moved into the team's netns |
| **Port security** | Neutron feature that drops traffic whose source MAC/IP is not the port's own (or an allowed pair) |
| **Quarantine** | IPAM state after release; the IP waits for gateway acks, Neutron absence and a minimum time before reuse |
| **Reconciler** | Loop comparing desired state with reality and repairing drift |
| **Runtime API** | HAProxy's admin socket commands (`set server`, `add map`, `prepare/commit map`) that change memory without a reload |
| **Security group (SG)** | Neutron's per-port stateful firewall; rules can reference other groups (`remote_group`) |
| **SKIP LOCKED** | `SELECT … FOR UPDATE SKIP LOCKED`: each concurrent transaction takes a different free row instead of waiting |
| **Slot backend** | Pre-declared HAProxy backend (`be_slot001`…) with one server whose address we set at runtime |
| **Split brain** | Both gateways believe they are MASTER |
| **SSE (Server-Sent Events)** | One-way HTTP stream from server to browser; used for live dashboard updates |
| **Static stability** | The data plane keeps working with its last-good state when the control plane is down |
| **Step (controller)** | One idempotent unit of provisioning or teardown with `Done()` and `Apply()` |
| **Sweeper** | Periodic leader task (expiry, quarantine release) |
| **track_file / track_process / track_script** | keepalived checks that change a node's priority or put it in FAULT |
| **TTL (lease)** | How long a team's lease lives before automatic teardown |
| **URI SAN (SPIFFE-style)** | Identity inside a certificate, e.g. `spiffe://cstam/gateway/gw1`, checked by the server |
| **VIP (virtual IP)** | 10.20.0.10, held by whichever gateway is VRRP MASTER |
| **Virtual port (OVN)** | An OVN logical port with no fixed location, bound to whichever parent port sends a GARP for its IP |
| **VRRP** | Virtual Router Redundancy Protocol (IP protocol 112), spoken by keepalived to elect the MASTER |
| **Warm pool** | Pre-created and pre-attached team ports (IP state WARM) that make registration fast in the pod-NIC model |

::: learn
**Learning checkpoint**

1. Explain ten glossary terms of your choice to someone who does not know them, with an example from this project.
2. Which three rules in 28.6 protect you most at the pitch, and why?
:::
