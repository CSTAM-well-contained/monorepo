# Lab image for the keepalived integration test: HAProxy 3.2 + keepalived on one container.
FROM haproxy:3.2
USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends keepalived curl iproute2 procps \
 && rm -rf /var/lib/apt/lists/*
