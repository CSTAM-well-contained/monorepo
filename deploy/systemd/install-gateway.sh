#!/bin/sh
# install-gateway.sh BIN_DIR ENV_FILE: install gwagent and the CSTAM units on a gateway, as root.
# Needs the haproxy and keepalived packages (cloud-init installs them). Idempotent: run it again
# to upgrade the binary or change the env file. ENV_FILE: see deploy/env/gwagent.env.example.
set -eu
BIN=$1 ENV=$2
HERE=$(dirname "$0")

id cstam >/dev/null 2>&1 || useradd -r -s /usr/sbin/nologin -G haproxy cstam
id keepalived_script >/dev/null 2>&1 || useradd -r -s /usr/sbin/nologin keepalived_script

install -m 0755 "$BIN/gwagent" /usr/local/bin/gwagent
install -d -m 0750 -o cstam -g cstam /etc/cstam
install -d -m 0770 -o cstam -g haproxy /var/lib/cstam
install -m 0600 -o cstam -g cstam "$ENV" /etc/cstam/gwagent.env
install -m 0644 "$HERE/cstam-gw-bootstrap.service" "$HERE/gwagent.service" /etc/systemd/system/
install -d /etc/systemd/system/haproxy.service.d
install -m 0644 "$HERE/haproxy.service.d/cstam.conf" /etc/systemd/system/haproxy.service.d/cstam.conf
install -m 0644 "$HERE/cstam-gw.tmpfiles.conf" /etc/tmpfiles.d/cstam-gw.conf
systemd-tmpfiles --create /etc/tmpfiles.d/cstam-gw.conf

systemctl daemon-reload
systemctl enable cstam-gw-bootstrap.service haproxy.service keepalived.service gwagent.service
# the bootstrap rewrites the files; the others pick them up
systemctl restart cstam-gw-bootstrap.service
systemctl restart haproxy.service keepalived.service gwagent.service
systemctl --no-pager --lines=0 status cstam-gw-bootstrap haproxy keepalived gwagent
