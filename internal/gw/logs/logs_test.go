package logs

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/CSTAM-well-contained/monorepo/internal/model"
)

// a real line from HAProxy 3.2 with "log ... format rfc5424"
const sample = `<134>1 2026-10-20T17:45:00.123456+00:00 gw1 haproxy 812 - - {"ts":"20/Oct/2026:17:45:00.120","gw":"gw1","client":"10.20.0.1","host":"team01.demo.cstam.test","method":"GET","path":"/","be":"be_slot001","srv":"team","status":200,"bytes":231,"tt":3,"tr":1,"term":"--"}`

func TestParse(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		ok   bool
		host string
	}{
		{"rfc5424 line", sample, true, "team01.demo.cstam.test"},
		{"healthz is not traffic", strings.Replace(sample, `"path":"/"`, `"path":"/__gw/healthz"`, 1), false, ""},
		{"canary is not traffic", strings.Replace(sample, "team01.demo.cstam.test", "canary.gw.internal", 1), false, ""},
		{"not json", "<134>1 - gw1 haproxy - - - Proxy fe_http started.", false, ""},
		{"broken json", "<134>1 - - - - - {\"ts\":", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, ok := Parse([]byte(tc.msg))
			if ok != tc.ok || l.Host != tc.host {
				t.Fatalf("Parse = %+v, %v", l, ok)
			}
			if ok && (l.Status != 200 || l.TotalMS != 3 || l.Backend != "be_slot001" || l.GW != "gw1") {
				t.Fatalf("fields: %+v", l)
			}
		})
	}
}

func TestClip(t *testing.T) {
	l := Clip(model.TrafficLine{Host: strings.Repeat("h", 1000), Path: strings.Repeat("/", 1000)})
	if len(l.Host) != 128 || len(l.Path) != 200 {
		t.Fatalf("host %d path %d", len(l.Host), len(l.Path))
	}
}

func TestBufferDropsOldest(t *testing.T) {
	c := New(3)
	for i := range 5 {
		c.Add(model.TrafficLine{Status: 200 + i})
	}
	got, dropped := c.Drain(2)
	if dropped != 2 || len(got) != 2 || got[0].Status != 202 || got[1].Status != 203 {
		t.Fatalf("drain = %+v, dropped %d", got, dropped)
	}
	c.Lost(7)
	got, dropped = c.Drain(10)
	if dropped != 7 || len(got) != 1 || got[0].Status != 204 {
		t.Fatalf("second drain = %+v, dropped %d", got, dropped)
	}
	if got, _ := c.Drain(10); len(got) != 0 {
		t.Fatalf("empty drain = %+v", got)
	}
}

func TestListenUDP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := New(10)
	addr := "127.0.0.1:" + freePort(t)
	done := make(chan error, 1)
	go func() { done <- c.Listen(ctx, addr) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if conn, err := net.Dial("udp", addr); err == nil {
			_, _ = conn.Write([]byte(sample))
			_ = conn.Close()
		}
		time.Sleep(20 * time.Millisecond)
		if got, _ := c.Drain(10); len(got) > 0 {
			if got[0].Host != "team01.demo.cstam.test" {
				t.Fatalf("got %+v", got[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no line received")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	_, port, _ := net.SplitHostPort(pc.LocalAddr().String())
	return port
}
