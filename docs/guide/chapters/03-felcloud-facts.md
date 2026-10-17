# 3. What FelCloud gives us

A design that ignores the real cloud fails on the real cloud. Every fact in this chapter was read from FelCloud on 16 October 2026 with **read-only** commands (`openstack --os-cloud cstam ... list/show`, plus read-only GET requests on the Octavia API). Nothing was created or changed. Each fact comes with its consequence for the design.

Re-run the commands yourself in Step 0 (the "doctor" checks). Clouds change, and your numbers must be your own.

## 3.1 Services

| Fact | Command | Consequence |
|---|---|---|
| Real OpenStack IaaS: Keystone **v3.14**, Nova (compute 2.1, microversions up to **2.100**), Neutron, Cinder, Glance, Swift, Octavia, Heat, Placement, Skyline dashboard | `openstack catalog list`, `openstack versions show` | We can use tenant-level APIs for everything. Heat exists (bonus: "OpenStack native orchestration"). Swift exists (backups of the database, release artifacts). |
| **No container service**: neither Magnum nor Zun | `catalog list` | Containers run on our own VMs. |
| **No Barbican** (secrets) | `catalog list` | Octavia cannot terminate TLS for us; we terminate TLS in HAProxy. |
| **No Designate** (DNS), although the `dns-integration` extension is listed | `catalog list`, `extension list` | We cannot create DNS records through OpenStack. DNS depends on FelCloud (a wildcard record or a delegated zone) or on `sslip.io` as a zero-dependency fallback. |
| Keystone 3.14 supports **application credential access rules** (since 3.13) | `versions show --service identity` | The control plane can run with a credential restricted to exactly the Neutron/Nova calls it needs. |
| Octavia has two providers, **`amphora`** and **`ovn`**, and two flavors, **"Active-Standby LoadBalancer"** and "Standalone LoadBalancer". No load balancers exist. | `GET /v2/lbaas/providers`, `/v2/lbaas/flavors` | Two credible fallbacks for the HA layer (chapter 4 and ADR-07). The amphora Active-Standby flavor uses the same mechanism as our VIP (an unbound port plus allowed-address-pairs plus keepalived), so if it works on FelCloud, our design should work too. Amphora VMs run in Octavia's service project and do not use our quota. |
| `python-octaviaclient` and `python-heatclient` are not installed locally | – | `pip install python-octaviaclient python-heatclient` when you need them. |

## 3.2 Network

| Fact | Command | Consequence |
|---|---|---|
| `cstam-sandbox-net` = 10.20.0.0/24, gateway 10.20.0.1, DNS 8.8.8.8 / 1.1.1.1 | `subnet show cstam-sandbox-subnet` | One shared private network for gateways, hosts, control plane and team IPs. |
| **Neutron's allocation pool is only 10.20.0.200 – 10.20.0.250** (51 addresses) | `subnet show cstam-sandbox-subnet -c allocation_pools` | Neutron auto-assigns addresses only from .200–.250. Addresses .2–.199 are given out only when a port asks for a specific IP. **Our IPAM owns .32–.191**, so Neutron can never hand one of our quarantined IPs to someone else automatically. **No subnet change is needed.** |
| **Our project owns the network** (`project_id` matches our token) | `network show cstam-sandbox-net -c project_id` | As owner, we may create ports with an explicit fixed IP (Neutron's default policy allows owner or admin). |
| Existing VM `sbx-f0699704` has **10.20.0.68**, outside the pool | `port list --network cstam-sandbox-net` | Proves that fixed IPs outside the pool work. .68 is inside our team range, so the IPAM seeds it as `RESERVED`. The reconciler must detect any "foreign" port in our range. |
| 10.20.0.200 is a `network:distributed` port | `port list` | This is the **OVN metadata port**: FelCloud uses **ML2/OVN**. OVN-specific behavior (virtual ports, ARP-based binding) applies to our VIP and team IPs. |
| **MTU = 1442** (Geneve overlay); `INTERNET` is 1500 | `network show cstam-sandbox-net -c mtu` | Docker and our team interfaces default to 1500. Packets bigger than 1442 bytes are dropped silently when ICMP "fragmentation needed" is filtered, so large downloads hang. **Every NIC, bridge and Docker network we create uses MTU 1442**, plus TCP MSS clamping in the AAP fallback. |
| No router connects `cstam-sandbox-net` to `INTERNET`. The only router, `private_router`, has no external gateway and serves another subnet. | `router list`, `router show` | Step 2 creates `cstam-router` with an external gateway on `INTERNET`. Without it, a floating IP cannot work. |
| External network `INTERNET` = 197.5.133.0/24 | `network list --external` | Source of our one floating IP (plus one for the control plane's SSH and dashboard). |
| A shared external network `Rancher-mgmt` (172.19.0.0/16) is owned by FelCloud | `network list` | FelCloud runs Rancher, so **the judges know Kubernetes**. Expect "why not k8s?" in Q&A (answer in chapter 4). |

### Neutron extensions

| Present | Why we care |
|---|---|
| `allowed-address-pairs`, `port-security` | VRRP VIP on two gateway ports; AAP fallback for team IPs |
| `revision-if-match`, `standard-attr-revisions` | Compare-and-swap on port updates (`If-Match: revision_number=N`, HTTP 412 on conflict). Fixes the lost-update race on allowed-address-pairs. |
| `standard-attr-tag`, `tag-creation` | Tags can be set **in the create request**, so a port is never untagged, even briefly. The reconciler finds our objects by tag. |
| `l3-ha` | The router's gateway is highly available on FelCloud's side. |
| `qos`, `qos-bw-limit-direction`, `qos-fip` | Bandwidth limits per port (bonus option). |
| `address-group`, `security-groups-remote-address-group`, `stateful-security-group` | Address groups in rules (we use remote security groups instead). |
| `floating-ip-port-forwarding-detail`, `extraroute`, `enable-default-route-ecmp`, `external-gateway-multihoming` | Not used. |

| Absent | Consequence |
|---|---|
| **`trunk`** | No VLAN subport per team. |
| **`allowed-address-pairs-atomic-add-remove`** | An allowed-address-pairs update **replaces the whole list**. Concurrent updates lose writes unless we serialize them and use `If-Match`. |
| BGP dynamic routing | No BGP/ECMP active/active gateways; tenants cannot peer with provider routers. |
| DVR (`dvr`) | Not relevant; OVN handles distributed routing itself. Whether floating IPs are centralized or distributed cannot be read as non-admin and must be tested (experiment E1). |

## 3.3 Compute and quotas

| Fact | Command | Consequence |
|---|---|---|
| Quota: **10 instances, 20 vCPU, 50 GB RAM** | `quota show` | VM-per-team cannot pass the 10+ stress test (chapter 4). We budget 7 VMs. |
| Quota: **10 volumes**, 1000 GB | `quota show` | If FelCloud images boot from volume (the existing VMs did), every VM uses one of 10 volumes. |
| Quota: **10 security groups, 100 security-group rules** | `quota show` | **One security group per team is impossible.** We use 5 role groups and about 36 rules, with one shared `cstam-team` group. |
| Quota: 50 floating IPs, **500 ports**, 10 routers, 10 server groups, 10 server-group members | `quota show` | Ports are not a constraint (50 teams = 50 ports plus infrastructure). |
| Usage: 2 instances, 5 vCPU, 5 GB RAM already used by `sbx-f0699704` (1c1g, ACTIVE) and `adam` (4c4g, **SHUTOFF**: still counts) | `limits show --absolute` | 8 instances and 15 vCPU are free. Deleting `adam` (only if FelCloud agrees) frees 4 vCPU. |
| `sbx-f0699704` took **39 s** from creation to ACTIVE, booting from a 10 GB volume | `server show` (created / launched timestamps) | A real VM boot time on this cloud, before cloud-init even starts. Reinforces "no VM boot per team". |
| `sbx-f0699704`'s security group is described "CSTAM sandbox: HTTP from gateways" but has **no ingress rules** | `security group show` | A known finding to mention: our groups are created with explicit rules referencing the gateway group. |
| Nova microversions up to **2.100** | `versions show --service compute` | `soft-anti-affinity` (needs 2.15) is available. Interface hot-plug is available. |
| `hypervisor list` returns **403** | – | We cannot count hypervisors. Anti-affinity is proven after boot by comparing each server's `hostId` (a per-project hash of the host). |
| One availability zone: `TN-Carthage` | `availability zone list` | Anti-affinity protects against a single hypervisor failure, not a site failure. |
| Images: Ubuntu 22.04/24.04, Debian 12/13, Rocky 10, Fedora CoreOS 43. Ubuntu 24.04 has `hw_qemu_guest_agent=yes` and no `hw_machine_type` | `image list`, `image show` | Ubuntu 24.04 everywhere (keepalived 2.2, HAProxy 2.8 in the distribution, 3.2 from a PPA). The machine type (`pc` or `q35`) decides how many NICs can be hot-plugged into a VM, which decides the team-networking model (experiment H1). |
| Flavors `G0.basic.*`, `G0.shared.*`, `G0.optimized.*` | `flavor list` | Gateways `G0.basic.1c2g`, control plane `G0.basic.2c4g`, hosts `G0.basic.4c8g`. "basic" rather than "shared" to reduce CPU steal, which matters for VRRP timers (confirm with FelCloud). |
| No Heat stacks exist | `stack list` (needs heatclient) | Heat is free for our infrastructure templates (bonus). |

## 3.4 The capacity budget

| VM | Flavor | Count | vCPU | RAM |
|---|---|---|---|---|
| Gateways | G0.basic.1c2g | 2 | 2 | 4 GB |
| Control plane | G0.basic.2c4g | 1 | 2 | 4 GB |
| Hackathon hosts | G0.basic.4c8g | 2 | 8 | 16 GB |
| Existing (`sbx-f0699704`, `adam`) | 1c1g, 4c4g | 2 | 5 | 5 GB |
| **Total vs quota** | | **7 / 10** | **17 / 20** | **29 / 50 GB** |

A third host fits if `adam` is deleted (with FelCloud's agreement). The Step 1 lab reuses the real gateway and host VMs, so experiments cost no extra quota.

## 3.5 Things we could not verify (and how we will)

Some behaviors are invisible to a non-admin user. They become Step 1 experiments.

| Unknown | Why it matters | Experiment |
|---|---|---|
| Does a floating IP on an **unbound VIP port** follow keepalived failover on OVN (centralized vs distributed floating IPs)? | The whole HA design | E1 |
| Does a reserved team port plus an allowed-address-pair on the host make the team IP reachable, with ARP answered by the **host VM's MAC**? | AAP fallback for team networking | E2 |
| Neutron's `max_allowed_address_pair` (default 10) | AAP fallback capacity per host | H2 |
| How many NICs can be **hot-plugged** into one VM, and how long one attach takes | Pod-NIC model (our primary team-networking model) | H1 |
| Can we create ports with explicit fixed IPs outside the pool with our credential? | Team pool .32–.191 | E3 |
| Does VRRP (IP protocol 112) pass between two ports on this cloud? | Keepalived at all | E1/E6 |
| Does the Octavia `ovn` provider support TCP health monitors? | Fallback F2 | E7 |
| Is FelCloud's CPU steal low enough for 0.5 s VRRP adverts? | Failover time | E6 (1 h soak) |

## 3.6 Questions for the FelCloud mentors (send on day 1)

Answers take days, so Step 0 sends these immediately.

1. **DNS.** Can you delegate `cstam.felcloud.tn` to us (NS records pointing at our gateways), or create a wildcard record `*.cstam.felcloud.tn` → our floating IP? Could you create one CNAME `_acme-challenge.<hackathon>.cstam.felcloud.tn` → a host we control, for wildcard TLS via Let's Encrypt DNS-01?
2. **Quota.** Is a quota increase possible (instances, security groups)? It would allow a "premium" VM-per-team mode.
3. **Existing resources.** Who owns `sbx-f0699704` and `adam`? May we delete them? Is there an existing CSTAM tool we should build on?
4. **Octavia.** Is `loadbalancer failover` allowed for our role? Is the `ovn` provider's health monitor supported?
5. **VM settings.** Do your images use the `pc` or `q35` machine type, and what is `num_pcie_ports`? (Decides NIC hot-plug capacity.) Is `max_allowed_address_pair` still the default 10?
6. **Floating IPs.** Are floating IPs centralized or distributed in your OVN deployment?

::: learn
**Learning checkpoint**

1. Why does a tiny Neutron allocation pool (.200–.250) make our IPAM *safer*, not weaker?
2. What happens to a 4 KB HTTP response sent from a container with MTU 1500 over a network with MTU 1442, if ICMP is filtered?
3. Why can we not give each team its own security group, and what do we do instead?
4. Which three facts in this chapter changed the original (v1) design, and how?
:::
