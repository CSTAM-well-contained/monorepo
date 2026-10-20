package main

import (
	"fmt"
	"net/netip"

	"github.com/pulumi/pulumi-openstack/sdk/v5/go/openstack/networking"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// exclude returns the prefixes that cover all of "all" except "hole", fewest first: the
// complement of a subnet, for security groups, which can only allow, never deny.
func exclude(all, hole netip.Prefix) ([]netip.Prefix, error) {
	all, hole = all.Masked(), hole.Masked()
	if !all.Contains(hole.Addr()) || hole.Bits() < all.Bits() {
		return nil, fmt.Errorf("%s is not inside %s", hole, all)
	}
	var out []netip.Prefix
	cur := all
	for cur.Bits() < hole.Bits() {
		// split cur in two halves: keep the one without the hole, descend into the other
		lo := netip.PrefixFrom(cur.Addr(), cur.Bits()+1)
		hi := netip.PrefixFrom(nextHalf(lo), cur.Bits()+1)
		if lo.Contains(hole.Addr()) {
			out, cur = append(out, hi), lo
		} else {
			out, cur = append(out, lo), hi
		}
	}
	return out, nil
}

// nextHalf is the first address after prefix p.
func nextHalf(p netip.Prefix) netip.Addr {
	a := p.Addr().As4()
	n := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	n += 1 << (32 - p.Bits())
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
}

// teamEgress lets team sandboxes open connections to anywhere except our own subnet: the
// internet (packages, images), the metadata service (169.254.169.254), DNS. Not cp, the
// gateways or other teams: ingress rules on those already refuse most of it, this also closes
// what must stay open to admins from anywhere (cp SSH, notebook L14). Replies to the
// gateways' connections need no rule: security groups are stateful.
func teamEgress(ctx *pulumi.Context, team *networking.SecGroup) error {
	subnet := netip.MustParsePrefix(topo.SubnetCIDR)
	prefixes, err := exclude(netip.MustParsePrefix("0.0.0.0/0"), subnet)
	if err != nil {
		return err
	}
	for i, p := range prefixes {
		if _, err := networking.NewSecGroupRule(ctx, fmt.Sprintf("cstam-team-egress%02d", i), &networking.SecGroupRuleArgs{
			SecurityGroupId: team.ID(),
			Direction:       pulumi.String("egress"),
			Ethertype:       pulumi.String("IPv4"),
			RemoteIpPrefix:  pulumi.String(p.String()),
			Description:     pulumi.String("team egress outside " + topo.SubnetCIDR),
		}); err != nil {
			return err
		}
	}
	_, err = networking.NewSecGroupRule(ctx, "cstam-team-egress-v6", &networking.SecGroupRuleArgs{
		SecurityGroupId: team.ID(),
		Direction:       pulumi.String("egress"),
		Ethertype:       pulumi.String("IPv6"),
		RemoteIpPrefix:  pulumi.String("::/0"),
		Description:     pulumi.String("team egress IPv6 (the subnet is IPv4 only)"),
	})
	return err
}
