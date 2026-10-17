# 21. Step 12 – The admin dashboard

The dashboard is worth 10 points in Phase 3. It is also the screen the jury watches during the live demo. The brief lists exactly what it must show:

- active team sandboxes and their OpenStack private IPs;
- real-time gateway health (primary vs standby);
- mapped subdomains and live traffic logs;
- manual lifecycle controls: spin-up, teardown, manual gateway failover, manual config rollback.

The single most important design rule: **the dashboard contains no business logic.** Everything it shows comes from the REST API or the SSE stream. Every button calls an API endpoint that `cstamctl` can also call. This keeps the dashboard simple to build, keeps the API as the single audited entry point, and means everything you demo by clicking can also be scripted for the benchmark.

::: note
**Goal:** a React dashboard, embedded in the `cpd` binary, showing every Phase-3 item live and offering the four manual controls · **Points served:** Admin dashboard (10), pitch (10) · **Time estimate:** 3–4 days (can start in week 2 against a mock API) · **Depends on:** OpenAPI spec (Step 6), SSE endpoint (Step 7); wired to the real system after Gate G2
:::

## 12.1 Understand first: where the data comes from

![Figure 21.1 – Dashboard data flow: agents and workers feed the SSE broker, the browser reads REST + SSE and sends actions to the API](diagrams/f15-dashboard-flow.png)

There are two kinds of data:

| Kind | Example | How the browser gets it |
|---|---|---|
| **State** (slowly changing, must be correct) | list of teams, phases, IPs, gateway VRRP state, config versions, IP pool | REST `GET` on page load (TanStack Query cache) + SSE events that update the cache |
| **Streams** (fast, best effort) | live traffic log lines, requests per second | SSE only, bounded buffers, rate-capped per client |

Where the server gets them:

- **Database events.** Every state change in the controller is written in a transaction that also inserts a row into `events` and calls `pg_notify('cstam_events', …)`. The notification is delivered only if the transaction commits, so the browser never sees a state that does not exist.
- **Agent reports.** gwagent and hostagent send heartbeats every 2 s (VRRP state, applied generation, HAProxy up, cert expiry) and the gateway streams traffic log lines (`PushTrafficLogs`). cpd keeps the latest status in memory and in the `gateways`/`hosts` tables.
- **Aggregates.** Every 10 s, cpd writes per-team request counts, 5xx counts and p95 latency to an aggregate table, for the small charts.

## 12.2 Technology and project setup

| Criterion | **React + Vite + TS + Tailwind + TanStack Query (+ shadcn/ui)** | Go `templ` + htmx |
|---|---|---|
| Toolchain | Node + npm, separate build | Pure Go, one binary |
| Live updates | `EventSource` + a hook that patches the query cache | `hx-ext="sse"` swaps HTML fragments |
| Rich widgets (IP grid, charts, filterable log table, config diff) | Large ecosystem | More hand-made JavaScript |
| Visual polish per hour | Very high with shadcn/ui | Medium |
| Agentic coding quality | Excellent | Good |
| Listed in the brief's recommended skills | Yes (React, Tailwind) | No |
| Verdict | **Chosen** | Fallback if the frontend blocks you in week 4 |

```bash
cd web
npm create vite@latest . -- --template react-ts
npm install @tanstack/react-query react-router-dom openapi-fetch
npm install -D tailwindcss @tailwindcss/vite openapi-typescript
npx shadcn@latest init                       # choose: TypeScript, Tailwind, src/components/ui
npx shadcn@latest add button card badge table dialog drawer tabs input select tooltip alert sonner
npx openapi-typescript ../api/openapi/cstam.v1.yaml -o src/api/schema.d.ts
```

`src/api/client.ts` creates a typed client from the generated schema. If the API changes and the dashboard uses an old field, `npm run build` fails. The compiler enforces the contract on both sides: oapi-codegen on the Go side, openapi-typescript on the browser side.

```ts
// web/src/api/client.ts
import createClient from "openapi-fetch";
import type { paths } from "./schema";

export const api = createClient<paths>({
  baseUrl: "/",
  credentials: "same-origin",
  headers: { "X-CSRF": "1" },   // required by cpd for every state-changing request
});
```

Development loop: `npm run dev` with a Vite proxy to cpd (or to a mock server generated from the OpenAPI examples while the backend is not ready). Production: `npm run build` → `web/dist` → embedded in cpd.

### 12.2.1 Embedding in cpd

```go
// web/embed.go
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
```

```go
// internal/api/spa.go
// Serves the dashboard: real files when they exist, index.html for any other non-API path (client-side routing).
func SPAHandler(dist fs.FS) http.Handler {
	sub, _ := fs.Sub(dist, "dist")
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			r = r.Clone(r.Context())
			r.URL.Path = "/" // unknown path -> index.html, React Router takes over
		}
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // Vite file names contain a hash
		}
		files.ServeHTTP(w, r)
	})
}

// in the router: mux.Handle("/v1/", apiHandler); mux.Handle("/", SPAHandler(web.Dist))
```

Same origin for API and dashboard means no CORS, and the session cookie just works.

## 12.3 Pages and wireframes

Navigation: **Overview · Teams · Gateways · Routing & Config · Traffic · IPAM · Audit**. A hackathon selector and a "live" indicator (green dot when the SSE stream is connected) sit in the header.

### Overview

```text
┌ CSTAM Control ────────────────────────────────── hacka ▾ ── ● live ── alice (admin) ┐
│ ┌ Gateways ───────────────────────────┐ ┌ Teams ─────────┐ ┌ IP pool (.32-.191) ──┐ │
│ │ gw1 ★ MASTER   VIP 10.20.0.10 here  │ │ 12 active      │ │ 12 active  3 quar.   │ │
│ │     haproxy ✓  prio 150  gen 42 ✓   │ │  1 provisioning│ │  4 warm  140 free    │ │
│ │ gw2   BACKUP   prio 100  gen 42 ✓   │ │  0 failed      │ │ ███░░░░░░░░░░░░░░░   │ │
│ │ last failover 14:02:11 (1.4 s)      │ │ [+ Spin up]    │ └──────────────────────┘ │
│ └─────────────────────────────────────┘ └────────────────┘                          │
│ ┌ Requests/s (5 min, per gateway) ────────────────┐ ┌ Recent events ─────────────┐ │
│ │ ▁▂▃▅▆▇▇▆▅▅▆▇  gw1    ▁ gw2    5xx: 0             │ │ 14:05 team7 ACTIVE 1.9 s   │ │
│ └──────────────────────────────────────────────────┘ │ 14:04 config v42 applied   │ │
│                                                      │ 14:02 FAILOVER gw1 → gw2   │ │
└──────────────────────────────────────────────────────┴────────────────────────────┘
```

### Teams list and detail drawer

```text
┌ Teams ─────────────────────────────────────────────── [search] [phase ▾] [+ Spin up] ┐
│ Team    Subdomain                      Team IP     Host    Phase              TTL      │
│ team1   team1.hacka.cstam.felcloud.tn  10.20.0.32  host-a  ● ACTIVE           3h12m  ⋯ │
│ team2   team2.hacka.cstam.felcloud.tn  10.20.0.33  host-a  ● ACTIVE           3h12m  ⋯ │
│ team9   team9.hacka.cstam.felcloud.tn  10.20.0.40  host-b  ◌ PROVISIONING 4/7         │
└────────────────────────────────────────────────────────────────────────────────────────┘
Drawer (team1):
  Neutron port cstam-team-0190a1… (ACTIVE, SG cstam-team), NIC eth0 in netns team1, MTU 1442
  Lease: ACTIVE, expires 18:20 (countdown), [Extend TTL]
  Apps: web :80 ● healthy (slot be_slot001)   api :8000 ● healthy (slot be_slot002)
  Provisioning timeline: lease 4 ms · port 310 ms · host 420 ms · route 60 ms · acks 40 ms · health 120 ms
  Last 50 requests (live) · Conditions (IPLeased, PortReady, HostReady, RoutesSynced, Healthy)
  [Teardown]  (type the team name to confirm)
```

### Gateways

```text
┌ gw1 ──────────────── ★ MASTER ┐   ┌ gw2 ──────────────── BACKUP ┐
│ VIP 10.20.0.10   ● held here  │   │ VIP              ○ standby  │
│ keepalived ✓  prio 150        │   │ keepalived ✓  prio 100      │
│ haproxy ✓  up 3h  reload #17  │   │ haproxy ✓  up 3h  reload #17│
│ generation 42  ✓ in sync      │   │ generation 42  ✓ in sync    │
│ config v0007 sha 9f3c…        │   │ config v0007 sha 9f3c…      │
│ agent cert expires in 27 d    │   │ agent cert expires in 27 d  │
│ [Fail over to gw2 (planned)]  │   │                             │
│ [Simulate crash ⚠ demo mode]  │   │                             │
└───────────────────────────────┘   └─────────────────────────────┘
VRRP timeline: ──gw1──────────┤14:02 gw2 (1.4 s)├──────┤14:31 gw1 (0.3 s, planned)├────
```

### Routing & Config

A table of `config_versions`: version, created at, author, reason, sha, outcome (`active`, `rolled_back`, `rejected`), validate/reload/verify/rollback milliseconds. Select two versions to see a side-by-side diff. Buttons: **[Roll back to this version]**, and in demo mode **[Push test config ▾ broken syntax | valid but broken]**. A second tab shows the live route table (FQDN → slot → team IP:port, generation added) with a filter.

### Traffic

A live table: time, gateway, team, host header, method, path, status, total time (ms), client IP. Filters: team, status class (2xx/3xx/4xx/5xx), gateway, free text. A pause/resume button, and a badge "dropped N lines" when the per-client rate cap was hit.

### IPAM

```text
┌ IP pool 10.20.0.0/24 ─────────────────── legend: ░ free ▒ warm ▓ active ◆ quarantine ■ infra ┐
│ .0   ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■     (.1 router, .5 cp, .10 VIP, .11/.12 gw)          │
│ .16  ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■ ■     (.21/.22 hosts)                                   │
│ .32  ▓ ▓ ▓ ▓ ▓ ▓ ▓ ▓ ▓ ▓ ▓ ▓ ◆ ◆ ◆ ▒     hover .44: QUARANTINE until 14:06:31,              │
│ .48  ▒ ▒ ▒ ░ ░ ░ ░ ░ ░ ░ ░ ░ ░ ░ ░ ░            blocked by: none, last team: team7            │
│ .64  ░ ░ ░ ░ ■ ░ ░ ░ ░ ░ ░ ░ ░ ░ ░ ░     (.68 RESERVED: foreign VM sbx-f0699704)           │
│ ...                                                                                            │
│ .192 ■ lab pool ... .200-.250 ■ Neutron allocation pool (not managed by us)                   │
└────────────────────────────────────────────────────────────────────────────────────────────────┘
```

This one picture explains the IPAM to a jury in five seconds. During the demo, tear down a team and let them watch its cell go ACTIVE → RELEASING → QUARANTINE (with a countdown) → FREE.

### Audit

A table of `audit_log`: time, actor, IP, action, target, outcome. Filters by actor, action, target. A **[Verify chain]** button calls `POST /v1/audit:verify` and shows "chain intact (12 483 entries)" or the first broken link.

## 12.4 Data each page needs

| Page | REST (initial load) | SSE topics (live) |
|---|---|---|
| Overview | `GET /v1/summary`, `GET /v1/gateways`, `GET /v1/events?limit=20` | `gateway`, `team`, `metrics` |
| Teams | `GET /v1/teams?hackathon_id=&phase=`, `GET /v1/teams/{id}` | `team`, `provision_step` |
| Gateways | `GET /v1/gateways` (VRRP, priority, VIP holder, generation, config sha, HAProxy status, cert expiry), `GET /v1/gateways/{id}/transitions` | `gateway` |
| Routing & Config | `GET /v1/config-versions`, `GET /v1/config-versions/{v}/diff?against=`, `GET /v1/routes` | `config` |
| Traffic | `GET /v1/traffic/recent?team_id=&limit=200` | `traffic` (filtered server side) |
| IPAM | `GET /v1/pools/{id}/addresses` | `ipam` |
| Audit | `GET /v1/audit?page_token=` | – |

Actions:

| Button | Endpoint |
|---|---|
| Spin up | `POST /v1/hackathons/{id}/teams` (with an `Idempotency-Key` generated per dialog) |
| Teardown | `DELETE /v1/teams/{id}` |
| Extend TTL | `POST /v1/teams/{id}:renew` |
| Planned failover / failback | `POST /v1/gateways/{id}:failover`, `POST /v1/gateways/{id}:failback` |
| Simulate crash (demo mode only) | `POST /v1/gateways/{id}:simulate-crash` |
| Roll back config | `POST /v1/config-versions/{v}:rollback` |
| Push test config (demo mode only) | `POST /v1/config-versions` with `{"template":"…","reason":"demo broken"}` |
| Verify audit chain | `POST /v1/audit:verify` |

## 12.5 Real time with SSE

### 12.5.1 Why SSE and not WebSocket

Server-Sent Events (SSE) is a one-way stream from server to browser over plain HTTP. That is all the dashboard needs, because commands go through normal REST calls. SSE gives automatic reconnection in the browser (`EventSource`), resume with the `Last-Event-ID` header, and cookies sent automatically. WebSocket would add a protocol upgrade, framing code and hand-written reconnection, for a feature (browser → server streaming) we do not use.

### 12.5.2 The broker in Go

The broker has one hard rule: **it never blocks on a slow browser.** If a browser cannot keep up, its events are dropped and counted, and the browser is told to resync. A blocked broker would freeze every other dashboard and, worse, the code paths that publish.

```go
// internal/api/sse/broker.go
type Event struct {
	ID    uint64          `json:"-"`
	Topic string          `json:"topic"` // team, gateway, config, ipam, traffic, metrics, provision_step
	Type  string          `json:"type"`  // e.g. team.phase, gateway.vrrp
	Team  string          `json:"team,omitempty"`
	Data  json.RawMessage `json:"data"`
}

type Sub struct {
	C       chan Event
	Filter  func(Event) bool
	Dropped atomic.Uint64
	limiter *rate.Limiter // only applied to topic "traffic"
}

type Broker struct {
	mu   sync.RWMutex
	subs map[*Sub]struct{}
	ring *Ring[Event] // last 2000 state events, for Last-Event-ID replay
	seq  atomic.Uint64
}

func (b *Broker) Publish(e Event) {
	e.ID = b.seq.Add(1)
	if e.Topic != "traffic" {
		b.ring.Push(e)
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs {
		if !s.Filter(e) {
			continue
		}
		if e.Topic == "traffic" && !s.limiter.Allow() {
			s.Dropped.Add(1)
			continue
		}
		select {
		case s.C <- e:
		default:
			s.Dropped.Add(1) // never block
		}
	}
}

func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")

	sub := b.subscribe(filterFromQuery(r.URL.Query()))
	defer b.unsubscribe(sub)
	// replay what the browser missed while reconnecting
	if last, err := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64); err == nil {
		if missed, ok := b.ring.Since(last); ok {
			for _, e := range missed {
				if sub.Filter(e) {
					writeEvent(w, e)
				}
			}
		} else {
			fmt.Fprint(w, "event: resync\ndata: {}\n\n") // too old: client refetches everything
		}
	}
	_ = rc.Flush()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-sub.C:
			writeEvent(w, e)
			_ = rc.Flush()
		case <-ping.C:
			fmt.Fprintf(w, ": ping dropped=%d\n\n", sub.Dropped.Load())
			_ = rc.Flush()
		}
	}
}

func writeEvent(w io.Writer, e Event) {
	data, _ := json.Marshal(e)
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Topic, data)
}
```

Who publishes:

- a `LISTEN cstam_events` goroutine (dedicated Postgres connection) that turns notifications into `Event`s. If you run two cpd instances, both receive the notifications, so both brokers stay complete;
- the agent service (`ControlService.Report` and `PushTrafficLogs`) for gateway status and traffic lines;
- a 1 s ticker publishing aggregated `metrics` (requests/s and 5xx per gateway and per team).

::: warn
**Timeouts on the way.** If you expose the dashboard through HAProxy (for the pitch), the SSE connection is a long-lived HTTP response. HAProxy's `timeout server` (60 s in our config) would cut it. The 15 s ping keeps the connection active, so it is never idle long enough to time out. Without the ping, you would see the "live" dot blink every minute.
:::

### 12.5.3 The browser hook

```ts
// web/src/hooks/useEventStream.ts
import { useEffect, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";

type Ev = { topic: string; type: string; team?: string; data: any };

export function useEventStream(topics: string[], onTraffic?: (e: Ev) => void) {
  const qc = useQueryClient();
  const [live, setLive] = useState(false);
  const trafficRef = useRef(onTraffic);
  trafficRef.current = onTraffic;

  useEffect(() => {
    const es = new EventSource(`/v1/stream?topics=${topics.join(",")}`, { withCredentials: true });
    es.onopen = () => setLive(true);
    es.onerror = () => setLive(false);            // EventSource reconnects by itself
    es.addEventListener("resync", () => qc.invalidateQueries());
    for (const t of topics) {
      es.addEventListener(t, (msg) => {
        const e: Ev = JSON.parse((msg as MessageEvent).data);
        switch (e.topic) {
          case "team":
            qc.setQueryData(["team", e.data.id], e.data);           // detail drawer
            qc.invalidateQueries({ queryKey: ["teams"] });           // list refetch (cheap, debounced by Query)
            break;
          case "gateway":
            qc.setQueryData(["gateways"], (old: any[] | undefined) =>
              old?.map((g) => (g.name === e.data.name ? { ...g, ...e.data } : g)));
            break;
          case "ipam":
            qc.setQueryData(["ipam"], (old: any) => patchAddress(old, e.data));
            break;
          case "traffic":
            trafficRef.current?.(e);                                 // appended to a bounded array (last 500)
            break;
          default:
            qc.invalidateQueries({ queryKey: [e.topic] });
        }
      });
    }
    return () => es.close();
  }, [topics.join(","), qc]);

  return live;
}
```

Rules: one multiplexed stream per browser tab (browsers allow about 6 HTTP/1.1 connections per origin); traffic lines are rate-capped per client (50 lines/s); charts use the `metrics` topic, never the raw log lines. At 2 000 requests per second you cannot ship every line to a browser, and you do not need to.

## 12.6 Manual controls: how they work technically

| Button | API | What really happens | Safety |
|---|---|---|---|
| **Spin up** | `POST /v1/hackathons/{id}/teams` | Same pipeline as a registration (Step 7). The drawer shows each step live via `provision_step` events. | admin role; audited; idempotency key per dialog |
| **Teardown** | `DELETE /v1/teams/{id}` | Sets `desired=ABSENT`. The controller runs the strict teardown order (Step 8). The drawer shows each step. | type the team name to confirm; audited |
| **Planned failover** | `POST /v1/gateways/gw1:failover` | cpd sets `force_backup=true` in gw1's desired state. gw1's agent writes `1` into `/run/cstam/force-backup`. keepalived's `vrrp_track_file` puts gw1 in FAULT, gw1 sends a priority-0 advert, gw2 becomes MASTER within about one advert interval. No process is killed, so in-flight requests on gw1 finish. | refused (409) if gw2 is not `synced` at the same generation and HAProxy-healthy, unless `?force=true`, because failing over to a stale gateway is the outage you are trying to avoid |
| **Failback** | `POST /v1/gateways/gw1:failback` | clears `force_backup`; gw1 returns to BACKUP. Because of `nopreempt`, it does not take the VIP back by itself; press "failover" on gw2 if you want gw1 to be master again. | same check |
| **Simulate crash** | `POST /v1/gateways/gw1:simulate-crash` | only when cpd runs with `CSTAM_DEMO_MODE=1`. gw1's agent kills HAProxy with SIGKILL and masks its restart for 60 s. This shows the **unplanned** path (`vrrp_track_process`). | demo mode only; double confirm; audited |
| **Roll back config** | `POST /v1/config-versions/41:rollback` | cpd creates a **new** version (for example v43) whose content is a copy of v41, with `reason="rollback to v41 by alice"`. It goes through the normal pipeline on both gateways (validate → reload → verify → commit). History is never rewritten: you roll *forward* to old content. Routes are not part of this; they always come from the current team table, so a rollback can never bring back a deleted team's route. | shows the diff first; audited |

::: why
**Why rollback creates a new version instead of "going back".** If rollback rewrote history, the audit trail would lie, and a gateway that was offline during the rollback would not know which version is current. With roll-forward, every gateway simply converges to the highest version, and the history shows exactly what happened and who did it.
:::

## 12.7 UX rules for master and backup

- The **MASTER card is always on the left**, with a green border, a ★, and the text "VIP 10.20.0.10 is here". Never rely on color alone (color-blind users, projectors that wash out colors).
- The source of truth is what the **agents report**, cross-checked:
  - both report MASTER → full-width red banner "Split brain: 2 masters" (chaos test CH-05);
  - none reports MASTER → red banner "No master: public traffic is down";
  - an agent silent for more than 6 s → its card turns grey with "no heartbeat since …".
- A **config sync badge** compares the acked generation and config sha of both gateways. If they differ: amber "gw2 behind (gen 41 vs 42)". This matters because failing over to a stale config is the second most likely real outage, and the planned-failover button refuses it.
- The **VRRP timeline** under the cards records every transition with its measured duration (from the agents' `notify` timestamps). During the demo, the jury sees "failover 1.4 s" appear by itself.
- Buttons that affect production say the consequence in plain words: "All public traffic will move to gw2. Expected interruption: under 1 second."

## 12.8 Authentication in the UI

- Login page posts to `POST /v1/auth/login`. cpd checks the argon2id hash and sets the session cookie (`HttpOnly; Secure; SameSite=Strict`). The browser never sees a token, so XSS cannot steal one from `localStorage`.
- Two roles: `admin` (everything) and `viewer` (read-only; buttons hidden and the API answers 403 anyway). **Give the jury a viewer account** during the pitch: they can watch everything live on their own laptop but cannot click "teardown".
- Every state-changing request sends `X-CSRF: 1` (see `client.ts`). cpd rejects state-changing requests without it, or with a foreign `Origin`.
- Exposure: by default the dashboard is reachable only from `ADMIN_CIDR` (security group `cstam-cp`, port 443). For the pitch you can publish `admin.cstam.felcloud.tn` through HAProxy with TLS and an IP allowlist; HAProxy talks to cpd over TLS verified against our CA.

## Done when

::: done
**Done when**

- Every Phase-3 item is visible against the real system: active teams with their OpenStack IPs, gateway MASTER/BACKUP live, mapped subdomains, live traffic lines.
- Spin-up from the dashboard creates a team that answers through the floating IP; teardown removes it and the IPAM grid shows the IP going to QUARANTINE then FREE.
- Planned failover moves the VIP to gw2 with 0 failed requests in a 200 rps `loadgen` run; the VRRP timeline shows the transition with its duration.
- Simulate crash (demo mode) produces a visible failover and the banner/cards update without a page reload.
- Rollback to an older config version creates a new version and both gateways converge to it (sync badge green).
- Killing the SSE connection (stop cpd for 10 s) turns the live dot red; on restart it reconnects and the data is correct without a manual refresh.
- A viewer account cannot trigger any action (403 from the API, buttons hidden).
- `npm run build` fails if the OpenAPI spec changes a field the UI uses (try it once by renaming a field).
:::

## Pitfalls

::: warn
**Logic leaking into the UI.** A computed "is this team healthy?" in TypeScript is logic that the CLI and the tests do not see. If the dashboard needs a derived value, add it to the API response.
:::

::: warn
**Rendering 2 000 log lines per second.** The traffic table must keep a bounded array (for example the last 500 lines) and use `requestAnimationFrame` batching or a virtualized table. Otherwise the browser on the demo laptop freezes exactly when the load test runs.
:::

::: warn
**Trusting the dashboard's own view of VRRP.** The dashboard must show what the agents report, not what cpd last asked for. A desired state of "gw1 master" means nothing if keepalived decided otherwise.
:::

## Learning checkpoint

::: learn
**Answer these yourself before moving on**

1. Why is it safe for the broker to drop events for a slow client, and how does the client recover a correct view?
2. Why is `pg_notify` sent inside the transaction instead of after the commit in Go code?
3. What exactly happens, from the button click to the VIP moving, when you press "planned failover"? Which file, which keepalived feature, which advert?
4. Why does the planned failover refuse to run when gw2 is one generation behind?
5. Why does rollback create a new version number?
:::

## Driving Claude Code for this step

::: claude
**How to use Claude Code here**

Frontend code is where agentic coding is strongest, and where it is easiest to accept code you do not understand. Keep control of the data flow and let the agent do the layout.

- Start with the contract: *"Read api/openapi/cstam.v1.yaml. List every field the Overview, Teams and Gateways pages need according to section 12.4 of my guide and tell me which ones are missing from the spec. Do not write UI code yet."*
- Then page by page: *"Build the Gateways page with shadcn/ui cards following the wireframe in section 12.3 and the UX rules in 12.7. Data from `useQuery(['gateways'])` and the `useEventStream` hook. No business logic in components: if a value must be computed, tell me and we add it to the API."*
- Review: check that every button calls exactly one API endpoint, and that nothing in `web/src` contains IP ranges, timing rules or state transitions.
:::
