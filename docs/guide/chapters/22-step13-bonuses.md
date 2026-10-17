# 22. Step 13 – Bonus features

The brief lists five optional features, worth +2 points each. They are not all equal. Some take a few hours because the architecture already did the hard work; others depend on things FelCloud must give you (DNS). Do them in this order, which is points per hour of effort:

| Rank | Bonus | Effort (estimate) | Why this rank |
|---|---|---|---|
| 1 | Smart health monitoring & fallback (maintenance page) | 3–4 h | Slot backends already have health checks; you add one rule and an HTML page |
| 2 | Rate limiting & bandwidth throttling | 4–6 h | Stick tables and `bwlim` are HAProxy features; per-team values come from the API |
| 3 | OpenStack native orchestration (Heat) | ~1 day | Optional VM-per-team driver: `cpd` creates one Heat stack per premium team and reuses the IPAM (the platform itself stays in Pulumi) |
| 4 | Automated SSL/TLS | 0.5–1.5 days | Depends on DNS access from FelCloud (ask on day 1) |
| 5 | Custom domain binding | 0.5 day after TLS | Needs verification and certificates, so it comes last |

::: note
**Goal:** bonuses 1–3 done and measured; 4–5 done if DNS access arrived · **Points served:** up to +10 · **Time estimate:** 3–5 days total · **Depends on:** Gate G2; do not start bonuses before every scored item works (Gate G3 in the plan)
:::

::: warn
**Bonuses never come before scored items.** A +2 bonus does not compensate for a failed 15-point stress test. Start this step only when Gate G3 ("every scored item is demoable") has passed, unless a bonus is almost free (the maintenance page often is).
:::

## 13.1 Smart health monitoring and fallback (maintenance page)

### Understand first

Each team app has its own slot backend in HAProxy (`be_slot001` … `be_slot128`) with exactly one server: the team's IP and port. HAProxy checks that server every 2 s. When the check fails twice (`fall 2`), the server is marked DOWN. At that moment the backend has zero usable servers (`nbsrv() = 0`). Instead of returning a raw 503 error, the backend returns a friendly page: "This team's app is restarting, retrying in 10 seconds", with a `Retry-After` header.

The same signal goes to the dashboard: the gateway agent reads server states from the runtime API every second and reports them, so the team row shows "app down → maintenance page" in real time.

### Do it

The slot template (already in Step 3) contains the rule. Check it is there:

```haproxy
backend be_slot001
    option httpchk
    http-check send meth GET uri /.cstam/health ver HTTP/1.1 hdr Host health.local
    http-check expect status 200-499
    http-request return status 503 content-type text/html file /etc/haproxy/errors/maintenance.html hdr Retry-After 10 if { nbsrv() lt 1 }
    server s 10.20.0.32:80 check inter 2s fastinter 500ms downinter 2s fall 2 rise 1
```

::: why
**Why `expect status 200-499`?** The health check asks "is something listening and answering HTTP?", not "is the team's app perfect?". A team app that returns 404 for `/.cstam/health` is alive. Only connection errors, timeouts and 5xx count as down. If you used `expect status 200`, most team apps would be marked down forever because they do not implement a health endpoint.
:::

`/etc/haproxy/errors/maintenance.html` (deployed by the gateway agent with the rest of the config):

```html
<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="refresh" content="10">
<title>Restarting</title><style>body{font-family:sans-serif;max-width:40em;margin:4em auto;color:#333}</style></head>
<body><h1>This app is restarting</h1>
<p>The team's application is not answering right now. The platform is watching it and this page will reload by itself.</p>
<p><small>CSTAM × FelCloud</small></p></body></html>
```

In the gateway agent, add a 1 s loop that runs `show servers state` (one command for all backends), compares with the previous result, and sends changes to cpd in the heartbeat (`slot_health` field). cpd publishes `team.health` events; the dashboard shows them.

### Done when

::: done
**Done when**

```bash
ssh host-a 'docker kill team1-web'
curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: team1.hacka.cstam.felcloud.tn' http://$FIP/   # within ~2-4 s: 503
curl -s -H 'Host: team1.hacka.cstam.felcloud.tn' http://$FIP/ | grep -c "restarting"            # 1
# Docker restart policy brings the container back -> after rise 1 (~2 s) -> 200 again
```

The dashboard team row shows DOWN → maintenance page → healthy, with times. Record "time to maintenance page" and "time to recover" in the benchmark report (chaos CH-08).
:::

## 13.2 Rate limiting and bandwidth throttling

### Understand first

Two different things:

- **Request rate limiting** protects the gateway and other teams from one team's flood (or from an attacker flooding one team's URL). HAProxy keeps a **stick table**: an in-memory table keyed by the Host header, storing the request rate over a sliding 10 s window. When the rate exceeds the team's limit, HAProxy answers 429 without touching the backend.
- **Bandwidth throttling** caps bytes per second. Downloads (gateway → client) are capped in HAProxy with the `bwlim-out` filter. Uploads from a team's containers to the internet are capped on the team's NIC inside its namespace with Linux traffic control (`tc`).

Per-team values live in the database (`teams.rate_limit_rps`, `teams.bw_out_kbps`) and reach the gateways as two extra maps in the same generation-tracked desired state as routes. So a limit change is just a route-like change: runtime map update, no reload.

### Do it: request rate limit

Already in the frontend from Step 3:

```haproxy
frontend fe_http
    http-request track-sc0 var(txn.host) table st_host_rate
    http-request set-var(txn.rl) var(txn.host),map_str_int(/etc/haproxy/maps/ratelimit.map,500)
    http-request deny deny_status 429 if { sc_http_req_rate(0),sub(txn.rl) gt 0 }

backend st_host_rate
    stick-table type string len 128 size 10k expire 30s store http_req_rate(10s)
```

Read it as: "count requests per host over 10 s; the allowed number is the value from `ratelimit.map` for this host, or 500 if absent; deny with 429 when the count minus the allowed number is positive". `ratelimit.map` contains lines like `team1.hacka.cstam.felcloud.tn 1000` (requests per 10 s).

### Do it: download bandwidth

In each slot backend (rendered by the template):

```haproxy
backend be_slot001
    filter bwlim-out lim_out default-limit 2m default-period 1s
    http-response set-var(txn.bw) var(txn.host),map_str_int(/etc/haproxy/maps/bandwidth.map,2097152)
    http-response set-bandwidth-limit lim_out limit var(txn.bw) period 1s
    # ... health check, maintenance rule and server line as before
```

`bwlim-out` exists since HAProxy 2.7, so it works with the 3.2 LTS we install (and with the distro 2.8). The default is 2 MB/s per stream; `bandwidth.map` overrides it per host, in bytes per period.

### Do it: upload bandwidth from the team

The host agent adds a token bucket filter on the team's NIC, inside its namespace, in `Ensure()`:

```bash
ip netns exec team1 tc qdisc replace dev eth0 root tbf rate 20mbit burst 64kb latency 50ms
ip netns exec team1 tc -s qdisc show dev eth0        # verify, shows sent bytes and drops
```

A root qdisc on the team's NIC shapes what the team **sends** (uploads, outbound requests). Combined with the HAProxy limit on what clients download, both directions are covered. (Neutron QoS policies on the team port would be the OpenStack-native way to do the same, and FelCloud lists the `qos` and `qos-bw-limit-direction` extensions; creating QoS policies may need extra permissions. Mention it as an alternative in the report.)

### Done when

::: done
**Done when** (load scenario L-03 from Step 11)

```bash
# team1 limited to 100 req/s (1000 per 10 s), team2 default
./bin/loadgen --url http://10.20.0.10/ --host team1.hacka.cstam.felcloud.tn --rps 2000 --dur 60s --out l03-team1.csv &
./bin/loadgen --url http://10.20.0.10/ --host team2.hacka.cstam.felcloud.tn --rps 50   --dur 60s --out l03-team2.csv
# team1: most responses 429, accepted rate ~100/s; team2: 100 % 200, p99 within +10 % of baseline
curl -s -o /dev/null -w '%{speed_download}\n' -H 'Host: team1.hacka.cstam.felcloud.tn' http://10.20.0.10/big.bin
# ~ 2 MB/s (2097152 bytes/s), or the per-team value
```
:::

## 13.3 OpenStack native orchestration (Heat)

### Understand first

Heat is OpenStack's orchestration service. You describe resources (networks, ports, servers, security groups) in a YAML template; Heat creates them in the right order, tracks them as a **stack**, updates them when the template changes, and deletes everything with one command. FelCloud runs Heat (catalog checked), but the CLI plugin is not installed on your laptop: `pip install python-heatclient`.

How we use it:

1. **VM-per-team sandbox driver**: a small stack per "premium" team (one port with the IPAM's fixed IP, one 1c1g server, cloud-init that runs the team's container). It answers the brief's "VMs/containers" literally and reuses the IPAM, the gateways and the teardown order unchanged. Only possible within the 10-instance quota, so it is a demo of one or two teams, not a mode for 10+.

::: why
**Why the platform is not ported to Heat.** The permanent infrastructure is already code (Pulumi in Go, Step 2). Porting it to a HOT template would only duplicate it, and two tools managing the same `cstam-vip` port would fight each other. The bonus is earned where Heat adds something real: **`cpd` itself drives Heat** to create one stack per premium team. If you want to show HOT syntax anyway, `infra/heat/` can hold an equivalent template used only in a separate test project, never against the `prod` stack.
:::

### Do it: VM-per-team driver

```yaml
# infra/heat/team-vm.yaml (one stack per premium team, created by cpd through the Heat API)
heat_template_version: 2021-04-16
parameters:
  team_id:  {type: string}
  team_ip:  {type: string}          # from our IPAM, inside 10.20.0.32-191
  image:    {type: string}          # the team's container image, run by cloud-init
resources:
  port:
    type: OS::Neutron::Port
    properties:
      name: {list_join: ['-', ['cstam-team', {get_param: team_id}]]}
      network: cstam-sandbox-net
      fixed_ips: [{subnet: cstam-sandbox-subnet, ip_address: {get_param: team_ip}}]
      security_groups: [cstam-team]
  server:
    type: OS::Nova::Server
    properties:
      name: {list_join: ['-', ['cstam-vm', {get_param: team_id}]]}
      flavor: G0.basic.1c1g
      image: "Ubuntu 24.04 LTS - Noble Numbat"
      networks: [{port: {get_resource: port}}]
      user_data_format: RAW
      user_data:
        str_replace:
          template: |
            #cloud-config
            packages: [docker.io]
            runcmd:
              - docker run -d --restart unless-stopped -p 80:80 $IMAGE
          params: {$IMAGE: {get_param: image}}
```

In Go, this is a second implementation of the `SandboxDriver` interface. Its `Ensure` creates the stack (gophercloud `openstack/orchestration/v1/stacks`), its `Done` checks `stack_status = CREATE_COMPLETE` and the port ACTIVE, its `Remove` deletes the stack. The IPAM, the route, the generation acks and the teardown order are the same as for containers. Expect 40–90 s to ACTIVE (one VM boot), which is why it is a premium mode, not the default.

### Done when

::: done
**Done when**

- One premium team created with the VM driver gets a Heat stack `cstam-team-<id>`; `openstack stack show cstam-team-<id>` is `CREATE_COMPLETE` and the team answers through the gateways.
- Deleting the team deletes its stack (`openstack stack list` no longer shows it), and its IP follows the normal quarantine.
- `pulumi preview` on the platform still shows no changes: the driver never touches resources owned by the Pulumi stack.
:::

## 13.4 Automated SSL/TLS

### Understand first: the two traps

**Trap 1: wildcard depth.** A certificate for `*.cstam.felcloud.tn` covers `hacka.cstam.felcloud.tn` but **not** `team5.hacka.cstam.felcloud.tn`. TLS wildcards match exactly one label. You need **one wildcard certificate per hackathon**: `*.hacka.cstam.felcloud.tn`.

**Trap 2: Let's Encrypt limits are shared.** Let's Encrypt allows 50 certificates per registered domain per week. The registered domain is `felcloud.tn`, which FelCloud uses for its own customers. Issuing one certificate per team (HTTP-01 for each hostname) during your tests could block FelCloud's real customers. That would be "disrupting existing infrastructure". So: **always use the Let's Encrypt staging endpoint until the final demo**, and prefer one wildcard per hackathon.

### The options

| Option | Needs from FelCloud | Certificates | Verdict |
|---|---|---|---|
| **A. DNS-01 with acme-dns via one CNAME** | one static record: `_acme-challenge.hacka.cstam.felcloud.tn CNAME <uuid>.auth.<our-acme-dns-host>` | 1 wildcard per hackathon | **Best** if they agree (ask on day 1) |
| B. DNS-01 with an API token for their DNS provider | an API token (more trust than A) | 1 wildcard per hackathon | Good, but more to ask |
| C. HTTP-01 per hostname | only the wildcard A record (already needed) | 1 per team | Works without help; mind the rate limit; staging until demo |
| D. Self-signed wildcard from our own CA | nothing | 1 per hackathon | Demo fallback; import the CA into the demo browser |

Wildcard certificates **require** DNS-01. That is why A or B is needed for the clean solution.

### Do it (option A, with C as fallback)

1. Run `acme-dns` (a small DNS server for ACME challenges) on the control plane VM. Add a rule to `cstam-cp` for UDP and TCP 53 from anywhere, and give the control plane its own floating IP (it has one for SSH).
2. Register an account with acme-dns: it returns a username, password and a subdomain `<uuid>.auth.<host>`. Ask FelCloud to add the CNAME above.
3. In cpd, a cert manager uses `github.com/go-acme/lego/v4` with the `acmedns` provider:

```go
// internal/certs/manager.go (core)
func (m *Manager) Issue(ctx context.Context, hackSlug string) (*certificate.Resource, error) {
	user := &acmeUser{email: m.cfg.Email, key: m.accountKey}
	cfg := lego.NewConfig(user)
	cfg.CADirURL = m.cfg.DirectoryURL // lego.LEDirectoryStaging until the final demo!
	cfg.Certificate.KeyType = certcrypto.EC256
	client, err := lego.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	prov, err := acmedns.NewDNSProviderConfig(m.acmeDNSConfig()) // server URL + stored account
	if err != nil {
		return nil, err
	}
	if err := client.Challenge.SetDNS01Provider(prov); err != nil {
		return nil, err
	}
	if user.reg == nil {
		if user.reg, err = client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true}); err != nil {
			return nil, err
		}
	}
	return client.Certificate.Obtain(certificate.ObtainRequest{
		Domains: []string{"*." + hackSlug + "." + m.cfg.BaseDomain},
		Bundle:  true,
	})
}
```

4. The certificate (PEM: certificate + chain + key) is stored encrypted in the database and becomes part of the gateways' desired state, exactly like routes. Each gateway agent writes it to `/etc/haproxy/certs/hacka.pem` and updates HAProxy **without a reload**:

```bash
# what the agent does (runtime API), for a new certificate file
echo "new ssl cert /etc/haproxy/certs/hacka.pem" | socat stdio /run/haproxy/admin.sock
printf 'set ssl cert /etc/haproxy/certs/hacka.pem <<\n%s\n\n' "$(cat hacka.pem)" | socat stdio /run/haproxy/admin.sock
echo "commit ssl cert /etc/haproxy/certs/hacka.pem" | socat stdio /run/haproxy/admin.sock
echo "add ssl crt-list /etc/haproxy/certs/crt-list.txt /etc/haproxy/certs/hacka.pem *.hacka.cstam.felcloud.tn" \
  | socat stdio /run/haproxy/admin.sock
# and the same lines are written to the files on disk, so a reload reproduces memory
```

5. Renewal: the cert manager checks every 12 h and renews 30 days before expiry. The dashboard shows certificate expiry per hackathon.

Fallback C (no DNS help): HAProxy already sends `/.well-known/acme-challenge/` to `be_acme` (the control plane). lego's HTTP-01 provider serves the token there. Issue one certificate per team hostname at team creation, staging endpoint until the demo.

### Done when

::: done
**Done when**

```bash
curl -sv https://team1.hacka.cstam.felcloud.tn/ 2>&1 | grep -E "subject:|issuer:|SSL certificate verify"
# subject: CN=*.hacka.cstam.felcloud.tn ; issuer: (STAGING) ... or Let's Encrypt R1x for production
openssl s_client -connect $FIP:443 -servername team1.hacka.cstam.felcloud.tn </dev/null 2>/dev/null \
  | openssl x509 -noout -enddate
```

A certificate update during a `loadgen` run over HTTPS produces 0 failed requests (runtime update, no reload).
:::

## 13.5 Custom domain binding

### Understand first

A team wants `shop.example.org` to show its app. Three things must happen: prove the team controls the domain (otherwise anyone could hijack anyone's domain), route the domain to the team's slot, and give it a certificate.

### Do it

1. API: `POST /v1/teams/{id}/domains {"domain":"shop.example.org"}` → returns a verification token and two instructions:
   - `shop.example.org CNAME team1.hacka.cstam.felcloud.tn` (traffic goes to our floating IP);
   - `_cstam-verify.shop.example.org TXT "cstam-verify=<token>"` (ownership proof).
2. A controller step checks both with a DNS lookup (`net.Resolver` with a public resolver such as 1.1.1.1, not the VM's default) every 30 s, up to 24 h. State: `PENDING_VERIFICATION → VERIFIED`.
3. Once verified: insert a `team_domains` row. The partial unique index `(domain) WHERE active` prevents two teams from claiming the same domain (edge case 22). Add a route entry (same slot as the team's main app) in the next generation.
4. Certificate: HTTP-01 for that exact name (wildcards are impossible for a domain you do not control), staging first, then production. Add it with `add ssl crt-list … <path> shop.example.org`.
5. Teardown: the custom domain route is removed in the same generation as the team's other routes, so the edges.md safety argument covers it too.

### Done when

::: done
**Done when**

- A domain without the TXT record stays `PENDING_VERIFICATION` and is never routed.
- With both records, `curl -s https://shop.example.org/` reaches team1's app with a valid certificate.
- A second team asking for the same domain gets 409.
- Deleting team1 removes the custom domain route in the same generation (both gateways acked) before the IP is quarantined.
:::

## Pitfalls

::: warn
**Rate limits on the wrong key.** If you key the stick table on the client IP instead of the Host header, every visitor behind the same university NAT shares one limit, and the jury on conference Wi-Fi may get 429s in your demo. Key on the host (per team), and optionally add a second, much higher, per-client-IP limit against abuse.
:::

::: warn
**Using the production Let's Encrypt endpoint while testing.** Five failed validations per hour and 50 certificates per week per registered domain. Switch the directory URL to production once, the day before the final demo, and only for the wildcard.
:::

::: warn
**Heat stacks and manual changes.** If you change a resource by hand (for example add an SG rule with the CLI), the next `stack update` may revert it or fail. After you move to Heat, all infrastructure changes go through the template.
:::

## Learning checkpoint

::: learn
**Answer these yourself before moving on**

1. Why does `*.cstam.felcloud.tn` not cover `team5.hacka.cstam.felcloud.tn`, while the DNS wildcard does?
2. Why do wildcard certificates require the DNS-01 challenge?
3. What is in a stick table entry, and when does it expire?
4. In which direction does a root `tbf` qdisc on the team's NIC limit traffic, and which component limits the other direction?
5. Why is the custom domain ownership check done with a TXT record and not only with the CNAME?
:::

## Driving Claude Code for this step

::: claude
**How to use Claude Code here**

- Bonuses are well-bounded tasks, good for the agent: *"Add per-team rate limit and bandwidth to the gateway desired state: new fields in the proto, two extra maps rendered by the agent, runtime update with prepare/commit map, and a unit test with golden files for the rendered maps. Follow section 13.2. Plan first."*
- For Heat: *"Write infra/heat/team-vm.yaml and the cpd driver that creates one stack per premium team with gophercloud orchestration/v1/stacks. Explain each resource and each depends_on. Do not run any openstack command; give me the validate command."*
- For TLS: ask the agent to write the cert manager against the **staging** directory only, with the directory URL coming from configuration, and a test that fails if the production URL is the default.
:::
