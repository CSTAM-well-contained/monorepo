// Package keepalived renders the VRRP config and scripts that move the VIP between the two
// gateways, and reads the state file the notify script writes.
package keepalived

import (
	"bytes"
	_ "embed" // templates and scripts
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/CSTAM-well-contained/monorepo/internal/config"
)

//go:embed keepalived.conf.tmpl
var confTmpl string

// CheckScript and NotifyScript are installed root:root 0755 (enable_script_security).
var (
	//go:embed chk_gateway.sh
	CheckScript []byte
	//go:embed vrrp-notify.sh
	NotifyScript []byte
)

var tmpl = template.Must(template.New("keepalived.conf").Parse(confTmpl))

// Settings for one gateway. gw1 and gw2 differ only in RouterID, Priority, SrcIP and PeerIP.
type Settings struct {
	RouterID     string // cstam-gw1
	Interface    string // ens3
	VRID         int    // virtual_router_id, 1..255
	Priority     int    // 150 on gw1, 100 on gw2
	AdvertInt    time.Duration
	SrcIP        netip.Addr
	PeerIP       netip.Addr
	VIP          netip.Addr
	ScriptUser   string // keepalived_script
	CheckScript  string // /usr/local/libexec/cstam/chk_gateway
	NotifyScript string // /usr/local/libexec/cstam/vrrp-notify
	// ForceBackupFile holds 0 or 1; 1 puts this gateway in FAULT (manual failover).
	ForceBackupFile string
	// TrackProcess enables vrrp_track_process (needs the initial PID namespace).
	TrackProcess bool
}

var (
	nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	userRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`) // Unix user name
)

// Validate rejects settings that would render a wrong config.
func (s *Settings) Validate() error {
	var errs []error
	for what, v := range map[string]string{"router id": s.RouterID, "interface": s.Interface} {
		if !nameRE.MatchString(v) {
			errs = append(errs, fmt.Errorf("%s %q is not a valid name", what, v))
		}
	}
	if !userRE.MatchString(s.ScriptUser) {
		errs = append(errs, fmt.Errorf("script user %q is not a valid user name", s.ScriptUser))
	}
	if s.VRID < 1 || s.VRID > 255 {
		errs = append(errs, fmt.Errorf("vrid %d outside 1..255", s.VRID))
	}
	if s.Priority < 1 || s.Priority > 254 {
		errs = append(errs, fmt.Errorf("priority %d outside 1..254", s.Priority))
	}
	if s.AdvertInt < 10*time.Millisecond || s.AdvertInt > 40*time.Second {
		errs = append(errs, fmt.Errorf("advert interval %v outside 10ms..40s", s.AdvertInt))
	}
	for what, ip := range map[string]netip.Addr{"src": s.SrcIP, "peer": s.PeerIP, "vip": s.VIP} {
		if !ip.Is4() {
			errs = append(errs, fmt.Errorf("%s ip %s is not IPv4", what, ip))
		}
	}
	if s.SrcIP == s.PeerIP || s.VIP == s.SrcIP || s.VIP == s.PeerIP {
		errs = append(errs, errors.New("src, peer and vip must be three different addresses"))
	}
	for what, p := range map[string]string{"check script": s.CheckScript, "notify script": s.NotifyScript, "force backup file": s.ForceBackupFile} {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, " \t\n\"'") {
			errs = append(errs, fmt.Errorf("%s %q must be an absolute path without spaces or quotes", what, p))
		}
	}
	return errors.Join(errs...)
}

// Render produces keepalived.conf.
func Render(s Settings) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	err := tmpl.Execute(&b, struct {
		Settings
		AdvertInt string
	}{s, strconv.FormatFloat(s.AdvertInt.Seconds(), 'f', -1, 64)})
	return b.Bytes(), err
}

// MasterDownInterval is how long the backup waits before taking over after a silent failure
// of the master: 3 x advert_int + skew, skew = (256 - priority) x advert_int / 256.
func MasterDownInterval(advert time.Duration, backupPriority int) time.Duration {
	return 3*advert + time.Duration(256-backupPriority)*advert/256
}

// State is the last transition written by the notify script.
type State struct {
	Name     string // MASTER, BACKUP, FAULT, STOP
	At       time.Time
	Priority int
}

// ReadState parses the state file: "MASTER 1760000000.123456789 150".
func ReadState(path string) (State, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path comes from agent config
	if err != nil {
		return State{}, err
	}
	return ParseState(string(data))
}

// ParseState parses one line of the state file.
func ParseState(line string) (State, error) {
	f := strings.Fields(line)
	if len(f) != 3 {
		return State{}, fmt.Errorf("vrrp state %q: want 3 fields", line)
	}
	sec, err := strconv.ParseFloat(f[1], 64)
	if err != nil {
		return State{}, fmt.Errorf("vrrp state time %q: %w", f[1], err)
	}
	prio, err := strconv.Atoi(f[2])
	if err != nil {
		return State{}, fmt.Errorf("vrrp state priority %q: %w", f[2], err)
	}
	whole := int64(sec)
	return State{Name: f[0], At: time.Unix(whole, int64((sec-float64(whole))*1e9)), Priority: prio}, nil
}

// Default install paths on a gateway.
const (
	DefaultConf    = "/etc/keepalived/keepalived.conf"
	DefaultLibexec = "/usr/local/libexec/cstam"
)

// ForGateway returns the settings of gateway name (gw1, gw2) from the IP plan: gw1 has the
// higher priority, each one names the other as its unicast peer.
func ForGateway(name, iface string) (Settings, error) {
	var idx int
	switch name {
	case "gw1":
		idx = 0
	case "gw2":
		idx = 1
	default:
		return Settings{}, fmt.Errorf("gateway %q: want gw1 or gw2", name)
	}
	s := Settings{
		RouterID: "cstam-" + name, Interface: iface, VRID: 51,
		Priority: []int{150, 100}[idx], AdvertInt: 500 * time.Millisecond,
		SrcIP: config.Gateways[idx], PeerIP: config.Gateways[1-idx], VIP: config.VIP,
		ScriptUser:      "keepalived_script",
		CheckScript:     DefaultLibexec + "/chk_gateway",
		NotifyScript:    DefaultLibexec + "/vrrp-notify",
		ForceBackupFile: "/run/cstam/force-backup",
		TrackProcess:    true,
	}
	return s, s.Validate()
}

// Install renders keepalived.conf to conf and writes the two scripts where s says (mode
// 0755: keepalived refuses scripts writable by others). It does not restart keepalived.
func Install(s Settings, conf string) error {
	data, err := Render(s)
	if err != nil {
		return err
	}
	files := []struct {
		path string
		data []byte
		mode os.FileMode
	}{{conf, data, 0o644}, {s.CheckScript, CheckScript, 0o755}, {s.NotifyScript, NotifyScript, 0o755}}
	for _, f := range files {
		if err := writeFile(f.path, f.data, f.mode); err != nil {
			return err
		}
	}
	return nil
}

// writeFile replaces path atomically (temp file in the same directory, then rename).
func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // system config dirs
		return fmt.Errorf("keepalived install: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cstam-*")
	if err != nil {
		return fmt.Errorf("keepalived install: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("keepalived install %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("keepalived install %s: %w", path, err)
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return fmt.Errorf("keepalived install %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("keepalived install %s: %w", path, err)
	}
	return nil
}
