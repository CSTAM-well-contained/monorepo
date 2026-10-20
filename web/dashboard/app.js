// CSTAM admin dashboard. Plain ES module, no build step, embedded in cpd.
// Rules: every value comes from the REST API (no business logic here), and data is put in
// the page only as text nodes (Host headers and paths in the traffic log come from the
// internet). web_test.go fails if this file uses innerHTML, outerHTML,
// insertAdjacentHTML, document.write or eval.

const $ = (id) => document.getElementById(id);
const TOKEN_KEY = "cstam.token";
const MAX_TRAFFIC = 300;

const S = {
  token: "", tab: "overview", tick: 0, lastOK: 0, failing: false,
  status: null, teams: [], hacks: [], ipam: [], configs: [],
  events: [], lastEvent: -1,
  traffic: [], lastTraffic: 0, tstats: {}, paused: false,
};

// ---------- DOM helpers ----------
function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? "" : String(v));
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : String(kid));
  }
  return el;
}
// put replaces the children of el; null and false are skipped (replaceChildren would print "null").
const put = (el, ...kids) => el.replaceChildren(...kids.flat(Infinity).filter((k) => k != null && k !== false));
const badge = (text, kind) => h("span", { class: "badge b-" + kind }, text);
const table = (heads, rows, empty, cls) =>
  rows.length === 0
    ? h("div", { class: "table-wrap" }, h("div", { class: "empty" }, empty))
    : h("div", { class: "table-wrap " + (cls || "") }, h("table", {}, h("thead", {}, h("tr", {}, heads.map((x) => h("th", {}, x)))), h("tbody", {}, rows)));

function ago(iso) {
  if (!iso) return "never";
  const s = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000));
  if (s < 60) return s + " s ago";
  if (s < 3600) return Math.floor(s / 60) + " min ago";
  return Math.floor(s / 3600) + " h ago";
}
function left(iso) {
  const s = Math.round((new Date(iso).getTime() - Date.now()) / 1000);
  if (s <= 0) return "expired";
  const hrs = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
  return hrs > 0 ? `${hrs}h${String(m).padStart(2, "0")}` : `${m}m${String(sec).padStart(2, "0")}s`;
}
const clock = (iso) => new Date(iso).toLocaleTimeString([], { hour12: false });

let toastTimer;
function toast(msg, bad) {
  const t = $("toast");
  t.textContent = msg;
  t.className = "toast" + (bad ? " bad" : "");
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), bad ? 7000 : 3500);
}

// confirmAction opens the dialog; with word set, the user must type it (teardown).
function confirmAction(title, text, word) {
  const d = $("confirm");
  $("confirm-title").textContent = title;
  $("confirm-text").textContent = text;
  $("confirm-type-label").hidden = !word;
  $("confirm-word").textContent = word || "";
  const input = $("confirm-input");
  input.value = "";
  const ok = $("confirm-ok");
  ok.disabled = !!word;
  input.oninput = () => (ok.disabled = input.value !== word);
  return new Promise((resolve) => {
    d.onclose = () => resolve(d.returnValue === "ok");
    d.returnValue = "cancel";
    d.showModal();
    (word ? input : ok).focus();
  });
}

// ---------- API ----------
class AuthError extends Error {}
async function api(method, path, body) {
  const headers = { Authorization: "Bearer " + S.token };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const r = await fetch(path, { method, headers, cache: "no-store", body: body === undefined ? undefined : JSON.stringify(body) });
  const data = await r.json().catch(() => ({}));
  if (r.status === 401) throw new AuthError(data.error || "token refused");
  if (!r.ok) throw new Error(data.error || "HTTP " + r.status);
  return data;
}

// act runs one button action, reports it, and refreshes at once.
async function act(btn, label, fn) {
  if (btn) btn.disabled = true;
  try {
    const msg = await fn();
    toast(msg || label + ": done");
    await poll(true);
  } catch (e) {
    if (e instanceof AuthError) return logout(e.message);
    toast(label + " failed: " + e.message, true);
  } finally {
    if (btn) btn.disabled = false;
  }
}

// ---------- polling ----------
async function poll(full) {
  const n = S.tick++;
  const jobs = [
    api("GET", "/v1/status").then((v) => (S.status = v)),
    api("GET", "/v1/teams").then((v) => (S.teams = v)),
    pollTraffic(),
  ];
  if (full || n % 2 === 0) {
    jobs.push(api("GET", "/v1/ipam").then((v) => (S.ipam = v)));
    jobs.push(pollEvents());
  }
  if (full || n % 5 === 0) {
    jobs.push(api("GET", "/v1/hackathons").then((v) => (S.hacks = v)));
    jobs.push(api("GET", "/v1/config").then((v) => (S.configs = v)));
  }
  try {
    await Promise.all(jobs);
    S.lastOK = Date.now();
    S.failing = false;
  } catch (e) {
    if (e instanceof AuthError) return logout(e.message);
    S.failing = true;
  }
  render();
}

async function pollEvents() {
  const q = S.lastEvent < 0 ? "?limit=200" : `?after=${S.lastEvent}&limit=500`;
  const evs = await api("GET", "/v1/events" + q);
  if (evs.length) {
    S.events = S.events.concat(evs).slice(-500);
    S.lastEvent = evs[evs.length - 1].id;
  }
}

async function pollTraffic() {
  const p = await api("GET", `/v1/traffic?after=${S.lastTraffic}&limit=500`);
  S.tstats = p.gateways || {};
  if (p.last_id < S.lastTraffic) S.traffic = []; // cpd restarted
  S.lastTraffic = p.last_id;
  if (!S.paused && p.lines.length) S.traffic = S.traffic.concat(p.lines).slice(-MAX_TRAFFIC);
}

// ---------- rendering ----------
function render() {
  const live = $("live");
  const fresh = Date.now() - S.lastOK < 3000;
  live.className = "live " + (fresh ? "on" : "off");
  live.textContent = fresh ? "live" : S.lastOK ? "offline since " + clock(new Date(S.lastOK).toISOString()) : "connecting";

  const alerts = $("alerts");
  const list = (S.status && S.status.alerts) || [];
  const msgs = S.failing ? ["cpd does not answer: the data below may be old"].concat(list) : list;
  put(alerts, ...msgs.map((m) => h("div", {}, m)));
  alerts.hidden = msgs.length === 0;

  if (!S.status) return;
  ({ overview: renderOverview, teams: renderTeams, traffic: renderTraffic, ipam: renderIPAM,
     config: renderConfig, events: renderEvents })[S.tab]();
}

function stateBadge(g) {
  if (!g.alive) return badge("NO REPORT", "idle");
  switch (g.vrrp_state) {
    case "MASTER": return badge("★ MASTER", "ok");
    case "BACKUP": return badge("BACKUP", "idle");
    case "FAULT": return badge("FAULT", "bad");
    case "STOP": return badge("STOPPED", "bad");
    default: return badge("UNKNOWN", "warn");
  }
}

function gatewayCard(g) {
  const st = S.status;
  const master = g.alive && g.vrrp_state === "MASTER";
  const t = S.tstats[g.name];
  const cls = ["card", "gw", master ? "master" : "", g.vrrp_state === "FAULT" ? "fault" : "", g.alive ? "" : "silent"].join(" ");
  const cfg = g.config_outcome && g.config_outcome !== "applied" && g.config_tried
    ? h("span", {}, g.config_version + " ", badge(g.config_tried + " " + g.config_outcome.replace("_", " "), "bad"))
    : g.config_version || "-";
  const btn = g.force_backup
    ? h("button", { onclick: (e) => failback(e.target, g.name) }, "Failback (clear forced backup)")
    : h("button", { class: master ? "danger" : "", onclick: (e) => failover(e.target, g.name, master) },
        master ? "Planned failover" : "Force to backup");
  return h("div", { class: cls },
    h("div", { class: "head" }, h("span", { class: "name" }, g.name), stateBadge(g),
      g.force_backup ? badge("forced backup", "warn") : null),
    master ? h("div", { class: "vip" }, "VIP " + st.vip + " is here") : h("div", { class: "muted" }, "standby"),
    h("dl", { class: "kv" },
      h("dt", {}, "routes"), h("dd", {}, "generation " + g.acked_generation + " ",
        g.in_sync ? badge("in sync", "ok") : badge("behind " + st.generation, "warn")),
      h("dt", {}, "config"), h("dd", {}, cfg),
      h("dt", {}, "traffic"), h("dd", {}, t ? `${t.rps.toFixed(1)} req/s, ${t.errors_5xx_10s} 5xx in 10 s` : "no lines yet"),
      h("dt", {}, "VRRP since"), h("dd", {}, g.vrrp_since ? ago(g.vrrp_since) : "-"),
      h("dt", {}, "last report"), h("dd", {}, ago(g.last_seen))),
    h("div", { class: "row" }, btn));
}

function failover(btn, name, master) {
  const peer = (S.status.gateways.find((g) => g.name !== name) || {}).name || "the peer";
  const text = master
    ? `All public traffic will move to ${peer}. Expected interruption: under 1 second. ${name} stays up and becomes BACKUP-capable again after failback.`
    : `${name} will refuse to take the VIP until you fail it back.`;
  confirmAction(`Fail over ${name}?`, text).then((ok) => ok &&
    act(btn, "Failover", async () => { await api("POST", `/v1/gateways/${encodeURIComponent(name)}/failover`); return `${name} is stepping down`; }));
}
function failback(btn, name) {
  act(btn, "Failback", async () => { await api("POST", `/v1/gateways/${encodeURIComponent(name)}/failback`); return `${name} may take the VIP again (it stays BACKUP until needed)`; });
}

function tile(num, label) {
  return h("div", { class: "card tile" }, h("div", { class: "num" }, num), h("div", { class: "label" }, label));
}

function renderOverview() {
  const st = S.status;
  const gws = [...st.gateways].sort((a, b) => (b.alive && b.vrrp_state === "MASTER") - (a.alive && a.vrrp_state === "MASTER") || a.name.localeCompare(b.name));
  const teams = st.teams || {}, pool = st.ip_pool || {};
  const rps = Object.values(S.tstats).reduce((s, g) => s + g.rps, 0);
  const vrrp = S.events.filter((e) => e.kind.startsWith("gateway.") || e.kind.startsWith("config.")).slice(-8).reverse();
  put($("tab-overview"),
    h("div", { class: "grid two" },
      gws.length ? gws.map(gatewayCard) : h("div", { class: "card empty" }, "No gateway has reported yet.")),
    h("div", { class: "grid tiles" },
      tile(teams.ready || 0, "teams ready"),
      tile(teams.provisioning || 0, "provisioning"),
      tile(teams.failed || 0, "failed"),
      tile(pool.free || 0, "free IPs"),
      tile(pool.allocated || 0, "IPs leased"),
      tile(pool.quarantine || 0, "IPs in quarantine"),
      tile(rps.toFixed(1), "req/s (10 s)"),
      tile(st.config_version || "-", "config version")),
    h("div", { class: "grid two" },
      h("div", { class: "card" }, h("h3", {}, "IP pool"), ipGrid(), legend()),
      h("div", { class: "card" }, h("h3", {}, "Gateway and config timeline"), eventList(vrrp, "No failover or config change yet."),
        h("h3", { style: "margin-top:12px" }, "Latest events"), eventList(S.events.slice(-8).reverse(), "No events yet."))));
}

function ipGrid() {
  return h("div", { class: "ipgrid" }, S.ipam.map((a) => {
    const last = a.ip.split(".").pop();
    const who = a.team ? ` ${a.team} (${a.hackathon})` : "";
    const since = a.state === "quarantine" && a.released_at ? `, released ${ago(a.released_at)}` : "";
    return h("div", { class: "ip " + a.state, title: `${a.ip}: ${a.state}${who}${since}` }, last);
  }));
}
function legend() {
  return h("div", { class: "legend" },
    h("span", { style: "--c: var(--free)" }, "free"), h("span", { style: "--c: var(--alloc)" }, "leased"),
    h("span", { style: "--c: var(--quar)" }, "quarantine (waits for both gateways)"), h("span", { style: "--c: var(--resv)" }, "reserved"));
}

// short cuts a long error for the timelines; the Config tab shows it whole.
const short = (s) => (!s ? "" : s.length > 110 ? s.slice(0, 110) + "…" : s);

function describe(e) {
  const d = e.detail || {};
  switch (e.kind) {
    case "team.created": return `${e.team} created in ${d.hackathon}`;
    case "team.ready": return `${e.team} ready in ${Number(d.seconds).toFixed(1)} s`;
    case "team.failed": return `${e.team} failed: ${short(d.error)}`;
    case "team.deleting": return `${e.team} teardown started`;
    case "team.deleted": return `${e.team} deleted`;
    case "ip.allocated": return `${d.ip} leased to ${e.team || "a team"} (slot ${d.slot})`;
    case "ip.quarantined": return `${d.ip} in quarantine`;
    case "ip.freed": return `${d.ip} free again`;
    case "gateway.vrrp": return `${d.gateway} is now ${d.state || "unknown"}`;
    case "gateway.failover": return `planned failover of ${d.gateway}${d.force ? " (forced)" : ""}`;
    case "gateway.failback": return `failback of ${d.gateway}`;
    case "config.created": return `config ${d.version} pushed: ${d.reason || "no reason"}`;
    case "config.rollback": return `config ${d.version}: ${d.reason}`;
    case "config.applied": return `${d.gateway} serves config ${d.version}`;
    case "config.rejected":
    case "config.rolled_back":
    case "config.failed":
      return `${d.gateway} ${e.kind.slice(7).replace("_", " ")} config ${d.version}, serves ${d.serving}: ${short(d.error)}`;
    default: return `${e.kind} ${e.team || ""} ${JSON.stringify(d)}`;
  }
}
function eventList(evs, empty) {
  if (!evs.length) return h("div", { class: "empty" }, empty);
  return h("ul", { class: "events" }, evs.map((e) => h("li", {}, h("time", { datetime: e.at }, clock(e.at)), h("span", {}, describe(e)))));
}

const teamBadge = { ready: "ok", provisioning: "warn", deleting: "warn", failed: "bad", deleted: "idle" };
function renderTeams() {
  const sel = $("spinup-hack");
  const active = S.hacks.filter((x) => x.state === "active").map((x) => x.name);
  if ([...sel.options].map((o) => o.value).join() !== active.join()) {
    const keep = sel.value;
    put(sel, ...active.map((n) => h("option", { value: n }, n)));
    if (active.includes(keep)) sel.value = keep;
  }
  const rows = S.teams.map((t) => h("tr", {},
    h("td", {}, t.name), h("td", {}, t.hackathon), h("td", { class: "mono" }, t.fqdn),
    h("td", { class: "mono" }, t.ip || "-"), h("td", {}, t.slot || "-"), h("td", {}, t.app_port),
    h("td", {}, badge(t.state, teamBadge[t.state] || "idle"), t.error ? h("div", { class: "small error" }, t.error) : null),
    h("td", {}, t.state === "ready" || t.state === "provisioning" ? left(t.lease_expires_at) : "-"),
    h("td", {},
      h("button", { class: "small", disabled: t.state !== "ready", onclick: (e) =>
        act(e.target, "Renew", async () => { await api("POST", `/v1/teams/${t.id}/renew`); return `${t.name}: lease extended`; }) }, "Extend"),
      " ",
      h("button", { class: "small danger", disabled: t.state === "deleting", onclick: (e) =>
        confirmAction(`Tear down ${t.name}?`, `The sandbox is deleted, the route ${t.fqdn} is removed from both gateways, and ${t.ip || "its IP"} goes to quarantine until both gateways confirm.`, t.name)
          .then((ok) => ok && act(e.target, "Teardown", async () => { await api("DELETE", `/v1/teams/${t.id}`); return `${t.name}: teardown started`; })) }, "Tear down"))));
  put($("teams-list"),table(["Team", "Hackathon", "Subdomain", "Private IP", "Slot", "Port", "State", "Lease left", ""], rows, "No team yet: spin one up above."));
}

function renderTraffic() {
  const hosts = S.teams.map((t) => t.fqdn);
  const dl = $("traffic-hosts");
  if ([...dl.options].map((o) => o.value).join() !== hosts.join()) put(dl, ...hosts.map((x) => h("option", { value: x })));
  const filter = $("traffic-host").value.trim().toLowerCase();
  const lines = S.traffic.filter((l) => !filter || l.host.includes(filter)).slice(-200).reverse();
  const stats = Object.entries(S.tstats).sort().map(([gw, g]) => tile(`${g.rps.toFixed(1)}/s`, `${gw}: ${g.requests_10s} requests, ${g.errors_5xx_10s} 5xx in 10 s, ${g.dropped_total} lines dropped`));
  const rows = lines.map((l) => h("tr", {},
    h("td", { class: "mono" }, clock(l.at)), h("td", {}, l.gw), h("td", { class: "mono" }, l.client),
    h("td", { class: "mono" }, l.host), h("td", {}, l.method), h("td", { class: "mono wrap" }, l.path),
    h("td", { class: "s" + String(l.status)[0] }, l.status), h("td", {}, l.tt + " ms"), h("td", { class: "mono" }, l.be)));
  put($("traffic-view"),
    h("div", { class: "grid tiles" }, stats.length ? stats : tile("0/s", "no traffic seen yet")),
    S.paused ? h("p", { class: "muted" }, "Paused: new lines are not added.") : null,
    table(["Time", "Gateway", "Client", "Host", "Method", "Path", "Status", "Time", "Backend"], rows, "No requests yet. Lines arrive about once a second.", "scroll"));
}

function renderIPAM() {
  const used = S.ipam.filter((a) => a.state !== "free");
  const rows = used.map((a) => h("tr", {}, h("td", { class: "mono" }, a.ip), h("td", {}, badge(a.state, { allocated: "ok", quarantine: "warn", reserved: "idle" }[a.state] || "idle")),
    h("td", {}, a.team || "-"), h("td", {}, a.hackathon || "-"), h("td", {}, a.released_at ? ago(a.released_at) : "-"), h("td", {}, ago(a.updated_at))));
  put($("tab-ipam"),
    h("div", { class: "card" }, h("h2", {}, `Team pool ${S.ipam.length ? S.ipam[0].ip + " - " + S.ipam[S.ipam.length - 1].ip : ""}`),
      h("p", { class: "muted small" }, "Leases are short; an IP released by a teardown or an expired lease stays in quarantine until both gateways confirmed the route is gone, then becomes free."),
      ipGrid(), legend()),
    table(["IP", "State", "Team", "Hackathon", "Released", "Changed"], rows, "Every address is free."));
}

function renderConfig() {
  const st = S.status;
  const gwRows = st.gateways.map((g) => h("tr", {}, h("td", {}, g.name), h("td", { class: "mono" }, g.config_version || "-"),
    h("td", { class: "mono" }, g.config_tried || "-"),
    h("td", {}, g.config_outcome ? badge(g.config_outcome.replace("_", " "), g.config_outcome === "applied" ? "ok" : "bad") : "-"),
    h("td", { class: "wrap small" }, g.config_error || "")));
  const vRows = S.configs.map((v) => h("tr", {},
    h("td", { class: "mono" }, v.version, v.current ? " " : null, v.current ? badge("current", "ok") : null),
    h("td", {}, clock(v.created_at)), h("td", {}, v.rate_limit),
    h("td", { class: "mono wrap small" }, v.extra.length ? v.extra.join("\n") : "-"),
    h("td", { class: "wrap" }, v.reason || "-"),
    h("td", {}, h("button", { class: "small", onclick: (e) =>
      confirmAction(`Roll back to ${v.version}?`, `A new version is created with the content of ${v.version}; both gateways reload to it (check, hitless reload, verify).`)
        .then((ok) => ok && act(e.target, "Rollback", async () => { const n = await api("POST", `/v1/config/${v.version}/rollback`); return `${n.version} pushed (${n.reason})`; })) },
      "Roll back to this"))));
  put($("config-view"),
    h("h2", {}, "Gateways"), table(["Gateway", "Serves", "Last tried", "Outcome", "Error"], gwRows, "No gateway has reported yet."),
    h("h2", {}, "Versions"), table(["Version", "Created", "Rate limit", "Extra lines", "Reason", ""], vRows, "No version pushed yet: the gateways use their command-line settings."));
}

function renderEvents() {
  const rows = S.events.slice().reverse().map((e) => h("tr", {}, h("td", { class: "mono" }, e.id), h("td", { class: "mono" }, clock(e.at)),
    h("td", { class: "mono" }, e.kind), h("td", { class: "wrap" }, describe(e))));
  put($("tab-events"),table(["#", "Time", "Kind", "What happened"], rows, "No events yet.", "scroll"));
}

// ---------- forms and navigation ----------
function wire() {
  $("tabs").addEventListener("click", (e) => {
    const b = e.target.closest("button[data-tab]");
    if (!b) return;
    S.tab = b.dataset.tab;
    for (const x of $("tabs").querySelectorAll("button")) x.classList.toggle("active", x === b);
    for (const s of document.querySelectorAll(".tab")) s.hidden = s.id !== "tab-" + S.tab;
    render();
  });
  $("logout").onclick = () => logout("");
  $("spinup").onsubmit = (e) => {
    e.preventDefault();
    const hack = $("spinup-hack").value, name = $("spinup-name").value.trim();
    if (!hack) return toast("Create a hackathon first", true);
    act(e.submitter, "Spin up", async () => {
      const t = await api("POST", `/v1/hackathons/${encodeURIComponent(hack)}/teams`,
        { name, app_port: Number($("spinup-port").value) || 80, image: $("spinup-image").value.trim() });
      $("spinup-name").value = "";
      return `${t.name}: provisioning on ${t.ip}`;
    });
  };
  $("newhack").onsubmit = (e) => {
    e.preventDefault();
    act(e.submitter, "New hackathon", async () => {
      const x = await api("POST", "/v1/hackathons", { name: $("newhack-name").value.trim(), domain: $("newhack-domain").value.trim(),
        lease_ttl_seconds: Math.round(Number($("newhack-ttl").value) * 60) });
      return `hackathon ${x.name} created (${x.domain})`;
    });
  };
  $("pushcfg").onsubmit = (e) => {
    e.preventDefault();
    const extra = $("cfg-extra").value.split("\n").map((l) => l.trim()).filter(Boolean);
    act(e.submitter, "Push config", async () => {
      const v = await api("POST", "/v1/config", { rate_limit: Number($("cfg-rate").value), extra, reason: $("cfg-reason").value.trim() });
      return `${v.version} pushed: watch the gateways below`;
    });
  };
  $("preset-broken").onclick = () => {
    $("cfg-extra").value = "http-request return status 500 if { path /__gw/healthz }";
    $("cfg-reason").value = "demo: valid syntax, health check broken";
  };
  $("preset-typo").onclick = () => {
    $("cfg-extra").value = "this-is-not-a-directive";
    $("cfg-reason").value = "demo: syntax error";
  };
  $("traffic-pause").onclick = (e) => {
    S.paused = !S.paused;
    e.target.textContent = S.paused ? "Resume" : "Pause";
    render();
  };
  $("traffic-clear").onclick = () => { S.traffic = []; render(); };
  $("traffic-host").oninput = () => render();
  $("login-form").onsubmit = async (e) => {
    e.preventDefault();
    S.token = $("login-token").value.trim();
    try {
      await api("GET", "/v1/status");
    } catch (err) {
      $("login-error").textContent = err instanceof AuthError ? "This token is refused." : "cpd does not answer: " + err.message;
      return;
    }
    try { sessionStorage.setItem(TOKEN_KEY, S.token); } catch { /* private mode: works for this page only */ }
    start();
  };
}

let timer;
function start() {
  $("login").hidden = true;
  $("app").hidden = false;
  $("login-token").value = "";
  clearInterval(timer);
  poll(true);
  timer = setInterval(() => { if (!document.hidden) poll(false); }, 1000);
}

function logout(reason) {
  clearInterval(timer);
  try { sessionStorage.removeItem(TOKEN_KEY); } catch { /* ignore */ }
  S.token = "";
  $("app").hidden = true;
  $("login").hidden = false;
  $("login-error").textContent = reason ? "Logged out: " + reason : "";
}

wire();
try { S.token = sessionStorage.getItem(TOKEN_KEY) || ""; } catch { S.token = ""; }
if (S.token) start(); else logout("");
