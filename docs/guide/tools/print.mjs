// Print an HTML file to PDF with page numbers using Chrome DevTools Protocol (no npm deps; Node 22).
import { spawn } from 'node:child_process';
import { writeFileSync } from 'node:fs';
import { setTimeout as sleep } from 'node:timers/promises';
const [,, input, output, label = 'Implementation guide'] = process.argv;
const port = 9333;
const chrome = spawn('google-chrome', ['--headless=new', `--remote-debugging-port=${port}`, '--no-first-run',
  '--disable-gpu', '--allow-file-access-from-files', `--user-data-dir=/tmp/claude-1000/chrome-print-${process.pid}`, 'about:blank'], { stdio: 'ignore' });
let wsUrl;
for (let i = 0; i < 50 && !wsUrl; i++) {
  await sleep(200);
  try { const r = await fetch(`http://127.0.0.1:${port}/json/list`); const t = (await r.json()).find(x => x.type === 'page'); wsUrl = t?.webSocketDebuggerUrl; } catch {}
}
const ws = new WebSocket(wsUrl);
await new Promise(r => ws.addEventListener('open', r));
let id = 0; const pending = new Map(); const events = [];
ws.addEventListener('message', ev => { const m = JSON.parse(ev.data); if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } else events.push(m); });
const send = (method, params = {}) => new Promise(r => { const i = ++id; pending.set(i, r); ws.send(JSON.stringify({ id: i, method, params })); });
await send('Page.enable');
await send('Page.navigate', { url: 'file://' + input });
for (let i = 0; i < 600 && !events.some(e => e.method === 'Page.loadEventFired'); i++) await sleep(100);
await sleep(1500);
const footer = `<div style="font-size:7.5pt;color:#868e96;width:100%;padding:0 14mm;display:flex;justify-content:space-between;font-family:Arial,sans-serif">
  <span>CSTAM x FelCloud · The Resilient IP Optimizer · ${label}</span><span><span class="pageNumber"></span> / <span class="totalPages"></span></span></div>`;
const res = await send('Page.printToPDF', { printBackground: true, preferCSSPageSize: true, displayHeaderFooter: true,
  headerTemplate: '<div></div>', footerTemplate: footer, transferMode: 'ReturnAsBase64' });
if (!res.result) { console.error(JSON.stringify(res.error)); process.exit(1); }
writeFileSync(output, Buffer.from(res.result.data, 'base64'));
ws.close(); chrome.kill();
console.log('wrote', output);
