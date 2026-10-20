# Phase 2 video: recording script (max. 3 minutes)

**Automatic version (used for the submission, 2:31, 1080p):** `deploy/video/make-video.ps1`
records it with no screen recorder: it runs `deploy/dev/video.sh` on the real platform, captures
the live dashboard with headless Edge, composes each frame (terminal, dashboard, subtitles,
`deploy/video/stage.html`), reads the narration with a natural voice (Edge online voices, default `-Voice "Ava Multilingual"`; the offline Windows voice is the fallback) and encodes the MP4
with Windows' own video engine. On Windows, after `QUARANTINE=10s GW_MODE=systemd
deploy/dev/demo.sh up` and `deploy/dev/demo.sh teams 3` in WSL:
```powershell
powershell -ExecutionPolicy Bypass -File \\wsl.localhost\Debian\home\lenovo\cstam\deploy\video\make-video.ps1
```
The output goes to `Videos\CSTAM-phase2-demo.mp4` (`-Out` to change). The narration text is the
`$scenes` list in that script.

The manual version below is for recording yourself with your own voice.

The demo runs by itself (`deploy/dev/video.sh`, about 70 s of action); you press **Enter** to go
to the next step while you speak. Screen: terminal on the left half, the dashboard on the right.

## Before recording (5 minutes)
1. Windows Terminal, `wsl -d Debian`, then:
   ```bash
   cd ~/cstam && git pull --rebase && make build
   deploy/dev/demo.sh down
   QUARANTINE=10s GW_MODE=systemd deploy/dev/demo.sh up     # about 1 minute
   deploy/dev/demo.sh teams 3                               # a few teams already running
   cat /run/user/1000/cstam-demo/admin.token                # copy it
   ```
   `QUARANTINE=10s` shortens the IP quarantine (60 s in production) so recycling fits in the
   video; the script says so on screen.
2. Edge: `http://localhost:8443/`, paste the token. Zoom the page to 80-90 % so the Overview fits.
3. Second Edge tab: the isolation screenshots,
   `https://github.com/CSTAM-well-contained/monorepo/blob/main/docs/official/phase2.md#3-network-isolation-and-access-safeguards-5`.
4. Terminal font large (Ctrl + = twice), `clear`, then type `deploy/dev/video.sh` **without** Enter.
5. Optional dry run: `VIDEO_AUTO=1 deploy/dev/video.sh` (each run uses a new hackathon name).

## Record
Windows 11 Snipping Tool: **Win + Shift + R**, select the whole screen, turn the microphone on,
**Start**. Stop with the red button: the MP4 is saved in `Videos\Screen Recordings`. If it is
longer than 3:00, trim it in Clipchamp (preinstalled): drag the clip ends, Export 1080p.

## Storyboard

| Time | Press / click | Say (short version) |
|---|---|---|
| 0:00 | Dashboard **Overview**. Press Enter in the terminal (script starts, first banner) | "This is CSTAM, our Resilient IP Optimizer for FelCloud. Two HA gateways, gw1 is MASTER, gw2 is BACKUP, and a control plane that provisions sandboxes, leases private IPs and routes subdomains. Here is Phase 2: stress, teardown and isolation." |
| 0:15 | Enter (**1/4 stress**). Watch the **IP pool** grid on Overview fill up | "Twenty teams register at the same time. Each one gets a private IP from our IPAM, a sandbox, and a route on both gateways. A team counts as done only when its own page answers through the public VIP." |
| 0:35 | (results appear) Click **Teams** | "Twenty out of twenty, no error, twenty distinct IPs and slots, p95 around three seconds. In our benchmark, fifty at once: fifty out of fifty." |
| 0:50 | Click **Traffic**, then Enter (**2/4 flood**) | "Now one team floods the gateway as fast as it can while the nineteen others get normal traffic. The flooded team is refused with 429; the others: zero errors, p99 about two milliseconds. The gateway management endpoint is hidden from outside: 404." |
| 1:15 | Enter (**isolation on FelCloud**). Switch to the GitHub tab with the screenshots | "Isolation between sandboxes is enforced by Neutron security groups on FelCloud. We measured it on FelCloud with test VMs: from a team VM, other teams, the gateways' management and the control plane are all dropped; from a gateway, the team page answers. The test found that a sandbox could reach the control plane's SSH; we fixed it with egress rules and measured again." |
| 1:40 | Back to the dashboard **Overview**, Enter (**3/4 lease**) | "Automated teardown. These three teams have a 12-second lease. Nobody touches anything: when it expires, the route is removed from both gateways, the sandbox is deleted, and the subdomain answers 404." |
| 2:05 | Enter (**3/4 hackathon end**). Watch the IP grid: blue, then yellow, then grey | "Ending the hackathon purges every sandbox and route at once. The IPs go to quarantine, yellow, and come back to the pool only after both gateways confirm the route is gone. Ten seconds for this video, sixty in production, so a recycled IP can never receive another team's traffic." |
| 2:35 | Enter (**4/4 benchmark**) | "The benchmark report has all the numbers: provisioning latency, failover in 0.3 seconds when planned and under 2 seconds on a crash, IP recycling, and 3,000 requests per second with zero errors. Code, benchmark report and security documentation are in the repository. Thank you." |
| 2:55 | Stop recording | |

## If something goes wrong
- A step fails: stop, `deploy/dev/demo.sh status`, then run `deploy/dev/video.sh` again (new
  hackathon name each run). Nothing needs cleaning between takes.
- The dashboard says "offline": `deploy/dev/demo.sh up` again (it keeps what is running).
- Too long: speak shorter on 0:50 and 1:15, or trim the waiting parts in Clipchamp.
