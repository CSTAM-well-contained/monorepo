# 17. Step 8 – Teardown, IP recycling and the reconciler

## Step 8 – Teardown, IP recycling and the reconciler

::: note
**Goal:** deleting a team (by the admin, by the team, by TTL expiry, or by a failed provisioning) removes everything in a strict, safe order, and its IP becomes reusable as fast as safety allows. One team's URL can never serve another team's app, even if a gateway is down, a host is unreachable, or `cpd` crashes in the middle. A reconciler continuously repairs drift between the database and the real world.
**Points served:** IPAM & reclamation (10), automated teardown & lifecycle security (10), stress test (15, the "no leaks" half), and much of the jury Q&A.
**Time estimate:** 3–4 days part-time.
**Depends on:** Step 7 (controller, Step interface, agent protocol, fake system). Gateways (Step 4) must ack with a digest.
:::

### Understand first

#### The bug we are preventing

`docs/design-v1/edge-cases.md` describes it: team A's sandbox is deleted, Neutron releases 10.20.0.57, a second later team B gets 10.20.0.57 during the stress test, but a stale route still sends `teamA.hacka…` to 10.20.0.57. Nothing crashes, but team A's URL now serves team B's app. That is a **security bug** (wrong data to the wrong people), and it is exactly the "IP conflict" the challenge brief talks about.

We state what must never happen as a property, and design every rule in this chapter to protect it.

::: why
**Safety property S (no cross-team delivery)**

A request whose `Host` is the FQDN of team T is never delivered to a backend that belongs to another team T′.

For a violation, two things must be true **at the same moment**: (1) some gateway that can receive traffic on the VIP maps `fqdn(T) → x`, and (2) IP `x` is bound to team T′ on some host. (2) requires T′ to have leased `x`, which requires `x` to go `QUARANTINE → FREE`. So if we make `QUARANTINE → FREE` impossible while any gateway that can receive traffic still maps `fqdn(T) → x`, the violation cannot happen.
:::

#### Nothing cleans itself up

| Resource | What happens on its own | Consequence if we forget |
|---|---|---|
| Gateway route | nothing: HAProxy knows nothing about OpenStack | route points at an IP that may be reused |
| Neutron port | stays until deleted; once deleted, its IP is free in Neutron at once | IP leak, or early reuse by someone with `--fixed-ip` |
| Attached NIC (pod-NIC) | deleting the team's containers puts the NIC back in the host root namespace; it stays attached | port cannot be deleted cleanly; Nova slot wasted |
| Allowed-address pair (AAP mode) | stays on the host port | the host can still answer for the IP |
| conntrack entries (AAP mode DNAT) | up to 5 days | established flows keep going to the old place |
| DNS | wildcard record: nothing to delete | the gateway must answer 404 for unknown hosts |

#### The strict order, and why

![Figure 17.1 – Teardown order (left) and the gateway acknowledgement gate (right). An IP returns to FREE only when every member gateway has a verified map without the old route.](diagrams/f12-teardown-acks.png)

| # | Step | Done-check | If the component is down |
|---|---|---|---|
| 1 | `desired = ABSENT`, phase `DELETING`, `BeginRelease` (IP → RELEASING) | – | – |
| 2 | **Remove the route**: one tx sets `routes.removed_generation = Gr` (bump `config_head`) and `NOTIFY` | route row has `removed_generation` | – |
| 3 | **Wait for the MASTER's ack ≥ Gr** (the subdomain now answers 404) | `gateways.acked_generation` | MASTER unreachable → wait (requeue); the backup's ack is required later, at step 8 |
| 4 | **Host cleanup** via host desired state: containers, pod, namespace firewall (pod-NIC) / bridge, DNAT/SNAT, firewall lines, `ip addr del`, conntrack flush (AAP) | host `applied_rev ≥` the revision without the team, and the agent no longer lists it | host unreachable > 60 s → continue; step 5 fences it; the agent purges the leftovers when it comes back |
| 5a | **pod-NIC: detach the NIC** from the host VM (Nova) | port `device_id` empty | must succeed; retry with backoff + alert |
| 5b | **AAP: remove the pair** from the host port (syncer) | host port pairs do not contain `x/32` | must succeed; retry + alert |
| 6 | **Delete the Neutron port** | `GET ports?fixed_ips=ip_address=x` is empty → `MarkNeutronAbsent` | must succeed; retry |
| 7 | `QuarantineIP(lease, Gr)`; lease RELEASED; team `DELETED` (or `FAILED` if it was a rolled-back provisioning) | rows | – |
| 8 | **Sweeper**: `FREE` when quarantine time passed ∧ `min(acked_generation of member gateways) ≥ Gr` ∧ fresh Neutron-absent | – | backup down → IP stays in QUARANTINE (safe), dashboard shows "blocked by gw2 ack" |

::: why
**Why route first**

As long as a route `fqdn(T) → x` exists anywhere, `x` must stay unusable for anyone else. Route removal ends the *meaning* of `x` for team T. Every other step ends only the *ability* to serve. If the port were deleted first, Neutron would consider `x` free (the edges.md bug). Our split pool (Chapter 15) keeps Neutron from *auto*-assigning `x`, but anyone with an explicit `--fixed-ip` could still take it.

**Why detaching the NIC / removing the AAP comes before deleting the port**

This step is our **fence** against a host we cannot reach. In pod-NIC mode, after detach the NIC no longer exists in the host VM, whatever the host agent believes. In AAP mode, once `x` is no longer in the host port's allowed pairs, OVN port security drops every packet sourced from `x` on that host (including ARP replies). So a zombie host that still has `x` configured can neither answer for `x` nor send as `x`. In AAP mode the reserved port is an OVN *virtual* port whose parent is the host port; deleting it while the pair still exists can leave a stale binding.

**Why the port is last**

The Neutron port is Neutron's own lock on `x`. We keep it until nothing in our system references `x` any more (no route, no NIC, no pair). After that, Neutron is the only thing that could hand `x` out, and the pool split makes that impossible without an explicit request, which our IPAM only makes after FREE.
:::

#### Generations: a version number for the routing table

Every change to the routing table gets a new **generation** number. Gateways apply full snapshots of the table at a generation, and acknowledge the generation they applied, with a digest (a SHA-256 hash of the sorted map). The rule "an IP may only be reused once every gateway has acknowledged a generation that no longer contains the old route" is then one comparison of two numbers.

There is a trap: the **commit order**. If generation numbers came from a sequence (`nextval`), transaction A could take generation 5 and transaction B generation 6, and B could commit first. A gateway would then fetch head = 6, apply it and ack 6, while A's change (generation 5, maybe a route removal) is not even visible yet. The ack "≥ 5" would be a lie.

Fix: bump a **single row** `config_head` with `UPDATE … RETURNING`. The row lock is held until commit, so generation `n+1` cannot commit before `n`. Whenever a reader sees head = G, all generations ≤ G are committed.

```sql
-- name: BumpGeneration :one
UPDATE config_head SET generation = generation + 1 RETURNING generation, structural_version;

-- name: RemoveRoutes :execrows
UPDATE routes SET removed_generation = sqlc.arg(gen)
WHERE team_id = sqlc.arg(team_id) AND removed_generation IS NULL;

-- name: SnapshotAt :many   -- run in a REPEATABLE READ tx right after reading the head
SELECT fqdn, slot, host(target_ip)::text AS ip, target_port FROM routes
WHERE added_generation <= sqlc.arg(gen) AND (removed_generation IS NULL OR removed_generation > sqlc.arg(gen))
ORDER BY fqdn;

-- name: AckGeneration :execrows
UPDATE gateways g SET acked_generation = GREATEST(g.acked_generation, sqlc.arg(gen)),
       acked_digest = sqlc.arg(digest), acked_at = now(), last_seen_at = now()
WHERE g.name = sqlc.arg(name) AND g.member
  AND EXISTS (SELECT 1 FROM config_versions v WHERE v.generation = sqlc.arg(gen) AND v.map_digest = sqlc.arg(digest));
```

Notes:

- `GREATEST(...)`: an out-of-order or late ack can never make the acknowledged generation go **backwards**.
- The `EXISTS … map_digest` clause: the gateway proves it applied *exactly* the snapshot of that generation. An agent bug cannot fake an ack by accident.
- A **publisher** coalesces route changes that arrive within 50 ms into one generation: 50 teardowns at the end of a hackathon produce a handful of generations, not 50 reloads.

#### Gateway agent rules (implemented in Step 4, required here)

1. Pull the **full snapshot** for the head generation (`WatchGateway`). Never apply deltas.
2. **Never apply a generation lower than the one already applied.**
3. Apply: write `hosts.map` atomically, then update memory via the runtime API (Chapter 13). For removed routes: `del map`, put the slot in maintenance, `shutdown sessions server be_slotNNN/s` (kills WebSockets), park the slot address, and `ip neigh del <ip>` to drop the ARP entry.
4. Verify: `show map` → recompute the digest → it must equal the snapshot's digest. Only then `Ack(gen, digest)`.
5. **Boot rule:** at boot the agent removes `/run/cstam/ready`. Keepalived's `chk_gateway` requires that file, so the gateway stays in FAULT and **cannot take the VIP** until it has synced to the control plane's head generation. If the control plane is unreachable for 10 s, the agent becomes ready with its last-good map (a stale gateway is better than no gateway), reports `synced=false`, and the dashboard shows it.

#### Proof sketch that S holds (including a dead backup gateway)

Let Gr be the generation that removed `fqdn(T) → x`.

1. `x` becomes FREE only if every gateway with `member = true` has `acked_generation ≥ Gr` (the sweeper SQL).
2. A member that acked G′ ≥ Gr had, at ack time, a verified map equal to snapshot(G′), and snapshot(G′) does not contain `fqdn(T) → x`. A removed route never reappears in a later snapshot, because route rows are never "un-removed". A *rollback* restores only the HAProxy structural template; routes always come from the current table.
3. The agent applies only increasing generations and persists the map before acking. After a reboot it holds ≥ G′, or it is not VRRP-eligible (boot rule) until it syncs.
4. A gateway with `member = false` has `fenced_at` set. Fencing removes the VIP from its port's allowed pairs **and** sets `admin_state_up = false` on its port, so it cannot receive VIP traffic whatever keepalived believes.
5. Therefore, when `x` is FREE, no gateway able to receive traffic maps `fqdn(T) → x`, so condition (2) of a violation can only start after condition (1) became impossible. ∎

**Liveness** (things eventually make progress): if gw2 is down, IPs released meanwhile stay in QUARANTINE, visible as "blocked by gw2 ack". With 159 team IPs and ~50 teams this is harmless for hours. For a long outage, the admin runs `POST /v1/gateways/gw2:fence`; the backlog frees at the next sweep. Subdomain reuse is independent: `team1` can be registered again as soon as the old team is DELETED, because the new team gets a **new** IP; a stale gateway would at worst answer 503 for it, never serve someone else's app.

#### Quarantine: what the 60 seconds are for

The safety gate is **not** time: it is the acknowledgements plus "Neutron has no port with this IP". Quarantine is hygiene on top, sized for effects the acks do not cover:

| Leftover after route removal | Typical lifetime | Covered by |
|---|---|---|
| Gateway ARP entry for the old IP → host MAC | 30 s reachable (15–45 s random) + 5 s probe delay | quarantine 60 s + gateway agent `ip neigh del` + GARP from the new owner |
| HAProxy idle keep-alive server connections | `timeout http-keep-alive 5s` | quarantine |
| WebSocket / SSE streams through HAProxy | `timeout tunnel 1h` | **not** time: the agent runs `shutdown sessions` before acking |
| conntrack DNAT entries on the old host (AAP mode) | up to 5 days | host agent `conntrack -D -d <ip>` in cleanup |
| OVN binding of the old NIC / virtual port | until detach / next GARP | detach (pod-NIC) or AAP removal + GARP |

So 60 s is the default. For the "IP recycling speed" benchmark, report two numbers: **safe-to-reuse** (gated by acks and Neutron, typically seconds) and **reusable** (plus quarantine). During the live demo set `CSTAM_QUARANTINE=20s` so the jury sees the cell turn FREE.

#### Lease expiry and renew

```sql
-- name: RenewLease :one
UPDATE leases l SET expires_at = least(now() + sqlc.arg(secs)::int * interval '1 second', t.max_expires_at)
FROM teams t
WHERE l.id = sqlc.arg(lease_id) AND l.team_id = t.id
  AND l.state = 'ACTIVE' AND l.expires_at > now()      -- no resurrection of an expired lease
  AND t.desired = 'PRESENT'
RETURNING l.expires_at;

-- name: ExpireLeases :many    -- expiry sweeper, leader, every 5 s
UPDATE teams t SET desired = 'ABSENT', spec_version = spec_version + 1,
       failure_reason = CASE WHEN l.state = 'PENDING' THEN 'provision_timeout' ELSE failure_reason END
FROM leases l
WHERE l.team_id = t.id AND l.state IN ('PENDING','ACTIVE') AND l.expires_at <= now()
  AND t.desired = 'PRESENT'
RETURNING t.id;

-- name: MarkExpiring :exec
UPDATE teams t SET phase = 'EXPIRING'
FROM leases l
WHERE l.team_id = t.id AND l.state = 'ACTIVE' AND t.phase = 'ACTIVE'
  AND l.expires_at - now() < interval '10 minutes';
```

`POST /v1/teams/{id}:renew` returns `409 lease_expired_or_not_active` when the update matches 0 rows. Renew moves `EXPIRING` back to `ACTIVE`. Expiry only sets `desired = ABSENT`; the normal teardown does the rest.

### Build it in Go

#### 8.1 The teardown steps

The controller from Step 7 picks `c.Teardown` when `desired = ABSENT`. Each step follows the same `Done/Apply` contract, and the list order **is** the safety order: a step never runs before the previous one reported Done.

```go
// internal/lifecycle/teardown.go
func TeardownSteps(d Deps) []Step {
	steps := []Step{
		markDeleting{d},      // phase -> DELETING, BeginRelease (IP -> RELEASING)
		removeRoutes{d},      // routes.removed_generation = Gr (one tx with BumpGeneration); remembers Gr
		waitMasterAck{d},     // MASTER acked >= Gr  -> subdomain answers 404
		hostCleanup{d},       // team removed from host desired state; wait for agent ack (or fence after 60 s)
	}
	if d.NetMode == NetModePodNIC {
		steps = append(steps, detachNIC{d}) // Nova detach, wait device_id == ""
	} else {
		steps = append(steps, removeAAP{d}) // kick per-host syncer, wait pair absent
	}
	return append(steps,
		deletePort{d},  // 404 counts as success; then GET by fixed IP -> MarkNeutronAbsent
		quarantine{d},  // QuarantineIP(lease, Gr); lease RELEASED; team DELETED or FAILED
	)
}
```

```go
type removeRoutes struct{ d Deps }

func (s removeRoutes) Name() string           { return "routes.remove" }
func (s removeRoutes) Timeout() time.Duration { return 2 * time.Second }
func (s removeRoutes) Done(ctx context.Context, t *Team) (bool, error) {
	gr, live, err := db.New(s.d.Pool).TeamRouteRemoval(ctx, t.ID) // max(removed_generation), count(live)
	if err != nil {
		return false, err
	}
	if live == 0 {
		t.RemovedGeneration = gr // 0 if the team never had a route
		return true, nil
	}
	return false, nil
}
func (s removeRoutes) Apply(ctx context.Context, t *Team) error {
	return ipam.InTx(ctx, s.d.Pool, func(q *db.Queries, tx pgx.Tx) error {
		if _, err := q.LockTeamFenced(ctx, db.LockTeamFencedParams{ID: t.ID, Epoch: t.Epoch}); err != nil {
			return ErrLostClaim
		}
		gen, err := q.BumpGeneration(ctx) // lock order: team -> config_head
		if err != nil {
			return err
		}
		if _, err := q.RemoveRoutes(ctx, db.RemoveRoutesParams{TeamID: t.ID, Gen: gen.Generation}); err != nil {
			return err
		}
		if err := insertConfigVersion(ctx, q, gen, "ROUTES", "remove "+t.Slug); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "SELECT pg_notify('cstam_gw', $1)", strconv.FormatInt(gen.Generation, 10))
		return err
	})
}
```

```go
type hostCleanup struct{ d Deps }

func (s hostCleanup) Done(ctx context.Context, t *Team) (bool, error) {
	st, err := db.New(s.d.Pool).HostTeamRemoval(ctx, db.HostTeamRemovalParams{HostID: t.HostID, TeamID: t.ID})
	if err != nil {
		return false, err
	}
	if st.AppliedRev >= st.RemovalRev && !st.StillListed {
		return true, nil
	}
	if st.HostUnreachableFor > 60*time.Second {
		// Do not wait forever for a dead host: the next step (detach / AAP removal) is the fence.
		s.d.Events.Emit(ctx, "host.fenced_for_team", t.ID, st.HostName)
		return true, nil
	}
	return false, &ErrWaiting{After: time.Second, Reason: "waiting for host agent cleanup"}
}
func (s hostCleanup) Apply(ctx context.Context, t *Team) error {
	return s.d.Hosts.RemoveTeamFromDesired(ctx, t.HostID, t.ID) // desired_rev++, host_teams row marked removing, NOTIFY
}
```

```go
type deletePort struct{ d Deps }

func (s deletePort) Done(ctx context.Context, t *Team) (bool, error) {
	p, err := s.d.Neutron.PortByFixedIP(ctx, t.IP) // fresh GET, never cached
	if err != nil {
		return false, err
	}
	if p == nil {
		return true, db.New(s.d.Pool).MarkNeutronAbsentForLease(ctx, t.LeaseID) // recorded after QUARANTINE too (sweeper re-checks)
	}
	if !p.HasTag("cstam-lease:" + t.LeaseID.String()) {
		// someone else's port has this IP now: never delete it; alert and keep our lease in RELEASING
		return false, &FatalForSystem{Reason: "foreign port on releasing IP " + t.IP.String()}
	}
	return false, nil
}
func (s deletePort) Apply(ctx context.Context, t *Team) error {
	return s.d.Neutron.DeletePort(ctx, t.PortID.String()) // 404 => nil
}
```

The `quarantine` step calls `IPAM.Quarantine(lease, Gr)`. Its SQL refuses if any live route still targets the IP, so even a reordered step list in a future refactor could not skip the route removal.

::: why
**Why `MarkNeutronAbsent` is checked again by the sweeper**

`QuarantineIP` resets `state_changed_at`. The sweeper requires `neutron_absent_at >= state_changed_at`, i.e. a "no port" observation made **after** entering quarantine. The reconciler's Neutron scan (every 30 s) refreshes it for every QUARANTINE IP. So the IP is freed only on a fresh observation, never on an old one.
:::

#### 8.2 Fencing and rejoining a gateway

Two admin endpoints, both audited:

| Endpoint | What it does, in order |
|---|---|
| `POST /v1/gateways/{name}:fence` | 1. remove the VIP 10.20.0.10 from that gateway port's allowed pairs (If-Match). 2. `admin_state_up = false` on its port. 3. `member = false, fenced_at = now()` in one tx. Now the sweeper ignores it, and it physically cannot receive VIP traffic. |
| `POST /v1/gateways/{name}:rejoin` | 1. `admin_state_up = true`. 2. re-add the VIP pair. 3. `member = true, fenced_at = NULL, acked_generation = 0, synced = false`. The boot rule keeps it in FAULT until it has synced to the head. |

The CHECK constraint `member OR fenced_at IS NOT NULL` makes "drop a gateway from the quorum without fencing it" impossible at the database level.

#### 8.3 The reconciler

The controller makes each team converge when something changes. The **reconciler** catches what changed *without* telling us: a port deleted by hand, a host rebooted, an agent bug, a crash in the middle of the warm refiller.

![Figure 17.2 – The reconciler compares the database with Neutron, the host ports, the host agents and the gateway agents, and repairs each kind of drift. It never touches objects that are not tagged as ours.](diagrams/f13-reconciler.png)

| Loop | Interval | Leader only? | Purpose |
|---|---|---|---|
| Controller workers (Step 7) | NOTIFY + 1 s poll | no (claims) | drive each team to its desired state |
| Expiry sweeper | 5 s | yes | EXPIRING flags, `desired = ABSENT` at TTL and at the PENDING deadline |
| IP sweeper (`FreeEligibleIPs`) | 2 s | yes (idempotent anyway) | QUARANTINE → FREE |
| Neutron drift scan | 30 s + on demand | yes | ports by tag vs leases and warm IPs; foreign ports in our range; NIC attachments; AAP lists |
| Gateway drift | each heartbeat (2 s) | no | ack lag, digest mismatch, two MASTERs |
| Host drift | each heartbeat (2 s) + full diff every 60 s | no | the agent's team list vs the DB |
| Invariants check | 60 s | yes | run the invariants SQL of Chapter 15; any row → alert + dashboard banner |

"Ours" means tags `cstam` **and** `cstam-env:<this env>`. Anything else is never modified, only reported.

| Drift (observed vs DB) | Detection | Repair | Guard |
|---|---|---|---|
| Orphan port: tagged ours, lease unknown or RELEASED, not a warm port | `ports?tags=cstam,cstam-env:prod` vs `leases` and warm IPs | mark-and-sweep: remember `suspect_since`; on the next scan, if still orphan and created > 2 min ago, detach (if attached) and delete | team not claimed; circuit breaker |
| Warming IP with NULL port older than 2 min | DB | find port by fixed IP + tag `cstam-warm:<host>` → adopt (`MarkWarm`); else put the IP into the quarantine path | – |
| Lease LEASED/ACTIVE but no port | scan | `next_attempt_at = now()` on the team; its `teamPort` step recreates the port with the same fixed IP | if the IP is now foreign → team DEGRADED, re-lease |
| Port exists but detached (pod-NIC) for an ACTIVE team | `device_id` empty | team `next_attempt_at = now()`; `attachNIC` re-attaches | – |
| Foreign port in our range (no `cstam` tag) | ports whose fixed IP ∈ our pools | `MarkForeign` if FREE; alert if leased (conflict) | **never delete** |
| FOREIGN IP no longer used | 2 consecutive scans show no port | FOREIGN → FREE | – |
| Host port AAP has an extra or missing pair (AAP mode) | pairs vs desired | the syncer writes the desired list | only `/32`s inside our pools, or our `/27` |
| Gateway acked < head for > 10 s | `gateways` | poke the agent; teams `DEGRADED`; alert | – |
| Gateway digest ≠ expected | ack rejected | agent forced to full resync | – |
| Both gateways MASTER for > 2 s | heartbeats | alert "split brain"; set `force_backup` on the lower-priority one | – |
| Host agent reports a team not in the DB (or deleted) | heartbeat list | remove it via host desired state (the agent purges) | only names `cstam-*` |
| DB team ACTIVE but the agent lacks it (host reboot) | `applied_rev` behind or team missing | the agent re-applies its full desired state | – |
| QUARANTINE IP without fresh `neutron_absent_at` | DB | `GET ports?fixed_ips=ip_address=<ip>`; empty → `MarkNeutronAbsent` | – |

::: warn
**Circuit breaker (do not skip this)**

If one scan would delete more than `max(5, 10 % of our ports)` objects, the reconciler deletes **nothing**, logs the plan, and raises a red alert on the dashboard. This protects against the disaster "the database was restored from yesterday's backup → everything looks orphaned → the reconciler wipes the whole event". A human then runs `cstamctl gc --dry-run` and decides.
:::

```go
// internal/reconcile/neutron.go
func (r *Reconciler) ScanNeutron(ctx context.Context) error {
	ours, err := r.n.ListPortsByTags(ctx, "cstam", "cstam-env:"+r.env)
	if err != nil {
		return err
	}
	inRange, err := r.n.ListPortsInRange(ctx, r.poolStart, r.poolEnd) // all ports, filtered by fixed IP in our pools
	if err != nil {
		return err
	}
	plan := r.diff(ctx, ours, inRange) // pure function: easy to unit-test
	if len(plan.Deletes) > max(5, len(ours)/10) {
		r.alert(ctx, "reconciler circuit breaker", plan)
		return ErrCircuitOpen
	}
	for _, a := range plan.Actions() { // adopt, mark foreign, free foreign, mark absent, kick team, delete orphan
		if err := a.Do(ctx); err != nil {
			slog.Warn("reconcile action", "action", a, "err", err)
		}
	}
	return nil
}
```

Keep `diff` a pure function of `(db snapshot, neutron ports, now)`, so `TestReconcileDiff` can cover every row of the drift table with plain data.

`cstamctl gc --dry-run` prints the same plan without executing it. Run it before every demo.

### Tests

These tests are the most valuable code in the project: they turn "we think the order is safe" into "we proved it for every interleaving we could generate". Put them in `internal/lifecycle/safety_test.go`.

**DB clock control.** Tests need to move time forward without sleeping. Use a SQL function instead of `now()` in the sweeper and expiry SQL:

```sql
-- migrations/00004_test_clock.sql
CREATE FUNCTION cstam_now() RETURNS timestamptz LANGUAGE sql STABLE AS $$
  SELECT now() + coalesce(nullif(current_setting('cstam.now_offset', true), '')::interval, '0'::interval)
$$;
```

In production `cstam.now_offset` is never set, so `cstam_now() = now()`. Tests set it per session (`SET cstam.now_offset = '61 seconds'`).

**1. The edges.md scenario.** The fake Neutron hands out the lowest free IP to anyone who asks without a fixed IP (like Neutron would if our pool split were broken). The test asserts the routing map never maps team A's FQDN to an IP leased to team B, for every step boundary.

```go
func TestEdgesMdScenario(t *testing.T) {
	sys := testutil.NewSystem(t, testutil.WithQuarantine(0)) // worst case: no hygiene delay
	a := sys.API.CreateTeam(t, "hacka", "teama", "k1")
	sys.RunUntil(t, 10*time.Second, func() bool { return sys.Phase(a) == "ACTIVE" })
	ipA := sys.LeaseIP(a)

	sys.Gateway("gw2").SetDown(true) // backup does not ack
	sys.API.DeleteTeam(t, a)
	sys.RunUntil(t, 10*time.Second, func() bool { return sys.Phase(a) == "DELETED" })

	// Lease every other IP; ipA must never be handed out while gw2 still has the old route.
	for i := 0; i < 158; i++ {
		id := sys.API.CreateTeam(t, "hacka", fmt.Sprintf("t%03d", i), fmt.Sprint(i))
		sys.Step(5)
		require.NotEqual(t, ipA, sys.LeaseIP(id))
		sys.CheckSafety(t) // property S over every fake gateway that can receive traffic
	}
	sys.Gateway("gw2").SetDown(false) // gw2 syncs, acks the head
	sys.RunUntil(t, 5*time.Second, func() bool { return sys.IPState(ipA) == "FREE" })
}
```

**2. Backup down during teardown** (`TestTeardownWithBackupDown`): gw2 agent stopped → delete team1 (IP x) → x stays QUARANTINE and the dashboard reason says "blocked by gw2" → start gw2 → within 5 s x becomes FREE. A variant fences gw2 instead of restarting it: x becomes FREE after the fence, and `gw2`'s port is admin-down in the fake.

**3. Commit order of generations:**

```go
func TestGenerationCommitOrder(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	tx1, _ := pool.Begin(ctx)
	var g1 int64
	require.NoError(t, tx1.QueryRow(ctx, "UPDATE config_head SET generation = generation + 1 RETURNING generation").Scan(&g1))
	done := make(chan int64)
	go func() { // tx2 must block on the row lock until tx1 commits
		tx2, _ := pool.Begin(ctx)
		var g2 int64
		_ = tx2.QueryRow(ctx, "UPDATE config_head SET generation = generation + 1 RETURNING generation").Scan(&g2)
		_ = tx2.Commit(ctx)
		done <- g2
	}()
	select {
	case <-done:
		t.Fatal("tx2 committed before tx1")
	case <-time.After(300 * time.Millisecond):
	}
	require.NoError(t, tx1.Commit(ctx))
	require.Equal(t, g1+1, <-done)
}
```

**4. No resurrection on rollback** (`TestNoResurrectOnRollback`): delete a team, then roll back the structural HAProxy template to the version that existed while the team was live → the new snapshot does **not** contain the team's route.

**5. Ack regressions** (`TestAckNeverGoesBack`): ack 10, then a late ack 7 → `acked_generation` stays 10. Ack with a wrong digest → rejected, unchanged.

**6. Property-based test with `pgregory.net/rapid`:** random interleavings of register, delete, controller ticks, gw2 down/up, worker crash, Neutron faults and DB clock jumps; after every action check property S; at the end, turn faults off, run until stable, and check convergence (DB = fake Neutron = fake agents = fake gateway maps, no orphans).

```go
func TestLifecycleProperties(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		sys := testutil.NewSystem(rt)
		rt.Repeat(map[string]func(*rapid.T){
			"register":   func(rt *rapid.T) { sys.Register(rapid.StringMatching(`[a-z]{1,3}`).Draw(rt, "slug")) },
			"delete":     func(rt *rapid.T) { sys.DeleteRandom(rt) },
			"tick":       func(rt *rapid.T) { sys.Step(rapid.IntRange(1, 20).Draw(rt, "steps")) },
			"gw2Down":    func(rt *rapid.T) { sys.Gateway("gw2").SetDown(true) },
			"gw2Up":      func(rt *rapid.T) { sys.Gateway("gw2").SetDown(false) },
			"crash":      func(rt *rapid.T) { sys.CrashWorker() },
			"neutronErr": func(rt *rapid.T) { sys.Neutron.Inject(rapid.SampledFrom(fake.Faults).Draw(rt, "fault")) },
			"advance":    func(rt *rapid.T) { sys.Clock.AdvanceDB(time.Duration(rapid.IntRange(1, 120).Draw(rt, "s")) * time.Second) },
			"":           func(rt *rapid.T) { sys.CheckSafety(rt) }, // runs after every action
		})
		sys.Quiesce()
		sys.CheckConverged(rt)
	})
}
```

The testcontainers database is shared per test process; `sys` resets it between rapid iterations with `TRUNCATE … RESTART IDENTITY CASCADE` and re-seeds the pool. Run it with `go test -run TestLifecycleProperties -rapid.checks=500` before every milestone.

### Do it by hand (chaos on FelCloud)

Run these once the tests pass, with the load generator running, and write the results in `docs/notebook/experiments.md`:

| Experiment | Command | Pass criteria |
|---|---|---|
| Kill the control plane mid-stress | `cstamctl bench register -n 20 &` then on cp `sudo systemctl kill -s KILL cpd`, start it again 10 s later | 20/20 ACTIVE, 20 tagged ports, 0 orphans after 2 scans, invariants OK |
| gw2 down during mass teardown | `openstack server stop cstam-gw2`; delete 10 teams; check 10 IPs in QUARANTINE "blocked by gw2"; `openstack server start cstam-gw2` | IPs FREE within ~5 s after gw2 syncs; a probe loop on the deleted FQDNs never sees a 200 |
| Hard reboot a host | `openstack server reboot --hard cstam-host-a` | all its teams HEALTHY again within ~90 s without any control-plane action (the agent re-applies its desired state) |
| Host agent stopped during teardown | on host-a `sudo systemctl stop cstam-hostagent`; delete a team | teardown finishes (fenced by detach/AAP removal after 60 s); when the agent starts, leftovers are purged |
| Orphan and foreign ports | **you** run: `openstack port create --network cstam-sandbox-net --fixed-ip subnet=cstam-sandbox-subnet,ip-address=10.20.0.190 --tag cstam --tag cstam-env:prod orphan-test`, and the same without tags at `.191` | the tagged one is deleted within 2 scans (~60 s); the untagged one is only reported and `.191` becomes FOREIGN; delete it yourself afterwards |

### Done when

::: done
**Done when**
```bash
go test -race ./internal/lifecycle/... -run 'TestEdgesMdScenario|TestTeardownWithBackupDown|TestGenerationCommitOrder|TestNoResurrectOnRollback|TestAckNeverGoesBack'
go test ./internal/lifecycle -run TestLifecycleProperties -rapid.checks=500
go test ./internal/reconcile -run TestReconcileDiff

# On FelCloud, after deleting team1:
curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: team1.hacka.cstam.felcloud.tn' http://<FIP>/   # 404 within ~1 s
openstack port list --tags cstam-team:<id> -f value | wc -l                                     # 0
psql -c "select ip,state,quarantine_until from ip_addresses where ip='10.20.0.57'"               # QUARANTINE, then FREE
cstamctl gc --dry-run                                                                           # "nothing to do"
```
:::

### Pitfalls

::: warn
**"404 counts as success" only for deletes of our own objects**

Treat `404 Not Found` on `DeletePort` / detach as success (someone or something already did it). But a `GET` that finds a port with our IP and **without** our lease tag is not success: it is a conflict. Stop and alert, never delete it.
:::

::: warn
**Do not make the teardown wait forever on the backup gateway**

The user-visible deletion (DELETED phase, 404 on the subdomain) only needs the MASTER's ack. Only IP **reuse** needs every member's ack, and that is enforced by the sweeper, not by the teardown steps. Mixing the two makes every teardown hang while gw2 is being rebooted.
:::

::: warn
**Never "un-remove" a route**

A rollback, a retry or a re-registration must never set `removed_generation` back to NULL. Create a new route row instead. The proof of property S depends on removed routes never coming back.
:::

::: warn
**Tags filter by both `cstam` and the environment**

If you test from your laptop against FelCloud with `CSTAM_ENV=dev` while the demo runs with `prod`, a reconciler that only looks at `cstam` would delete the other environment's ports as orphans. Always filter on `cstam-env:<env>` too.
:::

### Learning checkpoint

::: learn
**Answer these yourself before Step 9**

1. Draw the edges.md timeline (delete A, reuse IP for B, stale route). Mark the exact teardown step and SQL condition that stops it.
2. Why would generation numbers from a sequence break the ack rule? Give a two-transaction example.
3. gw2 is powered off for 3 hours. What happens to IPs released during that time, to new registrations, and to the subdomain of a deleted team? What does fencing change?
4. Why is quarantine *not* the safety mechanism? List two effects that only quarantine covers.
5. What does the circuit breaker protect against, and why is "max(5, 10 %)" a reasonable threshold here?
:::

### Driving Claude Code for this step

::: claude
**Prompts that work well**

- *"Write TestEdgesMdScenario and TestTeardownWithBackupDown first, exactly as in chapter 17, and make them fail against the current code. Then implement the teardown steps until they pass. Do not change the sweeper SQL."*
- *"Act as a hostile reviewer. Try to find an interleaving of two teardowns, one registration, a gw2 restart and a cpd crash that violates property S. Use the proof sketch in chapter 17 and point to the line of code or SQL that blocks each attempt."*
- *"Implement Reconciler.diff as a pure function and write a table-driven test with one case per row of the drift table."*

**What to review in the diff:** the teardown step list order matches the table exactly; every delete is guarded by our tags; the sweeper SQL was not "simplified"; `GREATEST` is used for acks; the circuit breaker exists and is tested.
:::
