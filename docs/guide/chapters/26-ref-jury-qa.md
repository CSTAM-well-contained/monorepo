# 26. Reference C – Jury questions and strong answers

The jury is FelCloud's CEO and a cloud/DevOps engineer, plus the CSTAM committee. They will test two things: does it work, and do **you** understand it. The answers below are short on purpose. Learn the idea, not the words. Numbers in `[brackets]` must be replaced by your own measurements from the benchmark report; never quote a number you did not measure.

::: note
**How to answer**

1. Answer in one sentence first. 2. Give the mechanism in two or three sentences. 3. Point to evidence: a test name, a measurement, a dashboard screen. 4. If you do not know, say what experiment would tell you. Admitting a limit you have measured is stronger than a vague promise.
:::

## 26.1 Architecture choices

**1. Why not microservices?**
Because the code must run in three places, we have three daemons: the control plane, the gateway agent and the host agent. Inside the control plane we kept one binary with clear packages, because our key invariant ("one IP belongs to one team, and a route points to it only while the lease is active") must change in one database transaction. Splitting IPAM into its own service would turn IP allocation into a distributed transaction, which is exactly the kind of partial failure this challenge is about.

**2. Why not Kubernetes? FelCloud runs Rancher.**
Kubernetes solves scheduling, and scheduling is not the scored problem. The scored problems are the IP lifecycle on Neutron, gateway high availability and safe config reloads. Kubernetes would hide two of them inside controllers, pod IPs are not OpenStack IPs, and an overlay inside FelCloud's Geneve overlay would drop the MTU further. It also needs about three nodes for real HA, which our 10-instance quota does not allow.

**3. Why not Octavia? FelCloud already has it.**
We tested what it offers. Octavia L7 policies put the load balancer in `PENDING_UPDATE` on every change, so ten simultaneous registrations would queue or fail with 409. We also cannot crash an amphora live, because it lives in the service project. And TLS termination needs Barbican, which FelCloud does not run. Keepalived gives a failover we control and measured at `[X s]`. Octavia's `ovn` provider is our pre-built fallback if VRRP had failed in Step 1.

**4. Why one Docker host per hackathon instead of one VM per team?**
The quota allows at most 5–7 more VMs, and a VM took about 40 s to boot on FelCloud. The stress test needs 10+ teams in seconds. With the pod-NIC model each team still gets a real Neutron port, its own IP and its own security group enforcement, so the OpenStack-visible result is the same as a VM, at container speed.

**5. What is "pod-NIC"?**
For each team we create a Neutron port with a fixed IP from our pool, hot-plug it into the host VM, and move that network interface into the team's own network namespace, held by a small `pause` container. The team's containers join that namespace. HAProxy routes straight to the team IP: no NAT, no bridge. Neutron's security group on that port blocks team-to-team traffic, so isolation is enforced by OpenStack, not only by our firewall.

**6. Why Active/Standby and not Active/Active?**
Capacity is not the bottleneck: one HAProxy on one vCPU handles far more than hackathon traffic `[measured max rps]`. Active/Active would double the failure modes (split traffic, DNS round-robin, state) for capacity we do not need. Our standby is hot: same config generation, already listening, and it takes over in `[X s]`.

## 26.2 High availability

**7. How fast is failover, and what does "seamless" mean?**
Measured: HAProxy killed `[x s]`, keepalived stopped `[x s]`, VM crash `[x s]` p50 / `[x s]` p95 over `[n]` runs. "Seamless" for us means every **new** connection succeeds after that gap, with no human action. Open TCP connections on the dead gateway are lost, because HAProxy is a full proxy; browsers retry them.

**8. Why not sub-second on a VM crash?**
The backup must miss three adverts before it knows the master is dead: 3 × 0.5 s plus a skew. We could use 0.2 s adverts, but on a 1-vCPU VM that risks false failovers. We chose 0.5 s and measured it. Planned failover and HAProxy death are already sub-second.

**9. What about split brain?**
VRRP runs unicast, and only between the two gateways (security group `cstam-vrrp`). If adverts are lost anyway, both nodes become master. That is safe for routing because both have acknowledged the same configuration generation. The control plane detects two masters for more than 2 s and forces the lower one into FAULT. We tested it by dropping VRRP packets on gw2 (`CH-05`).

**10. What if both gateways die?**
Public traffic stops; that is the definition of the pair. Nothing is lost: all state is in the database, and a rebuilt gateway pulls the latest generation in seconds. Anti-affinity makes one hypervisor failure unable to kill both.

**11. Can a gateway that comes back with an old config take the VIP and serve old routes?**
No. At boot, the agent marks the gateway "not ready", and keepalived's check script keeps it in FAULT until the agent has synced to the current generation. That covers even a VM restored from an old image.

## 26.3 Reload and rollback

**12. How does a zero-downtime reload work?**
Adding or removing a team does not reload at all: we change the server address of a pre-declared slot backend and the host map through HAProxy's runtime API. Structural changes use master-worker mode: the new worker inherits the listening sockets, old workers finish their requests. Proof: `[60 000]` requests during `[20]` reloads and rollbacks, `[0]` failures.

**13. How do you catch a config that is valid but wrong?**
Two gates. Before: `haproxy -c` rejects bad syntax and the live config is never touched. After: we check that the new config version is the one serving and that a canary route answers. If not, we restore last-good in `[x ms]`.

**14. Why write the files on disk if you use the runtime API?**
Runtime changes live only in memory. The next reload would read the files and silently drop them. So the files always hold the full desired state, and the runtime API is just the fast path to the same state.

## 26.4 IPAM and lifecycle

**15. How do you guarantee two teams never get the same IP?**
Three independent locks. PostgreSQL picks a free row with `SELECT … FOR UPDATE SKIP LOCKED` inside a transaction. A partial unique index forbids two live leases on one IP. Neutron itself refuses a fixed IP that is already used. Test: 100 goroutines → 100 distinct IPs, and the stress test end to end.

**16. Explain the stale-route bug and why it cannot happen.**
If a port is deleted before its route, the IP can be given to team B while a gateway still sends team A's subdomain there. We remove the route first and the port last. An IP only leaves quarantine when **every** gateway has acknowledged a configuration generation that no longer contains the route, and Neutron shows no port holding it. A property test runs `[500]` random interleavings with crashes and a backup gateway going down, and checks after each action that no route can reach another team's IP.

**17. Why generation numbers from a single row instead of a sequence?**
With a sequence, transaction 6 can commit before transaction 5. A gateway could then acknowledge 6 while the removal in 5 is not visible yet: a false guarantee. Bumping one row holds a lock until commit, so generations become visible in order.

**18. Why the 60-second quarantine if you already have acknowledgements?**
Acks cover routes. Quarantine covers what acks cannot see: ARP caches, keep-alive connections, conntrack entries. It is hygiene on top of the safety rule. It is configurable; we report recycle time with and without it: `[x s]` / `[y s]`.

**19. What happens if the control plane crashes halfway through provisioning?**
Each team is a desired state in the database. On restart, a worker claims unfinished teams and continues. Every step first checks "is this already done?" (for example, it looks up the port by its fixed IP), so nothing is created twice. Test: kill `cpd` during a 20-team wave → 20 ACTIVE, 0 orphan ports.

**20. Is the control plane a single point of failure?**
Not for traffic. The data path is floating IP → VIP → HAProxy → team IP; the control plane is not on it. If it dies, existing sites, failover and local rollback keep working; only new registrations pause. We tested `[5]` minutes with `cpd` stopped: `[0]` failed requests. Backups go to Swift every 5 minutes; restore took `[N]` minutes.

## 26.5 Security

**21. Can a team reach another team, the metadata service or the gateways?**
No, and it is tested: our attack suite runs `[30]` probes from inside team containers, each with a positive control. Team to team is dropped by Neutron's security group and by the namespace firewall. Metadata, management IPs and private ranges are dropped by nftables inside each team namespace. Result: `[30/30]` blocked.

**22. A container escapes. What then?**
Containers run without capabilities, with `no-new-privileges`, a read-only root, and limits on CPU, memory and processes. The host has no OpenStack credential; only the control plane does, and that credential is restricted to port operations. The blast radius is one hackathon host. We say honestly that containers share a kernel; gVisor is an optional strict mode.

**23. Who can talk to your agents?**
Nobody: agents have no inbound API. They dial out to the control plane over mutual TLS with certificates from our own CA, TLS 1.3, identity checked from the certificate. The security groups also allow only the control plane to reach them.

**24. Where is the OpenStack credential, and what can it do?**
Only on the control-plane VM, readable only by the service user. It is an application credential restricted by access rules to the Neutron and Nova calls we need, with an expiry, and it cannot create new credentials.

## 26.6 Delivery and ownership

**25. What did the AI write, and do you understand it?**
I used Claude Code as a pair programmer under written rules: it explains before it writes, works in small reviewed changes, and never touches the cloud. I ran every infrastructure step by hand first. Ask me about any part: the IPAM lock, the teardown order, the reload pipeline, the VRRP timers.

**26. How would FelCloud run this in production?**
Everything is code: the platform is a Pulumi program in Go (`pulumi preview` shows every change before it happens), static binaries with systemd units, agents enrolled with one command. Configuration is versioned, every change is audited, and `make uninstall` proves we leave nothing behind. Next steps would be a Postgres replica for the control plane and Designate when FelCloud enables it.

**27. How does it scale beyond one /24?**
IPAM works per pool; adding a second subnet is a migration that adds rows. Hosts scale horizontally, HAProxy maps handle tens of thousands of entries, and the port quota is 500. The real limit is Neutron API throughput, which we rate-limit and measured at `[x]` port creations per second.

**28. What would you do with one more month?**
Move a host's teams to another host with the same IPs when it dies (re-home), a VM-per-team "premium" driver through Heat, and a Postgres replica for the control plane.

::: learn
**Learning checkpoint**

1. Pick five questions at random and answer them aloud in under 45 seconds each, without reading.
2. For each answer that uses a number, find the exact line in `docs/benchmark-report.md` that backs it.
3. Ask a friend (or Claude, in "jury mode") for three questions not in this list. Write your answers in `docs/notebook/learning.md`.
:::
