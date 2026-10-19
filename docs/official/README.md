# CSTAM Resilient IP Optimizer: official documentation

Deliverable documents for the FelCloud challenge ([brief](../challenge/brief.md)). The team's
working notes are elsewhere: [HANDOFF](../HANDOFF.md), [lab notebook](../notebook/experiments.md),
[build guide](../guide/chapters/).

| Document | Content |
|---|---|
| [architecture.md](architecture.md) | Components, HA gateway data flow, provisioning and teardown flows, IPAM lifecycle, security |
| [api.md](api.md) | REST API reference (admin and gateway endpoints), with curl examples |
| [benchmark.md](benchmark.md) | Measured results: reload, rollback, failover, provisioning, stress test, IP recycling |

## Challenge requirements and where they are met

| Requirement (points) | Implementation | Evidence |
|---|---|---|
| API-triggered sandbox provisioning (15) | `cpd` REST API -> controller -> sandbox driver (`internal/openstack`: one VM per team on a Neutron port with a fixed IP; `internal/docker`: one container per team for local runs) | benchmark B4, B5 |
| Dual HA gateways, subdomain routing, failover (15) | gw1/gw2: HAProxy 3.2 + keepalived VRRPv3 (unicast, 0.5 s) + `gwagent`; routing by `Host` through 128 slot backends | B2, B6 |
| IPAM and reclamation, short leases (10) | `internal/ipam` in PostgreSQL: lease per team, quarantine until every gateway acked, sweep | B3, B7 |
| Zero-downtime reload, validation, sub-second rollback (10) | `internal/gw/applier`: render -> `haproxy -c` -> atomic swap -> master-CLI reload -> verify -> rollback | B1 |
| Stress test, 10+ simultaneous teams (Phase 2, 15) | `deploy/dev/demo.sh stress N` | B5 |
| Automated teardown (Phase 2, 10) | reconciler: lease expiry, hackathon end, failed-team cleanup, IP sweep | B7 |
| Network isolation (Phase 2, 5) | Neutron security group `cstam-team`: only the gateways reach team ports | architecture.md, section 6 |

## Run it yourself
```bash
make build
deploy/dev/demo.sh up          # Postgres, cpd, gw1, gw2 on a Docker network with the FelCloud IP plan
deploy/dev/demo.sh teams 3     # create teams, curl each one through the VIP
deploy/dev/demo.sh failover    # freeze the MASTER gateway, measure the takeover
deploy/dev/demo.sh reload      # broken config rejected and rolled back on real HAProxy
deploy/dev/demo.sh stress 20   # 20 registrations at once
deploy/dev/demo.sh teardown    # end the hackathon, watch the IPs go back to the pool
deploy/dev/demo.sh down
```
