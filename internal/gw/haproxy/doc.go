// Package haproxy drives the gateway's HAProxy: it renders haproxy.cfg and the map files,
// checks a config with `haproxy -c`, and talks to the runtime and master CLI sockets.
// It knows nothing about the control plane; the applier package orchestrates it.
package haproxy
