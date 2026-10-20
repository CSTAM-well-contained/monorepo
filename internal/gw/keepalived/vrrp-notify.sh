#!/bin/sh
# vrrp-notify: keepalived calls it with INSTANCE VI_GW STATE PRIORITY on every state change.
# The gateway agent watches the state file and reports transitions to the control plane.
# keepalived runs notify scripts with umask 077; the unprivileged agent must read the state.
umask 022
RUN="${CSTAM_RUN:-/run/cstam}"
STATE="$3"; TS="$(date +%s.%N)"
printf '%s %s %s\n' "$STATE" "$TS" "$4" > "$RUN/vrrp.state.tmp" && mv "$RUN/vrrp.state.tmp" "$RUN/vrrp.state"
printf '%s %s %s\n' "$STATE" "$TS" "$4" >> "$RUN/vrrp.log"
logger -t vrrp-notify "state=$STATE prio=$4 ts=$TS" 2>/dev/null
exit 0
