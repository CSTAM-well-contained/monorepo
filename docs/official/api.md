# REST API reference (`cpd`)

Base URL: `http://10.20.0.5:8080` on FelCloud, `http://10.20.0.1:8080` in the local demo.
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
`app_port` defaults to 80. On FelCloud the `cstam-team` security group only lets the gateways
reach ports **80 and 8000-8099**: use one of those. `image` is optional: without it the sandbox serves a page with the team
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
{"generation": 7,
 "ip_pool": {"allocated": 3, "free": 152, "quarantine": 4, "reserved": 1},
 "teams": {"ready": 3, "deleted": 4},
 "gateways": [{"name": "gw1", "acked_generation": 7, "config_version": "v1",
               "vrrp_state": "MASTER", "vrrp_since": "...", "last_seen": "..."}]}
```

## Gateway endpoints (used by `gwagent`)

### `GET /v1/gateways/{gw}/desired?after=N&wait=25s` -> 200
Long-poll: answers at once if the routes generation differs from `N` (or without `after`),
otherwise when it changes or after `wait` (at most 25 s).
```json
{"generation": 7, "routes": [{"host": "team5.hack1.cstam.felcloud.tn", "slot": 5,
                               "addr": "10.20.0.36", "port": 8080}]}
```

### `POST /v1/gateways/{gw}/ack` -> 200
```json
{"generation": 7, "config_version": "v1", "vrrp_state": "MASTER"}
```
Records what the gateway serves. A generation ahead of the database is refused (409): it would let
the sweep free IPs too early.

## Example session
```bash
A=(-H "Authorization: Bearer $CSTAM_ADMIN_TOKEN" -H "Content-Type: application/json")
curl "${A[@]}" -X POST $CPD/v1/hackathons -d '{"name":"hack1","domain":"hack1.cstam.test"}'
curl "${A[@]}" -X POST $CPD/v1/hackathons/hack1/teams -d '{"name":"team1"}'
curl "${A[@]}" $CPD/v1/hackathons/hack1/teams          # wait for "state":"ready"
curl -H "Host: team1.hack1.cstam.test" http://10.20.0.10/
curl "${A[@]}" -X DELETE $CPD/v1/hackathons/hack1
```
