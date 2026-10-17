# 9. Step 0 – Project setup

## Step 0 – Repository, tools and working rules

::: note
**Goal:** a Git repository with the final folder layout, a Go module that builds and tests in CI, a `CLAUDE.md` that tells Claude Code how to work with you, a lab notebook, working SSH and OpenStack access from your laptop, and an email sent to the FelCloud mentors.
**Points served:** none directly. It protects every later step from lost work, leaked secrets and confusion.
**Time estimate:** 2–3 hours.
**Depends on:** nothing.
:::

### Understand first

Before you write any Go, three things must be true:

1. **Nothing secret can reach Git.** Your folder already contains `.secrets/` (an access JSON and console notes). One wrong `git add .` would publish them. The `.gitignore` and a secret scanner (gitleaks) make that mistake impossible, not just unlikely.
2. **The layout is decided once.** Claude Code is much better when the repository has a clear shape. If `internal/ipam` exists, it puts IPAM code there. If nothing exists, every session invents a new structure.
3. **The rules for the agent are written down.** `CLAUDE.md` is read by Claude Code at the start of every session. It is where you say "never create OpenStack resources", "explain before coding" and "small diffs only". You write it once, and you do not have to repeat it.

The other part of Step 0 is **lead time**. Some answers only FelCloud can give: a DNS record for `*.cstam.felcloud.tn`, a quota increase, whether you may delete `adam` and `sbx-f0699704`. Those answers can take days. Send the email today, so the answers arrive while you run the experiments in Step 1.

### Do it by hand

#### 0.1 Tools on your laptop

| Tool | Version | Why | Install check |
|---|---|---|---|
| Go | 1.25.x | all backend code | `go version` → `go1.25.x` |
| Docker | 27+ | testcontainers (Postgres, HAProxy) in tests | `docker run --rm hello-world` |
| buf | 1.x | generates Connect-RPC code from `.proto` | `buf --version` |
| sqlc | 1.31+ | generates typed Go from SQL | `sqlc version` |
| goose | 3.29+ | database migrations | `goose --version` |
| oapi-codegen | 2.x | generates the REST server interface from OpenAPI | `oapi-codegen --version` |
| golangci-lint | 2.x | linting | `golangci-lint version` |
| Node.js | 22 LTS | dashboard build (Vite) | `node --version` |
| openstack CLI | latest | everything on FelCloud | `openstack --version` |
| Pulumi CLI | 3.x | infrastructure as code (Step 2) | `pulumi version` (install: `curl -fsSL https://get.pulumi.com \| sh`) |
| python-octaviaclient | latest | `openstack loadbalancer ...` (fallback F2, experiment E7) | `openstack loadbalancer provider list` |
| socat, jq, tcpdump | distro | talk to HAProxy sockets, read JSON, capture VRRP | `which socat jq tcpdump` |

Go tools are installed into `$(go env GOPATH)/bin`:

```bash
go install github.com/bufbuild/buf/cmd/buf@latest
go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
go install github.com/pressly/goose/v3/cmd/goose@latest
go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install connectrpc.com/connect/cmd/protoc-gen-connect-go@latest
# golangci-lint: use the official install script (pins a version)
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b "$(go env GOPATH)/bin" v2.5.0
pipx install python-openstackclient && pipx inject python-openstackclient python-octaviaclient
```

Check OpenStack access (read-only). Your application credential is already configured as cloud `cstam` in `~/.config/openstack/clouds.yaml`:

```bash
export OS_CLOUD=cstam
openstack token issue -f value -c expires        # prints a future timestamp
openstack network show cstam-sandbox-net -f value -c mtu   # 1442
openstack subnet show cstam-sandbox-subnet -f value -c allocation_pools   # 10.20.0.200-10.20.0.250
```

::: warn
**`clouds.yaml` is a secret.** It must stay in `~/.config/openstack/` with mode `600` and must never be copied into the repository. Run `chmod 600 ~/.config/openstack/clouds.yaml` now.
:::

#### 0.2 Create the repository

```bash
cd ~/cstam
git init -b main
cat > .gitignore <<'EOF'
# secrets: never commit
.secrets/
*.key
*.pem
*.p12
clouds.yaml
.env
*.env
!deploy/env/*.env.example
# build output
/bin/
/dist/
web/node_modules/
web/dist/
coverage.out
# local state
/tmp/
bench/results/
EOF
git add .gitignore && git commit -m "chore: gitignore secrets and build output"
git status --ignored --short | grep secrets     # must print: !! .secrets/
```

`!!` means "ignored". If you do not see it, stop and fix `.gitignore` before anything else.

Install the secret scanner as a pre-commit hook:

```bash
# gitleaks: https://github.com/gitleaks/gitleaks/releases (single binary)
cat > .git/hooks/pre-commit <<'EOF'
#!/bin/sh
exec gitleaks protect --staged --redact -v
EOF
chmod +x .git/hooks/pre-commit
gitleaks detect --redact -v      # scans history: "no leaks found"
```

#### 0.3 Go module and folder layout

```bash
go mod init github.com/<your-github-user>/cstam
go mod edit -go=1.25
```

Create the layout. Empty folders get a `doc.go` (Go packages) or a `.gitkeep` so Git keeps them.

```text
cstam/
├── go.mod / go.sum
├── Makefile
├── CLAUDE.md
├── buf.yaml  buf.gen.yaml        # proto lint + Connect codegen
├── sqlc.yaml
├── .golangci.yml
├── api/
│   ├── openapi/cstam.v1.yaml     # public REST contract (source of truth)
│   └── proto/cstam/agent/v1/     # control.proto: services hosted by cpd, called by agents
├── gen/                          # generated code (committed)
├── cmd/
│   ├── cpd/                      # control-plane daemon (roles: api, controller)
│   ├── gwagent/                  # gateway agent
│   ├── hostagent/                # host agent
│   └── cstamctl/                 # CLI: pki, doctor, bench, sectest, gc --dry-run
├── internal/
│   ├── model/                    # domain types, no dependencies
│   ├── config/                   # env structs per binary + topology.go (IP plan)
│   ├── store/                    # Store interface, sqlc output, queries/*.sql
│   ├── ipam/                     # allocator + IP state machine
│   ├── lifecycle/                # team controller, steps, claims
│   ├── openstack/                # NeutronClient interface, gophercloud impl, fake
│   ├── edge/                     # generations, route snapshots, config versions
│   ├── agentapi/                 # Connect services inside cpd (agents call these)
│   ├── api/                      # REST handlers, middleware, SSE
│   ├── auth/  audit/  pki/  events/
│   ├── gw/                       # gateway agent internals: haproxy/, applier/, keepalived/, logs/
│   ├── host/                     # host agent internals: netdriver/podnic, netdriver/aap, docker/, firewall/
│   ├── platform/                 # tls, retry, httpserver, metrics, systemd notify
│   └── testutil/                 # testcontainers helpers, fakes
├── migrations/                   # goose SQL files
├── web/                          # React + Vite dashboard (embedded into cpd)
├── deploy/
│   ├── systemd/  cloud-init/  haproxy/  keepalived/  env/
├── infra/                        # pulumi/ (IaC in Go, separate go.mod), topology.env, lab/
├── bench/                        # failover-probe, loadgen, provbench, chaos
└── docs/                         # design, guide, experiments.md, learning.md, reports
```

```bash
mkdir -p api/openapi api/proto/cstam/agent/v1 gen \
  cmd/{cpd,gwagent,hostagent,cstamctl} \
  internal/{model,config,store/queries,ipam,lifecycle,openstack/fake,edge,agentapi,api,auth,audit,pki,events,platform,testutil} \
  internal/gw/{haproxy,applier,keepalived,logs} internal/host/{netdriver/podnic,netdriver/aap,docker,firewall} \
  migrations web deploy/{systemd,cloud-init,haproxy,keepalived,env} infra/lab bench docs
for d in cmd/*; do
  printf 'package main\n\nfunc main() {}\n' > "$d/main.go"
done
find internal -type d -empty -exec sh -c 'printf "package %s\n" "$(basename "$1")" > "$1/doc.go"' _ {} \;
go build ./... && go vet ./...
```

Put the IP plan in **one** Go file and **one** shell file, so firewall rules, security groups, tests and the dashboard all read the same numbers:

```go
// internal/config/topology.go
package config

import "net/netip"

var (
	Subnet      = netip.MustParsePrefix("10.20.0.0/24")
	Router      = netip.MustParseAddr("10.20.0.1")
	ControlIP   = netip.MustParseAddr("10.20.0.5")
	VIP         = netip.MustParseAddr("10.20.0.10")
	Gateways    = []netip.Addr{netip.MustParseAddr("10.20.0.11"), netip.MustParseAddr("10.20.0.12")}
	Hosts       = []netip.Addr{netip.MustParseAddr("10.20.0.21"), netip.MustParseAddr("10.20.0.22")}
	TeamPoolLo  = netip.MustParseAddr("10.20.0.32")
	TeamPoolHi  = netip.MustParseAddr("10.20.0.191")
	LabPoolLo   = netip.MustParseAddr("10.20.0.192")
	LabPoolHi   = netip.MustParseAddr("10.20.0.199")
	ReservedIPs = []netip.Addr{netip.MustParseAddr("10.20.0.68")} // sbx-f0699704, not ours
	MTU         = 1442
)
```

```bash
# infra/topology.env  (sourced by every infra script)
export OS_CLOUD=cstam
NET=cstam-sandbox-net
SUB=cstam-sandbox-subnet
EXT_NET=INTERNET
CP_IP=10.20.0.5
VIP_IP=10.20.0.10
GW1_IP=10.20.0.11
GW2_IP=10.20.0.12
HOSTA_IP=10.20.0.21
HOSTB_IP=10.20.0.22
IMAGE="Ubuntu 24.04 LTS - Noble Numbat"
GW_FLAVOR=G0.basic.1c2g
CP_FLAVOR=G0.basic.2c4g
HOST_FLAVOR=G0.basic.4c8g
KEY=cstam-key
```

#### 0.4 Makefile

```make
# Makefile
GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty)
LDFLAGS := -s -w -X main.version=$(VERSION)
BINS    := cpd gwagent hostagent cstamctl

.PHONY: gen lint test test-int build web clean

gen:                         ## regenerate all generated code
	buf generate
	sqlc generate
	oapi-codegen -config api/openapi/oapi-codegen.yaml api/openapi/cstam.v1.yaml

lint:
	golangci-lint run ./...
	buf lint

test:                        ## fast tests (no Docker)
	$(GO) test -race -count=1 ./...

test-int:                    ## integration tests (needs Docker: Postgres, HAProxy)
	$(GO) test -race -count=1 -tags integration ./...

build: web
	@mkdir -p bin
	@for b in $(BINS); do CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/$$b ./cmd/$$b; done

web:
	cd web && npm ci && npm run build

clean:
	rm -rf bin web/dist
```

Later steps add `bench-all`, `sectest`, `deploy-gw`, `deploy-host`, `deploy-cp`, `lab-clean`.

#### 0.5 Linter configuration

```yaml
# .golangci.yml
version: "2"
linters:
  enable:
    - govet
    - staticcheck
    - errcheck
    - gosec
    - revive
    - errorlint
    - contextcheck
    - bodyclose
    - sqlclosecheck
    - depguard
  settings:
    depguard:
      rules:
        agents-no-db:
          files: ["**/internal/gw/**", "**/internal/host/**", "**/cmd/gwagent/**", "**/cmd/hostagent/**"]
          deny:
            - pkg: github.com/jackc/pgx/v5
              desc: agents never talk to the database
            - pkg: github.com/gophercloud/gophercloud/v2
              desc: agents have no OpenStack credentials (only cpd does)
```

The `depguard` rule is a security rule written as a linter: a gateway or host agent that imports the OpenStack SDK fails CI. The only exception is fallback F1 (Step 3), which gives the gateway agent a credential limited to one floating IP. If you need F1, you add an explicit allowlist for `internal/gw/fipmove` and nothing else.

#### 0.6 Continuous integration

```yaml
# .github/workflows/ci.yml
name: ci
on: [push, pull_request]
jobs:
  test:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - uses: actions/setup-go@v5
        with: { go-version: "1.25.x" }
      - run: go vet ./...
      - uses: golangci/golangci-lint-action@v8
        with: { version: v2.5.0 }
      - run: go test -race -count=1 ./...
      - run: go test -race -count=1 -tags integration ./...   # Docker is available on GitHub runners
  secrets:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - uses: gitleaks/gitleaks-action@v2
```

GitHub runners cannot reach FelCloud's private network. Tests that need the real cloud use the build tags `e2e` and `hostint` and run from the control-plane VM later (Steps 10 and 11).

#### 0.7 CLAUDE.md

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

#### 0.8 Lab notebook template

```markdown
# docs/notebook/experiments.md

## Template
### E<n> – <title>  (date, who)
- Question:
- Commands (exact):
- Output (trimmed):
- Measurement (median / max, n runs):
- Conclusion / decision:
- Cleanup done: yes/no (resources deleted: ...)

## Decisions log
| Date | Decision | Based on | Alternatives rejected |
|---|---|---|---|
```

Also create `docs/notebook/learning.md`. You will write your answers to every "Learning checkpoint" box there. It becomes your preparation for the jury questions.

#### 0.9 SSH key and SSH config

Only the control-plane VM will get its own floating IP for SSH. Gateways and hosts are reached through it as a jump host. During Step 1 (before the control plane exists) the lab VMs get temporary floating IPs.

```bash
ssh-keygen -t ed25519 -f ~/.ssh/cstam_ed25519 -C cstam -N ''
cat >> ~/.ssh/config <<'EOF'
Host cstam-cp
    HostName <CP_FLOATING_IP>
    User ubuntu
    IdentityFile ~/.ssh/cstam_ed25519
Host cstam-gw1
    HostName 10.20.0.11
Host cstam-gw2
    HostName 10.20.0.12
Host cstam-host-a
    HostName 10.20.0.21
Host cstam-host-b
    HostName 10.20.0.22
Host cstam-gw1 cstam-gw2 cstam-host-a cstam-host-b
    User ubuntu
    IdentityFile ~/.ssh/cstam_ed25519
    ProxyJump cstam-cp
EOF
```

You will fill in `<CP_FLOATING_IP>` in Step 2.

#### 0.10 Quota budget

| Resource | Quota | Existing (sbx, adam) | Lab (Step 1, deleted after) | Final platform | Total final |
|---|---|---|---|---|---|
| Instances | 10 | 2 | 2 | 5 (2 gw, 1 cp, 2 hosts) | 7 |
| vCPU | 20 | 5 | 2 | 12 (1+1+2+4+4) | 17 |
| RAM | 50 GB | 5 GB | 4 GB | 24 GB | 29 GB |
| Security groups | 10 | 2 | 1 | 5 | 7 (8 with lab) |
| SG rules | 100 | ~6 | ~8 | ~36 | ~42 |
| Floating IPs | 50 | 0 | 3 | 2 (VIP, cp) | 2 |
| Ports | 500 | 4 | ~25 | 5 + 1 per team | small |
| Volumes | 10 | 2 | 0–2 | 0–5 (only if boot-from-volume is required) | ≤ 7 |

If FelCloud images must boot from a volume (the existing VMs did), each VM also uses one of the 10 volumes. That still fits.

Later, `cstamctl doctor` will check this budget automatically before any provisioning (read-only API calls): quota usage, that the router exists, that the subnet allocation pool is still `.200–.250`, that no foreign port sits in the team pool, and that the MTU is 1442. You write it in Step 6; for now the table is enough.

#### 0.11 Email to the FelCloud mentors (send today)

```text
Subject: CSTAM-FELCLOUD – four questions about the OpenStack project (team <name>)

Hello Mr. Benyaiche, Mr. Zied,

We are building the Resilient IP Optimizer on the FelCloud OpenStack project "cstam".
Four answers would help us a lot, and some have lead time:

1. DNS: could you delegate the zone cstam.felcloud.tn to us (NS records pointing to our
   gateways), or at least create a wildcard A record *.cstam.felcloud.tn pointing to one
   floating IP we will give you? We also want to issue TLS certificates with Let's Encrypt
   DNS-01 without using felcloud.tn's rate limit: a single CNAME for
   _acme-challenge.<hackathon>.cstam.felcloud.tn would be enough.
2. Quota: our design fits in 10 instances / 20 vCPU, but a small increase (for example
   14 instances / 28 vCPU) would let us show an optional one-VM-per-team mode.
3. Existing resources: the VMs "adam" (SHUTOFF) and "sbx-f0699704" (fixed IP 10.20.0.68)
   are in our project. Are they yours? May we delete "adam" to free quota? We will not
   touch them without your answer.
4. Octavia: is "openstack loadbalancer failover" allowed for our role, and is the OVN provider
   supported with TCP health monitors? We want it as a documented fallback for our VRRP gateways.

Also, is the Neutron setting max_allowed_address_pair still the default (10)?

Thank you,
<name>
```

### Build it in Go

Nothing to build yet beyond the empty `main.go` files. One small, useful piece: a `version` flag in every binary, so you always know what is deployed.

```go
// cmd/gwagent/main.go (same pattern in every cmd/)
package main

import (
	"flag"
	"fmt"
	"os"
)

var version = "dev" // set by -ldflags "-X main.version=..."

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}
}
```

### Done when

::: done
**Done when**

- `git status --ignored --short | grep secrets` prints `!! .secrets/`.
- `gitleaks detect --redact -v` prints `no leaks found`.
- `make build && ./bin/gwagent -version` prints a version string.
- `make lint test` passes locally, and the CI workflow is green on GitHub.
- `openstack network show cstam-sandbox-net -f value -c mtu` prints `1442`.
- `CLAUDE.md`, `docs/notebook/experiments.md`, `docs/notebook/learning.md`, `internal/config/topology.go` and `infra/topology.env` exist and are committed.
- The email to the mentors is sent (note the date in `docs/notebook/experiments.md`).
:::

### Pitfalls

::: warn
**`git add .` before `.gitignore`.** If you ever committed `.secrets/` by mistake, deleting the file in a new commit is not enough: it is still in history. Rewrite history (`git filter-repo --path .secrets --invert-paths`) **before** the first push, and rotate the credential anyway.
:::

::: warn
**Tool versions drift.** `@latest` is fine on day 1, but record the exact versions in `docs/notebook/experiments.md` (`go version`, `sqlc version`, ...). A generator upgrade in week 4 can change generated code and create confusing diffs.
:::

### Learning checkpoint

::: learn
**Answer these yourself in docs/notebook/learning.md**

1. Why is a `.gitignore` alone not enough to protect secrets, and what does the pre-commit hook add?
2. Why do the agents get a lint rule that forbids importing gophercloud and pgx? What attack does that reduce?
3. Why put the IP plan in one Go file and one shell file instead of writing IPs where they are used?
4. Which FelCloud answers block which steps of the plan? (Hint: DNS blocks TLS and the final demo URLs; quota blocks the VM driver.)
:::

### Driving Claude Code for this step

::: claude
**Prompts to use**

- "Read CLAUDE.md and docs/guide Step 0. Create the folder layout, the Makefile and .golangci.yml exactly as described. Do not add dependencies. Show me the tree when done."
- "Write internal/config/topology.go and a unit test that checks: the team pool does not overlap the Neutron pool 10.20.0.200–250, the VIP and gateways are outside the team pool, and 10.20.0.68 is in ReservedIPs."

**Review in the diff:** no file under `.secrets/` touched; no hardcoded IP outside `topology.go`/`topology.env`; `go.mod` has no surprise dependencies.
:::
