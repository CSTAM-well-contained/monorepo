# 10. Step 1 – Burn down the risks (experiments)

## Step 1 – Prove the risky network behaviors on FelCloud before writing code

::: note
**Goal:** measured answers, written in `docs/notebook/experiments.md`, to the questions that can break the design: does a floating IP follow a VRRP virtual IP on FelCloud's OVN? How fast? Can a team get a real Neutron IP on a host VM, and by which mechanism (hot-plugged NIC or allowed-address-pairs)? What are the limits? At the end you pass **Gate G1** and lock two decisions: the HA mechanism and the team-network model.
**Points served:** de-risks HA gateways (15), provisioning (15), stress test (15), IPAM (10).
**Time estimate:** 1.5–3 days (most of it is waiting for VMs and measuring 5 times).
**Depends on:** Step 0.
:::

### Understand first

Everything in this project sits on a few behaviors of OpenStack Neutron with the OVN backend that we **cannot read** as a normal project user (the settings are admin-only). The only way to know is to try. If we wrote the control plane first and found out in week 3 that the floating IP does not follow the VIP, we would lose a week. So we test the risky parts first, by hand, with throw-away "lab" resources.

![Figure 10.1 – How a request reaches the master gateway, and what happens on failover](diagrams/f07-vip-failover.png)

The mechanisms you are testing:

- **VIP (virtual IP) and VRRP.** VRRP (Virtual Router Redundancy Protocol) lets two machines share one IP address. The master sends an "I am alive" advertisement every 0.5 s. If the backup stops hearing it, the backup takes the IP. Keepalived is the Linux program that speaks VRRP.
- **Allowed-address-pairs (AAP).** Neutron's port security drops every packet whose source IP is not the port's own IP (anti-spoofing). AAP is the list of *extra* IPs a port may use. Both gateways list the VIP in their AAP, so either may answer for it.
- **OVN virtual ports and GARP.** In ML2/OVN, a port with no VM (our VIP port) whose IP appears in other ports' AAP becomes an OVN **virtual port**. OVN does not know where it lives until one parent sends a **gratuitous ARP (GARP)**: an ARP packet that says "10.20.0.10 is at my MAC". The hypervisor that sees the GARP binds the virtual port to that VM. Failover = the new master sends a GARP, and OVN moves the binding. **No GARP, no failover.**
- **Floating IP (FIP).** A public IP that the OVN logical router translates (DNAT) to a private IP. We attach it to the VIP port. If FelCloud uses *distributed* floating IPs, NAT happens on the compute node of the port, and virtual ports have had bugs there. That is risk number one.
- **NIC hot-plug.** `openstack server add port` attaches a new virtual network card to a running VM. If it works fast and many times, each team can get its own real Neutron port (our "pod-NIC" model). The limit is the number of free PCI slots in the VM (machine type `pc` has about 25; `q35` may have very few).

Everything you create in Step 1 is named `cstam-lab-*` and tagged `cstam-lab`, and is deleted at the end.

#### The experiment list

| ID | Question | Decides | Needed for gate? |
|---|---|---|---|
| E3 | Can we create ports with a fixed IP outside the allocation pool? | IPAM range .32–.191 | yes |
| E0 | Router to INTERNET works? | everything public | yes |
| E1 | Does the FIP follow the VIP on keepalived failover, and how fast? | HA mechanism (P / F1 / F2) | yes |
| E6 | Failback without a second outage; split-brain behavior | keepalived settings | yes |
| H1 | How many NICs can be hot-plugged into one VM, how fast? Does a NIC moved into a netns work? | pod-NIC model | yes |
| E2 | Does a team IP via AAP + GARP reach the right VM (ARP gives the host's MAC)? | AAP fallback | yes |
| H2 | What is the AAP limit per port? | AAP fallback capacity | yes |
| E4 | Do concurrent AAP updates lose writes? Does `If-Match` return 412? | AAP syncer design | yes (if AAP model) |
| E5 | Does MTU 1442 matter for containers? | Docker settings | yes |
| E10 | Can a container on a VM reach the metadata service? | isolation rules | yes |
| E9 | Do we have DNS names (FelCloud wildcard or sslip.io)? | demo URLs, TLS | no (always a fallback) |
| E7 | Octavia OVN provider L4 load balancer with health monitor | fallback F2 | only if E1 fails |

### Do it by hand

All commands run on your laptop with `source infra/topology.env` (which sets `OS_CLOUD=cstam`), except where a VM name is shown in the prompt. **You run every command that creates or deletes something. Claude Code only proposes.**

#### 1.1 E3 – Fixed IP outside the allocation pool

Why: our IPAM hands out `.32–.191`, outside Neutron's automatic pool `.200–.250`. This only works if our project may request explicit fixed IPs there. The existing VM at `.68` suggests yes; prove it.

```bash
openstack port create --network cstam-sandbox-net \
  --fixed-ip subnet=cstam-sandbox-subnet,ip-address=10.20.0.192 \
  --tag cstam-lab --description "lab E3" cstam-lab-e3
openstack port show cstam-lab-e3 -f value -c fixed_ips -c status
# expected: [{'subnet_id': '...', 'ip_address': '10.20.0.192'}]  DOWN
openstack port delete cstam-lab-e3
```

Also test the duplicate case, because our code relies on Neutron refusing a second port with the same IP:

```bash
openstack port create --network cstam-sandbox-net --fixed-ip subnet=cstam-sandbox-subnet,ip-address=10.20.0.192 --tag cstam-lab cstam-lab-e3a
openstack port create --network cstam-sandbox-net --fixed-ip subnet=cstam-sandbox-subnet,ip-address=10.20.0.192 --tag cstam-lab cstam-lab-e3b
# expected second call: ConflictException: 409 ... IP address 10.20.0.192 already allocated in subnet ...
openstack port delete cstam-lab-e3a
```

Record: "E3: fixed IP outside pool OK; duplicate → 409 IpAddressAlreadyAllocated."

#### 1.2 E0 – Router to the Internet

`cstam-sandbox-net` has no router with an external gateway today. Without it, no floating IP can work and VMs cannot download packages. This router is permanent (it is also part of Step 2).

```bash
openstack router create cstam-router --external-gateway INTERNET --enable-snat --tag cstam
openstack router add subnet cstam-router cstam-sandbox-subnet
openstack router show cstam-router -f json -c external_gateway_info | jq '.external_gateway_info.external_fixed_ips'
# expected: one entry with ip_address 197.5.133.x
openstack port list --router cstam-router -c "Fixed IP Addresses" -f value
# expected: includes 10.20.0.1
```

::: warn
**Check first.** `openstack router list` shows only `private_router` today. That router serves another subnet and has no external gateway; do not touch it. If `cstam-router` already exists (you ran this before), skip the create.
:::

#### 1.3 Lab resources for E1, E6, E2, H1, H2, E4, E5, E10

One lab security group, a lab VIP and two small lab VMs (`cstam-lab-a`, `cstam-lab-b`), each 1 vCPU / 2 GB:

```bash
ADMIN_CIDR="$(curl -s https://ifconfig.me)/32"
openstack security group create cstam-lab --description "CSTAM lab (Step 1, delete after)"
R="openstack security group rule create --ingress --ethertype IPv4"
$R cstam-lab --protocol tcp --dst-port 80 --remote-ip 0.0.0.0/0
$R cstam-lab --protocol tcp --dst-port 22 --remote-ip "$ADMIN_CIDR"
$R cstam-lab --protocol 112 --remote-group cstam-lab          # VRRP between lab VMs
$R cstam-lab --protocol tcp --remote-group cstam-lab           # any TCP between lab VMs
$R cstam-lab --protocol icmp --remote-group cstam-lab

N="openstack port create --network cstam-sandbox-net --tag cstam-lab --security-group cstam-lab"
# VIP port FIRST, then the ports that list it in allowed-address-pairs (exact IP, no CIDR)
$N --fixed-ip subnet=cstam-sandbox-subnet,ip-address=10.20.0.192 --description "lab VIP - never attach" cstam-lab-vip
$N --fixed-ip subnet=cstam-sandbox-subnet,ip-address=10.20.0.193 --allowed-address ip-address=10.20.0.192 cstam-lab-a-port
$N --fixed-ip subnet=cstam-sandbox-subnet,ip-address=10.20.0.194 --allowed-address ip-address=10.20.0.192 cstam-lab-b-port

openstack keypair create --public-key ~/.ssh/cstam_ed25519.pub cstam-key   # once; reused in Step 2
```

Lab cloud-init (installs what the experiments need; **no secrets**):

```yaml
# infra/lab/lab.yaml
#cloud-config
package_update: true
packages: [keepalived, haproxy, socat, curl, jq, iputils-arping, tcpdump, chrony, docker.io, bc]
write_files:
  - path: /etc/sysctl.d/90-cstam-lab.conf
    content: |
      net.ipv4.ip_nonlocal_bind = 1
      net.ipv4.conf.all.arp_ignore = 1
      net.ipv4.conf.all.arp_announce = 2
runcmd:
  - sysctl --system
  - useradd -r -s /usr/sbin/nologin keepalived_script || true
  - systemctl enable --now chrony
```

```bash
for v in a b; do
  openstack server create --flavor G0.basic.1c2g --image "Ubuntu 24.04 LTS - Noble Numbat" \
    --port cstam-lab-$v-port --key-name cstam-key --user-data infra/lab/lab.yaml \
    --property cstam-lab=1 --wait cstam-lab-$v
done
# If the server create fails with "image too large"/boot-from-volume errors, add: --boot-from-volume 20
# temporary FIPs for SSH (deleted in cleanup)
for v in a b; do
  openstack floating ip create INTERNET --port cstam-lab-$v-port --tag cstam-lab -f value -c floating_ip_address
done
# the FIP under test, on the lab VIP port
openstack floating ip create INTERNET --port cstam-lab-vip --fixed-ip-address 10.20.0.192 --tag cstam-lab -f value -c floating_ip_address
```

Write the three addresses down: `LAB_A_FIP`, `LAB_B_FIP`, `LAB_VIP_FIP`. SSH with `ssh -i ~/.ssh/cstam_ed25519 ubuntu@$LAB_A_FIP`.

On both VMs, find the interface name (usually `ens3`):

```bash
ip -o -4 route show default | awk '{print $5}'      # ens3
cat /sys/class/dmi/id/product_name                  # machine type, record it (needed for H1)
```

#### 1.4 E1 – VIP + floating IP + keepalived failover

**A. A minimal HAProxy on each lab VM** that answers with its own name (so no backend is needed):

```haproxy
# /etc/haproxy/haproxy.cfg on cstam-lab-a (use "lab-b" on the other VM)
global
    master-worker
    stats socket /run/haproxy/admin.sock mode 660 level admin
defaults
    mode http
    timeout connect 2s
    timeout client 10s
    timeout server 10s
frontend fe
    bind :80
    http-request return status 200 content-type text/plain lf-string "I am lab-a %[date]\n" hdr X-Served-By lab-a
```

```bash
sudo haproxy -c -f /etc/haproxy/haproxy.cfg && sudo systemctl restart haproxy
curl -s localhost        # I am lab-a ...
```

**B. Keepalived** (`/etc/keepalived/keepalived.conf`). This is the production configuration from Step 3, reduced. Start with `advert_int 1` as the baseline, then repeat everything with `0.5`.

```conf
global_defs {
    router_id lab-a                   # lab-b on the other VM
    vrrp_version 3
    enable_script_security
    script_user keepalived_script
    vrrp_garp_master_delay 1
    vrrp_garp_master_repeat 3
    vrrp_garp_master_refresh 10
    vrrp_garp_master_refresh_repeat 1
    vrrp_higher_prio_send_advert true
}
vrrp_track_process haproxy_proc {
    process haproxy
    quorum 1
    delay 0
}
vrrp_instance VI_LAB {
    state BACKUP
    nopreempt
    interface ens3
    virtual_router_id 61
    priority 150                      # lab-b: 100
    advert_int 1                      # baseline; then 0.5
    unicast_src_ip 10.20.0.193        # lab-b: 10.20.0.194
    unicast_peer {
        10.20.0.194                   # lab-b: 10.20.0.193
    }
    virtual_ipaddress {
        10.20.0.192/32 dev ens3
    }
    track_process {
        haproxy_proc weight 0
    }
}
```

```bash
sudo keepalived -t -f /etc/keepalived/keepalived.conf && sudo systemctl restart keepalived
journalctl -u keepalived -n 5 --no-pager   # one of them: "(VI_LAB) Entering MASTER STATE"
```

**C. Checks before any failure** (expected output in comments):

```bash
# on the master (lab-a)
ip -br addr show ens3                 # ens3 UP 10.20.0.193/24 10.20.0.192/32
# on lab-b (internal client)
curl -s 10.20.0.192                   # I am lab-a ...
arping -c 2 -I ens3 10.20.0.192       # reply from lab-a's MAC
openstack port show cstam-lab-a-port -f value -c mac_address    # (laptop) must equal that MAC
# from the laptop (public path)
curl -s http://$LAB_VIP_FIP/          # I am lab-a ...
```

If the internal path works but the public path does not, stop here and read the gate rules (fallback F1 or F2).

**D. The measurement probe.** One new TCP connection every 10 ms, 300 ms timeout, one line per request. Put it in the repository now; you will reuse it for the benchmark report.

```go
// bench/failover-probe/main.go
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	url := flag.String("url", "http://10.20.0.10/", "target URL")
	host := flag.String("host", "", "optional Host header")
	every := flag.Duration("every", 10*time.Millisecond, "interval between requests")
	timeout := flag.Duration("timeout", 300*time.Millisecond, "per-request timeout")
	flag.Parse()

	client := &http.Client{
		Timeout:   *timeout,
		Transport: &http.Transport{DisableKeepAlives: true}, // new TCP connection each time
	}
	ticker := time.NewTicker(*every)
	defer ticker.Stop()
	for range ticker.C {
		go probe(client, *url, *host) // do not let a slow request delay the next one
	}
}

func probe(c *http.Client, url, host string) {
	start := time.Now()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if host != "" {
		req.Host = host
	}
	who, code, status := "-", 0, "err"
	resp, err := c.Do(req)
	if err == nil {
		code, who, status = resp.StatusCode, resp.Header.Get("X-Served-By"), "ok"
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	fmt.Fprintf(os.Stdout, "%d %s %d %s %d\n", start.UnixMilli(), who, code, status, time.Since(start).Milliseconds())
}
```

```bash
go run ./bench/failover-probe -url http://$LAB_VIP_FIP/ > /tmp/e1-run1.log      # laptop: public path
# in parallel on lab-b (internal path; copy the binary with scp first):
./failover-probe -url http://10.20.0.192/ > /tmp/e1-int-run1.log
# in parallel on lab-b: capture VRRP and ARP
sudo tcpdump -n -tt -i ens3 'ip proto 112 or arp' -w /tmp/e1-run1.pcap
```

Both VMs need `chronyc tracking` with an offset of a few milliseconds, otherwise you cannot compare their logs.

**E. Failure injections.** Do each one 5 times with the probe running, about 30 s apart. Restore between runs.

| # | Injection (on the master) | Simulates | Expected (advert 1 s / 0.5 s) | Restore |
|---|---|---|---|---|
| 1 | `sudo systemctl stop keepalived` | clean stop (sends priority 0) | 0.2–1 s / same | `sudo systemctl start keepalived` |
| 2 | `sudo systemctl kill -s KILL haproxy` | proxy crash, VM alive | 0.2–1 s / same | `sudo systemctl start haproxy` |
| 3 | `sudo pkill -STOP keepalived` | hung keepalived (silent) | ~3.6 s / ~1.8 s | `sudo pkill -CONT keepalived` |
| 4 | `openstack server pause cstam-lab-a` (laptop) | frozen VM | ~3.6 s / ~1.8 s + OVN rebind | `openstack server unpause cstam-lab-a` |
| 5 | `echo c \| sudo tee /proc/sysrq-trigger` | kernel panic (the demo crash) | ~3.6 s / ~1.8 s + OVN rebind | `openstack server reboot --hard cstam-lab-a` |

**F. How to compute the failover time.** In the probe log: failover = timestamp of the **first** `200` served by the new master minus timestamp of the **last** `200` from the old master. Every `err` line in between is a client-visible failure. A tiny script makes it repeatable:

```bash
# bench/failover-probe/gap.sh: prints the longest gap between two successful requests, in ms
awk '$4=="ok"{ if (prev && $1-prev>max) {max=$1-prev; at=$1} prev=$1 } END{print "max_gap_ms=" max, "at=" at}' "$1"
```

In the pcap you can split the gap into two parts:

```bash
tshark -r /tmp/e1-run1.pcap -T fields -e frame.time_epoch -e ip.src -e _ws.col.Info | less
# detection   = first advert from the new master − last advert from the old master
# convergence = first 200 from the new master − first GARP from the new master  (this is OVN rebinding)
```

Record in `docs/notebook/experiments.md` a table: injection type × (median, max) for advert 1 s and 0.5 s, both public and internal paths.

#### 1.5 E6 – Failback and split brain

**Failback.** After injection 4 (pause) unpause lab-a. With `nopreempt`, lab-a must stay BACKUP and the probe must show **zero errors** at that moment. Then run the opposite test once with `preempt` (remove `nopreempt`, restart both): you will see a second short outage when lab-a takes the VIP back. That is why we choose `nopreempt`.

**Split brain.** Block VRRP on the backup:

```bash
# on lab-b
sudo iptables -I INPUT -p 112 -j DROP
journalctl -u keepalived -f        # lab-b: "Entering MASTER STATE" after ~1.8 s -> two masters
# meanwhile the probe from the laptop: X-Served-By flips between lab-a and lab-b (last GARP wins in OVN)
sudo iptables -D INPUT -p 112 -j DROP
# expected: within 1-2 s one of them returns to BACKUP; probe stable again
```

Record how long it took to converge after you removed the rule. The control plane will detect "two masters for more than 2 s" and force one into FAULT (Step 3).

#### 1.6 H1 – NIC hot-plug: limit, latency, and a pod-NIC smoke test

This decides the **primary** team-network model. Use ports **without** a fixed IP: Neutron gives them addresses from its own pool `.200–.250`, which is fine for a test, and they are deleted right after.

```bash
# laptop
: > /tmp/h1.txt
for i in $(seq 1 16); do
  pid=$(openstack port create --network cstam-sandbox-net --security-group cstam-lab \
        --tag cstam-lab -f value -c id cstam-lab-hp-$i)
  s=$(date +%s.%N)
  if ! openstack server add port cstam-lab-b "$pid"; then echo "FAILED at $i" | tee -a /tmp/h1.txt; break; fi
  until [ "$(openstack port show "$pid" -f value -c status)" = ACTIVE ]; do sleep 0.2; done
  echo "$i $(echo "$(date +%s.%N) - $s" | bc)" | tee -a /tmp/h1.txt
done
```

Inside `cstam-lab-b`:

```bash
ip -br link | grep -c '^ens'            # number of NICs (expected 17 = 1 + 16)
cat /sys/class/dmi/id/product_name      # "Standard PC (i440FX + PIIX, 1996)" = pc ; "... Q35 ..." = q35
```

Also measure concurrency: attach 4 ports at the same time (four `openstack server add port` in background with `&`). Nova serializes attaches per instance, so the total should be about 4 × the single time. Record it: it sizes the warm pool.

**Pod-NIC smoke test** (the mechanism from Step 5, by hand, on one of the attached NICs):

```bash
# laptop: pick one hot-plugged port, note its MAC and IP
openstack port show cstam-lab-hp-1 -f value -c mac_address -c fixed_ips
# on lab-b (replace MAC and IP)
MAC=fa:16:3e:xx:xx:xx; IP=10.20.0.2xx
IF=$(for d in /sys/class/net/*; do [ "$(cat $d/address)" = "$MAC" ] && basename $d; done); echo $IF
sudo docker run -d --name lab-pod --network none registry.k8s.io/pause:3.10
PID=$(sudo docker inspect -f '{{.State.Pid}}' lab-pod)
sudo ip netns attach labpod "$PID"
sudo ip link set dev "$IF" netns labpod
sudo ip -n labpod link set dev "$IF" name eth0
sudo ip -n labpod link set dev eth0 mtu 1442 up
sudo ip -n labpod addr add $IP/24 dev eth0
sudo ip -n labpod route add default via 10.20.0.1
sudo docker run -d --name lab-web --network container:lab-pod traefik/whoami --port 80
# on lab-a:
curl -s http://$IP/ | head -3          # Hostname: ... IP: 10.20.0.2xx  -> works
```

Then delete the pod and check the NIC comes back to the root namespace:

```bash
sudo docker rm -f lab-web lab-pod; sudo ip netns delete labpod
ip -br link | grep -i "$MAC" || ip -br link   # the NIC is back in the root netns (named eth0 or ensX)
```

Cleanup H1 (laptop):

```bash
for i in $(seq 1 16); do
  id=$(openstack port show cstam-lab-hp-$i -f value -c id 2>/dev/null) || continue
  openstack server remove port cstam-lab-b "$id" 2>/dev/null
  openstack port delete "$id"
done
```

#### 1.7 E2 – Team IP via allowed-address-pairs (the fallback model)

```bash
# laptop: a reserved port for the "team" IP, then allow it on lab-b's port
openstack port create --network cstam-sandbox-net --fixed-ip subnet=cstam-sandbox-subnet,ip-address=10.20.0.195 \
  --security-group cstam-lab --tag cstam-lab cstam-lab-team
openstack port set cstam-lab-b-port --allowed-address ip-address=10.20.0.195
# lab-b
sudo ip addr add 10.20.0.195/32 dev ens3
sudo arping -U -c 3 -I ens3 10.20.0.195        # GARP: binds the OVN virtual port to lab-b
sudo docker run -d --name lab-team -p 10.20.0.195:80:80 traefik/whoami --port 80
# lab-a
curl -s 10.20.0.195 | head -2                  # works
arping -c 2 -I ens3 10.20.0.195                # MAC must be lab-b's MAC, NOT cstam-lab-team's MAC
openstack port show cstam-lab-b-port -f value -c mac_address    # (laptop)
openstack port show cstam-lab-team   -f value -c mac_address    # (laptop) must NOT be the one answering
```

Repeat 10 times: remove the AAP, re-add it, re-send the GARP, test again. Also test **without** the `arping` step once: if the IP is unreachable until you send the GARP, record that the host agent must always send it.

#### 1.8 H2 – The allowed-address-pairs limit

```bash
# laptop: add 12 extra pairs one by one to lab-b's port (no ports needed, the IPs just must be unused)
for i in $(seq 1 12); do
  openstack port set cstam-lab-b-port --allowed-address ip-address=10.20.0.$((179+i)) \
    || { echo "AAP limit hit at $i"; break; }
done
openstack port show cstam-lab-b-port -f json -c allowed_address_pairs | jq '.allowed_address_pairs | length'
# cleanup: keep only the lab VIP and the E2 team IP
openstack port set cstam-lab-b-port --no-allowed-address \
  --allowed-address ip-address=10.20.0.192 --allowed-address ip-address=10.20.0.195
```

An error like `Exceeded maximum amount of allowed address pairs (10)` means the default Neutron limit applies. With the VIP and E2 already in the list, the 9th addition fails.

#### 1.9 E4 – Lost updates on allowed-address-pairs

Neutron replaces the whole AAP list on every update. Two programs that each "read the list, add one IP, write the list" at the same time can erase each other's change. This small Go program proves it, then proves the fix (`If-Match: revision_number`, which FelCloud supports through the `revision-if-match` extension).

```go
// infra/lab/e4/main.go   (run from the laptop; uses your clouds.yaml, cloud "cstam")
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/config"
	"github.com/gophercloud/gophercloud/v2/openstack/config/clouds"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
)

func main() {
	portID := flag.String("port", "", "lab port id (cstam-lab-b-port)")
	safe := flag.Bool("safe", false, "use If-Match revision_number and retry on 412")
	n := flag.Int("n", 6, "concurrent writers (keep under the AAP limit)")
	flag.Parse()
	ctx := context.Background()

	ao, eo, tlsCfg, err := clouds.Parse(clouds.WithCloudName("cstam"))
	if err != nil { log.Fatal(err) }
	pc, err := config.NewProviderClient(ctx, ao, config.WithTLSConfig(tlsCfg))
	if err != nil { log.Fatal(err) }
	nc, err := openstack.NewNetworkV2(pc, eo)
	if err != nil { log.Fatal(err) }

	var wg sync.WaitGroup
	conflicts := 0
	var mu sync.Mutex
	for i := 0; i < *n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip := fmt.Sprintf("10.20.0.%d", 180+i)
			for attempt := 0; attempt < 10; attempt++ {
				p, err := ports.Get(ctx, nc, *portID).Extract()
				if err != nil { log.Println(err); return }
				pairs := append(p.AllowedAddressPairs, ports.AddressPair{IPAddress: ip})
				opts := ports.UpdateOpts{AllowedAddressPairs: &pairs}
				if *safe {
					rev := p.RevisionNumber
					opts.RevisionNumber = &rev // sent as If-Match: revision_number=<rev>
				}
				_, err = ports.Update(ctx, nc, *portID, opts).Extract()
				if *safe && gophercloud.ResponseCodeIs(err, http.StatusPreconditionFailed) {
					mu.Lock(); conflicts++; mu.Unlock()
					continue // someone else wrote first: re-read and retry
				}
				if err != nil { log.Println(ip, err) }
				return
			}
		}(i)
	}
	wg.Wait()
	p, _ := ports.Get(ctx, nc, *portID).Extract()
	fmt.Printf("final pairs=%d (expected %d + existing), 412 conflicts=%d\n", len(p.AllowedAddressPairs), *n, conflicts)
}
```

```bash
LAB_B_PORT=$(openstack port show cstam-lab-b-port -f value -c id)
go run ./infra/lab/e4 -port $LAB_B_PORT -n 6           # naive: expect FEWER than 6 new pairs (lost updates)
# reset the list (command at the end of 1.8), then:
go run ./infra/lab/e4 -port $LAB_B_PORT -n 6 -safe     # safe: expect all 6 pairs and some 412 conflicts
```

::: note
**If the gophercloud version you pinned has no `RevisionNumber` field** in `ports.UpdateOpts`, send the header yourself: `gophercloud.RequestOpts{MoreHeaders: map[string]string{"If-Match": fmt.Sprintf("revision_number=%d", rev)}}` on a raw `nc.Put`. Check with `go doc github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports.UpdateOpts`.
:::

Record both results. The numbers go into the report as the reason for the AAP syncer design.

#### 1.10 E5 – MTU 1442

```bash
# lab-b: a Docker network with the default MTU (1500) and one with 1442
sudo docker network create mtu1500
sudo docker network create -o com.docker.network.driver.mtu=1442 mtu1442
for n in mtu1500 mtu1442; do
  echo "== $n"; sudo docker run --rm --network $n curlimages/curl -s -o /dev/null \
    -w '%{http_code} %{size_download} %{time_total}\n' --max-time 60 https://proof.ovh.net/files/100Mb.dat
done
sudo docker network rm mtu1500 mtu1442
```

If the 1500 run stalls or is much slower and the 1442 run is fine, the MTU matters on FelCloud. Even if both work (path MTU discovery can save you), we set 1442 everywhere: it costs nothing.

#### 1.11 E10 – Metadata reachability from a container

```bash
# lab-b
sudo docker run --rm curlimages/curl -s -m 3 http://169.254.169.254/openstack/latest/meta_data.json | head -c 200; echo
```

Expected: JSON about **lab-b** (name, uuid, keys). This proves a team container can read its host VM's metadata (user-data included) unless we block it. The block rules come in Steps 5 and 9; the test becomes security test S06.

#### 1.12 E9 – Hostnames

```bash
dig +short team1-hacka.$(echo $LAB_VIP_FIP | tr . -).sslip.io          # prints LAB_VIP_FIP
curl -s http://team1-hacka.$(echo $LAB_VIP_FIP | tr . -).sslip.io/     # I am lab-a ...
```

sslip.io is a public DNS service that resolves any name containing an IP to that IP. It is our zero-dependency fallback when FelCloud has not created the wildcard record yet. Record the mentors' answer about `*.cstam.felcloud.tn` here when it arrives.

#### 1.13 E7 (optional) – Octavia OVN provider in front of two gateways

Only if E1 fails on the public path, or if you want a number for the report. It does not use our instance quota.

```bash
openstack loadbalancer create --name cstam-lab-lb --provider ovn --vip-subnet-id cstam-sandbox-subnet --wait
openstack loadbalancer listener create --name cstam-lab-l80 --protocol TCP --protocol-port 80 --wait cstam-lab-lb
openstack loadbalancer pool create --name cstam-lab-p80 --listener cstam-lab-l80 --protocol TCP --lb-algorithm SOURCE_IP_PORT --wait
openstack loadbalancer member create --address 10.20.0.193 --protocol-port 80 --subnet-id cstam-sandbox-subnet --wait cstam-lab-p80
openstack loadbalancer member create --address 10.20.0.194 --protocol-port 80 --subnet-id cstam-sandbox-subnet --wait cstam-lab-p80
openstack loadbalancer healthmonitor create --type TCP --delay 1 --timeout 1 --max-retries 1 --max-retries-down 1 --wait cstam-lab-p80
VIP_PORT=$(openstack loadbalancer show cstam-lab-lb -f value -c vip_port_id)
openstack floating ip create INTERNET --port $VIP_PORT --tag cstam-lab
```

Run the probe against that FIP and stop HAProxy on one VM: measure the gap as in E1. If the health monitor create is refused, the OVN provider of this release does not support it: record that, F2 then uses the amphora "Active-Standby LoadBalancer" flavor instead (slower, minutes to create).

Delete it afterwards: `openstack loadbalancer delete --cascade --wait cstam-lab-lb`.

### Build it in Go

Step 1 is about measuring, not building. The only Go code is the probe (`bench/failover-probe`) and the E4 program. Both are kept: the probe is reused in Steps 3, 4 and 11; the E4 program becomes a contract test of the real Neutron client in Step 7 (build tag `e2e`).

### Gate G1 – lock two decisions

::: gate
**Gate G1: HA mechanism**

| Observation | Decision |
|---|---|
| Public and internal paths fail over in < 5 s for all 5 injection types, 5/5 runs, never stuck | **P: keepalived VRRP VIP + FIP on the virtual port** (primary design). Use `advert_int 0.5`. |
| Internal path works, public FIP path fails or stays on the old master | **F1: the gateway agent moves the FIP** to its own fixed IP when it becomes MASTER (Neutron API, credential limited to that FIP). Expected 1.5–4 s. |
| The VIP itself does not work (no ARP reply with the master's MAC even after GARP), or VRRP is dropped by the platform | **F2: Octavia OVN-provider L4 load balancer** (E7) in front of both HAProxy gateways (active/active). If E7 has no health monitor: amphora Active-Standby flavor. |
| Two FIPs + DNS round-robin | Rejected in all cases: failover takes minutes because of DNS caches. |
:::

::: gate
**Gate G1: team-network model**

| Observation | Decision |
|---|---|
| H1: ≥ 12 NICs attached, each attach < 10 s, pod-NIC smoke test works | **Primary: pod-NIC** (real Neutron port per team, hot-plugged, moved into the team's netns). Warm pool size = how many teams must start in a burst (10–12 per host). |
| H1 fails early (q35, "No more available PCI slots") but E2 works and H2 ≥ 16 pairs | **Fallback: AAP model** with the per-host AAP syncer and `If-Match` (E4). |
| H1 fails and H2 = 10 | **Fallback: AAP model, CIDR mode**: each host gets one aligned `/27` pair (e.g. `10.20.0.64/27`), team ports are still created (admin-down) to reserve each IP in Neutron. Or: at most ~8 teams per host and a second host. |
| E2 fails (ARP answered with the reserved port's MAC, even after GARP) and H1 fails | Stop. Ask FelCloud (machine type `pc` image property, `max_allowed_address_pair`). Meanwhile build with CIDR-mode AAP without reserved ports. |

Write both decisions, with the measured numbers, in the "Decisions log" of `docs/notebook/experiments.md`. **After G1, do not reopen these decisions** unless a later step proves them wrong on the real system.
:::

### Cleanup

Everything from Step 1 except the router and the keypair is deleted. Delete in dependency order: floating IPs, then servers, then ports, then the security group.

```bash
# infra/lab/clean.sh: deletes ONLY resources named cstam-lab-* / tagged cstam-lab
set -euo pipefail
export OS_CLOUD=cstam
for fip in $(openstack floating ip list --tags cstam-lab -f value -c ID); do openstack floating ip delete "$fip"; done
openstack loadbalancer delete --cascade --wait cstam-lab-lb 2>/dev/null || true
for s in cstam-lab-a cstam-lab-b; do openstack server delete --wait "$s" 2>/dev/null || true; done
for p in $(openstack port list --tags cstam-lab -f value -c ID); do openstack port delete "$p"; done
openstack security group delete cstam-lab 2>/dev/null || true
echo "left over:"; openstack port list --tags cstam-lab -f value; openstack server list --name '^cstam-lab' -f value
```

::: done
**Done when**

- `docs/notebook/experiments.md` has one filled entry per experiment E3, E0, E1 (table of 5 injections × 2 advert values × 2 paths, median and max), E6, H1 (count, per-attach time, machine type), E2, H2 (limit), E4 (naive vs safe numbers), E5, E10, E9.
- The two Gate G1 decisions are in the decisions log with the numbers that justify them.
- `infra/lab/clean.sh` printed nothing under "left over", and `openstack server list` shows only `sbx-f0699704` and `adam`.
- `cstam-router` still exists and has an external gateway on INTERNET.
:::

### Pitfalls

::: warn
**Wrong order for the VIP port.** If you create the gateway ports with the VIP in their AAP *before* the VIP port exists, or use a CIDR (`10.20.0.192/32` as a network) instead of the exact IP, Neutron may not mark the VIP port as an OVN virtual port. Symptom: `arping 10.20.0.192` answers with the **VIP port's** MAC, which lives on no hypervisor, so traffic disappears. Fix: delete and recreate in the right order.
:::

::: warn
**No GARP, no failover.** If the backup becomes MASTER (journal says so) but the probe stays broken, OVN did not see a GARP. Check `tcpdump -n -i ens3 arp` on the new master. The `vrrp_garp_*` settings and the `arp_ignore/arp_announce` sysctls are there for this.
:::

::: warn
**Time.** Without `chrony` in sync, the timestamps of two machines cannot be compared and your failover numbers are wrong. Check `chronyc tracking` before each measurement series.
:::

::: warn
**A failed hot-plug can leave a port attached.** If `server add port` times out, check `openstack port show <id> -c device_id` before deleting the port. Detach first, then delete.
:::

### Learning checkpoint

::: learn
**Answer these yourself in docs/notebook/learning.md**

1. Why does Neutron drop packets for the VIP if the VIP is not in the gateway port's allowed-address-pairs?
2. What exactly does a gratuitous ARP do during a failover on OVN? What happens if it is lost?
3. Why does `systemctl stop keepalived` fail over faster than a kernel panic? Compute the master-down interval for `advert_int 0.5` and priority 100.
4. In E4, explain with a timeline of two writers why one IP is lost without `If-Match`, and why `If-Match` fixes it.
5. Why can a container read the host VM's metadata, and why is that dangerous?
:::

### Driving Claude Code for this step

::: claude
**Prompts to use**

- "We are in Step 1. Explain how OVN binds a virtual port from a gratuitous ARP, then review my keepalived.conf for the lab. Do not run any openstack command that creates or deletes anything."
- "Write bench/failover-probe as described in the guide, plus gap.sh. Add a `-host` flag. Show me how to run it."
- "Here is my probe log and pcap summary [paste]. Compute the failover time for each injection and fill the table in docs/notebook/experiments.md."

**Review:** the probe uses a new connection per request (`DisableKeepAlives: true`) and runs each request in its own goroutine, otherwise a stalled request hides the gap.
:::
