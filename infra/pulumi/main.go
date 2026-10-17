// Command cstam-infra is the Pulumi program for the permanent CSTAM infrastructure on
// FelCloud. Run it with the Pulumi CLI (never `go run`):
//
//	cd infra/pulumi
//	pulumi preview      # what would change, read-only
//	pulumi up           # apply after you read the plan
//
// It manages ONLY resources it created (router, 5 security groups, VIP and
// infrastructure ports, server group, VMs, floating IPs). The FelCloud network, subnet
// and external network are looked up read-only. Team ports are never managed here:
// cpd creates, attaches and deletes them at runtime.
package main

import (
	"github.com/pulumi/pulumi-openstack/sdk/v5/go/openstack/networking"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		s, err := loadSettings(ctx)
		if err != nil {
			return err
		}
		ex, err := lookupExisting(ctx, s)
		if err != nil {
			return err
		}
		routerIf, err := newRouter(ctx, s, ex) // attached router: everything below waits for it
		if err != nil {
			return err
		}
		sg, err := newSecGroups(ctx, s)
		if err != nil {
			return err
		}
		ports, err := newPorts(ctx, s, ex, sg, routerIf)
		if err != nil {
			return err
		}
		servers, err := newServers(ctx, s, ports)
		if err != nil {
			return err
		}

		// Floating IPs. The public one is protected: DNS points at it, and getting the same
		// address back after a delete is not guaranteed.
		publicFIP, err := networking.NewFloatingIp(ctx, "cstam-public-fip", &networking.FloatingIpArgs{
			Pool:        pulumi.String(s.ExternalNetwork),
			PortId:      ports.VIP.ID(),
			FixedIp:     pulumi.String(topo.VIPIP),
			Description: pulumi.String("cstam public entry (wildcard DNS target)"),
			Tags:        s.tags(),
		}, pulumi.Protect(true), pulumi.DependsOn([]pulumi.Resource{routerIf}))
		if err != nil {
			return err
		}
		cpFIP, err := networking.NewFloatingIp(ctx, "cstam-cp-fip", &networking.FloatingIpArgs{
			Pool:        pulumi.String(s.ExternalNetwork),
			PortId:      ports.CP.ID(),
			Description: pulumi.String("cstam control plane (ssh bastion, dashboard)"),
			Tags:        s.tags(),
		}, pulumi.DependsOn([]pulumi.Resource{routerIf, servers.CP}))
		if err != nil {
			return err
		}

		// Outputs: read them with `pulumi stack output`. cpd's environment file (network,
		// subnet, host port and security group IDs) and ~/.ssh/config take their values from here.
		ctx.Export("publicFip", publicFIP.Address)
		ctx.Export("cpFip", cpFIP.Address)
		ctx.Export("networkId", pulumi.String(ex.NetworkID))
		ctx.Export("subnetId", pulumi.String(ex.SubnetID))
		ctx.Export("vipPortId", ports.VIP.ID())
		ctx.Export("hostAPortId", ports.HostA.ID())
		ctx.Export("hostAServerId", servers.HostA.ID())
		ctx.Export("teamSecGroupId", sg.Team.ID())
		ctx.Export("gwSecGroupId", sg.GW.ID())
		if ports.HostB != nil {
			ctx.Export("hostBPortId", ports.HostB.ID())
			ctx.Export("hostBServerId", servers.HostB.ID())
		}
		ctx.Export("teamNetMode", pulumi.String(s.TeamNetMode))
		return nil
	})
}
