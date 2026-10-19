#!/usr/bin/env bash
# demo.sh: the whole platform on one machine, with the FelCloud IP plan (infra/topology.env).
# Postgres and cpd (docker sandbox driver) run on the host side; gw1 and gw2 are containers
# running HAProxy + keepalived + gwagent and share the VIP; each team is a container at its
# IPAM address. Needs Docker, curl, jq and `make build`.
#
#   demo.sh up            start everything
#   demo.sh teams [N]     create hackathon $HACK (default demo) with N teams (default 3), curl each via the VIP
#   demo.sh failover [pause|kill]
#                         break the MASTER gateway while probing the VIP every 20 ms
#   demo.sh reload        broken HAProxy config -> rollback (applier integration tests)
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
CPD="http://$ROUTER:8080"
HACK=${HACK:-demo}              # hackathon name; an ended one keeps its name
DOMAIN=$HACK.cstam.test
GW_IMAGE=cstam-lab-gw:test      # same image as the keepalived integration test
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
  [ -s "$STATE/gw.env" ] || echo "CSTAM_GATEWAY_TOKEN=$(openssl rand -hex 16)" >"$STATE/gw.env"
  chmod 600 "$STATE"/*

  echo "== postgres"
  docker inspect "$PG" >/dev/null 2>&1 ||
    docker run -d --name "$PG" -e POSTGRES_PASSWORD=demo -p 127.0.0.1:55433:5432 postgres:18-alpine >/dev/null
  for _ in $(seq 30); do docker exec "$PG" pg_isready -q -U postgres && break; sleep 1; done
  sleep 1

  echo "== cpd on $CPD (docker sandbox driver)"
  if ! curl -sf "$CPD/healthz" >/dev/null; then
    ( set -a; . "$STATE/gw.env"; set +a
      CSTAM_DB_DSN=postgres://postgres:demo@127.0.0.1:55433/postgres CSTAM_DRIVER=docker \
      CSTAM_DOCKER_NETWORK=$DNET CSTAM_ADMIN_TOKEN=$(cat "$STATE/admin.token") \
      exec setsid ./bin/cpd -listen "$ROUTER:8080" ) >"$STATE/cpd.log" 2>&1 &
    echo $! >"$STATE/cpd.pid"
    for _ in $(seq 40); do curl -sf "$CPD/healthz" >/dev/null && break; sleep 0.25; done
    curl -sf "$CPD/healthz" >/dev/null || { tail -5 "$STATE/cpd.log"; die "cpd did not start"; }
  fi

  for gw in gw1 gw2; do
    ip=GW1_IP; [ "$gw" = gw2 ] && ip=GW2_IP
    docker inspect "cstam-demo-$gw" >/dev/null 2>&1 && continue
    echo "== $gw on ${!ip}"
    docker run -d --name "cstam-demo-$gw" --hostname "$gw" --network "$DNET" --ip "${!ip}" \
      --cap-add NET_ADMIN --cap-add NET_RAW --cap-add NET_BROADCAST \
      --env-file "$STATE/gw.env" -e "GW_NAME=$gw" -e "CPD_URL=$CPD" \
      -v "$PWD/bin:/opt/cstam/bin:ro" -v "$PWD/deploy/dev/gw-entrypoint.sh:/entrypoint.sh:ro" \
      --entrypoint /bin/sh "$GW_IMAGE" /entrypoint.sh >/dev/null
  done
  echo "== waiting for the VIP $VIP_IP"
  for _ in $(seq 60); do [ -n "$(master)" ] && break; sleep 0.5; done
  [ -n "$(master)" ] || die "nobody answers on the VIP (docker logs cstam-demo-gw1)"
  echo "VIP served by $(master). API: $CPD, admin token in $STATE/admin.token"
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
    *) die "failover pause|kill" ;;
  esac
  wait "$probe"
  if [ "$how" = pause ]; then
    docker unpause "cstam-demo-$m" >/dev/null
  else # HAProxy is not supervised in the container: restart the gateway
    docker restart "cstam-demo-$m" >/dev/null
  fi
  awk -v t="$t" -v old="$m" '
    $1 >= t && $2 == 200 && $3 != old && !back { back = $1; by = $3 }
    $1 >= t && $2 != 200 { lost++ }
    $1 >= t { total++ }
    END {
      if (!back) { print "no other gateway took over"; exit 1 }
      printf "%s took over %.0f ms after the failure; %d of %d probes failed\n", by, (back - t) * 1000, lost, total
    }' "$STATE/probe" || true
  sleep 3
  # A frozen master wakes up still MASTER with the higher priority and takes the VIP back;
  # a restarted one comes back as BACKUP (nopreempt).
  echo "$m recovered; VIP now served by $(master)"
}

reload() {
  echo "== hot reload, broken config rejected and rolled back (real HAProxy 3.2 in Docker)"
  go test -count=1 -tags integration -v -run 'TestBrokenConfigRollsBack|TestReloadDropsNoRequest' ./internal/gw/applier/ |
    grep -E '^(=== RUN|--- |\s+\S+_test.go|ok|FAIL)'
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
  up|teams|failover|reload|teardown|status|down) "$@" ;;
  *) sed -n '2,15p' "$0"; exit 2 ;;
esac
