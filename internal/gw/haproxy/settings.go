package haproxy

import (
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"
)

// CanaryHost always routes to be_slot000; the applier requests it after every reload.
const CanaryHost = "canary.gw.internal"

// ParkAddr is where an unused slot points: TEST-NET-1, never routable.
var ParkAddr = netip.MustParseAddr("192.0.2.1")

// Settings are the inputs of haproxy.cfg that only change with a reload.
// Routes are not in here: they change at runtime (see Route).
type Settings struct {
	Version string // CFG_VERSION, served in X-Config-Version
	GWName  string // gw1 or gw2

	Dir         string // holds haproxy.cfg, maps/ and errors/ (production: /etc/haproxy)
	AdminSocket string // runtime API socket (production: /run/haproxy/admin.sock)
	SocketMode  string // octal, e.g. "660"
	SocketGroup string // "" = keep the process group

	Chroot string // "" = no chroot (tests run unprivileged)
	User   string
	Group  string

	HTTPBind   string // ":80"
	StatsBind  string // "10.20.0.11:8404", "" = no stats frontend
	TrafficLog string // "127.0.0.1:5514", "" = no traffic log
	ACMEAddr   string // "10.20.0.5:8402", "" = no ACME backend
	CanaryAddr string // the agent's canary responder, "127.0.0.1:9102"

	InternalSources []string // may call /__gw/healthz
	Slots           int      // team slots be_slot001..be_slotN
	RateLimit       int      // default requests per 10 s per Host

	// Extra are raw frontend lines inserted before the internal endpoints.
	// Admin escape hatch, and how the tests inject a valid but broken config.
	Extra []string
}

// Route publishes one Host on one slot. Several hosts may share a slot (same team app).
type Route struct {
	Host string
	Slot int
	Addr netip.Addr
	Port uint16
}

var (
	versionRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)
	nameRE    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	// lowercase DNS name with at least one dot, labels of 1 to 63 characters
	hostRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	modeRE = regexp.MustCompile(`^[0-7]{3,4}$`)
)

// MaxHostLen matches the capture length in the frontend.
const MaxHostLen = 128

// Validate rejects settings that would render a wrong or unsafe config.
func (s *Settings) Validate() error {
	var errs []error
	if !versionRE.MatchString(s.Version) {
		errs = append(errs, fmt.Errorf("version %q: want 1-32 of [A-Za-z0-9._-]", s.Version))
	}
	if !nameRE.MatchString(s.GWName) {
		errs = append(errs, fmt.Errorf("gateway name %q is not a valid name", s.GWName))
	}
	for name, p := range map[string]string{"dir": s.Dir, "admin socket": s.AdminSocket, "chroot": s.Chroot} {
		if p == "" && name == "chroot" {
			continue
		}
		if !filepath.IsAbs(p) || strings.ContainsAny(p, " \t\n\"'") {
			errs = append(errs, fmt.Errorf("%s %q must be an absolute path without spaces or quotes", name, p))
		}
	}
	if !modeRE.MatchString(s.SocketMode) {
		errs = append(errs, fmt.Errorf("socket mode %q is not octal", s.SocketMode))
	}
	for name, v := range map[string]string{"socket group": s.SocketGroup, "user": s.User, "group": s.Group} {
		if v != "" && !nameRE.MatchString(v) {
			errs = append(errs, fmt.Errorf("%s %q is not a valid name", name, v))
		}
	}
	for name, v := range map[string]string{"http bind": s.HTTPBind, "stats bind": s.StatsBind, "traffic log": s.TrafficLog, "acme": s.ACMEAddr, "canary": s.CanaryAddr} {
		if v == "" && name != "http bind" && name != "canary" {
			continue
		}
		if err := checkBind(v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	for _, src := range s.InternalSources {
		if _, err := netip.ParseAddr(src); err != nil {
			if _, err := netip.ParsePrefix(src); err != nil {
				errs = append(errs, fmt.Errorf("internal source %q is not an IP or CIDR", src))
			}
		}
	}
	if s.Slots < 1 || s.Slots > 999 {
		errs = append(errs, fmt.Errorf("slots = %d, want 1..999", s.Slots))
	}
	if s.RateLimit < 1 {
		errs = append(errs, fmt.Errorf("rate limit = %d, want > 0", s.RateLimit))
	}
	for _, l := range s.Extra {
		if strings.ContainsAny(l, "\r\n") {
			errs = append(errs, fmt.Errorf("extra line %q contains a newline", l))
		}
	}
	return errors.Join(errs...)
}

// checkBind accepts "host:port" or ":port" with a numeric port.
func checkBind(v string) error {
	if strings.HasPrefix(v, ":") {
		v = "0.0.0.0" + v
	}
	ap, err := netip.ParseAddrPort(v)
	if err != nil {
		return fmt.Errorf("%q is not ip:port", v)
	}
	if ap.Port() == 0 {
		return fmt.Errorf("%q has port 0", v)
	}
	return nil
}

// SlotName is the backend name of a slot: be_slot007.
func SlotName(n int) string { return fmt.Sprintf("be_slot%03d", n) }

// Endpoint is where a slot sends traffic.
type Endpoint struct {
	Addr netip.Addr
	Port uint16
}

func (e Endpoint) String() string { return netip.AddrPortFrom(e.Addr, e.Port).String() }

// SlotTable checks routes against the slot count and returns slot -> endpoint.
// It fails on a duplicate host or a slot claimed with two different endpoints.
func SlotTable(routes []Route, slots int) (map[int]Endpoint, error) {
	table := make(map[int]Endpoint, len(routes))
	hosts := make(map[string]bool, len(routes))
	for _, r := range routes {
		switch {
		case len(r.Host) > MaxHostLen || !hostRE.MatchString(r.Host):
			return nil, fmt.Errorf("host %q is not a lowercase DNS name of at most %d characters", r.Host, MaxHostLen)
		case r.Host == CanaryHost:
			return nil, fmt.Errorf("host %q is reserved", r.Host)
		case hosts[r.Host]:
			return nil, fmt.Errorf("host %q is routed twice", r.Host)
		case r.Slot < 1 || r.Slot > slots:
			return nil, fmt.Errorf("host %q: slot %d outside 1..%d", r.Host, r.Slot, slots)
		case !r.Addr.Is4() || r.Addr == ParkAddr:
			return nil, fmt.Errorf("host %q: address %s is not a usable IPv4", r.Host, r.Addr)
		case r.Port == 0:
			return nil, fmt.Errorf("host %q: port 0", r.Host)
		}
		hosts[r.Host] = true
		ep := Endpoint{r.Addr, r.Port}
		if old, ok := table[r.Slot]; ok && old != ep {
			return nil, fmt.Errorf("slot %d points to both %s and %s", r.Slot, old, ep)
		}
		table[r.Slot] = ep
	}
	return table, nil
}
