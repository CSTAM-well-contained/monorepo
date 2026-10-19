package main

import (
	"fmt"

	"github.com/pulumi/pulumi-openstack/sdk/v5/go/openstack/networking"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// SecGroups are the 5 role groups. Quota on FelCloud: 10 groups, 100 rules.
type SecGroups struct {
	GW, VRRP, CP, Host, Team *networking.SecGroup
}

// rule is one ingress rule. Exactly one of remoteIP / remoteGroup is set.
type rule struct {
	group       string // which of our groups the rule belongs to
	desc        string
	protocol    string // "tcp", "icmp", "112" (VRRP)
	portMin     int    // 0 = any port
	portMax     int
	remoteIP    string // CIDR
	remoteGroup string // name of one of our groups
}

func newSecGroups(ctx *pulumi.Context, s Settings) (*SecGroups, error) {
	mk := func(name, desc string) (*networking.SecGroup, error) {
		return networking.NewSecGroup(ctx, name, &networking.SecGroupArgs{
			Name:        pulumi.String(name),
			Description: pulumi.String(desc),
			// Keep the default egress rules (IPv4 + IPv6 allow all): apt, docker pull, NTP.
			// Team egress is restricted per namespace with nftables on the hosts.
			DeleteDefaultRules: pulumi.Bool(false),
			Tags:               s.tags(),
		})
	}
	var g SecGroups
	var err error
	if g.GW, err = mk("cstam-gw", "CSTAM gateways and VIP port"); err != nil {
		return nil, err
	}
	if g.VRRP, err = mk("cstam-vrrp", "CSTAM VRRP between gateways only"); err != nil {
		return nil, err
	}
	if g.CP, err = mk("cstam-cp", "CSTAM control plane"); err != nil {
		return nil, err
	}
	if g.Host, err = mk("cstam-host", "CSTAM hackathon hosts, primary NIC (management)"); err != nil {
		return nil, err
	}
	if g.Team, err = mk("cstam-team", "CSTAM team ports: only gateways may connect"); err != nil {
		return nil, err
	}

	byName := map[string]*networking.SecGroup{
		"cstam-gw": g.GW, "cstam-vrrp": g.VRRP, "cstam-cp": g.CP, "cstam-host": g.Host, "cstam-team": g.Team,
	}

	// The whole firewall policy of the platform, in one table. Note what is NOT here:
	//  - no rule opens a port on an agent: agents dial out to cpd on 8443;
	//  - no "self" rule on cstam-team: Neutron itself drops team-to-team traffic;
	//  - cstam-host is never a remote group (in AAP mode team IPs are members of it).
	rules := []rule{
		{group: "cstam-gw", desc: "public http", protocol: "tcp", portMin: 80, portMax: 80, remoteIP: "0.0.0.0/0"},
		{group: "cstam-gw", desc: "public https", protocol: "tcp", portMin: 443, portMax: 443, remoteIP: "0.0.0.0/0"},
		{group: "cstam-gw", desc: "ssh from cp", protocol: "tcp", portMin: 22, portMax: 22, remoteGroup: "cstam-cp"},
		{group: "cstam-gw", desc: "haproxy stats from cp", protocol: "tcp", portMin: 8404, portMax: 8404, remoteGroup: "cstam-cp"},
		{group: "cstam-gw", desc: "icmp from cp", protocol: "icmp", remoteGroup: "cstam-cp"},
		{group: "cstam-vrrp", desc: "vrrp between gateways", protocol: "112", remoteGroup: "cstam-vrrp"},
		{group: "cstam-cp", desc: "admin ssh (key-only)", protocol: "tcp", portMin: 22, portMax: 22, remoteIP: s.AdminCIDR},
		{group: "cstam-cp", desc: "admin dashboard (login + TLS)", protocol: "tcp", portMin: 443, portMax: 443, remoteIP: s.AdminCIDR},
		{group: "cstam-cp", desc: "gw agents to cp", protocol: "tcp", portMin: 8443, portMax: 8443, remoteGroup: "cstam-gw"},
		{group: "cstam-cp", desc: "host agents to cp", protocol: "tcp", portMin: 8443, portMax: 8443, remoteGroup: "cstam-host"},
		{group: "cstam-host", desc: "ssh from cp", protocol: "tcp", portMin: 22, portMax: 22, remoteGroup: "cstam-cp"},
		{group: "cstam-host", desc: "icmp from cp", protocol: "icmp", remoteGroup: "cstam-cp"},
		// team app ports: must match config.TeamPorts in internal/config/topology.go
		{group: "cstam-team", desc: "team http via gw", protocol: "tcp", portMin: 80, portMax: 80, remoteGroup: "cstam-gw"},
		{group: "cstam-team", desc: "team apps via gw", protocol: "tcp", portMin: 8000, portMax: 8099, remoteGroup: "cstam-gw"},
	}

	for i, r := range rules {
		args := &networking.SecGroupRuleArgs{
			SecurityGroupId: byName[r.group].ID(),
			Direction:       pulumi.String("ingress"),
			Ethertype:       pulumi.String("IPv4"),
			Protocol:        pulumi.String(r.protocol),
			Description:     pulumi.String(r.desc),
		}
		if r.portMin > 0 {
			args.PortRangeMin = pulumi.Int(r.portMin)
			args.PortRangeMax = pulumi.Int(r.portMax)
		}
		switch {
		case r.remoteIP != "":
			args.RemoteIpPrefix = pulumi.String(r.remoteIP)
		case r.remoteGroup != "":
			args.RemoteGroupId = byName[r.remoteGroup].ID()
		default:
			return nil, fmt.Errorf("rule %q has no remote", r.desc)
		}
		// The logical name must be unique and stable: changing it makes Pulumi replace the rule.
		name := fmt.Sprintf("%s-r%02d", r.group, i)
		if _, err := networking.NewSecGroupRule(ctx, name, args); err != nil {
			return nil, err
		}
	}
	return &g, nil
}
