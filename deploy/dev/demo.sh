#!/usr/bin/env bash
# demo.sh: the whole platform on one machine, with the FelCloud IP plan (infra/topology.env).
# Postgres and cpd (docker sandbox driver) run on the host side; gw1 and gw2 are containers
# running HAProxy + keepalived + gwagent and share the VIP; each team is a container at its
# IPAM address. Needs Docker, curl, jq and `make build`.
#
#   demo.sh up            start everything (CPD_WORKERS=n: sandbox operations in flight, default 4;
#                         GW_MODE=systemd: gateways run systemd with the units of deploy/systemd)
#   demo.sh teams [N]     create hackathon $HACK (default demo) with N teams (default 3), curl each via the VIP
#   demo.sh stress [N]    N registrations at once (default 20): latency p50/p95, distinct IPs
#   demo.sh failover [pause|kill|planned]
#                         break the MASTER gateway while probing the VIP every 20 ms
#                         (planned: POST /v1/gateways/<master>/failover, then failback)
#   demo.sh load [SECONDS] [WORKERS] [pause|kill]
#                         HTTP load through the VIP on every ready team (cstamctl load); with
#                         pause|kill the MASTER is broken 5 s in (RATE=n: total req/s, default
#                         150 per team, under the 200/s per-Host limit; KEEPALIVE=false;
#                         TIMEOUT=0.3s: per-request timeout, default 2s)
#   demo.sh reload        broken HAProxy config -> rollback (applier integration tests)
#   demo.sh config        the same through cpd on the running gateways: good, broken, typo,
#                         then manual rollback, while a team is probed through the VIP
#   demo.sh teardown      end the hackathon: team containers removed, IPs quarantined then freed
#   demo.sh status        cpd status and containers
#   demo.sh down          remove everything
set -euo pipefail
cd "$(dirname "$0")/../.."
# shellcheck source=../../infra/topology.env
source infra/topology.env

DNET=cstam-demo
ROUTER="${VIP_IP%.*}.1"         # the Docker bridge plays the router of the subnet
SUBNET="${VIP_IP%.*}.0/24"
CPD="http://$ROUTER:8443"
HACK=${HACK:-demo}              # hackathon name; an ended one keeps its name
DOMAIN=$HACK.cstam.test
GW_IMAGE=cstam-lab-gw:test      # same image as the keepalived integration test
GW_IMAGE_SYSTEMD=cstam-lab-gw-systemd:test
STATE="${XDG_RUNTIME_DIR:-/tmp}"; STATE="${STATE%/}/cstam-demo"
PG=cstam-demo-pg

die() { echo "demo: $*" >&2; exit 1; }
api() { # api METHOD PATH [JSON]
  curl -sS -X "$1" -H "Authorization: Bearer $(cat "$STATE/admin.token")" \
    -H "Content-Type: application/json" ${3:+-d "$3"} "$CPD$2"
}
now() { date +%s.%N; }
ms() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.0f", (b - a) * 1000 }'; }
# via HOST: GET / on the VIP with that Host header; prints "<code> <served-by> <body>"
via() {
  curl -s -m "${TMO:-1}" -o "$STATE/body" -D "$STATE/hdr" -w '%{http_code}' -H "Host: $1" "http://$VIP_IP/" >"$STATE/code" || true
  local by; by=$(tr -d '\r' <"$STATE/hdr" | awk -F': ' 'tolower($1)=="x-served-by" {print $2}')
  echo "$(cat "$STATE/code") ${by:--} $(tr -d '\n' <"$STATE/body")"
}
# master: the gateway whose keepalived says MASTER (healthz is internal-only)
master() {
  for gw in gw1 gw2; do
    docker exec "cstam-demo-$gw" cat /run/cstam/vrrp.state 2>/dev/null | grep -q '^MASTER ' && echo "$gw"
  done | head -1 || true
}

# systemd_gw NAME IP: a gateway booting systemd, set up by deploy/systemd/install-gateway.sh
# exactly as on a FelCloud VM (only the interface and the process tracking differ).
systemd_gw() {
  local gw=$1 ip=$2 ctr="cstam-demo-$1"
  docker run -d --name "$ctr" --hostname "$gw" --network "$DNET" --ip "$ip" \
    --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock \
    -v "$PWD/bin:/opt/cstam/bin:ro" -v "$PWD/deploy/systemd:/opt/cstam/systemd:ro" \
    "$GW_IMAGE_SYSTEMD" >/dev/null
  for _ in $(seq 40); do docker exec "$ctr" systemctl is-system-running 2>/dev/null | grep -qE 'running|degraded' && break; sleep 0.5; done
  { grep CSTAM_GATEWAY_TOKEN "$STATE/gw.env"
    echo "GWAGENT_OPTS=-name $gw -cpd $CPD -haproxy-user haproxy -vrrp-track-process=false"
    echo "VRRP_IFACE=eth0"; } | docker exec -i "$ctr" sh -c 'cat >/tmp/gwagent.env'
  docker exec "$ctr" sh /opt/cstam/systemd/install-gateway.sh /opt/cstam/bin /tmp/gwagent.env >/dev/null ||
    die "install-gateway.sh failed on $gw (docker exec $ctr journalctl -xe)"
  docker exec "$ctr" rm -f /tmp/gwagent.env
}

up() {
  [ -x bin/cpd ] && [ -x bin/gwagent ] || make build >/dev/null
  mkdir -p "$STATE" && chmod 700 "$STATE"
  if ! docker image inspect "$GW_IMAGE" >/dev/null 2>&1; then
    echo "== building the gateway image (HAProxy 3.2 + keepalived)"
    docker build -q -t "$GW_IMAGE" internal/gw/keepalived/testdata >/dev/null
  fi
  docker network inspect "$DNET" >/dev/null 2>&1 ||
    docker network create --subnet "$SUBNET" --gateway "$ROUTER" --ip-range "${VIP_IP%.*}.0/29" "$DNET" >/dev/null
  [ -s "$STATE/admin.token" ] || openssl rand -hex 16 >"$STATE/admin.token"
  [ -s "$STATE/viewer.token" ] || openssl rand -hex 16 >"$STATE/viewer.token"
  [ -s "$STATE/gw.env" ] || echo "CSTAM_GATEWAY_TOKEN=$(openssl rand -hex 16)" >"$STATE/gw.env"
  chmod 600 "$STATE/admin.token" "$STATE/viewer.token" "$STATE/gw.env"

  echo "== postgres"
  docker inspect "$PG" >/dev/null 2>&1 ||
    docker run -d --name "$PG" -e POSTGRES_PASSWORD=demo -p 127.0.0.1:55433:5432 postgres:18-alpine >/dev/null
  for _ in $(seq 30); do docker exec "$PG" pg_isready -q -U postgres && break; sleep 1; done
  sleep 1

  echo "== cpd on $CPD (docker sandbox driver)"
  if ! curl -sf "$CPD/healthz" >/dev/null; then
    ( set -a; . "$STATE/gw.env"; set +a
      CSTAM_DB_DSN=postgres://postgres:demo@127.0.0.1:55433/postgres CSTAM_DRIVER=docker \
      CSTAM_DOCKER_NETWORK=$DNET CSTAM_ADMIN_TOKEN=$(cat "$STATE/admin.token") CSTAM_VIEWER_TOKEN=$(cat "$STATE/viewer.token") \
      exec setsid ./bin/cpd -listen "$ROUTER:8443" -workers "${CPD_WORKERS:-4}" ) >"$STATE/cpd.log" 2>&1 &
    echo $! >"$STATE/cpd.pid"
    for _ in $(seq 40); do curl -sf "$CPD/healthz" >/dev/null && break; sleep 0.25; done
    curl -sf "$CPD/healthz" >/dev/null || { tail -5 "$STATE/cpd.log"; die "cpd did not start"; }
  fi

  echo "${GW_MODE:-plain}" >"$STATE/mode"
  if [ "${GW_MODE:-plain}" = systemd ] && ! docker image inspect "$GW_IMAGE_SYSTEMD" >/dev/null 2>&1; then
    echo "== building the systemd gateway image"
    docker build -q --build-arg "BASE=$GW_IMAGE" -t "$GW_IMAGE_SYSTEMD" deploy/systemd/test >/dev/null
  fi
  for gw in gw1 gw2; do
    ip=GW1_IP; [ "$gw" = gw2 ] && ip=GW2_IP
    docker inspect "cstam-demo-$gw" >/dev/null 2>&1 && continue
    echo "== $gw on ${!ip} (${GW_MODE:-plain})"
    if [ "${GW_MODE:-plain}" = systemd ]; then
      systemd_gw "$gw" "${!ip}"
      continue
    fi
    docker run -d --name "cstam-demo-$gw" --hostname "$gw" --network "$DNET" --ip "${!ip}" \
      --cap-add NET_ADMIN --cap-add NET_RAW --cap-add NET_BROADCAST \
      --env-file "$STATE/gw.env" -e "GW_NAME=$gw" -e "CPD_URL=$CPD" \
      -v "$PWD/bin:/opt/cstam/bin:ro" -v "$PWD/deploy/dev/gw-entrypoint.sh:/entrypoint.sh:ro" \
      --entrypoint /bin/sh "$GW_IMAGE" /entrypoint.sh >/dev/null
  done
  echo "== waiting for the VIP $VIP_IP"
  for _ in $(seq 60); do [ -n "$(master)" ] && break; sleep 0.5; done
  [ -n "$(master)" ] || die "nobody answers on the VIP (docker logs cstam-demo-gw1)"
  echo "VIP served by $(master). API and dashboard: $CPD/, tokens in $STATE/admin.token and viewer.token (read-only)"
}

teams() {
  local n=${1:-3}
  api POST /v1/hackathons "{\"name\":\"$HACK\",\"domain\":\"$DOMAIN\",\"lease_ttl_seconds\":3600}" >/dev/null
  declare -A t0
  for i in $(seq -f %02g "$n"); do
    t0[team$i]=$(now)
    local r; r=$(api POST "/v1/hackathons/$HACK/teams" "{\"name\":\"team$i\",\"app_port\":8080}")
    if jq -e .error <<<"$r" >/dev/null; then die "team$i: $(jq -r .error <<<"$r") (after a teardown: HACK=other $0 teams)"; fi
  done
  echo "== $n teams requested; time from the API call to the first answer through the VIP"
  for i in $(seq -f %02g "$n"); do
    local host="team$i.$DOMAIN" out=""
    for _ in $(seq 300); do out=$(via "$host"); [ "${out%% *}" = 200 ] && break; sleep 0.1; done
    printf '%-24s %6s ms  %s\n' "$host" "$(ms "${t0[team$i]}" "$(now)")" "$out"
  done
  echo "== teams in cpd"
  api GET "/v1/hackathons/$HACK/teams" | jq -r '.[] | "\(.name)\t\(.state)\t\(.ip)\tslot \(.slot)"'
}

# stress N: N team registrations sent at the same time; reports the time until each team
# answers through the VIP with its own page, and checks that IPs and slots are all distinct.
stress() {
  local n=${1:-20} dir="$STATE/stress"
  rm -rf "$dir" && mkdir -p "$dir"
  api POST /v1/hackathons "{\"name\":\"$HACK\",\"domain\":\"$DOMAIN\",\"lease_ttl_seconds\":3600}" >/dev/null
  local t0; t0=$(now)
  for i in $(seq -f %03g "$n"); do
    api POST "/v1/hackathons/$HACK/teams" "{\"name\":\"s$i\",\"app_port\":8080}" >"$dir/post-$i" &
  done
  wait
  echo "== $n registrations sent in parallel; all API answers in $(ms "$t0" "$(now)") ms"
  local rejected; rejected=$(cat "$dir"/post-* | jq -s '[.[] | select(.error)] | length')
  local pending=() i
  for i in $(seq -f %03g "$n"); do jq -e .error "$dir/post-$i" >/dev/null || pending+=("$i"); done
  while [ ${#pending[@]} -gt 0 ] && [ "$(ms "$t0" "$(now)")" -lt 180000 ]; do
    local left=()
    for i in "${pending[@]}"; do
      local body; body=$(curl -s -m 0.5 -H "Host: s$i.$DOMAIN" "http://$VIP_IP/" || true)
      if [[ $body == "team s$i of $HACK at "* ]]; then
        echo "$(ms "$t0" "$(now)") s$i ${body##* }" >>"$dir/routed"
      else
        left+=("$i")
      fi
    done
    pending=("${left[@]+"${left[@]}"}")
    sleep 0.1
  done
  sort -n "$dir/routed" -o "$dir/routed" 2>/dev/null || touch "$dir/routed"
  local teams; teams=$(api GET "/v1/hackathons/$HACK/teams")
  awk -v n="$n" -v rejected="$rejected" -v never="${#pending[@]}" '
    { t[NR] = $1 }
    END {
      if (NR == 0) { print "no team routed"; exit 1 }
      p50 = t[int((NR - 1) * 0.50) + 1]; p95 = t[int((NR - 1) * 0.95) + 1]
      printf "routed %d/%d (rejected by the API %d, never routed %d)\n", NR, n, rejected, never
      printf "API call to own page through the VIP: min %d ms, p50 %d ms, p95 %d ms, max %d ms\n", t[1], p50, p95, t[NR]
    }' "$dir/routed"
  echo "distinct IPs $(jq '[.[].ip] | unique | length' <<<"$teams"), distinct slots $(jq '[.[].slot] | unique | length' <<<"$teams"), states $(jq -c 'group_by(.state) | map({(.[0].state): length}) | add' <<<"$teams")"
}

failover() {
  local how=${1:-pause} m; m=$(master)
  [ -n "$m" ] || die "no master on the VIP"
  local other=gw1; [ "$m" = gw1 ] && other=gw2
  for _ in $(seq 60); do
    docker exec "cstam-demo-$other" cat /run/cstam/vrrp.state 2>/dev/null | grep -q '^BACKUP ' && break
    sleep 0.5
  done
  docker exec "cstam-demo-$other" cat /run/cstam/vrrp.state | grep -q '^BACKUP ' || die "$other is not BACKUP yet"
  local host; host=$(api GET "/v1/hackathons/$HACK/teams" | jq -r '[.[] | select(.state=="ready")][0].name // empty')
  host=${host:+$host.$DOMAIN}; host=${host:-$VIP_IP}
  echo "== $m is MASTER; probing http://$VIP_IP/ (Host $host) every 20 ms, then '$how' $m"
  : >"$STATE/probe"
  ( end=$(( $(date +%s) + 12 ))
    while [ "$(date +%s)" -lt "$end" ]; do echo "$(now) $(TMO=0.2 via "$host" | cut -d' ' -f1,2)" >>"$STATE/probe"; sleep 0.02; done ) &
  local probe=$!
  sleep 2
  local t; t=$(now)
  case $how in
    pause) docker pause "cstam-demo-$m" >/dev/null ;;
    kill)  docker exec "cstam-demo-$m" pkill -KILL haproxy ;;
    planned) api POST "/v1/gateways/$m/failover" | jq -c "{alerts}" ;;
    *) die "failover pause|kill|planned" ;;
  esac
  wait "$probe"
  if [ "$how" = planned ]; then
    api POST "/v1/gateways/$m/failback" >/dev/null
  elif [ "$how" = pause ]; then
    docker unpause "cstam-demo-$m" >/dev/null
  else
    # systemd restarts HAProxy by itself (Restart=always); the plain mode has no supervisor
    [ "$(cat "$STATE/mode" 2>/dev/null)" = systemd ] || docker restart "cstam-demo-$m" >/dev/null
  fi
  # Either the peer takes over, or (systemd) HAProxy is restarted in place before keepalived
  # reacts: then the same gateway answers again and the outage is the restart time.
  awk -v t="$t" -v old="$m" '
    $1 >= t && $2 == 200 && $3 != old && !back { back = $1; by = $3 }
    $1 >= t && $2 != 200 { lost++; if (!down) down = $1; up = 0 }
    $1 >= t && $2 == 200 && down && !up { up = $1 }
    $1 >= t { total++ }
    END {
      if (back) printf "%s took over %.0f ms after the failure; %d of %d probes failed\n", by, (back - t) * 1000, lost, total
      else if (up) printf "%s recovered in place (no failover): VIP down %.0f ms; %d of %d probes failed\n", old, (up - t) * 1000, lost, total
      else if (!lost) print "no outage seen"
      else { print "the VIP did not come back during the probe"; exit 1 }
    }' "$STATE/probe" || true
  sleep 3
  # A frozen master wakes up still MASTER with the higher priority and takes the VIP back;
  # a restarted one comes back as BACKUP (nopreempt).
  echo "$m recovered; VIP now served by $(master)"
}

load() {
  local secs=${1:-20} conns=${2:-32} how=${3:-} hosts m
  hosts=$(api GET "/v1/hackathons/$HACK/teams" | jq -r '[.[] | select(.state == "ready") | .fqdn] | join(",")')
  [ -n "$hosts" ] || die "no ready team in $HACK (run: demo.sh teams N)"
  m=$(master)
  [ -z "$how" ] || [ -n "$m" ] || die "no master on the VIP"
  echo "== load: $(tr , '\n' <<<"$hosts" | wc -l) teams, $conns workers, ${secs}s, keep-alive ${KEEPALIVE:-true}${how:+, $how $m at t=5s}"
  if [ -n "$how" ]; then
    ( sleep 5
      case $how in
        pause) docker pause "cstam-demo-$m" >/dev/null ;;
        kill)  docker exec "cstam-demo-$m" pkill -KILL haproxy ;;
      esac ) &
  fi
  local rate=${RATE:-$(( 150 * $(tr , '\n' <<<"$hosts" | wc -l) ))}
  ./bin/cstamctl load -url "http://$VIP_IP/" -hosts "$hosts" -c "$conns" -d "${secs}s" -rate "$rate" -timeout "${TIMEOUT:-2s}" \
    -keepalive="${KEEPALIVE:-true}" ${how:+-timeline} || true
  wait
  case $how in
    pause) docker unpause "cstam-demo-$m" >/dev/null ;;
    kill) [ "$(cat "$STATE/mode" 2>/dev/null)" = systemd ] || docker restart "cstam-demo-$m" >/dev/null ;;
  esac
}

reload() {
  echo "== hot reload, broken config rejected and rolled back (real HAProxy 3.2 in Docker)"
  go test -count=1 -tags integration -v -run 'TestBrokenConfigRollsBack|TestReloadDropsNoRequest' ./internal/gw/applier/ |
    grep -E '^(=== RUN|--- |\s+\S+_test.go|ok|FAIL)'
}

# config: the reload/rollback story through cpd on the running gateways: a good version, a
# valid-but-broken one (rolled back by each gateway), a syntax error (rejected), then a manual
# rollback, with a team probed through the VIP the whole time.
config() {
  local host; host=$(api GET "/v1/hackathons/$HACK/teams" | jq -r '[.[] | select(.state=="ready")][0].fqdn // empty')
  [ -n "$host" ] || die "no ready team in $HACK (run: demo.sh teams N)"
  : >"$STATE/probe"
  ( end=$(( $(date +%s) + 16 ))
    while [ "$(date +%s)" -lt "$end" ]; do TMO=0.5 via "$host" | cut -d' ' -f1 >>"$STATE/probe"; sleep 0.02; done ) &
  local probe=$! good="" step v
  for step in \
    'push a good version (rate limit 1000 per 10 s)|{"rate_limit":1000,"reason":"demo: good"}' \
    'push a valid but broken version (healthz answers 500)|{"rate_limit":1000,"extra":["http-request return status 500 if { path /__gw/healthz }"],"reason":"demo: broken"}' \
    'push a syntax error|{"rate_limit":1000,"extra":["this-is-not-a-directive"],"reason":"demo: typo"}'; do
    echo "== ${step%%|*}"
    v=$(api POST /v1/config "${step#*|}" | jq -r .version)
    good=${good:-$v}
    sleep 3
    api GET /v1/status | jq -r '.gateways[] | "   \(.name): tried \(.config_tried) -> \(.config_outcome), serves \(.config_version)  \(.config_error // "" | .[0:60])"'
  done
  echo "== manual rollback to $good"
  api POST "/v1/config/$good/rollback" | jq -r '"   new version \(.version): \(.reason)"'
  sleep 3
  api GET /v1/status | jq -r '.gateways[] | "   \(.name): serves \(.config_version) (\(.config_outcome))"'
  wait "$probe"
  echo "== $host meanwhile: $(grep -c '^200' "$STATE/probe") x 200, $(grep -vc '^200' "$STATE/probe") other"
}

teardown() {
  echo "== ending hackathon $HACK"
  api DELETE "/v1/hackathons/$HACK" >/dev/null
  # IPs stay in quarantine until every gateway acked the route removal and 60 s passed
  local t last="" line; t=$(now)
  for _ in $(seq 240); do
    local st; st=$(api GET /v1/status)
    line="teams $(jq -c .teams <<<"$st")  ip_pool $(jq -c .ip_pool <<<"$st")  containers $(docker ps -aq --filter label=cstam.sandbox | wc -l)"
    [ "$line" != "$last" ] && printf '%6s ms  %s\n' "$(ms "$t" "$(now)")" "$line" && last=$line
    [ "$(jq '.ip_pool.quarantine // 0' <<<"$st")" = 0 ] && [ "$(jq '[.teams[]] | add // 0' <<<"$st")" = "$(jq '.teams.deleted // 0' <<<"$st")" ] && break
    sleep 0.5
  done
}

status() {
  api GET /v1/status | jq .
  docker ps --filter name=cstam- --format 'table {{.Names}}\t{{.Status}}'
  echo "VIP served by: $(master)"
}

down() {
  [ -s "$STATE/cpd.pid" ] && kill "$(cat "$STATE/cpd.pid")" 2>/dev/null || true
  local ids; ids=$(docker ps -aq --filter label=cstam.sandbox)
  [ -n "$ids" ] && docker rm -f $ids >/dev/null
  docker rm -f cstam-demo-gw1 cstam-demo-gw2 "$PG" >/dev/null 2>&1 || true
  docker network rm "$DNET" >/dev/null 2>&1 || true
  rm -f "$STATE/cpd.pid"
  echo "demo removed"
}

case "${1:-}" in
  up|teams|stress|failover|load|reload|config|teardown|status|down) "$@" ;;
  *) sed -n "2,24p" "$0"; exit 2 ;;
esac
