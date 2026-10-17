# 25. Reference B – Failure matrix and operations runbook

This chapter is for the day of the event and for the demo. Part 25.1 tells you what the system does by itself when something breaks. Parts 25.2–25.11 are step-by-step procedures for the things **you** do by hand. Part 25.12 lists the metrics and alerts to watch.

::: warn
**Who runs what**

Every procedure that creates, changes or deletes an OpenStack resource is run by you, by hand, after reading it. Claude Code may prepare the commands; it never runs them. Commands assume `export OS_CLOUD=cstam` on your laptop and SSH through the control plane (`ssh -J ubuntu@<CP_FIP> ubuntu@10.20.0.x`).
:::

## 25.1 Failure matrix

| Failure | Detected by | Automatic response | What users see | What you do |
|---|---|---|---|---|
| gw1 VM crash / kernel panic | gw2 misses 3 VRRP adverts (1.5 s + skew) | gw2 becomes MASTER, GARP, OVN rebinds the VIP | ~1.8–2.2 s gap (expected, measure it); open connections reset | Nothing. Restart gw1 later; it rejoins as BACKUP (`nopreempt`) |
| HAProxy dies on the master | `vrrp_track_process` (netlink, instant) | Master goes FAULT, backup takes over | < 1 s gap | Check `journalctl -u haproxy`; systemd restarts it |
| keepalived stopped on master | Priority-0 advert | Backup takes over after skew (~0.3 s) | < 1 s gap | none |
| gwagent dies | Heartbeat missing > 6 s | systemd restarts it; agent re-syncs before `ready` | Nothing (HAProxy keeps serving) | none |
| Gateway comes back with an old map | `synced=false`, `/run/cstam/ready` missing | keepalived stays FAULT until synced | Nothing | none |
| Bad structural config pushed | `haproxy -c` | Rejected, live config untouched | Nothing | Read the stored stderr in Routing & Config |
| Valid but broken config | Verify step (healthz version + canary) | Restore last-good + reload (< 1 s) | At most a sub-second blip | Inspect the rejected version |
| Split brain (two MASTERs) | Both agents report MASTER > 2 s | `cpd` writes `force-backup` on the lower-priority node | Short flapping | Find the cause (SG `cstam-vrrp`, network); see 25.6 |
| Backup gateway down for long | `acked_generation` lag on dashboard | Released IPs stay in QUARANTINE (safe) | Nothing | If > 30 min: fence it (25.4) |
| hostagent dies | Heartbeat missing | systemd restart; team status `DEGRADED` meanwhile | Nothing (containers keep running) | none |
| Host VM reboot | Heartbeats stop, then resume | Agent re-plumbs every team from desired state | Teams on that host down ~60–90 s | Watch the dashboard |
| Host VM lost | Heartbeats gone | Teams `DEGRADED`, maintenance page (bonus) | Teams on that host down | Re-provision on host-b or tear down (25.8) |
| Team pod restarts | Docker event | Agent re-plumbs the netns | A few seconds | none |
| `cpd` crash | systemd | Restart; resync continues unfinished teams | No traffic impact; dashboard briefly down | none |
| Control-plane VM lost | Dashboard down | None; data plane serves last-good | No traffic impact; no new teams | Rebuild and restore (25.7) |
| PostgreSQL down | `/readyz` fails | API 503, controller pauses | No traffic impact | `systemctl status postgresql` |
| Neutron API slow / 5xx | Errors in `cstam_neutron_request_seconds` | Retry + backoff; teams wait in PROVISIONING | Slower registration | Tell the organizers; nothing else |
| Orphan port / stale route | Reconciler drift scan | Mark-and-sweep after 2 scans | Nothing | Read `gc.*` events |
| Reconciler wants to delete too much | Circuit breaker | Deletes nothing, raises an alert | Nothing | Investigate before re-enabling (25.10) |
| IP pool exhausted | IPAM | `503` with `Retry-After` | New teams refused | Shorten quarantine or tear down idle teams |

## 25.2 Planned (manual) failover

Use this before maintenance on gw1, or in the demo to show a zero-loss planned switch.

1. Dashboard → Gateways → gw1 → **Make backup (planned)**. The API writes `force-backup=1` into gw1's desired state.
2. gw1's agent writes `1` into `/run/cstam/force-backup`. keepalived's `vrrp_track_file` puts gw1 into FAULT, and gw2 takes the VIP.
3. Check:
   ```bash
   ssh gw2 'ip -br addr show ens3'          # 10.20.0.10/32 is now on gw2
   ssh gw2 'journalctl -u keepalived -n 5'  # "Entering MASTER STATE"
   curl -sI http://$FIP/ -H 'Host: team1.hacka.cstam.felcloud.tn' | grep X-Served-By   # gw2
   ```
4. To return: **Clear backup flag** on gw1. Because of `nopreempt`, gw2 stays MASTER. Use the same button on gw2 if you want gw1 to be master again.

The API refuses a planned failover if the peer is not healthy and synced (`force=true` overrides it, and the action is audited).

## 25.3 Simulated crash (demo mode only)

Enabled only with `CSTAM_DEMO_MODE=1` on `cpd`.

- From the dashboard: **Simulate crash** on the current master. The agent kills HAProxy with `SIGKILL` and masks its restart for 60 s.
- From the terminal (the real crash the jury asked for): `ssh gw1 'echo c | sudo tee /proc/sysrq-trigger'` (kernel panic), or `openstack server stop cstam-gw1`.
- Recovery: `openstack server start cstam-gw1` (or `reboot --hard`). gw1 boots, its agent syncs, it becomes ready and stays BACKUP.

## 25.4 Fence and rejoin a gateway

Use this when a gateway is down for a long time and its missing acks block IP recycling.

**Fence gw2:**
```bash
curl -X POST https://<cp>/v1/gateways/gw2:fence -H "Authorization: Bearer $TOKEN"
```
What `cpd` does, in this order (you can also do it by hand):
```bash
openstack port unset --allowed-address ip-address=10.20.0.10 cstam-gw2-port   # gw2 can no longer hold the VIP
openstack port set --disable cstam-gw2-port                                   # no traffic at all to gw2
# then in the DB: member=false, fenced_at=now()  -> quarantined IPs are freed at the next sweep
```
**Rejoin gw2** (after repair):
```bash
curl -X POST https://<cp>/v1/gateways/gw2:rejoin -H "Authorization: Bearer $TOKEN"
# = port set --enable, re-add allowed address 10.20.0.10, member=true, acked_generation=0
```
gw2 cannot win VRRP until its agent has synced to head (boot rule). Check `GET /v1/gateways` shows `synced=true` and the same generation as gw1.

## 25.5 Roll back a configuration

Dashboard → Routing & Config → pick version N → **Rollback to this version**. Technically this creates a new version N' that copies N's structural template (history is never rewritten). It goes through the normal pipeline (validate → swap → reload → verify) on both gateways. Routes are not rolled back: they always come from the current teams table.

By CLI: `curl -X POST https://<cp>/v1/config-versions/41:rollback -H "Authorization: Bearer $TOKEN"`.

## 25.6 Investigate a split brain

```bash
for g in 10.20.0.11 10.20.0.12; do ssh -J ubuntu@$CP_FIP ubuntu@$g 'hostname; ip -br addr show ens3; journalctl -u keepalived -n 3 --no-pager'; done
ssh gw2 'sudo timeout 3 tcpdump -n -i ens3 ip proto 112'   # expect one advert every 0.5 s from the master
openstack security group rule list cstam-vrrp              # proto 112 from cstam-vrrp must exist
```
Most likely causes: the `cstam-vrrp` group missing on one port, a paused VM resumed, or a network cut. Fix the cause; the lower-priority node steps down by itself once adverts flow again.

## 25.7 Restore the control plane

Backups: `pg_dump` every 5 minutes to the Swift container `cstam-backups` (cron on the cp).

1. Rebuild the VM with the foundation script part for `cstam-cp` (same port `cstam-cp-port`, same IP .5).
2. Install PostgreSQL and `cpd` (cloud-init + `make deploy-cp`).
3. Restore the newest dump:
   ```bash
   openstack object list cstam-backups --sort-key last_modified | tail -3
   openstack object save cstam-backups cstam-<timestamp>.dump --file /tmp/cstam.dump
   scp /tmp/cstam.dump cp:/tmp/ && ssh cp 'sudo -u postgres pg_restore -d cstam --clean /tmp/cstam.dump'
   ```
4. Start `cpd` with the reconciler in **dry-run** first (`CSTAM_RECONCILE_DRYRUN=1`). Read the planned actions. If they are sane, restart without dry-run.
5. Agents reconnect by themselves (they dial out to .5).

Measure this once and put the time in the benchmark report ("control plane destroyed → rebuilt in N minutes, 0 failed requests on existing teams").

## 25.8 Add, drain or retire a host

- **Add:** create port `cstam-host-b-port` (.22, SG `cstam-host`), boot the VM, enroll the agent (`cstamctl node enroll host-b`), register it: `POST /v1/hosts`. For pod-NIC, the warm pool for that host fills automatically.
- **Drain:** `POST /v1/hosts/host-a:drain`. The scheduler stops placing new teams there; existing teams keep running.
- **Retire:** drain, tear down or re-provision its teams, then `POST /v1/hosts/host-a:retire`. Only then delete the VM. The warm-pool ports go through normal teardown.

## 25.9 Rotate certificates and credentials

**Agent and cp certificates (30-day leaves):**
```bash
cstamctl pki issue --name gw1 --ip 10.20.0.11 --usage server,client --out pki/
cstamctl node enroll gw1 --certs-only        # copies over SSH, then: systemctl reload cstam-gwagent
```
Agents re-read their certificate on `SIGHUP`. Rotate the BACKUP gateway first, then fail over and rotate the other.

**OpenStack application credential** (on the cp only):
1. Create the new credential with the same access rules and a new expiry.
2. Write the new `clouds.yaml` to `/etc/cstam/secrets/` (mode 600), `systemctl restart cstam-cp`.
3. Check `cstamctl doctor` (read calls work), then delete the old credential.

## 25.10 Reconciler alerts and the circuit breaker

When the breaker fires, the reconciler logs the full deletion plan and deletes nothing.
```bash
journalctl -u cstam-cp | grep -A50 'circuit breaker'
cstamctl gc --dry-run      # shows what would be deleted and why
```
Typical causes: the DB was restored from an older backup, or `CSTAM_ENV` changed (tags no longer match). Never "fix" this by raising the threshold. Find out why the DB and Neutron disagree.

## 25.11 Clean uninstall (`make uninstall`)

FelCloud must be able to see that we leave nothing behind. The uninstall works only on resources tagged `cstam` or named `cstam-*`.

```bash
# 1. snapshot before
openstack port list -f value -c ID | sort > /tmp/ports.before   # (take this before the first install too)
# 2. uninstall: tear down teams via the API, then `cd infra/pulumi && pulumi destroy` (shows the plan, asks for confirmation; protected VIP port and public IP stay unless you unprotect them)
make uninstall
# 3. prove nothing is left
openstack port list --tags cstam -f value | wc -l          # 0
openstack server list --name '^cstam-' -f value | wc -l    # 0
openstack security group list -f value -c Name | grep -c '^cstam-'   # 0
openstack floating ip list -f value -c Port | grep -c . || true
diff /tmp/ports.before <(openstack port list -f value -c ID | sort)   # only pre-existing ports remain
```
The destroy script never touches `sbx-f0699704`, `adam`, `private_router`, `Rancher-mgmt`, `lb-mgmt-net`, `INTERNET` or `default`.

## 25.12 Metrics and alerts

| Metric | Meaning | Alert when |
|---|---|---|
| `cstam_gateway_vrrp_state{gw}` | 1 = MASTER per gateway | count(MASTER) ≠ 1 for > 2 s |
| `cstam_gateway_acked_generation{gw}` vs head | Generation lag | lag > 0 for > 10 s |
| `cstam_gateway_apply_total{result}` | applied / rejected / rolled_back | any `rolled_back` (info), rejected (warn) |
| `cstam_rollback_seconds` | Rollback duration | p95 > 1 s |
| `cstam_teams{phase}` | Teams per phase | PROVISIONING > 0 for > 120 s |
| `cstam_ipam_ips{state}` | IPs per state | FREE < 10 |
| `cstam_warm_pool{host}` | Warm ports per host | below target for > 5 min |
| `cstam_neutron_request_seconds{op,code}` | Neutron latency and errors | 5xx rate > 0.1/s |
| `cstam_reconcile_step_seconds{step}` | Time per provisioning step | none (benchmark input) |
| `cstam_reconciler_breaker_open` | Circuit breaker state | = 1 |
| `cstam_agent_last_seen_seconds{node}` | Heartbeat age | > 10 s |
| `cstam_logs_dropped_total` | Traffic log lines dropped | rising fast (info only) |
| HAProxy `haproxy_backend_status{proxy="be_slot…"}` | Slot health | team DOWN > 60 s |

During the event, keep the dashboard Overview open: it shows the same signals without Prometheus.

::: learn
**Learning checkpoint**

1. A jury member kills gw1. List, in order, what happens in the next 3 seconds and who does it (kernel, keepalived, OVN, HAProxy, agent, cpd).
2. Why does fencing remove the allowed address **and** disable the port, instead of only setting `member=false` in the database?
3. Why does the restore procedure start the reconciler in dry-run?
4. Which failures in 25.1 have **no** impact on visitor traffic, and why?
:::
