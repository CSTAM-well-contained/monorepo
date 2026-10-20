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
web/            admin dashboard (plain HTML/JS, embedded in cpd, served at /)
deploy/         cloud-init, systemd units, demo scripts (deploy/dev), FelCloud isolation lab, video recorder
infra/pulumi/   infrastructure as code (Go): router, security groups, ports, VMs, floating IPs
infra/ssh-keys/ public SSH keys of the team (installed on every VM)
bench/          load, failover and stress test tools
docs/           brief, implementation guide, team setup, lab notebook
```

## Phase 2 submission (integration and testing)

| Deliverable | Where |
|---|---|
| Performance benchmark report | [PDF](docs/official/pdf/CSTAM-benchmark-report.pdf) · [benchmark.md](docs/official/benchmark.md) · [Phase 2 validation](docs/official/phase2.md) |
| API integration and security architecture | [PDF](docs/official/pdf/CSTAM-api-security.pdf) · [api.md](docs/official/api.md) · [openapi.yaml](docs/official/openapi.yaml) · [security.md](docs/official/security.md) · [architecture.md](docs/official/architecture.md) |
| Video (2:34) | link submitted via SwissTransfer; made with [`deploy/video/`](deploy/video/make-video.ps1), script in [video-script.md](docs/official/video-script.md) |

Measured: 20 and 50 simultaneous team registrations, 0 errors; teardown and lease expiry purge
sandboxes, routes and IPs automatically (IPs recycled only after both gateways confirm);
isolation between sandboxes **measured on FelCloud**; one flooded team refused while the others
see 0 errors; 3,000 req/s through the gateways with 0 errors; planned failover in 0.3 s.

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
- **Phase 1** built and tested: control plane `cpd` (REST API, IPAM, lifecycle), OpenStack VM
  sandboxes (tested on FelCloud), HA gateways with zero-downtime reload and automatic rollback.
- **Phase 2** validated ([phase2.md](docs/official/phase2.md)): stress test, automated teardown,
  network isolation measured on FelCloud.
- **Phase 3** admin dashboard built ([dashboard.md](docs/official/dashboard.md)): live teams, IPs,
  gateways, traffic; spin-up, teardown, planned failover, config rollback; read-only jury token.
- FelCloud infrastructure is deployed except the public floating IPs: FelCloud's public pool is
  full (`ExternalIpAddressExhausted`), so the public demo on FelCloud waits for FelCloud.
What is left: [`docs/HANDOFF.md`](docs/HANDOFF.md).
