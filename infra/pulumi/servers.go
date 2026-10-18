package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pulumi/pulumi-openstack/sdk/v5/go/openstack/compute"
	"github.com/pulumi/pulumi-openstack/sdk/v5/go/openstack/images"
	"github.com/pulumi/pulumi-openstack/sdk/v5/go/openstack/networking"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// Servers are the 3 always-on VMs. Hackathon hosts are not here: cpd creates them on demand.
type Servers struct {
	GW1, GW2, CP *compute.Instance
}

// cloudInitDir is relative to this Pulumi project (infra/pulumi).
const cloudInitDir = "../../deploy/cloud-init"

func newServers(ctx *pulumi.Context, s Settings, p *Ports) (*Servers, error) {
	// No Nova keypair: a keypair holds one key, and the team has several. cloud-init puts
	// every key from infra/ssh-keys into ~ubuntu/.ssh/authorized_keys at first boot.
	// Keys added later are pushed with infra/sync-ssh-keys.sh (user data never changes).
	var keys strings.Builder
	keys.WriteString("\nssh_authorized_keys:\n")
	for _, k := range s.SSHPublicKeys {
		fmt.Fprintf(&keys, "  - %s\n", k)
	}

	// Gateways in different hypervisors. FelCloud hides the hypervisor list, so if the
	// second gateway fails with "No valid host was found", switch the stack config to
	// soft-anti-affinity (Step 2 pitfalls) and run `pulumi up` again.
	sgroup, err := compute.NewServerGroup(ctx, "cstam-gw-ag", &compute.ServerGroupArgs{
		Name:     pulumi.String("cstam-gw-ag"),
		Policies: pulumi.String(s.ServerGroupPolicy),
	})
	if err != nil {
		return nil, err
	}

	var imageID string
	if s.BootVolumeGB > 0 {
		img, err := images.LookupImage(ctx, &images.LookupImageArgs{Name: pulumi.StringRef(s.Image), MostRecent: pulumi.BoolRef(true)})
		if err != nil {
			return nil, err
		}
		imageID = img.Id
	}

	boot := func(name, flavor string, port *networking.Port, userData, role string, group pulumi.IDOutput) (*compute.Instance, error) {
		ud, err := os.ReadFile(filepath.Join(cloudInitDir, userData))
		if err != nil {
			return nil, fmt.Errorf("%s: read cloud-init: %w", name, err)
		}
		args := &compute.InstanceArgs{
			Name:       pulumi.String(name),
			FlavorName: pulumi.String(flavor),
			UserData:   pulumi.String(strings.TrimRight(string(ud), "\n") + "\n" + keys.String()),
			// Boot on the pre-created port: no "default" security group, fixed IP.
			// Do NOT set SecurityGroups here: the port carries them.
			Networks: compute.InstanceNetworkArray{compute.InstanceNetworkArgs{Port: port.ID()}},
			Metadata: pulumi.StringMap{"cstam-role": pulumi.String(role)},
			Tags:     s.tags("cstam-role:" + role),
		}
		if s.BootVolumeGB > 0 {
			args.BlockDevices = compute.InstanceBlockDeviceArray{compute.InstanceBlockDeviceArgs{
				SourceType:          pulumi.String("image"),
				Uuid:                pulumi.String(imageID),
				DestinationType:     pulumi.String("volume"),
				VolumeSize:          pulumi.Int(s.BootVolumeGB),
				BootIndex:           pulumi.Int(0),
				DeleteOnTermination: pulumi.Bool(true),
			}}
		} else {
			args.ImageName = pulumi.String(s.Image)
		}
		if group != (pulumi.IDOutput{}) {
			args.SchedulerHints = compute.InstanceSchedulerHintArray{compute.InstanceSchedulerHintArgs{Group: group}}
		}
		// Editing a cloud-init file must not silently rebuild a running VM (user data is
		// immutable in Nova, so a change would mean delete + create). Rebuild on purpose with
		// `pulumi up --replace <urn>` when you really want it.
		return compute.NewInstance(ctx, name, args,
			pulumi.IgnoreChanges([]string{"userData", "imageName", "imageId", "blockDevices"}))
	}

	var sv Servers
	if sv.GW1, err = boot("cstam-gw1", s.GWFlavor, p.GW1, "gateway.yaml", "gateway", sgroup.ID()); err != nil {
		return nil, err
	}
	if sv.GW2, err = boot("cstam-gw2", s.GWFlavor, p.GW2, "gateway.yaml", "gateway", sgroup.ID()); err != nil {
		return nil, err
	}
	if sv.CP, err = boot("cstam-cp", s.CPFlavor, p.CP, "controlplane.yaml", "controlplane", pulumi.IDOutput{}); err != nil {
		return nil, err
	}
	return &sv, nil
}
