# 19. Step 10 – Gate G2: one team end to end on FelCloud

Up to now every piece was built and tested on its own. The control plane ran against a fake Neutron, the gateway agent was tested against a local HAProxy, and the host agent was tested on one host. This step joins them on the real cloud for the first time. At the end, one `curl` call creates a team. Its subdomain answers through the floating IP. A second `curl` deletes it, and nothing is left behind.

This is the most important gate in the project. Every scored item after this point (stress test, failover benchmark, dashboard, security suite) assumes that one team works end to end. If it does not, adding features only adds more places for bugs to hide.

::: note
**Goal:** `POST /v1/hackathons/{id}/teams` → team ACTIVE → `http://team1.hacka.<domain>/` returns the team's app through the floating IP → `DELETE` → route gone on both gateways, netns gone, Neutron port gone, IP in QUARANTINE then FREE. · **Points served:** Provisioning (15), IPAM (10), Teardown (10), and the base for everything else · **Time estimate:** 1.5–3 days (most of it is debugging the first run) · **Depends on:** Steps 0–9
:::

## 10.1 Understand first: what "end to end" really exercises

One registration touches every component and every trust boundary in the system:

| Hop | Component | What can break here for the first time |
|---|---|---|
| 1 | Browser or `curl` → cpd public API (:443) | TLS certificate, admin token, OpenAPI validation |
| 2 | cpd → PostgreSQL | Migrations not applied, wrong `CSTAM_DATABASE_URL`, peer auth |
| 3 | cpd controller → Neutron (gophercloud) | Application credential access rules too narrow, wrong network or subnet ID |
| 4 | cpd → Nova (attach interface) | `server add port` permission, hot-plug limit, Nova per-instance lock |
| 5 | hostagent ← cpd (agent dials out over mTLS) | Certificates, clock skew, SG rule for 8443 from `cstam-host` |
| 6 | hostagent → kernel (netns, nft) and Docker | NIC not found by MAC, MTU, pause image not pulled |
| 7 | gwagent ← cpd (desired routes, generation) | Generation not acked, digest mismatch, slot pool full |
| 8 | HAProxy → team IP:80 | SG `cstam-team` missing the rule from `cstam-gw`, app not listening |
| 9 | Floating IP → VIP → master gateway | VRRP state, OVN virtual port binding (already proven at G1) |

Each hop has a log and a command that shows its state. The method in this step is simple: run one registration, and check each hop in order until you find the first one that is wrong. Never debug from the browser backwards. Always go forwards from the API.

## 10.2 Deploy everything

### 10.2.1 Build the binaries

All four binaries are static Go binaries. They do not need anything installed on the VMs except systemd, and for the agents, the tools they drive (HAProxy, keepalived, Docker, nft).

```bash
# on your laptop, in the repo root
make gen                               # sqlc, oapi-codegen, buf generate, openapi-typescript
make web                               # npm ci && npm run build -> web/dist (embedded in cpd)
VERSION=$(git describe --tags --always --dirty)
for b in cpd gwagent hostagent cstamctl; do
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" -o bin/$b ./cmd/$b
done
ls -lh bin/                            # each binary 15-40 MB
go version -m bin/gwagent | grep -c gophercloud   # expected: 0 (agents never link the OpenStack SDK)
```

The last check is a small proof of the architecture: the gateway agent does not contain OpenStack code, so it cannot hold OpenStack credentials by accident.

### 10.2.2 Create the PKI

The internal CA (Step 9) signs one certificate per component. Run this on your laptop and keep `ca.key` offline (an encrypted USB key, or `age`-encrypted in a folder outside the repo).

```bash
./bin/cstamctl pki init --out ~/cstam-pki                 # ca.crt, ca.key (ECDSA P-256, 1 year)
./bin/cstamctl pki issue --ca ~/cstam-pki --name cpd       --ip 10.20.0.5  --uri spiffe://cstam/cpd
./bin/cstamctl pki issue --ca ~/cstam-pki --name gw1       --ip 10.20.0.11 --uri spiffe://cstam/gateway/gw1
./bin/cstamctl pki issue --ca ~/cstam-pki --name gw2       --ip 10.20.0.12 --uri spiffe://cstam/gateway/gw2
./bin/cstamctl pki issue --ca ~/cstam-pki --name host-a    --ip 10.20.0.21 --uri spiffe://cstam/host/host-a
ls ~/cstam-pki                          # ca.crt ca.key cpd.crt cpd.key gw1.crt gw1.key ...
openssl x509 -in ~/cstam-pki/gw1.crt -noout -ext subjectAltName
# expected: IP Address:10.20.0.11, URI:spiffe://cstam/gateway/gw1
```

::: warn
**Never copy `ca.key` to a VM.** Only leaf certificates and their keys go to the machines. If the control plane is ever compromised, the attacker must not be able to mint certificates for a fake gateway.
:::

### 10.2.3 Enroll each node

`cloud-init` installed only packages (Step 2). Secrets, certificates and binaries arrive afterwards over SSH, through the control plane VM acting as the bastion. `cstamctl node enroll` wraps the commands below. Run them by hand once so you know what it does.

```bash
# ~/.ssh/config (laptop)
# Host cstam-cp
#     HostName <cp floating IP>
#     User ubuntu
#     IdentityFile ~/.ssh/id_ed25519
# Host gw1 gw2 host-a host-b
#     ProxyJump cstam-cp
#     User ubuntu
# Host gw1
#     HostName 10.20.0.11
# Host gw2
#     HostName 10.20.0.12
# Host host-a
#     HostName 10.20.0.21

./bin/cstamctl node enroll gw1 --role gateway --pki ~/cstam-pki --bin bin/gwagent
```

What `node enroll gw1` does, step by step:

```bash
scp bin/gwagent gw1:/tmp/gwagent
scp ~/cstam-pki/{ca.crt,gw1.crt,gw1.key} gw1:/tmp/
scp deploy/env/gwagent.env.gw1 gw1:/tmp/gwagent.env
scp deploy/systemd/gwagent.service gw1:/tmp/
ssh gw1 'sudo install -m 0755 /tmp/gwagent /usr/local/bin/gwagent &&
         sudo install -d -m 0750 -o cstam -g cstam /etc/cstam /etc/cstam/pki /var/lib/cstam &&
         sudo install -m 0640 -o root -g cstam /tmp/ca.crt /tmp/gw1.crt /etc/cstam/pki/ &&
         sudo install -m 0400 -o cstam -g cstam /tmp/gw1.key /etc/cstam/pki/ &&
         sudo install -m 0600 -o cstam -g cstam /tmp/gwagent.env /etc/cstam/gwagent.env &&
         sudo install -m 0644 /tmp/gwagent.service /etc/systemd/system/ &&
         sudo shred -u /tmp/gw1.key && rm -f /tmp/gwagent /tmp/*.crt /tmp/gwagent.env &&
         sudo systemctl daemon-reload && sudo systemctl enable --now gwagent'
```

Repeat for gw2 (`--role gateway`), host-a (`--role host --bin bin/hostagent`) and the control plane (`--role cp --bin bin/cpd`). The control plane also receives the restricted OpenStack application credential (Step 9.4) as `/etc/cstam/secrets/clouds.yaml`, mode 0600, loaded with systemd `LoadCredential=`.

### 10.2.4 Environment files

Each daemon reads its configuration from environment variables (package `caarlos0/env`). The `.env.example` files in `deploy/env/` are the documentation. The values that matter for the first run:

```bash
# /etc/cstam/cpd.env  (control plane, 10.20.0.5)
CSTAM_ROLES=api,controller
CSTAM_DATABASE_URL=postgres:///cstam?host=/var/run/postgresql
CSTAM_PUBLIC_ADDR=:443                 # dashboard + REST API (SG: ADMIN_CIDR only)
CSTAM_AGENT_ADDR=:8443                 # agents dial in here (SG: cstam-gw, cstam-host)
CSTAM_METRICS_ADDR=127.0.0.1:9090
OS_CLOUD=cstam
OS_CLIENT_CONFIG_FILE=${CREDENTIALS_DIRECTORY}/clouds.yaml
CSTAM_NETWORK_ID=31953abe-d3d8-46b5-aa73-173b7e3ebd58
CSTAM_SUBNET_ID=256466d8-f51d-4136-b1b3-c66d183f8c77
CSTAM_TEAM_SG=cstam-team
CSTAM_ENV=dev                          # goes into the tag cstam-env:dev
CSTAM_BASE_DOMAIN=cstam.felcloud.tn    # or <FIP-with-dashes>.sslip.io
CSTAM_QUARANTINE=60s
CSTAM_NEUTRON_RPS=10
CSTAM_NEUTRON_MAX_INFLIGHT=8
CSTAM_TLS_CERT=/etc/cstam/pki/cpd.crt
CSTAM_TLS_KEY=/etc/cstam/pki/cpd.key
CSTAM_TLS_CA=/etc/cstam/pki/ca.crt

# /etc/cstam/gwagent.env  (gw1)
CSTAM_AGENT_NAME=gw1
CSTAM_CP_URL=https://10.20.0.5:8443
CSTAM_TLS_CERT=/etc/cstam/pki/gw1.crt
CSTAM_TLS_KEY=/etc/cstam/pki/gw1.key
CSTAM_TLS_CA=/etc/cstam/pki/ca.crt
CSTAM_HAPROXY_ADMIN_SOCK=/run/haproxy/admin.sock
CSTAM_HAPROXY_MASTER_SOCK=/run/haproxy-master.sock
CSTAM_SLOTS=128
CSTAM_LOG_LISTEN=127.0.0.1:5514

# /etc/cstam/hostagent.env  (host-a)
CSTAM_AGENT_NAME=host-a
CSTAM_CP_URL=https://10.20.0.5:8443
CSTAM_NET_DRIVER=podnic                # or "aap" if Gate G1 chose the fallback
CSTAM_PAUSE_IMAGE=registry.k8s.io/pause:3.10
CSTAM_MTU=1442
CSTAM_GATEWAYS=10.20.0.11,10.20.0.12
```

The network and subnet IDs above are the real ones from FelCloud (read-only checks). Check them again with `openstack network show cstam-sandbox-net -f value -c id` before using them.

### 10.2.5 Systemd units

All three daemons use `Type=notify`: systemd marks a unit "started" only when the program sends `READY=1`. The agents send it only after they synced with the control plane, or after they decided to run with their last-good state (static stability, Step 4 and Step 5).

```ini
# /etc/systemd/system/cpd.service
[Unit]
Description=CSTAM control plane
After=network-online.target postgresql.service
Wants=network-online.target
Requires=postgresql.service

[Service]
Type=notify
User=cstam
Group=cstam
EnvironmentFile=/etc/cstam/cpd.env
LoadCredential=clouds.yaml:/etc/cstam/secrets/clouds.yaml
ExecStartPre=/usr/local/bin/cpd migrate
ExecStart=/usr/local/bin/cpd
Restart=always
RestartSec=1
WatchdogSec=30
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ReadWritePaths=/var/lib/cstam
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
SystemCallFilter=@system-service

[Install]
WantedBy=multi-user.target
```

```ini
# /etc/systemd/system/gwagent.service
[Unit]
Description=CSTAM gateway agent
After=network-online.target haproxy.service
Wants=network-online.target

[Service]
Type=notify
User=cstam
Group=haproxy
EnvironmentFile=/etc/cstam/gwagent.env
ExecStart=/usr/local/bin/gwagent
Restart=always
RestartSec=1
WatchdogSec=30
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/etc/haproxy /var/lib/cstam /run/cstam
CapabilityBoundingSet=

[Install]
WantedBy=multi-user.target
```

The gateway agent needs to run `haproxy -c` (no privilege needed) and to talk to the master CLI socket for reloads (group `haproxy`, mode 660). It does not need `systemctl reload` because the master CLI `reload` command does the same job.

```ini
# /etc/systemd/system/hostagent.service
[Unit]
Description=CSTAM host agent
After=network-online.target docker.service
Requires=docker.service

[Service]
Type=notify
User=root
EnvironmentFile=/etc/cstam/hostagent.env
ExecStart=/usr/local/bin/hostagent
Restart=always
RestartSec=1
WatchdogSec=30
NoNewPrivileges=yes
ProtectHome=yes
PrivateTmp=yes
CapabilityBoundingSet=CAP_NET_ADMIN CAP_SYS_ADMIN CAP_NET_RAW CAP_DAC_OVERRIDE CAP_CHOWN

[Install]
WantedBy=multi-user.target
```

::: warn
**Be honest about the host agent.** It runs as root and talks to the Docker socket. Docker access is equivalent to root on the host. The mitigation is that the agent has no listening port at all (it dials out to the control plane), its API is typed (no "run this command"), and the only identity allowed to send it desired state is the control plane certificate. Write this sentence in the security document; a jury member will respect that you know it.
:::

### 10.2.6 Check that every agent is connected

```bash
ssh cstam-cp 'journalctl -u cpd -n 50 -o cat | jq -c "select(.msg|test(\"agent\"))"'
# expected lines:
# {"msg":"agent connected","agent":"gw1","identity":"spiffe://cstam/gateway/gw1"}
# {"msg":"agent connected","agent":"gw2","identity":"spiffe://cstam/gateway/gw2"}
# {"msg":"agent connected","agent":"host-a","identity":"spiffe://cstam/host/host-a"}

curl -s -H "Authorization: Bearer $CSTAM_TOKEN" https://<cp-fip>/v1/gateways | jq '.items[] | {name, vrrp, synced, acked_generation}'
# expected: gw1 MASTER synced true, gw2 BACKUP synced true, same acked_generation
```

If an agent is missing, go to the troubleshooting table (section 10.7, rows "mTLS" and "agent cannot reach cp").

## 10.3 Register the first hackathon and host

The hackathon and its host are created by an admin, once per event. In production a script does this. For the first run, use `curl` so you see every field.

```bash
API=https://<cp-fip>
H="Authorization: Bearer $CSTAM_TOKEN"

curl -s -X POST $API/v1/hackathons -H "$H" -H 'Content-Type: application/json' -d '{
  "slug": "hacka",
  "name": "CSTAM test hackathon A",
  "starts_at": "2026-10-20T08:00:00Z",
  "ends_at":   "2026-10-21T20:00:00Z",
  "max_teams": 40,
  "default_ttl": "4h"
}' | jq .
# -> {"id":"0190...","slug":"hacka","status":"DRAFT",...}

HID=<id from above>
SERVER_ID=$(openstack server show cstam-host-a -f value -c id)
PORT_ID=$(openstack port show cstam-host-a-port -f value -c id)

curl -s -X POST $API/v1/hosts -H "$H" -H 'Content-Type: application/json' -d "{
  \"hackathon_id\": \"$HID\",
  \"name\": \"host-a\",
  \"nova_server_id\": \"$SERVER_ID\",
  \"neutron_port_id\": \"$PORT_ID\",
  \"mgmt_ip\": \"10.20.0.21\",
  \"capacity\": 20,
  \"warm_pool\": 4
}" | jq .

curl -s -X PATCH $API/v1/hackathons/$HID -H "$H" -H 'Content-Type: application/json' -d '{"status":"OPEN"}'
```

With the pod-NIC driver, opening the hackathon starts the **warm pool**: the controller creates 4 Neutron ports (state `WARM` in the IPAM), attaches them to host-a one at a time, and the host agent leaves them unconfigured in the root namespace. Watch it:

```bash
watch -n1 "openstack port list --tags cstam,cstam-env:dev -c Name -c Status -c 'Fixed IP Addresses' -f value"
# after ~10-25 s: 4 ports named cstam-warm-<uuid>, status ACTIVE, IPs from 10.20.0.32 upwards
ssh host-a 'ip -br link | grep -v -E "^(lo|ens3|docker)"'
# 4 extra NICs (ens4..ens7 or similar), state DOWN, no address
```

::: why
**Why a warm pool, again?** Nova serializes interface attaches per instance: about 2–6 s each (expected, measured in H1). Ten simultaneous registrations would wait in line for 20–60 s. With pre-attached spare NICs, a registration only renames and re-tags a port (one fast Neutron `PUT`) and plumbs a namespace (milliseconds). The refill happens in the background, after the burst.
:::

## 10.4 Create the first team and watch every hop

Open four terminals before you send the request. This is the moment where you learn the most about your own system.

```bash
# T1: control plane decisions
ssh cstam-cp 'journalctl -fu cpd -o cat | jq -c "{t:.time,team:.team,step:.step,msg:.msg,err:.err}"'
# T2: gateway that is MASTER
ssh gw1 'journalctl -fu gwagent -o cat | jq -c "{t:.time,gen:.generation,msg:.msg}"'
# T3: host agent
ssh host-a 'journalctl -fu hostagent -o cat | jq -c "{t:.time,team:.team,msg:.msg,err:.err}"'
# T4: your commands
```

In T4:

```bash
curl -s -X POST $API/v1/hackathons/$HID/teams -H "$H" \
  -H "Idempotency-Key: $(uuidgen)" -H 'Content-Type: application/json' \
  -d '{"name":"team1","apps":[{"name":"web","image":"traefik/whoami:v1.10","port":80}]}' | jq .
# HTTP 202
# {"id":"0190a1...","name":"team1","fqdn":"team1.hacka.cstam.felcloud.tn",
#  "desired":"PRESENT","phase":"REQUESTED","ip":null,...}
TID=<id>
watch -n0.5 "curl -s -H '$H' $API/v1/teams/$TID | jq -c '{phase, ip, conditions:[.conditions[]|{type,status}]}'"
```

The expected sequence in the logs (times are estimates; record your real ones in `docs/notebook/experiments.md`):

| t (s) | Where | Log line (simplified) | What it proves |
|---|---|---|---|
| 0.00 | cpd | `team claimed epoch=1`, `ip leased 10.20.0.32 (WARM→LEASED)` | IPAM transaction works, warm port taken |
| 0.05 | cpd | `step neutron.port: adopt warm port, rename cstam-team-<uuid>, tags` | Neutron credential and tags work |
| 0.40 | cpd | `step host.sync: desired_rev=7 sent` | the host's desired state changed |
| 0.45 | host-a | `team1: NIC ens4 (fa:16:3e:..) → netns team1, 10.20.0.32/24, mtu 1442` | NIC found by MAC, moved, configured |
| 0.60 | host-a | `team1: nft applied`, `team1-pod up`, `team1-web started` | firewall and containers |
| 0.90 | host-a | `ack desired_rev=7` | host converged |
| 0.95 | cpd | `step route: route added generation=12, slot be_slot001` | generation bump |
| 1.00 | gw1 / gw2 | `apply gen=12: set server be_slot001/s addr 10.20.0.32:80; add map` then `ack gen=12 digest=…` | both gateways converged |
| 1.20 | cpd | `step health: GET 10.20.0.32/ via gw1 → 200` | the gateway really reaches the team |
| 1.25 | cpd | `team ACTIVE, lease ACTIVE until 2026-…` | done |

Then test from your laptop, through the floating IP:

```bash
curl -s http://team1.hacka.cstam.felcloud.tn/          # with FelCloud wildcard DNS
# or, without DNS:
curl -s -H 'Host: team1.hacka.cstam.felcloud.tn' http://$FIP/
# whoami answers: Hostname: ..., IP: 10.20.0.32, X-Forwarded-For: <your IP>, X-Served-By header from HAProxy
curl -sI -H 'Host: nope.hacka.cstam.felcloud.tn' http://$FIP/ | head -1     # HTTP/1.1 404 Not Found
```

### 10.4.1 Inspect every layer by hand

Do this once carefully. It is also exactly the tour you will give the jury.

```bash
# Neutron: one port, our tags, ACTIVE, bound to host-a
openstack port list --tags cstam-team:$TID -f json | jq '.[] | {Name, Status, "Fixed IP Addresses"}'
openstack port show cstam-team-$TID -c device_id -c security_group_ids -c tags
# device_id = host-a server id; security group = cstam-team only

# HAProxy on BOTH gateways: map entry + slot address + server state
for g in gw1 gw2; do
  echo "== $g"
  ssh $g 'echo "show map /etc/haproxy/maps/hosts.map" | sudo socat stdio /run/haproxy/admin.sock'
  ssh $g 'echo "show servers state be_slot001" | sudo socat stdio /run/haproxy/admin.sock | tail -1'
  ssh $g 'grep -c team1 /etc/haproxy/maps/hosts.map'      # the file on disk matches memory: 1
done

# host-a: namespace, address, routes, firewall, containers
ssh host-a 'sudo ip netns list'                                  # team1 (id: ...)
ssh host-a 'sudo ip -n team1 -br addr'                           # lo UNKNOWN 127.0.0.1/8 ; eth0 UP 10.20.0.32/24
ssh host-a 'sudo ip -n team1 route'                              # default via 10.20.0.1 dev eth0
ssh host-a 'sudo ip -n team1 link show eth0 | grep -o "mtu [0-9]*"'   # mtu 1442
ssh host-a 'sudo ip netns exec team1 nft list table inet pod | head -20'
ssh host-a 'docker ps --filter label=cstam.team=team1 --format "{{.Names}} {{.Status}}"'

# database view
ssh cstam-cp "sudo -u cstam psql cstam -c \"select ip,state,current_lease_id is not null as leased from ip_addresses where ip='10.20.0.32'\""
ssh cstam-cp "sudo -u cstam psql cstam -c 'select name,acked_generation,vrrp,synced from gateways'"
```

### 10.4.2 Check isolation right away

Create a second team (`team2`) the same way, then check from inside team2's namespace that team1 cannot be reached. You will automate this properly in the security suite (Step 9), but seeing it once now is reassuring.

```bash
ssh host-a 'sudo ip netns exec team2 curl -s -m 3 http://10.20.0.32/ ; echo "exit=$?"'
# exit=28 (timeout): dropped by Neutron SG cstam-team (no self rule) and by team2's own nft output chain
ssh host-a 'sudo ip netns exec team2 curl -s -m 3 http://169.254.169.254/ ; echo "exit=$?"'
# exit=28: metadata blocked in the pod's nft output chain
ssh gw1 'curl -s -m 3 http://10.20.0.32/ | head -2'
# works: the gateway is allowed by cstam-team
```

## 10.5 Delete the team and prove nothing is left

```bash
curl -s -X DELETE $API/v1/teams/$TID -H "$H" | jq '{phase,desired}'
# {"phase":"ACTIVE","desired":"ABSENT"}  then quickly DELETING
watch -n0.5 "curl -s -H '$H' $API/v1/teams/$TID | jq -c '{phase,last_error}'"
```

The teardown runs in the locked order (Step 8). The host does not get to clean up until the gateways have removed the route. Expected log order:

1. `cpd`: `route removed generation=13` → gw1 and gw2 log `apply gen=13: del map, shutdown sessions, slot parked` → `ack gen=13`.
2. `cpd`: `master acked ≥ 13: subdomain now 404`.
3. `host-a`: `team1: containers removed, netns deleted, NIC back in root netns`.
4. `cpd`: `detach port (server remove port)`, then `delete port`.
5. `cpd`: `ip 10.20.0.32 QUARANTINE until … required_ack=13`.
6. 60 s later, the sweeper: `ip 10.20.0.32 FREE` (only if both gateways acked ≥ 13 and Neutron shows no port with that IP).

The leftover check. Every line must print nothing or zero:

```bash
openstack port list --tags cstam-team:$TID -f value                               # (empty)
for g in gw1 gw2; do ssh $g "grep -c team1 /etc/haproxy/maps/hosts.map"; done      # 0, 0
for g in gw1 gw2; do ssh $g 'echo "show map /etc/haproxy/maps/hosts.map" | sudo socat stdio /run/haproxy/admin.sock | grep -c team1'; done  # 0, 0
ssh host-a 'sudo ip netns list | grep -c team1'                                    # 0
ssh host-a 'docker ps -a --filter label=cstam.team=team1 -q | wc -l'               # 0
ssh host-a 'sudo conntrack -L -d 10.20.0.32 2>/dev/null | wc -l'                   # 0
ssh cstam-cp "sudo -u cstam psql cstam -tAc \"select state from ip_addresses where ip='10.20.0.32'\""   # QUARANTINE, then FREE after 60 s
curl -sI -H 'Host: team1.hacka.cstam.felcloud.tn' http://$FIP/ | head -1         # 404
```

::: note
**Make this a script.** Put the leftover checks into `cstamctl doctor leftovers --team <id>`. You will run it hundreds of times in Step 11 (churn test) and once more live in front of the jury.
:::

## 10.6 The end-to-end test suite

Manual checks prove it works once. The `e2e` suite proves it keeps working after every change. It runs on your laptop or on the control plane VM against the real cloud, with a build tag so it never runs by accident in CI.

```go
//go:build e2e

// test/e2e/team_lifecycle_test.go
package e2e

func TestTeamLifecycle(t *testing.T) {
	env := LoadEnv(t)                 // API URL, admin token, FIP, hackathon id, OS_CLOUD for read-only checks
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	name := "e2e-" + randomSuffix(6)
	team := env.API.CreateTeam(ctx, t, env.HackathonID, name, whoamiApp())
	t.Cleanup(func() { env.API.DeleteTeamAndWait(context.Background(), t, team.ID) }) // also runs on failure

	ready := env.API.WaitPhase(ctx, t, team.ID, "ACTIVE", 60*time.Second)
	require.True(t, env.Pool.Contains(ready.IP), "IP must come from 10.20.0.32-191")

	// 1. Neutron view: exactly one tagged port with that IP
	ports := env.Neutron.PortsByTag(ctx, t, "cstam-team:"+team.ID)
	require.Len(t, ports, 1)
	require.Equal(t, ready.IP, ports[0].FixedIP)

	// 2. Both gateways serve it (ask each gateway directly, not only through the VIP)
	for _, gw := range env.Gateways { // 10.20.0.11, 10.20.0.12 reached from the cp VM
		body := HTTPGet(t, "http://"+gw+"/", team.FQDN)
		require.Contains(t, body, ready.IP.String())
	}
	// 3. Public path through the floating IP
	require.Equal(t, 200, HTTPStatus(t, "http://"+env.FIP+"/", team.FQDN))

	// 4. Delete, then no leftovers
	env.API.DeleteTeamAndWait(ctx, t, team.ID)
	require.Empty(t, env.Neutron.PortsByTag(ctx, t, "cstam-team:"+team.ID))
	require.Equal(t, 404, HTTPStatus(t, "http://"+env.FIP+"/", team.FQDN))
	for _, gw := range env.GatewaySSH {
		require.Zero(t, gw.CountMapEntries(t, team.FQDN))
	}
	require.Eventually(t, func() bool { return env.DB.IPState(t, ready.IP) == "FREE" },
		env.Quarantine+30*time.Second, time.Second)
}
```

Other tests in the same package:

| Test | What it checks |
|---|---|
| `TestTeamLifecycle` | above |
| `TestIdempotentCreate` | same `Idempotency-Key` twice → same team ID, one Neutron port |
| `TestDuplicateName` | two concurrent POSTs with the same name → one 202, one 409 |
| `TestTwoTeamsIsolated` | team A cannot reach team B (exec in netns via host SSH), gateway can reach both |
| `TestUnknownHost404` | random host header → 404 from both gateways |
| `TestDeleteWhileProvisioning` | DELETE right after POST → ends DELETED/FAILED, zero leftovers |
| `TestTeardownWithBackupGatewayDown` | stop gwagent on gw2, delete team, IP stays QUARANTINE; start gwagent, IP goes FREE |
| `TestHostAgentRestart` | `systemctl restart hostagent` → existing teams keep answering (0 errors in a 10 rps probe) |
| `TestControlPlaneDown` | stop cpd → team keeps answering through the FIP for 60 s |

Run them:

```bash
go test -tags e2e ./test/e2e -count=1 -v -timeout 20m 2>&1 | tee docs/e2e-$(date +%F).log
```

::: warn
**Clean up by tag, always.** Every test resource carries the tags `cstam` and `cstam-env:e2e`, and `t.Cleanup` deletes the team even when an assertion fails. After the suite, `openstack port list --tags cstam-env:e2e -f value | wc -l` must be 0. If it is not, run `cstamctl gc --env e2e --dry-run`, read the plan, then run it without `--dry-run`.
:::

## 10.7 Troubleshooting table

Go through the table in hop order (section 10.1). "Check" commands are read-only.

| Symptom | Likely cause | Check | Fix |
|---|---|---|---|
| Agent never shows "connected" in cpd logs | SG: no rule for 8443 from `cstam-gw`/`cstam-host` on `cstam-cp` | `openstack security group rule list cstam-cp`; on the agent VM: `nc -vz 10.20.0.5 8443` | Add the rule (Step 2.2). |
| Agent log: `x509: certificate signed by unknown authority` | Wrong `ca.crt` copied, or certificate issued from another CA run | `openssl verify -CAfile /etc/cstam/pki/ca.crt /etc/cstam/pki/gw1.crt` | Re-issue from the same CA, re-enroll. |
| Agent log: `x509: certificate is valid for 10.20.0.5, not …` | cpd certificate IP SAN does not match `CSTAM_CP_URL` | `openssl x509 -in cpd.crt -noout -ext subjectAltName` | Re-issue with `--ip 10.20.0.5`; use the IP, not a hostname, in `CSTAM_CP_URL`. |
| cpd rejects agent: `permission_denied: identity spiffe://cstam/host/host-a cannot act as gw1` | Env file copied from another node | `grep AGENT_NAME /etc/cstam/*.env` | Fix the name; identities must match. |
| Agent: `certificate has expired or is not yet valid` | Clock not synchronized | `chronyc tracking` (offset should be < 50 ms) | `sudo systemctl enable --now chrony`, wait, restart agent. |
| Team stuck at `PortReady=False reason=Forbidden` | Application credential access rules miss a path (for example `/v2.0/ports/*/` with trailing slash, or the Nova attach path) | cpd log shows method and path; test with `openstack --os-cloud cstam-runtime port list` | Add the exact path to the access rules, create a new credential, rotate (Reference B). |
| Team stuck at `PortReady=False reason=IpAddressAlreadyAllocated`, IP marked FOREIGN | A port that is not ours holds the IP (someone used `--fixed-ip` by hand) | `openstack port list --fixed-ip ip-address=10.20.0.X` | Expected behavior: IPAM marks it FOREIGN and leases another IP. If it loops, check the tag-matching code. |
| `server add port` fails: `No more available PCI slots` | Host VM machine type `q35` without spare root ports (H1) | in host: `cat /sys/class/dmi/id/product_name` | Gate G1 should have caught it: switch `CSTAM_NET_DRIVER=aap`, or lower `capacity`. |
| Host agent: `no NIC with MAC fa:16:3e:…` | Attach not finished yet (port ACTIVE before the guest sees the PCI device), or udev renamed it late | `ip -br link`; `dmesg | tail` (virtio_net probe) | Retry with backoff up to 10 s (already in the agent); if it never appears, detach/reattach. |
| NIC moved but `curl` from gw1 times out | Address/route missing in netns, or MTU, or nft input chain wrong | `ip -n team1 -br addr; ip -n team1 route`; `ip netns exec team1 nft list ruleset` | Re-run `Ensure` (idempotent). Check the gateway IPs in the nft set. |
| `curl` from gw1 times out, netns looks right | SG `cstam-team` missing the rule "tcp 80 from `cstam-gw`" or the port has the `default` SG | `openstack port show cstam-team-<id> -c security_group_ids` | Port must have exactly `cstam-team`. |
| Small responses work, large ones hang (pages load half, `git clone` stalls) | MTU: something still at 1500 inside a 1442 overlay | `ip -n team1 link show eth0`; `docker network inspect` (AAP mode); `ping -M do -s 1414 10.20.0.11` from netns | Set MTU 1442 everywhere; in AAP mode add MSS clamping (`--clamp-mss-to-pmtu`). |
| AAP mode only: team IP unreachable, `arping` from gw1 answers with an unknown MAC | OVN virtual port not bound to host-a (no GARP sent), or the reserved port was created after the AAP | `arping -c2 -I ens3 10.20.0.X` on gw1; compare MAC with `openstack port show cstam-host-a-port -c mac_address` | Host agent must run `arping -U -c3 -I ens3 <ip>` after `ip addr add`; create the port before adding the AAP. |
| AAP mode only: 1 team of 10 unreachable after a burst | Lost update on `allowed_address_pairs` | `openstack port show cstam-host-a-port -c allowed_address_pairs` vs the DB list | Bug: something wrote AAP outside the per-host syncer, or `If-Match` is not sent. |
| Gateways: `gwagent: digest mismatch at gen 12` | Agent rendered a different map than the cp snapshot (sorting, lowercase, trailing dot) | cpd log shows expected vs actual digest | Fix the canonical form (sorted `fqdn ip:port\n`, lowercase). The agent then does a full resync. |
| Team stuck `RoutesSynced=False waiting for gw2 ack ≥ 12` | gw2 agent stopped or disconnected | `curl $API/v1/gateways`; `ssh gw2 systemctl status gwagent` | Start it. If gw2 is dead for long, fence it (Reference B). This wait is the safety rule working. |
| Route present on gw1, missing on gw2 after failover | gw2 became MASTER while stale: the readiness gate is not wired into keepalived | `ssh gw2 cat /run/cstam/ready`; `grep chk_gateway /etc/keepalived/keepalived.conf` | `chk_gateway` must require `/run/cstam/ready`; the agent must remove it when generation lags. |
| `Success=0` on master CLI reload | New config valid for `haproxy -c` but fails at start (for example a cert file missing on this node only) | `echo "show startup-logs" \| socat /run/haproxy-master.sock stdio` | The agent already rolled back; fix the file distribution. |
| Health step fails with 503 maintenance page | Team app not listening on the declared port, or still starting | `ip netns exec team1 ss -ltn` | Health probe should target the pod's port; increase start timeout; the app spec port must match. |
| `DELETE` hangs in `DELETING` with `detach port: 409 Conflict` | Nova instance locked by another attach (warm-pool refill running) | cpd log; `openstack server event list cstam-host-a` | Retries are expected; if > 2 min, check that refill and detach use the same per-host semaphore. |
| IP stays QUARANTINE forever | One member gateway has `acked_generation` below the removal generation, or `neutron_absent_at` never set | `select name,acked_generation from gateways;` and `select * from ip_addresses where state='QUARANTINE'` | Bring the gateway back or fence it; check the reconciler's Neutron scan runs. |

## Done when

::: done
**Done when**

- `go test -tags e2e ./test/e2e -count=1` passes all tests listed in 10.6, twice in a row.
- `curl -H 'Host: team1.hacka.<domain>' http://$FIP/` returns the team app; `X-Served-By` shows the MASTER gateway.
- After `DELETE`, the leftover check in 10.5 prints only empty or zero results, and the IP is `FREE` 60 s later.
- `openstack port list --tags cstam,cstam-env:dev -f value | wc -l` equals (number of live teams + warm-pool size).
- Your `docs/notebook/experiments.md` contains the timing table of 10.4 with your real numbers.
:::

::: gate
**Gate G2 – one team end to end on FelCloud**

**GO** if all "Done when" items pass. Continue with Step 11 (stress, chaos, benchmarks) and Step 12 (dashboard wired to the real API).

**NO-GO** if any hop still fails after one day of debugging: stop all new feature work. Fix the integration first. Features built on a broken path will have to be debugged twice.

**Cut line 1 (apply now if you are behind schedule by more than 3 days):** postpone the VM-per-team driver, host re-homing, DNS delegation and custom domains. They are differentiators, not scored requirements.
:::

## Pitfalls

::: warn
**Debugging from the wrong end.** When the browser shows an error, the tempting reflex is to look at HAProxy first. Always start from the API response and walk forwards through the hops in 10.1. The first hop that disagrees with the expected state is the bug; the rest are consequences.
:::

::: warn
**"It worked once" is not "it works".** The first successful run often hides a race that only appears with two teams, or after a restart. Run the e2e suite twice, restart each agent once between the runs, and only then declare the gate passed.
:::

::: warn
**Leftover test resources consume the quota.** 500 ports sounds like a lot, but a test loop that leaks one port per run fills it in an afternoon. Check the tag count after every session.
:::

## Learning checkpoint

::: learn
**Answer these yourself before moving on**

1. Draw the path of one HTTP request from your laptop to the `whoami` container, naming every IP address and every component that touches it.
2. During `POST /teams`, which steps are inside one database transaction and which ones talk to an external system? Why must a transaction never stay open during a Neutron call?
3. Why does the teardown wait for the gateway acknowledgement before touching the host? What would go wrong if the order were reversed?
4. If the control plane dies right after Neutron created the port but before the database saved it, what happens on restart? Which mechanism finds and adopts the port?
5. Why does the gateway agent not need OpenStack credentials at all?
:::

## Driving Claude Code for this step

::: claude
**How to use Claude Code here**

This step is mostly running and observing, so use Claude Code as a debugging partner, not as an author.

- Paste the cpd, gwagent and hostagent log lines for one registration and ask: *"Here are the logs of one team registration in hop order. Find the first hop where the actual state differs from the expected state in section 10.4 of my guide, and explain why. Do not propose code yet."*
- For the e2e suite: *"Write the test `TestTeardownWithBackupGatewayDown` in test/e2e using the existing helpers. It must stop gwagent on gw2 through SSH, delete a team, assert the IP stays QUARANTINE for at least 2× the quarantine time, start gwagent, and assert the IP becomes FREE. Show me the plan first."*
- Review every diff that touches teardown ordering yourself, line by line. If you cannot explain why each step is in its place, do not merge it.
- Never let Claude Code run `openstack ... create/delete/set` commands. Ask it to print them; you run them.
:::
