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

// Desired is the full routing table at one generation. Every change of the published
// routes increases Generation; a gateway applies the whole table, never a diff.
type Desired struct {
	Generation int64   `json:"generation"`
	Routes     []Route `json:"routes"`
}

// Ack is what a gateway reports after applying a generation.
type Ack struct {
	Generation    int64  `json:"generation"`
	ConfigVersion string `json:"config_version"`
	VRRPState     string `json:"vrrp_state"` // MASTER, BACKUP, FAULT, STOP or empty
}
