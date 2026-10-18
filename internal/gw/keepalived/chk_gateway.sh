#!/bin/sh
# chk_gateway: ready = the gateway agent synced its routes since boot, AND HAProxy answers.
[ -f "${CSTAM_RUN:-/run/cstam}/ready" ] || exit 1
exec curl -fsS -o /dev/null --max-time 0.8 "http://127.0.0.1:${HEALTH_PORT:-80}/__gw/healthz"
