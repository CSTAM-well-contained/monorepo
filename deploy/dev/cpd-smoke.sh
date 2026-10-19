#!/usr/bin/env bash
# Smoke test of cpd: Postgres in Docker, fake driver, the API through curl.
set -u
cd "$(dirname "$0")/../.."
docker run -d --rm --name cstam-smoke-pg -e POSTGRES_PASSWORD=pw -p 127.0.0.1:55432:5432 postgres:18-alpine >/dev/null
trap 'kill $CPD 2>/dev/null; docker stop -t1 cstam-smoke-pg >/dev/null' EXIT
for i in $(seq 30); do docker exec cstam-smoke-pg pg_isready -q -U postgres && break; sleep 1; done
sleep 1
export CSTAM_DB_DSN=postgres://postgres:pw@127.0.0.1:55432/postgres CSTAM_DRIVER=fake \
  CSTAM_ADMIN_TOKEN="$(openssl rand -hex 16)" CSTAM_GATEWAY_TOKEN="$(openssl rand -hex 16)"
./bin/cpd -listen 127.0.0.1:18080 2>"${TMPDIR:-/tmp}/cpd-smoke.log" &
CPD=$!
for i in $(seq 20); do curl -sf 127.0.0.1:18080/healthz >/dev/null && break; sleep 0.5; done
A=(-s -H "Authorization: Bearer $CSTAM_ADMIN_TOKEN" -H "Content-Type: application/json")
G=(-s -H "Authorization: Bearer $CSTAM_GATEWAY_TOKEN" -H "Content-Type: application/json")
B=http://127.0.0.1:18080
echo "== create hackathon";  curl "${A[@]}" -X POST $B/v1/hackathons -d '{"name":"demo","domain":"demo.cstam.example","lease_ttl_seconds":600}'
echo "== create team";       curl "${A[@]}" -X POST $B/v1/hackathons/demo/teams -d '{"name":"alpha","app_port":8080}' -w ' [%{http_code}]\n'
curl "${A[@]}" -X POST $B/v1/hackathons/demo/teams -d '{"name":"beta"}' -o /dev/null
sleep 1
echo "== list teams";        curl "${A[@]}" $B/v1/hackathons/demo/teams
echo "== desired (gw1)";     curl "${G[@]}" "$B/v1/gateways/gw1/desired"
echo "== long-poll wakes on delete"; ( sleep 1; curl "${A[@]}" -X DELETE $B/v1/teams/2 -o /dev/null ) &
curl "${G[@]}" "$B/v1/gateways/gw1/desired?after=2" -w ' [%{time_total}s]\n'
echo "== ack";               curl "${G[@]}" -X POST $B/v1/gateways/gw1/ack -d '{"generation":3,"config_version":"v1","vrrp_state":"MASTER"}'
echo "== bad token";         curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer nope" $B/v1/status
sleep 2.5
echo "== status";            curl "${A[@]}" $B/v1/status
echo "== end hackathon";     curl "${A[@]}" -X DELETE $B/v1/hackathons/demo -w ' [%{http_code}]\n'
sleep 3
curl "${A[@]}" $B/v1/hackathons/demo; echo
echo "== cpd log (last lines)"; tail -4 "${TMPDIR:-/tmp}/cpd-smoke.log"
