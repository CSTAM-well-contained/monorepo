# 4. Architectures we considered

Before committing to a design, we generated eight fundamentally different end-to-end architectures and scored each one against the official points, the FelCloud constraints from chapter 3, and the effort available. This chapter shows that comparison. Its purpose is twofold: to make sure we picked the best design, and to give you ready answers when a judge asks "why not X?".

![Figure 4.1 – The eight architectures compared on runtime, team-IP model and HA layer. Green = chosen, light green = pre-built fallback, amber = viable, red = rejected.](diagrams/f02-options.png)

## 4.1 Two claims we checked first

### Claim 1: "One VM per team does not fit the quota"

Budget arithmetic with the verified quota (10 instances, 20 vCPU, 50 GB) and the two VMs that already exist:

| Item | Instances | vCPU | RAM |
|---|---|---|---|
| Existing (`sbx-f0699704` 1c1g, `adam` 4c4g SHUTOFF, still counted) | 2 | 5 | 5 GB |
| 2 gateways (1c2g) | 2 | 2 | 4 GB |
| Control plane (2c4g) | 1 | 2 | 4 GB |
| **Left for team VMs** | **5** | **11** | **37 GB** |

Even with the smallest flavor (1c1g), only **5 team VMs** fit, or 7 if `adam` and `sbx-f0699704` were deleted. The stress test requires 10+ **simultaneous** registrations, and each VM takes about 40 s to reach ACTIVE on FelCloud (measured on `sbx-f0699704`) before cloud-init even runs. **Verdict: the claim is correct.** VM-per-team cannot pass the stress test on this quota. It only becomes viable with at least 20 instances, and even then it scores lower on "rapid" provisioning (40–90 s against 1–5 s).

But note: the judges probably *think* in VMs. The brief says "VMs/containers", and `sbx-f0699704` looks like FelCloud's own one-VM-per-sandbox tool. The strongest position is to **support both through one driver interface**: containers by default for density and speed, and an optional `vm` driver (one Nova VM through Heat) for a "premium" team, if time allows.

### Claim 2: "A Neutron IP per team on a Docker host is what judges want"

The brief says "isolated student VMs/containers on OpenStack with **dynamically assigned private IPs**" and "**managed OpenStack IP pool** allocation".

- A plain Docker container IP (172.x) is **not** an OpenStack IP. A judge opening the OpenStack dashboard would see nothing. This scores badly on both Provisioning and IPAM.
- Giving each team a **real Neutron port** with an IP from the subnet, named `cstam-team-<id>` and tagged with its lease, does match the brief. The port appears and disappears in the dashboard, and a judge filtering ports by tag sees the IPAM state inside OpenStack.
- The open question is **how the team IP reaches the team's containers** on the host VM. Two models are possible on FelCloud, and both are kept (ADR-13):
  - **pod-NIC (primary):** the team's Neutron port is hot-plugged into the host VM as a real NIC and moved into the team's network namespace. Neutron's security groups then isolate teams natively.
  - **AAP (fallback):** the team's Neutron port stays unbound; its IP is added to the host port's allowed-address-pairs and bound inside the VM. It depends on OVN treating the team port as a *virtual* port.

## 4.2 The eight architectures

For each one: how it covers the scored items, the risk on FelCloud, the performance, the effort and the "wow" for judges.

### A. Docker host per hackathon + Neutron IP per team + HAProxy/keepalived VRRP pair (v1)

- **Provisioning:** Neutron port + allowed-address-pair + host agent builds a bridge, NAT and router container. About 1–3 s per team. A real OpenStack IP per team.
- **HA:** VIP (reserved port) + floating IP + keepalived unicast VRRP. You can kill gw1 live.
- **IPAM:** Postgres state machine over Neutron ports. **Reload/rollback:** `haproxy -c`, hitless reload, health gate. **Stress:** no VM boots, so dozens of teams.
- **Isolation:** bridges + host firewall + host security group. **Teardown:** ordered and reconciled.
- **FelCloud risk:** floating IP on an OVN virtual port; team IP via allowed-address-pairs on OVN; lost updates on the allowed-address-pairs list; MTU.
- **Effort:** medium–high (host agent and NAT are the hardest code). **Wow:** high; the scored work is visibly our own.

### B. One small VM per team (Nova + Neutron port + Heat stack per team), same gateway pair

- Strongest isolation (own kernel, own port, own security group, Neutron QoS per port).
- **Fails the stress test on quota** (5–7 teams, 40–90 s each).
- **Effort:** low–medium. **Wow:** medium ("that's just Nova").
- **Use:** as an optional second sandbox driver, not as the main runtime.

### C. Octavia-centric gateway (amphora Active-Standby flavor, one L7 policy per subdomain)

- HA is real and FelCloud-native, and uses none of our quota.
- Routing: listener HTTP:80, one L7 policy per team (`HOST_NAME EQUAL_TO team5…` → pool → member team-IP:80).
- **Problems:**
  1. Every change puts the load balancer into `PENDING_UPDATE` for about 2–10 s, and concurrent API calls get **409 Conflict**. Ten simultaneous registrations must be serialized: 20–100 s plus retry storms. This hurts the stress test.
  2. Syntax validation and rollback are done by Octavia's API, not by us. Little to show for those 10 points.
  3. **We cannot kill an amphora**: it lives in the service project, and `loadbalancer failover` is admin-only by default. The required "simulated primary gateway crash" demo becomes impossible.
  4. TLS termination needs Barbican, which FelCloud does not offer.
- **Wow:** low–medium. Rejected as the gateway; an Octavia variant is kept as fallback (see E).

### D. k3s / Kubernetes: namespace per team, Ingress controller, MetalLB / kube-vip, NetworkPolicy

- Provisioning and isolation are easy (NetworkPolicy needs Calico or Cilium; flannel does not enforce it).
- **IPAM:** pod IPs are *not* OpenStack IPs. Showing "OpenStack private IPs" needs a `LoadBalancer` service per team with MetalLB announcing 10.20.0.x addresses, which needs allowed-address-pairs on every node port: the same OVN risk as A, plus MetalLB's ARP speaker on OVN.
- **HA:** kube-vip, which is VRRP-like again. The ingress controller hides validation and rollback, so the "our pipeline" story disappears.
- **FelCloud risk:** overlay inside overlay (VXLAN inside Geneve) gives an MTU around 1392; meaningful HA needs 3 nodes; about 1 GB RAM overhead per node.
- **Effort:** high, more moving parts in a live demo. **Wow:** medium. FelCloud runs Rancher, so Kubernetes is not impressive to them; a clean bespoke design shows understanding.

::: why
**The answer to "why not Kubernetes?"**

"Kubernetes solves scheduling, which is not the scored problem. The scored problems are the IP lifecycle on Neutron, gateway HA and configuration safety. Kubernetes would hide two of them inside controllers we did not write, and it would add an overlay that reduces the MTU again on top of Geneve. We did borrow its best idea: our control plane uses level-triggered reconcilers, like Kubernetes controllers."
:::

### E. OVN-native L4 load balancer (Octavia `ovn` provider) active/active in front of two HAProxy gateways

- The OVN load balancer lives in OVN flows on every hypervisor. **There is no VM, no VRRP and no allowed-address-pair for the VIP.** The floating IP goes on the load balancer's VIP; members are gw1:80 and gw2:80 with a TCP health monitor.
- Both gateways carry traffic (active/active). When gw1 dies, the health monitor marks it down after `delay × retries` (about 2–3 s); connections to gw1 are lost.
- **Pros:** removes the riskiest network behavior; uses "Octavia", which the brief names.
- **Cons:** failover timing is controlled by OVN, not by us; the dashboard's "Primary vs Standby" and "manual failover" wording fits less well; health-monitor support depends on the OVN provider version (must be tested).
- **Effort:** low. **Wow:** medium–high when combined with A. **Kept as pre-built fallback F2.**

### F. A, but with a hot-plugged Neutron NIC per team instead of allowed-address-pairs

- Each team gets a real *bound* port with its own MAC on the host VM. The host agent moves that NIC into the team's network namespace. Neutron security groups enforce team-to-team isolation natively ("Neutron Security Groups" is literally in the scoring), OVN port security prevents spoofing, and no NAT is needed.
- **Original concerns:** Nova serializes interface attaches per instance (about 2–8 s each), so ten teams on one host would take 30–80 s; PCI hot-plug slots are limited (about 25 on the `pc` machine type, possibly very few on `q35`); cloud-init or netplan may try DHCP on new NICs.
- **What changed the verdict:** a **warm pool**. The control plane pre-creates and pre-attaches a few ports per host when a hackathon is created (and refills in the background). A registration then only renames and re-tags a warm port and plumbs the network namespace: about 1 s, no Nova call on the hot path. The slot limit is measured in experiment H1, and netplan only matches the primary NIC by default.
- **Effort:** medium. **Risk:** medium (decided by H1). **Wow:** high: "each team has its own Neutron port and security group; isolation is enforced by OpenStack itself."

### G. Traefik + Consul (service catalog drives routing)

- Elegant service discovery and built-in Let's Encrypt.
- Traefik has **no real "validate before apply"**: it logs and skips bad dynamic config, so the rollback item is hard to demonstrate. Consul needs 3 servers for HA (quota), and Traefik's own HA still needs VRRP or Octavia.
- **Rejected.**

### H. BGP/ECMP active/active, or an eBPF/XDP gateway

- **BGP:** FelCloud has no Neutron BGP dynamic routing and tenants cannot peer with provider routers. Not possible.
- **XDP:** routing by subdomain needs the HTTP `Host` header or TLS SNI, which means L7 and reassembled TCP streams. XDP sees single packets. Not feasible for L7 routing. eBPF is only useful for isolation filters or SYN-flood protection, and Heat earns the same bonus more cheaply.
- **Rejected** as an architecture.

## 4.3 Weighted decision matrix

Weights follow the official points, plus feasibility and effort. Scores run from 1 (bad) to 5 (excellent). **Weighted score = Σ(weight × score) / 5**, maximum 100.

| Criterion (weight) | A v1 (AAP) | B VM/team | C Octavia L7 | D k3s | E OVN-LB A/A + A | F pod-NIC (no warm pool) | **F+ pod-NIC + warm pool** | G Traefik+Consul | H BGP/XDP |
|---|---|---|---|---|---|---|---|---|---|
| Provisioning fidelity (15) | 4 | 5 | 3 | 3 | 4 | 5 | **5** | 3 | 1 |
| HA gateway, live-killable (15) | 5 | 5 | 2 | 4 | 4 | 5 | **5** | 3 | 1 |
| IPAM on OpenStack (10) | 5 | 5 | 4 | 2 | 5 | 5 | **5** | 3 | 2 |
| Reload/rollback as our own work (10) | 5 | 5 | 1 | 2 | 5 | 5 | **5** | 1 | 1 |
| Stress 10+ within quota (15) | 5 | **1** | 2 | 5 | 5 | 3 | **5** | 4 | 1 |
| Teardown / lifecycle (10) | 5 | 5 | 4 | 4 | 5 | 5 | **5** | 4 | 2 |
| Isolation (5) | 4 | 5 | 4 | 5 | 4 | 5 | **5** | 3 | 3 |
| FelCloud feasibility risk (10) | 3 | 4 | 3 | 3 | 3 | 2 | **2** | 4 | 1 |
| Effort / time to working (10) | 3 | 4 | 4 | 2 | 4 | 3 | **3** | 3 | 1 |
| **Weighted score / 100** | **88** | 81 | 55 | 67 | 87 | 84 | **90** | 63 | 25 |

How to read it:

- **F+ (pod-NIC with a warm pool) scores highest**, but carries the most uncertainty on FelCloud (hot-plug slot limits). **A (allowed-address-pairs)** is two points behind and shares about 90 % of the code: same gateways, same control plane, same IPAM. Only the host agent's network driver differs. So we build **one system with a pluggable network driver**, and gate G1 (Step 1) picks the driver from measured facts.
- **E** ties with A on score; the difference is only the HA mechanism. VRRP matches the brief's wording ("Active/Standby", "manual failover", "primary crash") and shows our own work. **E stays the pre-planned fallback** if VRRP on OVN does not work.
- **B**'s fidelity is cancelled by the quota, a gating failure on a 15-point item. Optional second driver only.

::: why
**The winner: "A+"**

The v1 family (Docker hosts, a real Neutron IP per team, HAProxy and keepalived), with:

- **pod-NIC** team networking as primary and **AAP** as fallback, chosen at gate G1;
- an **OVN load balancer (E)** and a **floating-IP-moving notify script** as pre-built HA fallbacks;
- the fixes found by querying FelCloud: IPAM range outside Neutron's pool, MTU 1442, compare-and-swap on port updates, generation-tracked gateway configuration;
- an optional **VM driver** (B, through Heat) if time allows.
:::

## 4.4 Active/Standby or Active/Active?

### What active/active could look like on FelCloud

| Option | How | Failover | Verdict |
|---|---|---|---|
| Two floating IPs + two DNS A records (round robin) | Browsers try the next A record after a connection failure | Fast if the port is refused, **20+ s** if packets are dropped (a dead VM drops them); resolver caches add more. Needs DNS control we do not have. | Rejected: not "instant" |
| BGP/ECMP anycast | – | – | Not available to tenants |
| OVN load balancer VIP with both gateways as members | Octavia `ovn` provider + health monitor | `delay × retries` ≈ 2–3 s, not controlled by us | The only real A/A option; kept as fallback F2 |
| Two VIPs with VRRP in both directions + two floating IPs + DNS round robin | gw1 master of VIP-a, gw2 master of VIP-b | VRRP speed per VIP | Doubles complexity for capacity we do not need |

### Is it worth it?

**No, not as the main mode.** Capacity is not the bottleneck: one HAProxy on one vCPU handles well over 10,000 requests per second, and hackathon traffic is perhaps 100. The brief's dashboard wording ("Primary vs Standby", "manual gateway failover") fits Active/Standby. VRRP Active/Standby is the textbook answer, and FelCloud engineers can verify it by reading `keepalived.conf`.

::: why
**One sentence for the pitch**

"We chose Active/Standby on purpose. Active/Active doubles the failure modes (split traffic, shared state, DNS) to add capacity we measured we do not need. Our standby is hot: same configuration generation, already listening, and it takes over in under two seconds."
:::

::: learn
**Learning checkpoint**

1. Do the quota arithmetic for VM-per-team yourself, including `adam`. How many team VMs fit?
2. Why does Octavia L7 hurt the stress test? Name the HTTP status code involved.
3. What does the warm pool change in architecture F, and what does it *not* change (which limit remains)?
4. Explain to a Kubernetes fan, in three sentences, why we did not use k3s.
5. Why is two floating IPs plus round-robin DNS not "instant failover"?
:::
