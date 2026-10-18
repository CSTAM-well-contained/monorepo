# Lab notebook

Every experiment, measurement and decision: command, output, date, conclusion.
Benchmark and technical reports are written from this file.

## Template
### E<n> – <title>  (date, who)
- Question:
- Commands (exact):
- Output (trimmed):
- Measurement (median / max, n runs):
- Conclusion / decision:
- Cleanup done: yes/no (resources deleted: ...)

---

### F1 – Project facts at start  (2026-10-16, read-only)
- Subnet `cstam-sandbox-subnet` 10.20.0.0/24, Neutron allocation pool only .200–.250 (.200 = OVN metadata port).
- MTU 1442 (Geneve/OVN). Quota: 10 instances, 20 vCPU, 50 GB RAM, 10 SG, 100 SG rules, 50 FIPs.
- Object storage is Ceph RGW with an S3 API at https://object-store.api.felcloud.io (used for Pulumi state).

### C1 – Clean-up of pre-existing VMs  (2026-10-17, team confirmed unused)
- Deleted VMs `adam` (SHUTOFF, 4c4g) and `sbx-f0699704` (10.20.0.68), their 2 boot volumes,
  the sbx port and its security group. Kept the OVN metadata port (10.20.0.200) and all FelCloud networks.
- 10.20.0.68 stays RESERVED in our IPAM until a Neutron scan confirms nobody uses it.

### D1 – First `pulumi up` of the platform  (2026-10-17)
- Router: `enable_snat` in external_gateway_info → **403 PolicyNotAuthorized** for non-admins.
  Fix: do not send it (SNAT is on by default). Router outside IP: 197.5.133.180.
- Gateways with **anti-affinity**: gw2 → ERROR "Exhausted all hosts available for retrying build failures".
  Switched to **soft-anti-affinity**: both ACTIVE but **same hostId** (same hypervisor).
  Consequence: HA covers VM / HAProxy / keepalived failures, not a hypervisor failure. Asked FelCloud.
- Floating IPs: **ExternalIpAddressExhausted** on INTERNET (197.5.133.0/24, pool .1–.230).
  No way in from outside until FelCloud frees IPs. Asked mentors for 2 floating IPs.
- Serial console log is empty on FelCloud (`openstack console log show`): use the VNC console URL instead.

### L1 – Gateway agent: reload and rollback against real HAProxy  (2026-10-17, laptop, Docker)
- Test: `go test -tags integration ./internal/gw/applier/` (haproxy:3.2 = 3.2.26, host network).
- 20 hitless reloads under 8 client loops (half keep-alive): **96,606 requests, 0 failed**. Slowest apply 211 ms.
- Invalid config (`haproxy -c` fails): rejected in 71 ms, old version keeps serving.
- Valid but broken config (healthz returns 500): check 85 ms, reload 61 ms, verify gave up after 500 ms,
  **rollback 73 ms**, total 0.72 s. Last good version serves again.
- Routes added, moved and removed at runtime (no reload); they survive a later reload.

### L2 – keepalived failover between two containers  (2026-10-17, laptop, Docker bridge)
- Test: `go test -tags integration ./internal/gw/keepalived/` (keepalived 2.3.3, VRRPv3 unicast, advert 0.5 s, nopreempt).
- VIP unreachable (client probing every 20 ms, 100 ms timeout):
  | Failure on the master | Outage |
  |---|---|
  | manual failover (`force-backup` = 1) | 0.20 s |
  | clean keepalived stop (priority-0 advert) | 0.20 s |
  | HAProxy killed, caught by `chk_gateway` (interval 1, fall 2) | 1.5 s |
  | master frozen (`docker pause`, silent failure) | 1.7 s (theory 1.8 s) |
- Two guide bugs found and fixed: `delay 0` in `vrrp_track_process` is refused (must be > 0, omit it);
  `track_interface` on the instance's own interface is refused (it is tracked implicitly).
- `vrrp_track_process` cannot be tested in a container (process events only reach the initial PID
  namespace). **To do on the real gateways**: expect HAProxy-kill failover well under 1 s with it.
- Not covered by Docker: OVN GARP/rebinding time on FelCloud. Re-measure on the VMs (Phase 1 demo).

## Decisions log
| Date | Decision | Based on | Alternatives rejected |
|---|---|---|---|
| 2026-10-16 | Backend in Go, modular monolith + 2 agents | guide ch. 6 ADR-01 | microservices |
| 2026-10-17 | IaC = Pulumi in Go, shared state in FelCloud S3 bucket `cstam-pulumi-state` | team of several people | local state, Pulumi Cloud, Heat, Terraform |
| 2026-10-17 | Admin access: SSH/dashboard open to any IP, key-only, fail2ban on cp | teammates on changing networks | IP allowlist, WireGuard |
| 2026-10-17 | soft-anti-affinity for the gateway server group | D1 | strict anti-affinity (fails on FelCloud) |
