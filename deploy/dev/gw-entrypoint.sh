#!/bin/sh
# Entrypoint of a demo gateway container (deploy/dev/demo.sh): the same three processes as
# on a FelCloud gateway. gwagent writes haproxy.cfg and keepalived.conf, then HAProxy and
# keepalived start, then gwagent syncs the routes from cpd. Needs GW_NAME, CPD_URL and
# CSTAM_GATEWAY_TOKEN in the environment and the binaries in /opt/cstam/bin.
set -eu
B=/opt/cstam/bin
rm -f /run/haproxy/*.sock # left by the previous run after docker restart
install -d /run/haproxy /run/cstam /var/lib/cstam /etc/haproxy /etc/keepalived
id keepalived_script >/dev/null 2>&1 || useradd -r -s /usr/sbin/nologin keepalived_script

# vrrp_track_process needs the host PID namespace: off in a container (see HANDOFF.md 6)
"$B/gwagent" -bootstrap -name "$GW_NAME" -cpd "$CPD_URL" -vrrp-iface eth0 -vrrp-track-process=false
haproxy -W -db -S /run/haproxy/master.sock,mode,600 -f /etc/haproxy/haproxy.cfg &
keepalived -n -l -D -f /etc/keepalived/keepalived.conf &
# like After=haproxy.service on a gateway: the agent talks to HAProxy as soon as it starts
for _ in $(seq 50); do [ -S /run/haproxy/admin.sock ] && break; sleep 0.1; done
exec "$B/gwagent" -name "$GW_NAME" -cpd "$CPD_URL"
