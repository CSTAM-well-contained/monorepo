// Narration with Edge's natural voices (make-video.ps1). The page records its own tab audio
// (Edge started with --auto-accept-this-tab-capture) while speechSynthesis speaks, then returns
// 16-bit mono PCM at 22050 Hz, silence trimmed and peak-normalized, as base64.
window.__voiceInit = async (name) => {
  const pick = () => speechSynthesis.getVoices().find((v) => v.name.startsWith("Microsoft " + name + " Online"));
  for (let i = 0; i < 20 && !pick(); i++) await new Promise((r) => setTimeout(r, 250));
  window.__voice = pick();
  if (!window.__voice) return "voice not found: " + name;
  window.__stream = await navigator.mediaDevices.getDisplayMedia({ video: true, audio: true, preferCurrentTab: true, selfBrowserSurface: "include" });
  if (!window.__stream.getAudioTracks().length) return "no audio track";
  return "ok " + window.__voice.name;
};

window.__say = async (text, rate) => {
  const rec = new MediaRecorder(new MediaStream(window.__stream.getAudioTracks()), { mimeType: "audio/webm" });
  const chunks = [];
  rec.ondataavailable = (e) => chunks.push(e.data);
  rec.start(100);
  const res = await new Promise((r) => {
    const u = new SpeechSynthesisUtterance(text);
    u.voice = window.__voice; u.rate = rate;
    u.onend = () => r("end"); u.onerror = (e) => r("error " + e.error);
    speechSynthesis.speak(u);
    setTimeout(() => r("timeout"), 90000);
  });
  await new Promise((r) => setTimeout(r, 500));
  rec.stop();
  await new Promise((r) => (rec.onstop = r));
  if (res !== "end") throw new Error("speech " + res);

  const dec = await new AudioContext().decodeAudioData(await new Blob(chunks).arrayBuffer());
  const sr = 22050;
  const off = new OfflineAudioContext(1, Math.ceil(dec.duration * sr), sr);
  const src = off.createBufferSource(); src.buffer = dec; src.connect(off.destination); src.start();
  const x = (await off.startRendering()).getChannelData(0);
  let a = 0, b = x.length - 1;
  while (a < b && Math.abs(x[a]) < 0.008) a++;
  while (b > a && Math.abs(x[b]) < 0.008) b--;
  a = Math.max(0, a - Math.round(0.08 * sr)); b = Math.min(x.length - 1, b + Math.round(0.2 * sr));
  let peak = 0;
  for (let i = a; i <= b; i++) peak = Math.max(peak, Math.abs(x[i]));
  const gain = peak > 0 ? Math.min(4, 0.9 / peak) : 1;
  const pcm = new Int16Array(b - a + 1);
  for (let i = 0; i < pcm.length; i++) pcm[i] = Math.max(-1, Math.min(1, x[a + i] * gain)) * 32767;
  const bytes = new Uint8Array(pcm.buffer);
  let s = "";
  for (let i = 0; i < bytes.length; i += 0x8000) s += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000));
  return btoa(s);
};
"loaded";
