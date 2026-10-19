# CSTAM × FelCloud: The Resilient IP Optimizer

Our entry for the CSTAM 3.0 challenge **CSTAM-FELCLOUD**: automated sandbox provisioning on
FelCloud's OpenStack, a redundant HAProxy + keepalived gateway pair, an IPAM that recycles
private IPs safely, zero-downtime reload with automatic rollback, and an admin dashboard.

## Layout

```
cmd/            cpd (control plane), gwagent, hostagent, cstamctl (CLI)
internal/       all Go packages (IPAM, controller, agents, API, ...)
api/            OpenAPI (public REST) and protobuf (agent protocol) contracts
migrations/     PostgreSQL schema (goose)
web/            admin dashboard (React)
deploy/         cloud-init, systemd units, HAProxy and keepalived templates
infra/pulumi/   infrastructure as code (Go): router, security groups, ports, VMs, floating IPs
infra/ssh-keys/ public SSH keys of the team (installed on every VM)
bench/          load, failover and stress test tools
docs/           brief, implementation guide, team setup, lab notebook
```

## Quick start

```bash
make lint test build              # Go code: lint, tests, binaries in bin/
make infra-check                  # Pulumi program: build + read-only preview of the cloud
make test-int                     # integration tests (Docker: PostgreSQL, HAProxy, keepalived)
deploy/dev/demo.sh up             # the whole platform on one machine (then: teams, failover, stress, down)
```
New teammate: read [`docs/README.md`](docs/README.md), then do [`docs/team/README.md`](docs/team/README.md).
Jury documents (architecture, API, benchmark report): [`docs/official/`](docs/official/README.md).

## Status
Phase 1 is built and tested: control plane `cpd` (REST API, IPAM, lifecycle), OpenStack VM
sandboxes (tested on FelCloud), HA gateways with zero-downtime reload and rollback, and a local
demo stack with a stress test (50 simultaneous teams). Infrastructure is deployed except the
floating IPs (waiting for FelCloud), so the public demo on FelCloud is pending.
What is left: [`docs/HANDOFF.md`](docs/HANDOFF.md).
