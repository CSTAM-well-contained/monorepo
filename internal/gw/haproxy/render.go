package haproxy

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
)

//go:embed haproxy.cfg.tmpl
var cfgTmpl string

//go:embed assets
var assets embed.FS

var tmpl = template.Must(template.New("haproxy.cfg").Parse(cfgTmpl))

type slotView struct {
	Name     string
	Endpoint string
	Used     bool
}

// Render produces haproxy.cfg. Used slots get their real address, so a reload keeps the routes.
func Render(s Settings, routes []Route) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	table, err := SlotTable(routes, s.Slots)
	if err != nil {
		return nil, err
	}
	slots := make([]slotView, s.Slots)
	for i := range slots {
		n := i + 1
		ep, used := table[n]
		if !used {
			ep = Endpoint{ParkAddr, 80}
		}
		slots[i] = slotView{Name: SlotName(n), Endpoint: ep.String(), Used: used}
	}
	var b bytes.Buffer
	err = tmpl.Execute(&b, struct {
		S          Settings
		Slots      []slotView
		MaxHostLen int
	}{s, slots, MaxHostLen})
	return b.Bytes(), err
}

// MapEntry is one line of a map file.
type MapEntry struct{ Key, Value string }

// HostsMap is the content of hosts.map: one "host backend" line per route plus the canary.
func HostsMap(routes []Route) []MapEntry {
	out := []MapEntry{{CanaryHost, "be_slot000"}}
	for _, r := range routes {
		out = append(out, MapEntry{r.Host, SlotName(r.Slot)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// MetaMap is the content of meta.map.
func MetaMap(generation uint64) []MapEntry {
	return []MapEntry{{"generation", fmt.Sprint(generation)}}
}

func mapFile(entries []MapEntry) []byte {
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.Key + " " + e.Value + "\n")
	}
	return []byte(b.String())
}

// CfgPath is haproxy.cfg under Settings.Dir.
func CfgPath(dir string) string { return filepath.Join(dir, "haproxy.cfg") }

// HostsPath is the routing map under Settings.Dir.
func HostsPath(dir string) string { return filepath.Join(dir, "maps", "hosts.map") }

// MetaPath is the map holding the routes generation under Settings.Dir.
func MetaPath(dir string) string { return filepath.Join(dir, "maps", "meta.map") }

// InstallStatic creates maps/ and errors/ with the error pages, and the map files that
// do not exist yet. Existing map files are kept.
func InstallStatic(dir string) error {
	for _, d := range []string{"maps", "errors"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil { //nolint:gosec // haproxy (another user) reads these
			return err
		}
	}
	for _, name := range []string{"404.html", "maintenance.html", "503-maintenance.http"} {
		data, err := assets.ReadFile("assets/" + name)
		if err != nil {
			return err
		}
		if err := WriteFileAtomic(filepath.Join(dir, "errors", name), data); err != nil {
			return err
		}
	}
	for name, data := range map[string][]byte{
		"hosts.map":     mapFile(HostsMap(nil)),
		"meta.map":      mapFile(MetaMap(0)),
		"ratelimit.map": nil,
	} {
		p := filepath.Join(dir, "maps", name)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			if err := WriteFileAtomic(p, data); err != nil {
				return err
			}
		}
	}
	return nil
}

// WriteMaps writes hosts.map and meta.map, so a reload or a restart sees the same routes.
func WriteMaps(dir string, generation uint64, routes []Route) error {
	if err := WriteFileAtomic(HostsPath(dir), mapFile(HostsMap(routes))); err != nil {
		return err
	}
	return WriteFileAtomic(MetaPath(dir), mapFile(MetaMap(generation)))
}

// WriteFileAtomic writes a temporary file in the same directory, syncs it, then renames it
// over path: a reader sees the old file or the new one, never half of one.
func WriteFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op after a successful rename
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o644)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
