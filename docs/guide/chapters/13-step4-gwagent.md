# 13. Step 4 – The gateway agent in Go (validate, apply, verify, roll back)

## Step 4 – `gwagent`: every routing change is validated before and verified after

::: note
**Goal:** a Go program, `gwagent`, runs on both gateways. It pulls the desired routing state from the control plane, applies it to HAProxy without reloading for team changes and with a hitless reload for structural changes, checks that the result really serves traffic, rolls back automatically in under one second when it does not, and acknowledges the generation it applied with a digest. It also reports VRRP state, streams traffic logs, and implements the manual-failover flag.
**Points served:** zero-downtime reload and auto-rollback (10), HA gateways (15, because both gateways stay identical), live traffic logs for the dashboard (part of 10), the edges.md safety rule (teardown and lifecycle security, 10).
**Time estimate:** 3–4 days.
**Depends on:** Step 3 (the hand-written `haproxy.cfg`, maps, slots, keepalived files). The control-plane side of the protocol (`WatchGateway`, `Ack`) arrives in Step 7; until then you test the agent against a small fake control plane.
:::

### Understand first

In Step 3 you changed routes by typing runtime commands on each gateway. That works, but it has three problems that the jury will look for:

1. **Nothing stops a bad change.** A typo in the config file takes HAProxy down at the next reload.
2. **Nothing proves the change worked.** HAProxy can accept a config that is valid but routes everything to the 404 backend.
3. **Nothing keeps the two gateways equal.** If you forget gw2, a failover lands on stale routes.

The gateway agent fixes all three with one idea: **the control plane owns the desired state; the agent's only job is to make HAProxy equal to it, safely, and to prove it.**

![Figure 13.1 – The gateway agent pipeline: two safety gates (validate before, verify after), automatic rollback, and an acknowledgement only when the result is verified](diagrams/f08-reload-rollback.png)

#### What the agent is responsible for

| Responsibility | How | Why |
|---|---|---|
| Receive desired state | Dials out to `cpd` (`https://10.20.0.5:8443`, mTLS) and calls `WatchGateway`; receives full `GatewayDesired` snapshots | No inbound management port on the gateway; a lost message is repaired by the next snapshot |
| Apply route changes | Runtime API (`set server addr`, versioned maps) after writing the files | Zero reloads for team add and remove |
| Apply structural changes | Render → `haproxy -c` → swap → master CLI `reload` | Hitless reload with a safety gate |
| Verify | `/__gw/healthz` (config version and routes generation), canary host, map digest | Proof that the new state serves |
| Roll back | Restore last-good files and memory | A bad change never stays live |
| Acknowledge | `Ack(generation, digest, outcome, timings)` | The control plane frees IPs only after both gateways acknowledged (Step 8) |
| Readiness | Creates `/run/cstam/ready` only after the first successful sync | Keepalived's `chk_gateway` keeps a stale gateway out of MASTER |
| Manual failover | Writes `1` or `0` into `/run/cstam/force-backup` from `force_backup` | Dashboard button (Step 12) |
| Report | Heartbeat every 2 s: VRRP state, HAProxy health, applied generation | Dashboard, split-brain detection |
| Traffic logs | UDP syslog listener on `127.0.0.1:5514`, batches to `PushTrafficLogs` | Live traffic view |
| Canary | Tiny HTTP responder on `127.0.0.1:9102` | The always-present route used by verification |

#### Two change classes

| Class | Examples | Mechanism | Typical time |
|---|---|---|---|
| **Route change** (almost all changes) | team added or removed, app port changed, rate limit changed | write files, then runtime API | under 5 ms |
| **Structural change** | slot pool resized, TLS options, new frontend rule, a new template version | render, validate, swap, reload | 50–300 ms |

The agent decides the class by comparing the snapshot with what it last applied: if `structural_version` changed, it is structural; otherwise it is a route change. A structural change also re-applies all routes (the new config is rendered with the full route set, so the files already contain them).

#### The two safety gates

1. **Validate before.** For a structural change, the candidate config is checked by HAProxy itself: `haproxy -c -f candidate.cfg`. If it fails, the live config is not touched at all. For a route change, the agent validates every field itself (hostname syntax, IP inside the team pool `10.20.0.32`–`.191`, port 80 or 8000–8099, slot 1–128, no two hosts on the same slot), because the runtime API cannot be dry-run.
2. **Verify after.** The agent asks the running HAProxy what it is serving: `/__gw/healthz` must answer with the new `X-Config-Version`, the canary host must answer `canary`, and the map content (`show map`) must hash to the digest the control plane sent. Only then is the change "applied".

If the second gate fails, the agent restores the previous state and verifies again. That is the **rollback**, and the time from the first failed probe to the moment the old state is verified is the "rollback time" in the benchmark report (target under 1 s).

#### Generations and the digest

Every change of the routing table in the control plane gets a new, strictly increasing **generation** number (Step 8 explains why it comes from a single locked row). A `GatewayDesired` snapshot says: "at generation 42, the complete route set is this list, and its digest is this hash."

The digest is a SHA-256 over a **canonical text form** of the routes: one line per route, `fqdn slot ip:port`, all lowercase, sorted, each line ending with `\n`. The control plane computes it when it builds the snapshot; the agent recomputes it from what HAProxy really holds (`show map` plus `show servers state`). If they differ, the agent did not apply the snapshot correctly, and it must not acknowledge.

Two rules follow, and both are tested:

- **Never apply a generation lower than the one already applied.** Streams reconnect, responses arrive late; an old snapshot must never overwrite a newer one.
- **Never acknowledge without verifying.** The acknowledgement is the proof the control plane relies on to recycle IPs safely (the edges.md bug).

#### The boot rule (static stability)

When the gateway VM boots, HAProxy starts with whatever files are on disk. They may be old (the VM was down for an hour, or was rebuilt from a snapshot). If that gateway became MASTER immediately, it could route a team's hostname to an IP that has since been given to another team.

So, at start, the agent **removes** `/run/cstam/ready`. Keepalived's `chk_gateway` script requires that file, so the node stays in FAULT and cannot take the VIP. The agent then syncs to the control plane's head generation and only then creates the file.

If the control plane cannot be reached within 10 s, the agent still creates the file, using its last-good state, and reports `synced=false`. A slightly stale gateway is better than no gateway when both are down; the IP-recycling rule in Step 8 already guarantees that IPs removed while a gateway was out of sync are not reused until it catches up.

This is **static stability**: the data plane keeps working on its last-good state when the control plane is gone, and it never needs the control plane to be up to forward traffic.

### Do it by hand

Before writing Go, run the whole pipeline by hand once on gw1. You will automate exactly these commands.

#### 4.1 Directory layout on the gateway

```bash
sudo install -d -o cstam -g haproxy -m 0775 /var/lib/cstam /var/lib/cstam/cfg /var/lib/cstam/maps /var/lib/cstam/lastgood
ls -ld /etc/haproxy/maps /run/cstam
```

```text
/var/lib/cstam/state.json            # applied generation, digest, structural version, slot table
/var/lib/cstam/cfg/haproxy.cfg.v000002   # candidate configs, one per structural version
/var/lib/cstam/maps/hosts.map.g000042    # map snapshot per generation (keep the last 20)
/var/lib/cstam/lastgood/             # copy of the files that last passed verification
/etc/haproxy/haproxy.cfg             # live config (atomically replaced)
/etc/haproxy/maps/hosts.map          # live map file (atomically replaced)
/run/cstam/ready                     # exists only when the agent is synced
/run/cstam/force-backup              # 0 or 1, read by keepalived track_file
/run/cstam/vrrp.state                # written by the keepalived notify script
```

#### 4.2 A structural change, step by step

Make a copy of the live config, change something harmless (the version), and push it through the pipeline:

```bash
cd /var/lib/cstam
sed 's/setenv CFG_VERSION v000001/setenv CFG_VERSION v000002/' /etc/haproxy/haproxy.cfg > cfg/haproxy.cfg.v000002

# Gate 1: validate
time sudo haproxy -c -q -f cfg/haproxy.cfg.v000002 && echo VALID
# expected: VALID, real time around 30-80 ms with 128 slots

# Keep last-good, then swap atomically (rename on the same filesystem is atomic)
sudo cp -a /etc/haproxy/haproxy.cfg lastgood/haproxy.cfg
sudo cp cfg/haproxy.cfg.v000002 /etc/haproxy/haproxy.cfg.tmp
sudo mv /etc/haproxy/haproxy.cfg.tmp /etc/haproxy/haproxy.cfg

# Hitless reload through the master CLI
echo "reload" | sudo socat /run/haproxy-master.sock stdio
# expected first line: Success=1   (then "--" and the startup messages of the new worker)

# Gate 2: verify
curl -si localhost/__gw/healthz | grep -i x-config-version     # X-Config-Version: v000002
curl -s -H 'Host: canary.gw.internal' localhost                 # canary
```

`Success=0` means the new worker failed to start. The old workers are **still serving** (the master keeps them), so nothing is down, but you must roll back so that the files on disk are not a config that cannot start.

#### 4.3 A broken config is rejected

```bash
sed 's/bind :80/bind :80 sslx/' /etc/haproxy/haproxy.cfg > cfg/haproxy.cfg.bad
sudo haproxy -c -q -f cfg/haproxy.cfg.bad; echo "exit=$?"
# expected: an "unknown keyword 'sslx'" message and exit=1. The live file is never touched.
```

#### 4.4 A valid-but-wrong config is rolled back

This is the case the jury likes most: HAProxy accepts the file, but it breaks routing.

```bash
# remove the routing line: everything now goes to be_404, but the syntax is fine
grep -v 'use_backend %\[var(txn.host),map' /etc/haproxy/haproxy.cfg \
  | sed 's/v000002/v000003/' > cfg/haproxy.cfg.v000003
sudo haproxy -c -q -f cfg/haproxy.cfg.v000003 && echo "VALID (but wrong)"

sudo cp -a /etc/haproxy/haproxy.cfg lastgood/haproxy.cfg
sudo install -m 0644 cfg/haproxy.cfg.v000003 /etc/haproxy/haproxy.cfg
echo reload | sudo socat /run/haproxy-master.sock stdio | head -1    # Success=1

curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: canary.gw.internal' localhost   # 404 -> verify FAILS

# ROLLBACK
sudo install -m 0644 lastgood/haproxy.cfg /etc/haproxy/haproxy.cfg
echo reload | sudo socat /run/haproxy-master.sock stdio | head -1
curl -s -H 'Host: canary.gw.internal' localhost                     # canary again
curl -si localhost/__gw/healthz | grep -i x-config-version          # v000002 (the old one)
```

Time it with a stopwatch first; the Go agent will measure it to the millisecond.

#### 4.5 A route change with rollback

```bash
S=/run/haproxy/admin.sock; M=/etc/haproxy/maps/hosts.map
hap() { echo "$*" | sudo socat stdio "$S"; }

hap "show map $M" > /tmp/map.before          # remember N-1

# apply N: add team1 on slot 1
hap "set server be_slot001/s addr 10.20.0.40 port 80"
hap "set server be_slot001/s state ready"
printf 'canary.gw.internal be_slot000\nteam1.hacka.cstam.felcloud.tn be_slot001\n' | sudo tee $M.tmp >/dev/null
sudo mv $M.tmp $M
V=$(hap "prepare map $M" | awk '{print $NF}')
printf 'add map @%s %s <<\n%s\n\n' "$V" "$M" "$(cat $M)" | sudo socat stdio "$S"
hap "commit map @$V $M"

# verify: canary + the new route
curl -s -H 'Host: canary.gw.internal' localhost
curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: team1.hacka.cstam.felcloud.tn' localhost
```

If verification failed, rollback is the same three commands with the content of `/tmp/map.before` (and putting the slot back in maintenance). It takes a few milliseconds because nothing is reloaded.

::: why
**Why write the file before touching memory?**
If the agent crashed between the two operations, one order leaves memory newer than the file: the next reload would silently remove the route. The other order leaves the file newer than memory: the next apply or reload simply catches memory up. "Files first" means a crash can only lose speed, never a route. The same reasoning applies to removal: the file loses the line first, then memory.
:::

### Build it in Go

#### 4.6 Package layout

```text
cmd/gwagent/main.go              # wiring, config, signal handling, systemd notify
internal/gw/haproxy/runtime.go   # admin socket client (one command per connection)
internal/gw/haproxy/master.go    # master CLI: reload with status, show proc
internal/gw/haproxy/render.go    # text/template rendering of haproxy.cfg and maps
internal/gw/haproxy/validate.go  # haproxy -c on a candidate file
internal/gw/applier/applier.go   # the pipeline: diff, apply, verify, rollback, timings
internal/gw/applier/state.go     # state.json, last-good files, atomic writes
internal/gw/keepalived/vrrp.go   # read vrrp.state, write force-backup, ready file
internal/gw/logs/listener.go     # UDP syslog receiver, JSON parse, bounded batches
internal/gw/sync/client.go       # WatchGateway stream, Ack, Report, PushTrafficLogs
internal/gw/canary/canary.go     # 127.0.0.1:9102 responder
internal/edge/digest.go          # canonical route form + SHA-256 (shared with cpd)
```

`internal/edge/digest.go` is imported by both `cpd` and `gwagent`. Having one function for both sides is what makes the digest check meaningful.

```go
// internal/edge/digest.go
package edge

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

// Route is the canonical routing entry, shared by the control plane and the gateway agent.
type Route struct {
	FQDN string // lowercase, no trailing dot
	Slot int    // 1..128
	IP   string // dotted IPv4 inside the team pool
	Port int    // 80 or 8000..8099
}

// Canonical returns the exact text that is hashed: "fqdn slot ip:port\n", sorted.
func Canonical(rs []Route) string {
	lines := make([]string, 0, len(rs))
	for _, r := range rs {
		lines = append(lines, fmt.Sprintf("%s %d %s:%d\n", strings.ToLower(strings.TrimSuffix(r.FQDN, ".")), r.Slot, r.IP, r.Port))
	}
	sort.Strings(lines)
	return strings.Join(lines, "")
}

func Digest(rs []Route) []byte {
	h := sha256.Sum256([]byte(Canonical(rs)))
	return h[:]
}
```

#### 4.7 Configuration

```go
// cmd/gwagent/config.go
type Config struct {
	Name          string        `env:"CSTAM_AGENT_NAME,required"`          // gw1 | gw2
	CPURL         string        `env:"CSTAM_CP_URL,required"`              // https://10.20.0.5:8443
	TLSCert       string        `env:"CSTAM_TLS_CERT,required"`
	TLSKey        string        `env:"CSTAM_TLS_KEY,required"`
	TLSCA         string        `env:"CSTAM_TLS_CA,required"`
	AdminSock     string        `env:"CSTAM_HAPROXY_ADMIN_SOCK" envDefault:"/run/haproxy/admin.sock"`
	MasterSock    string        `env:"CSTAM_HAPROXY_MASTER_SOCK" envDefault:"/run/haproxy-master.sock"`
	Slots         int           `env:"CSTAM_SLOTS" envDefault:"128"`
	LogListen     string        `env:"CSTAM_LOG_LISTEN" envDefault:"127.0.0.1:5514"`
	OwnIP         string        `env:"CSTAM_OWN_IP"`                       // 10.20.0.11; derived from the default route if empty
	StateDir      string        `env:"CSTAM_STATE_DIR" envDefault:"/var/lib/cstam"`
	RunDir        string        `env:"CSTAM_RUN_DIR" envDefault:"/run/cstam"`
	SyncTimeout   time.Duration `env:"CSTAM_BOOT_SYNC_TIMEOUT" envDefault:"10s"`
	VerifyBudget  time.Duration `env:"CSTAM_VERIFY_BUDGET" envDefault:"600ms"`
}
```

The names match `/etc/cstam/gwagent.env` from Step 10. Never hardcode `10.20.0.x` in the agent: the own IP comes from the environment or from the default route, and the pool limits come from `internal/config/topology.go`.

#### 4.8 The runtime socket client

The HAProxy admin socket speaks plain text. In non-interactive mode, the agent opens a connection, writes one line, reads until HAProxy closes the connection.

```go
// internal/gw/haproxy/runtime.go
package haproxy

type Runtime struct{ Socket string }

// Cmd sends one command (or several separated by ';') and returns the full answer.
func (r Runtime) Cmd(ctx context.Context, cmd string) (string, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", r.Socket)
	if err != nil {
		return "", fmt.Errorf("dial %s: %w", r.Socket, err)
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if _, err := io.WriteString(c, cmd+"\n"); err != nil {
		return "", err
	}
	out, err := io.ReadAll(c)
	return strings.TrimSpace(string(out)), err
}

// SetServer points a slot at a team (ready=true) or parks it (ready=false).
func (r Runtime) SetServer(ctx context.Context, slot int, ip string, port int, ready bool) error {
	be := fmt.Sprintf("be_slot%03d/s", slot)
	cmds := []string{fmt.Sprintf("set server %s addr %s port %d", be, ip, port)}
	if ready {
		cmds = append(cmds, "set server "+be+" state ready")
	} else {
		cmds = append([]string{"set server " + be + " state maint", "shutdown sessions server " + be}, cmds...)
	}
	for _, c := range cmds {
		out, err := r.Cmd(ctx, c)
		if err != nil {
			return err
		}
		if out != "" && !strings.Contains(out, "IP changed") && !strings.Contains(out, "no need to change") {
			return fmt.Errorf("%q: %s", c, out) // HAProxy prints an error text, never an exit code
		}
	}
	return nil
}

// ReplaceMap swaps the whole map atomically: prepare, add @v (one payload), commit.
func (r Runtime) ReplaceMap(ctx context.Context, mapPath string, lines []string) error {
	resp, err := r.Cmd(ctx, "prepare map "+mapPath) // "New version created: 7"
	if err != nil {
		return err
	}
	f := strings.Fields(resp)
	if len(f) == 0 {
		return fmt.Errorf("prepare map: %q", resp)
	}
	ver := f[len(f)-1]
	payload := fmt.Sprintf("add map @%s %s <<\n%s\n", ver, mapPath, strings.Join(lines, "\n"))
	if out, err := r.Cmd(ctx, payload); err != nil || out != "" {
		_, _ = r.Cmd(ctx, fmt.Sprintf("clear map @%s %s", ver, mapPath)) // drop the draft
		return fmt.Errorf("add map: %v %q", err, out)
	}
	out, err := r.Cmd(ctx, fmt.Sprintf("commit map @%s %s", ver, mapPath))
	if err != nil || out != "" {
		return fmt.Errorf("commit map: %v %q", err, out)
	}
	return nil
}

// ShowMap returns "key value" pairs currently live.
func (r Runtime) ShowMap(ctx context.Context, mapPath string) (map[string]string, error) {
	out, err := r.Cmd(ctx, "show map "+mapPath) // lines: "0x55d1... key value"
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 3 {
			m[f[1]] = f[2]
		}
	}
	return m, nil
}
```

::: warn
**HAProxy answers errors as text, not as status codes.** A command can "succeed" at the socket level and still print `No such server.` Every call must check the text. The tests in 4.14 run each command against a real HAProxy so the expected answers are known, not guessed. Check the exact strings for your version once with `socat`, and keep them in one place.
:::

#### 4.9 Reload through the master CLI

```go
// internal/gw/haproxy/master.go
type Master struct{ Socket string }

// Reload asks the master for a hitless reload and reports whether new workers started.
// HAProxy >= 2.7 answers "Success=1" or "Success=0", then "--", then the startup log.
func (m Master) Reload(ctx context.Context) (ok bool, startupLog string, err error) {
	out, err := Runtime{Socket: m.Socket}.Cmd(ctx, "reload")
	if err != nil {
		return false, "", err
	}
	first, rest, _ := strings.Cut(out, "\n")
	switch strings.TrimSpace(first) {
	case "Success=1":
		return true, rest, nil
	case "Success=0":
		return false, rest, nil
	default:
		return false, out, fmt.Errorf("unexpected reload answer %q", first)
	}
}
```

The master keeps the old workers alive until the new ones are ready, and old workers finish their in-flight requests (`hard-stop-after 30s`). That is why a reload drops no request.

#### 4.10 Validation of a candidate config

```go
// internal/gw/haproxy/validate.go
func Validate(ctx context.Context, cfgPath string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "haproxy", "-c", "-q", "-f", cfgPath) // argv, never "sh -c"
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &RejectedError{Output: stderr.String()}
	}
	return nil
}

type RejectedError struct{ Output string }

func (e *RejectedError) Error() string { return "haproxy -c rejected config: " + e.Output }
```

`haproxy -c` also opens the map files the config references. That is why the candidate config references candidate map paths under `/var/lib/cstam/maps/`, and the swap step renames those files into `/etc/haproxy/maps/`. If you validate a config that points at the live maps, you are not testing the maps you are about to install.

#### 4.11 Rendering

The control plane sends the structural template body (`structural_body`) only when `structural_version` changes. The agent stores it under `/var/lib/cstam/cfg/template.v000002` and executes it with its own values. The template is plain Go `text/template`, rendered from typed data only (never from free strings coming from teams):

```go
// internal/gw/haproxy/render.go
type TemplateData struct {
	GatewayName   string // gw1
	OwnIP         string // 10.20.0.11
	ConfigVersion string // v000002
	Slots         []SlotData
	MapDir        string // /etc/haproxy/maps (live) or /var/lib/cstam/maps/cand (candidate)
}

type SlotData struct {
	N       int    // 1..128
	Addr    string // 192.0.2.1 when unused
	Port    int
	Enabled bool
}

func RenderConfig(tmpl string, d TemplateData) ([]byte, error) {
	t, err := template.New("haproxy").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, d); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
```

The slot part of the template:

```text
{{- range .Slots }}
backend be_slot{{ printf "%03d" .N }}
    http-request return status 503 content-type text/html file /etc/haproxy/errors/maintenance.html hdr Retry-After 10 if { nbsrv() lt 1 }
    server s {{ .Addr }}:{{ .Port }}{{ if not .Enabled }} disabled{{ end }} check inter 2s fastinter 500ms downinter 2s fall 2 rise 1
{{- end }}
```

Rendering the slot addresses into the file means that after any reload, the slots already point at the right teams: memory and files agree.

#### 4.12 The applier

The applier is the heart of the agent. It is a single goroutine (one apply at a time), so there is no concurrent mutation of HAProxy from inside the agent.

```go
// internal/gw/applier/applier.go
package applier

type Outcome string

const (
	Applied    Outcome = "applied"
	Rejected   Outcome = "rejected"
	RolledBack Outcome = "rolled_back"
	Skipped    Outcome = "skipped" // older or equal generation
)

type Desired struct {
	Generation        int64
	Digest            []byte
	Routes            []edge.Route
	StructuralVersion int32
	StructuralBody    string // empty if unchanged
	ForceBackup       bool
}

type Result struct {
	Generation                                 int64
	Digest                                     []byte
	Outcome                                    Outcome
	ValidateMS, ApplyMS, VerifyMS, RollbackMS  int64
	Detail                                     string
}

type Applier struct {
	RT      haproxy.Runtime
	Master  haproxy.Master
	State   *StateStore // state.json + last-good files
	Prober  Prober      // HTTP checks against 127.0.0.1
	Cfg     Config
	Log     *slog.Logger
	Clock   func() time.Time
}

func (a *Applier) Apply(ctx context.Context, d Desired) Result {
	cur := a.State.Current()
	if d.Generation <= cur.Generation && d.StructuralVersion == cur.StructuralVersion {
		return Result{Generation: cur.Generation, Digest: cur.Digest, Outcome: Skipped}
	}
	if !bytes.Equal(edge.Digest(d.Routes), d.Digest) {
		// the snapshot is not self-consistent: never apply it
		return Result{Generation: d.Generation, Outcome: Rejected, Detail: "snapshot digest mismatch"}
	}
	if err := ValidateRoutes(d.Routes, a.Cfg.Topology); err != nil { // gate 1 for routes
		return Result{Generation: d.Generation, Outcome: Rejected, Detail: err.Error()}
	}
	if d.StructuralVersion != cur.StructuralVersion {
		return a.applyStructural(ctx, cur, d)
	}
	return a.applyRoutes(ctx, cur, d)
}
```

Route path:

```go
func (a *Applier) applyRoutes(ctx context.Context, cur State, d Desired) Result {
	res := Result{Generation: d.Generation, Digest: d.Digest}
	t0 := a.Clock()

	add, del := diffRoutes(cur.Routes, d.Routes) // by slot and fqdn
	// 1. files first (map + slot table in state.json), atomic rename
	if err := a.State.WriteMapFile(d.Generation, d.Routes); err != nil {
		res.Outcome, res.Detail = Rejected, err.Error()
		return res
	}
	// 2. memory: new slots ready BEFORE the map points at them; removed hosts unrouted BEFORE the slot is parked
	err := a.setSlots(ctx, add, true)
	if err == nil {
		err = a.RT.ReplaceMap(ctx, a.Cfg.LiveMap(), mapLines(d.Routes))
	}
	if err == nil {
		err = a.setSlots(ctx, del, false) // maint + shutdown sessions + park at 192.0.2.1
	}
	if err == nil {
		err = a.RT.SetMeta(ctx, d.Generation) // meta.map "generation N" -> X-Routes-Generation
	}
	res.ApplyMS = ms(a.Clock().Sub(t0))

	// 3. verify
	t1 := a.Clock()
	if err == nil {
		err = a.verify(ctx, cur.ConfigVersion, d)
	}
	res.VerifyMS = ms(a.Clock().Sub(t1))
	if err != nil {
		t2 := a.Clock()
		rbErr := a.restoreRoutes(ctx, cur) // previous map version + previous slot addresses
		res.RollbackMS = ms(a.Clock().Sub(t2))
		res.Outcome, res.Detail = RolledBack, fmt.Sprintf("verify: %v; rollback: %v", err, rbErr)
		return res
	}
	for _, r := range del {
		_ = flushNeighbour(ctx, r.IP) // ip neigh del <ip> dev <iface>: no stale ARP entry towards the old owner
	}
	a.State.Commit(d.Generation, d.Digest, d.Routes, cur.StructuralVersion)
	res.Outcome = Applied
	return res
}
```

Structural path:

```go
func (a *Applier) applyStructural(ctx context.Context, cur State, d Desired) Result {
	res := Result{Generation: d.Generation, Digest: d.Digest}
	ver := fmt.Sprintf("v%06d", d.StructuralVersion)

	// render candidate config + candidate maps into /var/lib/cstam
	cand, err := a.renderCandidate(d, ver)
	if err != nil {
		res.Outcome, res.Detail = Rejected, err.Error()
		return res
	}
	// gate 1
	t0 := a.Clock()
	err = haproxy.Validate(ctx, cand.ConfigPath)
	res.ValidateMS = ms(a.Clock().Sub(t0))
	if err != nil {
		res.Outcome, res.Detail = Rejected, err.Error() // live config untouched
		return res
	}
	// swap: keep last-good, atomic rename of config and maps
	t1 := a.Clock()
	if err := a.State.SaveLastGood(); err != nil {
		res.Outcome, res.Detail = Rejected, err.Error()
		return res
	}
	if err := a.State.Swap(cand); err != nil {
		res.Outcome, res.Detail = Rejected, err.Error()
		return res
	}
	ok, startup, err := a.Master.Reload(ctx)
	res.ApplyMS = ms(a.Clock().Sub(t1))

	// gate 2
	t2 := a.Clock()
	if err == nil && !ok {
		err = fmt.Errorf("reload Success=0: %s", firstLines(startup, 5))
	}
	if err == nil {
		err = a.verify(ctx, ver, d)
	}
	res.VerifyMS = ms(a.Clock().Sub(t2))
	if err != nil {
		t3 := a.Clock()
		rbErr := a.rollbackStructural(ctx, cur) // restore last-good files, reload, verify old version
		res.RollbackMS = ms(a.Clock().Sub(t3))
		res.Outcome, res.Detail = RolledBack, fmt.Sprintf("%v; rollback: %v", err, rbErr)
		return res
	}
	a.State.Commit(d.Generation, d.Digest, d.Routes, d.StructuralVersion)
	res.Outcome = Applied
	return res
}
```

Verification:

```go
func (a *Applier) verify(ctx context.Context, wantVersion string, d Desired) error {
	ctx, cancel := context.WithTimeout(ctx, a.Cfg.VerifyBudget) // 600 ms total
	defer cancel()
	return retry(ctx, 3, 200*time.Millisecond, func(ctx context.Context) error {
		// 1. the running config is the expected one, with the expected routes generation
		h, err := a.Prober.Healthz(ctx) // GET http://127.0.0.1/__gw/healthz
		if err != nil {
			return err
		}
		if h.ConfigVersion != wantVersion || h.RoutesGeneration != d.Generation {
			return fmt.Errorf("live version %s gen %d, want %s gen %d", h.ConfigVersion, h.RoutesGeneration, wantVersion, d.Generation)
		}
		// 2. the full path frontend -> map -> slot -> server works
		if body, err := a.Prober.Get(ctx, "canary.gw.internal", "/"); err != nil || body != "canary\n" {
			return fmt.Errorf("canary: %q %v", body, err)
		}
		// 3. what HAProxy holds hashes to the expected digest
		live, err := a.liveRoutes(ctx) // show map + show servers state, converted back to edge.Route
		if err != nil {
			return err
		}
		if !bytes.Equal(edge.Digest(live), d.Digest) {
			return errors.New("live digest differs from desired digest")
		}
		return nil
	})
}
```

::: why
**Why verify the digest from HAProxy's memory instead of from our own file?**
The file is what we *wrote*; memory is what *serves*. The acknowledgement must describe what serves, because the control plane uses it to decide that an IP can safely go to another team. Reading back from `show map` and `show servers state` is a few milliseconds and closes the gap between "we tried" and "it is live".
:::

The probe for team health (used by the control plane in Step 7) goes through the same path: the control plane asks the master gateway agent, in its desired state stream, to check `Host: team1.hacka.cstam.felcloud.tn` and path `/.cstam/health`. The agent answers in its next `Report`. Keeping all probes local to the gateway means the check uses exactly the route the public traffic uses.

#### 4.13 The sync loop: watch, apply, acknowledge, report

```go
// internal/gw/sync/client.go
func (s *Syncer) Run(ctx context.Context) error {
	// boot rule: not VRRP-eligible until synced (or until the timeout below)
	_ = os.Remove(filepath.Join(s.RunDir, "ready"))
	bootDeadline := time.AfterFunc(s.BootSyncTimeout, func() {
		s.Log.Warn("control plane unreachable, serving last-good state", "gen", s.Applier.State.Current().Generation)
		s.markReady(false) // ready file created, synced=false reported
	})
	defer bootDeadline.Stop()

	go s.reportLoop(ctx) // every 2 s: vrrp state, haproxy_ok, applied generation, synced
	backoff := newBackoff(200*time.Millisecond, 10*time.Second)

	for ctx.Err() == nil {
		after := s.Applier.State.Current().Generation
		stream, err := s.Client.WatchGateway(ctx, connect.NewRequest(&agentv1.WatchRequest{Agent: s.Name, After: after}))
		if err != nil {
			sleep(ctx, backoff.Next())
			continue
		}
		for stream.Receive() {
			backoff.Reset()
			msg := stream.Msg()
			if msg.Generation == 0 { // keepalive frame
				continue
			}
			d := fromProto(msg)
			s.Keepalived.SetForceBackup(d.ForceBackup) // writes 0/1 to /run/cstam/force-backup (only on change)
			res := s.Applier.Apply(ctx, d)
			if res.Outcome == applier.Skipped {
				continue
			}
			s.ack(ctx, res) // Ack with generation, digest, outcome, timings
			if res.Outcome == applier.Applied {
				bootDeadline.Stop()
				s.markReady(true)
			}
		}
		s.Log.Warn("watch stream ended", "err", stream.Err())
		sleep(ctx, backoff.Next())
	}
	return ctx.Err()
}
```

Three details:

- **Rejected and rolled-back outcomes are acknowledged too**, with the outcome and the error text. The control plane records them in `config_versions` (the dashboard shows "rejected: unknown keyword 'sslx'"), but it only counts `applied` acks for IP recycling.
- **The ack after a rollback carries the old generation and digest.** The gateway is honest about what serves.
- **`markReady(true)` happens once.** After that, the ready file stays, even if a later apply is rolled back: a gateway that rolled back is still serving a verified state.

The agent also re-reads its full state from HAProxy every 60 s and compares the digest with `state.json`. If someone edited HAProxy by hand, the digest differs: the agent logs it, reports `drift=true`, and re-applies its last snapshot. Nobody should edit a gateway by hand after this step.

#### 4.14 VRRP state and the manual-failover flag

```go
// internal/gw/keepalived/vrrp.go
type Keepalived struct{ RunDir string }

// State reads "MASTER 1760627542.123 150" written by the notify script.
func (k Keepalived) State() (state string, since time.Time, err error)

// SetForceBackup writes the track_file atomically; keepalived reacts within one advert interval.
func (k Keepalived) SetForceBackup(on bool) error {
	v := "0\n"
	if on {
		v = "1\n"
	}
	return atomicWrite(filepath.Join(k.RunDir, "force-backup"), []byte(v), 0o664)
}
```

The agent watches `/run/cstam/vrrp.state` with `fsnotify` (`github.com/fsnotify/fsnotify`) and sends a `Report` immediately on each change, with the timestamp from the notify script. That is how the dashboard can show "failover at 14:02:11.384, 1.4 s".

#### 4.15 Traffic logs

HAProxy sends one RFC 5424 syslog datagram per request to `127.0.0.1:5514`; the message part is the JSON line defined by `log-format` in Step 3.

```go
// internal/gw/logs/listener.go
func (l *Listener) Run(ctx context.Context, out chan<- []byte) error {
	pc, err := net.ListenPacket("udp", l.Addr) // 127.0.0.1:5514, loopback only
	if err != nil {
		return err
	}
	defer pc.Close()
	buf := make([]byte, 9000)
	for ctx.Err() == nil {
		_ = pc.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			continue
		}
		i := bytes.IndexByte(buf[:n], '{') // skip the RFC 5424 header, keep the JSON payload
		if i < 0 {
			continue
		}
		line := append([]byte(nil), buf[i:n]...)
		select {
		case out <- line:
		default:
			l.Dropped.Add(1) // never block HAProxy's logging path
		}
	}
	return ctx.Err()
}
```

A batcher sends at most 200 lines or every 250 ms on the `PushTrafficLogs` client stream. If the control plane is unreachable, the batch is dropped and counted (`cstam_logs_dropped_total`). Logs must never slow down routing.

#### 4.16 Canary responder and main

```go
// internal/gw/canary/canary.go
func Serve(ctx context.Context) error {
	srv := &http.Server{Addr: "127.0.0.1:9102", ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "canary\n") })}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
```

```go
// cmd/gwagent/main.go (core)
func run(ctx context.Context, cfg Config) error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("agent", cfg.Name)
	tlsCfg, err := pki.ClientTLS(cfg.TLSCert, cfg.TLSKey, cfg.TLSCA) // Step 9: server cert must be the cp's
	if err != nil {
		return err
	}
	httpc := &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsCfg}} // golang.org/x/net/http2
	client := agentv1connect.NewControlServiceClient(httpc, cfg.CPURL)

	st, err := applier.OpenState(cfg.StateDir) // loads state.json (or empty state on first boot)
	if err != nil {
		return err
	}
	ap := &applier.Applier{RT: haproxy.Runtime{Socket: cfg.AdminSock}, Master: haproxy.Master{Socket: cfg.MasterSock},
		State: st, Prober: applier.LocalProber{}, Cfg: applier.ConfigFrom(cfg), Log: log, Clock: time.Now}

	logsCh := make(chan []byte, 10000)
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return canary.Serve(ctx) })
	g.Go(func() error { return (&logs.Listener{Addr: cfg.LogListen}).Run(ctx, logsCh) })
	g.Go(func() error { return sync.PushLogs(ctx, client, cfg.Name, logsCh) })
	g.Go(func() error { return (&sync.Syncer{Name: cfg.Name, Client: client, Applier: ap, RunDir: cfg.RunDir,
		BootSyncTimeout: cfg.SyncTimeout, Keepalived: keepalived.Keepalived{RunDir: cfg.RunDir}, Log: log}).Run(ctx) })
	_, _ = daemon.SdNotify(false, daemon.SdNotifyReady) // coreos/go-systemd; ready = process up (VRRP readiness is the file)
	go watchdog(ctx)                                      // WATCHDOG=1 every 10 s
	return g.Wait()
}
```

#### 4.17 Tests

Testing in four layers, cheapest first.

**1. Pure unit tests** (no HAProxy):

| Test | Asserts |
|---|---|
| `TestCanonicalDigest` | order and case do not change the digest; one changed port does |
| `TestApplySkipsOlderGeneration` | apply 10, then 7 → `Skipped`, state still 10 |
| `TestValidateRoutes` | IP outside `.32–.191` rejected; port 22 rejected; two hosts on one slot rejected; `team1;evil` rejected |
| `TestDiffOrder` | the fake runtime records: `set server ... ready` before `commit map` for adds; `commit map` before `state maint` for removals |
| `TestRollbackOnVerifyFailure` | fake prober fails the canary → previous map restored, `RolledBack`, ack carries the old generation |
| `TestRenderGolden` | `RenderConfig` with 128 slots equals `testdata/haproxy.gw1.golden` (`go test -update` to refresh) |

**2. Integration with a real HAProxy** (testcontainers, image `haproxy:3.2`): start a container with the rendered config and the sockets in a shared volume, then:

```go
func TestRealHAProxyPipeline(t *testing.T) {
	h := testutil.StartHAProxy(t, "haproxy:3.2", testdata("haproxy.gw1.golden")) // exposes admin + master sockets via a volume
	ap := newApplierFor(t, h)

	// valid route change: applied, live digest equals desired
	d1 := desired(1, route("team1.hacka.cstam.felcloud.tn", 1, h.BackendIP(), 80))
	require.Equal(t, applier.Applied, ap.Apply(ctx, d1).Outcome)

	// broken structural change: rejected, live config version unchanged
	bad := withStructural(d1, 2, brokenTemplate)
	require.Equal(t, applier.Rejected, ap.Apply(ctx, bad).Outcome)
	require.Equal(t, "v000001", h.Healthz(t).ConfigVersion)

	// valid-but-wrong structural change: rolled back in < 1 s
	wrong := withStructural(d1, 3, templateWithoutRouting)
	res := ap.Apply(ctx, wrong)
	require.Equal(t, applier.RolledBack, res.Outcome)
	require.Less(t, res.RollbackMS, int64(1000))
	require.Equal(t, "v000001", h.Healthz(t).ConfigVersion)
}
```

**3. Contract test** against the fake control plane (`internal/agentapi/fake`): it sends snapshots 1, 2, 3, then an old snapshot 2 again, then closes the stream; the test checks the acks (1, 2, 3, nothing for the second 2) and that the agent reconnects with `after=3`.

**4. On the real gateways**: the zero-failed-requests proof below.

#### 4.18 Proof: zero failed requests during reloads

Run from the control-plane VM (so your home connection is not the bottleneck), with `bench/loadgen` (vegeta as a library, constant rate, one new connection per request in the first run):

```bash
go run ./bench/loadgen -url http://10.20.0.10/ -host team1.hacka.cstam.felcloud.tn \
   -rps 500 -duration 120s -keepalive=false -out bench/results/step4/reload-nokeepalive.csv &
go run ./bench/reloader -cp https://10.20.0.5 -route-changes 30 -structural 20 -bad 1 -bad-valid 1 \
   -out bench/results/step4/actions.csv
wait
go run ./bench/report summarize bench/results/step4/reload-nokeepalive.csv
# expected: requests 60000, non-2xx 0, transport errors 0
```

Repeat with `-keepalive=true`. Cross-check HAProxy's own counters before and after (`echo "show stat" | socat stdio /run/haproxy/admin.sock`, columns `ereq`, `econ`, `eresp`, `hrsp_5xx` on `fe_http`): the deltas must be 0.

::: note
**Who triggers the reloads in this test?** `bench/reloader` changes the desired state in the control plane (new structural versions, route changes), exactly like a real operator, and the agents apply them. Before Step 7 exists, use the fake control plane's `-push` mode: it serves snapshots from files.
:::

### Done when

::: done
**Done when**

- `go test -race ./internal/gw/... ./internal/edge/...` passes, including the testcontainers HAProxy test.
- On both gateways, `systemctl status gwagent` is active; `journalctl -u gwagent` shows `applied gen=N` for each change, and gw1 and gw2 always report the same generation and digest:
  `curl -s localhost/__gw/healthz` on each gateway shows the same `X-Routes-Generation`.
- A route change through the (fake) control plane is live on both gateways in under 100 ms, with **no reload** (`echo "show proc" | sudo socat /run/haproxy-master.sock stdio` shows the same reload counter before and after).
- A broken structural change is acknowledged as `rejected`; `md5sum /etc/haproxy/haproxy.cfg` is unchanged.
- A valid-but-wrong structural change is acknowledged as `rolled_back` with `rollback_ms < 1000`; the live `X-Config-Version` is the old one.
- After `sudo systemctl restart gwagent` on the master, `/run/cstam/ready` disappears and comes back within 1 s; with the control plane stopped, it comes back after 10 s and the agent reports `synced=false`.
- Writing `force_backup=true` for gw1 in the fake desired state moves the VIP to gw2 in under 1 s.
- The reload proof shows 0 failed requests out of 60 000, with and without keep-alive. CSV files and summary in `bench/results/step4/`.
:::

### Pitfalls

::: warn
**Applying from two goroutines.** If the watch loop and the 60-second drift check both call HAProxy at the same time, a rollback can restore a map that the other goroutine just replaced. Exactly one goroutine owns the applier; everything else sends it requests through a channel.
:::

::: warn
**Validating the live maps instead of the candidate maps.** `haproxy -c` reads the map files the config names. The candidate config must name the candidate map files; the swap renames both together.
:::

::: warn
**Forgetting `shutdown sessions` on removal.** A WebSocket opened to a team stays open after its route is removed (`timeout tunnel 1h`). If the IP is later given to another team, that old session still points at the new owner. The agent cuts the sessions before acknowledging the removal.
:::

::: warn
**Treating `Success=0` as "HAProxy is down".** It is not: the old workers are still serving. The right reaction is to roll back the files and reload again, not to restart HAProxy (a restart would drop connections).
:::

::: warn
**Clock-based ordering.** Never compare timestamps to decide which snapshot is newer. Only the generation number orders snapshots.
:::

### Learning checkpoint

::: learn
**Answer these yourself in docs/notebook/learning.md**

1. Draw the structural pipeline from memory and name the two safety gates. What exactly does each gate check?
2. Why are files written before the runtime API for an add, and why is the map entry removed before the slot is parked for a removal?
3. What is in the digest, who computes it, and why is it read back from HAProxy's memory rather than from the file?
4. A gateway reboots while the control plane is down. What does it serve, when does it become VRRP-eligible, and why is that safe for IP recycling?
5. What does `Success=0` from the master CLI mean for traffic, and what does the agent do next?
:::

### Driving Claude Code for this step

::: claude
**Prompts to use**

- Start with the contract, not the code: *"Read chapter 13 sections 4.6–4.13 of my guide and internal/edge. List the Go types and functions you will create for internal/gw/applier, with one sentence each. No code yet."*
- Then one package at a time: *"Implement internal/gw/haproxy/runtime.go and master.go. Every command must check HAProxy's text answer. Write the testcontainers test with haproxy:3.2 that proves set server, prepare/commit map and reload work, and print the exact answers you observed."*
- For the applier: *"Write the tests of section 4.17 layer 1 first, with a fake Runtime that records the order of calls. Then implement Applier until they pass. Show me the call log for one add and one removal."*
- Review as the jury: *"Find a sequence of events in which this agent acknowledges a generation that HAProxy is not actually serving."*

**What to review in the diff:** only one goroutine calls the runtime; every `exec` uses argv; the ack after rollback carries the old generation; nothing in `internal/gw` imports gophercloud (depguard); no IP literal outside tests and topology.
:::
