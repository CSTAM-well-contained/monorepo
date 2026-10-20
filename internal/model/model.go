// Package model holds the JSON types exchanged between cpd and the gateway agents. It has
// no dependencies, so the agents can import it (they never import pgx or gophercloud).
package model

// Route publishes one team: requests for Host go to Addr:Port through backend be_slot<Slot>.
type Route struct {
	Host string `json:"host"`
	Slot int    `json:"slot"`
	Addr string `json:"addr"`
	Port int    `json:"port"`
}

// Desired is the full desired state of the gateways at one generation. Every change of the
// published routes or of ForceBackup increases Generation; a gateway applies the whole
// table, never a diff.
type Desired struct {
	Generation int64   `json:"generation"`
	Routes     []Route `json:"routes"`
	// ForceBackup names the gateways asked to step down (manual failover): their agent
	// writes 1 into keepalived's force-backup file, the others write 0.
	ForceBackup []string `json:"force_backup,omitempty"`
	// Config is the newest HAProxy config version; nil until an admin pushes one (the
	// gateways then keep the settings of their command line).
	Config *Config `json:"config,omitempty"`
}

// Config is the part of haproxy.cfg that cpd owns. A new Version means a validated hitless
// reload on each gateway, rolled back there if the new config does not serve.
type Config struct {
	Version   string   `json:"version"`         // c<id>
	RateLimit int      `json:"rate_limit"`      // requests per 10 s per Host
	Extra     []string `json:"extra,omitempty"` // raw lines in the HTTP frontend (admin escape hatch)
}

// TrafficLine is one HTTP request seen by a gateway (the JSON log-format of haproxy.cfg).
type TrafficLine struct {
	TS      string `json:"ts"` // HAProxy %tr, e.g. 20/Oct/2026:17:45:00.123
	GW      string `json:"gw"`
	Client  string `json:"client"`
	Host    string `json:"host"`
	Method  string `json:"method"`
	Path    string `json:"path"` // without the query string
	Backend string `json:"be"`
	Server  string `json:"srv"`
	Status  int    `json:"status"`
	Bytes   int64  `json:"bytes"`
	TotalMS int    `json:"tt"` // total time
	RespMS  int    `json:"tr"` // server response time
	Term    string `json:"term"`
}

// TrafficBatch is what a gateway agent ships to cpd about once a second.
type TrafficBatch struct {
	Lines   []TrafficLine `json:"lines"`
	Dropped int64         `json:"dropped"` // lines lost on the gateway since the last batch
}

// MaxTrafficBatch caps the lines of one batch (the API body limit is 64 KiB).
const MaxTrafficBatch = 150

// Ack is what a gateway reports after applying a generation.
type Ack struct {
	Generation    int64  `json:"generation"`
	ConfigVersion string `json:"config_version"` // the version that serves
	VRRPState     string `json:"vrrp_state"`     // MASTER, BACKUP, FAULT, STOP or empty
	// ConfigOutcome is what happened to the last config version tried: applied, rejected
	// (haproxy -c), rolled_back (loaded, failed verification), failed (rollback failed too).
	ConfigTried   string `json:"config_tried,omitempty"`
	ConfigOutcome string `json:"config_outcome,omitempty"`
	ConfigError   string `json:"config_error,omitempty"`
}
