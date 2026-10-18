package config

import (
	"net/netip"
	"testing"
)

// The plan's safety rules: if one fails, two systems could hand out the same IP.
func TestTopologyInvariants(t *testing.T) {
	infra := append([]netip.Addr{Router, ControlIP, VIP, HostRangeLo, HostRangeHi}, Gateways...)

	for _, ip := range infra {
		if !Subnet.Contains(ip) {
			t.Errorf("%s is outside the subnet %s", ip, Subnet)
		}
		if InTeamPool(ip) {
			t.Errorf("infrastructure IP %s is inside the team pool", ip)
		}
		if InNeutronPool(ip) {
			t.Errorf("infrastructure IP %s is inside Neutron's allocation pool", ip)
		}
	}

	// The team pool and the lab pool must not reach Neutron's pool.
	for _, ip := range []netip.Addr{TeamPoolLo, TeamPoolHi, LabPoolLo, LabPoolHi} {
		if InNeutronPool(ip) {
			t.Errorf("%s overlaps Neutron's allocation pool", ip)
		}
	}
	if HostRangeHi.Compare(TeamPoolLo) >= 0 {
		t.Errorf("host range ends at %s, inside the team pool starting at %s", HostRangeHi, TeamPoolLo)
	}
	if TeamPoolHi.Compare(LabPoolLo) >= 0 {
		t.Errorf("team pool %s-%s overlaps lab pool starting at %s", TeamPoolLo, TeamPoolHi, LabPoolLo)
	}

	reserved := netip.MustParseAddr("10.20.0.68")
	found := false
	for _, ip := range ReservedIPs {
		found = found || ip == reserved
	}
	if !found {
		t.Errorf("10.20.0.68 must stay in ReservedIPs")
	}

	if MTU != 1442 {
		t.Errorf("MTU = %d, FelCloud's Geneve network is 1442", MTU)
	}
}

func TestPoolSize(t *testing.T) {
	n := 0
	for ip := TeamPoolLo; ip.Compare(TeamPoolHi) <= 0; ip = ip.Next() {
		n++
	}
	if n != 160 {
		t.Fatalf("team pool has %d addresses, want 160", n)
	}
}
