package keepalived

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForGateway(t *testing.T) {
	tests := []struct {
		name, iface, wantErr string
		want                 []string
	}{
		{"gw1", "ens3", "", []string{"priority 150", "unicast_src_ip 10.20.0.11", "10.20.0.12\n", "10.20.0.10/32 dev ens3", "advert_int 0.5"}},
		{"gw2", "eth0", "", []string{"priority 100", "unicast_src_ip 10.20.0.12", "10.20.0.11\n", "router_id cstam-gw2"}},
		{"gw3", "ens3", "want gw1 or gw2", nil},
		{"gw1", "bad iface", "not a valid name", nil},
	}
	for _, tt := range tests {
		s, err := ForGateway(tt.name, tt.iface)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ForGateway(%s, %q) = %v, want %q", tt.name, tt.iface, err, tt.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ForGateway(%s): %v", tt.name, err)
		}
		conf, err := Render(s)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range tt.want {
			if !strings.Contains(string(conf), w) {
				t.Errorf("%s config lacks %q", tt.name, w)
			}
		}
	}
}

func TestInstall(t *testing.T) {
	dir := t.TempDir()
	s, err := ForGateway("gw1", "eth0")
	if err != nil {
		t.Fatal(err)
	}
	s.CheckScript = filepath.Join(dir, "libexec", "chk_gateway")
	s.NotifyScript = filepath.Join(dir, "libexec", "vrrp-notify")
	conf := filepath.Join(dir, "etc", "keepalived.conf")
	for range 2 { // idempotent
		if err := Install(s, conf); err != nil {
			t.Fatal(err)
		}
	}
	for path, mode := range map[string]os.FileMode{conf: 0o644, s.CheckScript: 0o755, s.NotifyScript: 0o755} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != mode {
			t.Errorf("%s mode %v, want %v", path, st.Mode().Perm(), mode)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*", ".cstam-*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}
