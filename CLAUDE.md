# CSTAM monorepo: rules for Claude Code

Project: the CSTAM 3.0 "Resilient IP Optimizer" challenge for FelCloud (OpenStack/OVN).
The design and the build plan are in `docs/guide/chapters/` (start with `docs/README.md`).
Follow the guide's step order and its go/no-go gates.

## How we work
- Explain before coding: the mechanism, the plan, the files you will touch, how we verify it.
- Small changes: one package or one behaviour at a time, under ~300 lines of diff.
- Every change has tests and a "done when" command a teammate can run.
- Unsure how FelCloud behaves? Say so and propose an experiment; record the result in
  `docs/notebook/experiments.md` (also read it first: it lists what we already verified on FelCloud).
- Do not commit or push unless asked.

## Hard rules
- Cloud changes only through Pulumi (`infra/pulumi`, shared state on FelCloud S3).
  Run `pulumi preview` and show the plan before `pulumi up`.
  Never `pulumi destroy` or `pulumi state unprotect` unless asked in the current conversation.
- No direct `openstack ... create/delete/set`, except Step 1 lab resources named `cstam-lab-*`
  and tagged `cstam-lab`. Read-only `list`/`show` is always fine.
- Never read, print or copy secrets: `.secrets/`, `*.key`, `*.pem`, `clouds.yaml`, `*.env`,
  `~/.aws/credentials`, `~/.config/cstam/`.
- No new Go dependency without asking.
- Run external commands with an argv slice, never `sh -c` with string concatenation.
- Validate every name that reaches HAProxy, nftables, iptables or Docker with
  `^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`.

## Code conventions
- Go 1.25, module `github.com/CSTAM-well-contained/monorepo`. Binaries in `cmd/`, the rest in
  `internal/`. `infra/pulumi` is a separate Go module.
- The IP plan lives in three files that must match: `internal/config/topology.go`,
  `infra/topology.env`, `infra/pulumi/config.go`. No hardcoded IPs elsewhere.
- Wrap errors with `%w`. No panics in agents. Every network call gets a context with a timeout.
- Logging: `log/slog`, JSON. Never log request bodies sent to OpenStack.
- Tests are table-driven. Build tags: `integration` (testcontainers), `hostint` (root on a host),
  `e2e` (real FelCloud).
- Agents (`gwagent`, `hostagent`) never import pgx or gophercloud (enforced by depguard).
- SQL lives in `internal/store/queries/*.sql` (sqlc). No ORM.
- Before handing work back: `make lint test build`.
