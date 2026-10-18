package main

import (
	"github.com/pulumi/pulumi-openstack/sdk/v5/go/openstack/networking"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// Ports are pre-created with fixed IPs so that (1) IPs are predictable for keepalived
// and the firewall, (2) security groups are set before the VM boots (the "default"
// group is never attached), (3) gateway ports list the VIP from the start.
type Ports struct {
	VIP, GW1, GW2, CP *networking.Port
}

func newPorts(ctx *pulumi.Context, s Settings, ex existing, sg *SecGroups, routerIf pulumi.Resource) (*Ports, error) {
	type spec struct {
		name, ip, desc string
		groups         []*networking.SecGroup
		pairs          []string // allowed-address-pairs (exact IPs or CIDRs)
		extra          []pulumi.ResourceOption
	}
	mk := func(sp spec) (*networking.Port, error) {
		ids := pulumi.StringArray{}
		for _, g := range sp.groups {
			ids = append(ids, g.ID())
		}
		pairs := networking.PortAllowedAddressPairArray{}
		for _, p := range sp.pairs {
			pairs = append(pairs, networking.PortAllowedAddressPairArgs{IpAddress: pulumi.String(p)})
		}
		opts := append([]pulumi.ResourceOption{pulumi.DependsOn([]pulumi.Resource{routerIf})}, sp.extra...)
		return networking.NewPort(ctx, sp.name, &networking.PortArgs{
			Name:      pulumi.String(sp.name),
			NetworkId: pulumi.String(ex.NetworkID),
			FixedIps: networking.PortFixedIpArray{networking.PortFixedIpArgs{
				SubnetId:  pulumi.String(ex.SubnetID),
				IpAddress: pulumi.String(sp.ip),
			}},
			SecurityGroupIds:    ids,
			AllowedAddressPairs: pairs,
			Description:         pulumi.String(sp.desc),
			Tags:                s.tags(),
		}, opts...)
	}

	var p Ports
	var err error

	// 1. The VIP first: a port owned by no VM. OVN turns it into a "virtual" port as soon
	//    as its IP appears in the allowed-address-pairs of the gateway ports below.
	//    Protected: deleting it would also break the floating IP that DNS points to.
	if p.VIP, err = mk(spec{name: "cstam-vip", ip: topo.VIPIP, desc: "VRRP VIP - never attach to a server",
		groups: []*networking.SecGroup{sg.GW}, extra: []pulumi.ResourceOption{pulumi.Protect(true)}}); err != nil {
		return nil, err
	}
	// 2. Gateway ports list the VIP (exact IP, not a CIDR) and depend on the VIP port.
	after := pulumi.DependsOn([]pulumi.Resource{p.VIP})
	gwGroups := []*networking.SecGroup{sg.GW, sg.VRRP}
	if p.GW1, err = mk(spec{name: "cstam-gw1-port", ip: topo.GW1IP, desc: "gateway 1",
		groups: gwGroups, pairs: []string{topo.VIPIP}, extra: []pulumi.ResourceOption{after}}); err != nil {
		return nil, err
	}
	if p.GW2, err = mk(spec{name: "cstam-gw2-port", ip: topo.GW2IP, desc: "gateway 2",
		groups: gwGroups, pairs: []string{topo.VIPIP}, extra: []pulumi.ResourceOption{after}}); err != nil {
		return nil, err
	}
	if p.CP, err = mk(spec{name: "cstam-cp-port", ip: topo.CPIP, desc: "control plane",
		groups: []*networking.SecGroup{sg.CP}}); err != nil {
		return nil, err
	}

	// No host ports here: hackathon hosts are created on demand by cpd (see main.go).
	return &p, nil
}
