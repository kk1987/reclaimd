import * as api from './api.js';
import * as I from './i18n.js';
import * as C from './charts.js';
import { decodeProfile } from './codec.js';
import { connect } from './stream.js';

const $ = (id) => document.getElementById(id);

const state = {
  disks: [],
  selected: null,
  detail: null,
  rounds: [],
  events: [],
  profile: null,
  stack: [],
  freshness: null,
  stackMode: 'abs',
  /* Disks whose scan has been asked for but has not started yet. The request
     only moves next_scan_at; the round begins on the daemon's next enumeration
     tick, and until then the disk still reports scanning:false. Without this,
     the button that sent the request is live again the moment the reply lands
     and the same round gets asked for two or three times. */
  scanRequested: new Map(), // key -> ms timestamp the wait gives up at
};

/* Blocks per superblock. The daemon's own segment size is 32 MiB at 1 MiB
   blocks; the mod histogram and the stubborn-region scan both key off it. */
const BLOCKS_PER_SEGMENT = 32;

/* The daemon enumerates every 30 seconds, so a requested round can be that long
   in coming. Three ticks is the outside case; past that the request went
   nowhere -- the stick was pulled, maintenance was switched off -- and the
   button is better back than dead. */
const SCAN_REQUEST_GRACE_MS = 90_000;

function markScanRequested(key) {
  state.scanRequested.set(key, Date.now() + SCAN_REQUEST_GRACE_MS);
}

/* A request is spent once the round it asked for is running, and equally once
   nothing can start: no such disk, gone, or no longer maintained. */
function reapScanRequests() {
  const now = Date.now();
  for (const [key, until] of state.scanRequested) {
    const d = state.disks.find((x) => x.key === key);
    if (!d || d.scanning || !d.present || !d.enabled || now > until) {
      state.scanRequested.delete(key);
    }
  }
}

/* ---------------------------------------------------------------- boot ---- */

async function boot() {
  I.applyStatic();
  wireChrome();
  await refreshAll();
  connect({
    onHello: (d) => { if (d.disks) { state.disks = d.disks; renderDiskbar(); } setConn('live'); },
    onProgress: onProgress,
    onState: async () => {
      /* Scanning on or off is exactly what these events report, and the disk
         card draws that from the list rather than from the detail. Refreshing
         only the detail leaves the card saying Scanning after the round ended,
         and its button disabled with it. */
      try { state.disks = await api.getDisks(); } catch (e) { /* below reports */ }
      await refreshDetail(true);
    },
    onResync: () => refreshAll(),
    onStatus: setConn,
  });
  scheduleRedraw();
}

function wireChrome() {
  setConn(); // label the initial idle dot; it has no meaning until it has one
  document.querySelectorAll('[data-lang]').forEach((b) => {
    b.setAttribute('aria-pressed', String(b.dataset.lang === I.lang()));
    b.addEventListener('click', () => {
      I.setLang(b.dataset.lang);
      document.querySelectorAll('[data-lang]').forEach((o) =>
        o.setAttribute('aria-pressed', String(o.dataset.lang === I.lang())));
      I.applyStatic();
      setConn();
      setTheme();
      /* Re-render from the cached payloads: switching language must not cost a
         single request, which matters when the server is a router. */
      renderAll();
    });
  });

  setTheme(); // three-state cycle: a label that only says "toggle" says nothing
  $('theme-btn').addEventListener('click', () => {
    const cur = document.documentElement.getAttribute('data-theme');
    const next = cur === 'dark' ? 'light' : cur === 'light' ? null : 'dark';
    /* Going back to "follow the system" means REMOVING the attribute, not
       setting it to "auto" -- the CSS keys off its absence. */
    if (next) document.documentElement.setAttribute('data-theme', next);
    else document.documentElement.removeAttribute('data-theme');
    try {
      if (next) localStorage.setItem('rcl.theme', next);
      else localStorage.removeItem('rcl.theme');
    } catch (e) {}
    setTheme();
    scheduleRedraw();
  });

  window.addEventListener('hashchange', () => {
    const k = decodeURIComponent(location.hash.replace(/^#\/disk\//, ''));
    if (k && k !== state.selected) { state.selected = k; refreshDetail(); }
  });

  document.querySelectorAll('#stack-mode [data-mode]').forEach((b) => {
    b.addEventListener('click', () => {
      state.stackMode = b.dataset.mode;
      document.querySelectorAll('#stack-mode [data-mode]').forEach((o) =>
        o.setAttribute('aria-pressed', String(o.dataset.mode === state.stackMode)));
      renderStack();
    });
  });

  new ResizeObserver(scheduleRedraw).observe($('map-canvas'));
  matchMedia('(prefers-color-scheme: dark)').addEventListener('change', scheduleRedraw);
  new MutationObserver(scheduleRedraw).observe(document.documentElement,
    { attributes: true, attributeFilter: ['data-theme'] });
}

async function refreshAll() {
  try {
    state.disks = await api.getDisks();
  } catch (e) { setConn('down'); return; }
  if (!state.selected || !state.disks.some((d) => d.key === state.selected)) {
    state.selected = state.disks[0]?.key || null;
  }
  renderDiskbar();
  await refreshDetail();
}

async function refreshDetail(soft) {
  if (!state.selected) { renderAll(); return; }
  try {
    const [detail, rounds, events] = await Promise.all([
      api.getDisk(state.selected),
      api.getRounds(state.selected, 60),
      api.getEvents(state.selected, 120),
    ]);
    state.detail = detail;
    state.rounds = rounds;
    state.events = events;
    if (!soft || !state.profile) await loadProfiles();
  } catch (e) { setConn('down'); }
  renderAll();
}

async function loadProfiles() {
  state.profile = null;
  state.stack = [];
  try {
    state.profile = decodeProfile(await api.getProfile(state.selected, 'latest'));
  } catch (e) { /* no pass yet */ }

  try {
    const buf = await api.getFreshness(state.selected);
    state.freshness = new Uint32Array(buf);
  } catch (e) { state.freshness = null; }

  /* Only completed passes are fetched for the waterfall, and those are
     immutable and cached, so this costs one request per pass ever. */
  const seqs = state.rounds.map((r) => r.seq).filter(Boolean).slice(-10);
  for (const seq of seqs) {
    try {
      state.stack.push(decodeProfile(await api.getProfile(state.selected, seq)));
    } catch (e) { /* pruned out of retention */ }
  }
}

/* ------------------------------------------------------------- render ---- */

function renderAll() {
  I.applyStatic();
  renderDiskbar();
  renderVerdict();
  renderControllers();
  renderMap();
  renderStack();
  renderFreshness();
  renderStructure();
  renderTrends();
  renderEvents();
  renderFooter();
  renderLive(state.detail?.live || null);
}

function renderDiskbar() {
  const bar = $('diskbar');
  bar.innerHTML = '';
  reapScanRequests();
  for (const d of state.disks) {
    const el = document.createElement('article');
    el.className = 'diskcard';
    el.dataset.enabled = String(d.enabled);
    if (d.key === state.selected) el.setAttribute('aria-current', 'true');
    const name = `${d.identity?.vendor || ''} ${d.identity?.model || d.key}`.trim();
    const size = d.identity?.size_bytes ? I.fmtBytes(d.identity.size_bytes) : '';
    /* "Generic Flash Disk" is what a whole class of sticks calls itself, so the
       name alone cannot tell two of them apart. The serial can, and it is the
       one printed on the device -- the same string `refresh` demands back as
       its confirmation. Fall back to the key, which is unique even when the
       stick reports no serial at all. */
    const ident = d.identity?.serial || d.key;
    let status = d.present ? '' : I.t('disk.absent');
    if (d.scanning) status = I.t('disk.scanning');
    else if (!d.enabled) status = I.t('disk.disabled');
    else if (!d.adopted) status = I.t('disk.probation');

    /* "Scan now" is only an action when a scan is not the current state. It
       used to sit there enabled mid-round, where pressing it set next_scan_at
       to now and nothing else happened -- an control that looks like it worked
       and did nothing. There is no stop: a round backs off on its own terms,
       and cutting one short mid-pread is not something a button should offer. */
    const scanning = d.scanning;
    const requested = state.scanRequested.has(d.key);
    const scannable = d.present && d.enabled && !scanning && !requested;
    const scanTitle = scanning ? 'disk.scanRunning'
      : requested ? 'disk.scanQueued'
      : !d.present ? 'disk.absent'
      : !d.enabled ? 'disk.scanNeedsMaintain'
      : 'disk.scanNow';

    el.innerHTML = `
      <div class="row"><span class="dot" data-grade="${d.health?.grade || 'unknown'}"></span>
        <h3 class="wrapy">${esc(name)}</h3></div>
      <div class="row mono" style="font-size:11.5px;color:var(--ink-3)">
        <span>${esc(size)}</span><span>${esc(status)}</span></div>
      <div class="row mono" style="font-size:11.5px;color:var(--ink-3)">
        <span title="${esc(I.t('disk.identBy'))}">${esc(ident)}</span></div>
      <div class="row"><span class="mono" style="font-size:11.5px">${
        d.next_scan_ts ? I.fmtRel(d.next_scan_ts) : '—'}</span>
        <span style="display:flex;gap:4px">
          <button type="button" class="iconbtn" data-scan="${esc(d.key)}"
            title="${esc(I.t(scanTitle))}" ${scannable ? '' : 'disabled'}>▶</button>
          <button type="button" class="iconbtn" data-toggle="${esc(d.key)}"
            role="switch" aria-checked="${d.enabled}"
            title="${esc(I.t(d.enabled ? 'disk.excludeAction' : 'disk.maintainAction'))}"
            >${d.enabled ? '◉' : '◎'}</button>
        </span>
      </div>`;
    el.addEventListener('click', (ev) => {
      if (ev.target.dataset.toggle || ev.target.dataset.scan) return;
      state.selected = d.key;
      location.hash = '#/disk/' + encodeURIComponent(d.key);
      refreshDetail();
    });
    el.querySelector('[data-scan]').addEventListener('click', async (ev) => {
      ev.stopPropagation();
      if (!scannable) return;
      // Disabled on the click, not on the answer: the gap between the two is
      // the race, and it is a network round trip wide.
      ev.currentTarget.disabled = true;
      markScanRequested(d.key);
      try {
        await api.requestScan(d.key);
        await refreshAll();
      } catch (err) {
        state.scanRequested.delete(d.key);
        renderDiskbar();
        if (err.code === 'SCAN_IN_PROGRESS') { toast(I.t('disk.scanRunning')); return; }
        if (err.code !== 'SCAN_SUPPRESSED') { toast(err.message); return; }
        /* The refusal is the window doing its job, so the way past it is a
           second, separate act rather than a retry of the same click: the
           dialog names what opened the window and how much of it is left, and
           the override is recorded as an event either way. */
        const left = d.suppress_until_ts
          ? I.fmtDur(d.suppress_until_ts - Math.floor(Date.now() / 1000)) : '';
        const why = I.t(d.last_outcome === 'dropout'
          ? 'reason.SUPPRESSED_AFTER_DROPOUT' : 'reason.SUPPRESSED_AFTER_NEAR_HANG');
        if (!confirm(I.t('confirm.override', { left, why }))) return;
        markScanRequested(d.key);
        renderDiskbar();
        try {
          await api.requestScan(d.key, true);
          toast(I.t('toast.overridden'));
          await refreshAll();
        } catch (e2) {
          state.scanRequested.delete(d.key);
          renderDiskbar();
          toast(e2.message);
        }
      }
    });
    el.querySelector('[data-toggle]').addEventListener('click', async (ev) => {
      ev.stopPropagation();
      try {
        await api.setEnabled(d.key, !d.enabled);
        toast(I.t(d.enabled ? 'toast.disabled' : 'toast.enabled'));
        await refreshAll();
      } catch (err) { toast(err.message); }
    });
    bar.appendChild(el);
  }
  bar.hidden = state.disks.length === 0;
}

function renderVerdict() {
  const d = state.detail;
  const sec = $('verdict');
  if (!d) { sec.dataset.grade = 'unknown'; $('verdict-line').textContent = I.t('grade.unknown'); return; }
  const h = d.health || { grade: 'unknown', rule: 'NO_COMPLETE_PASS' };
  sec.dataset.grade = h.grade;
  $('verdict-line').textContent = I.t('grade.' + h.grade);
  /* The rule that fired is printed, not a score. A score would answer "how
     bad"; the question is "what should I do about it". */
  $('verdict-why').textContent = I.t('rule.' + h.rule, I.fmtParams(h.params));

  $('kpi-next').textContent = d.next_scan_ts ? I.fmtRel(d.next_scan_ts) : '—';
  $('kpi-last').textContent = d.last_scan_ts ? I.fmtRel(d.last_scan_ts) : '—';
  const iv = (d.controllers || []).find((c) => c.id === 'scan_interval');
  $('kpi-interval').textContent = iv ? I.fmtDur(iv.value * 3600) : '—';
  $('kpi-healed').textContent = I.fmtNum(d.healed_total_n || 0);
}

function renderControllers() {
  const host = $('ctrl-rows');
  host.innerHTML = '';
  for (const c of state.detail?.controllers || []) {
    const el = document.createElement('details');
    el.className = 'ctrl';
    const val = c.unit === 'h' ? I.fmtDur(c.value * 3600)
      : c.unit === 'ms' ? I.fmtLatency(c.value)
      : c.unit === 'x' ? c.value + '×' : String(c.value);
    const why = I.t('reason.' + c.reason, I.fmtParams(c.params));

    let body = '';
    if (c.formula) body += `<div><span class="cap">${I.t('ctrl.formula')}</span> <code>${esc(c.formula)}</code></div>`;
    if (c.whatif) {
      /* The strongest single element on the page: it turns "trust the adaptive
         algorithm" into three numbers anybody can check next week. */
      const w = c.whatif;
      body += `<div class="whatif"><span class="cap">${I.t('ctrl.whatif')}</span>` +
        `<span>${I.t('whatif.clean', { v: I.fmtDur(w.clean_h * 3600) })}</span>` +
        `<span>${I.t('whatif.slow', { v: I.fmtDur(w.slow_h * 3600) })}</span>` +
        `<span>${I.t('whatif.dropout', { v: I.fmtDur(w.dropout_h * 3600) })}</span></div>`;
    }
    el.innerHTML = `<summary>
        <span class="k">${I.t('ctrl.' + c.id)}</span>
        <span class="v">${esc(val)}</span>
        <span class="why wrapy">${esc(why)}</span>
      </summary><div class="body">${body}</div>`;
    host.appendChild(el);
  }
}

function renderMap() {
  const p = state.profile;
  const label = $('map-label'), stat = $('map-stat');
  if (!p) {
    label.textContent = I.t('map.none');
    stat.textContent = '';
    $('map-legend').innerHTML = '';
    C.drawStrip($('map-canvas'), null);
    $('map-axis').innerHTML = '';
    return;
  }
  const last = state.rounds[state.rounds.length - 1];
  label.textContent = I.fmtAbs(p.startedTs);
  stat.textContent = I.t('map.stat', {
    seq: p.seq,
    slow: I.fmtNum(last?.slow_blocks_n || 0),
    drop: I.fmtNum(last?.dropouts_n || 0),
    base: I.fmtLatency(p.baselineMs),
  });
  const live = state.detail?.live;
  C.drawStrip($('map-canvas'), p, {
    cursorMiB: live ? live.pos_mib : null,
    totalMiB: live ? live.total_mib : null,
  });
  $('map-legend').innerHTML = C.legendHTML(p.baselineMs);

  const totalGiB = (p.count * p.blockSize) / (1 << 30);
  const ticks = [];
  for (let i = 0; i <= 7; i++) ticks.push(Math.round((totalGiB * i) / 7));
  $('map-axis').innerHTML = ticks.map((g, i) =>
    `<span>${g}${i === ticks.length - 1 ? ' GiB' : ''}</span>`).join('');
}

function renderStack() {
  const sec = $('stack');
  if (state.stack.length < 2) { sec.hidden = true; return; }
  sec.hidden = false;
  C.drawStack($('stack-canvas'), state.stack, { rowH: 16, mode: state.stackMode });
  $('stack-legend').innerHTML = state.stackMode === 'abs'
    ? C.legendHTML(state.stack[state.stack.length - 1].baselineMs)
    : `<span><i style="background:var(--diff-better-2)"></i>${I.t('stack.legend.better')}</span>
       <span><i style="background:var(--diff-same)"></i>${I.t('stack.legend.same')}</span>
       <span><i style="background:var(--diff-worse-2)"></i>${I.t('stack.legend.worse')}</span>
       <span class="cap" style="flex:1 1 100%">${I.t('stack.diffNote')}</span>`;
  $('stack-labels').innerHTML = state.stack.map((p) =>
    `<div style="height:18px">${I.fmtStamp(p.startedTs)}</div>`).join('');
  $('stack-stats').innerHTML = state.stack.map((p) => {
    const r = state.rounds.find((x) => x.seq === p.seq);
    return `<div style="height:18px">${r ? (r.slow_blocks_n || 0) + '/' + (r.dropouts_n || 0) : ''}</div>`;
  }).join('');
}

function renderFreshness() {
  const sec = $('fresh');
  if (!state.freshness || !state.freshness.length) { sec.hidden = true; return; }
  sec.hidden = false;
  const iv = state.detail?.interval_s || 0;
  const counts = C.drawFreshness($('fresh-canvas'), state.freshness, iv);

  const oldest = state.detail?.oldest_data_s;
  const never = Array.prototype.reduce.call(state.freshness, (n, v) => n + (v === 0 ? 1 : 0), 0);
  /* The single most honest measure of what this tool is protecting, so it gets
     headline size rather than a row in a table. */
  let head = oldest ? I.t('fresh.oldest', { age: I.fmtDur(oldest) }) : '';
  if (never > 0) head += `<span class="est"> · ${I.t('fresh.never', { n: never })}</span>`;
  $('fresh-oldest').innerHTML = head;

  $('fresh-legend').innerHTML = [0, 1, 2, 3].map((b) =>
    `<span><i style="background:var(--fresh-${b})"></i>${I.t('fresh.legend.' + b)}
     <span class="mono">${counts[b]}</span></span>`).join('');

  const p = state.profile;
  if (p) {
    const totalGiB = (p.count * p.blockSize) / (1 << 30);
    $('fresh-axis').innerHTML = [0, 1, 2, 3, 4, 5, 6, 7].map((i, n, a) =>
      `<span>${Math.round((totalGiB * i) / 7)}${i === 7 ? ' GiB' : ''}</span>`).join('');
  }
}

function renderStructure() {
  const sec = $('structure');
  if (!state.profile) { sec.hidden = true; return; }
  sec.hidden = false;

  const segMiB = (BLOCKS_PER_SEGMENT * state.profile.blockSize) / (1 << 20);
  const r = C.svgModHistogram($('mod-hist'), state.profile, BLOCKS_PER_SEGMENT);
  if (r && r.totalSlow > 0 && r.peak === BLOCKS_PER_SEGMENT - 1) {
    $('mod-caption').textContent = I.t('structure.caption', {
      seg: segMiB + ' MiB', peak: r.peak, peakMs: I.fmtLatency(r.peakMs) });
  } else {
    $('mod-caption').textContent = I.t('structure.captionFlat', { seg: segMiB + ' MiB' });
  }

  const stub = C.findStubborn(state.stack.length ? state.stack : [state.profile],
    BLOCKS_PER_SEGMENT);
  const list = $('stubborn');
  list.innerHTML = '';
  if (!stub.length) {
    list.innerHTML = `<li class="empty">${esc(I.t('structure.stubbornNone'))}</li>`;
    return;
  }
  for (const sgm of stub) {
    const li = document.createElement('li');
    li.innerHTML = `<span>${esc(I.fmtMiB(sgm.offsetMiB))}</span>
      <span class="n">${esc(I.t('structure.streak', { n: sgm.streak }))}</span>`;
    list.appendChild(li);
  }
}

function renderTrends() {
  const sec = $('trends');
  if (state.rounds.length < 2) { sec.hidden = true; return; }
  sec.hidden = false;
  C.svgTrends($('trend-svg'), state.rounds);
}

function renderEvents() {
  const list = $('event-list');
  list.innerHTML = '';
  const items = state.events.slice(-120).reverse();
  for (const e of items) {
    const li = document.createElement('li');
    const at = e.offset ? I.t('event.at', { off: I.fmtMiB(e.offset / (1 << 20)) }) : '';
    const params = I.fmtParams(e.params);
    const extra = Object.entries(params)
      .filter(([k]) => !['offsetMib'].includes(k))
      .slice(0, 3).map(([k, v]) => `${k}=${v}`).join(' · ');
    li.innerHTML = `<time datetime="${new Date(e.ts).toISOString()}">${
      I.fmtStamp(new Date(e.ts).getTime() / 1000)}</time>
      <span class="chip" data-t="${esc(e.type)}">${esc(I.t('event.' + e.type))}</span>
      <span class="wrapy mono" style="font-size:12px;color:var(--ink-3)">${esc(at)} ${esc(extra)}</span>`;
    list.appendChild(li);
  }
}

function renderFooter() {
  const w = state.detail?.bytes_written_by_tool || 0;
  /* A tool whose reason for existing is wear should publish its own wear bill. */
  $('foot-stats').textContent = I.t('foot.written', { written: I.fmtBytes(w) });
}

function renderLive(live) {
  const sec = $('live');
  if (!live) { sec.hidden = true; return; }
  sec.hidden = false;
  const pct = live.total_mib ? (live.done_mib / live.total_mib) * 100 : 0;
  $('live-fill').style.width = pct.toFixed(1) + '%';
  sec.querySelector('.bar').setAttribute('aria-valuenow', pct.toFixed(0));
  $('lv-pos').textContent = `${I.fmtMiB(live.pos_mib)} / ${I.fmtMiB(live.total_mib)}`;
  $('lv-speed').textContent = I.fmtNum(live.speed_mibs_n, 1) + ' MiB/s';
  $('lv-drift').textContent = `${live.drift_n}× (${I.fmtLatency(live.lat_p50_ms)} / ${I.fmtLatency(live.base_p50_ms)})`;
  $('lv-eta').textContent = I.fmtDur(live.eta_s);
  $('lv-found').textContent = I.t('live.found.fmt', {
    slow: live.slow_n, danger: live.danger_n, defer: live.defer_n });
  C.svgDutyGauge($('duty-gauge'), live.drift_n || 1, 1.25);
}

function onProgress(p) {
  if (!state.detail || p.disk !== state.selected) return;
  state.detail.live = p;
  renderLive(p);
  if (state.profile) {
    C.drawStrip($('map-canvas'), state.profile,
      { cursorMiB: p.pos_mib, totalMiB: p.total_mib });
  }
}

/* --------------------------------------------------------------- misc ---- */

let redrawPending = false;
function scheduleRedraw() {
  if (redrawPending) return;
  redrawPending = true;
  requestAnimationFrame(() => {
    redrawPending = false;
    if (state.profile) {
      const live = state.detail?.live;
      C.drawStrip($('map-canvas'), state.profile,
        { cursorMiB: live?.pos_mib, totalMiB: live?.total_mib });
      $('map-legend').innerHTML = C.legendHTML(state.profile.baselineMs);
    }
    if (state.stack.length >= 2) {
      C.drawStack($('stack-canvas'), state.stack, { rowH: 16, mode: state.stackMode });
    }
    if (state.freshness) {
      C.drawFreshness($('fresh-canvas'), state.freshness, state.detail?.interval_s || 0);
    }
    if (state.profile) C.svgModHistogram($('mod-hist'), state.profile, BLOCKS_PER_SEGMENT);
    if (state.rounds.length >= 2) C.svgTrends($('trend-svg'), state.rounds);
  });
}

// A coloured dot with no accessible name is a dot nobody can read. The label
// is the only thing that says what green means, so it is set with the state
// rather than left to a legend somewhere else on the page. The state is kept
// so a language switch can relabel it without waiting for the next change.
// The theme button cycles dark -> light -> follow the system, so its label has
// to name the state it is in as well as where the next press goes. Absent the
// attribute means "follow the system", which is the state with no name of its
// own in the DOM.
function setTheme() {
  const el = $('theme-btn');
  const label = I.t('theme.' + (document.documentElement.getAttribute('data-theme') || 'system'));
  el.title = label;
  el.setAttribute('aria-label', label);
}

function setConn(s) {
  const el = $('conn');
  if (s) el.dataset.state = s;
  const label = I.t('conn.' + el.dataset.state);
  el.title = label;
  el.setAttribute('aria-label', label);
}

let toastTimer = null;
function toast(msg) {
  const el = $('toast');
  el.textContent = msg;
  el.dataset.show = '1';
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.dataset.show = '0'; }, 2600);
}

function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

boot();
