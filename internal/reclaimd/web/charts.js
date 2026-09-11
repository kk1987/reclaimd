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
    diff: [v('--diff-better-2'), v('--diff-better-1'), v('--diff-same'),
           v('--diff-worse-1'), v('--diff-worse-2')],
    fresh: [v('--fresh-0'), v('--fresh-1'), v('--fresh-2'), v('--fresh-3')],
    sunk: v('--sunk'), ink: v('--ink'), accent: v('--accent'), line: v('--line'),
  };
}

/* Each bin keeps its worst value. A bin spans however many blocks the canvas
   width leaves it, and a mean over that many ordinary reads would pull a
   single near-hang down to something indistinguishable from the baseline.
   That outlier is the entire reason the map exists. */
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
   canvas so they share one downsample and one colour scale. Rows that were
   sampled differently would not compare honestly. */
export function drawStack(canvas, profiles, opts = {}) {
  const pal = opts.palette || readPalette();
  const mode = opts.mode || 'abs';
  /* In diff mode every row is compared against one reference: the row above it
     ("prev") or the very first pass ("first"). Comparing against the first pass
     shows the whole story at once, a wall of red turning to a wall of blue. */
  const refFor = (r) => mode === 'prev' ? profiles[r - 1] : profiles[0];
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

  const binCache = profiles.map((p) => downsampleMax(p.values, W, p.baselineMs));

  profiles.forEach((prof, r) => {
    const y = r * (rowH + gap);
    const bins = binCache[r];
    const ref = mode === 'abs' ? null : refFor(r);
    const refBins = ref ? binCache[profiles.indexOf(ref)] : null;

    for (let x = 0; x < W; x++) {
      const b = bins[x];
      if (b.skip) { ctx.fillStyle = pal.skip; ctx.fillRect(x, y, 1, rowH); continue; }
      if (b.code < 0) continue;
      const now = bucketOf(b.code, prof.baselineMs);

      if (!refBins || ref === prof) {
        ctx.fillStyle = pal.heat[now] || pal.heat[0];
      } else {
        const rb = refBins[x];
        const was = (rb && rb.code >= 0) ? bucketOf(rb.code, ref.baselineMs) : null;
        if (was === null) { ctx.fillStyle = pal.skip; }
        else if (was === 0 && now === 0) {
          /* Both ends normal. Masked deliberately: without this the ordinary
             8-12 ms jitter of a healthy drive fills the entire chart with
             noise and buries the handful of cells that actually changed. */
          ctx.fillStyle = pal.diff[2];
        } else {
          const d = Math.max(-2, Math.min(2, now - was));
          ctx.fillStyle = pal.diff[d + 2];
        }
      }
      ctx.fillRect(x, y, 1, rowH);
    }
    ctx.fillStyle = pal.drop;
    for (let x = 0; x < W; x++) if (bins[x].drop) ctx.fillRect(x, y, 1.5, rowH);
  });
}

/* Freshness: how long since this tool last read each segment.
   The scale is keyed to the drive's own current interval instead of absolute
   days, so the map keeps reading as "are we behind schedule?" however the
   schedule has adapted. */
export function drawFreshness(canvas, ages, intervalS, opts = {}) {
  const pal = opts.palette || readPalette();
  const dpr = window.devicePixelRatio || 1;
  const W = canvas.clientWidth || 600;
  const H = canvas.clientHeight || 24;
  canvas.width = Math.round(W * dpr);
  canvas.height = Math.round(H * dpr);
  const ctx = canvas.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.fillStyle = pal.sunk;
  ctx.fillRect(0, 0, W, H);
  if (!ages || !ages.length) return [0, 0, 0, 0];

  const now = Date.now() / 1000;
  const iv = intervalS > 0 ? intervalS : 7 * 86400;
  const bucket = (t) => {
    if (t === 0) return 3;
    const age = now - t;
    return age > iv * 2 ? 3 : age > iv ? 2 : age > iv / 2 ? 1 : 0;
  };
  /* The legend counts segments, as the headline above it does. It used to
     count the columns drawn, and on a disk with more segments than the canvas
     has pixels 3684 never-read segments showed up in the legend as 994. */
  const counts = [0, 0, 0, 0];
  for (const t of ages) counts[bucket(t)]++;
  for (let x = 0; x < W; x++) {
    const lo = Math.floor((x * ages.length) / W);
    const hi = Math.max(lo + 1, Math.floor(((x + 1) * ages.length) / W));
    let b = 0; // the stalest segment in the column is the one at risk
    for (let i = lo; i < hi && i < ages.length; i++) b = Math.max(b, bucket(ages[i]));
    ctx.fillStyle = pal.fresh[b];
    ctx.fillRect(x, 0, 1, H);
  }
  return counts;
}

/* Per-offset-within-superblock latency, the evidence that the daemon has
   worked out the physical layout of the drive in front of it: on the reference
   stick 90.5% of the extreme blocks land on the last position, the final
   wordline of an erase block. */
export function svgModHistogram(el, prof, blocksPerSegment) {
  if (!prof || !blocksPerSegment) { el.innerHTML = ''; return null; }
  const n = blocksPerSegment;
  const worst = new Float64Array(n);
  const count = new Uint32Array(n);
  for (let i = 0; i < prof.values.length; i++) {
    const c = prof.values[i];
    const ms = codeToMs(c);
    if (ms === null) continue;
    const k = i % n;
    if (ms > worst[k]) worst[k] = ms;
    if (bucketOf(c, prof.baselineMs) >= 1) count[k]++;
  }
  const max = Math.max(1, ...worst);
  const peak = worst.indexOf(max);

  const W = 420, H = 190, L = 40, R = 8, T = 14, B = 26;
  const iw = W - L - R, ih = H - T - B, bw = iw / n;
  const parts = [];
  for (const frac of [0, 0.5, 1]) {
    const y = T + ih - frac * ih;
    parts.push(`<line x1="${L}" y1="${y}" x2="${W - R}" y2="${y}" stroke="var(--line)" stroke-width="1"/>`);
    parts.push(`<text x="${L - 6}" y="${y + 3.5}" text-anchor="end" font-size="10"
      font-family="var(--mono)" fill="var(--ink-3)">${Math.round(max * frac)}</text>`);
  }
  for (let k = 0; k < n; k++) {
    const h = (worst[k] / max) * ih;
    parts.push(`<rect x="${(L + k * bw + 0.4).toFixed(1)}" y="${(T + ih - h).toFixed(1)}"
      width="${Math.max(0.8, bw - 0.8).toFixed(1)}" height="${Math.max(h, 0.8).toFixed(1)}"
      fill="${k === peak ? 'var(--heat-3)' : 'var(--ink-3)'}" rx="0.5"
      ><title>+${k}: ${Math.round(worst[k])} ms, ${count[k]} slow</title></rect>`);
  }
  for (const k of [0, Math.floor(n / 4), Math.floor(n / 2), n - 1]) {
    parts.push(`<text x="${(L + k * bw + bw / 2).toFixed(1)}" y="${H - 10}" text-anchor="middle"
      font-size="10" font-family="var(--mono)" fill="var(--ink-3)">${k}</text>`);
  }
  el.innerHTML = parts.join('');
  return { peak, peakMs: max, peakSlow: count[peak],
           totalSlow: count.reduce((a, b) => a + b, 0) };
}

/* Segments that keep coming back slow and never report healed. This is the only
   output of the whole tool that asks for a human: 99% of degraded blocks fix
   themselves on the next read, so the 1% that do not are worth the attention
   that would be wasted on the rest. */
export function findStubborn(profiles, blocksPerSegment, limit = 12) {
  if (profiles.length < 2) return [];
  const newest = profiles[profiles.length - 1];
  const segs = Math.ceil(newest.values.length / blocksPerSegment);
  const out = [];
  for (let seg = 0; seg < segs; seg++) {
    let streak = 0;
    for (let r = profiles.length - 1; r >= 0; r--) {
      const p = profiles[r];
      const lo = seg * blocksPerSegment;
      const hi = Math.min(lo + blocksPerSegment, p.values.length);
      let bad = false, sawGood = false;
      for (let i = lo; i < hi; i++) {
        const c = p.values[i];
        if (c === 0xFFFF) continue;
        if (bucketOf(c, p.baselineMs) >= 1) { bad = true; break; }
        sawGood = true;
      }
      if (bad) streak++;
      else if (sawGood) break; // it read clean at some point, so it is not stuck
      else break;              // only skips from here back, no evidence either way
    }
    if (streak >= 2) {
      out.push({ seg, streak, offsetMiB: (seg * blocksPerSegment * newest.blockSize) / (1 << 20) });
    }
  }
  out.sort((a, b) => b.streak - a.streak);
  return out.slice(0, limit);
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

/* Small multiples on a shared real-time axis. Plotting against pass number
   would hide the thing most worth seeing: when the interval widens, the gaps
   widen with it, which is the adaptive policy visibly working. */
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
