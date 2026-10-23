"use strict";
// BigBadRescue window. Talks to the Go server on 127.0.0.1.

const $ = (id) => document.getElementById(id);
const el = (tag, cls, text) => {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
};

const S = {
  app: null,
  settings: {},
  conn: { status: "signed-out" },
  pace: {},
  queue: { running: null, waiting: [] },
  formats: [],
  rows: [],          // search results
  selected: new Set(), // note ids
  active: null,      // note id in the viewer
  detail: null,
  items: [],         // all memory items (review tab)
  revSel: new Set(),
  tab: "find",
  confirmTimer: null,
};

async function api(method, path, body) {
  const opt = { method, headers: {} };
  if (body !== undefined) {
    opt.headers["Content-Type"] = "application/json";
    opt.body = JSON.stringify(body);
  }
  const r = await fetch(path, opt);
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || `HTTP ${r.status}`);
  return data;
}

function toast(text, kind) {
  const t = el("div", "toast " + (kind || ""), text);
  $("toasts").append(t);
  setTimeout(() => t.remove(), kind === "error" ? 9000 : 4500);
}

const fmtDate = (s) => {
  if (!s) return "";
  const d = new Date(s);
  return isNaN(d) ? s : d.toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" });
};
const fmtSize = (n) => n >= 1e6 ? (n / 1e6).toFixed(2) + " MB" : Math.max(1, Math.round(n / 1e3)) + " KB";

/* ---------- state from the server ---------- */

function applyState(st) {
  S.app = st;
  S.settings = st.settings;
  S.formats = st.formats;
  $("ver").textContent = st.version;
  $("demoBanner").hidden = !st.demo;
  if (st.ffmpeg.error) {
    $("ffWarn").hidden = false;
    $("ffWarnMsg").textContent = st.ffmpeg.error + ". Install it with `brew install ffmpeg`, or use BigBadRescue.app, which includes it.";
    $("ffLine").textContent = "ffmpeg not found";
  } else {
    $("ffWarn").hidden = true;
    $("ffLine").textContent = "ffmpeg " + st.ffmpeg.version;
  }
  renderChips();
  renderSettings();
  applyConn(st.conn);
  applyPace(st.pace);
  applyQueue(st.queue);
}

function applyConn(c) {
  S.conn = c;
  const box = $("conn");
  box.className = "conn " + c.status;
  const text = {
    "signed-out": "Not connected",
    "signing-in": "Signing in…",
    "connecting": "Connecting…",
    "connected": "Connected to Evernote",
    "error": "Not connected",
  }[c.status] || c.status;
  $("connText").textContent = text;
  $("connectBtn").textContent = c.status === "error" ? "Try again" : "Connect Evernote";
  $("connectBtn").disabled = c.status === "connecting" || c.status === "signing-in";
  $("signinBanner").hidden = c.status !== "signing-in";
  if (c.signInUrl) $("signinLink").href = c.signInUrl;
  $("connErr").hidden = c.status !== "error";
  $("connErr").textContent = c.message || "";
  $("acctLine").textContent = c.status === "connected" ? "Connected. Your sign-in is kept in the macOS Keychain." : "Not connected.";
  if (c.status === "connected" && !S.notebooksLoaded) loadNotebooks();
  updateBar();
}

let pauseTick = null;
function applyPace(p) {
  S.pace = p;
  const m = $("meter");
  m.className = "meter " + p.state;
  const label = { steady: "steady", slowing: "slowing down", paused: "paused" }[p.state] || p.state;
  m.textContent = `Pace: ${label} · ${p.callsLastHour} calls this hour`;
  const paused = p.state === "paused";
  $("pauseBanner").hidden = !paused;
  clearInterval(pauseTick);
  if (paused) {
    const until = new Date(p.pausedUntil);
    $("pauseAt").textContent = until.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
    const tick = () => {
      const s = Math.max(0, Math.round((until - Date.now()) / 1000));
      $("pauseLeft").textContent = s >= 60 ? `${Math.floor(s / 60)} min ${s % 60} s` : `${s} s`;
    };
    tick();
    pauseTick = setInterval(tick, 1000);
  }
}

function applyQueue(q) {
  S.queue = q;
  const busy = !!q.running || q.waiting.length > 0;
  $("job").hidden = !busy;
  $("stopBtn").hidden = !busy;
  if (q.running) {
    const kind = q.running.kind === "cleanup" ? "Cleaning up" : "Converting";
    $("jobTitle").textContent = `${kind} “${q.running.title}”`;
    $("jobStep").textContent = "· " + q.running.step + (q.running.file ? " · " + q.running.file : "");
    $("jobFill").style.width = Math.round((q.running.frac || 0) * 100) + "%";
  } else {
    $("jobTitle").textContent = "Waiting";
    $("jobStep").textContent = "";
    $("jobFill").style.width = "0";
  }
  $("jobQueue").textContent = q.waiting.length ? `${q.waiting.length} more note${q.waiting.length === 1 ? "" : "s"} waiting` : "";
  renderRows();
}

function queuedKind(noteId) {
  if (S.queue.running && S.queue.running.noteId === noteId) return "working";
  return S.queue.waiting.some((j) => j.noteId === noteId) ? "queued" : "";
}

/* ---------- events ---------- */

function connectEvents() {
  const es = new EventSource("/api/events");
  es.addEventListener("state", (e) => applyState(JSON.parse(e.data)));
  es.addEventListener("conn", (e) => applyConn(JSON.parse(e.data)));
  es.addEventListener("pace", (e) => applyPace(JSON.parse(e.data)));
  es.addEventListener("queue", (e) => applyQueue(JSON.parse(e.data)));
  es.addEventListener("settings", (e) => { S.settings = JSON.parse(e.data); renderSettings(); });
  es.addEventListener("toast", (e) => { const t = JSON.parse(e.data); toast(t.text, t.kind); });
  es.addEventListener("item", (e) => {
    const it = JSON.parse(e.data);
    const i = S.items.findIndex((x) => x.key === it.key);
    if (i >= 0) S.items[i] = it; else S.items.push(it);
    renderReview();
    if (S.detail && S.detail.noteId === it.noteId) refreshDetailSoon();
  });
  es.addEventListener("note", (e) => {
    const n = JSON.parse(e.data);
    const row = S.rows.find((r) => r.noteId === n.noteId);
    if (row) row.status = n.status;
    renderRows();
    if (S.active === n.noteId) refreshDetailSoon();
  });
  es.addEventListener("bye", () => { document.body.innerHTML = "<p style='padding:40px;text-align:center'>BigBadRescue has quit. You can close this window.</p>"; });
  es.onerror = () => {};
}

/* ---------- find & convert ---------- */

function renderChips() {
  const box = $("formatChips");
  if (box.childElementCount) return;
  for (const f of S.formats) {
    const b = el("button", "chip", f.label);
    b.type = "button";
    b.dataset.key = f.key;
    b.setAttribute("aria-pressed", "true");
    b.title = f.mimes.join(", ");
    b.onclick = () => b.setAttribute("aria-pressed", b.getAttribute("aria-pressed") === "true" ? "false" : "true");
    box.append(b);
  }
}

function filter() {
  return {
    field: $("field").value,
    from: $("from").value,
    to: $("to").value,
    notebookId: $("notebook").value,
    formats: [...document.querySelectorAll("#formatChips .chip[aria-pressed=true]")].map((b) => b.dataset.key),
  };
}

function needConnection() {
  if (S.conn.status !== "connected") {
    toast("Connect to Evernote first.", "error");
    return true;
  }
  return false;
}

async function loadNotebooks() {
  S.notebooksLoaded = true;
  try {
    const nbs = await api("GET", "/api/notebooks");
    const sel = $("notebook");
    nbs.sort((a, b) => a.label.localeCompare(b.label));
    for (const nb of nbs) {
      const o = el("option", "", nb.label);
      o.value = nb.notebookId;
      sel.append(o);
    }
  } catch (e) {
    S.notebooksLoaded = false;
  }
}

async function search(ev) {
  ev && ev.preventDefault();
  if (needConnection()) return;
  const f = filter();
  if (f.from && f.to && f.from > f.to) { toast("The start date is after the end date.", "error"); return; }
  $("searchBtn").disabled = true;
  $("searchBtn").textContent = "Searching…";
  try {
    const res = await api("POST", "/api/search", f);
    S.rows = res.rows;
    S.selected.clear();
    if (res.capped) toast("That range has a lot of notes, so the list stops at the first 3,000. Narrow the dates to see the rest.");
    renderRows();
    if (!S.rows.length) toast("No notes with those attachment types in that range.");
  } catch (e) {
    toast(e.message, "error");
  } finally {
    $("searchBtn").disabled = false;
    $("searchBtn").textContent = "Search";
  }
}

async function scan() {
  if (needConnection()) return;
  const out = $("scanOut");
  out.hidden = false;
  out.textContent = "Counting…";
  try {
    const counts = await api("POST", "/api/scan", filter());
    out.textContent = "";
    if (!counts.length) { out.textContent = "No audio attachments in this range."; return; }
    out.append(el("span", "", "Notes in this range with:"));
    for (const c of counts) {
      const s = el("span");
      s.append(el("b", "", c.notes.toLocaleString()), ` ${c.format} (${c.mime})`);
      out.append(s);
    }
  } catch (e) {
    out.textContent = e.message;
  }
}

function visibleRows() {
  const show = $("show").value;
  return S.rows.filter((r) => {
    if (show === "todo") return r.status === "todo" || r.status === "partial";
    if (show === "converted") return ["converted", "reviewed", "cleaned"].includes(r.status);
    if (show === "problems") return r.status === "failed" || r.status === "rejected";
    return true;
  });
}

const statusLabel = {
  todo: "", partial: "Part done", converted: "Converted", reviewed: "Sounds right",
  cleaned: "Cleaned up", failed: "Problem", rejected: "Sounds wrong", queued: "Queued", working: "Working…",
};

function selectable(r) {
  return r.status === "todo" || r.status === "partial" || r.status === "failed";
}

function renderRows() {
  const ul = $("rows");
  const rows = visibleRows();
  ul.textContent = "";
  $("listEmpty").hidden = S.rows.length > 0;
  for (const r of rows) {
    const li = el("li", "row" + (r.noteId === S.active ? " active" : ""));
    li.setAttribute("role", "option");
    li.dataset.id = r.noteId;
    const cb = el("input");
    cb.type = "checkbox";
    cb.checked = S.selected.has(r.noteId);
    cb.disabled = !selectable(r);
    cb.title = cb.disabled ? "Already converted" : "Select for conversion";
    cb.onclick = (e) => {
      e.stopPropagation();
      if (e.shiftKey && S.lastClicked) rangeSelect(S.lastClicked, r.noteId, cb.checked);
      else if (cb.checked) S.selected.add(r.noteId); else S.selected.delete(r.noteId);
      S.lastClicked = r.noteId;
      renderRows();
    };
    const main = el("div");
    main.style.minWidth = "0";
    main.append(el("div", "row-t", r.title || "Untitled"));
    const sub = el("div", "row-s");
    sub.append(fmtDate(r.created));
    for (const f of r.formats) sub.append(el("span", "fmt " + f, f));
    main.append(sub);
    if (r.snippet) main.append(el("div", "row-snip", r.snippet));
    const q = queuedKind(r.noteId);
    const st = q || r.status;
    const badge = el("span", "badge " + st, statusLabel[st] || "");
    if (!statusLabel[st]) badge.hidden = true;
    li.append(cb, main, badge);
    li.onclick = () => openNote(r.noteId);
    ul.append(li);
  }
  const sel = rows.filter(selectable);
  $("selAll").checked = sel.length > 0 && sel.every((r) => S.selected.has(r.noteId));
  $("selAll").disabled = sel.length === 0;
  $("listCount").textContent = S.rows.length
    ? `${rows.length.toLocaleString()} note${rows.length === 1 ? "" : "s"}` + (rows.length !== S.rows.length ? ` of ${S.rows.length.toLocaleString()}` : "")
    : "Search to list notes";
  updateBar();
}

function rangeSelect(a, b, on) {
  const ids = visibleRows().map((r) => r.noteId);
  let i = ids.indexOf(a), j = ids.indexOf(b);
  if (i < 0 || j < 0) return;
  if (i > j) [i, j] = [j, i];
  for (const id of ids.slice(i, j + 1)) {
    const r = S.rows.find((x) => x.noteId === id);
    if (!selectable(r)) continue;
    if (on) S.selected.add(id); else S.selected.delete(id);
  }
}

function updateBar() {
  const n = S.selected.size;
  const onFind = S.tab === "find";
  $("selInfo").hidden = !onFind;
  $("convertBtn").hidden = !onFind;
  $("selInfo").textContent = n ? `${n} note${n === 1 ? "" : "s"} selected` : "No notes selected";
  $("convertBtn").disabled = n === 0 || S.conn.status !== "connected" || !!(S.app && S.app.ffmpeg.error);
  const ready = cleanupReady();
  $("cleanupBtn").hidden = S.tab !== "review";
  $("cleanupBtn").disabled = ready.length === 0 || S.conn.status !== "connected";
  $("cleanupBtn").textContent = ready.length ? `Remove originals from ${ready.length} note${ready.length === 1 ? "" : "s"}…` : "Remove originals…";
  if (S.tab !== "review") $("confirmCleanup").hidden = true;
}

let detailTimer = null;
function refreshDetailSoon() {
  clearTimeout(detailTimer);
  detailTimer = setTimeout(() => S.active && openNote(S.active, true), 600);
}

async function openNote(id, quiet) {
  S.active = id;
  if (!quiet) renderRows();
  $("viewEmpty").hidden = true;
  $("viewBody").hidden = false;
  if (!quiet) {
    $("vTitle").textContent = "Loading…";
    $("vMeta").textContent = "";
    $("vAtts").textContent = "";
    $("vText").textContent = "";
    $("vTags").textContent = "";
  }
  try {
    const d = await api("GET", "/api/note?id=" + encodeURIComponent(id));
    if (S.active !== id) return;
    S.detail = d;
    renderDetail(d);
  } catch (e) {
    $("vTitle").textContent = "Couldn't open this note";
    $("vMeta").textContent = e.message;
  }
}

const itemLabel = {
  "backed-up": "Backed up", converted: "Converted", checked: "Checked", uploaded: "Uploaded",
  attached: "Attached", verified: "MP3 added, waiting for review", reviewed: "Sounds right",
  cleaned: "Original removed", failed: "Problem", rejected: "Marked as sounding wrong",
};

function renderDetail(d) {
  $("vTitle").textContent = d.title || "Untitled";
  $("vMeta").textContent = `Created ${fmtDate(d.created)} · updated ${fmtDate(d.updated)}`;
  const tags = $("vTags");
  tags.textContent = "";
  for (const t of d.tags || []) tags.append(el("span", "", t));
  const box = $("vAtts");
  const playing = [...box.querySelectorAll("audio")].some((a) => !a.paused);
  if (playing) return; // don't interrupt playback with a refresh
  box.textContent = "";
  const audio = (d.attachments || []).filter((a) => a.audio || a.format);
  if (!audio.length) box.append(el("div", "att-d", "No audio attachments on this note."));
  for (const a of audio) {
    const card = el("div", "att" + (a.converted ? " new" : ""));
    const n = el("div", "att-n");
    n.append(a.name || a.hash.slice(0, 8));
    if (a.format) n.append(el("span", "fmt " + a.format, a.format));
    if (a.converted) n.append(el("span", "badge converted", "New MP3"));
    else if (a.playable) n.append(el("span", "badge", "Plays on phones"));
    else if (a.format) n.append(el("span", "badge failed", "Doesn't play on phones"));
    card.append(n);
    const right = el("div", "att-d", fmtSize(a.size));
    if (a.item) {
      const b = el("span", "badge " + a.item.state, itemLabel[a.item.state] || a.item.state);
      right.textContent = "";
      right.append(fmtSize(a.size), " ", b);
    }
    card.append(right);
    const au = el("audio");
    au.controls = true;
    au.preload = "none";
    au.src = `/api/audio?note=${encodeURIComponent(d.noteId)}&hash=${a.hash}&mime=${encodeURIComponent(a.mime)}`;
    au.title = a.playable ? "" : "BigBadRescue makes a quick preview, because browsers can't play this format";
    card.append(au);
    if (a.item && a.item.error) card.append(el("div", "err", a.item.error));
    box.append(card);
  }
  $("vText").textContent = d.text || "";
  $("vText").hidden = !d.text;
}

async function convertSelected() {
  if (needConnection()) return;
  const notes = S.rows.filter((r) => S.selected.has(r.noteId)).map((r) => ({ noteId: r.noteId, title: r.title }));
  if (!notes.length) return;
  try {
    const r = await api("POST", "/api/convert", { notes });
    toast(`${r.queued} note${r.queued === 1 ? "" : "s"} added to the queue.`);
    S.selected.clear();
    renderRows();
  } catch (e) {
    toast(e.message, "error");
  }
}

/* ---------- review ---------- */

async function loadItems() {
  try {
    S.items = await api("GET", "/api/items");
  } catch (e) {
    S.items = [];
  }
  renderReview();
}

function reviewList() {
  const show = $("reviewShow").value;
  return S.items.filter((it) => {
    if (show === "waiting") return it.state === "verified";
    if (show === "reviewed") return it.state === "reviewed";
    if (show === "cleaned") return it.state === "cleaned";
    if (show === "problems") return it.state === "failed" || it.state === "rejected";
    return true;
  });
}

function cleanupReady() {
  const trust = S.settings && S.settings.trustAutoCheck;
  const notes = new Map();
  for (const it of S.items) {
    if (it.state === "reviewed" || (trust && it.state === "verified")) notes.set(it.noteId, it.noteTitle);
  }
  return [...notes].map(([noteId, title]) => ({ noteId, title }));
}

function renderReview() {
  const waiting = S.items.filter((it) => it.state === "verified").length;
  $("reviewCount").hidden = waiting === 0;
  $("reviewCount").textContent = waiting;
  const list = reviewList();
  const ul = $("revRows");
  // Keep players that are playing.
  const playing = [...ul.querySelectorAll("audio")].some((a) => !a.paused);
  if (playing && S.tab === "review") { updateRevBulk(list); updateBar(); return; }
  ul.textContent = "";
  $("revEmpty").hidden = list.length > 0;
  for (const it of list) {
    const li = el("li", "rev");
    const cb = el("input");
    cb.type = "checkbox";
    cb.checked = S.revSel.has(it.key);
    cb.disabled = !["verified", "reviewed", "rejected"].includes(it.state);
    cb.onchange = () => { if (cb.checked) S.revSel.add(it.key); else S.revSel.delete(it.key); updateRevBulk(list); };
    const info = el("div");
    info.style.minWidth = "0";
    info.append(el("div", "rev-t", it.noteTitle || "Untitled"));
    info.append(el("div", "rev-s", `${fmtDate(it.noteCreated)} · ${it.srcName}`));
    const b = el("span", "badge " + it.state, itemLabel[it.state] || it.state);
    const bl = el("div");
    bl.style.marginTop = "4px";
    bl.append(b);
    info.append(bl);
    if (it.error) info.append(el("div", "rev-err", it.error));
    const players = el("div", "players");
    if (it.backup) players.append(player("Original " + it.format, `/api/review/audio?key=${encodeURIComponent(it.key)}&which=orig`));
    if (it.mp3Hash) players.append(player("New MP3" + (it.mp3Size ? " · " + fmtSize(it.mp3Size) : ""), `/api/review/audio?key=${encodeURIComponent(it.key)}&which=mp3`));
    const btns = el("div", "rev-btns");
    if (it.state === "verified" || it.state === "rejected") {
      const good = el("button", "btn small good", "Sounds right");
      good.onclick = () => review([it.key], true);
      btns.append(good);
    }
    if (it.state === "verified" || it.state === "reviewed") {
      const bad = el("button", "btn small", "Sounds wrong");
      bad.title = "Keep the original on the note and don't clean it up";
      bad.onclick = () => review([it.key], false);
      btns.append(bad);
    }
    if (it.state === "failed") {
      const retry = el("button", "btn small", "Retry");
      retry.onclick = async () => {
        try { await api("POST", "/api/convert", { notes: [{ noteId: it.noteId, title: it.noteTitle }] }); toast("Queued again."); }
        catch (e) { toast(e.message, "error"); }
      };
      btns.append(retry);
    }
    if (it.backup) {
      const show = el("button", "btn small", "Show backup");
      show.onclick = () => api("POST", "/api/reveal", { path: it.backup }).catch((e) => toast(e.message, "error"));
      btns.append(show);
    }
    li.append(cb, info, players, btns);
    ul.append(li);
  }
  updateRevBulk(list);
  updateBar();
}

function player(label, src) {
  const p = el("div", "player");
  p.append(el("span", "label", label));
  const a = el("audio");
  a.controls = true;
  a.preload = "none";
  a.src = src;
  p.append(a);
  return p;
}

function updateRevBulk(list) {
  const sel = list.filter((it) => S.revSel.has(it.key));
  $("revGood").disabled = sel.length === 0;
  $("revBad").disabled = sel.length === 0;
  const eligible = list.filter((it) => ["verified", "reviewed", "rejected"].includes(it.state));
  $("revSelAll").disabled = eligible.length === 0;
  $("revSelAll").checked = eligible.length > 0 && eligible.every((it) => S.revSel.has(it.key));
  $("revCount").textContent = list.length
    ? `${list.length} recording${list.length === 1 ? "" : "s"}` + (sel.length ? ` · ${sel.length} selected` : "")
    : "Nothing here yet";
}

async function review(keys, ok) {
  try {
    await api("POST", "/api/review", { keys, ok });
    for (const k of keys) S.revSel.delete(k);
    await loadItems();
  } catch (e) {
    toast(e.message, "error");
  }
}

function askCleanup() {
  const notes = cleanupReady();
  if (!notes.length) return;
  $("cleanN").textContent = `${notes.length} original${notes.length === 1 ? "" : "s"}`;
  $("confirmCleanup").hidden = false;
  $("cleanupBtn").hidden = true;
  clearTimeout(S.confirmTimer);
  S.confirmTimer = setTimeout(() => { $("confirmCleanup").hidden = true; updateBar(); }, 8000);
}

async function cleanup() {
  clearTimeout(S.confirmTimer);
  $("confirmCleanup").hidden = true;
  const notes = cleanupReady();
  try {
    const r = await api("POST", "/api/cleanup", { notes });
    toast(`Removing originals from ${r.queued} note${r.queued === 1 ? "" : "s"}. Backups stay in your backups folder.`);
  } catch (e) {
    toast(e.message, "error");
  }
  updateBar();
}

/* ---------- settings ---------- */

function renderSettings() {
  const s = S.settings;
  if (!s) return;
  for (const r of document.querySelectorAll("input[name=preset]")) r.checked = r.value === s.preset;
  if (document.activeElement !== $("backupDir")) $("backupDir").value = s.backupDir || "";
  $("interval").value = s.intervalMs;
  $("intervalOut").textContent = (s.intervalMs / 1000).toFixed(2).replace(/0$/, "") + " s";
  $("tagNotes").checked = !!s.tagNotes;
  $("trustAuto").checked = !!s.trustAutoCheck;
  if (document.activeElement !== $("clientId")) $("clientId").value = s.clientId || "";
  if (document.activeElement !== $("clientMeta")) $("clientMeta").value = s.clientMetadataUrl || "";
}

async function saveSettings(patch) {
  try {
    S.settings = await api("PUT", "/api/settings", { ...S.settings, ...patch });
    renderSettings();
    updateBar();
  } catch (e) {
    toast(e.message, "error");
    renderSettings();
  }
}

/* ---------- tabs and wiring ---------- */

function showTab(name) {
  S.tab = name;
  for (const t of document.querySelectorAll(".tab")) t.classList.toggle("on", t.dataset.tab === name);
  $("pageFind").hidden = name !== "find";
  $("pageReview").hidden = name !== "review";
  $("pageSettings").hidden = name !== "settings";
  if (name === "review") loadItems();
  updateBar();
}

function wire() {
  for (const t of document.querySelectorAll(".tab")) t.onclick = () => showTab(t.dataset.tab);
  $("connectBtn").onclick = () => api("POST", "/api/connect").catch((e) => toast(e.message, "error"));
  $("filters").onsubmit = search;
  $("scanBtn").onclick = scan;
  $("show").onchange = renderRows;
  $("selAll").onchange = () => {
    for (const r of visibleRows()) {
      if (!selectable(r)) continue;
      if ($("selAll").checked) S.selected.add(r.noteId); else S.selected.delete(r.noteId);
    }
    renderRows();
  };
  $("convertBtn").onclick = convertSelected;
  $("stopBtn").onclick = () => api("POST", "/api/stop").then(() => toast("Stopped. Everything done so far is saved.")).catch((e) => toast(e.message, "error"));
  $("resumeBtn").onclick = () => api("POST", "/api/resume").catch(() => {});
  $("cleanupBtn").onclick = askCleanup;
  $("confirmCleanup").onclick = cleanup;
  $("reviewShow").onchange = () => { S.revSel.clear(); renderReview(); };
  $("revSelAll").onchange = () => {
    for (const it of reviewList()) {
      if (!["verified", "reviewed", "rejected"].includes(it.state)) continue;
      if ($("revSelAll").checked) S.revSel.add(it.key); else S.revSel.delete(it.key);
    }
    renderReview();
  };
  $("revGood").onclick = () => review([...S.revSel], true);
  $("revBad").onclick = () => review([...S.revSel], false);

  for (const r of document.querySelectorAll("input[name=preset]")) r.onchange = () => saveSettings({ preset: r.value });
  $("backupDir").onchange = () => saveSettings({ backupDir: $("backupDir").value });
  $("pickBackup").onclick = () => api("POST", "/api/pick/backup").then((s) => { if (!s.cancelled) { S.settings = s; renderSettings(); } }).catch((e) => toast(e.message, "error"));
  $("showBackup").onclick = () => api("POST", "/api/reveal", {}).catch((e) => toast(e.message, "error"));
  $("interval").oninput = () => { $("intervalOut").textContent = ($("interval").value / 1000).toFixed(2).replace(/0$/, "") + " s"; };
  $("interval").onchange = () => saveSettings({ intervalMs: +$("interval").value });
  $("tagNotes").onchange = () => saveSettings({ tagNotes: $("tagNotes").checked });
  $("trustAuto").onchange = () => saveSettings({ trustAutoCheck: $("trustAuto").checked });
  $("clientId").onchange = () => saveSettings({ clientId: $("clientId").value.trim() });
  $("clientMeta").onchange = () => saveSettings({ clientMetadataUrl: $("clientMeta").value.trim() });
  $("signOut").onclick = () => api("POST", "/api/signout").then(() => toast("Signed out. Your sign-in was removed from the Keychain.")).catch((e) => toast(e.message, "error"));
  $("quitBtn").onclick = () => api("POST", "/api/quit").catch(() => {});

  // The end date starts at today.
  $("to").value = new Date().toISOString().slice(0, 10);
}

wire();
connectEvents();
loadItems();
