# 1. How to use this guide

This guide shows how to build the CSTAM x FelCloud challenge solution, "The Resilient IP Optimizer", from an empty folder to a system you can demonstrate live on FelCloud's OpenStack. The backend is written in Go.

It is written for one reader: **you, building the system with an AI coding agent (Claude Code) while still understanding every line that gets written.** "Vibe coding" means accepting code you cannot explain. That does not work here: the jury will ask why the gateway fails over in 1.8 seconds and not 0.2, or what stops two teams from getting the same IP. You need answers you understand, not answers you remember.

## 1.1 What is in the guide

The guide has four parts.

| Part | Chapters | What you get |
|---|---|---|
| **I. Understand the design** | 1–8 | The problem and how it is scored, the facts we verified on FelCloud, the architectures we rejected, the final architecture, every architecture decision with its alternatives, the Go software architecture, and the build flow. |
| **II. Build it, step by step** | Steps 0–9 | The core system: setup, risk experiments on FelCloud, the OpenStack foundation, the gateway pair, the gateway agent, team networking and the host agent, the control plane with IPAM, provisioning, teardown and reconciliation, and security. |
| **III. Prove it and ship it** | Steps 10–14 | End-to-end integration, stress, chaos and benchmark report, the dashboard, bonus features, documentation, the video and the pitch. |
| **IV. Reference** | Appendices | Edge-case catalogue, failure matrix, jury questions with answers, the delivery timeline, cut lists, glossary. |

Read Part I once, completely, before you write any code. It is long because every later step depends on a decision made there. When a later step says "we use slot backends" or "the IP stays in QUARANTINE until both gateways ack", Part I has already explained why, and you will not have to trust the agent.

## 1.2 The loop you run for every step

Every build step in Part II and Part III follows the same loop. Do not skip parts of it, even when you are in a hurry. The loop is what turns agentic coding into learning instead of copy-paste.

| # | Phase | Who does it | What it produces |
|---|---|---|---|
| 1 | **Explain** | You read the "Understand first" section; you ask Claude to explain anything unclear | You can describe the mechanism in two sentences |
| 2 | **By hand first** | You run the `openstack`, `ip`, `nft`, `haproxy`, `docker` commands yourself | You have seen it work (or fail) with your own eyes; outputs go into `docs/notebook/experiments.md` |
| 3 | **Small diff** | Claude writes one focused change (one package or one behavior, under ~300 lines); you review it | Code you can explain line by line |
| 4 | **Prove it** | You run the "Done when" checks: tests, commands, measurements | Evidence: green tests, measured numbers |
| 5 | **Commit** | You commit with a message that says what, why and how it was verified; you answer the "Learning checkpoint" questions in `docs/notebook/learning.md` | A step you own |

::: why
**Why "by hand first"**

Infrastructure code automates commands. If you never ran the commands, you cannot tell whether the code is wrong or the cloud is behaving differently from what you expect. On FelCloud the cloud really does behave in specific ways (OVN virtual ports, an MTU of 1442, a tiny Neutron allocation pool). Running things by hand first is how you find this out cheaply, before it is buried in a Go reconcile loop.
:::

## 1.3 Rules for agentic coding on this project

These rules go into the repository's `CLAUDE.md` in Step 0, so the agent reads them every session. They exist to protect you, the cloud, and your understanding.

1. **Claude never creates, modifies or deletes OpenStack resources.** It may run read-only commands (`openstack ... list`, `show`). Anything that creates or deletes (ports, VMs, floating IPs, security groups) is proposed as a command; **you run it**. FelCloud is a real hosting company's cloud, shared with other people.
2. **Claude never reads or prints secrets**: `.secrets/`, `clouds.yaml`, `*.key`, `.env` files.
3. **Small steps.** One package or one behavior per change. If a diff is too big to review in ten minutes, ask for it to be split.
4. **Explain before code.** For anything touching more than one package, use plan mode: Claude explains the mechanism, the files, the interfaces, the tests and the risks; you approve the plan before any code is written.
5. **Every change has a test and a "done when" command.**
6. **No shell string concatenation.** External commands are executed with an argument list (`exec.Command("ip", "addr", "add", ...)`), never `sh -c "..."`. All names that reach configs (HAProxy maps, nftables, Docker) are validated against a strict regular expression first.
7. **Decisions and measurements are logged** in `docs/notebook/experiments.md`. This file later becomes most of your benchmark report.

::: warn
**The cloud is not yours alone**

The project already contains resources you did not create: the VMs `sbx-f0699704` and `adam`, a port on 10.20.0.200 (the OVN metadata port), the shared networks `INTERNET`, `Rancher-mgmt`, `lb-mgmt-net`, `private_network`, and the router `private_router`. **Never delete them.** Ask the FelCloud mentors before touching `sbx-f0699704` or `adam`. Everything we create carries the tag `cstam`, so it can always be listed and cleaned up separately.
:::

## 1.4 Conventions used everywhere

### Names and the IP plan

All examples use the same names and addresses. They are defined once here and in `internal/config/topology.go` (Go) and `infra/topology.env` (shell).

| IP (10.20.0.x) | What | Neutron port name |
|---|---|---|
| .1 | `cstam-router` interface (subnet gateway) | router port |
| .5 | Control plane VM `cstam-cp` | `cstam-cp-port` |
| .10 | **VRRP VIP** (port bound to no VM; the floating IP points here) | `cstam-vip` |
| .11 / .12 | Gateways `cstam-gw1` / `cstam-gw2` | `cstam-gw1-port` / `cstam-gw2-port` |
| .21 / .22 | Hackathon hosts `cstam-host-a` / `cstam-host-b` | `cstam-host-a-port` / `cstam-host-b-port` |
| .32 – .191 | **Team pool** owned by our IPAM (160 addresses; .68 is reserved because the existing `sbx-f0699704` VM uses it) | `cstam-team-<team-uuid>` |
| .192 – .199 | Lab and chaos-test pool (Step 1 experiments use these) | `cstam-lab-*` |
| .200 – .250 | Neutron's own allocation pool (already configured). Never used by us. | – |

Other conventions:

| Item | Value |
|---|---|
| Network / subnet | `cstam-sandbox-net` / `cstam-sandbox-subnet`, 10.20.0.0/24, **MTU 1442** |
| External network | `INTERNET` (197.5.133.0/24) |
| OpenStack CLI | run `export OS_CLOUD=cstam` once per shell; the cloud entry lives in `~/.config/openstack/clouds.yaml` |
| Network interface in VMs | usually `ens3`; always detect it with `ip -o -4 route show default \| awk '{print $5}'` |
| Domain | `BASE_DOMAIN` (default `cstam.felcloud.tn`). A team is `<team>.<hackathon>.<BASE_DOMAIN>`, an extra app is `<app>-<team>.<hackathon>.<BASE_DOMAIN>` |
| Domain without FelCloud DNS | `sslip.io`: `team1-hacka.197-5-133-NN.sslip.io` resolves to the floating IP with no setup |
| Tags on everything we create in Neutron | `cstam`, `cstam-env:<dev\|prod>`, plus `cstam-team:<uuid>`, `cstam-lease:<uuid>` on team ports |
| Go module | `github.com/<you>/cstam`, Go 1.25 |
| Binaries | `cpd` (control plane), `gwagent` (gateway agent), `hostagent` (host agent), `cstamctl` (CLI) |

### Placeholders

`<you>` is your GitHub user, `<FIP>` is the floating IP attached to the VIP, `<CP_FIP>` is the control plane's floating IP for SSH and the dashboard, and `ADMIN_CIDR` is your laptop's public IP followed by `/32`.

## 1.5 How the boxes work

Colored boxes mark the parts you must not miss.

::: why
**Why**: the reasoning behind a decision. If you only have time to read one kind of box before the jury, read these.
:::

::: warn
**Pitfall**: something that has broken for other people, or that is specific to FelCloud.
:::

::: note
**Note / Goal**: tips, and at the top of each step its goal, the points it serves, its time estimate and its dependencies.
:::

::: done
**Done when**: exact commands and their expected output. A step is finished only when every check in this box passes.
:::

::: learn
**Learning checkpoint**: questions you answer yourself, without the AI, in `docs/notebook/learning.md`. These answers are your preparation for the jury's questions.
:::

::: claude
**Driving Claude Code**: the prompt to paste for this step and what to look at in the diff.
:::

::: gate
**Gate**: a go / no-go decision. Each gate names the fallback to take if the result is "no-go".
:::

## 1.6 Reading order and time

1. Read chapters 1–8 in order (about three hours). Do not code yet.
2. Do Step 0 on day 1, including **emailing the mentors** (Step 0 lists the questions). Their answers take days.
3. Do the Step 1 experiments before anything else on the cloud. They decide two architecture choices at gate G1.
4. From then on, follow chapter 8's build flow. The control-plane track can start on day 3 against fakes, before the cloud experiments finish.

Estimates in this guide (latencies, failover times, hours of work) are marked as estimates. Replace each one with your own measurement as soon as you have it. The jury rewards measured numbers and penalizes adjectives.

::: claude
**Your first prompt in this project**

"Read docs/guide (the PDF text) chapter 1 and CLAUDE.md. Summarize the rules you must follow in this repository, in particular what you may and may not run against OpenStack. Do not write any code yet."

Then check that the summary contains rule 1 (no cloud writes) and rule 2 (no secrets). If it does not, fix `CLAUDE.md` before continuing.
:::
