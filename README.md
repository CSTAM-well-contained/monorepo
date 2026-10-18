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
```
New teammate: read [`docs/README.md`](docs/README.md), then do [`docs/team/README.md`](docs/team/README.md).

## Status
Infrastructure deployed except the floating IPs (waiting for FelCloud). Gateway reload/rollback
and keepalived failover built and tested locally. What is left: [`docs/HANDOFF.md`](docs/HANDOFF.md).
