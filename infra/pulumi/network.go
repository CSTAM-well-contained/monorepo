package main

import (
	"github.com/pulumi/pulumi-openstack/sdk/v5/go/openstack/networking"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// existing holds the FelCloud resources we use but do NOT own. They are looked up
// (read-only data sources), so `pulumi destroy` can never delete them.
type existing struct {
	NetworkID, SubnetID, ExternalNetworkID string
}

func lookupExisting(ctx *pulumi.Context, s Settings) (existing, error) {
	net, err := networking.LookupNetwork(ctx, &networking.LookupNetworkArgs{Name: pulumi.StringRef(s.Network)})
	if err != nil {
		return existing{}, err
	}
	sub, err := networking.LookupSubnet(ctx, &networking.LookupSubnetArgs{
		Name:      pulumi.StringRef(s.Subnet),
		NetworkId: pulumi.StringRef(net.Id),
	})
	if err != nil {
		return existing{}, err
	}
	ext, err := networking.LookupNetwork(ctx, &networking.LookupNetworkArgs{
		Name:     pulumi.StringRef(s.ExternalNetwork),
		External: pulumi.BoolRef(true),
	})
	if err != nil {
		return existing{}, err
	}
	return existing{NetworkID: net.Id, SubnetID: sub.Id, ExternalNetworkID: ext.Id}, nil
}

// newRouter creates cstam-router with its gateway on the external network (SNAT on by default)
// and attaches our subnet to it. The router interface takes the subnet gateway IP .1.
// It returns the interface: everything that needs the Internet (VM cloud-init, floating
// IPs) must depend on the interface, not only on the router, or it can be created first.
func newRouter(ctx *pulumi.Context, s Settings, ex existing) (*networking.RouterInterface, error) {
	r, err := networking.NewRouter(ctx, "cstam-router", &networking.RouterArgs{
		Name:              pulumi.String("cstam-router"),
		ExternalNetworkId: pulumi.String(ex.ExternalNetworkID),
		// EnableSnat is NOT set: on FelCloud (default Neutron policy) only admins may send
		// enable_snat (403 PolicyNotAuthorized). SNAT is on by default for a router gateway.
		Tags:              s.tags(),
	})
	if err != nil {
		return nil, err
	}
	return networking.NewRouterInterface(ctx, "cstam-router-if", &networking.RouterInterfaceArgs{
		RouterId: r.ID(),
		SubnetId: pulumi.String(ex.SubnetID),
	})
}
