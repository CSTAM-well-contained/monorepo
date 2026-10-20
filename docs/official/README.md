# CSTAM Resilient IP Optimizer: official documentation

Deliverable documents for the FelCloud challenge ([brief](../challenge/brief.md)). The team's
working notes are elsewhere: [HANDOFF](../HANDOFF.md), [lab notebook](../notebook/experiments.md),
[build guide](../guide/chapters/).

| Document | Content |
|---|---|
| [architecture.md](architecture.md) | Components, HA gateway data flow, provisioning and teardown flows, IPAM lifecycle, security |
| [api.md](api.md) | REST API reference, client integration guide, OpenStack API integration |
| [openapi.yaml](openapi.yaml) | OpenAPI 3.1 spec of the REST API (checked against the code by a test) |
| [security.md](security.md) | Threat model, trust boundaries, firewall table, gateway and API protections, secrets, verification, known gaps |
| [dashboard.md](dashboard.md) | Admin dashboard: pages, manual controls, security |
| [benchmark.md](benchmark.md) | Measured results: reload, rollback, failover (also under load), provisioning, stress test, HTTP load test, IP recycling |

## Challenge requirements and where they are met

| Requirement (points) | Implementation | Evidence |
|---|---|---|
| API-triggered sandbox provisioning (15) | `cpd` REST API -> controller -> sandbox driver (`internal/openstack`: one VM per team on a Neutron port with a fixed IP; `internal/docker`: one container per team for local runs) | benchmark B4, B5 |
| Dual HA gateways, subdomain routing, failover (15) | gw1/gw2: HAProxy 3.2 + keepalived VRRPv3 (unicast, 0.5 s) + `gwagent`; routing by `Host` through 128 slot backends | B2, B6 |
| IPAM and reclamation, short leases (10) | `internal/ipam` in PostgreSQL: lease per team, quarantine until every gateway acked, sweep | B3, B7 |
| Zero-downtime reload, validation, sub-second rollback (10) | `internal/gw/applier`: render -> `haproxy -c` -> atomic swap -> master-CLI reload -> verify -> rollback | B1 |
| Stress test, 10+ simultaneous teams (Phase 2, 15) | `deploy/dev/demo.sh stress N` | B5 |
| Automated teardown (Phase 2, 10) | reconciler: lease expiry, hackathon end, failed-team cleanup, IP sweep | B7 |
| Network isolation (Phase 2, 5) | Neutron security group `cstam-team`: only the gateways reach team ports; `/__gw` hidden from outside; per-team rate limit | security.md, B8 |
| Admin dashboard (Phase 3, 10) | `web/` (plain HTML/JS embedded in `cpd`, served at `/`): teams and private IPs, gateway MASTER/BACKUP live with alerts, subdomains, live traffic log, IP pool grid, config versions; buttons for spin-up, teardown, lease extension, planned failover/failback, config push and rollback. Every value and rule comes from the API | dashboard.md, notebook L10-L13 |
| Benchmark report (deliverable) | provisioning latency B4/B5, failover B6/B8, IP recycling B7, load test B8 | benchmark.md |
| API and security documentation (deliverable) | REST + OpenStack integration, OpenAPI spec, security architecture | api.md, openapi.yaml, security.md |

## Run it yourself
```bash
make build
deploy/dev/demo.sh up          # Postgres, cpd, gw1, gw2 on a Docker network with the FelCloud IP plan
deploy/dev/demo.sh teams 3     # create teams, curl each one through the VIP
deploy/dev/demo.sh failover    # freeze the MASTER gateway, measure the takeover
deploy/dev/demo.sh reload      # broken config rejected and rolled back on real HAProxy
deploy/dev/demo.sh config      # the same through cpd on the running gateways, then a manual rollback
deploy/dev/demo.sh failover planned   # planned failover through the API
# dashboard: http://10.20.0.1:8443/ , token in $XDG_RUNTIME_DIR/cstam-demo/admin.token
deploy/dev/demo.sh stress 20   # 20 registrations at once
deploy/dev/demo.sh load 20 64  # HTTP load through the VIP (add pause|kill: failover under load)
deploy/dev/demo.sh teardown    # end the hackathon, watch the IPs go back to the pool
deploy/dev/demo.sh down
```
