# 14. Step 5 – Team networking on the host and the host agent

## Step 5 – Give each team its own Neutron IP and network namespace, then automate it with `hostagent`

::: note
**Goal:** on `cstam-host-a`, a team has its own Neutron port (a real OpenStack IP from our pool), plugged into its own Linux network namespace, with its containers inside it, reachable **only** from the gateways, unable to reach other teams, the metadata service or the management network. First you build team1 by hand, then the Go `hostagent` does the same thing from the control plane's desired state, idempotently, and repairs it after a container restart or a reboot.
**Points served:** automated sandbox provisioning (15), network isolation (5), IPAM on real Neutron IPs (10), stress test (15, through the warm pool).
**Time estimate:** 3–4 days (1 by hand, 2–3 for the agent and its tests).
**Depends on:** Step 2 (host VM, security groups `cstam-host` and `cstam-team`), Gate G1 (which network model: **pod-NIC** if experiment H1 passed, **AAP** fallback otherwise). The control plane side (who creates and attaches the ports) is Step 7; until then you create ports by hand.
:::

### Understand first

#### What a "team sandbox" is on a host

A team gets three things on the hackathon host:

1. **An IP address that OpenStack knows about.** Not a Docker-internal `172.x` address, but a Neutron port with a fixed IP from our pool (`10.20.0.32`–`.191`). A judge opening Skyline sees the port, its name, its tags.
2. **An isolated network.** Packets to and from that IP pass through Neutron's security groups and through a small firewall that only the platform controls.
3. **Containers** that run the team's apps and share that network.

#### The pod-NIC model (primary)

![Figure 14.1 – Pod-NIC: each team owns a real Neutron port, hot-plugged into the host VM and moved into the team's own network namespace](diagrams/f09-pod-nic.png)

Step by step:

1. The control plane creates a Neutron port with the team's fixed IP and the security group `cstam-team`, and **attaches it to the host VM** (Nova "interface attach", like `openstack server add port`). Inside the VM, a new network card appears (for example `ens4`) with the port's MAC address.
2. The host agent starts a **pause container** for the team (`registry.k8s.io/pause:3.10`, `--network none`). It does nothing except hold a **network namespace** (netns): a private copy of the Linux network stack (its own interfaces, routes, firewall).
3. The agent finds the new NIC **by its MAC address** (names like `ens4` are not stable; the MAC is), moves it into the pause container's namespace, renames it `eth0`, sets MTU 1442, adds `10.20.0.32/24` and a default route via `10.20.0.1`.
4. It loads an **nftables** ruleset inside that namespace: input only from the two gateways on ports 80 and 8000–8099; output never to the metadata service, the management network or private ranges.
5. The team's app containers are started with `--network container:team1-pod`: they share the namespace, so an app listening on `:80` is directly reachable at `10.20.0.32:80`. No NAT, no bridge, no port mapping.

Why this is strong:

- **Neutron itself isolates teams.** `cstam-team` allows ingress only from members of `cstam-gw`. A packet from team1's NIC to team2's NIC leaves team1's port, enters OVN, and is dropped at team2's port, because no rule allows a `cstam-team` source. The host kernel never even routes between them, because they are in different namespaces.
- **Port security per team.** OVN only accepts packets from team1's NIC with team1's MAC and IP. A container cannot spoof another team's address; it is enforced outside the VM.
- **No allowed-address-pairs.** No 10-pair limit, no lost-update race on the host port, no virtual-port binding by gratuitous ARP. The team port is a normal, bound port.

The cost is Nova's interface attach: a few seconds each, and serialized per VM. The **warm pool** hides it: the control plane keeps a few ports already attached to each host (IP state `WARM`), unconfigured in the root namespace. A registration takes a warm NIC, so the slow attach is already done (Step 7).

::: why
**Why a pause container instead of `ip netns add team1`?**
A namespace created with `ip netns add` is not something Docker can attach containers to. A namespace owned by a container is: `--network container:team1-pod` is a documented Docker feature, and it is exactly what Kubernetes does with its "pod sandbox". The pause container also makes the namespace's life visible: `docker ps` shows one pod per team, and if it dies, Docker events tell the agent.
:::

#### The health placeholder

A team that just registered has not deployed anything yet, but the control plane needs to check the plumbing (gateway → team IP → namespace) before marking it ACTIVE (Step 7). So the agent starts a tiny **placeholder** container in the team's namespace: `busybox httpd` on port **8099**, serving `/.cstam/health`. Port 8099 is reserved for it, so it never collides with the team's apps (port 80 and 8000–8098) and it can stay for the whole life of the team. Until the team deploys an app on port 80, visitors get the gateway's maintenance page (503).

#### The AAP model (fallback)

If Gate G1 showed that hot-plug does not work on FelCloud (for example a q35 machine type with no free PCIe slots), the fallback is the improved v1 design:

- a reserved Neutron port (no device) holds the team IP;
- the IP is added to the host port's **allowed-address-pairs** by the control plane's per-host syncer (one writer, `If-Match` revision, Step 7);
- the agent binds the IP on `ens3` as a `/32` and sends a gratuitous ARP so OVN binds the virtual port to this host;
- a Docker bridge per team (MTU 1442), Docker publishes `10.20.0.32:80` → the app (DNAT), and traffic leaving the team is SNATed to the team IP;
- host firewall chains (`CSTAM-*` jumped from `DOCKER-USER`) block team-to-team, metadata and management traffic.

Both models sit behind one Go interface in the agent, so the control plane and the rest of the agent do not care which one runs.

| | **pod-NIC (primary)** | AAP (fallback) |
|---|---|---|
| Team IP is | a bound Neutron port, own NIC | a reserved port + allowed pair on the host port |
| Team-to-team isolation | Neutron SG + nft in netns | host iptables only |
| Anti-spoofing | OVN port security per team | host-wide (`rp_filter`, iptables) |
| NAT | none | DNAT + SNAT |
| Limits | free hot-plug slots per VM (H1) | `max_allowed_address_pair` (default 10) |
| Provisioning cost | attach 2–6 s, hidden by the warm pool | AAP update (coalesced) + GARP |

### Do it by hand

All commands on `cstam-host-a` unless written otherwise. Neutron commands run from your laptop (you run them; Claude Code only prints them).

#### 5.1 Prepare the host VM

Install Docker from Docker's own repository (the Ubuntu `docker.io` package also works):

```bash
ssh cstam-host-a
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker ubuntu        # for your own debugging only; the agent runs as root
```

`/etc/docker/daemon.json`:

```json
{
  "mtu": 1442,
  "bridge": "none",
  "live-restore": true,
  "iptables": true,
  "ip-forward": true,
  "userland-proxy": false,
  "no-new-privileges": true,
  "exec-opts": ["native.cgroupdriver=systemd"],
  "log-driver": "json-file",
  "log-opts": { "max-size": "10m", "max-file": "3" },
  "default-address-pools": [{ "base": "172.20.0.0/16", "size": 24 }]
}
```

| Option | Why |
|---|---|
| `mtu: 1442` | the OVN network MTU is 1442 (Geneve overhead); a 1500-byte packet from a container would be dropped and large downloads would hang |
| `bridge: none` | no `docker0` default bridge, so no container can end up on an unfiltered network by accident |
| `live-restore: true` | restarting or upgrading `dockerd` does not kill team containers |
| `no-new-privileges` | default for every container: setuid binaries cannot gain privileges |
| `default-address-pools` | only used by the AAP fallback bridges (`cst-tNNNN`, one `/24` each) |

::: note
**userns-remap.** Step 9 discusses `"userns-remap": "default"` (container root mapped to an unprivileged host user). It works with `--network container:` and with moving a NIC into the pause container's namespace, but it changes image storage and file ownership. Leave it **off** in this step, get the plumbing working, and turn it on in Step 9 with the security tests running.
:::

```bash
sudo systemctl restart docker && docker info | grep -E 'Live Restore|Cgroup Driver'
sudo docker pull registry.k8s.io/pause:3.10
sudo docker pull busybox:1.36            # health placeholder
sudo apt-get install -y nftables iproute2 iputils-arping conntrack jq
```

Kernel settings (`/etc/sysctl.d/90-cstam-host.conf`):

```conf
net.ipv4.conf.all.rp_filter = 1
net.ipv4.conf.default.rp_filter = 1
net.ipv4.conf.all.accept_redirects = 0
net.ipv4.conf.all.send_redirects = 0
net.ipv6.conf.all.disable_ipv6 = 1
net.ipv4.conf.all.arp_ignore = 1
net.ipv4.conf.all.arp_announce = 2
```

`sudo sysctl --system`. `arp_ignore=1` and `arp_announce=2` make each interface answer ARP only for its own addresses, which matters when a warm NIC sits unconfigured in the root namespace.

**Hot-plugged NICs must not get DHCP.** Ubuntu's cloud-init writes a netplan file that matches only the first NIC's MAC, so a new NIC is left alone. Check it once:

```bash
cat /etc/netplan/50-cloud-init.yaml      # should contain "match: macaddress: <ens3 MAC>" and nothing generic
networkctl list                          # after an attach: the new NIC is "unmanaged" or "off", never "configured"
```

If the netplan file matches by driver or by name pattern (`en*`), a hot-plugged NIC would get a second DHCP address in the root namespace. Fix the file to match the primary MAC only, then `sudo netplan apply`.

Host firewall for the root namespace. Teams never pass through it in the pod-NIC model; it only protects the host itself. It is its own nft table (never `flush ruleset`, which would delete Docker's tables):

```conf
# /etc/nftables.d/cstam-host.nft
table inet cstam_host {
  chain input {
    type filter hook input priority -10; policy drop;
    ct state established,related accept
    ct state invalid drop
    iif lo accept
    ip saddr 10.20.0.5 tcp dport 22 accept                 # SSH only from the control plane (bastion)
    ip saddr 10.20.0.5 icmp type echo-request accept
    udp sport 67 udp dport 68 accept                       # DHCP renewals for ens3
    icmp type { destination-unreachable, time-exceeded } accept
  }
}
```

```bash
sudo nft -c -f /etc/nftables.d/cstam-host.nft && sudo nft -f /etc/nftables.d/cstam-host.nft
sudo nft list table inet cstam_host
```

The host agent has **no listening port** (it dials out to the control plane), so only SSH from the control plane is needed. Persist the table with a systemd oneshot unit `After=docker.service` that runs the same `nft -f` (replace = `nft delete table inet cstam_host` then load).

#### 5.2 Team1 by hand (pod-NIC)

Use a lab IP (`10.20.0.193`) for the hand test, so the team pool stays clean for the IPAM.

**On your laptop** (you run these):

```bash
export OS_CLOUD=cstam
PORT_ID=$(openstack port create --network cstam-sandbox-net \
  --fixed-ip subnet=cstam-sandbox-subnet,ip-address=10.20.0.193 \
  --security-group cstam-team --tag cstam --tag cstam-lab \
  --description "cstam lab team1 (step 5 by hand)" -f value -c id cstam-lab-team1)
MAC=$(openstack port show $PORT_ID -f value -c mac_address); echo $MAC
time openstack server add port cstam-host-a $PORT_ID
until [ "$(openstack port show $PORT_ID -f value -c status)" = ACTIVE ]; do sleep 0.5; done; echo ACTIVE
```

**On the host**:

```bash
MAC=fa:16:3e:xx:xx:xx                                   # paste from above
IF=$(ip -o link | awk -v m="$MAC" '$0 ~ m {sub(":","",$2); print $2}'); echo "NIC is $IF"

# 1) the pod: holds the namespace, restarts with Docker
sudo docker run -d --name team1-pod --network none --restart unless-stopped \
  --label cstam.team=team1 --label cstam.role=pod registry.k8s.io/pause:3.10
PID=$(sudo docker inspect -f '{{.State.Pid}}' team1-pod)
sudo ip netns attach team1 "$PID"                       # /run/netns/team1 -> the pod's namespace (for ip -n)

# 2) move the NIC into the pod and configure it
sudo ip link set dev "$IF" netns team1
sudo ip -n team1 link set dev "$IF" name eth0
sudo ip -n team1 link set dev eth0 mtu 1442 up
sudo ip -n team1 addr add 10.20.0.193/24 dev eth0
sudo ip -n team1 route add default via 10.20.0.1
sudo ip -n team1 link set lo up
sudo ip -n team1 -br addr                               # lo UNKNOWN 127.0.0.1/8 ; eth0 UP 10.20.0.193/24
```

The firewall inside the namespace (`/etc/cstam/pod.nft`, rendered per team by the agent later):

```conf
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
    ip daddr 169.254.0.0/16 drop                     # metadata: would return the HOST VM's metadata
    ip daddr 10.20.0.0/24 drop                       # gateways, hosts, control plane, other teams
    ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10 } drop
  }
}
```

```bash
sudo ip netns exec team1 nft -c -f /etc/cstam/pod.nft && sudo ip netns exec team1 nft -f /etc/cstam/pod.nft
sudo ip netns exec team1 nft list ruleset
```

Note `10.20.0.0/24 drop` in output: replies to the gateways still work because they match `ct state established` first. Only connections the team *starts* towards the subnet are dropped. Outbound Internet works through the router (`10.20.0.1` is the next hop, not the destination).

::: warn
**Why the metadata block is not optional.** On OVN, the metadata service identifies the requester by source port. A request from team1's NIC, attached to the host VM, returns **the host VM's** metadata (user-data, SSH keys). Neutron cannot block it (it is served inside OVN), so the netns firewall is the only layer. Never put secrets in user-data anyway.
:::

Health placeholder and a test app:

```bash
# placeholder: /.cstam/health (and a welcome page) on :8099; the team's own apps use :80 and 8000-8098
sudo docker run -d --name team1-placeholder --network container:team1-pod --restart unless-stopped \
  --label cstam.team=team1 --label cstam.role=placeholder \
  --cap-drop ALL --read-only --tmpfs /www:size=1m --pids-limit 32 --memory 32m \
  busybox:1.36 sh -c 'mkdir -p /www/.cstam && echo ok > /www/.cstam/health && echo "<h1>team1: nothing deployed yet</h1>" > /www/index.html && exec httpd -f -p 8099 -h /www'
```

The placeholder listens on **8099** so it never collides with a team app on 80; the control plane's health step (Step 7) asks the master gateway agent to probe `<team-ip>:8099/.cstam/health` from the gateway, which is exactly the network path HAProxy uses.

A team app (hardened, as every team app will be):

```bash
sudo docker run -d --name team1-web --network container:team1-pod --restart unless-stopped \
  --label cstam.team=team1 --label cstam.app=web \
  --cap-drop ALL --cap-add NET_BIND_SERVICE --security-opt no-new-privileges \
  --read-only --tmpfs /tmp:size=64m --pids-limit 256 --memory 512m --cpus 1 \
  traefik/whoami:v1.10 --port 80
```

**Checks:**

```bash
# from gw1 (allowed: source is cstam-gw)
ssh cstam-gw1 'curl -s -m 2 10.20.0.193/ | head -3'                     # whoami output, Hostname: ...

# from the control plane (not a gateway): dropped by Neutron SG and by the pod firewall
ssh cstam-cp 'curl -s -m 2 -o /dev/null -w "%{http_code}\n" 10.20.0.193/ || echo blocked'   # blocked

# from inside the team: metadata, management and the gateway admin side are blocked
sudo ip netns exec team1 curl -s -m 2 http://169.254.169.254/ || echo "metadata blocked"
sudo ip netns exec team1 curl -s -m 2 http://10.20.0.5/ || echo "cp blocked"
sudo ip netns exec team1 curl -s -m 5 -o /dev/null -w '%{http_code}\n' https://example.com/   # 200: Internet works

# MTU: a large download completes
sudo ip netns exec team1 curl -s -o /dev/null -w '%{size_download}\n' https://speed.hetzner.de/10MB.bin
```

Then route it through the gateways with the runtime commands of Step 3 (or the fake control plane of Step 4): `team1.hacka.cstam.felcloud.tn → be_slot001 → 10.20.0.193:80`, and open it from the laptop through the floating IP.

**Second team, to prove isolation.** Repeat with `10.20.0.194` as `team2`, then:

```bash
sudo docker exec team1-web wget -qO- -T 2 http://10.20.0.194/ || echo "team1 -> team2 blocked"
```

The connection fails twice over: team1's own output rule drops `10.20.0.0/24`, and even with that rule removed (try it), Neutron drops the packet at team2's port because `cstam-team` only admits `cstam-gw` sources.

#### 5.3 Teardown by hand (the host part of the strict order)

The order is fixed by Step 8: route removed from both gateways first, then the host, then the NIC is detached, then the port is deleted.

```bash
# host
sudo docker rm -f team1-web team1-placeholder
sudo docker rm -f team1-pod                       # the namespace dies; the NIC returns to the root namespace
sudo ip netns delete team1 2>/dev/null || true    # removes the /run/netns/team1 bind mount
ip -o link | grep -i "$MAC"                       # the NIC is back in the root netns, name ethX/ensX, DOWN, no IP
```

```bash
# laptop
openstack server remove port cstam-host-a $PORT_ID
until [ -z "$(openstack port show $PORT_ID -f value -c device_id)" ]; do sleep 0.5; done
openstack port delete $PORT_ID
```

No conntrack cleanup is needed in this model: the team's connections lived in the team's namespace, which no longer exists.

#### 5.4 What happens on restarts

Try both, because the agent must handle them:

```bash
sudo docker restart team1-pod
sudo ip -n team1 -br addr 2>&1 | head -2          # the old netns is gone; the NIC is back in the root netns
ip -o link | grep -i "$MAC"
```

When the pause container restarts, Docker creates a **new** namespace. The NIC returns to the root namespace, unconfigured, and the app containers (joined to the old namespace) lose their network. After a VM reboot it is the same for every team. The agent therefore watches Docker events for every `cstam.role=pod` container and re-runs the whole plumbing for that team (it is idempotent), then restarts the team's app containers so they join the new namespace.

#### 5.5 The AAP fallback by hand (only if Gate G1 chose it)

```bash
# laptop: reserved port (no device) + allowed pair on the host port
openstack port create --network cstam-sandbox-net \
  --fixed-ip subnet=cstam-sandbox-subnet,ip-address=10.20.0.193 \
  --security-group cstam-team --tag cstam --tag cstam-lab cstam-lab-team1-reserved
openstack port set cstam-host-a-port --allowed-address ip-address=10.20.0.193
# (the control plane does this with If-Match revision numbers; by hand, never run two at once)
```

```bash
# host: bind the IP and announce it (OVN binds the virtual port from ARP)
sudo ip addr add 10.20.0.193/32 dev ens3
sudo arping -U -c 3 -I ens3 10.20.0.193

# team bridge (MTU 1442) and app with Docker's DNAT on the team IP
sudo docker network create -d bridge --subnet 172.20.1.0/24 \
  -o com.docker.network.bridge.name=cst-t0001 \
  -o com.docker.network.driver.mtu=1442 \
  -o com.docker.network.bridge.enable_icc=true team1-net
sudo docker run -d --name team1-web --network team1-net -p 10.20.0.193:80:80 \
  --cap-drop ALL --cap-add NET_BIND_SERVICE --security-opt no-new-privileges --read-only \
  --pids-limit 256 --memory 512m traefik/whoami:v1.10 --port 80

# outbound traffic of the team leaves with the team IP
sudo iptables -t nat -I POSTROUTING -s 172.20.1.0/24 ! -d 172.20.1.0/24 -j SNAT --to-source 10.20.0.193
```

Isolation in this model is the host firewall: our own chains `CSTAM-FWD`, `CSTAM-TEAM`, `CSTAM-ANTISPOOF`, `CSTAM-XTEAM`, `CSTAM-IN`, `CSTAM-HOST-IN`, jumped from `DOCKER-USER` and `INPUT`, rendered per host and applied atomically with `iptables-restore -w --noflush --test` then `iptables-restore -w --noflush`. The full rules text and the reasoning for each line are in Chapter 18 (Step 9). Teardown adds `conntrack -D -d 10.20.0.193` and `ip addr del 10.20.0.193/32 dev ens3`, and the control plane removes the allowed pair **before** deleting the reserved port.

### Build it in Go

#### 5.6 Package layout

```text
cmd/hostagent/main.go                  # wiring, config, systemd notify
internal/host/agent.go                 # reconcile loop: desired HostDesired -> drivers, ack, report
internal/host/netdriver/driver.go      # NetDriver interface + TeamSpec/TeamStatus types
internal/host/netdriver/podnic/        # pod-NIC driver (netlink, netns, nft in netns)
internal/host/netdriver/aap/           # AAP driver (addr /32, arping, bridge, iptables chains)
internal/host/docker/                  # moby client wrappers: pods, apps, placeholder, events
internal/host/firewall/                # nft and iptables-restore render/check/apply
internal/host/state/                   # last desired state on disk (static stability)
```

`hostagent` never imports gophercloud (depguard rule from Step 0): it never talks to OpenStack. It only acts on what it finds on the VM.

#### 5.7 Configuration

```go
// cmd/hostagent/config.go
type Config struct {
	Name       string   `env:"CSTAM_AGENT_NAME,required"`   // host-a
	CPURL      string   `env:"CSTAM_CP_URL,required"`       // https://10.20.0.5:8443
	TLSCert    string   `env:"CSTAM_TLS_CERT,required"`
	TLSKey     string   `env:"CSTAM_TLS_KEY,required"`
	TLSCA      string   `env:"CSTAM_TLS_CA,required"`
	NetDriver  string   `env:"CSTAM_NET_DRIVER" envDefault:"podnic"` // podnic | aap (decided at G1)
	PauseImage string   `env:"CSTAM_PAUSE_IMAGE" envDefault:"registry.k8s.io/pause:3.10"`
	HelperImg  string   `env:"CSTAM_PLACEHOLDER_IMAGE" envDefault:"busybox:1.36"`
	MTU        int      `env:"CSTAM_MTU" envDefault:"1442"`
	Gateways   []string `env:"CSTAM_GATEWAYS" envSeparator:","`           // 10.20.0.11,10.20.0.12
	Router     string   `env:"CSTAM_ROUTER_IP" envDefault:"10.20.0.1"`
	PrefixLen  int      `env:"CSTAM_PREFIX_LEN" envDefault:"24"`
	StateDir   string   `env:"CSTAM_STATE_DIR" envDefault:"/var/lib/cstam"`
	Workers    int      `env:"CSTAM_HOST_WORKERS" envDefault:"4"`        // teams plumbed in parallel
}
```

#### 5.8 The interface both drivers implement

```go
// internal/host/netdriver/driver.go
package netdriver

type App struct {
	Name   string  // ^[a-z0-9-]{1,32}$
	Image  string  // digest or tag from an allowed registry (Step 9)
	Port   int     // 80 or 8000..8098 (8099 is the placeholder)
	CPU    float64
	MemMiB int
}

type TeamSpec struct {
	TeamID  string // uuid
	Slug    string // team1
	IP      netip.Addr
	MAC     net.HardwareAddr // pod-NIC only
	NetMode string           // PODNIC | AAP
	Apps    []App
}

type TeamStatus struct {
	TeamID string
	Ready  bool   // plumbing done and placeholder/apps running
	Error  string // last error, empty if fine
}

// NetDriver makes the host match one team's spec. Every method is idempotent:
// calling Ensure twice changes nothing the second time; Remove of a missing team returns nil.
type NetDriver interface {
	Ensure(ctx context.Context, spec TeamSpec) (TeamStatus, error)
	Remove(ctx context.Context, teamID string) error
	List(ctx context.Context) ([]TeamStatus, error) // teams this host actually has (from Docker labels)
}
```

The proto messages of Step 7 (`HostDesired`, `TeamSpec`, `App`, `AckRequest.teams`) map one-to-one onto these types.

#### 5.9 The pod-NIC driver

Finding a NIC by MAC and moving it into the pod's namespace, with `github.com/vishvananda/netlink` and `github.com/vishvananda/netns`:

```go
// internal/host/netdriver/podnic/plumb.go
package podnic

// linkByMAC searches the root namespace for an interface with this MAC.
func linkByMAC(mac net.HardwareAddr) (netlink.Link, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		if bytes.Equal(l.Attrs().HardwareAddr, mac) {
			return l, nil
		}
	}
	return nil, ErrNICNotFound // not attached yet, or already inside a team namespace
}

// plumb moves the NIC into the pod's namespace (if needed) and configures it. Safe to repeat.
func (d *Driver) plumb(spec netdriver.TeamSpec, podPID int) error {
	target, err := netns.GetFromPid(podPID)
	if err != nil {
		return fmt.Errorf("netns of pod: %w", err)
	}
	defer target.Close()

	// 1. move, if the NIC is still in the root namespace
	if l, err := linkByMAC(spec.MAC); err == nil {
		if err := netlink.LinkSetDown(l); err != nil {
			return err
		}
		if err := netlink.LinkSetNsFd(l, int(target)); err != nil {
			return fmt.Errorf("move %s: %w", l.Attrs().Name, err)
		}
	} else if !errors.Is(err, ErrNICNotFound) {
		return err
	}

	// 2. configure inside the namespace with a handle bound to it (no thread switching needed)
	h, err := netlink.NewHandleAt(target)
	if err != nil {
		return err
	}
	defer h.Close()
	l, err := linkByMACIn(h, spec.MAC)
	if err != nil {
		return fmt.Errorf("NIC %s not in root nor in pod netns: %w", spec.MAC, err)
	}
	if l.Attrs().Name != "eth0" {
		if err := h.LinkSetName(l, "eth0"); err != nil {
			return err
		}
	}
	if err := h.LinkSetMTU(l, d.cfg.MTU); err != nil {
		return err
	}
	want := &netlink.Addr{IPNet: &net.IPNet{IP: spec.IP.AsSlice(), Mask: net.CIDRMask(d.cfg.PrefixLen, 32)}}
	if err := h.AddrReplace(l, want); err != nil { // replace = idempotent
		return err
	}
	if err := h.LinkSetUp(l); err != nil {
		return err
	}
	if lo, err := h.LinkByName("lo"); err == nil {
		_ = h.LinkSetUp(lo)
	}
	gw := net.ParseIP(d.cfg.Router)
	return h.RouteReplace(&netlink.Route{LinkIndex: l.Attrs().Index, Gw: gw, Dst: nil}) // default route
}
```

`AddrReplace` and `RouteReplace` are what makes `Ensure` idempotent: they create or overwrite, they never fail because the thing already exists.

The firewall inside the namespace: the agent renders `pod.nft` from a template with only typed values (the two gateway IPs, port ranges), checks it, then applies it. `nft` must run inside the namespace; `nsenter` does that without the agent switching its own threads:

```go
// internal/host/firewall/nft.go
func ApplyInNetns(ctx context.Context, podPID int, rules []byte) error {
	f, err := os.CreateTemp("", "pod-*.nft")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(rules); err != nil {
		return err
	}
	f.Close()
	pid := strconv.Itoa(podPID)
	// the file replaces the whole table atomically: "table inet pod" is deleted and re-created in one transaction
	for _, args := range [][]string{
		{"-t", pid, "-n", "nft", "-c", "-f", f.Name()}, // gate: syntax and semantic check
		{"-t", pid, "-n", "nft", "-f", f.Name()},
	} {
		out, err := exec.CommandContext(ctx, "nsenter", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("nsenter %v: %v: %s", args, err, out)
		}
	}
	return nil
}
```

To make "replace the whole table" atomic and idempotent, the rendered file starts with:

```conf
table inet pod
delete table inet pod
table inet pod {
  ...
}
```

The first line creates the table if it is missing (so the delete never fails), the second removes it, the third block re-creates it, all in one `nft -f` transaction.

The Docker side, with the moby client (`github.com/moby/moby/client`; the type packages may differ by version, so check the import paths of the version you pin):

```go
// internal/host/docker/pod.go
func (d *Docker) EnsurePod(ctx context.Context, slug, teamID string) (pid int, err error) {
	name := slug + "-pod"
	if c, err := d.inspect(ctx, name); err == nil && c.State.Running {
		return c.State.Pid, nil
	}
	_ = d.remove(ctx, name) // exited or half-created pod: start clean
	_, err = d.cli.ContainerCreate(ctx,
		&container.Config{Image: d.pauseImage, Labels: map[string]string{"cstam.team": teamID, "cstam.slug": slug, "cstam.role": "pod"}},
		&container.HostConfig{NetworkMode: "none", RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
			ReadonlyRootfs: true, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"}},
		nil, nil, name)
	if err != nil {
		return 0, err
	}
	if err := d.cli.ContainerStart(ctx, name, container.StartOptions{}); err != nil {
		return 0, err
	}
	c, err := d.inspect(ctx, name)
	if err != nil {
		return 0, err
	}
	return c.State.Pid, nil
}

// EnsureApp starts an app joined to the pod's namespace with the fixed hardening set.
func (d *Docker) EnsureApp(ctx context.Context, slug, teamID string, a netdriver.App) error {
	name := slug + "-" + a.Name
	if c, err := d.inspect(ctx, name); err == nil && c.State.Running && c.Config.Image == a.Image &&
		c.HostConfig.NetworkMode == container.NetworkMode("container:"+slug+"-pod") {
		return nil // already as desired
	}
	_ = d.remove(ctx, name)
	hc := &container.HostConfig{
		NetworkMode:    container.NetworkMode("container:" + slug + "-pod"),
		RestartPolicy:  container.RestartPolicy{Name: "unless-stopped"},
		CapDrop:        []string{"ALL"},
		CapAdd:         []string{"NET_BIND_SERVICE"},
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
		Tmpfs:          map[string]string{"/tmp": "rw,nosuid,nodev,noexec,size=64m"},
		Resources: container.Resources{
			Memory: int64(a.MemMiB) << 20, MemorySwap: int64(a.MemMiB) << 20,
			NanoCPUs: int64(a.CPU * 1e9), PidsLimit: ptr(int64(256)),
			CgroupParent: "cstam-" + slug + ".slice",
		},
	}
	if _, err := d.cli.ContainerCreate(ctx, &container.Config{Image: a.Image,
		Labels: map[string]string{"cstam.team": teamID, "cstam.slug": slug, "cstam.role": "app", "cstam.app": a.Name}},
		hc, nil, nil, name); err != nil {
		return err
	}
	return d.cli.ContainerStart(ctx, name, container.StartOptions{})
}
```

The driver's `Ensure` puts the pieces in order:

```go
// internal/host/netdriver/podnic/driver.go
func (d *Driver) Ensure(ctx context.Context, s netdriver.TeamSpec) (netdriver.TeamStatus, error) {
	st := netdriver.TeamStatus{TeamID: s.TeamID}
	pid, err := d.docker.EnsurePod(ctx, s.Slug, s.TeamID)
	if err == nil {
		err = d.plumb(s, pid)
	}
	if err == nil {
		err = firewall.ApplyInNetns(ctx, pid, firewall.RenderPod(d.cfg.Gateways))
	}
	if err == nil {
		err = d.docker.EnsurePlaceholder(ctx, s.Slug, s.TeamID) // busybox httpd on :8099
	}
	for _, a := range s.Apps {
		if err != nil {
			break
		}
		err = d.docker.EnsureApp(ctx, s.Slug, s.TeamID, a)
	}
	if err == nil {
		err = d.docker.RemoveAppsNotIn(ctx, s.TeamID, s.Apps) // an app removed from the spec is stopped
	}
	if err == nil {
		err = d.selfCheck(ctx, pid, s) // eth0 up with the IP, default route, nft table present, :8099 answers
	}
	if err != nil {
		st.Error = err.Error()
		return st, err
	}
	st.Ready = true
	return st, nil
}

func (d *Driver) Remove(ctx context.Context, teamID string) error {
	cs, err := d.docker.ByTeam(ctx, teamID) // apps, placeholder, pod (by label)
	if err != nil {
		return err
	}
	for _, c := range cs.AppsThenPod() { // pod last
		if err := d.docker.remove(ctx, c.Name); err != nil {
			return err
		}
	}
	_ = os.Remove("/run/netns/" + cs.Slug) // if a debug "ip netns attach" exists
	// the NIC is back in the root namespace now: make sure it is down and has no address
	if mac := cs.MAC(); mac != nil {
		if l, err := linkByMAC(mac); err == nil {
			_ = netlink.LinkSetDown(l)
			_ = flushAddrs(l)
		}
	}
	return nil
}
```

The MAC of a team is stored as a label on its pod (`cstam.mac`), so `Remove` works even if the desired state no longer contains the team.

#### 5.10 The AAP driver (fallback)

Same interface, different plumbing. The key differences in code:

```go
// internal/host/netdriver/aap/driver.go (excerpt)
func (d *Driver) Ensure(ctx context.Context, s netdriver.TeamSpec) (netdriver.TeamStatus, error) {
	ens3, err := netlink.LinkByName(d.cfg.PrimaryIf) // from the default route at start-up
	if err != nil {
		return failed(s, err)
	}
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: s.IP.AsSlice(), Mask: net.CIDRMask(32, 32)}, Scope: unix.RT_SCOPE_UNIVERSE}
	if err := netlink.AddrReplace(ens3, addr); err != nil {
		return failed(s, err)
	}
	// OVN binds the reserved (virtual) port to this host only after it sees ARP from us
	_ = exec.CommandContext(ctx, "arping", "-U", "-c", "3", "-I", d.cfg.PrimaryIf, s.IP.String()).Run()

	if err := d.docker.EnsureBridgeNetwork(ctx, s.Slug, bridgeSubnet(s), d.cfg.MTU); err != nil { // cst-tNNNN
		return failed(s, err)
	}
	for _, a := range s.Apps { // -p <teamIP>:<port>:<port>
		if err := d.docker.EnsureAppPublished(ctx, s, a); err != nil {
			return failed(s, err)
		}
	}
	// whole-host rule set for all teams on this host, rendered and applied atomically
	return ok(s), firewall.ApplyIptables(ctx, firewall.RenderHostChains(d.allTeams(), d.cfg))
}
```

`Remove` in AAP mode deletes the containers and the bridge, re-renders the host chains without the team, deletes the SNAT rule, runs `conntrack -D -d <teamIP>` and `conntrack -D -s <bridge subnet>` (so established flows do not survive), and removes the `/32` from `ens3`. Removing the allowed pair and deleting the reserved port are the control plane's job, after the agent acknowledges.

#### 5.11 The agent loop

```go
// internal/host/agent.go
type Agent struct {
	Name    string
	Driver  netdriver.NetDriver
	Client  agentv1connect.ControlServiceClient
	State   *state.Store // last applied HostDesired on disk
	Workers int
	Log     *slog.Logger
	kick    chan struct{}
}

func (a *Agent) Run(ctx context.Context) error {
	// 1. static stability: re-apply the last known desired state immediately (works without the control plane)
	if last, ok := a.State.Load(); ok {
		a.converge(ctx, last)
	}
	go a.watchDocker(ctx) // pod died or restarted -> kick
	go a.reportLoop(ctx)  // heartbeat every 2 s: teams ready, driver, versions

	backoff := newBackoff(200*time.Millisecond, 10*time.Second)
	for ctx.Err() == nil {
		stream, err := a.Client.WatchHost(ctx, connect.NewRequest(&agentv1.WatchRequest{Agent: a.Name, After: a.State.Revision()}))
		if err != nil {
			sleep(ctx, backoff.Next())
			continue
		}
		for stream.Receive() {
			backoff.Reset()
			msg := stream.Msg()
			if msg.Revision == 0 {
				continue // keepalive
			}
			if msg.Revision < a.State.Revision() {
				continue // never go back in time
			}
			desired := fromProto(msg)
			a.State.Save(desired) // persist BEFORE acting, so a crash resumes the same target
			statuses := a.converge(ctx, desired)
			a.ack(ctx, desired.Revision, statuses)
		}
		sleep(ctx, backoff.Next())
	}
	return ctx.Err()
}

// converge makes the host equal to desired: ensure every desired team, remove every other cstam team.
func (a *Agent) converge(ctx context.Context, d state.HostDesired) []netdriver.TeamStatus {
	have, _ := a.Driver.List(ctx)
	want := map[string]bool{}
	for _, t := range d.Teams {
		want[t.TeamID] = true
	}
	for _, h := range have {
		if !want[h.TeamID] {
			if err := a.Driver.Remove(ctx, h.TeamID); err != nil {
				a.Log.Error("remove", "team", h.TeamID, "err", err)
			}
		}
	}
	out := make([]netdriver.TeamStatus, len(d.Teams))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(a.Workers) // dockerd serializes some operations; 4 in parallel is the sweet spot
	for i, t := range d.Teams {
		g.Go(func() error {
			tctx, cancel := context.WithTimeout(gctx, 30*time.Second)
			defer cancel()
			st, err := a.Driver.Ensure(tctx, t)
			if err != nil {
				a.Log.Warn("ensure", "team", t.Slug, "err", err)
			}
			out[i] = st
			return nil // one team's failure never blocks the others
		})
	}
	_ = g.Wait()
	return out
}
```

The Docker events watcher re-converges when something changes underneath the agent:

```go
func (a *Agent) watchDocker(ctx context.Context) {
	f := filters.NewArgs(filters.Arg("label", "cstam.role=pod"), filters.Arg("type", "container"))
	for ctx.Err() == nil {
		msgs, errs := a.Docker.Events(ctx, events.ListOptions{Filters: f})
		for {
			select {
			case m := <-msgs:
				if m.Action == "start" || m.Action == "die" || m.Action == "restart" {
					a.Log.Info("pod event, re-plumbing", "pod", m.Actor.Attributes["name"], "action", m.Action)
					a.kickConverge() // debounced 500 ms; restarts the team's apps so they join the new netns
				}
			case <-errs:
				goto reconnect
			case <-ctx.Done():
				return
			}
		}
	reconnect:
		time.Sleep(time.Second)
	}
}
```

Also, every 30 s the agent re-runs `converge` with the persisted desired state. That catches anything events missed (someone ran `nft flush` inside a namespace, a NIC went down). Self-checks are cheap: a netlink query and an `nft list table` per team.

::: why
**Why the agent persists the desired state before acting.**
If the agent crashes in the middle of `converge`, systemd restarts it in a second. On start, it re-applies the last desired state from disk, without waiting for the control plane. A host reboot is the same: the teams come back as soon as Docker and the agent are up. The control plane being down never prevents a host from restoring its teams.
:::

#### 5.12 Tests

**Unit tests (laptop, no root):**

| Test | Asserts |
|---|---|
| `TestRenderPodNft` | golden file; gateways come only from config; no string from the team spec reaches the file |
| `TestRenderHostChains` (AAP) | golden; one `CSTAM-XTEAM` and one `CSTAM-ANTISPOOF` line per team, sorted |
| `TestConvergeRemovesUnknownTeams` | fake driver: a team present on the host but not in desired → `Remove` called once |
| `TestConvergeOneFailureDoesNotBlock` | team A's `Ensure` fails, team B still ensured, statuses reported for both |
| `TestNeverGoesBackInTime` | revision 5 applied, then 3 → ignored |
| `TestAppSpecValidation` | app port 22, 8099 or 65536 rejected; app name `a;b` rejected |

**Host integration tests** (build tag `hostint`, run as root on a disposable dev VM, never on the demo hosts):

```bash
sudo -E go test -tags hostint -run . -count=1 ./internal/host/...
```

They use a **veth pair** instead of a real hot-plugged NIC: the test creates `veth-test` in the root namespace with a known MAC, and the driver must find it by MAC and move it into the pod namespace exactly as with a real NIC. That tests all the plumbing without touching OpenStack.

| Test | Asserts |
|---|---|
| `TestEnsureTwiceNoChange` | second `Ensure`: no container recreated (same container IDs), `ip -n` output identical, nft ruleset identical |
| `TestRemoveLeavesNothing` | after `Remove`: no container with the team label, the test NIC back in root ns, down, no address |
| `TestPodRestartReplumbs` | `docker restart <slug>-pod` → within 3 s eth0 is back in the new namespace with the IP; the app answers again |
| `TestMetadataBlocked` | inside the namespace, a connection to `169.254.169.254:80` times out |

**On the real host**, the manual checks of 5.2 are repeated with the agent doing the work (the fake control plane serves a `HostDesired` with team1), and once more after `sudo reboot`.

### Done when

::: done
**Done when**

- By hand: team1 and team2 each answer from gw1 at their own IP; team1 cannot reach team2, `169.254.169.254`, `10.20.0.5` or the gateways' port 22; Internet and a 10 MB download work from inside the team. Commands and outputs saved in `docs/notebook/experiments.md`.
- `sudo -E go test -tags hostint ./internal/host/...` passes on the dev VM.
- With the agent running and the fake control plane serving team1 and team2: both teams are ready within 5 s of the snapshot (warm NICs already attached), and the agent acknowledges the revision with `ready=true` for both.
- Removing team2 from the desired state: `docker ps --filter label=cstam.team=<team2-id>` is empty, the NIC is back in the root namespace and down, nothing else changed (team1 never stopped answering in a 10 requests/s probe).
- `sudo docker restart team1-pod`: team1 answers again within 5 s, without any action from the control plane.
- `sudo reboot` of the host VM with the control plane **stopped**: after boot, team1 and team2 answer again (static stability), and the agent reports `synced=false` until the control plane is back.
- AAP mode only (if G1 chose it): the same checks, plus `iptables -L CSTAM-XTEAM -v -n` counters increase when team1 tries team2.
:::

### Pitfalls

::: warn
**The NIC is not where you expect.** After an attach, the NIC can take a second or two to appear in the VM; after a pod restart it is back in the root namespace with a new name. Always search by MAC, in the root namespace first, then in the pod namespace, and treat "not found yet" as *waiting* (retry), not as an error.
:::

::: warn
**MTU 1500 somewhere.** If any interface in the path keeps MTU 1500, small requests work and big responses hang (classic "the page loads but the image never does"). Check `ip -n <team> link show eth0` says `mtu 1442`, and test with a large download, not with `curl /`.
:::

::: warn
**Running `nft flush ruleset` on the host.** It deletes Docker's tables and every container loses networking. Our rules always live in their own tables (`inet cstam_host` in the root namespace, `inet pod` inside each team namespace) and are replaced table by table.
:::

::: warn
**Netplan configuring hot-plugged NICs.** A generic match (`en*`) in the host's netplan gives every new NIC a DHCP address in the root namespace before the agent moves it. The team IP would then be reachable on the host itself. Match the primary MAC only, and verify with `networkctl list` after an attach.
:::

::: warn
**Forgetting that app containers depend on the pod.** When the pod restarts, the apps keep running in a namespace that no longer exists, without network. The agent must restart them after re-plumbing; `restart: unless-stopped` alone does not fix it.
:::

### Learning checkpoint

::: learn
**Answer these yourself in docs/notebook/learning.md**

1. Follow a packet from HAProxy on gw1 to team1's web container in the pod-NIC model. Which security group and which nft chain does it pass?
2. Team1 tries to open `http://10.20.0.33/` (team2). Name every place where the packet is dropped, in order.
3. Why does the agent find the NIC by MAC and not by name? What happens to the NIC when the pause container dies?
4. Why must the metadata service be blocked inside the team namespace, and why can Neutron not do it for us?
5. In the AAP fallback, why is `arping -U` needed after `ip addr add`, and why is it not needed in the pod-NIC model?
:::

### Driving Claude Code for this step

::: claude
**Prompts to use**

- Before any code: *"Here are the commands I ran by hand in section 5.2 and their outputs [paste]. Explain each command and tell me which netlink/netns/moby call does the same thing in Go. No code yet."*
- Then: *"Implement internal/host/netdriver/podnic with the NetDriver interface from section 5.8. Every operation must be idempotent (Replace, not Add). Write the hostint tests of section 5.12 first, using a veth pair with a fixed MAC instead of a real NIC."*
- For the loop: *"Implement Agent.converge and the Docker events watcher. Show me a timeline of what happens when the team1 pod container restarts, with the log lines the agent prints."*
- Review as an attacker: *"I control the image and the app spec of team1. List every way I could reach team2, the host, the metadata service or the control plane with this code, and which line stops each attempt."*

**What to review in the diff:** no shell strings (`exec` with argv only); no value from the team spec is rendered into nft or iptables text except the validated IP and port numbers; `Remove` never touches containers without the `cstam.team` label; `internal/host` does not import gophercloud.
:::
