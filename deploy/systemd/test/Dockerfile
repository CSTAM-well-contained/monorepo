# A gateway with systemd as PID 1, to test deploy/systemd on one machine
# (deploy/dev/demo.sh with GW_MODE=systemd). Base: the lab image (HAProxy 3.2 + keepalived).
ARG BASE=cstam-lab-gw:test
FROM ${BASE}
USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv \
 && rm -rf /var/lib/apt/lists/* \
 && ln -sf /usr/local/sbin/haproxy /usr/sbin/haproxy
# the haproxy image is not the Debian package: stand-in for the package unit (overridden by
# our drop-in for ExecStart, as on the VMs)
COPY haproxy.service /etc/systemd/system/haproxy.service
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
