# 16. Step 7 – Provisioning a team (POST /teams)

## Step 7 – Provisioning a team

::: note
**Goal:** one API call `POST /v1/hackathons/{id}/teams` turns a registration into a reachable, isolated sandbox at `team1.hacka.cstam.felcloud.tn`, in a few seconds, with a real Neutron IP. Ten or more concurrent calls never share an IP, never leave half-built resources, and survive a crash of `cpd` in the middle.
**Points served:** Automated provisioning (15), stress test (15), IPAM (10).
**Time estimate:** 4–5 days part-time (the biggest step of the project).
**Depends on:** Step 6 (schema, IPAM), Step 4 (gateway agent applies desired state and acks), Step 5 (host agent with the pod-NIC or AAP driver), gate G1 decided which network mode the hosts use.
:::

### Understand first

#### The idea in one sentence

The API does not "do" provisioning. It writes **what we want** (a team row with `desired = PRESENT`) and returns `202 Accepted`. A **controller** then repeatedly compares what we want with what exists, and performs the next missing action, until they match. This is the Kubernetes controller pattern. It sounds indirect, but it gives us three things for free:

1. **Crash recovery.** If `cpd` dies in the middle, the next worker simply looks again and continues. Nothing is "lost in a stack frame".
2. **Teardown is the compensation.** If provisioning fails permanently, we set `desired = ABSENT` and the *normal* teardown runs. We do not write (and forget to test) a separate rollback path.
3. **Drift repair.** If someone deletes a team's Neutron port by hand, the same loop notices and recreates it.

![Figure 16.1 – POST /teams across components. The API answers 202 after one short transaction; the controller does the slow work and every step first checks whether it is already done.](diagrams/f11-provision-seq.png)

#### Steps are idempotent: Done() then Apply()

Every provisioning action is a `Step` with two methods:

- `Done(ctx, team)` **observes the real world** (Neutron, the agents, the DB) and answers "is this already true?"
- `Apply(ctx, team)` makes it true.

The worker runs `Done`; if false, `Apply`; then `Done` again to verify (never trust a 2xx alone). Running the whole list twice changes nothing the second time. This is what makes crashes harmless.

#### Who works on a team: claims with a fencing token

Several worker goroutines (and maybe several `cpd` processes) pick teams to work on. A worker **claims** a team for 30 seconds by incrementing `claim_epoch` with `FOR UPDATE SKIP LOCKED`. Every later write it makes says `WHERE claim_epoch = <my epoch>`. If the worker freezes (long GC pause, network partition) and its claim expires, another worker claims the team with `epoch + 1`, and every write of the frozen worker then affects 0 rows. The frozen worker sees `ErrLostClaim` and gives up. This number is called a **fencing token**.

What about an *external* side effect the frozen worker was in the middle of, such as creating a Neutron port? It is harmless: the new owner's `Done()` finds that port by its fixed IP and tags, and adopts it.

#### Error classes

| Class | Examples | Reaction |
|---|---|---|
| **Waiting** (not an error) | host agent has not yet acked the new desired revision; gateways have not acked generation G | release claim, retry in 300 ms–1 s |
| **Retryable** | timeout, 5xx, connection reset, 409 on a revision conflict, 412 Precondition Failed | backoff `min(2^attempts × 500 ms, 30 s)` + jitter |
| **Fatal for the team** | quota exceeded, pool exhausted, invalid spec, allowed-address-pair limit exceeded | `desired = ABSENT`, `failure_reason = …` → normal teardown → phase `FAILED` |
| **Fatal for the system** | 401 after re-authentication, network or subnet missing | stop all workers, alert, dashboard banner |

#### The provisioning steps

**Pod-NIC mode** (primary, decided at G1):

| # | Step | "Already done?" check | Action | Timeout |
|---|---|---|---|---|
| 0 | Claim + IP (one DB tx) | team has a live lease | choose host (least loaded READY host of the hackathon with capacity); `TakeWarmIP` on that host, else `LeaseIP`; phase `PROVISIONING` | 2 s |
| 1 | Neutron port | a port with this fixed IP exists and has tag `cstam-lease:<lease>` | warm: `PUT` name `cstam-team-<uuid>` + replace tags; cold: `POST` port with fixed IP, SG `cstam-team`, tags | 15 s |
| 2 | NIC attached to host | port `device_id` = host VM and status `ACTIVE` | cold only: Nova attach interface (`openstack server add port`) | 30 s |
| 3 | Host agent | host `applied_rev ≥ desired_rev` and agent reports team `ready` | write team into host desired state (`desired_rev++`) and wait | 30 s |
| 4 | Route + generation | live `routes` row for this team | one tx: pick free slot, insert route, bump `config_head.generation` → Ga | 2 s |
| 5 | Gateways | MASTER acked ≥ Ga (backup counted if reachable) | wait (agents pull) | 10 s |
| 6 | Health | `GET http://<team-ip>:80/` from the MASTER via the agent's probe, or the canary marker | probe | 5 s × 3 |
| 7 | Activate | lease ACTIVE, team ACTIVE | `ActivateLease` + `SetPhase` + event, one tx | 2 s |

**AAP fallback mode** (if H1 failed at G1): steps 1–2 become "create reserved port (no device)" and "host AAP syncer has added `<ip>/32` to the host port's allowed-address-pairs". Everything else is the same.

::: why
**Why health step 6 does not wait for the team's app**

A team that just registered has deployed nothing yet. If "healthy" meant "your app answers 200", the team would never become ACTIVE. In pod-NIC mode the host agent starts a tiny placeholder in the team namespace (part of the pause/pod setup, see Step 5) that answers `GET /.cstam/health` on port **8099**, so it never collides with the team's own app on port 80. In AAP mode the same placeholder runs behind the DNAT. The health step checks *our* plumbing (gateway → team IP → namespace), not the team's code.
:::

### Build it in Go

#### 7.1 The API transaction

The API handler does exactly one short transaction:

```go
// internal/api/teams.go
func (s *Server) CreateTeam(ctx context.Context, req gen.CreateTeamRequestObject) (gen.CreateTeamResponseObject, error) {
	p := auth.Principal(ctx)
	key := req.Params.IdempotencyKey // header Idempotency-Key (required for POST)
	body := req.Body
	if err := validateTeamSpec(body); err != nil { // slug regex, apps allowlist (Step 9)
		return gen.CreateTeam422ApplicationProblemPlusJSONResponse(problem(err)), nil
	}
	var out gen.Team
	err := ipam.InTx(ctx, s.deps.Pool, func(q *db.Queries, tx pgx.Tx) error {
		// 1. idempotency: same key + same body -> replay; same key + other body -> 422
		hit, err := idem.Begin(ctx, q, p.ID, key, "POST", req.Path(), hashJSON(body))
		if err != nil || hit != nil {
			out, _ = hit.Decode()
			return err
		}
		h, err := q.GetOpenHackathon(ctx, req.HackathonId) // status OPEN, live teams < max_teams
		if err != nil {
			return err
		}
		t, err := q.InsertTeam(ctx, db.InsertTeamParams{
			HackathonID: h.ID, Slug: body.Name, DisplayName: body.DisplayName,
			Fqdn:        fmt.Sprintf("%s.%s.%s", body.Name, h.Slug, s.deps.BaseDomain),
			Spec:        mustJSON(body.Apps), MaxExpiresAt: h.EndsAt,
		}) // unique partial indexes -> 23505 -> 409
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT pg_notify('cstam_work', $1)", t.ID.String()); err != nil {
			return err // NOTIFY is delivered only if this tx commits
		}
		out = toAPITeam(t)
		return idem.Finish(ctx, q, p.ID, key, 202, out)
	})
	if pgerr.IsUnique(err) {
		return gen.CreateTeam409ApplicationProblemPlusJSONResponse(problemConflict("team name already used in this hackathon")), nil
	}
	if err != nil {
		return nil, err
	}
	return gen.CreateTeam202JSONResponse{Body: out, Headers: gen.CreateTeam202ResponseHeaders{Location: "/v1/teams/" + out.Id.String()}}, nil
}
```

Response shape (the team *is* the long-running operation; poll `GET /v1/teams/{id}` or watch SSE):

```http
HTTP/1.1 202 Accepted
Location: /v1/teams/0190a1b2-7c11-7e0d-9f0a-3b2c1d0e9f00
Content-Type: application/json

{"id":"0190a1b2-…","name":"team1","fqdn":"team1.hacka.cstam.felcloud.tn",
 "desired":"PRESENT","phase":"REQUESTED","health":"UNKNOWN","ip":null,
 "steps":[], "expires_at":null}
```

When the controller progresses, `GET` shows `phase: PROVISIONING`, `ip: "10.20.0.57"`, and the step list with durations from the `operations` table: `[{"step":"neutron.port","state":"SUCCEEDED","ms":412}, …]`. The dashboard draws this as the provisioning timeline.

::: why
**Why the IP is not leased in the API transaction**

You could lease the IP directly in the API transaction (the response would then already show it). We lease it in the controller's claim transaction instead, because the host must be chosen first (in pod-NIC mode the warm IP depends on the host), and because a registration rejected later (hackathon closed between insert and claim) should not touch the pool at all. The API still answers in a few milliseconds.
:::

Idempotency middleware details (`internal/api/idem`): `INSERT … ON CONFLICT DO NOTHING` on `(principal, key)`. If the row exists **with** a response, replay it. If it exists **without** a response (in flight), return `409` with `Retry-After: 1`. Same key with a different `request_hash` → `422`. Keys are deleted after 24 h by the GC. For the stress test this means a client that times out and retries never creates two teams.

#### 7.2 The controller: claim, run steps, release

```sql
-- name: ClaimTeams :many
UPDATE teams t
SET claimed_by = sqlc.arg(worker), claim_epoch = t.claim_epoch + 1,
    claim_expires_at = now() + interval '30 seconds', attempts = t.attempts + 1
WHERE t.id IN (
  SELECT id FROM teams
  WHERE phase NOT IN ('DELETED','FAILED')
    AND (phase IN ('REQUESTED','PROVISIONING','DELETING') OR desired = 'ABSENT' OR health <> 'HEALTHY')
    AND next_attempt_at <= now()
    AND (claim_expires_at IS NULL OR claim_expires_at < now())
  ORDER BY next_attempt_at
  LIMIT sqlc.arg(n)
  FOR UPDATE SKIP LOCKED)
RETURNING t.id, t.claim_epoch;

-- name: ExtendClaim :execrows
UPDATE teams SET claim_expires_at = now() + interval '30 seconds'
WHERE id = sqlc.arg(id) AND claim_epoch = sqlc.arg(epoch);

-- name: ReleaseClaim :exec
UPDATE teams SET claimed_by = NULL, claim_expires_at = NULL,
       next_attempt_at = now() + sqlc.arg(backoff_ms)::int * interval '1 millisecond',
       last_error = sqlc.arg(err)
WHERE id = sqlc.arg(id) AND claim_epoch = sqlc.arg(epoch);
```

```go
// internal/lifecycle/controller.go
package lifecycle

// Step is one idempotent unit of work: Done observes the real world, Apply makes it true.
type Step interface {
	Name() string
	Done(ctx context.Context, t *Team) (bool, error)
	Apply(ctx context.Context, t *Team) error
	Timeout() time.Duration
}

type Controller struct {
	Pool      *pgxpool.Pool
	IPAM      *ipam.IPAM
	Provision []Step // claimIP, port, attach|aap, hostAgent, route, gatewaysAck, health, activate
	Teardown  []Step // Step 8
	Notify    <-chan string // LISTEN cstam_work
	WorkerID  string
	Workers   int // 16
	Log       *slog.Logger
}

func (c *Controller) Run(ctx context.Context) error {
	sem := make(chan struct{}, c.Workers)
	for {
		free := cap(sem) - len(sem)
		if free > 0 {
			claims, err := db.New(c.Pool).ClaimTeams(ctx, db.ClaimTeamsParams{Worker: c.WorkerID, N: int32(free)})
			if err != nil {
				c.Log.Error("claim", "err", err)
			}
			for _, cl := range claims {
				sem <- struct{}{}
				go func() { defer func() { <-sem }(); c.reconcileOne(ctx, cl.ID, cl.ClaimEpoch) }()
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.Notify: // a commit happened somewhere
		case <-time.After(time.Second): // a lost NOTIFY costs at most 1 s
		}
	}
}

func (c *Controller) reconcileOne(ctx context.Context, id uuid.UUID, epoch int64) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.keepClaim(ctx, cancel, id, epoch) // ExtendClaim every 10 s; cancel() when 0 rows (lost claim)

	t, err := c.load(ctx, id, epoch)
	if err != nil {
		c.release(ctx, id, epoch, err)
		return
	}
	steps := c.Provision
	if t.Desired == DesiredAbsent {
		steps = c.Teardown
	}
	for _, s := range steps {
		// A manual teardown during provisioning switches pipeline at the next step boundary.
		if changed, _ := c.desiredChanged(ctx, t); changed {
			c.release(ctx, id, epoch, nil) // next claim picks the other pipeline at once
			return
		}
		op := c.opStart(ctx, t, s.Name())
		done, err := s.Done(ctx, t)
		if err == nil && !done {
			sctx, stop := context.WithTimeout(ctx, s.Timeout())
			err = s.Apply(sctx, t)
			stop()
			if err == nil {
				done, err = s.Done(ctx, t) // verify
			}
		}
		c.opFinish(ctx, op, err)
		var wait *ErrWaiting
		switch {
		case errors.As(err, &wait):
			c.releaseAfter(ctx, id, epoch, wait.After, wait.Error())
			return
		case err != nil && IsFatalForTeam(err):
			c.failTeam(ctx, t, err) // desired=ABSENT, failure_reason; teardown runs next
			c.release(ctx, id, epoch, err)
			return
		case err != nil || !done:
			c.release(ctx, id, epoch, err) // backoff + jitter computed from attempts
			return
		}
	}
	c.release(ctx, id, epoch, nil)
}
```

`ErrWaiting` is a small type `{After time.Duration; Reason string}` returned by `Done`/`Apply` when a dependency (agent ack, gateway ack) is not ready yet. It is **not** a failure: it does not increase backoff and it shows on the dashboard as "waiting for gw2 ack ≥ 1234".

#### 7.3 Step 0: claim the IP and the host

```go
// internal/lifecycle/steps_ip.go
type claimIP struct{ c *Controller }

func (s claimIP) Name() string            { return "ipam.lease" }
func (s claimIP) Timeout() time.Duration  { return 2 * time.Second }
func (s claimIP) Done(_ context.Context, t *Team) (bool, error) { return t.LeaseID != uuid.Nil, nil }

func (s claimIP) Apply(ctx context.Context, t *Team) error {
	return ipam.InTx(ctx, s.c.Pool, func(q *db.Queries, tx pgx.Tx) error {
		// lock order: team row first
		if _, err := q.LockTeamFenced(ctx, db.LockTeamFencedParams{ID: t.ID, Epoch: t.Epoch}); err != nil {
			return ErrLostClaim
		}
		host, err := q.PickHost(ctx, t.HackathonID) // READY, live teams < capacity, least loaded
		if err != nil {
			return &FatalForTeam{Reason: "no_host_capacity"}
		}
		var leaseID, portID uuid.UUID
		var ip netip.Addr
		if host.NetMode == db.NetModePODNIC {
			leaseID, ip, portID, err = s.c.IPAM.TakeWarm(ctx, q, t.ID, host.ID)
		}
		if host.NetMode != db.NetModePODNIC || errors.Is(err, ipam.ErrNoWarm) {
			leaseID, ip, err = s.c.IPAM.Lease(ctx, q, t.ID, poolFor(host))
		}
		if errors.Is(err, ipam.ErrPoolExhausted) {
			return &FatalForTeam{Reason: "pool_exhausted"}
		}
		if err != nil {
			return err
		}
		t.LeaseID, t.IP, t.PortID, t.HostID = leaseID, ip, portID, host.ID
		return mustOne(q.StartProvisioning(ctx, db.StartProvisioningParams{
			ID: t.ID, Epoch: t.Epoch, HostID: host.ID})) // phase REQUESTED -> PROVISIONING, fenced
	})
}
```

The worker kicks the **warm refiller** (7.7) after taking a warm IP, so the pool is topped up in the background.

#### 7.4 The Neutron adapter (gophercloud v2)

Keep the interface small and in our vocabulary. The real implementation wraps gophercloud; tests use a fake (7.9).

```go
// internal/openstack/neutron.go
type Neutron interface {
	PortByFixedIP(ctx context.Context, ip netip.Addr) (*Port, error)                  // nil, nil when absent
	CreateTeamPort(ctx context.Context, spec PortSpec) (*Port, error)                 // fixed IP, SG, tags, name
	RetagPort(ctx context.Context, portID string, name string, tags []string) error   // warm -> team
	DeletePort(ctx context.Context, portID string) error                              // 404 => nil
	ListPortsByTags(ctx context.Context, tags ...string) ([]Port, error)
	GetPort(ctx context.Context, portID string) (*Port, error)
	// AAP mode only: replace the full list with compare-and-swap on revision_number.
	SetAllowedAddressPairs(ctx context.Context, portID string, cidrs []netip.Prefix, revision int) (*Port, error)
}
type Nova interface {
	AttachPort(ctx context.Context, serverID, portID string) error  // pod-NIC
	DetachPort(ctx context.Context, serverID, portID string) error  // 404 => nil
}
type Port struct {
	ID, Name, DeviceID, Status, MAC string
	FixedIP  netip.Addr
	Pairs    []netip.Prefix
	Tags     []string
	Revision int
}
func (p *Port) HasTag(tag string) bool { return slices.Contains(p.Tags, tag) }
```

The gophercloud packages (`github.com/gophercloud/gophercloud/v2/...`):

| Need | Package | Call |
|---|---|---|
| Client from `clouds.yaml` | `openstack/config/clouds`, `openstack/config`, `openstack` | `clouds.Parse(clouds.WithCloudName("cstam"))`, `config.NewProviderClient`, `openstack.NewNetworkV2`, `openstack.NewComputeV2` |
| Ports | `openstack/networking/v2/ports` | `ports.Create`, `ports.List(…, ports.ListOpts{FixedIPs: []ports.FixedIPOpts{{IPAddress: ip}}, Tags: "cstam"})`, `ports.Get`, `ports.Update`, `ports.Delete` |
| Tags | `openstack/networking/v2/extensions/attributestags` | `attributestags.ReplaceAll(ctx, nc, "ports", id, attributestags.ReplaceAllOpts{Tags: tags})` |
| If-Match revision | same `ports.UpdateOpts{RevisionNumber: &rev}` | sends `If-Match: revision_number=N`; `412` on conflict |
| Attach / detach NIC | `openstack/compute/v2/attachinterfaces` | `attachinterfaces.Create(ctx, cc, serverID, attachinterfaces.CreateOpts{PortID: id})`, `attachinterfaces.Delete(ctx, cc, serverID, portID)` |

```go
// internal/openstack/gopher.go (excerpt)
func (g *Gopher) CreateTeamPort(ctx context.Context, s PortSpec) (*Port, error) {
	if err := g.lim.Wait(ctx); err != nil { // x/time/rate: 10 req/s
		return nil, err
	}
	g.sem <- struct{}{}        // at most 8 Neutron calls in flight
	defer func() { <-g.sem }()
	var p *ports.Port
	err := retry.Do(ctx, 3, 200*time.Millisecond, 2*time.Second, NeutronRetryable, func(ctx context.Context) error {
		var err error
		p, err = ports.Create(ctx, g.net, ports.CreateOpts{
			NetworkID:      g.networkID,
			Name:           s.Name, // cstam-team-<uuid>
			Description:    s.Description,
			FixedIPs:       []ports.IP{{SubnetID: g.subnetID, IPAddress: s.IP.String()}},
			SecurityGroups: &[]string{g.teamSG}, // cstam-team
		}).Extract()
		return err
	})
	if err != nil {
		return nil, classifyNeutron(err) // 409 IpAddressAlreadyAllocated -> ErrIPTaken
	}
	if err := attributestags.ReplaceAll(ctx, g.net, "ports", p.ID,
		attributestags.ReplaceAllOpts{Tags: s.Tags}).Err; err != nil {
		return nil, err // the port exists untagged for a moment: Done() looks it up by fixed IP + name
	}
	return toPort(p, s.Tags), nil
}
```

::: warn
**Do not retry a non-idempotent POST blindly**

`POST /v2.0/ports` is **not** idempotent. If Neutron created the port but the response was lost (timeout), a blind retry creates nothing new only because the fixed IP is taken: you get `409 IpAddressAlreadyAllocated`. That is why `retry.Do` above only retries errors that happen *before* the request was sent (connection refused) or 5xx, and why the step's `Apply` treats `ErrIPTaken` as "go and look". `classifyNeutron` must map the 409 body `IpAddressAlreadyAllocated` to `ErrIPTaken`, and quota errors (`OverQuota`) to `FatalForTeam`.

Also: `ports.CreateOpts` has no `Tags` field in gophercloud, so tags are a second call. The deterministic **name** `cstam-team-<team-uuid>` is the primary identity; tags are for GC queries. On FelCloud the `tag-creation` extension exists, so you can also send tags inside the create body with a raw request (`gophercloud.ServiceClient.Post` with a custom map) – one call, atomic. Prefer that once it works.
:::

Retry helper and what is retryable:

```go
// internal/platform/retry/retry.go
func Do(ctx context.Context, attempts int, base, max time.Duration, retryable func(error) bool,
	fn func(context.Context) error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(ctx); err == nil || !retryable(err) {
			return err
		}
		backoff := min(max, base<<i)
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(time.Duration(rand.Int64N(int64(backoff)))): // full jitter
		}
	}
	return err
}

func NeutronRetryable(err error) bool {
	if code, ok := httpCode(err); ok {
		return code == 429 || code == 500 || code == 502 || code == 503 || code == 504
	}
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET)
}
```

Retry *small* at the call site (3 attempts), then return the error and let the controller's backoff handle the rest. Retrying at full depth in both places multiplies waiting times.

#### 7.5 Steps 1 and 2 (pod-NIC): the team port and the attached NIC

```go
type teamPort struct{ n openstack.Neutron; env string }

func (s teamPort) Name() string           { return "neutron.port" }
func (s teamPort) Timeout() time.Duration { return 15 * time.Second }

func (s teamPort) Done(ctx context.Context, t *Team) (bool, error) {
	p, err := s.n.PortByFixedIP(ctx, t.IP)
	if err != nil || p == nil {
		return false, err
	}
	if !p.HasTag("cstam-lease:" + t.LeaseID.String()) {
		if p.HasTag("cstam-warm:"+t.HostName) && p.ID == t.PortID.String() {
			return false, nil // our warm port, not yet re-tagged: Apply will retag it
		}
		if p.Name == teamPortName(t) { // created by us, tags call lost
			return false, nil
		}
		return false, &ForeignIPError{IP: t.IP, PortID: p.ID} // someone else's port: mark FOREIGN, re-lease
	}
	t.PortID = uuid.MustParse(p.ID)
	t.PortMAC = p.MAC
	return true, saveLeasePort(ctx, t) // UPDATE leases SET neutron_port_id ... (fenced)
}

func (s teamPort) Apply(ctx context.Context, t *Team) error {
	tags := []string{"cstam", "cstam-env:" + s.env, "cstam-team:" + t.ID.String(), "cstam-lease:" + t.LeaseID.String()}
	if t.PortID != uuid.Nil { // warm port: rename + retag only (fast)
		return s.n.RetagPort(ctx, t.PortID.String(), teamPortName(t), tags)
	}
	_, err := s.n.CreateTeamPort(ctx, openstack.PortSpec{
		Name: teamPortName(t), IP: t.IP, Tags: tags,
		Description: fmt.Sprintf("cstam team=%s hackathon=%s", t.Slug, t.HackathonSlug),
	})
	if errors.Is(err, openstack.ErrIPTaken) {
		return nil // Done() decides: ours (adopt) or foreign
	}
	return err
}

type attachNIC struct{ n openstack.Neutron; nova openstack.Nova }

func (s attachNIC) Name() string           { return "nova.attach" }
func (s attachNIC) Timeout() time.Duration { return 30 * time.Second }
func (s attachNIC) Done(ctx context.Context, t *Team) (bool, error) {
	p, err := s.n.GetPort(ctx, t.PortID.String())
	if err != nil {
		return false, err
	}
	if p.DeviceID == t.HostServerID && p.Status == "ACTIVE" {
		return true, nil
	}
	if p.DeviceID == t.HostServerID { // attached, OVN not finished binding yet
		return false, &ErrWaiting{After: 500 * time.Millisecond, Reason: "port binding"}
	}
	return false, nil
}
func (s attachNIC) Apply(ctx context.Context, t *Team) error {
	return s.nova.AttachPort(ctx, t.HostServerID, t.PortID.String())
}
```

The foreign case: when `Done` returns `ForeignIPError`, the controller runs one transaction: `MarkForeign(ip)` + release the PENDING lease (`state = RELEASED`) + lease a new IP. It allows this at most 3 times per team, then fails the team with `failure_reason = ip_conflicts`. A port held by somebody else is **never** deleted by us.

::: warn
**Nova attaches NICs one at a time per VM**

Nova takes a lock per instance for interface attach. Ten cold attaches on the same host run one after the other at 2–6 s each (measured in experiment H1). That is why the warm pool exists: with warm ports, step 2 is already done. Size `warm_per_host` to the expected registration burst (default 6; for the stress test set 12 on each host before the run). If the pool is empty, provisioning still works, it is only slower.
:::

#### 7.6 AAP fallback: the per-host syncer

In AAP mode, step 1 creates a *reserved* port (no device), and step 2 needs the team IP in the host port's `allowed_address_pairs`. That list is **replaced as a whole** on every update, and FelCloud has no atomic add/remove extension. Ten goroutines doing "GET port, append my IP, PUT port" would lose updates: the last writer wins, nine teams silently have no pair, and port security drops their traffic. This would fail the stress test at random.

Fix: workers never write the list. One **syncer per host** computes the full desired list from the database and writes it with compare-and-swap:

```go
// internal/lifecycle/aapsync.go
type AAPSyncer struct {
	pool *pgxpool.Pool
	n    openstack.Neutron
	kick map[uuid.UUID]chan struct{} // one channel per host
}

func (s *AAPSyncer) runHost(ctx context.Context, h db.Host) {
	t := time.NewTicker(10 * time.Second) // periodic resync also repairs drift
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.kick[h.ID]:
			time.Sleep(100 * time.Millisecond) // coalesce: let more changes arrive
		case <-t.C:
		}
		if err := s.syncOnce(ctx, h); err != nil {
			slog.Warn("aap sync", "host", h.Name, "err", err)
		}
	}
}

func (s *AAPSyncer) syncOnce(ctx context.Context, h db.Host) error {
	for attempt := 0; attempt < 10; attempt++ {
		want, err := db.New(s.pool).DesiredPairs(ctx, h.ID) // EXACT: leases LEASED/ACTIVE (+RELEASING not yet past AAP step) on this host; CIDR: the host block
		if err != nil {
			return err
		}
		cur, err := s.n.GetPort(ctx, h.NeutronPortID.String())
		if err != nil {
			return err
		}
		if equalPrefixes(cur.Pairs, want) {
			return nil
		}
		_, err = s.n.SetAllowedAddressPairs(ctx, cur.ID, want, cur.Revision) // If-Match: revision_number
		if errors.Is(err, openstack.ErrPreconditionFailed) {                 // HTTP 412: someone changed the port
			continue
		}
		if isAAPLimit(err) { // "Exceeded maximum amount of allowed address pairs"
			return markHostFull(ctx, s.pool, h.ID) // teams beyond the limit fail as FatalForTeam
		}
		return err
	}
	return errors.New("aap: too many conflicts")
}
```

The team's step 2 `Done` is "the host port's pairs contain `<ip>/32`"; its `Apply` only kicks the syncer and returns `ErrWaiting{300ms}`. Ten registrations within 100 ms become **one** Neutron update. If the AAP limit is 10 (experiment H2), use CIDR mode: each host port gets one pair for its `/27` block, written once at host enrolment, and step 2 is a no-op.

Also important in AAP mode: the host agent sends a gratuitous ARP (`arping -U -c 3 -I ens3 <team-ip>`) after binding the IP, because OVN binds the reserved (virtual) port to the host only when it sees ARP from it.

#### 7.7 The warm pool refiller (pod-NIC)

```go
// internal/lifecycle/warm.go: one goroutine on the leader
func (w *WarmRefiller) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.kick:
		case <-t.C:
		}
		for _, h := range w.readyPodNICHosts(ctx) {
			for w.countWarm(ctx, h) < h.WarmTarget {
				if err := w.addOne(ctx, h); err != nil {
					slog.Warn("warm refill", "host", h.Name, "err", err)
					break // try again next tick
				}
			}
		}
	}
}

func (w *WarmRefiller) addOne(ctx context.Context, h Host) error {
	ip, err := db.New(w.pool).ReserveForWarm(ctx, db.ReserveForWarmParams{PoolID: w.poolID, HostID: h.ID}) // FREE -> WARM(warming)
	if err != nil {
		return err
	}
	p, err := w.n.CreateTeamPort(ctx, openstack.PortSpec{
		Name: "cstam-warm-" + ip.String(), IP: ip,
		Tags: []string{"cstam", "cstam-env:" + w.env, "cstam-warm:" + h.Name},
	})
	if err != nil {
		return w.abandon(ctx, ip, err) // IP -> quarantine path; reconciler cleans any half-created port
	}
	if err := w.nova.AttachPort(ctx, h.ServerID, p.ID); err != nil {
		return w.abandon(ctx, ip, err)
	}
	_, err = db.New(w.pool).MarkWarm(ctx, db.MarkWarmParams{IP: ip, HostID: h.ID, PortID: uuid.MustParse(p.ID)})
	return err
}
```

The host agent does not plumb a warm NIC into any namespace. It just leaves it down in the root namespace (with no IP) until a team spec arrives that names its MAC.

#### 7.8 The agent protocol (Connect-RPC, agents dial out)

Agents (gateway and host) **open the connection to `cpd`**, never the opposite. So gateways and hosts need no inbound management port, only SSH from the control plane. Authentication is mutual TLS (Step 9). Every message carries **full desired state**, never deltas: a lost message is repaired by the next one.

```proto
// api/proto/cstam/agent/v1/agent.proto
syntax = "proto3";
package cstam.agent.v1;

service ControlService {
  // Long-poll style server stream: cpd sends a snapshot immediately if generation > after,
  // otherwise when the next change commits (or a keepalive every 15 s).
  rpc WatchGateway(WatchRequest) returns (stream GatewayDesired);
  rpc WatchHost(WatchRequest)    returns (stream HostDesired);
  rpc Ack(AckRequest)            returns (AckResponse);       // "I applied generation/revision N"
  rpc Report(AgentReport)        returns (ReportResponse);    // heartbeat every 2 s
  rpc PushTrafficLogs(stream LogBatch) returns (PushAck);     // gateways only
}

message WatchRequest { string agent = 1; int64 after = 2; }   // identity also checked from the mTLS cert

message GatewayDesired {
  int64  generation = 1;            // config_head.generation
  bytes  map_digest = 2;            // sha256 of sorted "fqdn slot ip:port\n" lines
  repeated Route routes = 3;
  int32  structural_version = 4;    // which HAProxy template
  string structural_body = 5;       // the rendered template text (only when changed)
  bool   force_backup = 6;          // manual failover button
}
message Route { string fqdn = 1; int32 slot = 2; string ip = 3; int32 port = 4; int32 rate_limit = 5; }

message HostDesired {
  int64 revision = 1;               // hosts.desired_rev
  repeated TeamSpec teams = 2;      // every team that must exist on this host
  repeated string warm_macs = 3;    // pod-NIC: NICs that must stay unplumbed
}
message TeamSpec {
  string team_id = 1; string slug = 2; string ip = 3; string mac = 4;   // mac only in pod-NIC
  string net_mode = 5;              // PODNIC | AAP
  repeated App apps = 6;
}
message App { string name = 1; string image = 2; int32 port = 3; double cpu = 4; int32 mem_mib = 5; }

message AckRequest {
  string agent = 1; int64 generation = 2; bytes digest = 3;
  string outcome = 4;               // applied | rejected | rolled_back
  int64 validate_ms = 5; int64 apply_ms = 6; int64 verify_ms = 7; int64 rollback_ms = 8;
  string detail = 9;
  repeated TeamStatus teams = 10;   // host agents: per-team ready/error
}
message TeamStatus { string team_id = 1; bool ready = 2; string error = 3; }
message AckResponse { bool accepted = 1; string reason = 2; }

message AgentReport {
  string agent = 1; string vrrp = 2; bool haproxy_ok = 3; bool synced = 4;
  int64 applied = 5; string version = 6;
}
message ReportResponse {}
message LogBatch { string agent = 1; repeated bytes lines = 2; }
message PushAck {}
```

Generate with `buf generate` (`protoc-gen-go` + `protoc-gen-connect-go`). The server side (`internal/agentapi`) implements `ControlService`; agents use the generated client with an mTLS `http.Client`.

Snapshot builders (`internal/agentapi/snapshot.go`):

```go
// BuildGatewayDesired reads the head generation and the routes visible at that generation
// in ONE repeatable-read transaction, so the snapshot and its generation always match.
func BuildGatewayDesired(ctx context.Context, pool *pgxpool.Pool, gw string) (*agentv1.GatewayDesired, error) {
	var out agentv1.GatewayDesired
	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			q := db.New(tx)
			head, err := q.GetConfigHead(ctx)
			if err != nil {
				return err
			}
			rows, err := q.SnapshotAt(ctx, head.Generation) // added <= G AND (removed IS NULL OR removed > G)
			if err != nil {
				return err
			}
			out.Generation = head.Generation
			out.StructuralVersion = head.StructuralVersion
			for _, r := range rows {
				out.Routes = append(out.Routes, &agentv1.Route{Fqdn: r.Fqdn, Slot: r.Slot, Ip: r.Ip.String(), Port: r.TargetPort})
			}
			out.MapDigest = routeDigest(out.Routes) // same function the agent uses to verify
			fb, err := q.GatewayForceBackup(ctx, gw)
			out.ForceBackup = fb
			return err
		})
	return &out, err
}
```

`Ack` verifies the digest against `config_versions.map_digest` for that generation and then runs `AckGeneration` (`acked_generation = GREATEST(acked_generation, $gen)`), Step 8. A wrong digest is rejected: an agent bug cannot fake an acknowledgement.

The `WatchGateway` handler: subscribe to `LISTEN cstam_gw`; send a snapshot if `head > after`; then send a new snapshot on each notification (coalesced over 50 ms), and an empty keepalive frame every 15 s. Host watch is the same with `cstam_host_<id>` and `desired_rev`.

#### 7.9 Steps 3–7

```go
// Step 3: host agent
type hostAgent struct{ pool *pgxpool.Pool }

func (s hostAgent) Name() string           { return "host.agent" }
func (s hostAgent) Timeout() time.Duration { return 30 * time.Second }
func (s hostAgent) Done(ctx context.Context, t *Team) (bool, error) {
	st, err := db.New(s.pool).HostTeamStatus(ctx, db.HostTeamStatusParams{HostID: t.HostID, TeamID: t.ID})
	if err != nil {
		return false, err
	}
	switch {
	case st.AppliedRev >= st.TeamRev && st.Ready:
		return true, nil
	case st.AppliedRev >= st.TeamRev && st.Error != "":
		return false, fmt.Errorf("host agent: %s", st.Error) // retryable; repeated -> FAILED by PENDING deadline
	case st.TeamRev > 0:
		return false, &ErrWaiting{After: 500 * time.Millisecond, Reason: "host agent sync"}
	}
	return false, nil
}
func (s hostAgent) Apply(ctx context.Context, t *Team) error {
	// mark the team as part of the host's desired state: bump desired_rev, remember it in host_teams.rev
	return ipam.InTx(ctx, s.pool, func(q *db.Queries, tx pgx.Tx) error {
		rev, err := q.BumpHostRev(ctx, t.HostID) // UPDATE hosts SET desired_rev = desired_rev + 1 RETURNING
		if err != nil {
			return err
		}
		if err := q.UpsertHostTeam(ctx, db.UpsertHostTeamParams{HostID: t.HostID, TeamID: t.ID, Rev: rev}); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "SELECT pg_notify('cstam_host_'||$1, $2)", t.HostID.String(), strconv.FormatInt(rev, 10))
		return err
	})
}
```

(`host_teams(host_id, team_id, rev, ready, error)` is a small table added in this step's migration `00003_host_teams.sql`; the agent's `Ack` fills `ready`/`error`.)

```go
// Step 4: route + generation (one transaction, commit-ordered generation)
func (s route) Apply(ctx context.Context, t *Team) error {
	return ipam.InTx(ctx, s.pool, func(q *db.Queries, tx pgx.Tx) error {
		slot, err := q.FreeSlot(ctx) // lowest slot 1..128 without a live route
		if err != nil {
			return &FatalForTeam{Reason: "no_gateway_slot"} // 128 apps max until the slot pool is resized
		}
		gen, err := q.BumpGeneration(ctx) // UPDATE config_head SET generation = generation + 1 RETURNING ...
		if err != nil {
			return err
		}
		for _, app := range t.ExposedApps() { // team1 -> :80, extra apps -> <app>-team1 on :8000-8099
			if err := q.InsertRoute(ctx, db.InsertRouteParams{TeamID: t.ID, LeaseID: t.LeaseID, Fqdn: app.FQDN,
				TargetIp: t.IP, TargetPort: app.Port, Slot: slot, AddedGeneration: gen.Generation}); err != nil {
				return err
			}
			slot++ // one slot per exposed app (FreeSlot returns a run of free slots in the real code)
		}
		if err := insertConfigVersion(ctx, q, gen, "ROUTES", "add "+t.Slug); err != nil { // stores map digest
			return err
		}
		t.RouteGeneration = gen.Generation
		_, err = tx.Exec(ctx, "SELECT pg_notify('cstam_gw', $1)", strconv.FormatInt(gen.Generation, 10))
		return err
	})
}

// Step 5: gateways acknowledged
func (s gatewaysAck) Done(ctx context.Context, t *Team) (bool, error) {
	gws, err := db.New(s.pool).MemberGateways(ctx)
	if err != nil {
		return false, err
	}
	masterOK := false
	for _, g := range gws {
		if g.Vrrp == db.VrrpStateMASTER && g.AckedGeneration >= t.RouteGeneration {
			masterOK = true
		}
	}
	if !masterOK {
		return false, &ErrWaiting{After: 200 * time.Millisecond, Reason: fmt.Sprintf("waiting for master ack >= %d", t.RouteGeneration)}
	}
	// a backup that is behind marks the team health DEGRADED, it does not block activation
	return true, nil
}
func (s gatewaysAck) Apply(context.Context, *Team) error { return nil } // agents pull by themselves
```

Step 6 (health) asks the MASTER gateway agent to probe the team from the gateway itself: `GET http://<team-ip>:8099/.cstam/health` (the placeholder from Step 5). This is exactly the network path HAProxy uses (gateway port → OVN → SG `cstam-team` → team NIC → namespace), and it works before the team has deployed anything. The route itself was already verified by the gateway's digest-checked ack in step 5. Step 7 (activate) runs `ActivateLease` + `SetPhase(PROVISIONING → ACTIVE)` + an `events` row + `NOTIFY cstam_events` in one fenced transaction.

#### 7.10 A fake Neutron with fault injection

Unit and integration tests never call FelCloud. The fake emulates exactly the behaviors that matter: fixed-IP uniqueness (409), revision numbers and 412, 404s, the AAP limit, and the nasty one: "created, then the response timed out".

```go
// internal/openstack/fake/fake.go
type Fault int

const (
	None Fault = iota
	Latency
	Err500
	TimeoutBeforeCommit
	TimeoutAfterCommit // the port IS created, the caller gets a timeout
	Conflict412
	OverQuota
)

type Fake struct {
	mu     sync.Mutex
	ports  map[string]*openstack.Port
	byIP   map[netip.Addr]string
	faults chan Fault // tests push faults; each call consumes one
	MaxAAP int        // 10, like Neutron's default
	Calls  []string   // call log, used to assert ordering (edges.md test)
}

func (f *Fake) CreateTeamPort(ctx context.Context, s openstack.PortSpec) (*openstack.Port, error) {
	f.record("CreateTeamPort " + s.IP.String())
	switch f.next() {
	case Latency:
		time.Sleep(time.Duration(200+rand.IntN(800)) * time.Millisecond)
	case Err500:
		return nil, openstack.ErrServer
	case TimeoutBeforeCommit:
		<-ctx.Done()
		return nil, ctx.Err()
	case TimeoutAfterCommit:
		_, _ = f.commit(s)
		<-ctx.Done()
		return nil, ctx.Err()
	case OverQuota:
		return nil, openstack.ErrOverQuota
	}
	return f.commit(s)
}

func (f *Fake) commit(s openstack.PortSpec) (*openstack.Port, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, taken := f.byIP[s.IP]; taken {
		return nil, openstack.ErrIPTaken
	}
	p := &openstack.Port{ID: uuid.NewString(), Name: s.Name, FixedIP: s.IP, Tags: slices.Clone(s.Tags), Status: "DOWN",
		MAC: fakeMAC(s.IP)}
	f.ports[p.ID], f.byIP[s.IP] = p, p.ID
	return p, nil
}

func (f *Fake) SetAllowedAddressPairs(_ context.Context, id string, pairs []netip.Prefix, rev int) (*openstack.Port, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.ports[id]
	if p.Revision != rev {
		return nil, openstack.ErrPreconditionFailed // 412
	}
	if len(pairs) > f.MaxAAP {
		return nil, openstack.ErrAAPLimit
	}
	p.Pairs, p.Revision = slices.Clone(pairs), p.Revision+1
	return p, nil
}
```

The same file has a `FakeNova` (attach sets `DeviceID` and, after a configurable delay, `Status = ACTIVE`, with a per-server mutex to imitate Nova's serialization), a `FakeHostAgent` and a `FakeGateway` (both ack desired state after a configurable delay, can be set "down"). Together they form `testutil.NewSystem(pool)`: the real controller plus fakes, used by every lifecycle test.

#### 7.11 Tests

```go
func TestProvisionHappyPath(t *testing.T) {
	sys := testutil.NewSystem(t) // real Postgres + real controller + fakes
	id := sys.API.CreateTeam(t, "hacka", "team1", "key-1")
	sys.RunUntil(t, 10*time.Second, func() bool { return sys.Phase(id) == "ACTIVE" })
	require.Len(t, sys.Neutron.PortsByTag("cstam-team:"+id.String()), 1)
	require.Equal(t, sys.LeaseIP(id), sys.Gateway("gw1").Route("team1.hacka.cstam.felcloud.tn").IP)
	testutil.AssertInvariants(t, sys.Pool)
}

// The same POST twice (client retry after a timeout) creates one team.
func TestIdempotentPost(t *testing.T) {
	sys := testutil.NewSystem(t)
	a := sys.API.CreateTeam(t, "hacka", "team1", "key-1")
	b := sys.API.CreateTeam(t, "hacka", "team1", "key-1")
	require.Equal(t, a, b)
	resp := sys.API.CreateTeamRaw(t, "hacka", `{"name":"team2"}`, "key-1") // same key, other body
	require.Equal(t, 422, resp.StatusCode)
}

// Neutron created the port but the response was lost.
func TestTimeoutAfterCommit(t *testing.T) {
	sys := testutil.NewSystem(t)
	sys.Neutron.Inject(fake.TimeoutAfterCommit)
	id := sys.API.CreateTeam(t, "hacka", "team1", "k")
	sys.RunUntil(t, 15*time.Second, func() bool { return sys.Phase(id) == "ACTIVE" })
	require.Len(t, sys.Neutron.PortsByIP(sys.LeaseIP(id)), 1) // adopted, not duplicated
}

// The worker dies right after the lease transaction committed.
func TestCrashAfterLease(t *testing.T) {
	sys := testutil.NewSystem(t)
	sys.Controller.CrashAfterStep("ipam.lease") // test hook: goroutine exits without releasing the claim
	id := sys.API.CreateTeam(t, "hacka", "team1", "k")
	sys.Clock.AdvanceDB(31 * time.Second)         // claim expires (DB time offset, see Step 8 tests)
	sys.RunUntil(t, 15*time.Second, func() bool { return sys.Phase(id) == "ACTIVE" })
	require.Equal(t, int64(2), sys.ClaimEpoch(id)) // second owner finished it
	testutil.AssertInvariants(t, sys.Pool)
}

// 20 teams on one host in AAP mode: the final pair list contains all 20 IPs (no lost update).
func TestAAPNoLostUpdate(t *testing.T) {
	sys := testutil.NewSystem(t, testutil.WithNetMode("AAP"), testutil.WithMaxAAP(64))
	ids := sys.API.CreateTeams(t, "hacka", 20)
	sys.RunUntil(t, 20*time.Second, func() bool { return sys.CountPhase("ACTIVE") == 20 })
	require.ElementsMatch(t, sys.LeaseIPs(ids), sys.Neutron.HostPairs("cstam-host-a"))
	require.Less(t, sys.Neutron.Count("SetAllowedAddressPairs"), 10) // coalesced
}

// Same name registered 20 times concurrently: exactly one wins.
func TestDuplicateNameRace(t *testing.T) {
	sys := testutil.NewSystem(t)
	codes := sys.API.ConcurrentCreate(t, "hacka", "team1", 20) // 20 different idempotency keys
	require.Equal(t, 1, count(codes, 202))
	require.Equal(t, 19, count(codes, 409))
}
```

### Do it by hand (on FelCloud, after the tests pass)

Deploy `cpd` to `cstam-cp` (the agents from Steps 4–5 are already enrolled), create a hackathon and register one team:

```bash
export CP=https://10.20.0.5     # via SSH tunnel: ssh -L 8443:10.20.0.5:443 ubuntu@<CP_FIP>
TOKEN=$CSTAM_BOOTSTRAP_TOKEN
curl -sk -H "Authorization: Bearer $TOKEN" -X POST $CP/v1/hackathons -d '{
  "slug":"hacka","name":"Hackathon A","status":"OPEN",
  "starts_at":"2026-10-20T08:00:00Z","ends_at":"2026-10-21T20:00:00Z","max_teams":40,"default_ttl":"4h"}'
HID=$(curl -sk -H "Authorization: Bearer $TOKEN" $CP/v1/hackathons | jq -r '.[0].id')

curl -sk -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: $(uuidgen)" \
  -X POST $CP/v1/hackathons/$HID/teams -d '{"name":"team1","display_name":"Team One","apps":[]}' | jq .
watch -n 0.5 "curl -sk -H 'Authorization: Bearer $TOKEN' $CP/v1/teams?hackathon_id=$HID | jq '.items[] | {name,phase,ip}'"
```

Then check that OpenStack agrees with the database:

```bash
openstack port list --tags cstam-env:prod -c Name -c "Fixed IP Addresses" -c Status
openstack port show cstam-team-<team-uuid> -c device_id -c status -c security_group_ids -c tags
curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: team1.hacka.cstam.felcloud.tn' http://<FIP>/   # 503 maintenance page: nothing deployed yet
# deploy the test app from Step 5 (team1-web on :80), then:
curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: team1.hacka.cstam.felcloud.tn' http://<FIP>/   # 200
```

### Done when

::: done
**Done when**
```bash
go test -race ./internal/lifecycle/... ./internal/agentapi/... ./internal/openstack/...
#   TestProvisionHappyPath, TestIdempotentPost, TestTimeoutAfterCommit, TestCrashAfterLease,
#   TestAAPNoLostUpdate, TestDuplicateNameRace: all PASS

# On FelCloud:
# 1. one POST -> ACTIVE in < 10 s (cold) or < 3 s (warm port), visible in the team's steps list
# 2. exactly one Neutron port per team:
openstack port list --tags cstam-team:<id> -f value -c "Fixed IP Addresses" | wc -l       # 1
# 3. kill -9 cpd in the middle of 5 registrations, restart: all 5 end ACTIVE, no duplicate ports
sudo systemctl kill -s KILL cpd; sleep 5; sudo systemctl start cpd
openstack port list --tags cstam -f value -c "Fixed IP Addresses" | sort | uniq -d          # prints nothing
# 4. the subdomain answers through the floating IP once the test app runs on :80
curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: team1.hacka.cstam.felcloud.tn' http://<FIP>/   # 200
```
:::

### Pitfalls

::: warn
**The NOTIFY payload is a hint, not data**

`pg_notify` messages are not durable and are delivered only to sessions listening at that moment. Never put the work itself in the payload. The payload is "something changed for team X"; the worker always re-reads the row. The 1-second poll catches any lost notification.
:::

::: warn
**MTU 1442 on everything you create**

In pod-NIC mode the NIC inherits the network MTU from Neutron (1442) only if the host agent sets it (`ip -n <team> link set eth0 mtu 1442`). In AAP mode the Docker bridge must be created with `com.docker.network.driver.mtu=1442`. A wrong MTU passes every health check (small responses) and then breaks large downloads during the demo.
:::

::: warn
**Gateway slots are a finite resource**

Each exposed app uses one `be_slotNNN` backend (128 by default). Count them in the admission check (`max_teams × apps per team ≤ free slots`). Resizing the slot pool is a *structural* change (reload), which is fine but should not happen during the stress test.
:::

::: warn
**Two IPs per team are impossible, by design**

If you later let a team expose apps on several IPs, you break `lease_live_team`. Keep one IP per team and many ports/hostnames on it.
:::

### Learning checkpoint

::: learn
**Answer these yourself before Step 8**

1. `cpd` crashes after Neutron created the port but before the port ID was saved. Walk through exactly what the next worker does, step by step, and why no second port is created.
2. Why does a frozen worker that wakes up after its claim expired not corrupt the team row? Which SQL clause stops it?
3. In AAP mode, why would 10 concurrent registrations lose updates without the syncer, and how does `If-Match` turn the lost update into a retry?
4. Why do we wait for the **master** gateway's ack before ACTIVE, but not for the backup's?
5. What is in a `GatewayDesired` snapshot, and why do we always send the full map rather than "add this route"?
:::

### Driving Claude Code for this step

::: claude
**Prompts that work well**

- *"Plan mode. Read chapter 16. Propose the files for internal/lifecycle (controller, steps, errors), internal/openstack (interface, gophercloud impl, fake) and internal/agentapi (Connect service, snapshot builders). Show the Step list for pod-NIC and for AAP side by side. No code yet."*
- *"Implement the fake Neutron and FakeNova exactly as specified in §7.10, then TestTimeoutAfterCommit. Run it and show me it passes. Then implement teamPort so that it passes."* (test first, then code).
- *"Explain classifyNeutron: list each Neutron error body we map, with the HTTP code and the class (retryable, fatal for team, fatal for system). Show me how you tested each mapping."*

**What to review in the diff:** no DB transaction around network calls; every fenced UPDATE has `claim_epoch = $epoch`; `Apply` treats 409 as "look again" and never deletes a port it did not tag; the agent protocol sends full snapshots; retries are small at the call site.
:::
