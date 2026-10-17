# 18. Step 9 – Security and isolation

::: note
**Goal** · A team can reach only its own sandbox; nobody can reach the management layer; every control is proven by an automated attack test whose report goes into the security documentation.
**Points served** · Network isolation and access safeguards (5), Automated teardown and lifecycle security (10, together with Step 8), the "security architecture documentation" deliverable, and many jury questions.
**Time estimate** · 10–14 hours (rules 3 h, hardening 2 h, PKI and auth 4 h, attack suite 3 h).
**Depends on** · Step 2 (security groups exist), Step 5 (host agent), Step 6 (audit table), Step 7 (agent protocol). Most of this step can be built in parallel with Step 8.
:::

Security in this project is not one feature. It is a set of **independent layers**, each of which stops some attack paths on its own. If one layer has a bug, another one still holds. This is called *defence in depth*. This chapter first explains what we protect and from whom (the threat model), then builds the five layers one by one, and finishes with `cstam-sectest`, the tool that attacks our own platform and proves that every layer works.

![Figure 18.1 – Defence in depth: five layers, the attackers on the left, the assets on the right; every attack path ends on the layer that stops it and has a test ID](diagrams/f14-defence.png)

## 9.1 Understand first: the threat model

A threat model answers four questions: what are we protecting (assets), from whom (actors), where does trust change (trust boundaries), and how could an attacker move (attack paths). We use a light version of STRIDE: **S**poofing, **T**ampering, **R**epudiation (denying you did something), **I**nformation disclosure, **D**enial of service, **E**levation of privilege.

### 9.1.1 Assets, most valuable first

| # | Asset | Why it matters | Where it lives |
|---|---|---|---|
| A1 | OpenStack application credential | It can create and delete ports and change address pairs, so it can hijack any IP of the project | Control plane VM only |
| A2 | Routing state (HAProxy maps, slot addresses, config versions) | A wrong route means one team's URL serves another team's app (the `edges.md` bug) | Gateways, `config_versions` table |
| A3 | IPAM state (leases, quarantine, generations) | A double allocation is an IP conflict and a misroute | PostgreSQL on the control plane |
| A4 | Gateway availability (VIP, keepalived, HAProxy) | The only path every team depends on | gw1, gw2 |
| A5 | Team workloads and data | Confidentiality between teams | Hackathon hosts, one netns per team |
| A6 | Agent identities and the internal CA key | Whoever holds a valid agent certificate can report fake state or pull desired state | `ca.key` offline, leaf keys on each node |
| A7 | Admin accounts, sessions, API tokens | Full control through the dashboard | Control plane DB (hashed) |
| A8 | Host OS integrity | A host compromise exposes all teams on that host | Hackathon hosts |
| A9 | Audit log | Accountability, incident analysis, evidence for the jury | Control plane DB (append-only, hash chain) |

### 9.1.2 Actors

| Actor | What we assume they can do | What they want |
|---|---|---|
| **Malicious team** | Full control of the code inside its own containers (it deploys any image through the API). No SSH, no Docker socket, no host access. | Read or attack other teams, reach the platform, escape, starve resources, take over another team's subdomain |
| **Compromised container** | The same as a malicious team, but hidden inside a normal team (a bad npm dependency, a remote code execution bug in a team app) | Pivot to the metadata service, management IPs, the host |
| **External attacker** | Can reach the floating IP from the Internet, send any `Host` header, flood | Take the gateway down, reach admin surfaces, abuse unknown subdomains |
| **Curious or careless admin** | Has a valid dashboard login | Look at data they should not, make a change and deny it later, push a broken config |
| **Our own bugs** | Races in IPAM, wrong teardown order | Misrouting, leaked resources |

### 9.1.3 Trust boundaries

A trust boundary is a line where data or traffic goes from a less trusted zone to a more trusted one. Every boundary needs a control.

```text
Internet ──[TB1: floating IP -> HAProxy :80/:443 only]──────────────> gateways
gateways ──[TB2: Neutron SG cstam-team: tcp 80, 8000-8099 from cstam-gw]──> team NICs
team containers ──[TB3: netns + nft + Neutron port security + cgroups/caps]──> host, other teams, subnet
agents ──[TB4: mTLS, agents dial OUT to cpd :8443, SG cstam-cp]──> control plane
control plane ──[TB5: restricted application credential]──> OpenStack APIs
admin browser ──[TB6: TLS + session cookie + roles + audit]──> control plane API
```

TB3 is the most important and the weakest boundary, because containers share the host kernel. That is why it gets the most layers and the most tests.

::: why
**Why the pod-NIC model makes TB3 stronger than v1**
In the v1 model (AAP fallback), team traffic crosses the host's root network namespace and only host iptables separate teams. In the pod-NIC model, each team's traffic leaves through its **own Neutron port**. OpenStack itself (OVN port security and the `cstam-team` security group) drops team-to-team packets *outside the VM*. A team that somehow got root inside the host VM still could not change OVN rules. Host firewalls become a second layer instead of the only one.
:::

### 9.1.4 Threat table

Layer names match Figure 18.1: **L1** gateway edge, **L2** Neutron security groups and port security, **L3** host and netns firewall, **L4** container hardening, **L5** control plane. Test IDs refer to section 9.9.

| ID | STRIDE | Actor | Attack path | Stopped by | Test |
|---|---|---|---|---|---|
| T01 | I | Malicious team | team1 → team2's team IP 10.20.0.33 (same host) | L2 (`cstam-team` has no rule for `cstam-team` sources), L3 (pod nft output drops 10.20.0.0/24) | S01 |
| T02 | I | Malicious team | team1 → team on the other host | L2 + L3 | S02 |
| T03 | E/I | Compromised container | → host primary IP 10.20.0.21 (sshd) | L3 pod output drop 10.20.0.0/24; L2 `cstam-host` only from `cstam-cp`; host nft input only from cp | S03 |
| T04 | I/E | Compromised container | → metadata 169.254.169.254 (would return the **host VM's** user-data and keys) | L3 pod nft output drop 169.254.0.0/16; L5 no secrets in user-data | S04 |
| T05 | T/E | Malicious team | → gateway 10.20.0.11 (SSH, stats 8404), VIP | L3 + L2 `cstam-gw` mgmt only from `cstam-cp` | S05 |
| T06 | T/E | Malicious team | → control plane 10.20.0.5 (:443, :8443, :22, Postgres) | L3 + L2 `cstam-cp`; Postgres on a unix socket only | S06 |
| T07 | S | Compromised container | Spoof another IP or MAC | L4 no `NET_ADMIN`/`NET_RAW`; L2 OVN port security drops any source IP/MAC that is not the port's | S07, S08 |
| T08 | D | Malicious team | Fork bomb, memory bomb, CPU burn | L4 `--pids-limit`, `--memory`, `--cpus`, per-team slice | S09, S10 |
| T09 | D | Malicious team | Fill the disk | L4 read-only rootfs, tmpfs size, log rotation, optional XFS quota | S11 |
| T10 | E | Malicious team | Ask the API for a privileged container, host mount, Docker socket | L5 app spec allowlist (unknown fields rejected) | S12 |
| T11 | T | Malicious team | Inject HAProxy config through a team name (`x\nbackend evil`) | L5 slug regex in API and DB domain, template escaping, `haproxy -c` | S13 |
| T12 | S/I | Our bug | Stale route after IP reuse (`edges.md`) | L5 teardown order, generation acks, quarantine (Step 8) | S14 |
| T13 | D | External attacker | HTTP flood on one subdomain | L1 per-host stick-table rate limit, `maxconn`, timeouts | load test L-03 (Step 11) |
| T14 | I/E | External attacker | Scan the FIP for admin ports | L2: only 80/443 on `cstam-gw` are open to the world | S15 |
| T15 | S | External attacker | Unknown `Host`, `admin.` subdomain | L1 default backend 404; admin UI not on the FIP | S16 |
| T16 | E | Compromised container | Egress abuse (spam on port 25, scanning) | L3 pod output policy, optional egress rate limit | S17 |
| T17 | R | Curious admin | Make a change, then deny it | L5 append-only, hash-chained `audit_log` | A01 |
| T18 | I | Curious admin | Read team secrets (env values) | L5 env values write-only, viewer role | A02 |
| T19 | S | Any | Rogue VM pretends to be an agent or the control plane | L5 mTLS: client cert from our CA + URI SAN check; agents pin the cp IP SAN | C01, C02 |
| T20 | I | Any | OpenStack credential leaked from repo or logs | `.gitignore`, gitleaks, credential restricted by access rules and expiry | A03 |
| T21 | D | Network fault | VRRP partition, split brain | L2 VRRP only between members; dashboard alarm; Step 3 mitigation | CH-05 (Step 11) |

## 9.2 Layer L2: Neutron security groups and port security

### 9.2.1 The five groups (already created in Step 2)

Step 2 created the groups with the exact rules below (`infra/pulumi/secgroups.go`). This section explains them; do not create them twice.

| Group | Attached to | Ingress rules | Purpose |
|---|---|---|---|
| `cstam-gw` | gw1, gw2, the `cstam-vip` port | tcp 80, 443 from 0.0.0.0/0 · tcp 22, 8404 and icmp from `cstam-cp` | Public HTTP(S); management only from the control plane |
| `cstam-vrrp` | gw1, gw2 | protocol 112 from `cstam-vrrp` | VRRP only between the two gateways |
| `cstam-cp` | cstam-cp | tcp 22, 443 from `ADMIN_CIDR` · tcp 8443 from `cstam-gw` and `cstam-host` | Admin access; agents dial in |
| `cstam-host` | host-a, host-b primary port | tcp 22 and icmp from `cstam-cp` | Host management only |
| `cstam-team` | every team port (pod-NIC) | tcp 80 and 8000–8099 from `cstam-gw` | Only gateways reach teams. **No self rule**, so team → team is dropped |

Rule budget: 5 groups × 2 default egress rules + 14 ingress rules + 6 existing ≈ 30 of the 100 allowed.

::: why
**Why `remote_group` and not IP ranges**
Everything lives in one /24, so an IP-range rule like "allow 10.20.0.0/24" would allow everybody. A `remote_group` rule says "allow packets whose source is a port that is a member of group X". Membership follows the ports: a rebuilt gateway with a new IP is trusted the moment it joins `cstam-gw`. OVN builds the member address sets from the **fixed IPs** of the member ports. HAProxy connects to teams from the gateway's own fixed IP (.11 or .12), never from the VIP, so the `cstam-team` rule matches.
:::

### 9.2.2 Port security: the anti-spoofing you get for free

Every Neutron port has `port_security_enabled=true` by default. OVN then drops any packet that leaves the port with a source MAC or source IP that is not the port's own (or an allowed-address pair). In the pod-NIC model, a team container that somehow got `NET_ADMIN` and added `10.20.0.33` to its interface still cannot send as team2: OVN drops the packet before it leaves the hypervisor.

### 9.2.3 Gotchas

::: warn
**Four ways to break L2 without noticing**
1. **The `default` group.** It allows all traffic from other members of `default`. Step 2 removes it from every CSTAM VM. Check: `openstack server show cstam-host-a -f value -c security_groups` must not list `default`. The two old VMs (`sbx-f0699704`, `adam`) are probably in `default`; that is one more reason they must not share a group with us.
2. **AAP addresses count as group members.** In the AAP fallback model, team IPs are allowed-address pairs on host ports, so they are "members" of `cstam-host`. **Never use `cstam-host` as a `remote_group` in a rule**, or every team would inherit host privileges. None of our rules do.
3. **The VIP port.** Traffic to the VIP is delivered to the parent gateway's VIF, so the parent's groups apply. We still put `cstam-gw` on `cstam-vip` for consistency.
4. **Egress.** We keep the default egress rules. Restricting egress in Neutron makes `apt`, `docker pull` and NTP fail in confusing ways. Team egress policy is enforced in the team's netns (9.3.2), where we know which team a packet belongs to.
:::

### 9.2.4 Done when

::: done
**Done when**
```bash
export OS_CLOUD=cstam
openstack security group rule list cstam-team -f value -c "IP Protocol" -c "Port Range" -c "Remote Security Group"
# exactly two ingress rules (tcp 80:80 and tcp 8000:8099, remote = cstam-gw id) + 2 egress defaults
for s in cstam-cp cstam-gw1 cstam-gw2 cstam-host-a; do
  echo -n "$s: "; openstack server show $s -f value -c security_groups
done                                   # no 'default' anywhere
openstack quota show --usage -f value | grep -i secgroup   # well below 10 / 100
```
:::

## 9.3 Layer L3: host and netns firewalls

Docker on Ubuntu 24.04 writes its rules through `iptables-nft` (the iptables command, nftables kernel engine). Several nftables tables can hook the same point, and **a packet is dropped if any base chain drops it**: an `accept` in Docker's table does not override a `drop` in ours. So we can add restrictions in our own tables without fighting Docker.

::: warn
**Never `flush ruleset` on a Docker host**
`/etc/nftables.conf` on Ubuntu starts with `flush ruleset`, which deletes Docker's tables too. Do not enable the stock `nftables.service` or `ufw` on hosts. Load only our own tables, with table-scoped commands (`nft delete table inet cstam_host` then `nft -f file`).
:::

### 9.3.1 Pod-NIC model: the host's own table

In the pod-NIC model, team traffic never crosses the host's root namespace. The root-namespace firewall only protects the host itself.

```text
# /etc/cstam/nft/cstam-host.nft  (rendered by hostagent, applied with: nft -c -f, then nft -f)
table inet cstam_host {
  chain input {
    type filter hook input priority -10; policy drop;
    ct state established,related accept
    ct state invalid drop
    iif lo accept
    ip saddr 10.20.0.5 tcp dport 22 accept               # SSH only from the bastion (cp)
    ip saddr 10.20.0.5 icmp type echo-request accept
    udp sport 67 udp dport 68 accept                      # DHCP for the primary NIC
  }
}
```

The host agent opens **no listening port** (it dials out to the control plane), so the input chain is short. Warm-pool NICs that sit unconfigured in the root namespace have no IP address, so they cannot receive anything for the host.

### 9.3.2 Pod-NIC model: the per-team netns table

This is the table the host agent loads inside each team's namespace (Step 5, file `pod.nft`). It is repeated here because it is a security control:

```text
table inet pod {
  set gateways { type ipv4_addr; elements = { 10.20.0.11, 10.20.0.12 } }

  chain input {
    type filter hook input priority 0; policy drop;
    ct state established,related accept
    ct state invalid drop
    iif lo accept
    ip saddr @gateways tcp dport { 80, 8000-8099 } accept
    icmp type { echo-request, destination-unreachable, time-exceeded } limit rate 10/second accept
  }
  chain output {
    type filter hook output priority 0; policy accept;
    ct state established,related accept
    oif lo accept
    ip daddr 169.254.0.0/16 counter drop          # metadata: would return the HOST VM's data
    ip daddr 10.20.0.0/24 counter drop            # gateways, cp, hosts, other teams
    ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10 } counter drop
    tcp dport 25 counter drop                     # no spam from FelCloud's IP range
    udp dport 53 accept
    tcp dport { 80, 443 } ct state new limit rate over 20/second burst 60 packets counter drop
    tcp dport { 80, 443 } accept
    counter drop                                  # everything else (scanning, IRC, raw TCP)
  }
}
```

Why each part exists:

- **Input from gateways only.** Neutron already enforces this (L2). The nft rule is the second layer and also covers ICMP and any rule mistake in Neutron.
- **Output drop of 10.20.0.0/24.** The default route goes to the router `10.20.0.1` as a *next hop*, not as a destination, so Internet egress is not affected. Direct connections to anything on the subnet are blocked.
- **Metadata.** On OVN the metadata service (169.254.169.254) is answered inside OVN for the requesting port's VM. A request from a team NIC on `cstam-host-a` gets **host-a's** metadata. Neutron security groups cannot block it (metadata is answered before the SG). This nft rule is the only layer that blocks it. That is why it must never be removed.
- **Egress policy.** DNS, HTTP and HTTPS are allowed so teams can `npm install`, with a per-team rate limit on new connections. Everything else is dropped.
- **Counters** on the drop rules make the attack tests visible: `ip netns exec team1 nft list table inet pod` shows how many packets each rule stopped.

Team containers run without `NET_ADMIN`, so they cannot change this table even though it lives in their namespace.

### 9.3.3 AAP fallback model: iptables chains via `DOCKER-USER`

If Gate G1 chose the fallback, team traffic crosses the host's root namespace (Docker bridge, DNAT, SNAT), and the host firewall becomes the main isolation layer. We use our own chains jumped from Docker's documented hook `DOCKER-USER`, applied atomically with `iptables-restore`. Bridges are named `cst-tNNNN` (15-character interface limit).

```text
*filter
:CSTAM-FWD - [0:0]
:CSTAM-TEAM - [0:0]
:CSTAM-ANTISPOOF - [0:0]
:CSTAM-XTEAM - [0:0]
:CSTAM-IN - [0:0]
:CSTAM-HOST-IN - [0:0]
-F CSTAM-FWD
-F CSTAM-TEAM
-F CSTAM-ANTISPOOF
-F CSTAM-XTEAM
-F CSTAM-IN
-F CSTAM-HOST-IN
# inbound: only gateways, only DNATed tcp/80
-A CSTAM-FWD -m conntrack --ctstate INVALID -j DROP
-A CSTAM-FWD -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
-A CSTAM-FWD -i ens3 -o cst-+ -s 10.20.0.11/32 -p tcp -m conntrack --ctstate DNAT -j ACCEPT
-A CSTAM-FWD -i ens3 -o cst-+ -s 10.20.0.12/32 -p tcp -m conntrack --ctstate DNAT -j ACCEPT
-A CSTAM-FWD -i ens3 -o cst-+ -j DROP
-A CSTAM-FWD -i cst-+ -j CSTAM-TEAM
-A CSTAM-FWD -j RETURN
# team-originated traffic
-A CSTAM-TEAM -j CSTAM-ANTISPOOF
-A CSTAM-TEAM -o cst-+ -j CSTAM-XTEAM
-A CSTAM-TEAM -d 169.254.0.0/16 -j DROP
-A CSTAM-TEAM -d 10.20.0.0/24 -j DROP
-A CSTAM-TEAM -d 10.0.0.0/8 -j DROP
-A CSTAM-TEAM -d 172.16.0.0/12 -j DROP
-A CSTAM-TEAM -d 192.168.0.0/16 -j DROP
-A CSTAM-TEAM -d 100.64.0.0/10 -j DROP
-A CSTAM-TEAM -o ens3 -p udp --dport 53 -j ACCEPT
-A CSTAM-TEAM -o ens3 -p tcp -m multiport --dports 80,443 -m conntrack --ctstate NEW -m hashlimit --hashlimit-above 20/sec --hashlimit-burst 60 --hashlimit-mode srcip --hashlimit-srcmask 24 --hashlimit-name cstam_egress -j DROP
-A CSTAM-TEAM -o ens3 -p tcp -m multiport --dports 80,443 -j ACCEPT
-A CSTAM-TEAM -j DROP
# one line per team (rendered)
-A CSTAM-ANTISPOOF -i cst-t0001 -s 172.20.1.0/24 -j RETURN
-A CSTAM-ANTISPOOF -j DROP
-A CSTAM-XTEAM -i cst-t0001 -o cst-t0001 -j ACCEPT
-A CSTAM-XTEAM -j DROP
# containers never talk to the host
-A CSTAM-IN -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
-A CSTAM-IN -j DROP
# team IPs never terminate on host services
-A CSTAM-HOST-IN -m iprange --dst-range 10.20.0.32-10.20.0.191 -j DROP
-A CSTAM-HOST-IN -j RETURN
COMMIT
```

Jumps, ensured idempotently by the agent:

```bash
iptables -w -C DOCKER-USER -j CSTAM-FWD 2>/dev/null || iptables -w -I DOCKER-USER 1 -j CSTAM-FWD
iptables -w -C INPUT -i cst-+ -j CSTAM-IN 2>/dev/null || iptables -w -I INPUT 1 -i cst-+ -j CSTAM-IN
iptables -w -C INPUT -i ens3 -j CSTAM-HOST-IN 2>/dev/null || iptables -w -I INPUT 2 -i ens3 -j CSTAM-HOST-IN
```

The two key tricks:
- **`--ctstate DNAT`** on the inbound rule: only connections that went through our DNAT rule (`-i ens3 -d <team IP> -p tcp --dport 80 -j DNAT`) are accepted, so nobody can reach `172.20.x.x` directly.
- **The DNAT rule matches `-i ens3`**, so team1 → team2's team IP is not DNATed. It becomes local delivery, hits `INPUT -i cst-t0001` → `CSTAM-IN` → drop.

Also bind host services to the primary IP only (`ListenAddress 10.20.0.21` in `sshd_config`), otherwise sshd answers on every team IP added to `ens3`.

### 9.3.4 The apply pipeline (same pattern as HAProxy)

Firewalls use the same "validate → apply → verify" pattern as the gateway, which is a nice story for the jury:

```go
// internal/host/firewall/apply.go
package firewall

// Apply validates rules first; the live ruleset is untouched if validation fails.
func ApplyNFT(ctx context.Context, netns string, rules []byte) error {
	check := []string{"nft", "-c", "-f", "-"}
	apply := []string{"nft", "-f", "-"}
	if netns != "" { // run inside the team namespace
		check = append([]string{"ip", "netns", "exec", netns}, check...)
		apply = append([]string{"ip", "netns", "exec", netns}, apply...)
	}
	if err := run(ctx, rules, check...); err != nil {
		return fmt.Errorf("nft validate: %w", err)
	}
	return run(ctx, rules, apply...)
}

func ApplyIPTables(ctx context.Context, rules []byte) error { // AAP fallback
	if err := run(ctx, rules, "iptables-restore", "-w", "--noflush", "--test"); err != nil {
		return fmt.Errorf("iptables validate: %w", err)
	}
	if err := run(ctx, rules, "iptables-restore", "-w", "--noflush"); err != nil {
		return err
	}
	return ensureJumps(ctx)
}

// run executes argv (never "sh -c") with the rules on stdin.
func run(ctx context.Context, stdin []byte, argv ...string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(stdin)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", argv[0], err, out)
	}
	return nil
}
```

Rules are rendered with `text/template` from **typed values only** (`netip.Addr`, `netip.Prefix`, validated slugs). A team name never reaches a rule file as free text. Rendering is deterministic (teams sorted by ID), so a golden-file test can compare the output byte by byte. The agent re-applies on start, after every team change, and every 30 s if a drift check (`nft -j list table inet pod` compared with the rendered file) finds a difference.

### 9.3.5 Teardown sanitization

When a team is deleted, established flows must not survive. In the pod-NIC model, deleting the pause container destroys the netns, and every socket and conntrack entry in it disappears with it. In the AAP fallback, the agent flushes conntrack explicitly:

```bash
conntrack -D -s 172.20.1.0/24 ; conntrack -D -d 172.20.1.0/24 ; conntrack -D -d 10.20.0.32
```

### 9.3.6 Done when

::: done
**Done when** (on cstam-host-a, with team1 running)
```bash
sudo nft list table inet cstam_host                     # present, policy drop
sudo ip netns exec team1 nft list table inet pod        # present, counters at 0 before tests
sudo ip netns exec team1 curl -s -m 3 http://169.254.169.254/ ; echo "exit=$?"   # exit=28 (timeout)
sudo ip netns exec team1 nft list chain inet pod output | grep 169.254          # counter > 0
sudo docker restart team1-pod && sleep 5 && sudo ip netns exec team1 nft list table inet pod >/dev/null && echo re-plumbed
```
:::

## 9.4 Layer L4: container hardening

Teams never get the Docker socket or SSH. They submit an **app spec** through the API (image, port, environment, resources). The host agent turns it into a container with a fixed set of hardening flags.

### 9.4.1 The app spec is an allowlist

```go
// internal/model/appspec.go
type AppSpec struct {
	Name     string            `json:"name"`            // ^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$
	Image    string            `json:"image"`           // registry/repo:tag or @sha256:..., allowed registries only
	Port     int               `json:"port"`            // 80 or 8000..8099
	Env      map[string]string `json:"env"`             // keys ^[A-Z_][A-Z0-9_]{0,63}$, values <= 4 KiB, never returned by GET
	CPU      float64           `json:"cpu"`             // <= team quota
	MemMiB   int               `json:"mem_mib"`         // <= team quota
	Writable bool              `json:"writable_rootfs"` // default false
}

func DecodeAppSpec(r io.Reader) (AppSpec, error) {
	dec := json.NewDecoder(io.LimitReader(r, 64<<10))
	dec.DisallowUnknownFields() // "privileged", "cap_add", "volumes", "network_mode" simply do not exist
	var s AppSpec
	if err := dec.Decode(&s); err != nil {
		return s, err
	}
	return s, s.Validate()
}
```

The host agent validates the spec again (defence in depth: a bug in the control plane must not be enough).

### 9.4.2 The exact container flags

The CLI form is shown because it is easy to test by hand; the agent makes the same call with the Docker SDK.

```bash
docker run -d --name team1-web \
  --network container:team1-pod \
  --label cstam.team=<team-uuid> --label cstam.app=web \
  --user 10001:10001 \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --security-opt apparmor=docker-default \
  --read-only --tmpfs /tmp:rw,nosuid,nodev,noexec,size=64m \
  --pids-limit 256 --ulimit nofile=4096:4096 \
  --memory 512m --memory-swap 512m --cpus 0.5 \
  --cgroup-parent cstam-team1.slice \
  --log-driver json-file --log-opt max-size=10m --log-opt max-file=3 \
  --restart unless-stopped \
  <registry>/<team image>@sha256:...
```

Apps listen on port 80 or 8000–8099. Binding port 80 without `CAP_NET_BIND_SERVICE` works because the pause container sets the namespaced sysctl `net.ipv4.ip_unprivileged_port_start=80` (the agent adds `--sysctl net.ipv4.ip_unprivileged_port_start=80` to the pause container, which owns the netns).

| Flag | Blocks | What it may break (tell teams in advance) |
|---|---|---|
| `--cap-drop ALL` | Raw sockets (spoofing, ARP tricks), `mount`, changing the netns firewall | Nothing for a normal web app |
| `no-new-privileges` | setuid escalation (`sudo`, `su`) | Images that switch user with `gosu` at start |
| `--read-only` + tmpfs | Malware persistence, writing to image layers | Apps writing caches: use `writable_rootfs: true` |
| `--pids-limit 256` | Fork bombs | Huge worker pools |
| `--memory` = `--memory-swap` | Memory bombs, swap thrashing | The app is OOM-killed (visible on the dashboard) |
| `--cpus` | CPU starvation of neighbours | Slower builds |
| `--user 10001` | Root inside the container | Images that need root at start |
| default seccomp | About 44 dangerous syscalls (`keyctl`, `bpf`, `unshare` ...) | Rarely anything |
| AppArmor `docker-default` | Writes to `/proc/sys`, `mount`, ptrace on host processes | Nothing |

### 9.4.3 Docker daemon and per-team slice

`/etc/docker/daemon.json` on hosts (Step 5 created it; the security-relevant keys are):

```json
{
  "no-new-privileges": true,
  "live-restore": true,
  "mtu": 1442,
  "bridge": "none",
  "log-driver": "json-file",
  "log-opts": { "max-size": "10m", "max-file": "3" },
  "exec-opts": ["native.cgroupdriver=systemd"],
  "cgroup-parent": "cstam.slice",
  "default-ulimits": { "nofile": { "Name": "nofile", "Soft": 4096, "Hard": 4096 } }
}
```

`"bridge": "none"` removes `docker0`, so there is no unfiltered default network. About `userns-remap`: it maps container root to an unprivileged host UID and is a strong control, but it makes `--network container:` and moving NICs into a container's netns more complex (the netns is then owned by a user namespace). **Default: off in the pod-NIC model**, compensated by `--user 10001`, `cap-drop ALL` and `no-new-privileges`. In the AAP fallback model it can be turned on (`"userns-remap": "default"`), and the attack suite should be re-run.

The per-container limits alone would let a team start 20 containers. A systemd slice per team caps the **sum**:

```ini
# created by the host agent through D-Bus (coreos/go-systemd StartTransientUnitContext); shown as a file for clarity
# /run/systemd/transient/cstam-team1.slice
[Slice]
Slice=cstam.slice
CPUQuota=150%
MemoryMax=1536M
MemorySwapMax=0
TasksMax=768
IOWeight=50
```

The top-level `cstam.slice` gets `MemoryMax` = host RAM − 1.5 GB and `CPUQuota` = (vCPUs − 0.5) × 100 %, so the host OS, dockerd and the agent always keep headroom.

Disk: read-only rootfs, tmpfs with a size, and log rotation already bound most disk use. For hard per-team quotas, put `/var/lib/docker` on a Cinder volume formatted XFS with `pquota` and add `--storage-opt size=2G` to writable apps. This is optional (it costs a volume of the 10-volume quota).

### 9.4.4 gVisor: optional strict mode

| | runc + hardening (default) | gVisor (`--runtime=runsc`) |
|---|---|---|
| Kernel attack surface | Host kernel, seccomp-filtered | User-space kernel; the host sees about 70 syscalls |
| Overhead | About 0 | Syscall-heavy apps 2–10× slower, +50 MB RAM per sandbox |
| Compatibility | Everything | Most web stacks; some database engines and tools fail |
| Works in a FelCloud VM | Yes | Yes with the `systrap` platform (no nested KVM needed) |
| Score impact | Covers the brief | Not scored directly; strong jury answer |

**Decision:** runc + hardening by default. An optional per-hackathon flag `isolation: strict` sets `Runtime: "runsc"` for app containers (never for the pause container). Do it only after Gate G3, and measure the overhead once for the report.

### 9.4.5 Done when

::: done
**Done when**
```bash
docker inspect team1-web --format '{{.HostConfig.CapDrop}} {{.HostConfig.ReadonlyRootfs}} {{.HostConfig.PidsLimit}} {{.HostConfig.Memory}}'
# [ALL] true 256 536870912
docker exec team1-web sh -c 'grep CapEff /proc/self/status'      # CapEff: 0000000000000000
systemctl show cstam-team1.slice -p MemoryMax -p TasksMax          # MemoryMax=1610612736 TasksMax=768
curl -s -X POST https://10.20.0.5/v1/teams/<id>/apps -H "Authorization: Bearer $TOKEN" \
  -d '{"name":"x","image":"nginx","port":80,"privileged":true}' -o /dev/null -w '%{http_code}\n'   # 422
```
:::

## 9.5 Layer L5 (part 1): the OpenStack credential and secrets

### 9.5.1 Inventory

| Secret | Holder | Storage | Rotation |
|---|---|---|---|
| OpenStack application credential (runtime) | cp only | `/etc/cstam/secrets/clouds.yaml`, mode 0600, passed with systemd `LoadCredential=` | New one per event, expiry = event end + 7 days |
| Internal CA private key | Your laptop, offline | `age`-encrypted outside the repo | CA 1 year, re-issue before the event |
| Leaf keys (cpd, gw1, gw2, host-a ...) | Each node | `/etc/cstam/pki/`, mode 0400, owner = service user | 90 days, `cstamctl pki issue` |
| Admin passwords | Humans | argon2id hash in `admins` | Forced change on first login |
| Sessions, API tokens | Browsers, bench | SHA-256 of the token in the DB | Sessions 8 h; tokens with expiry |
| PostgreSQL | – | No password: unix socket + `peer` auth | – |

### 9.5.2 A restricted application credential

The control plane needs only a few Neutron and Nova calls: ports (create, update, delete, list), attaching and detaching interfaces on host VMs, and reading servers. Keystone 3.14 on FelCloud supports **access rules**, which restrict an application credential to exact API paths. You run this once (it is a write operation, so you run it, not Claude):

```bash
cat > /tmp/access-rules.json <<'EOF'
[
 {"service":"network","method":"GET",   "path":"/v2.0/ports"},
 {"service":"network","method":"GET",   "path":"/v2.0/ports/*"},
 {"service":"network","method":"POST",  "path":"/v2.0/ports"},
 {"service":"network","method":"PUT",   "path":"/v2.0/ports/*"},
 {"service":"network","method":"DELETE","path":"/v2.0/ports/*"},
 {"service":"network","method":"PUT",   "path":"/v2.0/ports/*/tags"},
 {"service":"network","method":"GET",   "path":"/v2.0/networks/*"},
 {"service":"network","method":"GET",   "path":"/v2.0/subnets/*"},
 {"service":"compute","method":"GET",   "path":"/v2.1/servers/*"},
 {"service":"compute","method":"GET",   "path":"/v2.1/servers/*/os-interface"},
 {"service":"compute","method":"POST",  "path":"/v2.1/servers/*/os-interface"},
 {"service":"compute","method":"DELETE","path":"/v2.1/servers/*/os-interface/*"}
]
EOF
openstack application credential create cstam-cp-runtime \
  --role member --expiration 2026-12-31T00:00:00 \
  --access-rules /tmp/access-rules.json \
  --description "CSTAM control plane runtime (ports + interface attach only)"
shred -u /tmp/access-rules.json
```

Then write a `clouds.yaml` with `auth_type: v3applicationcredential`, the new ID and secret, copy it to the cp only, and delete the local copy.

::: warn
**Verify both directions**
Access-rule paths are matched literally per API prefix, and some clients call paths you did not expect (for example the Nova path may include the project ID on older deployments). Test before relying on it:
```bash
OS_CLIENT_CONFIG_FILE=./runtime-clouds.yaml openstack --os-cloud cstam-runtime port list --tags cstam   # works
OS_CLIENT_CONFIG_FILE=./runtime-clouds.yaml openstack --os-cloud cstam-runtime server create ...       # must fail (401/403)
OS_CLIENT_CONFIG_FILE=./runtime-clouds.yaml openstack --os-cloud cstam-runtime application credential create x  # must fail
```
If a needed call is refused, add exactly that path. Keep your broad credential for `infra/` scripts on your laptop. A credential created without `--unrestricted` can never create other credentials, so a compromised cp cannot persist itself.
:::

If Gate G1 picked fallback F1 (the keepalived notify script moves the floating IP), the gateways get a **second** credential whose only rule is `PUT /v2.0/floatingips/<the FIP id>`.

### 9.5.3 Rules for every secret

- Never in cloud-init user-data. It stays readable at 169.254.169.254 for the whole life of the VM.
- Never in git: `.gitignore` covers `.secrets/`, `*.key`, `clouds.yaml`, `.env`; a `gitleaks protect --staged` pre-commit hook (Step 0) catches the rest.
- Never in logs: wrap gophercloud's HTTP client with a logger that redacts `X-Auth-Token` and request bodies.
- Agents get their keys after boot through `cstamctl node enroll` over SSH via the bastion (Step 10).

## 9.6 Layer L5 (part 2): mTLS with a small Go CA

### 9.6.1 Options

| | Shared token over TLS | **Own Go CA (`cstamctl pki`)** | smallstep `step-ca` | WireGuard mesh |
|---|---|---|---|---|
| Mutual authentication | No | **Yes** | Yes | Yes (keys) |
| Moving parts | 0 | **One CLI command** | One more daemon | Kernel module + config per node |
| Explainable line by line | Yes | **Yes (~150 lines)** | Partly | Partly |
| Renewal | n/a | Manual or cron | Automatic | n/a |
| Verdict | Too weak | **Chosen** | Fine, not needed | Not needed |

### 9.6.2 The CA in Go

```go
// internal/pki/ca.go (used by cstamctl pki init / issue)
package pki

func NewCA(cn string, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"CSTAM"}},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              time.Now().Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true, // the CA cannot sign intermediate CAs
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	return pemCert(der), pemKey(key), nil
}

// Issue creates a leaf usable as BOTH server and client certificate.
func Issue(ca *x509.Certificate, caKey crypto.Signer, name string, ip netip.Addr, uri string, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	u, err := url.Parse(uri) // spiffe://cstam/gateway/gw1
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-5 * time.Minute),
		NotAfter:     time.Now().Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  []net.IP{ip.AsSlice()},
		URIs:         []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	return pemCert(der), pemKey(key), nil
}
```

### 9.6.3 Server side (cpd agent listener :8443) and client side (agents)

Agents dial out, so **cpd is the TLS server** and the agents are clients. Both sides verify each other.

```go
// internal/platform/tlsconf.go
func AgentServerTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	pool, err := loadPool(caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert, // no cert from our CA = handshake fails
	}, nil
}

func AgentClientTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	pool, err := loadPool(caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,      // ONLY our CA: a public CA cannot impersonate cpd
		ServerName:   "10.20.0.5", // must match the IP SAN of cpd.crt
	}, nil
}
```

Authentication says "this certificate was signed by our CA". **Authorization** says "this identity may do this call". A Connect interceptor on cpd reads the URI SAN and checks that the agent only speaks for itself:

```go
// internal/agentapi/authz.go
type peerCertKey struct{}

// PeerCertMiddleware runs before the Connect handlers and stores the verified client certificate.
func PeerCertMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			r = r.WithContext(context.WithValue(r.Context(), peerCertKey{}, r.TLS.PeerCertificates[0]))
		}
		next.ServeHTTP(w, r)
	})
}

func IdentityFrom(ctx context.Context) (kind, name string, err error) {
	cert, _ := ctx.Value(peerCertKey{}).(*x509.Certificate)
	if cert == nil || len(cert.URIs) != 1 {
		return "", "", connect.NewError(connect.CodeUnauthenticated, errors.New("no agent identity"))
	}
	parts := strings.Split(strings.TrimPrefix(cert.URIs[0].String(), "spiffe://cstam/"), "/")
	if len(parts) != 2 || (parts[0] != "gateway" && parts[0] != "host") {
		return "", "", connect.NewError(connect.CodePermissionDenied, errors.New("not an agent"))
	}
	return parts[0], parts[1], nil
}

// In Ack/Report/Watch handlers: the request's agent_id must equal the certificate name.
if kind != "gateway" || name != req.Msg.GatewayName {
	return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s/%s cannot act as %s", kind, name, req.Msg.GatewayName))
}
```

So a compromised host cannot acknowledge a generation on behalf of a gateway (which could otherwise free an IP too early, the `edges.md` bug again).

Rotation: leaves last 90 days; `cstamctl pki issue` + copy + `systemctl reload`. The agents re-read files on SIGHUP through `tls.Config.GetClientCertificate`. The dashboard's node panel shows certificate expiry (amber under 14 days).

::: done
**Done when**
```bash
# from gw1 (has gw1.crt): works
curl --cacert /etc/cstam/pki/ca.crt --cert /etc/cstam/pki/gw1.crt --key /etc/cstam/pki/gw1.key \
  -H 'Content-Type: application/json' -d '{}' https://10.20.0.5:8443/cstam.agent.v1.ControlService/Ping
# without a client certificate: handshake failure ("certificate required")
curl --cacert /etc/cstam/pki/ca.crt https://10.20.0.5:8443/ ; echo "exit=$?"
# with host-a's cert claiming to ack for gw1: permission_denied
```
:::

## 9.7 Layer L5 (part 3): admin authentication, CSRF, audit

### 9.7.1 Options

| | Static admin token | **Password (argon2id) + server-side session** | JWT | OIDC (Keycloak/Dex) |
|---|---|---|---|---|
| Revoke / logout everywhere | Rotate the token | **Delete rows** | Hard (needs a deny list) | Yes |
| Browser safety | Token in JavaScript storage (XSS can steal it) | **HttpOnly cookie** | Usually JavaScript storage | Cookie |
| Effort | 10 min | **About 1 day** | About 1 day | 2–3 days + one more VM |
| Verdict | Too weak | **Chosen** | No advantage here | Overkill |

Roles: `admin` (everything), `viewer` (read-only; give one to the jury during the pitch). Hashed personal API tokens for `cstamctl` and the benchmark tools.

### 9.7.2 Passwords and sessions

```go
// internal/auth/password.go  (golang.org/x/crypto/argon2)
const (time_, memKiB, threads, keyLen = 3, 64 * 1024, 2, 32)

func Hash(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	k := argon2.IDKey([]byte(pw), salt, time_, memKiB, threads, keyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", memKiB, time_, threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(k)), nil
}

func Verify(pw, encoded string) bool {
	salt, want, ok := parse(encoded)
	if !ok {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, time_, memKiB, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1 // no timing leak
}
```

Session rules:
- 32 random bytes → cookie `cstam_session` with `HttpOnly; Secure; SameSite=Strict; Path=/`. The database stores only `sha256(token)`, so a database dump does not give working sessions.
- Idle timeout 30 min, absolute timeout 8 h. Logout deletes the row.
- Login rate limit: 5 attempts per 5 minutes per source IP and per username (`golang.org/x/time/rate`), and always the same generic error message.

### 9.7.3 CSRF

CSRF (cross-site request forgery) is when another web page makes the admin's browser send a request with the admin's cookie. Three cheap defences, all on:
1. `SameSite=Strict` on the cookie: the browser does not send it on cross-site requests.
2. State-changing requests must carry the header `X-CSRF: 1`. A cross-site page cannot add a custom header without a CORS preflight, and we answer no CORS preflight.
3. The `Origin` header of state-changing requests must be the dashboard origin.

### 9.7.4 The audit log (hash chain)

Every mutating endpoint is wrapped by `audit.Middleware(action)`. The table was created in Step 6 (`audit_log`, with `prev_hash` and `hash`). Each row's hash covers the previous row's hash, so editing or deleting a row breaks the chain from that point on.

```go
// internal/audit/audit.go
func Append(ctx context.Context, tx pgx.Tx, e Entry) error {
	// serialise appenders: one chain, no forks
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(4242)"); err != nil {
		return err
	}
	var prev []byte
	err := tx.QueryRow(ctx, "SELECT hash FROM audit_log ORDER BY id DESC LIMIT 1").Scan(&prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	canon, _ := json.Marshal(e) // struct with fixed field order = canonical encoding; secrets redacted before
	h := sha256.Sum256(append(append([]byte{}, prev...), canon...))
	_, err = tx.Exec(ctx, `INSERT INTO audit_log (at, actor, actor_ip, action, target, request_id, outcome, details, prev_hash, hash)
	                       VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		e.At, e.Actor, e.ActorIP, e.Action, e.Target, e.RequestID, e.Outcome, e.Details, prev, h[:])
	return err
}
```

`cstamctl audit verify` walks the table in `id` order, recomputes every hash and reports the first broken link. The app database role has `REVOKE UPDATE, DELETE, TRUNCATE` on the table (Step 6), so only a superuser can even try.

## 9.8 Management surfaces: HAProxy, keepalived, SSH, systemd

### 9.8.1 HAProxy and keepalived

- HAProxy sockets are unix sockets only (`/run/haproxy/admin.sock`, `/run/haproxy-master.sock`, mode 0660, group `haproxy`). There is never a TCP admin socket.
- The stats/Prometheus frontend binds the gateway's own IP on 8404, and `cstam-gw` allows 8404 only from `cstam-cp`.
- The `/__gw/healthz` endpoint answers only `127.0.0.1`, the peer gateway and the cp (ACL `internal`); every other `/__gw/` path returns 404.
- The gateway agent's API to the control plane is **typed** (routes, slots, generation). It never accepts "run this command", so there is no command injection path from the control plane either.
- keepalived: `enable_script_security`, `script_user keepalived_script`, check scripts owned by root with mode 0755. VRRP PASS authentication is not used (it is plain text and deprecated in VRRPv3); protection is the `cstam-vrrp` group.

### 9.8.2 SSH and the bastion

Only the control plane VM has a floating IP for SSH; `cstam-cp` allows 22 only from `ADMIN_CIDR`. Gateways and hosts are reached through it with `ProxyJump` (Step 10 shows `~/.ssh/config`).

```text
# /etc/ssh/sshd_config.d/10-cstam.conf  (all CSTAM VMs)
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
AllowUsers ubuntu
X11Forwarding no
AllowAgentForwarding no
MaxAuthTries 3
ListenAddress 10.20.0.21      # host-specific: the primary IP only
```

Enable `unattended-upgrades` for security updates, but disable automatic reboots during the event.

### 9.8.3 systemd hardening of our own services

```ini
# drop-in for gwagent.service (cpd similar; hostagent needs more privileges)
[Service]
User=cstam
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/etc/haproxy /var/lib/cstam /run/cstam
CapabilityBoundingSet=
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
SystemCallArchitectures=native
```

::: warn
**Be honest about the host agent**
The host agent needs root-level powers: `CAP_NET_ADMIN`, `CAP_SYS_ADMIN` (moving NICs between namespaces) and Docker access (Docker access is root-equivalent). Say so in the security document. Its mitigations: it opens no listening port, it only pulls typed desired state over mTLS, it validates every spec again, and it executes external commands with argv lists, never through a shell.
:::

## 9.9 The attack suite: `cstam-sectest`

The rule is simple: **every control has an attack test, and every attack test has a positive control.** A "blocked" result means nothing if the probe cannot even reach its own app. Positive controls (P tests) prove the probe works, so a PASS on an attack really means "blocked by policy".

### 9.9.1 Setup

The harness creates three throw-away teams through the **real API**: `sec-a` and `sec-b` on host-a, `sec-c` on host-b (if it exists). Each has:
- a normal app (`traefik/whoami` on port 8000), the target for lateral tests;
- a **probe** container in the team's netns, built from `bench/probe/Dockerfile` (alpine + curl, netcat-openbsd, iproute2, iputils-arping, bind-tools), started with exactly the same hardening as a team app: a realistic attacker.
- With `--worst-case`, a second probe started **directly with Docker by the harness** with `--cap-add NET_ADMIN --cap-add NET_RAW` in the same netns. The API can never create this. It proves that L2 and L3 still hold if L4 were bypassed.

Probes run with `docker exec` on the host through SSH (the host agent deliberately has no exec feature).

### 9.9.2 Catalogue

Addresses: sec-a has 10.20.0.x(a), sec-b 10.20.0.x(b), sec-c 10.20.0.x(c); the harness reads them from the API.

| ID | From | Attempt (inside the probe) | Expected | Layer |
|---|---|---|---|---|
| P01 | sec-a probe | `curl -sf -m3 http://127.0.0.1:8000/` (own app, same netns) | **200** | positive control |
| P02 | sec-a probe | `curl -sf -m5 https://example.com -o /dev/null` | **200** | positive (egress allowed) |
| P03 | runner on cp | `curl -sf -m3 -H 'Host: sec-a.<hackathon>.<BASE_DOMAIN>' http://10.20.0.10/` | **200** | positive (gateway path) |
| S01 | sec-a | `curl -m3 http://<sec-b IP>:8000/` | timeout | L2 + L3 |
| S02 | sec-a | `curl -m3 http://<sec-c IP>:8000/` | timeout | L2 + L3 |
| S03 | sec-a | `nc -z -w2 10.20.0.21 22` (host primary IP) | timeout | L3 + L2 |
| S04 | sec-a | `curl -m3 http://169.254.169.254/openstack/latest/meta_data.json` | timeout; `nft` counter on 169.254 rule +1 | L3 (only layer) |
| S05 | sec-a | `nc -z -w2 10.20.0.11 22`; `nc -z -w2 10.20.0.11 8404`; `nc -z -w2 10.20.0.10 22` | timeout | L3 + L2 |
| S06 | sec-a | `nc -z -w2 10.20.0.5 443`; `... 8443`; `... 22`; `... 5432` | timeout | L3 + L2 |
| S07 | worst-case probe | `ip addr add <sec-b IP>/24 dev eth0; curl --interface <sec-b IP> -m3 http://example.com` | timeout (OVN port security drops the source) | L2 |
| S08 | normal probe | `ip addr add 1.2.3.4/32 dev eth0`; `arping -c1 -U -I eth0 10.20.0.1` | `Operation not permitted` | L4 |
| S09 | normal probe | fork bomb for 20 s | `can't fork`; P03 for sec-b still 200 during the attack; `ssh host-a true` < 1 s | L4 |
| S10 | normal probe | `tail /dev/zero` (memory) | exit 137 (OOM inside the container); host free RAM unchanged | L4 |
| S11 | normal probe | `dd if=/dev/zero of=/tmp/f bs=1M count=200` | ENOSPC at 64 MiB | L4 |
| S12 | runner | `POST /v1/teams/sec-a/apps` with `privileged`, `cap_add`, `network_mode`, `volumes` | 422 each; nothing created | L5 |
| S13 | runner | create teams named `x\nbackend evil`, `../../etc`, `a b`, a 200-character name | 422; gateway config generation unchanged | L5 |
| S14 | runner | `edges.md` scenario: create A, delete A, create B immediately, request A's host | 404, never B's app; B never gets A's IP during quarantine | L5 (Step 8) |
| S15 | laptop | `nmap -Pn -p 1-10000 <FIP>` | only 80 and 443 open | L2 |
| S16 | laptop | `curl -H 'Host: nope.<BASE_DOMAIN>' http://<FIP>/`; `curl http://<FIP>/__gw/healthz` | 404; 404 | L1 |
| S17 | normal probe | `nc -z -w3 smtp.gmail.com 25`; 200 new HTTPS connections in 1 s | 25 blocked; rate-limit counter increases | L3 |
| C01 | cp | TLS to cpd :8443 without client cert | handshake failure | L5 mTLS |
| C02 | host-a | `Ack` for gw1 with host-a's certificate | `permission_denied` | L5 mTLS authz |
| A01 | superuser psql | `UPDATE audit_log SET actor='x' WHERE id=…`, then `cstamctl audit verify` | verify reports the broken link | L5 audit |
| A02 | viewer session | `GET /v1/teams/sec-a/apps`; `DELETE /v1/teams/sec-a` | env values `"***"`; 403 | L5 RBAC |
| A03 | CI | `gitleaks detect` on the repository | no findings | hygiene |

### 9.9.3 Harness in Go

```go
// internal/sectest/sectest.go
package sectest

type Expect int

const (
	Allowed Expect = iota // exit 0 / 2xx
	Blocked               // timeout or refused (where refused is acceptable)
	Denied                // EPERM or HTTP 4xx
)

type Probe struct {
	ID, Desc, Layer string
	From            string   // "sec-a/probe", "sec-a/worst", "runner", "laptop"
	Cmd             []string // argv, never a shell string
	Expect          Expect
	Timeout         time.Duration
	Check           func(Result) error // optional: counter increased, neighbour still healthy...
}

type Result struct {
	ID              string
	ExitCode        int
	Stdout, Stderr  string
	Duration        time.Duration
	CounterDelta    map[string]int64 // nft/iptables counters before/after
	Verdict         string           // PASS | FAIL
}

type Executor interface {
	Run(ctx context.Context, p Probe) (Result, error) // ssh + docker exec, local exec, or HTTP
}

func Run(ctx context.Context, ex Executor, probes []Probe) []Result
func WriteMarkdown(w io.Writer, rs []Result) error // table for the security document
func WriteJSON(w io.Writer, rs []Result) error
```

Verdict rules: `Blocked` passes on curl exit 28 (timeout), `nc` exit 1, or "connection refused" only where the catalogue allows it; `Allowed` passes on exit 0 or HTTP 2xx; any FAIL makes the process exit with code 1. Each attack also snapshots the relevant counters (`ip netns exec <team> nft -j list table inet pod`) before and after, and the report prints "dropped by rule X (+n packets)". That is strong evidence in the report.

### 9.9.4 Running it

```bash
make sectest
# = go run ./cmd/cstamctl sectest --api https://10.20.0.5 --token $CSTAM_BENCH_TOKEN \
#     --host host-a --host2 host-b --worst-case --out reports/security
# creates sec-a/b/c, runs ~30 probes (~2 min), deletes the teams, writes reports/security/<timestamp>.{md,json}
```

GitHub-hosted CI cannot reach the private subnet. The recommended "CI-like" setup is a systemd timer on the cp that runs the suite nightly and after each deploy, and keeps the last report. The latest table goes verbatim into the security architecture document (Step 14).

::: done
**Done when**
- `make sectest` exits 0, with every P test PASS and every S/C/A test PASS.
- The report shows non-zero counter deltas for S01, S04, S05, S06, S17 (proof that a rule, not luck, stopped them).
- Re-running after a host reboot (`openstack server reboot --hard cstam-host-a`, run by you) gives the same result once the agent has re-plumbed the teams.
:::

## Pitfalls

::: warn
**The mistakes that silently break isolation**
- Testing only "blocked" without positive controls: a broken probe reports everything as blocked.
- A pod restart: the new netns has **no** nft table until the agent re-plumbs it. The agent must watch Docker events and re-apply before the team is reachable again (Step 5). S04 after `docker restart team1-pod` catches it.
- `nft flush ruleset` from a tutorial on a host: it removes Docker's rules and ours.
- Adding a "temporary" `--remote-ip 10.20.0.0/24` rule to `cstam-team` while debugging, and forgetting it. `make sectest` will catch it (S01 fails); never skip the suite before the demo.
- Putting a credential in cloud-init "just for the test": S04 would then expose it if the metadata block ever breaks.
- Letting the dashboard return environment values "for convenience": A02 fails.
:::

## Learning checkpoint

::: learn
**Answer these yourself before moving on**
1. Walk a packet from team1's container to team2's IP in the pod-NIC model. Name every point where it could be dropped and which one drops it first.
2. Why is the metadata block in the team's nft table the *only* layer that stops S04? Why can a Neutron security group not do it?
3. What does `--ctstate DNAT` add in the AAP fallback rules? Give one packet it accepts and one it drops.
4. What is the difference between authentication and authorization in our mTLS setup? Which attack does the URI SAN check stop?
5. Why does a hash chain make the audit log tamper-evident but not tamper-proof?
:::

## Driving Claude Code for this step

::: claude
- Rules first, by hand: *"Explain line by line what each rule in the `pod` nft table matches. For each rule, give one packet that matches and one that does not. Do not write code."* Then load the table by hand in a test netns and run S01/S04 yourself.
- Then the renderer: *"Write `internal/host/firewall` with a text/template renderer from typed values (netip.Addr, netip.Prefix), a golden-file test for 0, 1 and 3 teams, and ApplyNFT that validates with `nft -c -f -` before applying. External commands as argv only."*
- PKI: *"Implement `cstamctl pki init` and `pki issue` with crypto/x509 as in section 9.6.2, plus a test that issues a gw1 certificate and checks its IP and URI SANs and that the CA has MaxPathLenZero."*
- The suite: *"Implement the sectest catalogue as data (one Probe per row of 9.9.2) and an SSH+docker-exec Executor. Add a fake Executor test that checks verdict rules (timeout = Blocked PASS, exit 0 on an attack = FAIL)."*
- Review: no rule mentions an IP outside `topology.go`; no `sh -c`; the access-rules file contains no path you cannot explain; the hostagent's privileges are documented.
- Never let the agent create the application credential or modify security groups: it prints the commands, you run them.
:::
