# 11. Step 2 – OpenStack foundation as code

## Step 2 – Router, security groups, ports, server groups and VMs with Pulumi (Go)

::: note
**Goal:** the permanent platform exists on FelCloud and is described by one Pulumi program in Go (`infra/pulumi`): router, 5 security groups with every rule, the VIP port and the infrastructure ports with fixed IPs, a server group for the gateways, 5 VMs (2 gateways, 1 control plane, 2 hosts) booted with cloud-init, 2 floating IPs (public entry on the VIP, SSH/dashboard on the control plane), and a safe teardown (`pulumi destroy`).
**Points served:** prerequisite for everything. Infrastructure as code in the same language as the rest of the project.
**Time estimate:** 0.5–1 day.
**Depends on:** Step 1 and Gate G1 (the G1 decision is one config value: `teamNetMode`).
:::

### Understand first

![Figure 11.1 – OpenStack resources and the IP plan of cstam-sandbox-net](diagrams/f06-foundation.png)

We do **not** use one network per role. Every VM sits on `cstam-sandbox-net` (10.20.0.0/24). Isolation comes from **security groups** (SGs): stateful firewall rules that Neutron/OVN applies on every port. A rule can allow traffic "from any port that is a member of group X" (`--remote-group X`). This is better than IP ranges because membership follows the ports: a rebuilt gateway with a new IP is trusted automatically, and nothing else is.

The IP plan (the `topo` variable in `infra/pulumi/config.go`, mirrored in `internal/config/topology.go`):

| IP | What |
|---|---|
| 10.20.0.1 | `cstam-router` interface |
| 10.20.0.5 | control plane `cstam-cp` |
| 10.20.0.10 | VIP (`cstam-vip`, no VM, floating IP attached) |
| 10.20.0.11 / .12 | `cstam-gw1` / `cstam-gw2` |
| 10.20.0.21 / .22 | `cstam-host-a` / `cstam-host-b` |
| 10.20.0.32 – .191 | team pool, managed by our IPAM (.68 is not ours) |
| 10.20.0.192 – .199 | lab / chaos tests |
| 10.20.0.200 – .250 | Neutron's automatic pool (never used by us) |

Why pre-create ports with fixed IPs and boot servers on them (`server create --port`) instead of letting Nova create the ports? Three reasons: the IPs are predictable (keepalived and the firewall rules need them), the SGs are set on the port before the VM boots (so the `default` group is never attached), and the gateway ports can list the VIP in their allowed-address-pairs from the start.

Security group budget (quota: 10 groups, 100 rules). Each new group comes with 2 default egress rules (IPv4 and IPv6):

| Group | Attached to | Ingress rules | Total rules |
|---|---|---|---|
| `cstam-gw` | gw1, gw2, VIP port | 80, 443 from anywhere; 22, 8404, icmp from `cstam-cp` | 5 + 2 |
| `cstam-vrrp` | gw1, gw2 | proto 112 from `cstam-vrrp` | 1 + 2 |
| `cstam-cp` | cp | 22 and 443 from your admin IP; 8443 from `cstam-gw` and `cstam-host` | 4 + 2 |
| `cstam-host` | host-a, host-b (primary NIC) | 22 and icmp from `cstam-cp` | 2 + 2 |
| `cstam-team` | every team port | tcp 80 and 8000–8099 from `cstam-gw` only | 2 + 2 |
| **Total** | | | **~24 new** (plus ~6 existing) |

Note what is **not** there:

- No rule lets anything reach an agent. The agents **dial out** to the control plane on 8443; the gateways and hosts have no management port open except SSH from the control plane.
- No `cstam-team` rule with `--remote-group cstam-team`. Without a "self" rule, Neutron itself drops team-to-team traffic, even for two teams on the same host.
- `cstam-host` is never used as a remote group. In the AAP fallback model, team IPs are allowed-address-pairs of the host port, so they count as members of `cstam-host`; using it as a source would let every team in.

### Why Pulumi, and what it is

Infrastructure as code (IaC) means the platform is described in files in git, and a tool makes the cloud match the files. We use **Pulumi with Go** (decision ADR-22 in chapter 6): the same language as the rest of the project, real types and compile errors instead of YAML typos, and `pulumi preview` shows the exact plan before anything changes.

Five ideas are enough to use it well:

| Idea | Meaning for us |
|---|---|
| **Program** | `infra/pulumi/*.go`. It does not call OpenStack directly: it *declares* resources (`networking.NewPort(...)`). Pulumi compares the declarations with its state and decides what to create, update or delete. |
| **Stack** | One instance of the program with its own config and state. We have `prod` (and later maybe `dev`). The stack name becomes the tag `cstam-env:<stack>`. |
| **State** | Pulumi's record of what it created (IDs, properties). We keep it on the laptop (`pulumi login file://~/.pulumi-cstam`), outside git, and back it up. Lose it and Pulumi forgets the resources (you would re-adopt them with `pulumi import`). Pulumi Cloud's free tier is the alternative if you prefer hosted state. |
| **preview / up / destroy** | `preview` is read-only and prints the plan. `up` applies it. `destroy` deletes what the stack owns. You run all three yourself; Claude Code never runs `up` or `destroy`. |
| **Lookups vs resources** | `networking.LookupNetwork` only *reads* an existing object, so `cstam-sandbox-net`, its subnet and `INTERNET` can never be modified or deleted by our program. Only objects created with `New...` belong to the stack. |

Two resource options matter here:

- `pulumi.Protect(true)` on the VIP port and the public floating IP: `pulumi destroy` refuses to delete them until you unprotect them on purpose. DNS points at that address; getting the same IP back after a delete is not guaranteed.
- `pulumi.IgnoreChanges(...)`: Nova cannot change the user data of a running VM, so editing a cloud-init file would make Pulumi **delete and recreate** the VM. We ignore `userData` and the image fields, and rebuild a VM only on purpose with `pulumi up --replace <urn>`. In the AAP fallback mode, the host ports also ignore `allowedAddressPairs`, because `cpd`'s syncer writes them at runtime.

::: why
**Why not keep bash scripts, Heat or Terraform?**
Bash with `openstack` commands has no plan and no state: idempotency is hand-written guards. Heat would earn the "native orchestration" bonus, but HOT YAML is a third language in the project and its errors are hard to read. Terraform/OpenTofu is close to Pulumi (the Pulumi OpenStack provider is built from the same Terraform provider, so the resources and fields are identical) but uses HCL. Pulumi in Go keeps one language, and `go build` catches mistakes before the cloud does. The cost: one more CLI to install, and a state file to back up.
:::

### Do it

#### 2.1 The Pulumi program `infra/pulumi`

```text
infra/pulumi/
├── Pulumi.yaml                # project: name cstam-infra, runtime go
├── Pulumi.prod.yaml.example   # stack config template (no secrets)
├── go.mod                     # separate Go module: Pulumi's dependencies stay out of cpd and the agents
├── config.go                  # IP plan (topo) + settings with validation
├── network.go                 # read-only lookups, router + router interface
├── secgroups.go               # 5 groups and the whole rule table
├── ports.go                   # VIP port first (protected), gateway/cp/host ports
├── servers.go                 # keypair, server group, instances on pre-created ports
└── main.go                    # wiring, floating IPs, stack outputs
```

The full source is in the **Code Companion, listing for Step 2**, and in the repository. It compiles with the Pulumi Go SDK v3.270 and the OpenStack provider `github.com/pulumi/pulumi-openstack/sdk/v5` v5.5.3 (that module asks for Go 1.26; the `go` command downloads that toolchain automatically). Read it in this order: `config.go`, `secgroups.go`, `ports.go`, `servers.go`, `main.go`. The heart of it is two tables.

The firewall policy, one rule per line (`secgroups.go`):

```go
rules := []rule{
    {group: "cstam-gw", desc: "public http", protocol: "tcp", portMin: 80, portMax: 80, remoteIP: "0.0.0.0/0"},
    {group: "cstam-gw", desc: "public https", protocol: "tcp", portMin: 443, portMax: 443, remoteIP: "0.0.0.0/0"},
    {group: "cstam-gw", desc: "ssh from cp", protocol: "tcp", portMin: 22, portMax: 22, remoteGroup: "cstam-cp"},
    {group: "cstam-vrrp", desc: "vrrp between gateways", protocol: "112", remoteGroup: "cstam-vrrp"},
    {group: "cstam-cp", desc: "gw agents to cp", protocol: "tcp", portMin: 8443, portMax: 8443, remoteGroup: "cstam-gw"},
    {group: "cstam-team", desc: "team http via gw", protocol: "tcp", portMin: 80, portMax: 80, remoteGroup: "cstam-gw"},
    // ... 14 rules in total, see the Code Companion
}
```

The VIP port, then the gateway ports that list it (`ports.go`):

```go
p.VIP, err = mk(spec{name: "cstam-vip", ip: topo.VIPIP, desc: "VRRP VIP - never attach to a server",
    groups: []*networking.SecGroup{sg.GW}, extra: []pulumi.ResourceOption{pulumi.Protect(true)}})
after := pulumi.DependsOn([]pulumi.Resource{p.VIP})          // VIP first: OVN makes it a virtual port
p.GW1, err = mk(spec{name: "cstam-gw1-port", ip: topo.GW1IP, groups: []*networking.SecGroup{sg.GW, sg.VRRP},
    pairs: []string{topo.VIPIP}, extra: []pulumi.ResourceOption{after}})   // exact IP, never a CIDR
```

Servers boot **on the pre-created ports** and never get `SecurityGroups` themselves, so the `default` group is never attached (`servers.go`):

```go
args := &compute.InstanceArgs{
    Name: pulumi.String(name), FlavorName: pulumi.String(flavor), KeyPair: kp.Name,
    UserData: pulumi.String(string(ud)),                                   // deploy/cloud-init/*.yaml
    Networks: compute.InstanceNetworkArray{compute.InstanceNetworkArgs{Port: port.ID()}},
}
compute.NewInstance(ctx, name, args, pulumi.IgnoreChanges([]string{"userData", "imageName", "imageId", "blockDevices"}))
```

#### 2.2 Run it

Install once: `curl -fsSL https://get.pulumi.com | sh` (check with `pulumi version`).

```bash
cd infra/pulumi
pulumi login file://~/.pulumi-cstam                      # local state, outside the repo
export PULUMI_CONFIG_PASSPHRASE='<from your password manager>'
pulumi stack init prod
pulumi config set openstack:cloud cstam                  # credentials from ~/.config/openstack/clouds.yaml
pulumi config set adminCidr "$(curl -s https://ifconfig.me)/32"
pulumi config set sshPublicKeyFile ~/.ssh/cstam_ed25519.pub
pulumi config set teamNetMode podnic                     # the Gate G1 decision: podnic | aap | aap-cidr
# only if FelCloud refuses boot from image:  pulumi config set bootVolumeGB 20
# when you need the second host:             pulumi config set withHostB true

pulumi preview          # read-only. Expected: "+ 35 to create", networkId/subnetId already filled in
pulumi up               # shows the same plan again, asks yes/no: you answer
pulumi stack output     # publicFip, cpFip, networkId, subnetId, hostAPortId, teamSecGroupId ...
```

Put `cpFip` into `~/.ssh/config` (Step 0) and `publicFip` into your notes (DNS / sslip.io). Step 6 reads the IDs for `cpd`'s environment file with `pulumi stack output -j`.

::: note
**What the preview proved already.** This program was checked with a read-only `pulumi preview` against FelCloud on 17 October 2026: the lookups resolved `cstam-sandbox-net` and its subnet, and the plan contained 35 resources and no errors. Nothing was created. Your first `pulumi preview` should show the same.
:::

::: warn
**Anti-affinity can fail with "No valid host was found".** FelCloud hides the hypervisor list from us (403), so we cannot know in advance how many hypervisors can host this flavor. If `cstam-gw2` fails: `pulumi config set serverGroupPolicy soft-anti-affinity` then `pulumi up` (the server group and the gateways are replaced). Record in the report which policy worked.
:::

#### 2.3 Images and flavors

| Role | Flavor | vCPU / RAM | Why |
|---|---|---|---|
| gw1, gw2 | `G0.basic.1c2g` | 1 / 2 GB | HAProxy and keepalived need little. "basic" rather than "shared" to reduce CPU steal, which can cause false VRRP failovers at 0.5 s adverts. |
| cp | `G0.basic.2c4g` | 2 / 4 GB | Go API, PostgreSQL 16, dashboard. |
| host-a, host-b | `G0.basic.4c8g` | 4 / 8 GB | Docker and team containers. host-b is optional: `pulumi config set withHostB true` when you need 2 hosts (cross-host tests, more teams). |

All on the image `Ubuntu 24.04 LTS - Noble Numbat`. Check the names on your account: `openstack flavor list -f value -c Name | grep G0.basic` and `openstack image list -f value -c Name | grep 24.04`.

#### 2.4 Cloud-init files (no secrets, ever)

User-data stays readable from inside the VM at `169.254.169.254` for the life of the VM, and from any container that can reach it. So cloud-init only installs packages and sets kernel parameters. Certificates and configuration are delivered later by `cstamctl node enroll` over SSH (Step 6).

```yaml
# deploy/cloud-init/gateway.yaml
#cloud-config
package_update: true
packages: [keepalived, socat, chrony, curl, jq, iputils-arping, tcpdump, nftables, software-properties-common]
write_files:
  - path: /etc/sysctl.d/90-cstam-gw.conf
    content: |
      net.ipv4.ip_nonlocal_bind = 1
      net.core.somaxconn = 4096
      net.ipv4.ip_local_port_range = 10240 65000
      net.ipv4.conf.all.arp_ignore = 1
      net.ipv4.conf.all.arp_announce = 2
  - path: /etc/cstam/role
    content: "gateway\n"
runcmd:
  - sysctl --system
  - add-apt-repository -y ppa:vbernat/haproxy-3.2
  - apt-get update
  - DEBIAN_FRONTEND=noninteractive apt-get install -y 'haproxy=3.2.*'
  - useradd -r -s /usr/sbin/nologin keepalived_script || true
  - useradd -r -s /usr/sbin/nologin -G haproxy cstam || true
  - install -d -o cstam -g haproxy -m 0775 /run/cstam /var/lib/cstam
  - systemctl enable --now chrony
```

`net.ipv4.ip_nonlocal_bind=1` lets HAProxy start even when this node does not hold the VIP. `arp_ignore=1` and `arp_announce=2` make sure only the VRRP master answers ARP for the VIP and announces it with the right source: OVN learns the VIP's location from ARP, so this matters.

```yaml
# deploy/cloud-init/host.yaml
#cloud-config
package_update: true
packages: [ca-certificates, curl, gnupg, chrony, jq, nftables, iproute2, iputils-arping, conntrack, ethtool, xfsprogs]
write_files:
  - path: /etc/sysctl.d/90-cstam-host.conf
    content: |
      net.ipv4.ip_forward = 1
      net.ipv4.conf.all.rp_filter = 1
      net.ipv4.conf.default.rp_filter = 1
      net.ipv4.conf.all.send_redirects = 0
      net.ipv4.conf.all.accept_redirects = 0
      net.ipv6.conf.all.disable_ipv6 = 1
  - path: /etc/docker/daemon.json
    content: |
      {
        "bridge": "none",
        "mtu": 1442,
        "iptables": true,
        "ip-forward": true,
        "userland-proxy": false,
        "live-restore": true,
        "no-new-privileges": true,
        "exec-opts": ["native.cgroupdriver=systemd"],
        "default-address-pools": [{ "base": "172.20.0.0/16", "size": 24 }],
        "dns": ["1.1.1.1", "9.9.9.9"],
        "log-driver": "json-file",
        "log-opts": { "max-size": "10m", "max-file": "3" }
      }
  - path: /etc/cstam/role
    content: "host\n"
runcmd:
  - sysctl --system
  - install -m 0755 -d /etc/apt/keyrings
  - curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
  - echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" > /etc/apt/sources.list.d/docker.list
  - apt-get update
  - DEBIAN_FRONTEND=noninteractive apt-get install -y docker-ce docker-ce-cli containerd.io
  - systemctl enable --now docker chrony
  - docker pull registry.k8s.io/pause:3.10
  - docker pull traefik/whoami:latest
```

```yaml
# deploy/cloud-init/controlplane.yaml
#cloud-config
package_update: true
packages: [postgresql-16, chrony, jq, curl, nftables]
write_files:
  - path: /etc/cstam/role
    content: "controlplane\n"
runcmd:
  - useradd -r -s /usr/sbin/nologin cstam || true
  - install -d -o cstam -g cstam -m 0750 /etc/cstam /var/lib/cstam
  - sudo -u postgres psql -c "CREATE ROLE cstam LOGIN" || true
  - sudo -u postgres createdb -O cstam cstam || true
  - systemctl enable --now chrony postgresql
```

PostgreSQL listens only on its Unix socket (Ubuntu's default `listen_addresses = 'localhost'`), and the `cstam` OS user connects with `peer` authentication: there is no database password to leak.

#### 2.5 Checks after `pulumi up`

```bash
openstack server list --name '^cstam-' -c Name -c Status -c Networks
# cstam-gw1 ACTIVE 10.20.0.11 ; cstam-gw2 ACTIVE 10.20.0.12 ; cstam-cp ACTIVE 10.20.0.5, <CP_FIP> ; cstam-host-a ACTIVE 10.20.0.21
for s in cstam-gw1 cstam-gw2; do openstack server show $s -f value -c hostId; done   # two DIFFERENT values
for s in cstam-gw1 cstam-gw2 cstam-cp cstam-host-a; do echo "$s: $(openstack server show $s -f value -c security_groups)"; done
# no "default" anywhere
openstack port show cstam-gw1-port -f json -c allowed_address_pairs | jq -c .   # [{"ip_address":"10.20.0.10",...}]
openstack quota show --usage -f value | grep -E 'secgroup|instances|cores'
ssh cstam-cp true && ssh cstam-gw1 'cloud-init status --wait; haproxy -v | head -1'   # HAProxy version 3.2.x
ssh cstam-host-a 'cloud-init status --wait; docker info --format "{{.ServerVersion}} live-restore={{.LiveRestoreEnabled}}"'
```

#### 2.6 Safe teardown: `pulumi destroy`

A teardown must never touch something that is not ours (`adam`, `sbx-f0699704`, `private_router`, FelCloud's shared networks). With Pulumi this is structural, not a matter of careful scripting:

1. The stack only knows the resources it created. The network, subnet and `INTERNET` are lookups, so they are not in the state and cannot be deleted.
2. `pulumi destroy` prints the full plan first and asks for confirmation. Pulumi orders the deletions from the dependency graph (floating IPs → servers → ports → groups → router interface → router).
3. The VIP port and the public floating IP are protected. To really remove them: `pulumi state unprotect --all`, then `pulumi destroy`.

Team ports are not in the stack: `cpd` owns them. Before destroying the platform, tear down all teams through the API (or `cstamctl gc` in Step 8), then check `openstack port list --tags cstam` is empty except the infrastructure ports.

This is also your "FelCloud-safe" proof for the pitch: `openstack port list`, `server list` and `floating ip list` before `pulumi up` and after `pulumi destroy` are identical.

### Build it in Go

The Pulumi program **is** the Go for this step: `go vet ./...` and `go build ./...` in `infra/pulumi` must pass before every `pulumi preview`. The control plane will only need **runtime** operations on Neutron (team ports, tags, attach/detach, allowed-address-pairs in the fallback), with gophercloud, not Pulumi. In Step 6 you add `cstamctl doctor`, which re-checks (read-only) everything listed in 2.5.

### Done when

::: done
**Done when**

- `openstack server list --name '^cstam-'` shows gw1, gw2, cp and host-a `ACTIVE` with the planned IPs.
- The two gateways have different `hostId` values (or you recorded that soft-anti-affinity was needed).
- No CSTAM server has the `default` security group.
- `cstam-gw1-port` and `cstam-gw2-port` have exactly one allowed-address-pair: `10.20.0.10`.
- `ssh cstam-gw1`, `ssh cstam-gw2` and `ssh cstam-host-a` work through the control plane (ProxyJump).
- `haproxy -v` on both gateways shows 3.2.x; `docker info` on host-a shows `live-restore=true`.
- A second `pulumi preview` right after `pulumi up` shows **no changes**, and `pulumi refresh` shows no drift.
- Security group rule count is below 50: `openstack security group rule list -f value | wc -l`.
:::

### Pitfalls

::: warn
**The `default` security group sneaks in** when a server is created with `--network` instead of `--port`. Always boot on pre-created ports. If you see `default` on a server, remove it: `openstack server remove security group <server> default`.
:::

::: warn
**cloud-init failures are silent.** If HAProxy is not version 3.2, the PPA step failed (often DNS or the router not yet ready). Read `/var/log/cloud-init-output.log` on the VM and re-run the failed commands by hand, then fix the YAML.
:::

::: warn
**Do not put the control plane behind the VIP for SSH.** The VIP's floating IP is only for HTTP(S) to the gateways. The control plane has its own floating IP; everything else is reached through it.
:::

### Learning checkpoint

::: learn
**Answer these yourself in docs/notebook/learning.md**

1. Why does a `cstam-team` group without a "self" rule block team-to-team traffic, even for two teams on the same host VM?
2. Why is it dangerous to use `cstam-host` as a remote group in the AAP model?
3. Why do we pre-create ports with fixed IPs instead of letting Nova pick addresses?
4. What exactly would leak if you put the agent certificate in cloud-init user-data?
5. What does `hostId` prove, and what does it not prove?
6. What is in the Pulumi state, what happens if you lose it, and why is the network a lookup and not a resource?
:::

### Driving Claude Code for this step

::: claude
**Prompts to use**

- "Read infra/pulumi. For each resource, tell me why it exists, what depends on it, and what `pulumi up` would do if I changed its name. Do not run any pulumi command."
- "Explain every Protect and IgnoreChanges option in infra/pulumi and what would go wrong without it."
- "I changed X in the stack config; here is my `pulumi preview` output. Explain each line marked ~ or +- before I run `pulumi up`."

**Review:** the VIP port is created before the gateway ports (`DependsOn`); no `SecurityGroups` on instances; no secret in any cloud-init file or stack config; `go vet` clean; the preview shows only `cstam-*` resources.
:::
