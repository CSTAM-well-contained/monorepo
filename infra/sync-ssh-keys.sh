#!/usr/bin/env bash
# infra/sync-ssh-keys.sh: install every key in infra/ssh-keys/*.pub on every CSTAM VM.
#
# cloud-init installs the keys only at first boot. When a teammate joins later:
#   1. they add infra/ssh-keys/<name>.pub and commit it,
#   2. someone who can already log in runs this script.
# The VM's authorized_keys is REPLACED by the list, so removing a .pub file and re-running
# revokes that teammate's access.
set -euo pipefail
cd "$(dirname "$0")"

keys=$(cat ssh-keys/*.pub)
[ -n "$keys" ] || { echo "no keys in infra/ssh-keys" >&2; exit 1; }
# platform VMs only: on-demand hackathon hosts get the keys from cloud-init when cpd creates them
hosts=(cstam-cp cstam-gw1 cstam-gw2)

for h in "${hosts[@]}"; do
  printf '%s: ' "$h"
  # write to a temp file then rename, so a dropped connection never leaves an empty file
  printf '%s\n' "$keys" | ssh -o BatchMode=yes "$h" \
    'umask 077; cat > ~/.ssh/authorized_keys.new && mv ~/.ssh/authorized_keys.new ~/.ssh/authorized_keys && wc -l < ~/.ssh/authorized_keys'
done
