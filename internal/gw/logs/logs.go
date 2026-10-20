// Package logs collects the gateway's traffic log: HAProxy sends one JSON line per request
// over UDP syslog (RFC 5424) to the agent, which keeps a bounded buffer and ships it to cpd
// for the dashboard. Best effort by design: when the buffer is full, the oldest lines are
// dropped and counted, so a flood can never slow HAProxy or the agent down.
package logs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/CSTAM-well-contained/monorepo/internal/model"
)

// Collector buffers parsed lines.
type Collector struct {
	max int

	mu      sync.Mutex
	buf     []model.TrafficLine
	dropped int64
}

// New returns a collector that keeps at most size lines (default 2000).
func New(size int) *Collector {
	if size <= 0 {
		size = 2000
	}
	return &Collector{max: size}
}

// Listen reads syslog datagrams on addr (e.g. 127.0.0.1:5514) until ctx ends.
func (c *Collector) Listen(ctx context.Context, addr string) error {
	conn, err := (&net.ListenConfig{}).ListenPacket(ctx, "udp", addr)
	if err != nil {
		return fmt.Errorf("traffic log: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	buf := make([]byte, 9000) // haproxy.cfg: len 8192
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("traffic log: %w", err)
		}
		if l, ok := Parse(buf[:n]); ok {
			c.Add(l)
		}
	}
}

// Add appends a line, dropping the oldest one when the buffer is full.
func (c *Collector) Add(l model.TrafficLine) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.buf) >= c.max {
		c.buf = c.buf[1:]
		c.dropped++
	}
	c.buf = append(c.buf, l)
}

// Drain removes and returns up to n lines (oldest first) and the drop count since last time.
func (c *Collector) Drain(n int) ([]model.TrafficLine, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n = min(n, len(c.buf))
	out := make([]model.TrafficLine, n)
	copy(out, c.buf[:n])
	c.buf = append(c.buf[:0], c.buf[n:]...)
	d := c.dropped
	c.dropped = 0
	return out, d
}

// Lost counts lines the agent could not ship (cpd down): they are reported with the next batch.
func (c *Collector) Lost(n int64) {
	c.mu.Lock()
	c.dropped += n
	c.mu.Unlock()
}

// Parse extracts the JSON line of one syslog message. Requests to the internal endpoints
// (/__gw/healthz from keepalived, the applier and the canary) are not traffic and are skipped.
func Parse(msg []byte) (model.TrafficLine, bool) {
	i := bytes.IndexByte(msg, '{')
	if i < 0 {
		return model.TrafficLine{}, false
	}
	var l model.TrafficLine
	if err := json.Unmarshal(bytes.TrimSpace(msg[i:]), &l); err != nil {
		return model.TrafficLine{}, false
	}
	if strings.HasPrefix(l.Path, "/__gw/") || l.Host == "canary.gw.internal" {
		return model.TrafficLine{}, false
	}
	return Clip(l), true
}

// Clip bounds every text field, so a hostile Host header or path cannot bloat the buffers.
func Clip(l model.TrafficLine) model.TrafficLine {
	cut := func(s string, n int) string {
		if len(s) > n {
			return s[:n]
		}
		return s
	}
	l.TS, l.GW, l.Client = cut(l.TS, 40), cut(l.GW, 32), cut(l.Client, 64)
	l.Host, l.Method, l.Path = cut(l.Host, 128), cut(l.Method, 16), cut(l.Path, 200)
	l.Backend, l.Server, l.Term = cut(l.Backend, 32), cut(l.Server, 32), cut(l.Term, 8)
	return l
}
