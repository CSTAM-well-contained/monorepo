#!/usr/bin/env bash
# video.sh: plays the Phase 2 demo for the 3-minute video, step by step, with captions.
# Storyboard and narration: docs/official/video-script.md.
#
#   QUARANTINE=10s GW_MODE=systemd deploy/dev/demo.sh up    # once, before recording
#   (Windows) open http://localhost:8443/ and log in with the admin token
#   deploy/dev/video.sh          # Enter = next step;  VIDEO_AUTO=1: no pauses
#
# Needs the demo stack (demo.sh up) and jq. The FelCloud isolation step shows the results measured
# with lab VMs (deploy/lab/), since a VM boot takes minutes.
set -euo pipefail
cd "$(dirname "$0")/../.."
# shellcheck source=../../infra/topology.env
source infra/topology.env

ROUTER="${VIP_IP%.*}.1"
CPD="http://$ROUTER:8443"
STATE="${XDG_RUNTIME_DIR:-/tmp}"; STATE="${STATE%/}/cstam-demo"
HACK=${HACK:-p2-$(date +%H%M)}   # a fresh hackathon name for each take
export HACK
DOMAIN=$HACK.cstam.test

[ -s "$STATE/admin.token" ] || { echo "video: run deploy/dev/demo.sh up first" >&2; exit 1; }
api() { # api METHOD PATH [JSON]
  curl -sS -X "$1" -H "Authorization: Bearer $(cat "$STATE/admin.token")" \
    -H "Content-Type: application/json" ${3:+-d "$3"} "$CPD$2"
}
banner() { printf '\n\033[1;97;44m  %s  \033[0m\n' "$*"; }
say() { printf '\033[1;36m> %s\033[0m\n' "$*"; }
# next waits before the next step: Enter, a fixed pause (VIDEO_AUTO=1), or, for an automatic
# recording (deploy/video/), a gate: the step number is written to $VIDEO_MARK when this step's
# work is done, and the script goes on when the recorder writes a number >= it to $VIDEO_GATE.
step_no=0
next() {
  step_no=$((step_no + 1))
  if [ -n "${VIDEO_GATE:-}" ]; then
    echo "$step_no" >"$VIDEO_MARK"
    while [ "$(cat "$VIDEO_GATE" 2>/dev/null || echo 0)" -lt "$step_no" ]; do sleep 0.2; done
  elif [ "${VIDEO_AUTO:-}" = 1 ]; then sleep "${1:-3}"
  else read -rsp $'\033[2m  [Enter]\033[0m' _; echo; fi
}
code() { curl -s -o /dev/null -m 2 -w '%{http_code}' -H "Host: $1" "http://$VIP_IP${2:-/}"; }
quarantine=$(tr '\0' ' ' </proc/"$(cat "$STATE/cpd.pid")"/cmdline 2>/dev/null | sed -n 's/.*-quarantine \([^ ]*\).*/\1/p')

# ---------------------------------------------------------------------------
banner "CSTAM Resilient IP Optimizer  -  Phase 2: stress, teardown, isolation"
api GET /v1/status | jq -r '"gateways: " + ([.gateways[] | "\(.name) \(.vrrp_state)"] | join(", "))
  + "   VIP " + .vip + "   teams ready: \(.teams.ready // 0)   free IPs: \(.ip_pool.free)"'
next

# ---------------------------------------------------------------------------
banner "1/4  Stress test: 20 teams register at the same time"
say "20 parallel API calls; each team is done when its own page answers through the VIP"
deploy/dev/demo.sh stress 20 | sed 's/^/  /'
next 6

# ---------------------------------------------------------------------------
banner "2/4  Isolation on the gateways: one team floods, the others do not notice"
hosts=$(api GET "/v1/hackathons/$HACK/teams" | jq -r '[.[] | select(.state=="ready") | .fqdn]')
flooded=$(jq -r '.[0]' <<<"$hosts")
others=$(jq -r '.[1:] | join(",")' <<<"$hosts")
n_others=$(jq '.[1:] | length' <<<"$hosts")
say "$flooded: as fast as possible; the $n_others other teams: 50 req/s each, for 8 s"
./bin/cstamctl load -url "http://$VIP_IP/" -hosts "$flooded" -c 16 -d 8s -rate 0 -check-body=false -json >"$STATE/flood.json" 2>/dev/null &
./bin/cstamctl load -url "http://$VIP_IP/" -hosts "$others" -c 32 -d 8s -rate $((50 * n_others)) -json >"$STATE/others.json" 2>/dev/null || true
wait || true
jq -r --arg h "$flooded" '"  flooded \($h): \(.requests) requests, \(.status["200"] // 0) served, \(.status["429"] // 0) refused (429 Too Many Requests)"' "$STATE/flood.json"
jq -r '"  other teams:  \(.requests) requests, \(.errors) errors, \(.misrouted) misrouted, p99 \(.p99_ns / 1e6 * 10 | round / 10) ms"' "$STATE/others.json"
say "management endpoint of the gateway, asked from outside: HTTP $(code "$flooded" /__gw/healthz) (hidden)"
next 5

banner "2/4  Isolation between sandboxes: Neutron security groups on FelCloud"
say "measured on FelCloud with lab VMs (docs/official/phase2.md, screenshots on screen):"
cat <<'EOF'
    from a team VM      -> other team :80 :8000 :22   DROPPED
                        -> gateway SSH, stats         DROPPED
                        -> control plane SSH, API, DB DROPPED
    from a gateway VM   -> team :80                   OPEN
EOF
next 6

# ---------------------------------------------------------------------------
banner "3/4  Automated teardown: a lease expires, nobody acts"
api POST /v1/hackathons "{\"name\":\"$HACK-lease\",\"domain\":\"$HACK-lease.cstam.test\",\"lease_ttl_seconds\":12}" >/dev/null
for t in x1 x2 x3; do api POST "/v1/hackathons/$HACK-lease/teams" "{\"name\":\"$t\"}" >/dev/null; done
sleep 2
say "3 teams with a 12 s lease: x1.$HACK-lease.cstam.test answers HTTP $(code "x1.$HACK-lease.cstam.test")"
start=$(date +%s)
while [ "$(api GET "/v1/hackathons/$HACK-lease/teams" | jq length)" != 0 ]; do
  printf '\r  waiting for the lease: %2d s ' $(( $(date +%s) - start )); sleep 1
done
printf '\r'
say "lease over after $(( $(date +%s) - start + 2 )) s: sandboxes deleted, routes removed -> x1 now answers HTTP $(code "x1.$HACK-lease.cstam.test")"
next 4

banner "3/4  Hackathon ends: sandboxes, routes and IPs purged, IPs recycled"
say "IPs wait in quarantine until BOTH gateways confirm the route is gone${quarantine:+ (quarantine $quarantine for this video, 60 s in production)}"
deploy/dev/demo.sh teardown | sed 's/^/  /'
say "routes left: $(api GET /v1/routes | jq --arg d "$DOMAIN" '[.routes[] | select(.host | endswith($d))] | length'), sandbox containers left: $(docker ps -q --filter label=cstam.sandbox | wc -l | tr -d ' ') (other hackathons kept)"
next 5

# ---------------------------------------------------------------------------
banner "4/4  Benchmark report (docs/official/benchmark.md)"
cat <<'EOF'
  provisioning   API answers 50 registrations in < 0.6 s; 50/50 routed, 0 errors
  failover       planned 0.30 s; frozen master 1.3-1.9 s; HAProxy crash 0.46-0.69 s (systemd)
  IP recycling   back in the pool 1 s after both gateways ack + quarantine (60 s in production)
  load           3,000 req/s through the VIP, 0 errors, p99 1.2-1.6 ms
  isolation      flooded team refused, others 0 errors; sandboxes isolated on FelCloud
EOF
banner "github.com/CSTAM-well-contained/monorepo  -  docs/official/"
[ -z "${VIDEO_GATE:-}" ] || echo $((step_no + 1)) >"$VIDEO_MARK"   # done (automatic recording)
