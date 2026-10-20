# Security architecture

What the platform protects, from whom, how, and how each control is verified. Every control
names the code or file that implements it and the test or measurement that checks it. Known
gaps are listed at the end, not hidden.

## 1. Assets and threats

| Asset | Why it matters |
|---|---|
| Team sandboxes and their traffic | a team must not read, change or reach another team's app |
| The gateways (HAProxy, keepalived, `gwagent`) | the single public entry; a takeover would redirect every team |
| The control plane (`cpd`, PostgreSQL) | decides which IP and which route each team gets |
| The OpenStack application credential | can create and delete VMs and ports in a project shared with others |
| Team IPs | a reused IP that still receives traffic for its old team leaks data to the new one |

| Actor | Can | Must not be able to |
|---|---|---|
| Internet visitor | send HTTP to the floating IP | reach anything but published team apps; see management endpoints; exhaust a gateway |
| Team (participant) | run any code in its own sandbox | reach other teams, the gateways' management, the control plane, the OpenStack API with our rights; spoof another IP |
| Compromised gateway | serve wrong routes | read the database, call OpenStack, free IPs early |
| Other tenants of the shared OpenStack project | see and change any project resource | (outside our control) we only ever modify resources named `cstam-*` |

## 2. Trust boundaries

```
 Internet ──80/443──► floating IP ─► VIP 10.20.0.10 ─► MASTER gateway (cstam-gw)
                                                          │ HTTP to team IP:80 / 8000-8099
                                                          ▼
                                     team ports (cstam-team) ── no route to each other
 gateways ──8443 (bearer token)──► cpd on cstam-cp ──► PostgreSQL (local)
                                        └── HTTPS (application credential) ──► OpenStack API
 admin ──22/443 from AdminCIDR only──► cstam-cp
```

Only `cpd` holds the database DSN and the OpenStack credential. The agents hold a gateway token
that can read routes and post acks, nothing else.

## 3. Network isolation (Neutron security groups)

Five role groups, 14 ingress rules, all in one table in `infra/pulumi/secgroups.go`
(quota: 10 groups, 100 rules). Egress keeps the OpenStack default (allow).

| Group (on) | Rule | From |
|---|---|---|
| `cstam-gw` (gw1, gw2, VIP port) | TCP 80, 443 | `0.0.0.0/0` (public entry) |
| | TCP 22, TCP 8404 (stats/metrics), ICMP | `cstam-cp` only |
| `cstam-vrrp` (gw1, gw2) | IP protocol 112 (VRRP) | `cstam-vrrp` only: nobody else can send adverts |
| `cstam-cp` (cp) | TCP 22, 443 | `AdminCIDR` only |
| | TCP 8443 (`cpd`, `config.CPDPort`) | `cstam-gw`, `cstam-host` |
| `cstam-host` (hackathon hosts, Phase 2) | TCP 22, ICMP | `cstam-cp` only |
| `cstam-team` (every team port) | TCP 80 and 8000-8099 | `cstam-gw` only |
| `cstam-team` **egress** | any protocol to 0.0.0.0/0 **except 10.20.0.0/24** (24 prefixes, `infra/pulumi/egress.go`); no default rules | teams reach the internet and metadata, never cp, the gateways or each other |

What is deliberately **absent**:
- No "self" rule on `cstam-team`: Neutron drops team-to-team traffic, even inside the subnet.
- No rule opens a port on an agent: agents dial out to `cpd`; nothing can connect to them.
- `cstam-host` is never a remote group, so a team address added to a host can never inherit
  host rights.
- Team ports cannot reach `cpd` (8443 accepts only the gateway and host groups) or the gateways'
  SSH, stats or VRRP.

**Anti-spoofing.** Port security is on for every port: a team VM can only send from its own IP and
MAC. Only the gateway ports carry the VIP in `allowed_address_pairs`, so no team can answer ARP for
10.20.0.10 or take the VIP.

**App ports.** `cpd` refuses a team whose `app_port` is not 80 or 8000-8099 (`config.TeamPorts`,
`lifecycle.Spec.Validate`), so a route can never point at a port the firewall blocks.

## 4. The public frontend (HAProxy, `internal/gw/haproxy/haproxy.cfg.tmpl`)

| Control | How |
|---|---|
| Management endpoints hidden | `/__gw/*` answers only to `InternalSources` (localhost, cp, both gateways); from anywhere else it is a plain 404 that names nothing |
| Stats and Prometheus metrics | separate frontend on 8404, reachable from `cstam-cp` only (security group) |
| Unknown hosts | 404 page; there is no default backend that could leak a team |
| Per-team rate limit | stick table keyed on the normalized `Host` (lower case, port removed): 2000 requests per 10 s per Host by default (`-rate-limit`, per-Host overrides in `ratelimit.map`); above it `429`. A flooding client stays limited while it floods (refused requests count) |
| Slow clients | `timeout http-request 10s`, `client 30s`, `http-keep-alive 5s`, `queue 5s` |
| Team down | maintenance page with `Retry-After`, never another team's backend |
| Routing integrity | one backend per slot, one server per backend; the `hosts.map` swap is atomic; `gwagent` never applies an older generation (`TestNeverAppliesOlderGeneration`) and refuses invalid routes (`TestBadRouteIsNotApplied`) |
| Config changes | `haproxy -c` before use, verification through the real frontend after reload, rollback on failure (benchmark B1) |
| Config pushed through the API (`POST /v1/config`) | admin token only; `extra` lines are a deliberate admin escape hatch placed inside the HTTP frontend: at most 20 lines of 1-200 printable ASCII characters, so no newline can open a new section (`TestConfigInValidate`); the agent validates the settings again and each gateway runs the same check, reload, verify, rollback (notebook L11). An admin can still write a rule that misroutes traffic: that is the admin's power, recorded in the event log (`config.created`, `config.rollback`) |
| Traffic log (`/v1/traffic`) | lines carry the client IP, Host, method and path **without the query string** (no tokens in URLs leak); read with the admin token only, kept in memory (2000 lines), never written to disk; every text field is cut to a fixed size on the agent and again in cpd, a batch is at most 150 lines and 512 KiB (`TestClip`, `TestTrafficEndpoints`); the gateway name comes from the URL, not from the line |
| Planned failover (`POST /v1/gateways/{gw}/failover`) | admin token only; refused (409) unless another gateway is alive, serving the current generation, out of FAULT and not forced down (`TestFailover`), so one click cannot take the public entry down |

**Measured (benchmark B8):** one team flooded at ~79,000 requests/s for 20 s: 99.9 % answered
429, and the other 19 teams, at 2,850 requests/s together, had **0 errors** and a p99 latency of
3.7 ms.

## 5. Control-plane API (`internal/api`)

| Control | How |
|---|---|
| Authentication | `Authorization: Bearer`; two tokens (admin, gateway), at least 16 characters, must differ (`Server.Validate`, `TestValidateTokens`); compared in constant time (`crypto/subtle`) |
| Authorization | each route declares its token (`routes()`); a gateway token cannot call admin routes and vice versa (`TestRoutes`); the OpenAPI spec is checked against the routes (`TestOpenAPIListsEveryRoute`) |
| Read-only role | optional viewer token: admin `GET` routes only, 403 on every write, 401 on gateway routes (`TestViewerToken`); for the jury's laptops during the pitch |
| Dashboard | served by cpd with a strict CSP (`script-src 'self'`, no framing), data inserted as text only, checked by `go test ./web/` (no `innerHTML`/`eval`, no inline script); see dashboard.md |
| Request limits | body at most 64 KiB, `application/json` only (415), unknown JSON fields rejected |
| Server timeouts | read header 5 s, read 15 s, write 40 s (long-poll 25 s + margin), idle 90 s |
| Errors | client errors say what is wrong; internal errors return `internal error` and are logged only on the server |
| Integrity of acks | an ack for a generation newer than the database is refused (409), so a buggy or hostile gateway cannot make the sweep free IPs early (`TestGatewayAckGatesRecycling`) |
| Secrets | tokens and DSN only from the environment (`/etc/cstam/*.env`, mode 0600), never from flags or files in the repo |

## 6. Credentials and secrets

- **OpenStack**: a Keystone **application credential** (`auth_type: v3applicationcredential`), not
  a user password. Only `cpd` uses it (`OS_CLIENT_CONFIG_FILE=/etc/cstam/clouds.yaml`, 0600,
  owner `cstam`). The linter forbids gophercloud and pgx in agent code (`depguard` rule
  `agents-no-db-no-cloud`), so a gateway compromise exposes neither the cloud nor the database.
- **Repository**: `clouds.yaml`, `*.env`, keys and certificates are git-ignored; `gitleaks` scans
  the whole history in CI (`.github/workflows/ci.yml`), and in a local pre-commit hook on the
  machines where it is installed (hooks are not versioned).
- **Team VMs**: their cloud-init user-data contains no secret (it is readable from the metadata
  service by the VM itself): only the team name, hackathon, IP and port (`internal/openstack/userdata.go`).

## 7. Input validation and injection

Every value that reaches HAProxy, Docker, cloud-init or OpenStack is validated first:
- names (hackathon, team, gateway): `^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`, which also keeps the
  resource name `cstam-<hackathon>-<team>` a valid DNS label;
- container image: strict reference pattern, at most 255 characters;
- team IP: must be inside the team pool (or the lab pool), never Neutron's own range;
- app port: the firewall ranges above.

`lifecycle.Spec.Validate` runs before both sandbox drivers and before the user-data template, so
no value can break the YAML or a command (`TestSpecValidate`, `TestUserData`,
`TestCreateRejectsInvalidSpec`). External commands run as argv lists, never through a shell
(`gosec` in the linter).

## 8. Resource ownership and blast radius

- The OpenStack project is shared: the drivers only create resources named `cstam-*` and refuse to
  delete anything else (`Delete` checks the prefix; `TestDelete`). Team ports also carry the tags
  `cstam`, `cstam-team`.
- Pulumi manages only the platform it created; the VIP port and the public floating IP are
  `protect`ed. The OVN metadata port 10.20.0.200 is never touched.
- OpenStack calls are bounded (8 in flight) and retried only when idempotent (GET, DELETE, tag
  PUT); a lost port-create answer is detected by name, so a retry cannot create a second port.

## 9. IP reuse safety (reclamation)

An IP is freed only when (1) its route was removed in the same transaction that quarantined it,
(2) every gateway seen in the last 5 minutes acked that generation, (3) 60 s passed and (4) Neutron
shows no port holding it. So no gateway can still send a former team's traffic to the new owner
of the address (`TestQuarantineSweep`, `TestGatewayAckGatesRecycling`, `TestIPTakenStaysQuarantined`;
benchmark B7).

## 10. Host hardening (`deploy/systemd`)

| Unit | Runs as | Hardening |
|---|---|---|
| `gwagent.service` | `cstam`, group `haproxy` | `NoNewPrivileges`, `ProtectSystem=strict` (writes only `/etc/haproxy`, `/var/lib/cstam`, `/run/cstam`, `/run/haproxy`), `ProtectHome`, `PrivateTmp`, empty `CapabilityBoundingSet`, `RestrictAddressFamilies`, native syscalls only |
| `cpd.service` | `cstam` | same, with `/etc/cstam` read-only |
| HAProxy | master root, workers `haproxy` | admin and master sockets `0660` group `haproxy` |
| `cstam-gw-bootstrap.service` | root, oneshot | only writes the HAProxy and keepalived files before they start |

keepalived health and notify scripts run as `keepalived_script`, not root. Team containers in the
local demo run with memory, CPU and pid limits, `no-new-privileges` and all capabilities dropped.

## 11. Verification summary

| Claim | Evidence |
|---|---|
| Management endpoints invisible from outside | `TestGatewayGuards` (real HAProxy, outside source address) |
| Flood of one team limited, others unaffected | `TestGatewayGuards`; benchmark B8 |
| Tokens separated and checked | `TestRoutes`, `TestValidateTokens` |
| No IP reused while a gateway may still route it | `TestGatewayAckGatesRecycling`, `TestQuarantineSweep` |
| No invalid name, image or port reaches the cloud | `TestSpecValidate`, `TestValidation`, `TestCreateRejectsInvalidSpec` |
| Only `cstam-*` resources deleted | `TestDelete`, `Driver.Delete` prefix check |
| Firewall table | `infra/pulumi/secgroups.go`, `egress.go`; **measured on FelCloud** with lab VMs (phase2.md, notebook L14): team to team, team to management, team to cp all dropped; gateway to team allowed |

## 12. Known gaps and next steps

| Gap | Risk | Plan |
|---|---|---|
| Agents talk to `cpd` in plain HTTP inside the private network | someone already on `cstam-sandbox-net` could read the gateway token | the security groups only let the gateway and host groups reach 8443; mTLS (`internal/pki`) is the Phase 3 step |
| One gateway token shared by gw1 and gw2 | a compromised gateway could post acks as the other one | per-gateway tokens or mTLS client certificates |
| Rate-limit counters are per gateway (no `peers` section) | after a failover a flooding client starts from zero on the new MASTER | HAProxy `peers` between gw1 and gw2 |
| ~~Team egress is open~~ fixed 2026-10-20 | was: a sandbox could reach cp SSH (L14) | `cstam-team` egress only outside 10.20.0.0/24; the internet and metadata stay reachable |
| No TLS on the public entry yet | team sites are HTTP only | wildcard certificate (bonus, needs DNS from FelCloud) |
| The local Docker demo has no isolation between team containers | demo only | isolation is enforced by Neutron on FelCloud |
| gw1 and gw2 run on the same hypervisor (soft anti-affinity) | a hypervisor failure takes both | asked FelCloud; hard anti-affinity failed for lack of hosts |
