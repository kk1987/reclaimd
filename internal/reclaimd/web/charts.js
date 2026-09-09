import { bucketOf, codeToMs, LAT_SKIPPED, LAT_ERROR } from './codec.js';
import { t, fmtLatency, fmtNum } from './i18n.js';

/* Canvas for the strips (61184 points, far more than pixels, no per-element
   text) and SVG for the charts (few elements, needs axis labels, and inherits
   theme colours from CSS variables for free on a re-theme). */

export function readPalette() {
  const cs = getComputedStyle(document.documentElement);
  const v = (n) => cs.getPropertyValue(n).trim();
  return {
    heat: [v('--heat-0'), v('--heat-1'), v('--heat-2'), v('--heat-3')],
    drop: v('--heat-drop'), skip: v('--heat-skip'),
    sunk: v('--sunk'), ink: v('--ink'), accent: v('--accent'), line: v('--line'),
  };
}

/* Worst-of-bin, never mean. A single 1792 ms block averaged with 153 healthy
   ones reads as 21.6 ms, which is indistinguishable from noise -- and that
   outlier is the entire reason the map exists. */
export function downsampleMax(values, cols, baseMs) {
  const out = new Array(cols);
  const n = values.length;
  for (let x = 0; x < cols; x++) {
    const lo = Math.floor((x * n) / cols);
    const hi = Math.max(lo + 1, Math.floor(((x + 1) * n) / cols));
    let worst = -1, sawDrop = false, sawSkip = false, slowCount = 0, seen = 0;
    for (let i = lo; i < hi && i < n; i++) {
      const c = values[i];
      if (c === LAT_ERROR) { sawDrop = true; continue; }
      if (c === LAT_SKIPPED) { sawSkip = true; continue; }
      seen++;
      if (c > worst) worst = c;
      if (bucketOf(c, baseMs) >= 1) slowCount++;
    }
    out[x] = {
      code: worst, drop: sawDrop, skip: seen === 0 && sawSkip,
      density: seen > 0 ? slowCount / seen : 0, seen,
    };
  }
  return out;
}

export function drawStrip(canvas, prof, opts = {}) {
  const pal = opts.palette || readPalette();
  const dpr = window.devicePixelRatio || 1;
  const W = canvas.clientWidth || 600;
  const H = canvas.clientHeight || 46;
  canvas.width = Math.round(W * dpr);
  canvas.height = Math.round(H * dpr);
  const ctx = canvas.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.fillStyle = pal.sunk;
  ctx.fillRect(0, 0, W, H);
  if (!prof) return null;

  const bins = downsampleMax(prof.values, W, prof.baselineMs);
  const heatH = Math.round(H * 0.7);
  const densH = H - heatH;

  for (let x = 0; x < W; x++) {
    const b = bins[x];
    /* Skipped must never share a colour with fast. A pass that backed off a lot
       is mostly skipped blocks, and painting those as healthy would make a
       deteriorating drive look like it was getting better. */
    if (b.skip) {
      ctx.fillStyle = pal.skip;
      ctx.fillRect(x, 0, 1, heatH);
      continue;
    }
    if (b.code < 0) { continue; }
    ctx.fillStyle = pal.heat[bucketOf(b.code, prof.baselineMs)] || pal.heat[0];
    ctx.fillRect(x, 0, 1, heatH);
    if (b.density > 0 && densH > 0) {
      const h = Math.max(1, Math.round(Math.log1p(b.density * 9) / Math.log(10) * densH));
      ctx.fillStyle = pal.heat[1];
      ctx.fillRect(x, H - h, 1, h);
    }
  }
  /* Dropouts are a full-height tick: shape-encoded, so the information survives
     colour blindness and a monochrome printout. */
  ctx.fillStyle = pal.drop;
  for (let x = 0; x < W; x++) {
    if (bins[x].drop) ctx.fillRect(x, 0, 1.5, H);
  }

  if (opts.cursorMiB != null && opts.totalMiB > 0) {
    const cx = Math.round((opts.cursorMiB / opts.totalMiB) * W);
    ctx.fillStyle = pal.accent;
    ctx.fillRect(cx, 0, 2, H);
  }
  return bins;
}

/* The healing waterfall: one row per pass, time downward, all rows in a single
   canvas so they share one downsample and one colour scale. Comparing rows that
   were sampled differently would be a lie told with a picture. */
export function drawStack(canvas, profiles, opts = {}) {
  const pal = opts.palette || readPalette();
  const rowH = opts.rowH || 16, gap = 2;
  const dpr = window.devicePixelRatio || 1;
  const W = canvas.clientWidth || 600;
  const H = profiles.length * (rowH + gap);
  canvas.style.height = H + 'px';
  canvas.width = Math.round(W * dpr);
  canvas.height = Math.round(H * dpr);
  const ctx = canvas.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.fillStyle = pal.sunk;
  ctx.fillRect(0, 0, W, H);

  profiles.forEach((prof, r) => {
    const y = r * (rowH + gap);
    const bins = downsampleMax(prof.values, W, prof.baselineMs);
    for (let x = 0; x < W; x++) {
      const b = bins[x];
      if (b.skip) { ctx.fillStyle = pal.skip; }
      else if (b.code < 0) { continue; }
      else { ctx.fillStyle = pal.heat[bucketOf(b.code, prof.baselineMs)] || pal.heat[0]; }
      ctx.fillRect(x, y, 1, rowH);
    }
    ctx.fillStyle = pal.drop;
    for (let x = 0; x < W; x++) if (bins[x].drop) ctx.fillRect(x, y, 1.5, rowH);
  });
}

export function svgDutyGauge(el, drift, threshold) {
  const W = 320, H = 56, L = 8, R = 8, y = 30;
  const lo = 0.9, hi = 2.0;
  const pos = (v) => L + ((Math.min(Math.max(v, lo), hi) - lo) / (hi - lo)) * (W - L - R);
  const parts = [];
  parts.push(`<rect x="${L}" y="${y - 5}" width="${W - L - R}" height="10" rx="5" fill="var(--sunk)"/>`);
  parts.push(`<rect x="${L}" y="${y - 5}" width="${pos(drift) - L}" height="10" rx="5" fill="var(--accent)"/>`);
  for (const [v, label, color] of [[1.0, '1.00', 'var(--ink-3)'], [threshold, String(threshold), 'var(--warn)']]) {
    parts.push(`<line x1="${pos(v)}" y1="${y - 11}" x2="${pos(v)}" y2="${y + 11}" stroke="${color}" stroke-width="1.5"/>`);
    parts.push(`<text x="${pos(v)}" y="${y + 24}" text-anchor="middle" font-size="10"
      font-family="var(--mono)" fill="var(--ink-3)">${label}</text>`);
  }
  parts.push(`<text x="${pos(drift)}" y="${y - 15}" text-anchor="middle" font-size="11"
    font-family="var(--mono)" fill="var(--ink)">${drift.toFixed(2)}×</text>`);
  el.innerHTML = parts.join('');
}

/* Small multiples on a shared REAL-TIME axis. Plotting against pass number
   would hide the thing most worth seeing: when the interval widens, the gaps
   widen with it, and that is the adaptive policy visibly working. */
export function svgTrends(el, rounds) {
  if (!rounds.length) { el.innerHTML = ''; return; }
  const W = 720, H = 260, L = 46, R = 12, T = 14, B = 26;
  const rowH = (H - T - B - 20) / 2;
  const times = rounds.map((r) => new Date(r.started_at).getTime() / 1000);
  const t0 = Math.min(...times), t1 = Math.max(...times);
  const span = Math.max(1, t1 - t0);
  const x = (ts) => L + ((ts - t0) / span) * (W - L - R);

  let minGap = span;
  for (let i = 1; i < times.length; i++) minGap = Math.min(minGap, times[i] - times[i - 1] || span);
  const bw = Math.max(3, Math.min(22, ((minGap / span) * (W - L - R)) / 2));

  const parts = [];
  const rows = [
    { key: 'slow_blocks_n', y: T, label: t('trends.slow'), color: 'var(--heat-1)' },
    { key: 'dropouts_n', y: T + rowH + 20, label: t('trends.drop'), color: 'var(--bad)' },
  ];
  for (const row of rows) {
    const vals = rounds.map((r) => r[row.key] || 0);
    const max = Math.max(1, ...vals);
    parts.push(`<line x1="${L}" y1="${row.y + rowH}" x2="${W - R}" y2="${row.y + rowH}"
      stroke="var(--line)" stroke-width="1"/>`);
    parts.push(`<text x="${L - 8}" y="${row.y + 10}" text-anchor="end" font-size="10"
      font-family="var(--mono)" fill="var(--ink-3)">${max}</text>`);
    parts.push(`<text x="${L - 8}" y="${row.y + rowH}" text-anchor="end" font-size="10"
      font-family="var(--mono)" fill="var(--ink-3)">0</text>`);
    parts.push(`<text x="${L}" y="${row.y - 2}" font-size="10.5"
      font-family="var(--sans)" fill="var(--ink-2)">${row.label}</text>`);
    rounds.forEach((r, i) => {
      const v = vals[i];
      const h = (v / max) * rowH;
      const cx = x(times[i]);
      parts.push(`<rect x="${(cx - bw / 2).toFixed(1)}" y="${(row.y + rowH - h).toFixed(1)}"
        width="${bw.toFixed(1)}" height="${Math.max(v > 0 ? 1.5 : 0, h).toFixed(1)}"
        fill="${row.color}" rx="1"><title>#${r.seq}: ${v}</title></rect>`);
    });
  }
  el.innerHTML = parts.join('');
}

export function legendHTML(baseMs) {
  const b = baseMs > 0 ? baseMs : 10;
  const item = (varName, label, hint) =>
    `<span><i style="background:var(--${varName})"></i>${label}${hint ? ' <span class="mono">' + hint + '</span>' : ''}</span>`;
  return [
    item('heat-0', t('map.legend.normal'), '<' + fmtLatency(b * 5)),
    item('heat-1', t('map.legend.slow'), fmtLatency(b * 5) + '+'),
    item('heat-2', t('map.legend.bad'), fmtLatency(b * 20) + '+'),
    item('heat-3', t('map.legend.danger'), fmtLatency(b * 50) + '+'),
    item('heat-drop', t('map.legend.drop'), ''),
    item('heat-skip', t('map.legend.skip'), ''),
  ].join('');
}
