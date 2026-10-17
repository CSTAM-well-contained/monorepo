# 12. Step 3 – The gateway pair (HAProxy + Keepalived)

## Step 3 – Two gateways, one VIP, routing by hostname with zero reloads

::: note
**Goal:** both gateways run the production HAProxy and Keepalived configurations by hand (no agent yet). A request for `team1.hacka.cstam.felcloud.tn` reaches a test backend through either gateway; teams can be added and removed with runtime commands and **no reload**; killing the master moves the VIP to the backup in about 1–2 s, measured.
**Points served:** dual redundant HA gateways (15), basis of zero-downtime reload (10), bonuses (maintenance page, rate limiting).
**Time estimate:** 1–1.5 days.
**Depends on:** Step 2. Gate G1 decided P (VRRP VIP) or a fallback.
:::

### Understand first

A request travels: browser → DNS (`*.cstam.felcloud.tn` → public FIP) → OVN router (FIP → VIP 10.20.0.10) → the gateway that currently holds the VIP → HAProxy reads the `Host` header → looks it up in a **map file** → sends the request to that team's IP and port.

Two programs run on each gateway:

- **HAProxy** does the routing. It is a full proxy: the client's TCP connection ends in HAProxy, which opens a second connection to the team.
- **Keepalived** decides which gateway holds the VIP, using VRRP between the two gateways (unicast, every 0.5 s). It also watches HAProxy: if HAProxy dies, the node gives up the VIP immediately.

The hard requirement is "change routes without downtime". HAProxy can change many things **at runtime** through its admin socket, without reloading: the address of a server, the content of a map. We design the configuration so that adding or removing a team never needs a reload.

#### Routing strategy: why "slot backends"

| Strategy | Team add/remove | Health check per team | Maintenance page per team | Per-team stats and rate limit | Reloads |
|---|---|---|---|---|---|
| A. One backend per team written into the config | reload every time | yes | yes | yes | every change |
| B. Map `host → ip:port` + one generic backend using `set-dst` | `add map` only | no (one fake server) | weak | weak | none |
| C. `server-template` in one backend + `use-server` rules | runtime, but `use-server` rules are static | yes | partly | per server | needs reload to route a new host |
| **D. 128 pre-declared slot backends (1 server each) + map `host → slot` + `set server addr`** | **runtime only** | **yes** | **yes** | **yes** | **none** (one reload only to resize the pool) |

**Verdict: D.** The configuration declares `be_slot001` … `be_slot128`. An unused slot has a placeholder address and is disabled. To add a team, the agent picks a free slot, sets the server address to the team IP, enables it, then adds `team1.hacka.cstam.felcloud.tn → be_slot001` to the map. All runtime, under a millisecond. A ran out of reloads (every team = reload); B loses health checks, which we need for the maintenance-page bonus and the dashboard.

One principle makes runtime changes safe: **the files on disk always describe the full desired state, and the runtime API brings memory to the same state.** After any reload, HAProxy re-reads the files, so nothing done at runtime is lost.

### Do it by hand

#### 3.1 HAProxy installation check

Cloud-init installed HAProxy 3.2 LTS from `ppa:vbernat/haproxy-3.2` (Ubuntu's own 2.8 LTS also works with every command below).

```bash
ssh cstam-gw1
haproxy -v | head -1                       # HAProxy version 3.2.x
systemctl cat haproxy | grep -E 'ExecStart|EXTRAOPTS'
# Debian/Ubuntu unit starts: haproxy -Ws -f $CONFIG -p $PIDFILE $EXTRAOPTS  with EXTRAOPTS=-S /run/haproxy-master.sock
ls -l /run/haproxy-master.sock            # the master CLI socket (used for reload with status)
```

If the master socket is missing, add `EXTRAOPTS="-S /run/haproxy-master.sock"` to `/etc/default/haproxy` and restart.

#### 3.2 Files

```text
/etc/haproxy/haproxy.cfg                    # rendered config (later by the agent)
/etc/haproxy/maps/hosts.map                 # fqdn -> slot backend
/etc/haproxy/maps/ratelimit.map             # fqdn -> requests per 10 s (bonus)
/etc/haproxy/maps/https-only.map            # fqdn -> 1 (bonus, after TLS)
/etc/haproxy/maps/meta.map                  # generation -> N (which desired state is live)
/etc/haproxy/errors/503-maintenance.http    # raw HTTP response used by errorfile
/etc/haproxy/errors/maintenance.html        # HTML for http-request return
/etc/haproxy/errors/404.html
/etc/haproxy/certs/default.pem              # self-signed placeholder so the :443 bind is valid
/etc/haproxy/certs/crt-list.txt             # empty until TLS (bonus)
```

```bash
sudo install -d -o root -g haproxy -m 0775 /etc/haproxy/maps /etc/haproxy/errors /etc/haproxy/certs
printf 'canary.gw.internal be_slot000\n' | sudo tee /etc/haproxy/maps/hosts.map
sudo touch /etc/haproxy/maps/ratelimit.map /etc/haproxy/maps/https-only.map /etc/haproxy/certs/crt-list.txt
printf 'generation 0\n' | sudo tee /etc/haproxy/maps/meta.map
sudo openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 365 \
  -subj /CN=cstam-default -keyout /tmp/k.pem -out /tmp/c.pem
sudo sh -c 'cat /tmp/c.pem /tmp/k.pem > /etc/haproxy/certs/default.pem; rm /tmp/k.pem /tmp/c.pem; chmod 640 /etc/haproxy/certs/default.pem; chgrp haproxy /etc/haproxy/certs/default.pem'
```

Error pages. `errorfile` needs a **complete raw HTTP response** (status line, headers, blank line, body); `http-request return ... file` needs only the body.

```text
# /etc/haproxy/errors/503-maintenance.http
HTTP/1.1 503 Service Unavailable
Content-Type: text/html; charset=utf-8
Cache-Control: no-store
Retry-After: 10
Connection: close

<!doctype html><html><body style="font-family:sans-serif;text-align:center;margin-top:15%">
<h1>This team's app is restarting</h1><p>The platform is fine. Please retry in a few seconds.</p>
</body></html>
```

`maintenance.html` is the same HTML body without the header lines. `404.html` says "No team app is published at this address."

#### 3.3 The complete `haproxy.cfg` (gw1)

On gw2 only two lines differ: `setenv GW_NAME gw2` and `bind 10.20.0.12:8404`.

```haproxy
global
    log 127.0.0.1:5514 len 8192 format rfc5424 local0 info    # traffic log -> gateway agent (UDP, JSON payload)
    log /dev/log local1 notice                                # HAProxy's own events -> journal
    chroot /var/lib/haproxy
    user haproxy
    group haproxy
    master-worker
    stats socket /run/haproxy/admin.sock mode 660 group haproxy level admin expose-fd listeners
    stats timeout 30s
    hard-stop-after 30s                    # old workers finish in-flight requests after a reload, then exit
    setenv CFG_VERSION v000001             # rendered by the agent: proves which config is live
    setenv GW_NAME gw1
    ssl-default-bind-options ssl-min-ver TLSv1.2 no-tls-tickets
    ssl-default-bind-ciphersuites TLS_AES_128_GCM_SHA256:TLS_AES_256_GCM_SHA384:TLS_CHACHA20_POLY1305_SHA256

defaults
    mode http
    log global
    option dontlognull
    option forwardfor
    option http-keep-alive
    timeout connect 3s
    timeout client 30s
    timeout server 60s
    timeout http-request 10s
    timeout http-keep-alive 5s
    timeout queue 5s
    timeout tunnel 1h
    retries 2
    errorfile 503 /etc/haproxy/errors/503-maintenance.http
    log-format '{"ts":"%tr","gw":"%[env(GW_NAME)]","client":"%ci","host":%{+Q}[capture.req.hdr(0)],"method":"%HM","path":%{+Q}HPO,"be":"%b","srv":"%s","status":%ST,"bytes":%B,"tt":%Tt,"tr":%Tr,"term":"%ts"}'

frontend fe_http
    bind :80
    bind :443 ssl crt /etc/haproxy/certs/default.pem crt-list /etc/haproxy/certs/crt-list.txt alpn h2,http/1.1
    http-request set-var(txn.host) req.hdr(host),field(1,:),lower
    http-request capture var(txn.host) len 128

    # internal endpoints: only localhost, the peer gateway and the control plane
    acl internal src 127.0.0.1 10.20.0.5 10.20.0.11 10.20.0.12
    http-request return status 200 content-type text/plain lf-string "ok %[env(GW_NAME)] %[env(CFG_VERSION)]\n" hdr X-Config-Version "%[env(CFG_VERSION)]" hdr X-Routes-Generation "%[str(generation),map(/etc/haproxy/maps/meta.map,0)]" if { path /__gw/healthz } internal
    http-request deny deny_status 404 if { path_beg /__gw/ }

    # ACME HTTP-01 challenges (TLS bonus) are answered by the control plane
    use_backend be_acme if { path_beg /.well-known/acme-challenge/ }

    # per-team rate limit (bonus): default 500 requests / 10 s per host, override in ratelimit.map
    http-request track-sc0 var(txn.host) table st_host_rate
    http-request set-var(txn.rl) var(txn.host),map_str_int(/etc/haproxy/maps/ratelimit.map,500)
    http-request deny deny_status 429 if { sc_http_req_rate(0),sub(txn.rl) gt 0 }

    http-response set-header X-Served-By %[env(GW_NAME)]
    http-request redirect scheme https code 308 if !{ ssl_fc } { var(txn.host),map(/etc/haproxy/maps/https-only.map) -m found }

    # routing: Host -> slot backend name; unknown host -> 404
    use_backend %[var(txn.host),map(/etc/haproxy/maps/hosts.map,be_404)]
    default_backend be_404

frontend fe_stats
    bind 10.20.0.11:8404                   # gw2: 10.20.0.12:8404 (reachable only from cstam-cp via the SG)
    http-request use-service prometheus-exporter if { path /metrics }
    stats enable
    stats uri /stats
    stats refresh 5s

backend st_host_rate
    stick-table type string len 128 size 10k expire 30s store http_req_rate(10s),bytes_out_rate(10s)

backend be_404
    http-request return status 404 content-type text/html file /etc/haproxy/errors/404.html

backend be_acme
    server acme 10.20.0.5:8402

# canary: always routed; the agent's post-apply check uses it
backend be_slot000
    server s 127.0.0.1:9102 check inter 1s

# team slots 001..128 (rendered). Unused slot: placeholder TEST-NET address, disabled.
backend be_slot001
    http-request return status 503 content-type text/html file /etc/haproxy/errors/maintenance.html hdr Retry-After 10 if { nbsrv() lt 1 }
    server s 192.0.2.1:80 disabled check inter 2s fastinter 500ms downinter 2s fall 2 rise 1
# ... be_slot002 to be_slot128 identical
```

Generate the 128 slots by hand for now (the agent renders them later):

```bash
# on the gateway, append the slots to the config
for i in $(seq -w 1 128); do
  [ "$i" = "001" ] && continue
  printf '\nbackend be_slot%03d\n    http-request return status 503 content-type text/html file /etc/haproxy/errors/maintenance.html hdr Retry-After 10 if { nbsrv() lt 1 }\n    server s 192.0.2.1:80 disabled check inter 2s fastinter 500ms downinter 2s fall 2 rise 1\n' "$((10#$i))"
done | sudo tee -a /etc/haproxy/haproxy.cfg >/dev/null
```

The canary backend points to `127.0.0.1:9102`. Until the agent exists, run a tiny responder so the canary works:

```bash
sudo apt-get install -y python3
nohup python3 -c 'import http.server as h
class H(h.BaseHTTPRequestHandler):
    def do_GET(s): s.send_response(200); s.end_headers(); s.wfile.write(b"canary\n")
h.HTTPServer(("127.0.0.1",9102),H).serve_forever()' >/dev/null 2>&1 &
```

Validate and start:

```bash
sudo haproxy -c -f /etc/haproxy/haproxy.cfg          # "Configuration file is valid"
sudo systemctl restart haproxy
curl -s -H 'Host: canary.gw.internal' localhost      # canary
curl -s localhost/__gw/healthz                       # ok gw1 v000001
curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: nope.hacka.cstam.felcloud.tn' localhost   # 404
```

Line by line, the important parts:

| Line | What it does | Why |
|---|---|---|
| `master-worker` + `expose-fd listeners` | a master process keeps the listening sockets and hands them to new workers on reload | a reload never refuses a connection |
| `hard-stop-after 30s` | old workers get 30 s to finish in-flight requests | no request is cut by a reload |
| `setenv CFG_VERSION` + `/__gw/healthz` | the live config tells which version it is | the agent can verify that the new version really serves |
| `meta.map` + `X-Routes-Generation` | the live routing generation, updated at runtime | the peer gateway and the control plane can check freshness |
| `log ... 127.0.0.1:5514` + JSON `log-format` | every request becomes one JSON line sent to the local agent | live traffic logs for the dashboard |
| `track-sc0` + `st_host_rate` + 429 | counts requests per Host over 10 s | per-team rate limiting (bonus) |
| `use_backend %[...map(hosts.map,be_404)]` | the routing table | unknown hosts get a 404, never a random team |
| `nbsrv() lt 1` in a slot | no healthy server → maintenance page | graceful page when a team app crashes (bonus) |
| `192.0.2.1` | TEST-NET-1 address, never routable | a disabled slot can never send traffic anywhere real |

#### 3.4 Runtime operations: add and remove a team without reloading

Start a test app on host-a as team1. Use the model decided at G1; for now, the simplest is the AAP E2 recipe or a pod-NIC by hand (Step 5). Here we assume team1 answers at `10.20.0.40:80`.

```bash
S=/run/haproxy/admin.sock; M=/etc/haproxy/maps/hosts.map
hap() { echo "$*" | sudo socat stdio "$S"; }

# ADD team1. Order matters: server ready BEFORE the map entry, so no request hits the placeholder
hap "set server be_slot001/s addr 10.20.0.40 port 80"
hap "set server be_slot001/s state ready"
hap "add map $M team1.hacka.cstam.felcloud.tn be_slot001"
echo "team1.hacka.cstam.felcloud.tn be_slot001" | sudo tee -a $M >/dev/null    # keep the file in sync

# TEST (from the gateway itself)
curl -s -H 'Host: team1.hacka.cstam.felcloud.tn' localhost | head -3

# REMOVE team1. Reverse order: unroute first, then cut sessions, then park the slot
hap "del map $M team1.hacka.cstam.felcloud.tn"
hap "set server be_slot001/s state maint"
hap "shutdown sessions server be_slot001/s"
hap "set server be_slot001/s addr 192.0.2.1 port 80"
sudo sed -i '/^team1\.hacka\.cstam\.felcloud\.tn /d' $M

# INSPECT
hap "show map $M"
hap "show servers state be_slot001"
hap "show table st_host_rate"
```

Each command takes well under a millisecond. Do the same on gw2: **both gateways must always have the same routes**, otherwise a failover lands on stale routing.

**Atomic replacement of the whole map** (versioned maps, HAProxy ≥ 2.4). The agent uses it for full resync and rollback:

```bash
hap "prepare map $M"                     # -> "New version created: 7"
printf 'add map @7 %s <<\n%s\n\n' "$M" "$(cat /etc/haproxy/maps/hosts.map)" | sudo socat stdio "$S"
hap "commit map @7 $M"                   # atomic switch; older versions are purged
```

If anything fails between `prepare` and `commit`, the live map is untouched (`clear map @7 $M` drops the draft).

#### 3.5 Keepalived configuration

Timing first. The backup declares the master dead after **Master_Down_Interval = 3 × advert_int + Skew**, with **Skew = (256 − priority) × advert_int / 256**:

| advert_int | backup priority 100 | Comment |
|---|---|---|
| 1 s | 3 + 0.61 = 3.6 s | safe baseline |
| **0.5 s (VRRPv3)** | **1.5 + 0.30 = 1.8 s** | **chosen** |
| 0.2 s | 0.6 + 0.12 = 0.72 s | sub-second on a hard crash, but a 1-vCPU VM under load can miss adverts and fail over for nothing |

These times apply only to *silent* failures (kernel panic, frozen VM, network cut). A clean `systemctl stop keepalived` sends a priority-0 advert, and HAProxy death is caught instantly by `vrrp_track_process` (netlink process events, no polling): both fail over in about 0.1–0.5 s. Add the OVN rebind time (tens to hundreds of ms after the GARP).

`/etc/keepalived/keepalived.conf` on **gw1**:

```conf
global_defs {
    router_id cstam-gw1
    vrrp_version 3
    enable_script_security
    script_user keepalived_script
    vrrp_garp_master_delay 1          # second GARP burst 1 s after becoming master
    vrrp_garp_master_repeat 3         # 3 GARPs per burst
    vrrp_garp_master_refresh 10       # re-GARP every 10 s while master (heals a lost OVN binding)
    vrrp_garp_master_refresh_repeat 1
    vrrp_higher_prio_send_advert true
}

# end-to-end check: the agent says "ready" AND HAProxy answers through its real frontend
vrrp_script chk_gateway {
    script "/usr/local/libexec/cstam/chk_gateway"
    interval 1
    timeout 1
    fall 2
    rise 2
    init_fail
}

# instant: is a haproxy process alive?
vrrp_track_process haproxy_proc {
    process haproxy
    quorum 1
    delay 0
}

# manual failover button: the agent writes 1 into this file -> FAULT -> the peer takes the VIP
vrrp_track_file manual_failover {
    file /run/cstam/force-backup
    init_file 0 overwrite
}

vrrp_instance VI_GW {
    state BACKUP                      # both BACKUP + nopreempt: whoever is master keeps the VIP
    nopreempt
    interface ens3                    # check: ip -o -4 route show default | awk '{print $5}'
    virtual_router_id 51
    priority 150                      # gw2: 100
    advert_int 0.5
    unicast_src_ip 10.20.0.11         # gw2: 10.20.0.12
    unicast_peer {
        10.20.0.12                    # gw2: 10.20.0.11
    }
    virtual_ipaddress {
        10.20.0.10/32 dev ens3 label ens3:vip
    }
    track_interface {
        ens3
    }
    track_script {
        chk_gateway
    }
    track_process {
        haproxy_proc weight 0
    }
    track_file {
        manual_failover weight 0
    }
    notify /usr/local/libexec/cstam/vrrp-notify root
}
```

**gw2 differs in exactly four lines:** `router_id cstam-gw2`, `priority 100`, `unicast_src_ip 10.20.0.12`, `unicast_peer { 10.20.0.11 }`.

`weight 0` on a tracked item means "if it fails, go to FAULT state" (not "lower my priority"). A node in FAULT stops advertising, so the peer takes over at once.

The VIP is a `/32` on purpose: with a `/32`, the kernel never picks the VIP as the source address for HAProxy's connections to teams, so teams see the gateway's own IP (.11 or .12), which is what `cstam-team`'s `--remote-group cstam-gw` expects.

The check script (`root:root`, mode `0755`; `enable_script_security` refuses scripts writable by others):

```sh
#!/bin/sh
# /usr/local/libexec/cstam/chk_gateway
# ready = the gateway agent has synced its routes to the current generation since boot (Step 4)
[ -f /run/cstam/ready ] || exit 1
exec curl -fsS -o /dev/null --max-time 0.8 http://127.0.0.1/__gw/healthz
```

Until the agent exists (Step 4), create the file by hand: `sudo touch /run/cstam/ready`.

The notify script. Keepalived calls it with `$1=INSTANCE $2=VI_GW $3=STATE $4=PRIORITY` on every state change:

```sh
#!/bin/sh
# /usr/local/libexec/cstam/vrrp-notify   (root:root 0755)
STATE="$3"; TS="$(date +%s.%N)"
printf '%s %s %s\n' "$STATE" "$TS" "$4" > /run/cstam/vrrp.state.tmp && mv /run/cstam/vrrp.state.tmp /run/cstam/vrrp.state
logger -t vrrp-notify "state=$STATE prio=$4 ts=$TS"
if [ "$STATE" = "MASTER" ]; then
  arping -U -c 2 -I ens3 10.20.0.10 >/dev/null 2>&1 &     # one extra GARP burst for OVN
fi
exit 0
```

The gateway agent watches `/run/cstam/vrrp.state` (file notification) and reports every transition, with its timestamp, to the control plane. The dashboard shows "failover 1.4 s" from these timestamps.

```bash
sudo install -d -o cstam -g haproxy -m 0775 /run/cstam
echo 0 | sudo tee /run/cstam/force-backup
sudo keepalived -t -f /etc/keepalived/keepalived.conf && sudo systemctl restart keepalived
journalctl -u keepalived -n 20 --no-pager
```

::: note
**`/run` is emptied at boot.** Add a `tmpfiles.d` entry so `/run/cstam` exists after a reboot: `echo 'd /run/cstam 0775 cstam haproxy -' | sudo tee /etc/tmpfiles.d/cstam.conf`. The agent recreates `force-backup` and removes `ready` at start.
:::

#### 3.6 preempt or nopreempt

| | preempt (default) | **nopreempt (chosen)** |
|---|---|---|
| gw1 comes back after a crash | takes the VIP back → **a second short outage** | stays backup → only one outage |
| Which node is master | always gw1 when healthy | "whoever survived"; the dashboard shows it |
| Manual failover | lower the priority | `track_file` → FAULT (works with nopreempt) |

`nopreempt` only works when both nodes start in `state BACKUP`, which is why both configs say `BACKUP`.

#### 3.7 Manual failover (the dashboard button)

```bash
# on the current master
echo 1 | sudo tee /run/cstam/force-backup    # -> FAULT: the peer becomes MASTER within ~0.5 s
# later, to make it eligible again (it stays BACKUP because of nopreempt):
echo 0 | sudo tee /run/cstam/force-backup
```

No process is killed, so this "planned" failover loses at most the in-flight requests on the old master.

#### 3.8 Split brain

Split brain = both nodes believe they are master (adverts lost both ways). On OVN, both send GARPs and the VIP binding follows whichever sent the last one: traffic flaps.

Mitigations, in order:

1. The `cstam-vrrp` rule (proto 112 between members) must be right. Check on the backup: `sudo tcpdump -n -c 4 'ip proto 112'` shows one packet every 0.5 s.
2. `track_interface ens3`: a node whose NIC goes down enters FAULT.
3. Both agents report their VRRP state. If the control plane sees **two MASTERs for more than 2 s**, it sets `force_backup=true` in the desired state of the node with the lower priority; that agent writes `1` into `/run/cstam/force-backup`. Implemented in Step 4 (agent) and Step 7 (control plane).
4. Optional: add to `chk_gateway` a test that the router answers (`arping -c1 -w1 -I ens3 10.20.0.1`); a node cut off from the router stops claiming the VIP.

Both gateways always carry the same acknowledged routes (Step 4), so even during a flap no request goes to a wrong team; it only goes through a different gateway.

#### 3.9 Fallback F1: move the floating IP from the notify hook

Only if Gate G1 showed that the floating IP does not follow the VIP. The design:

- On `MASTER`, the gateway agent calls Neutron: `PUT /v2.0/floatingips/{id}` with `{"port_id": "<own port id>", "fixed_ip_address": "10.20.0.11"}` (gw2: its own port and .12). Expected failover 1.5–4 s.
- The gateway gets an **application credential restricted by access rules** to exactly that API path and the one floating IP. It is the only OpenStack credential outside the control plane.
- In Go: `floatingips.Update(ctx, nc, fipID, floatingips.UpdateOpts{PortID: &ownPortID, FixedIP: ownIP})` from `github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips`, in package `internal/gw/fipmove` (the only gateway package allowed to import gophercloud by the depguard rule).
- In split brain, the last writer wins; both gateways have the same routes, so it stays correct.

```bash
# access rules file for the F1 credential (you create it; the gateway receives it at enrolment)
cat > /tmp/fip-rules.json <<'EOF'
[{"service":"network","method":"PUT","path":"/v2.0/floatingips/<FIP_ID>"},
 {"service":"network","method":"GET","path":"/v2.0/floatingips/<FIP_ID>"}]
EOF
openstack application credential create cstam-gw-fipmove --role member \
  --expiration 2026-12-31T00:00:00 --access-rules /tmp/fip-rules.json
```

#### 3.10 Measure the failover on the real gateways

Repeat the Step 1 measurement with the production configs, through the public FIP and a real team host:

```bash
go run ./bench/failover-probe -url http://$PUBLIC_FIP/ -host team1.hacka.cstam.felcloud.tn > /tmp/f-haproxy.log
ssh cstam-gw1 'sudo systemctl kill -s KILL haproxy'           # injection 2; then restart haproxy
bash bench/failover-probe/gap.sh /tmp/f-haproxy.log
```

Do the five injections of Step 1 (5 runs each) and keep the logs in `bench/results/step3/`. These are the first numbers of the benchmark report.

### Build it in Go

Nothing in Go yet: you just wrote, by hand, exactly what the agent will automate. Commit the files you wrote into `deploy/haproxy/` (as Go `text/template` files later) and `deploy/keepalived/`, plus the two scripts into `deploy/keepalived/libexec/`.

### Done when

::: done
**Done when**

- `sudo haproxy -c -f /etc/haproxy/haproxy.cfg` prints `Configuration file is valid` on both gateways.
- From the laptop: `curl -s -H 'Host: team1.hacka.cstam.felcloud.tn' http://$PUBLIC_FIP/` reaches team1, and the `X-Served-By` header shows the master.
- `curl -H 'Host: nobody.hacka.cstam.felcloud.tn' http://$PUBLIC_FIP/` returns 404.
- Adding and removing team1 with runtime commands works on both gateways, and `systemctl reload haproxy` afterwards does not change the routing (files and memory agree).
- `echo 1 > /run/cstam/force-backup` on the master moves the VIP to the peer; the probe shows < 1 s of errors.
- Killing HAProxy on the master: failover < 1 s (median of 5). Kernel panic: ~1.8 s median, < 3 s max. Numbers saved in `bench/results/step3/` and `docs/notebook/experiments.md`.
- Bringing the old master back causes 0 errors (nopreempt).
:::

### Pitfalls

::: warn
**Map file and memory drift apart.** `add map` changes only memory. If you forget to write the file, the next reload silently removes the route. The agent always writes the file first; by hand, do both.
:::

::: warn
**`chk_gateway` is "init_fail".** Keepalived starts with the check failed, so after a reboot a gateway never becomes master until `/run/cstam/ready` exists. That is intended (a stale gateway must not take the VIP), but remember to create the file by hand until the agent does it.
:::

::: warn
**Interface name.** If the VM's interface is not `ens3`, keepalived logs "Cannot find interface" and stays in FAULT. Always render the name from `ip -o -4 route show default`.
:::

::: warn
**Both gateways must stay identical.** Any manual change on one gateway only is a future outage: after a failover, the other gateway serves different routes. From Step 4 on, never edit gateway files by hand.
:::

### Learning checkpoint

::: learn
**Answer these yourself in docs/notebook/learning.md**

1. Why does adding a team never need a reload with slot backends? What is the one change that still needs a reload?
2. In which order do you add a team at runtime, and why that order? Same question for removal.
3. Compute the master-down interval for advert_int 0.5 and priority 100. Why is HAProxy death much faster than that?
4. What would happen during a failover if gw2 had a map without team7's route?
5. Why is the VIP configured as a /32?
:::

### Driving Claude Code for this step

::: claude
**Prompts to use**

- "Explain this haproxy.cfg line by line, especially master-worker, expose-fd listeners, the map lookup and the slot backends. Then list three ways a typo here could route a request to the wrong team."
- "Turn deploy/haproxy/haproxy.cfg into a Go text/template with: gateway name, own IP, config version, slot count. Add a golden-file test that renders 128 slots and runs `haproxy -c` in a container."

**Review:** gw1 and gw2 differ only in the expected lines (`diff` the two rendered files); no slot points to a real IP when unused.
:::
