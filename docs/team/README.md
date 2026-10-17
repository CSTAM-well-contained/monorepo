# Joining the team: set up your machine

This takes about 30 minutes. At the end you can build the code, see the cloud through Pulumi,
and (once the floating IPs exist) SSH into our VMs.

**Secrets never go into git, chat channels or screenshots.** You receive one secret privately
from Ahmed (step 3). Everything else you create yourself.

Works on Ubuntu/Debian and macOS. On Windows, use WSL2 (Ubuntu) and follow the Ubuntu commands.

---

## 1. Install the tools

| Tool | Why | Check |
|---|---|---|
| git, make, jq, curl | basics | `git --version` |
| Go 1.25 | all our backend code | `go version` |
| OpenStack CLI | talk to FelCloud | `openstack --version` |
| Pulumi CLI 3.x | infrastructure as code | `pulumi version` |
| Docker | integration tests (later steps) | `docker run --rm hello-world` |
| Node.js 22 | dashboard (later steps) | `node --version` |

```bash
# Ubuntu / Debian
sudo apt update && sudo apt install -y git make jq curl pipx
# Go 1.25: https://go.dev/dl/  (then add /usr/local/go/bin and ~/go/bin to your PATH)
pipx install python-openstackclient && pipx ensurepath
curl -fsSL https://get.pulumi.com | sh            # adds ~/.pulumi/bin to your PATH; open a new terminal

# macOS (Homebrew)
brew install git make jq go pulumi pipx && pipx install python-openstackclient
```

Developer tools used by `make` (installed into `~/go/bin`):
```bash
go install github.com/zricethezav/gitleaks/v8@latest
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b "$(go env GOPATH)/bin"
```

## 2. Get the code

You need to be a member of the GitHub organization **CSTAM-well-contained** (ask Ahmed).
```bash
git clone https://github.com/CSTAM-well-contained/monorepo.git cstam
cd cstam
# secret scanner: blocks any commit that contains a password or key
printf '#!/bin/sh\nexec gitleaks protect --staged --redact -v\n' > .git/hooks/pre-commit
chmod +x .git/hooks/pre-commit
make lint test build                             # must end without errors
```

## 3. OpenStack access (the one secret you receive)

Ahmed sends you, **privately** (Signal or a password manager, never a group chat), two values:
the **application credential ID** and the **application credential secret** of our project.

Create `~/.config/openstack/clouds.yaml`:
```bash
mkdir -p ~/.config/openstack && chmod 700 ~/.config/openstack
cat > ~/.config/openstack/clouds.yaml <<'EOF'
clouds:
  cstam:
    auth_type: v3applicationcredential
    auth:
      auth_url: https://identity.api.felcloud.io
      application_credential_id: "<ID you received>"
      application_credential_secret: "<SECRET you received>"
    region_name: North-Africa
    interface: public
    identity_api_version: 3
EOF
chmod 600 ~/.config/openstack/clouds.yaml
```

Test it (read-only):
```bash
export OS_CLOUD=cstam
openstack token issue -f value -c expires                     # a date in the future
openstack server list -f value -c Name -c Status              # cstam-cp, cstam-gw1, cstam-gw2, cstam-host-a
```

## 4. Your own key for the shared Pulumi state

Pulumi keeps its state (what it created on FelCloud) in a FelCloud storage bucket,
`cstam-pulumi-state`, so the whole team sees the same thing. To read it you need an S3 key.
**Create your own**: no need to share one.
```bash
OS_CLOUD=cstam openstack ec2 credentials create -c access -c secret
```
Put the two values in `~/.aws/credentials` under a profile named `cstam`:
```bash
mkdir -p ~/.aws && chmod 700 ~/.aws
cat >> ~/.aws/credentials <<'EOF'

[cstam]
aws_access_key_id = <access>
aws_secret_access_key = <secret>
EOF
chmod 600 ~/.aws/credentials
```

## 5. Connect to Pulumi

The bucket address is already written in `infra/pulumi/Pulumi.yaml`, so Pulumi finds it alone.
Our stack config contains no secrets, so the encryption passphrase is empty. Pulumi still wants
the variable to exist; add it to your shell once:
```bash
echo 'export PULUMI_CONFIG_PASSPHRASE=""' >> ~/.bashrc     # or ~/.zshrc on macOS
source ~/.bashrc
```
Then:
```bash
cd infra/pulumi
pulumi whoami -v | grep "Backend URL"     # s3://cstam-pulumi-state?...  (if it shows something else: unset PULUMI_BACKEND_URL)
pulumi stack select prod
pulumi stack output                       # IDs and addresses of what we created
pulumi preview                            # read-only. Expected: no changes
```

**Rules for changing the cloud**
1. Change code in `infra/pulumi`, open a pull request, get a review.
2. `pulumi preview` and read every line (`+` create, `~` update, `-` delete, `+-` replace).
3. Say in the team chat that you are running `pulumi up`, then run it.
4. "the stack is currently locked": someone else is running. Wait; never `pulumi cancel` without asking them.
5. Never `pulumi destroy` or `pulumi state unprotect`: ask Ahmed first.

## 6. Your SSH key on our VMs

```bash
ssh-keygen -t ed25519 -f ~/.ssh/cstam_ed25519 -C "<your-name>@cstam" -N ''
cp ~/.ssh/cstam_ed25519.pub infra/ssh-keys/<your-name>.pub     # a PUBLIC key: safe to commit
git switch -c ssh/<your-name>
git add infra/ssh-keys/<your-name>.pub && git commit -m "ssh: add <your-name>" && git push -u origin HEAD
```
Open a pull request. Once merged, someone with access runs `infra/sync-ssh-keys.sh`, which installs
every key of `infra/ssh-keys/` on every VM.

Add to `~/.ssh/config` (the control plane is the entry point; the other VMs have no public address):
```
Host cstam-cp
    HostName <cpFip from: pulumi stack output cpFip>
    User ubuntu
    IdentityFile ~/.ssh/cstam_ed25519
Host cstam-gw1
    HostName 10.20.0.11
Host cstam-gw2
    HostName 10.20.0.12
Host cstam-host-a
    HostName 10.20.0.21
Host cstam-gw1 cstam-gw2 cstam-host-a
    User ubuntu
    IdentityFile ~/.ssh/cstam_ed25519
    ProxyJump cstam-cp
```
Test: `ssh cstam-cp hostname`, then `ssh cstam-gw1 hostname`.

> As of 17 October 2026 the floating IPs are not created yet (FelCloud has no free public IP),
> so `cpFip` is empty and SSH is not possible. Everything else above works.

## 7. Claude Code (optional)

`CLAUDE.md` at the repository root is read automatically: it holds our rules. If you want Claude
Code to run Pulumi for you, create `.claude/settings.local.json` (git-ignored, personal):
```json
{
  "permissions": { "allow": ["Bash(pulumi *)"] },
  "autoMode": {
    "allow": ["$defaults", "The user authorized Claude to run Pulumi (preview, up, refresh, stack output, config) for infra/pulumi against our FelCloud OpenStack project."]
  }
}
```

---

## Checklist

- [ ] `make lint test build` passes
- [ ] `openstack server list` shows our 4 VMs
- [ ] `pulumi preview` in `infra/pulumi` says no changes
- [ ] your key is in `infra/ssh-keys/` (merged)
- [ ] `ssh cstam-gw1 hostname` works (once floating IPs exist)

Then read [`../README.md`](../README.md) for the reading order of the docs.

## Troubleshooting

| Message | Cause | Fix |
|---|---|---|
| `The request you have made requires authentication (HTTP 401)` | wrong or mistyped credential in `clouds.yaml` | re-copy ID and secret; no spaces or quotes inside the values |
| `NoCredentialProviders` or `AccessDenied` from Pulumi | missing `[cstam]` profile in `~/.aws/credentials` | redo step 4 |
| `passphrase must be set` | `PULUMI_CONFIG_PASSPHRASE` not exported | redo step 5, open a new terminal |
| `Backend URL: file://...` or another bucket | `PULUMI_BACKEND_URL` is set in your environment | `unset PULUMI_BACKEND_URL` (and remove it from your shell config) |
| `the stack is currently locked` | a teammate is running `pulumi up` | wait and ask in the chat |
| `Permission denied (publickey)` on SSH | your key is not on the VMs yet | step 6: merged + `sync-ssh-keys.sh` run |
| `ExternalIpAddressExhausted` | FelCloud has no free public IP | not your fault: tell Ahmed |
