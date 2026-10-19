// Package config holds the fixed topology of the platform and, later, the
// environment structs of each binary.
package config

import "net/netip"

// The IP plan of cstam-sandbox-net. infra/topology.env (shell) and the topo
// variable in infra/pulumi/config.go must stay identical to these values.
var (
	Subnet    = netip.MustParsePrefix("10.20.0.0/24")
	Router    = netip.MustParseAddr("10.20.0.1")
	ControlIP = netip.MustParseAddr("10.20.0.5")
	VIP       = netip.MustParseAddr("10.20.0.10")
	Gateways  = []netip.Addr{netip.MustParseAddr("10.20.0.11"), netip.MustParseAddr("10.20.0.12")}
	// Hackathon hosts are created on demand by cpd, one or more per hackathon, and get a
	// fixed IP from this range (11 hosts at most, far above the instance quota).
	HostRangeLo   = netip.MustParseAddr("10.20.0.21")
	HostRangeHi   = netip.MustParseAddr("10.20.0.31")
	TeamPoolLo    = netip.MustParseAddr("10.20.0.32")
	TeamPoolHi    = netip.MustParseAddr("10.20.0.191")
	LabPoolLo     = netip.MustParseAddr("10.20.0.192")
	LabPoolHi     = netip.MustParseAddr("10.20.0.199")
	NeutronPoolLo = netip.MustParseAddr("10.20.0.200") // Neutron's own allocation pool: never ours
	NeutronPoolHi = netip.MustParseAddr("10.20.0.250")
	// ReservedIPs inside the team pool that the IPAM must never lease. 10.20.0.68 belonged
	// to sbx-f0699704 (deleted on 2026-10-17); it stays reserved until a Neutron scan shows
	// it free, so a forgotten fixed-IP user cannot collide with a team.
	ReservedIPs = []netip.Addr{netip.MustParseAddr("10.20.0.68")}
	MTU         = 1442
)

// InTeamPool reports whether ip is in the range our IPAM hands out.
func InTeamPool(ip netip.Addr) bool {
	return inRange(ip, TeamPoolLo, TeamPoolHi)
}

// InNeutronPool reports whether ip is in the range Neutron allocates by itself.
func InNeutronPool(ip netip.Addr) bool {
	return inRange(ip, NeutronPoolLo, NeutronPoolHi)
}

func inRange(ip, lo, hi netip.Addr) bool {
	return ip.Compare(lo) >= 0 && ip.Compare(hi) <= 0
}

// TeamPorts are the app port ranges the cstam-team security group opens to the gateways.
// infra/pulumi/secgroups.go must open exactly these ranges.
var TeamPorts = [][2]int{{80, 80}, {8000, 8099}}

// TeamPortAllowed reports whether the gateways can reach a team app on port p.
func TeamPortAllowed(p int) bool {
	for _, r := range TeamPorts {
		if p >= r[0] && p <= r[1] {
			return true
		}
	}
	return false
}
