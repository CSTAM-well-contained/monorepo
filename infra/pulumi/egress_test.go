package main

import (
	"net/netip"
	"testing"
)

func TestExclude(t *testing.T) {
	all := netip.MustParsePrefix("0.0.0.0/0")
	subnet := netip.MustParsePrefix(topo.SubnetCIDR)
	got, err := exclude(all, subnet)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 24 {
		t.Fatalf("%d prefixes, want 24 for a /24: %v", len(got), got)
	}
	// every probe address is covered exactly once, except the subnet itself
	probes := []string{"0.0.0.0", "8.8.8.8", "10.19.255.255", "10.20.0.0", "10.20.0.5", "10.20.0.255",
		"10.20.1.0", "10.21.0.1", "169.254.169.254", "197.5.133.180", "255.255.255.255"}
	for _, s := range probes {
		a := netip.MustParseAddr(s)
		n := 0
		for _, p := range got {
			if p.Contains(a) {
				n++
			}
		}
		want := 1
		if subnet.Contains(a) {
			want = 0
		}
		if n != want {
			t.Errorf("%s covered %d times, want %d", a, n, want)
		}
	}
	if _, err := exclude(netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.168.0.0/24")); err == nil {
		t.Fatal("a hole outside the range must fail")
	}
}
