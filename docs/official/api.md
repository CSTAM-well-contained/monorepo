# REST API reference (`cpd`)

Machine-readable spec: [openapi.yaml](openapi.yaml) (OpenAPI 3.1; a test fails if a route of the
server is missing from it). Security model: [security.md](security.md).

Base URL: `http://10.20.0.5:8443` on FelCloud (the only cp port the `cstam-cp` security group
opens to the gateways), `http://10.20.0.1:8443` in the local demo.
All bodies are JSON. Authentication: `Authorization: Bearer <token>`.
- **admin token** (`CSTAM_ADMIN_TOKEN`): every `/v1` endpoint except the gateway ones;
- **gateway token** (`CSTAM_GATEWAY_TOKEN`): only `/v1/gateways/...`.

`GET /healthz` needs no token and answers `{"status":"ok"}`.

## Errors

`{"error": "<message>"}` with:

| Code | When |
|---|---|
| 400 | invalid name, domain, port, image, JSON or parameter |
| 401 | missing or wrong token |
| 404 | unknown hackathon or team |
| 415 | body is not `application/json` |
| 409 | name already used, hackathon not active, lease already expired |
| 503 | IP pool exhausted (`Retry-After: 60`) |
| 500 | `internal error` (details only in the cpd log) |

## Hackathons

### `POST /v1/hackathons` -> 201
```json
{"name": "hack1", "domain": "hack1.cstam.felcloud.tn", "lease_ttl_seconds": 7200}
```
`name` is a DNS label; teams are published at `<team>.<domain>`. `lease_ttl_seconds` is the team
lease (default 7200, at most 30 days).
Response: `{"id","name","domain","state":"active","lease_ttl_seconds","created_at"}`.

### `GET /v1/hackathons` -> 200, `GET /v1/hackathons/{h}` -> 200

### `DELETE /v1/hackathons/{h}` -> 202
Ends the hackathon: every team goes to `deleting`, routes are removed, sandboxes deleted, IPs
quarantined then freed. The hackathon goes to `ended` (its name stays taken).

## Teams

### `POST /v1/hackathons/{h}/teams` -> 202
```json
{"name": "team5", "app_port": 8080, "image": "nginx:1.27"}
```
`app_port` defaults to 80 and must be **80 or 8000-8099**, the ports the `cstam-team` security
group opens to the gateways (400 otherwise). `image` is optional: without it the sandbox serves a page with the team
name and IP. The answer comes before the sandbox exists (`Location: /v1/teams/{id}`):
```json
{"id": 12, "hackathon": "hack1", "name": "team5", "fqdn": "team5.hack1.cstam.felcloud.tn",
 "state": "provisioning", "ip": "10.20.0.36", "slot": 5, "app_port": 8080,
 "lease_expires_at": "2026-10-19T21:00:00Z", "created_at": "2026-10-19T19:00:00Z"}
```
States: `provisioning`, `ready` (routed), `failed` (`error` says why; cleaned up after 10 min),
`deleting`, `deleted`.

### `GET /v1/hackathons/{h}/teams` -> 200, `GET /v1/teams/{id}` -> 200

### `POST /v1/teams/{id}/renew` -> 200
Sets the lease to now + the hackathon's `lease_ttl_seconds`: `{"lease_expires_at": "..."}`.
409 if the lease already expired or the team is not `provisioning`/`ready`.

### `DELETE /v1/teams/{id}` -> 202
Starts the teardown (same order as a lease expiry).

## Status

### `GET /v1/status` -> 200
```json
{"generation": 7, "config_version": "c4", "vip": "10.20.0.10",
 "ip_pool": {"allocated": 3, "free": 152, "quarantine": 4, "reserved": 1},
 "teams": {"ready": 3, "deleted": 4},
 "gateways": [{"name": "gw1", "acked_generation": 7, "config_version": "c4",
               "vrrp_state": "MASTER", "vrrp_since": "...", "last_seen": "...",
               "alive": true, "in_sync": true, "force_backup": false,
               "config_tried": "c4", "config_outcome": "applied"}],
 "alerts": []}
```
`alerts` is computed by cpd, never by a client: split brain (two MASTERs), no MASTER, a gateway
silent for 40 s, behind the current generation, in FAULT, or refusing the newest config version.

### Dashboard reads
| Endpoint | Returns |
|---|---|
| `GET /v1/teams` | live teams of every hackathon, newest first |
| `GET /v1/events?after=ID&limit=N` | event log (team states, IP lease/quarantine/free, VRRP transitions, failovers, config outcomes), oldest first; without `after`, the last N |
| `GET /v1/ipam` | every address of the team pool with state, team and release time |
| `GET /v1/routes` | the routing table the gateways should serve now |
| `GET /v1/traffic?after=ID&host=FQDN&limit=N` | live traffic lines (client, host, method, path without query, status, timings, backend slot) and per-gateway req/s and 5xx over 10 s; in memory, best effort |

## Manual controls
| Endpoint | Effect |
|---|---|
| `POST /v1/gateways/{gw}/failover[?force=true]` -> 202 | planned failover: the gateway's agent writes 1 into keepalived's force-backup file, the instance goes to FAULT, the peer takes the VIP (about 0.3 s, notebook L10). 409 when no other gateway is alive, in sync, out of FAULT and not forced down, unless `force=true` |
| `POST /v1/gateways/{gw}/failback` -> 202 | clears it; the gateway comes back as BACKUP (`nopreempt`) |
| `POST /v1/config` -> 201 | new HAProxy config version `{"rate_limit": 1000, "extra": ["..."], "reason": "..."}`; each gateway checks, reloads hitlessly, verifies, rolls back by itself on failure, and reports `config_outcome` |
| `GET /v1/config` -> 200 | versions, newest first |
| `POST /v1/config/{v}/rollback` -> 201 | manual rollback: a new version with the content of `v` (history is never rewritten, so a gateway that was offline converges to the newest) |

## Gateway endpoints (used by `gwagent`)

### `GET /v1/gateways/{gw}/desired?after=N&wait=25s` -> 200
Long-poll: answers at once if the routes generation differs from `N` (or without `after`),
otherwise when it changes or after `wait` (at most 25 s).
```json
{"generation": 7, "routes": [{"host": "team5.hack1.cstam.felcloud.tn", "slot": 5,
                               "addr": "10.20.0.36", "port": 8080}],
 "force_backup": [], "config": {"version": "c4", "rate_limit": 2000}}
```
`force_backup` and `config` are part of the same generation: a failover or a config push bumps it,
which wakes the long-polls.

### `POST /v1/gateways/{gw}/traffic` -> 200
Batches of at most 150 traffic lines, about once a second, plus the count of lines dropped on
the gateway. Best effort: a failed batch is counted as dropped, never retried.

### `POST /v1/gateways/{gw}/ack` -> 200
```json
{"generation": 7, "config_version": "c4", "vrrp_state": "MASTER",
 "config_tried": "c4", "config_outcome": "applied", "config_error": ""}
```
Records what the gateway serves. A generation ahead of the database is refused (409): it would let
the sweep free IPs too early.

## Integrating a registration portal (client side)

A hackathon portal (or `curl`) drives the platform with the admin token:
1. `POST /v1/hackathons` once per event (`409` = it already exists: fine on a retry).
2. For each team: `POST /v1/hackathons/{h}/teams`. The answer (`202`) already holds the team's
   `ip`, `slot` and `fqdn`. **Retrying is safe**: a second POST with the same name gets `409`;
   then `GET /v1/hackathons/{h}/teams` finds the team. On `503` wait `Retry-After` seconds (pool
   full: IPs come back after teardown and the 60 s quarantine).
3. Poll `GET /v1/teams/{id}` (every 1-2 s) until `state` is `ready` (typically 2-5 s with the
   docker driver, about 25 s with one VM per team) or `failed` (`error` says why).
4. Call `POST /v1/teams/{id}/renew` before `lease_expires_at` to keep a team; otherwise it is torn
   down automatically.
5. `DELETE /v1/hackathons/{h}` at the end: every route is removed at once, sandboxes are deleted,
   IPs return to the pool when both gateways have acked.

Every answer is JSON; errors are `{"error": "..."}` with the codes above. Client errors (4xx) must
not be retried unchanged; `503` and network errors may be.

## OpenStack API integration (`cpd` -> FelCloud)

`cpd` is the only component with cloud rights (application credential from `clouds.yaml`, see
[security.md](security.md)). The VM driver (`internal/openstack`, gophercloud v2) uses:

| Step | API call | Notes |
|---|---|---|
| start | Keystone token from the application credential; Nova `GET /flavors`, Glance `GET /images` | flavor `G0.basic.1c1g` and image `Debian 12 - Bookworm` resolved once |
| create, 1 | Neutron `GET /ports?name=cstam-<h>-<team>` | idempotency: an existing port with the same IP is reused |
| create, 2 | Neutron `POST /ports` with `fixed_ips` = the team IP, `security_groups` = `cstam-team` | not retried (not idempotent); `409 IpAddressAlreadyAllocated` -> look again by name (our own lost answer) or `ErrIPTaken` (the IP stays quarantined) |
| create, 3 | Neutron `PUT /ports/{id}/tags` (`cstam`, `cstam-team`) | for garbage collection by tag |
| create, 4 | Nova `GET /servers?name=...`, then `POST /servers` with the port and cloud-init user-data | user-data serves the team page or runs the team image |
| create, 5 | Nova `GET /servers/{id}` every 2 s until `ACTIVE` (`ERROR` -> team `failed`), 5 min max | |
| delete | Nova `DELETE /servers/{id}`, wait for `404`, then Neutron `DELETE /ports/{id}` | refuses names without `cstam-`; `404` counts as done |
| IP check | Neutron `GET /ports?network_id=...&fixed_ips=ip_address=...` | the sweep never frees an IP a port still holds |
| reconcile (every 2 s) | the create and delete calls above, driven by the database | resumes teams left in `provisioning` or `deleting` (e.g. after a cpd restart), cleans failed teams, expires leases. Resources are found by their deterministic name; there is no scan of the cloud for orphans (a row deleted by hand from the database would leave its VM: `List` by prefix and tag exists for that, used by the tests) |

Errors: idempotent calls are retried 3 times (200, 400, 800 ms) on 429, 5xx and network errors;
at most 8 calls in flight. Quota errors (`OverQuota`, 413) map to `ErrQuota` and the team fails
with a clear message instead of retrying forever. Measured on FelCloud: VM ACTIVE in 23 s, server
and port deleted in 14 s (benchmark B4). Static infrastructure (network, router, security groups,
VIP and gateway ports, floating IPs) is created by Pulumi (`infra/pulumi`), not by `cpd`.

## Example session
```bash
A=(-H "Authorization: Bearer $CSTAM_ADMIN_TOKEN" -H "Content-Type: application/json")
curl "${A[@]}" -X POST $CPD/v1/hackathons -d '{"name":"hack1","domain":"hack1.cstam.test"}'
curl "${A[@]}" -X POST $CPD/v1/hackathons/hack1/teams -d '{"name":"team1"}'
curl "${A[@]}" $CPD/v1/hackathons/hack1/teams          # wait for "state":"ready"
curl -H "Host: team1.hack1.cstam.test" http://10.20.0.10/
curl "${A[@]}" -X DELETE $CPD/v1/hackathons/hack1
```
