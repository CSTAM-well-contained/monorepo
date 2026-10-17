# 20. Step 11 – Stress tests, chaos and the benchmark report

Phase 2 of the challenge has two kinds of points. One kind is for behavior: the concurrent provisioning stress test (15 points) and automated teardown (10 points). The other is a deliverable: a **benchmark report** covering provisioning latency, gateway failover time, IP recycling speed and load test metrics. Teams are selected for the final from what they submit, so treat the report as a feature you build, not a document you write at the end.

This step builds three Go tools (`cstam-bench`, `failover-probe` and `loadgen`) and a chaos script set. It defines exactly what each number means, and generates the report and its charts with one command.

::: note
**Goal:** `make bench-all` runs every measurement on FelCloud and regenerates `docs/benchmark-report.md` with tables and charts; stress test 10 and 20 concurrent registrations pass with 0 errors · **Points served:** Stress test (15), Teardown & lifecycle (10), benchmark report deliverable, pitch numbers · **Time estimate:** 3–4 days · **Depends on:** Gate G2 (Step 10)
:::

## 11.1 Understand first: measuring honestly

Three ideas decide whether your numbers are worth anything.

**1. Define start and stop for every metric.** "Provisioning takes 2 s" means nothing until you say from which event to which event. Is it the moment the API returned 202, or the moment the team URL first answers 200 through the floating IP? The jury will ask. Every metric in this chapter has a written start and stop.

**2. Use open-loop load generation.** A closed-loop tool (like `hey`) sends the next request only after the previous one returned. When the server stalls during a failover, the tool also stalls, sends fewer requests, and reports fewer errors than a real user population would see. This effect is called *coordinated omission*. An open-loop tool (vegeta) sends at a constant rate no matter what. Every request that hits the gap is recorded as a failure, which is the honest answer.

**3. Do not over-read percentiles from few samples.** With 10 registrations, the "p99" is just the slowest one. Pool repeated runs (5 runs × 10 teams = 50 samples), report p50, p95 and max, and give p99 only when you have at least 100 samples. Say this in the report; it shows maturity.

## 11.2 The metrics, with exact definitions

| Metric | Start → stop | Instrument | Runs |
|---|---|---|---|
| **Provisioning latency** | client sends `POST /v1/hackathons/{id}/teams` → first HTTP 200 for the team's FQDN **through the floating IP** | `cstam-bench register` | concurrency 1, 10, 20, 50 × 5 repetitions |
| Provisioning: time to ACTIVE | POST sent → SSE event `team.phase=ACTIVE` | same tool (SSE) | same |
| Provisioning step breakdown | `started_at`/`finished_at` of each step: lease, neutron.port (warm adopt or create), nova.attach (cold only), host.sync, route+generation, gateway acks, health | `operations` table via `GET /v1/bench/ops?since=` | same |
| Provisioning success rate | teams ACTIVE / teams requested; leaked resources after teardown (ports tagged `cstam-env:bench` not in DB) | `cstam-bench` + `cstamctl doctor leftovers` | same |
| **Failover time** (per failure type) | fault injected (timestamp T0) → first 200 served by the other gateway (T1). Also: **longest gap** between two successful probes | `failover-probe` 10 ms interval + injector | 10 per type |
| Requests lost during failover | failed probes between last success on old master and first success on new master | probe CSV | same |
| Failback | `force-backup` flag cleared on peer → old master MASTER again; lost requests | probe + agent events | 5 |
| **IP recycle time** | (a) `DELETE /teams/x` → all resources gone (route acked on both gateways, port deleted); (b) → IP state `FREE` (includes quarantine); (c) → IP leased again to a new team | DB `events` on `ip.state` | 20 |
| Reload time | (a) runtime route add: agent receives generation → `add map` done; (b) structural reload: `reload` sent → verify OK with new version; (c) end to end: API change → first 200 for the new host via VIP | agent timings in `config_versions` + probe | 50 / 20 / 20 |
| Validation time | `haproxy -c` duration for 10 / 50 / 128 slots | agent timing | 20 |
| **Rollback time** | first failed verify probe → verify succeeds with the old version | agent `rollback_ms` + probe | 20, target < 1 s |
| Requests lost during reload/rollback | non-2xx + transport errors during the action windows | `loadgen` | 2 × 60 000 requests |
| Throughput / latency through the gateway | max rps with p99 < 200 ms and 0 errors; p50/p95/p99 at 25/50/75 % of it | `loadgen` L-01, L-02 | 3 |
| Rate-limit isolation (bonus) | 429 ratio for the flooding team; p99 change for the neighbor team | L-03 | 3 |
| Resource usage | CPU and RAM of HAProxy, agents, cpd, Postgres during L-01 and the 50-team burst | `pidstat -r -u 1`, `docker stats --no-stream` | – |

Expected orders of magnitude, so you can sanity-check your results. These are **estimates to be replaced by measurements**:

| Metric | Expected |
|---|---|
| Provisioning, warm pool, concurrency 1 | 1–2 s to first 200 |
| Provisioning, concurrency 10 | p95 < 5 s (warm pool large enough), < 10 s acceptable |
| Provisioning, concurrency 50 | p95 < 30 s (cold attaches dominate) |
| Failover, HAProxy killed / keepalived stopped | 0.2–1 s |
| Failover, VM crash (`advert_int 0.5`) | ~1.8 s + OVN rebind; p95 < 3 s |
| Runtime route add on both gateways | < 100 ms |
| Structural hitless reload | 50–300 ms |
| Rollback after failed verify | 300–800 ms |
| Requests lost during 20 reloads | 0 |
| IP recycle (to FREE) | 60 s quarantine + 3–6 s teardown |

## 11.3 The provisioning stress tool: `cstam-bench register`

### 11.3.1 Design

The tool must start N registrations **at the same instant**. Otherwise "10 concurrent" really means "10 in a row with some overlap". It uses a barrier: every goroutine is created first and waits on a closed channel. All of them are released together.

Timestamps come from two places:

- **Client side:** POST sent, 202 received, SSE `ACTIVE` received, first 200 through the floating IP.
- **Server side:** the `operations` table (one row per step per team), fetched after the wave. This tells you *where* the time went.

Each wave uses its own tag (`cstam-env:bench`, team names `bench-<run>-<i>`), so cleanup and leak checks are exact.

### 11.3.2 Code

```go
// cmd/cstamctl/bench_register.go (package main, subcommand "bench register")
type Result struct {
	TeamID   string
	Name     string
	IP       netip.Addr
	Sent     time.Time
	Accepted time.Duration // POST -> 202
	Active   time.Duration // POST -> SSE ACTIVE
	FirstOK  time.Duration // POST -> first 200 via FIP
	Err      string
}

func runWave(ctx context.Context, api *client.Client, fip, hackID string, n int, run string) []Result {
	res := make([]Result, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	events := api.Subscribe(ctx, "team") // one SSE stream for the whole wave, filtered by team id later
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("bench-%s-%02d", run, i)
			<-start // barrier: every goroutine waits here
			r := Result{Name: name, Sent: time.Now()}
			team, err := api.CreateTeam(ctx, hackID, name, uuid.NewString(), whoamiApp())
			if err != nil {
				r.Err = err.Error()
				res[i] = r
				return
			}
			r.TeamID, r.Accepted = team.ID, time.Since(r.Sent)
			st, err := events.WaitPhase(ctx, team.ID, "ACTIVE", 120*time.Second)
			if err != nil {
				r.Err = err.Error()
				res[i] = r
				return
			}
			r.IP, r.Active = st.IP, time.Since(r.Sent)
			if err := probeUntil200(ctx, "http://"+fip+"/", team.FQDN, 30*time.Second); err != nil {
				r.Err = "first-200: " + err.Error()
			}
			r.FirstOK = time.Since(r.Sent)
			res[i] = r
		}(i)
	}
	close(start) // release all goroutines within ~1 ms
	wg.Wait()
	return res
}

func probeUntil200(ctx context.Context, url, host string, max time.Duration) error {
	c := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		req.Host = host
		if resp, err := c.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("timeout")
}
```

After each wave the tool:

1. Fetches step durations: `GET /v1/bench/ops?since=<wave start>` (returns rows of `operations` for teams named `bench-<run>-*`).
2. Checks correctness: number of distinct IPs = n; all IPs inside 10.20.0.32–191; invariant SQL (Step 6) returns 0 rows through `GET /v1/bench/invariants`.
3. Deletes all teams of the wave, waits for `DELETED`, records teardown timings.
4. Runs `cstamctl doctor leftovers --env bench`. It must find 0 ports, 0 map entries, 0 netns.
5. Writes `bench/results/<run-id>/register-c<N>-r<k>.csv` and a JSON summary.

```bash
./bin/cstamctl bench register --api $API --token $CSTAM_BENCH_TOKEN --fip $FIP \
   --hackathon hacka --concurrency 10 --repeat 5 --out bench/results/$RUN_ID
```

::: warn
**Warm pool size decides the 10-team number.** If the warm pool holds 4 NICs and you send 10 registrations, 6 of them wait for cold attaches that Nova serializes per host. Set the pool size to at least the expected burst per host, and report both cases: "10 concurrent with warm pool 12: p95 X s" and "50 concurrent (pool exhausted, cold attaches): p95 Y s". Both are true and both are interesting.
:::

### 11.3.3 The churn test (lifecycle security)

The teardown score (10 points) is about cleaning up *everything*, every time. A churn test proves it statistically:

```bash
./bin/cstamctl bench churn --cycles 500 --parallel 5 --hackathon hacka
# loop: create team, wait ACTIVE, delete, wait DELETED; random 0-3 s pauses; 5 in parallel
# every 50 cycles: invariants check + leftovers check
```

Pass criteria: 500/500 cycles complete; 0 leaked ports, 0 stale map entries, 0 netns, 0 containers; the invariant queries return 0 rows after every check; no IP ever leased to two teams (checked from the `leases` history table at the end).

## 11.4 The failover benchmark

### 11.4.1 Probe

The probe from Step 1 (`bench/failover-probe`) opens a **new TCP connection for each request**, every 10 ms, with a 300 ms timeout. It prints one line per request: timestamp, `X-Served-By`, status, ok/err, latency. A new connection per request measures the network gap itself. If you reused keep-alive connections, you would also measure TCP retransmission timers (initial RTO 1 s), which is a client effect, not your system.

Run it in two places:

- From the **control plane VM** against the VIP (`http://10.20.0.10/`) with `Host: canary.gw.internal`: measures the internal path.
- From your **laptop** against the floating IP: measures the public path. Your home connection adds noise; say so.

### 11.4.2 Injector

`bench/failover` runs the fault command over SSH and records `time.Now()` just before sending it:

| Type | Command (on the current MASTER) | What it simulates |
|---|---|---|
| F-haproxy | `sudo systemctl kill -s KILL haproxy` (with `systemctl mask --runtime haproxy` during the test) | process crash, VM alive: `vrrp_track_process` |
| F-keepalived | `sudo systemctl stop keepalived` | graceful stop: priority 0 advert |
| F-vm | `echo c \| sudo tee /proc/sysrq-trigger` (kernel panic) | the live demo crash: silent failure, 3 × advert_int detection |
| F-planned | `POST /v1/gateways/gw1:failover` (writes `/run/cstam/force-backup`) | planned maintenance from the dashboard |

After each run the injector restores the node (`systemctl unmask/start`, `openstack server reboot --hard cstam-gw1` for F-vm), waits until the node is `synced` and `BACKUP`, then waits 30 s before the next run. Because both nodes use `nopreempt`, the master alternates between gw1 and gw2. That is fine: the probe records `X-Served-By`, so each run knows which node it hit.

```bash
./bin/cstamctl bench failover --type vm --runs 10 --probe-host cstam-cp --out bench/results/$RUN_ID
```

### 11.4.3 Computing the numbers

From the probe CSV, for each run:

- `T0` = injection timestamp.
- `last_old` = timestamp of the last 200 from the old master.
- `T1` = timestamp of the first 200 from the new master.
- **failover time** = `T1 − T0`; **gap** = `T1 − last_old`; **lost** = number of failed probes between them.

Cross-check with the keepalived journal on the new master (`journalctl -u keepalived -o short-precise | grep "Entering MASTER"`) and, if you want the network breakdown, the `tcpdump` capture of VRRP adverts and GARPs (Step 1, experiment E1).

Report mean ± standard deviation and max per type. Ten runs per type is enough for a mean and a max; it is not enough for a p99, so do not print one.

## 11.5 Reload and rollback under load: the zero-loss proof

This is the single most convincing artifact for the 10 reload/rollback points. It shows tens of thousands of requests with zero failures while you change routes, reload, and push broken configs.

### 11.5.1 The load generator

`bench/loadgen` uses vegeta as a library (`github.com/tsenart/vegeta/v12/lib`): constant rate, every request recorded.

```go
// bench/loadgen/main.go (core)
rate := vegeta.Rate{Freq: *rps, Per: time.Second}
targeter := vegeta.NewStaticTargeter(vegeta.Target{
	Method: "GET", URL: *url, Header: http.Header{"Host": []string{*host}},
})
attacker := vegeta.NewAttacker(
	vegeta.Timeout(*timeout),
	vegeta.KeepAlive(*keepalive),
	vegeta.MaxWorkers(2000),
)
w := csv.NewWriter(out) // ts_unix_ns, latency_ns, code, error, served_by
for res := range attacker.Attack(targeter, rate, *dur, "cstam") {
	w.Write([]string{
		strconv.FormatInt(res.Timestamp.UnixNano(), 10),
		strconv.FormatInt(int64(res.Latency), 10),
		strconv.Itoa(int(res.Code)),
		res.Error,
		res.Headers.Get("X-Served-By"),
	})
}
w.Flush()
```

### 11.5.2 The three tests

| ID | What happens during the run | Pass criteria |
|---|---|---|
| R-01 | Push a config with a syntax error (`bind :80 sslx`) through `POST /v1/config-versions` | API answers 202, operation ends `REJECTED_VALIDATION`; `X-Config-Version` on both gateways unchanged; 0 failed requests |
| R-02 | Push a config that passes `haproxy -c` but breaks routing (remove the `use_backend` line, so the canary returns 404) | operation ends `ROLLED_BACK`; `rollback_ms` < 1000 on both gateways; failed requests only for the canary host during < 1 s, 0 for team hosts (if the probe uses a team host, report the count honestly) |
| R-03 | 30 route adds/removes (runtime), 20 structural hitless reloads, 1 R-01, 1 R-02 (on a separate host header) | **0 non-2xx and 0 transport errors over 60 000 requests**, run twice: `--keepalive=false` (new connection per request, worst case for socket handover) and `--keepalive=true`; p99 during action windows within +20 % of baseline |

The action script (`bench/reloader`) writes every action with a nanosecond timestamp to `actions.csv`, so the chart can draw a vertical marker for each one. Cross-check with HAProxy's own counters: take `show stat` before and after the run, and confirm that the frontend columns `ereq`, `econ`, `eresp` and `hrsp_5xx` did not move.

```bash
./bin/loadgen --url http://10.20.0.10/ --host team1.hacka.cstam.felcloud.tn --rps 500 --dur 120s --keepalive=false \
   --out bench/results/$RUN_ID/r03-nokeepalive.csv &
./bin/cstamctl bench reloader --routes 30 --reloads 20 --bad-syntax 1 --bad-valid 1 \
   --out bench/results/$RUN_ID/r03-actions.csv
wait
```

::: why
**Why R-03 must use new connections too.** With keep-alive, most requests reuse a connection opened before the reload, so the listener handover is never really tested. With a new connection per request, hundreds of connections arrive *during* each reload. They succeed only because the new worker inherited the listening socket (`expose-fd listeners`). That is the property you claim, so that is the property you must test.
:::

## 11.6 IP recycle timing

The IPAM item says "short leases and immediate release". The edges.md safety rule says "never reuse an IP while anything still points at it". The report must show both: how fast an IP becomes safe, and how long the configured quarantine adds on top.

```bash
./bin/cstamctl bench recycle --teams 20 --quarantine 60s
./bin/cstamctl bench recycle --teams 20 --quarantine 5s     # "benchmark mode", to show the floor
```

For each team the tool records, from the `events` table:

- `t_delete`: DELETE accepted.
- `t_unrouted`: both gateways acked the removal generation (subdomain returns 404).
- `t_released`: Neutron port deleted, IP in `QUARANTINE`.
- `t_free`: IP back to `FREE`.
- `t_reused`: IP leased to a new team (the tool immediately creates teams to consume the freed IPs; LRU ordering means you may need to exhaust the pool to see reuse, so the tool creates teams until it sees the IP again).

Report "safe to reuse" (`t_released − t_delete`) and "reusable" (`t_free − t_delete`) separately.

## 11.7 Load scenarios

| ID | Scenario | Purpose |
|---|---|---|
| L-01 | Ramp 100 → 4000 rps through the VIP to a static team app, steps of 30 s, keep-alive on | max throughput before p99 > 200 ms or errors |
| L-02 | Same, directly to the team IP from a gateway (bypassing HAProxy) | gateway overhead = L-01 − L-02 |
| L-03 | 2000 rps on team1 with a per-team limit of 100 rps; 50 rps on team2 | per-team isolation of the rate limit: team2 p99 unchanged, team1 gets 429 |
| L-04 | 500 rps for 30 min | stability: HAProxy, agent and cpd memory flat |
| L-05 | The reload and failover runs (R-03, F-*) | resilience numbers |

Run the load generator from the control plane VM against the VIP so your home bandwidth is not the bottleneck, and do one small run from your laptop through the floating IP to prove the public path. State both in the report.

::: warn
**Do not DoS FelCloud.** 4000 rps of small responses inside your own subnet is fine. Do not run high-rate tests through the floating IP from outside, and do not leave L-04 running unattended. "Without disrupting existing infrastructure" is part of the brief.
:::

## 11.8 Chaos scenarios

Run each one with `loadgen` at a constant rate in the background so the impact is measured, not guessed. First make sure clocks are synchronized: `chronyc tracking` on every VM, offset under 5 ms. Otherwise timelines from different machines cannot be compared.

| ID | Fault injection (exact) | Expected behavior | Measure |
|---|---|---|---|
| CH-01 | MASTER gateway kernel panic: `echo c \| sudo tee /proc/sysrq-trigger` | BACKUP becomes MASTER after ~3 × advert_int + skew; floating IP keeps answering | failover time, lost requests |
| CH-02 | HAProxy killed: `systemctl kill -s KILL haproxy` (+ `mask --runtime`) | `vrrp_track_process` → FAULT → peer MASTER | same |
| CH-03 | keepalived stopped: `systemctl stop keepalived` | priority-0 advert → immediate takeover | same |
| CH-04 | Host agent killed: `pkill -9 hostagent` on host-a | data plane unaffected (0 errors on existing teams); new provisioning on host-a waits; systemd restarts the agent; it re-applies desired state | errors (expect 0), recovery time |
| CH-05 | VRRP partition: on gw2 `sudo nft add rule inet filter input ip protocol 112 drop` (temporary table) | both nodes MASTER (split brain). OVN binds the VIP to the last GARP sender. Dashboard shows red "2 masters". Because both gateways acked the same generation, both serve correct routes. Remove the rule → converge | time to detect, time to converge, wrong-team responses (must be 0) |
| CH-06 | Control plane down: `systemctl stop cpd` (and Postgres) | every existing route keeps working; failover still works; dashboard offline; on restart the reconciler finds 0 drift | errors during outage (expect 0) |
| CH-07 | cp ↔ gw2 partition: on gw2 drop traffic to 10.20.0.5:8443 | gw2 stops acking; new teams wait in `RoutesSynced=False` for gw2 (or go Degraded per policy); IPs released meanwhile stay QUARANTINE. Heal → gw2 resyncs → IPs FREE | sync lag, quarantine backlog |
| CH-08 | Team app container killed: `docker kill team1-web` | slot health check fails → maintenance page (bonus) → Docker restart policy brings it back | time to maintenance page, time to recover |
| CH-09 | Postgres restarted during a 10-team burst | in-flight API calls fail cleanly or retry; workers resume after reconnect; no leaked ports (reconciler) | leaked resources (expect 0), completion time |
| CH-10 | Host VM hard reboot: `openstack server reboot --hard cstam-host-a` | after boot, NICs come back in the root namespace unconfigured; the agent re-plumbs every team from desired state; teams healthy again | time to all teams healthy (target < 90 s) |
| CH-11 | Control plane killed mid-burst: `systemctl kill -s KILL cpd` 2 s after starting a 20-team wave | after restart, claims expire, workers resume; 20/20 ACTIVE, 0 orphans | completion time, orphans |

Script them in `bench/chaos/` as small shell scripts that print timestamps. Results go to `docs/notebook/experiments.md` and the report.

## 11.9 Charts

`bench/report` (Go) reads every CSV and JSON file in `bench/results/<run-id>/` and writes `docs/benchmark-report.md` plus PNG charts made with `gonum.org/v1/plot`. Generated charts mean the report is always in sync with the data, and you can regenerate everything after a fix.

| # | Chart | What it shows |
|---|---|---|
| 1 | Provisioning latency by concurrency: box plot for 1/10/20/50 + table of p50/p95/max | scalability |
| 2 | Provisioning step breakdown: stacked horizontal bars per concurrency | where the time goes (usually Neutron/Nova) |
| 3 | Failover timeline, one panel per failure type: x = seconds from injection, y = latency; green dots = 200 from gw1, blue = 200 from gw2, red = error; vertical line at T0 | what the failover looks like |
| 4 | Failover time distribution: strip plot of 10 runs per type, mean marker | consistency |
| 5 | Reload under load: latency over time, vertical markers for each action, headline "0 / 60 000 failed" | the zero-loss proof |
| 6 | IP lifecycle: Gantt-like bars per IP: teardown phases, quarantine, re-lease | recycling |
| 7 | Throughput curve: achieved vs offered rps, p99 vs offered rps (via VIP and direct) | gateway capacity and overhead |

```go
// bench/report/charts.go (excerpt: failover timeline)
func failoverTimeline(rows []ProbeRow, t0 time.Time, out string) error {
	p := plot.New()
	p.Title.Text = "Failover: kernel panic on MASTER"
	p.X.Label.Text = "seconds since injection"
	p.Y.Label.Text = "latency (ms)"
	var gw1, gw2, errs plotter.XYs
	for _, r := range rows {
		x := r.TS.Sub(t0).Seconds()
		switch {
		case !r.OK:
			errs = append(errs, plotter.XY{X: x, Y: 300})
		case r.ServedBy == "gw1":
			gw1 = append(gw1, plotter.XY{X: x, Y: float64(r.Latency.Milliseconds())})
		default:
			gw2 = append(gw2, plotter.XY{X: x, Y: float64(r.Latency.Milliseconds())})
		}
	}
	for _, s := range []struct {
		pts plotter.XYs
		c   color.Color
		n   string
	}{{gw1, color.RGBA{47, 158, 68, 255}, "gw1"}, {gw2, color.RGBA{25, 113, 194, 255}, "gw2"}, {errs, color.RGBA{224, 49, 49, 255}, "error"}} {
		sc, err := plotter.NewScatter(s.pts)
		if err != nil {
			return err
		}
		sc.GlyphStyle.Color = s.c
		sc.GlyphStyle.Radius = vg.Points(1.5)
		p.Add(sc)
		p.Legend.Add(s.n, sc)
	}
	return p.Save(18*vg.Centimeter, 8*vg.Centimeter, out)
}
```

## 11.10 Report template

```markdown
# CSTAM × FelCloud – Benchmark Report (v1.0, <date>)

## 1. Summary
| Metric | Target | Result (p50 / p95 / max) | Verdict |
|---|---|---|---|
| Provisioning, 10 concurrent (to first 200 via FIP) | p95 < 10 s, 0 errors | … | … |
| Provisioning, 20 concurrent | 0 errors | … | … |
| Failover, HAProxy killed | < 1 s | … | … |
| Failover, VM crash | p95 < 3 s | … | … |
| Requests lost during 20 reloads + 2 bad configs | 0 / 60 000 | … | … |
| Rollback after failed verify | < 1 s | … | … |
| IP safe-to-reuse / reusable | … / 60 s + … | … | … |
| Leaked resources after 500 churn cycles | 0 | … | … |

## 2. Environment
VMs (name, flavor, image, kernel), HAProxy / keepalived / Docker / Go / PostgreSQL versions,
topology diagram, where the load generator runs, clock sync (chrony offsets),
git commit SHA of the system and of the bench tools, warm pool size.

## 3. Methodology
Definitions of every metric (start → stop), number of runs, statistics method
(pooled samples, nearest-rank percentiles, no p99 below 100 samples), known biases.

## 4. Provisioning (latency by concurrency, step breakdown, success rate, leaked resources)
## 5. Gateway failover (4 failure types + failback; time, gap, lost requests)
## 6. Hot reload and rollback (reload, validation and rollback times; the zero-loss proof)
## 7. IPAM and recycling (teardown time, quarantine, reuse time, edges.md safety test)
## 8. Load test (throughput, latency, gateway overhead, rate limiting)
## 9. Chaos results (CH-01 … CH-11: expected vs observed)
## 10. Security test results (table from cstam-sectest with iptables/nft counter deltas)
## 11. Limitations and next steps
## Appendix: commands to reproduce (make bench-all), raw data index with SHA-256 per file
```

## 11.11 One command for everything

```makefile
# Makefile
RUN_ID ?= $(shell date +%Y%m%d-%H%M)
bench-all:
	./bin/cstamctl bench preflight                               # gateways synced, hosts up, pool free >= 60, clocks OK
	./bin/cstamctl bench register --concurrency 1  --repeat 5 --out bench/results/$(RUN_ID)
	./bin/cstamctl bench register --concurrency 10 --repeat 5 --out bench/results/$(RUN_ID)
	./bin/cstamctl bench register --concurrency 20 --repeat 5 --out bench/results/$(RUN_ID)
	./bin/cstamctl bench register --concurrency 50 --repeat 3 --out bench/results/$(RUN_ID)
	./bin/cstamctl bench churn    --cycles 500 --parallel 5    --out bench/results/$(RUN_ID)
	./bin/cstamctl bench failover --type haproxy,keepalived,vm,planned --runs 10 --out bench/results/$(RUN_ID)
	./bin/cstamctl bench reload   --scenario r01,r02,r03       --out bench/results/$(RUN_ID)
	./bin/cstamctl bench recycle  --teams 20                   --out bench/results/$(RUN_ID)
	./bin/cstamctl bench load     --scenario l01,l02,l03       --out bench/results/$(RUN_ID)
	./bin/cstamctl sectest                                      --out bench/results/$(RUN_ID)
	go run ./bench/report --in bench/results/$(RUN_ID) --out docs/benchmark-report.md
	sha256sum bench/results/$(RUN_ID)/* > bench/results/$(RUN_ID)/SHA256SUMS
```

The full run takes about 60–90 minutes (estimate). Run it once per milestone and once more with the final code. Commit the results directory, so the report is reproducible.

## Done when

::: done
**Done when**

- `cstamctl bench register --concurrency 10 --repeat 5` → 50/50 ACTIVE, 0 errors, 50 distinct IPs per wave, 0 leftovers after teardown.
- `--concurrency 20` → 0 errors; `--concurrency 50` reported honestly (even if slower).
- `bench churn --cycles 500` → 0 leaked ports, map entries, namespaces or containers; invariants clean.
- R-03 → 0 failed requests out of 60 000, in both keep-alive modes; R-01 rejected; R-02 rolled back in < 1 s.
- Failover runs: 10 per type recorded with mean, std dev and max.
- `make bench-all` regenerates `docs/benchmark-report.md` with the 7 charts and the summary table filled.
:::

## Pitfalls

::: warn
**Measuring the client, not the system.** A laptop on Wi-Fi adds 20–200 ms of jitter and occasional drops. Run precise measurements from the control plane VM inside the subnet. Use the laptop only for "the public path works" evidence.
:::

::: warn
**Reporting one lucky run.** Always repeat, always report max next to the median. A jury member who sees "failover 0.8 s" and then a 3 s blip in the live demo will trust nothing else in your report.
:::

::: warn
**Forgetting the warm pool and quarantine settings.** Results depend on both. Write them into the environment section of the report, and never change them silently between runs you compare.
:::

## Learning checkpoint

::: learn
**Answer these yourself before moving on**

1. What is coordinated omission, and why would `hey` report a shorter failover than vegeta?
2. Why does the failover probe open a new TCP connection for each request?
3. Your 10-team wave shows p95 = 4 s but one team took 19 s. Where in the step breakdown would you look first, and what are the two most likely causes?
4. Why is "reusable after 60 s" not the same claim as "safe to reuse"? Which one does the edges.md bug care about?
5. Why can R-03 pass with keep-alive on but fail with keep-alive off?
:::

## Driving Claude Code for this step

::: claude
**How to use Claude Code here**

- Tools are a good fit for agentic coding: *"Implement `cstamctl bench register` as described in section 11.3 of my guide. Start with a plan: files, types, how the barrier works, how SSE events are matched to teams. Include a unit test using an httptest server that fakes the API and SSE stream."*
- For the report: *"Write `bench/report` that reads the CSV formats defined in sections 11.3–11.6 and produces the 7 charts with gonum/plot and the markdown summary table. Use golden files for the markdown output."*
- Ask for a review of your statistics: *"Act as a skeptical jury member. Read docs/benchmark-report.md and list every claim that is not supported by the raw data or that over-reads the sample size."*
- Never let the agent run chaos scripts by itself against FelCloud. You run them, you watch them.
:::
