# Isolation lab on FelCloud (no floating IP needed)

Proves on the real Neutron that team sandboxes cannot reach each other or the management layer,
while the gateways reach the teams. Results: `docs/official/phase2.md`, notebook L14.

Three throwaway VMs in the lab range (CLAUDE.md allows manual `cstam-lab-*` resources, tagged
`cstam-lab`): A and B in `cstam-team`, C in `cstam-gw`. A and C probe at boot and write the
result table on their login screen (`/etc/issue`); read it with the noVNC console
(`openstack console url show cstam-lab-iso-a`). FelCloud keeps no serial console log.

```bash
source infra/topology.env
SUB_ID=$(openstack subnet show $SUB -f value -c id)
mk() { # name ip group user-data
  openstack port create --network $NET --fixed-ip subnet=$SUB_ID,ip-address=$2 \
    --security-group $3 --tag cstam-lab cstam-lab-iso-$1 -f value -c id >/tmp/port-$1
  openstack --os-compute-api-version 2.52 server create --flavor G0.basic.1c1g \
    --image "Debian 12 - Bookworm" --port $(cat /tmp/port-$1) --user-data $4 \
    --tag cstam-lab cstam-lab-iso-$1
}
mk b 10.20.0.193 cstam-team deploy/lab/iso-target.yaml
mk a 10.20.0.192 cstam-team deploy/lab/iso-probe.yaml
mk c 10.20.0.194 cstam-gw   deploy/lab/iso-probe.yaml
# about 5 minutes later: look at the noVNC screens of a and c, then clean up
openstack server delete --wait cstam-lab-iso-a cstam-lab-iso-b cstam-lab-iso-c
for v in a b c; do openstack port delete cstam-lab-iso-$v; done
```
Check the quota first (`openstack server list`, 10 instances for the whole project).
