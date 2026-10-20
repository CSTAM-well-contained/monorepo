<#
make-video.ps1: records the Phase 2 video (MP4, 1080p) with no screen recorder and no download.

  - deploy/dev/video.sh runs the real demo in WSL; this script paces it (VIDEO_GATE/VIDEO_MARK);
  - headless Edge (DevTools protocol) shows the live dashboard; every frame is the stage page
    (stage.html: terminal + dashboard + subtitles) captured at -Fps;
  - Windows text-to-speech reads the narration of each scene;
  - Windows.Media.Editing (the engine of the Photos app) encodes frames + voice to MP4.

Before: in WSL, `QUARANTINE=10s GW_MODE=systemd deploy/dev/demo.sh up` and `deploy/dev/demo.sh teams 3`.
Run (Windows PowerShell 5.1):
  powershell -ExecutionPolicy Bypass -File \\wsl.localhost\Debian\home\lenovo\cstam\deploy\video\make-video.ps1
#>
param(
  [string]$Distro = "Debian",
  [string]$User = "lenovo",
  [string]$Out = "$env:USERPROFILE\Videos\CSTAM-phase2-demo.mp4",
  [double]$Fps = 2,
  [int]$VoiceRate = 1,
  [int]$Bitrate = 4000000
)
$ErrorActionPreference = "Stop"
Add-Type -AssemblyName System.Speech, System.Runtime.WindowsRuntime

$repo = "\\wsl.localhost\$Distro\home\$User\cstam"
$wslTmp = "\\wsl.localhost\$Distro\tmp"
$work = Join-Path $env:TEMP "cstam-video"
Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force "$work\frames" | Out-Null
Copy-Item "$repo\deploy\video\stage.html" "$work\stage.html"
function Log($m) { Write-Output ("{0:HH:mm:ss} {1}" -f (Get-Date), $m) }

# ---------------------------------------------------------------- scenes and narration
$img = "$repo\docs\official\img"
function DataUrl($p) { "data:image/png;base64," + [Convert]::ToBase64String([IO.File]::ReadAllBytes($p)) }
$scenes = @(
  @{ label = "Introduction"; tab = "overview"; text = "This is CSTAM, our Resilient IP Optimizer for FelCloud. Two high availability gateways share a virtual IP: gateway one is master, gateway two is backup. A control plane provisions sandboxes, leases private IPs, and routes each team's subdomain. This video shows Phase 2: the stress test, automated teardown, and isolation." },
  @{ label = "1/4  Stress test"; tab = "overview"; text = "First, the stress test. Twenty teams register at the same time. Each one gets a private IP from our IP address manager, a sandbox, and a route on both gateways. A team only counts when its own page answers through the public virtual IP. Result: twenty out of twenty, no errors, twenty distinct addresses." },
  @{ label = "2/4  Isolation on the gateways"; tab = "traffic"; text = "Next, isolation on the gateways. One team floods the gateway as fast as it can, while the nineteen others receive normal traffic. The flooded team is refused with status 429. The other teams see zero errors, with a p99 latency of about two milliseconds. The gateway's management endpoint stays hidden from outside." },
  @{ label = "2/4  Isolation between sandboxes"; shots = @(
      @{ caption = "On FelCloud, from a team VM: other teams, gateway management and control plane are all dropped (FILTERED)"; src = (DataUrl "$img\iso-team-after.png") },
      @{ caption = "From a gateway VM: the team page answers (OPEN), management ports stay closed"; src = (DataUrl "$img\iso-gateway.png") });
     text = "Isolation between sandboxes is enforced by Neutron security groups on FelCloud. We measured it there with test virtual machines. From a team machine, the other teams, the gateway management, and the control plane are all dropped. From a gateway, the team page answers. The test also found that a sandbox could reach the control plane's SSH port, so we added egress rules, and measured again." },
  @{ label = "3/4  Automated teardown: lease expiry"; tab = "teams"; text = "Now, automated teardown. These three teams have a twelve second lease. Nobody touches anything. When the lease expires, the route is removed from both gateways, the sandbox is deleted, and the subdomain answers 404." },
  @{ label = "3/4  Hackathon end and IP recycling"; tab = "ipam"; text = "Ending the hackathon purges every sandbox and route. The addresses wait in quarantine, in yellow, until both gateways confirm the route is gone, then return to the pool. Ten seconds for this video, sixty in production, so a recycled address never receives another team's traffic." },
  @{ label = "4/4  Benchmark report"; tab = "overview"; text = "The benchmark report has all the numbers: provisioning latency, failover in 0.3 seconds when planned and under two seconds on a crash, IP recycling, and three thousand requests per second with zero errors. The code, the benchmark report, and the security documentation are in our repository. Thank you." }
)

Log "narration (text to speech)"
$rateHz = 22050
$fmt = New-Object System.Speech.AudioFormat.SpeechAudioFormatInfo($rateHz, [System.Speech.AudioFormat.AudioBitsPerSample]::Sixteen, [System.Speech.AudioFormat.AudioChannel]::Mono)
function PcmOf($wav) { # the data chunk of a WAV file
  $b = [IO.File]::ReadAllBytes($wav); $i = 12
  while ($i -lt $b.Length - 8) {
    $id = [Text.Encoding]::ASCII.GetString($b, $i, 4); $n = [BitConverter]::ToInt32($b, $i + 4)
    if ($id -eq "data") { $d = New-Object byte[] $n; [Array]::Copy($b, $i + 8, $d, 0, $n); return ,$d }
    $i += 8 + $n
  }
  throw "no data chunk in $wav"
}
for ($k = 0; $k -lt $scenes.Count; $k++) {
  $tts = New-Object System.Speech.Synthesis.SpeechSynthesizer
  $v = $tts.GetInstalledVoices() | Where-Object { $_.VoiceInfo.Culture.Name -like "en-*" } | Select-Object -First 1
  if ($v) { $tts.SelectVoice($v.VoiceInfo.Name) }
  $tts.Rate = $VoiceRate
  $tts.SetOutputToWaveFile("$work\voice$k.wav", $fmt); $tts.Speak($scenes[$k].text); $tts.Dispose()
  $scenes[$k].pcm = PcmOf "$work\voice$k.wav"
  $scenes[$k].secs = $scenes[$k].pcm.Length / (2.0 * $rateHz)
  Log ("  scene {0}: {1:N1} s" -f $k, $scenes[$k].secs)
}

# ---------------------------------------------------------------- headless Edge
$edge = "${env:ProgramFiles(x86)}\Microsoft\Edge\Application\msedge.exe"
if (-not (Test-Path $edge)) { $edge = "$env:ProgramFiles\Microsoft\Edge\Application\msedge.exe" }
$port = 9341
$edgeProc = Start-Process $edge -PassThru -ArgumentList "--headless=new", "--remote-debugging-port=$port",
  "--user-data-dir=$work\edge", "--window-size=1920,1080", "--hide-scrollbars", "--force-device-scale-factor=1",
  "--disable-background-timer-throttling", "--disable-renderer-backgrounding", "--disable-backgrounding-occluded-windows", "about:blank"
$script:ids = @{}
function Connect($wsUrl) {
  $ws = New-Object System.Net.WebSockets.ClientWebSocket
  $ws.Options.KeepAliveInterval = [TimeSpan]::FromSeconds(30)
  $ws.ConnectAsync([Uri]$wsUrl, [Threading.CancellationToken]::None).Wait()
  $script:ids[$ws.GetHashCode()] = 0
  return $ws
}
function Send($ws, $method, $params) {
  $h = $ws.GetHashCode(); $script:ids[$h]++; $id = $script:ids[$h]
  $bytes = [Text.Encoding]::UTF8.GetBytes((@{ id = $id; method = $method; params = $params } | ConvertTo-Json -Depth 10 -Compress))
  $ws.SendAsync([ArraySegment[byte]]$bytes, "Text", $true, [Threading.CancellationToken]::None).Wait()
  while ($true) {
    $ms = New-Object IO.MemoryStream
    do { $buf = New-Object byte[] 1048576; $r = $ws.ReceiveAsync([ArraySegment[byte]]$buf, [Threading.CancellationToken]::None).Result; $ms.Write($buf, 0, $r.Count) } while (-not $r.EndOfMessage)
    $o = [Text.Encoding]::UTF8.GetString($ms.ToArray()) | ConvertFrom-Json
    if ($o.id -eq $id) {
      if ($o.error) { throw "$method failed: $($o.error.message)" }
      return $o.result
    }
  }
}
try {
  $ver = $null
  for ($i = 0; $i -lt 40 -and -not $ver; $i++) { Start-Sleep -Milliseconds 250; try { $ver = Invoke-RestMethod "http://127.0.0.1:$port/json/version" } catch {} }
  $browser = Connect $ver.webSocketDebuggerUrl
  # two windows, so both pages count as visible (the dashboard stops polling when hidden)
  $stageId = (Send $browser "Target.createTarget" @{ url = "about:blank"; newWindow = $true }).targetId
  $dashId = (Send $browser "Target.createTarget" @{ url = "about:blank"; newWindow = $true }).targetId
  $targets = Invoke-RestMethod "http://127.0.0.1:$port/json"
  $stage = Connect ($targets | Where-Object { $_.id -eq $stageId }).webSocketDebuggerUrl
  $dash = Connect ($targets | Where-Object { $_.id -eq $dashId }).webSocketDebuggerUrl
  [void](Send $stage "Emulation.setDeviceMetricsOverride" @{ width = 1920; height = 1080; deviceScaleFactor = 1; mobile = $false })
  [void](Send $dash "Emulation.setDeviceMetricsOverride" @{ width = 1240; height = 976; deviceScaleFactor = 1; mobile = $false })
  [void](Send $dash "Emulation.setFocusEmulationEnabled" @{ enabled = $true })
  [void](Send $stage "Page.navigate" @{ url = ([Uri]"$work\stage.html").AbsoluteUri })

  $token = (wsl -d $Distro -e cat "/run/user/1000/cstam-demo/admin.token").Trim()
  [void](Send $dash "Page.navigate" @{ url = "http://localhost:8443/" })
  Start-Sleep -Seconds 2
  [void](Send $dash "Runtime.evaluate" @{ expression = "sessionStorage.setItem('cstam.token', '$token'); location.reload(); 1" })
  Start-Sleep -Seconds 3
  $hidden = (Send $dash "Runtime.evaluate" @{ expression = "document.hidden"; returnByValue = $true }).result.value
  Log "dashboard ready (document.hidden=$hidden)"
  function Tab($name) { if ($name) { [void](Send $dash "Runtime.evaluate" @{ expression = "document.querySelector('button[data-tab=$name]').click(); window.scrollTo(0,0); 1" }) } }

  # ---------------------------------------------------------------- the demo, paced by us
  $runner = "cd ~/cstam`necho 0 >/tmp/cstam-vgate`nrm -f /tmp/cstam-vmark /tmp/cstam-vout`nVIDEO_GATE=/tmp/cstam-vgate VIDEO_MARK=/tmp/cstam-vmark deploy/dev/video.sh >/tmp/cstam-vout 2>&1`n"
  [IO.File]::WriteAllText("$wslTmp\cstam-vrun.sh", $runner, (New-Object Text.UTF8Encoding $false))
  $demo = Start-Process wsl.exe -ArgumentList "-d $Distro -e bash -l /tmp/cstam-vrun.sh" -WindowStyle Hidden -PassThru
  function ReadInt($p) { try { [int]((Get-Content -Raw $p -ErrorAction Stop).Trim()) } catch { 0 } }
  function ReadOut() { try { $t = [IO.File]::ReadAllText("$wslTmp\cstam-vout"); if ($t.Length -gt 6000) { $t.Substring($t.Length - 6000) } else { $t } } catch { "" } }

  $frames = New-Object Collections.Generic.List[object]
  $starts = @(0.0)
  $k = 0; Tab $scenes[0].tab
  $sw = [Diagnostics.Stopwatch]::StartNew()
  $tick = 1.0 / $Fps; $n = 0
  Log "recording"
  while ($true) {
    $now = $sw.Elapsed.TotalSeconds
    $mark = ReadInt "$wslTmp\cstam-vmark"
    if ($mark -ge $k + 1 -and $now - $starts[$k] -ge $scenes[$k].secs + 1.2) {
      if ($k -eq $scenes.Count - 1) { break }
      $k++; $starts += $now
      [IO.File]::WriteAllText("$wslTmp\cstam-vgate", "$k`n")
      Tab $scenes[$k].tab
      Log ("scene {0} at {1:N1} s" -f $k, $now)
    }
    if ($now -gt 400) { throw "the demo did not finish in 400 s (see /tmp/cstam-vout in WSL)" }
    $s = $scenes[$k]
    $state = @{ scene = $s.label; sub = $s.text; term = (ReadOut); rows = 44 }
    if ($s.shots) { $state.shots = $s.shots; $state.label = "FelCloud: noVNC consoles of the isolation lab VMs (measured 2026-10-20)" }
    else {
      $shot = Send $dash "Page.captureScreenshot" @{ format = "jpeg"; quality = 85; clip = @{ x = 0; y = 0; width = 1240; height = 976; scale = 0.85 } }
      $state.dash = "data:image/jpeg;base64," + $shot.data
      $state.label = "admin dashboard, live (served by cpd): " + $s.tab
    }
    $json = $state | ConvertTo-Json -Compress -Depth 5
    [void](Send $stage "Runtime.evaluate" @{ expression = "update($json)"; awaitPromise = $true })
    $frame = Send $stage "Page.captureScreenshot" @{ format = "jpeg"; quality = 88 }
    $file = "$work\frames\f{0:D5}.jpg" -f $n
    [IO.File]::WriteAllBytes($file, [Convert]::FromBase64String($frame.data))
    $frames.Add(@{ file = $file; t = $now }); $n++
    $wait = ($n * $tick) - $sw.Elapsed.TotalSeconds
    if ($wait -gt 0) { Start-Sleep -Milliseconds ([int]($wait * 1000)) }
  }
  $end = $sw.Elapsed.TotalSeconds + 1.5
  Log ("recorded {0} frames, {1:N1} s" -f $frames.Count, $end)
} finally {
  Stop-Process -Id $edgeProc.Id -Force -ErrorAction SilentlyContinue
}

# ---------------------------------------------------------------- audio track
$total = [int][Math]::Ceiling($end * $rateHz) * 2
$pcm = New-Object byte[] $total
for ($k = 0; $k -lt $starts.Count; $k++) {
  $off = [int]($starts[$k] * $rateHz) * 2
  $len = [Math]::Min($scenes[$k].pcm.Length, $total - $off)
  [Array]::Copy($scenes[$k].pcm, 0, $pcm, $off, $len)
}
$wavPath = "$work\narration.wav"
$fs = [IO.File]::Create($wavPath); $bw = New-Object IO.BinaryWriter $fs
$bw.Write([Text.Encoding]::ASCII.GetBytes("RIFF")); $bw.Write([int](36 + $pcm.Length)); $bw.Write([Text.Encoding]::ASCII.GetBytes("WAVEfmt "))
$bw.Write([int]16); $bw.Write([int16]1); $bw.Write([int16]1); $bw.Write([int]$rateHz); $bw.Write([int]($rateHz * 2)); $bw.Write([int16]2); $bw.Write([int16]16)
$bw.Write([Text.Encoding]::ASCII.GetBytes("data")); $bw.Write([int]$pcm.Length); $bw.Write($pcm); $bw.Close()

# ---------------------------------------------------------------- encode (Windows.Media.Editing)
Log "encoding $($frames.Count) frames"
$asTask = [System.WindowsRuntimeSystemExtensions].GetMethods() | Where-Object { $_.Name -eq 'AsTask' -and $_.GetParameters().Count -eq 1 -and $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncOperation`1' } | Select-Object -First 1
$asTaskP = [System.WindowsRuntimeSystemExtensions].GetMethods() | Where-Object { $_.Name -eq 'AsTask' -and $_.GetParameters().Count -eq 1 -and $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncOperationWithProgress`2' } | Select-Object -First 1
function Await($op, [Type]$t) { $task = $asTask.MakeGenericMethod($t).Invoke($null, @($op)); $task.Wait(); $task.Result }
[void][Windows.Storage.StorageFile, Windows.Storage, ContentType = WindowsRuntime]
[void][Windows.Storage.StorageFolder, Windows.Storage, ContentType = WindowsRuntime]
[void][Windows.Media.Editing.MediaComposition, Windows.Media.Editing, ContentType = WindowsRuntime]
[void][Windows.Media.Editing.MediaClip, Windows.Media.Editing, ContentType = WindowsRuntime]
[void][Windows.Media.Editing.BackgroundAudioTrack, Windows.Media.Editing, ContentType = WindowsRuntime]
[void][Windows.Media.MediaProperties.MediaEncodingProfile, Windows.Media.MediaProperties, ContentType = WindowsRuntime]
[void][Windows.Media.Transcoding.TranscodeFailureReason, Windows.Media.Transcoding, ContentType = WindowsRuntime]
$addClip = [System.Collections.Generic.ICollection[Windows.Media.Editing.MediaClip]].GetMethod("Add")
$addAudio = [System.Collections.Generic.ICollection[Windows.Media.Editing.BackgroundAudioTrack]].GetMethod("Add")

$comp = New-Object Windows.Media.Editing.MediaComposition
for ($i = 0; $i -lt $frames.Count; $i++) {
  $next = if ($i + 1 -lt $frames.Count) { $frames[$i + 1].t } else { $end }
  $dur = [TimeSpan]::FromSeconds([Math]::Max(0.05, $next - $frames[$i].t))
  if ($i -eq 0) { $dur = [TimeSpan]::FromSeconds($frames[1].t) } # the first frame covers t=0
  $f = Await ([Windows.Storage.StorageFile]::GetFileFromPathAsync($frames[$i].file)) ([Windows.Storage.StorageFile])
  $clip = Await ([Windows.Media.Editing.MediaClip]::CreateFromImageFileAsync($f, $dur)) ([Windows.Media.Editing.MediaClip])
  [void]$addClip.Invoke($comp.Clips, @($clip))
}
$wf = Await ([Windows.Storage.StorageFile]::GetFileFromPathAsync($wavPath)) ([Windows.Storage.StorageFile])
[void]$addAudio.Invoke($comp.BackgroundAudioTracks, @((Await ([Windows.Media.Editing.BackgroundAudioTrack]::CreateFromFileAsync($wf)) ([Windows.Media.Editing.BackgroundAudioTrack]))))

$outDir = Split-Path $Out -Parent; New-Item -ItemType Directory -Force $outDir | Out-Null
$folder = Await ([Windows.Storage.StorageFolder]::GetFolderFromPathAsync($outDir)) ([Windows.Storage.StorageFolder])
$outFile = Await ($folder.CreateFileAsync((Split-Path $Out -Leaf), [Windows.Storage.CreationCollisionOption]::ReplaceExisting)) ([Windows.Storage.StorageFile])
$profile = [Windows.Media.MediaProperties.MediaEncodingProfile]::CreateMp4([Windows.Media.MediaProperties.VideoEncodingQuality]::HD1080p)
$profile.Video.Bitrate = $Bitrate
$op = $comp.RenderToFileAsync($outFile, [Windows.Media.Editing.MediaTrimmingPreference]::Precise, $profile)
$t = $asTaskP.MakeGenericMethod([Windows.Media.Transcoding.TranscodeFailureReason], [double]).Invoke($null, @($op)); $t.Wait()
if ($t.Result -ne [Windows.Media.Transcoding.TranscodeFailureReason]::None) { throw "encoding failed: $($t.Result)" }
Log ("done: {0} ({1:N1} MB, {2:N0} s)" -f $Out, ((Get-Item $Out).Length / 1MB), $end)
