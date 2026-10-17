# infra/pulumi: permanent CSTAM infrastructure (Pulumi, Go)

Creates on FelCloud: `cstam-router`, 5 security groups (14 rules), the VIP port and the
infrastructure ports with fixed IPs, the gateway server group, the VMs (gw1, gw2, cp,
host-a, optional host-b) booted with `deploy/cloud-init/*.yaml`, and 2 floating IPs.

It does **not** manage the network `cstam-sandbox-net`, its subnet, the external
network (all looked up read-only), or team ports (created at runtime by `cpd`).

| File | What it does |
|---|---|
| `config.go` | IP plan (`topo`) and per-stack settings with validation |
| `network.go` | read-only lookups + router and router interface |
| `secgroups.go` | the 5 groups and the whole rule table |
| `ports.go` | VIP port first (protected), gateway ports with the VIP pair, cp and host ports |
| `servers.go` | keypair, server group, instances on pre-created ports |
| `main.go` | wiring, floating IPs (public one protected), stack outputs |

## First time

```bash
cd infra/pulumi
pulumi login file://~/.pulumi-cstam          # state stays on your laptop (outside git)
export PULUMI_CONFIG_PASSPHRASE='<from your password manager>'
pulumi stack init prod
pulumi config set openstack:cloud cstam      # credentials come from clouds.yaml
pulumi config set adminCidr "$(curl -s https://ifconfig.me)/32"
pulumi config set sshPublicKeyFile ~/.ssh/cstam_ed25519.pub
pulumi preview                               # read the plan: 35 resources to create
pulumi up                                    # you run this, after reading the plan
pulumi stack output                          # publicFip, cpFip, port and server IDs
```

## Every day

- `pulumi preview` before every `pulumi up`. Never use `--yes` on prod.
- `pulumi refresh` re-reads FelCloud and shows drift (someone changed a port by hand).
- Rebuild one VM on purpose: `pulumi up --replace urn:...:compute/instance:Instance::cstam-gw2`.

## Tear down

```bash
pulumi destroy                               # everything except protected resources
pulumi state unprotect --all && pulumi destroy   # only when you really want to lose the public IP
```

Back up `~/.pulumi-cstam` (the state). If it is lost, Pulumi forgets the resources:
re-adopt them with `pulumi import` instead of creating duplicates.
