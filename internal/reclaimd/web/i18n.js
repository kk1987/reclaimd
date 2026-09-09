/* The daemon never emits a sentence. It emits enum codes plus numbers whose key
   carries a unit suffix, and this module turns that into Chinese or English.
   Keeping the boundary here is what makes the API translatable at all. */

export const DICT = {
  zh: {
    'app.tagline': 'USB 闪存保持力维护',
    'a11y.skip': '跳到正文', 'a11y.theme': '切换主题',
    'a11y.langGroup': '语言', 'a11y.diskbar': '磁盘列表',
    'verdict.eyebrow': '当前状态',
    'kpi.next': '下次扫描', 'kpi.last': '上次扫描',
    'kpi.interval': '当前间隔', 'kpi.healed': '累计治愈块',

    'grade.good': '这支盘目前健康', 'grade.watch': '需要留意',
    'grade.degraded': '正在退化', 'grade.failing': '状况不佳',
    'grade.unknown': '还没有完整的扫描记录',

    'rule.NO_COMPLETE_PASS': '还没跑完一整轮，无从判断。',
    'rule.RECENT_DROPOUT': '最近一轮（第 {seq} 轮）发生了 {drop} 次掉线。',
    'rule.REPEATED_DROPOUTS': '最近 {passes} 轮里累计掉线 {drop} 次。',
    'rule.NEAR_HANG_PRESENT': '出现 {danger} 个接近卡死的块（超过 {dangerThreshold} 的危险线）。',
    'rule.SLOW_OVER_BUDGET': '{read} 个已读块里有 {slow} 个偏慢，超过了 0.1% 的容忍度。',
    'rule.SLOW_PRESENT_UNDER_THRESHOLD': '有 {slow} 个慢块，但都没到 {dangerThreshold} 的危险线。',
    'rule.BASELINE_DRIFT': '基线延迟从历史最好的 {best} 漂到了 {now}，涨了 {drift}。',
    'rule.LAST_PASS_CLEAN': '最近一轮（第 {seq} 轮）全程干净。',

    'adaptive.title': '决策台',
    'adaptive.lede': '下面每个数都是这支盘自己推出来的，不是人定的。展开可以看到公式、输入和下一轮会往哪走。',
    'ctrl.scan_interval': '扫描间隔', 'ctrl.thresholds': '慢块阈值',
    'ctrl.duty_cycle': '占空比温控', 'ctrl.dropout_cooldown': '掉线冷却',
    'ctrl.formula': '公式', 'ctrl.whatif': '下一轮若',
    'whatif.clean': '仍干净 → {v}', 'whatif.slow': '有慢块 → {v}', 'whatif.dropout': '掉线 → {v}',

    'reason.INIT': '初始值，还没有历史可依据。',
    'reason.SCAN_CLEAN_BACKOFF': '最近 {cleanPasses} 轮全部干净，间隔按 ×{factor} 放宽：{prev} → {value}。',
    'reason.SCAN_SLOW_TIGHTEN': '上一轮有慢块，间隔按 ×{factor} 收紧：{prev} → {value}。',
    'reason.SCAN_DANGER_TIGHTEN': '出现接近卡死的块，间隔 ×{factor} 并冷却 {suppress}。',
    'reason.SCAN_DROPOUT_HALVE': '发生掉线，间隔 ×{factor} 并冷却 {suppress}。',
    'reason.SCAN_NEUTRAL_RETRY': '这一轮没测到什么，间隔不动。',
    'reason.CLOCK_REBASED': '时钟同步后重算了下次时间。',
    'reason.LEARNED_FROM_DISK': '基线 {base} → 慢 {slow} / 危险 {danger}。这两个数是这支盘自己的 p50 乘出来的。',
    'reason.DRIFT_RUNNING': '漂移 {drift}×（阈值 {threshold}×），全速。',
    'reason.DRIFT_RESTING': '漂移 {drift}×（阈值 {threshold}×），正在歇。',
    'reason.NOT_TRIGGERED': '未触发。',
    'reason.SUPPRESSED_AFTER_DROPOUT': '掉线后冷却中。',

    'live.title': '正在扫描',
    'live.pos': '位置', 'live.speed': '速度', 'live.drift': '延迟漂移',
    'live.eta': '预计剩余', 'live.found': '本轮发现',
    'live.dutyAria': '占空比温控表',
    'live.dutyCaveat': '这支"温度计"用的是读延迟本身。它在盘发热时会升高，在扫到退化区时同样会升高——两种成因分不开。但两种情况都该减速，所以混淆是无害的。',
    'live.found.fmt': '慢 {slow} · 危险 {danger} · 退避 {defer} 段',

    'map.title': '全盘延迟地图',
    'map.note': '每一列取区间内最慢的那一块，不取平均——一个 1792 毫秒的块混进 153 个 10 毫秒的块里，平均值只有 21.6 毫秒，就看不见了。',
    'map.legend.normal': '正常', 'map.legend.slow': '慢',
    'map.legend.bad': '很慢', 'map.legend.danger': '危险',
    'map.legend.drop': '掉线', 'map.legend.skip': '未测量',
    'map.stat': '第 {seq} 轮 · 慢 {slow} · 掉线 {drop} · 基线 {base}',
    'map.none': '还没有扫描数据',

    'stack.abs': '绝对', 'stack.prev': '对比上一轮', 'stack.first': '对比首轮',
    'stack.legend.better': '变好', 'stack.legend.same': '两轮都正常', 'stack.legend.worse': '变差',
    'stack.diffNote': '差分模式屏蔽了两轮都正常的格子——不屏蔽的话，健康盘 8–12 毫秒的正常抖动会铺满整张图，把真正变化的那几格埋掉。这里的"变好"是推导出来的；经回探证实的治愈在事件日志里。',

    'fresh.title': '新鲜度地图',
    'fresh.oldest': '最老的一块数据已经 {age} 没被读过',
    'fresh.never': '还有 {n} 段从未被读到过',
    'fresh.caveat': '这是本工具读到的时间，是新鲜度的下界而不是上界：盘上文件系统自己的读取同样会刷新数据，但从裸设备这一层看不见。色标绑定这支盘当前的自适应间隔，所以间隔变了图仍然读作"有没有落后进度"。',
    'fresh.legend.0': '刚读过', 'fresh.legend.1': '正常老化',
    'fresh.legend.2': '逾期', 'fresh.legend.3': '陈旧/从未读到',

    'structure.title': '物理结构',
    'structure.aria': '按 superblock 内偏移的最慢延迟',
    'structure.caption': '按 offset mod {seg} 分组的最慢延迟。峰值在偏移 {peak}（{peakMs}），也就是每个擦除块的最后一个位置——NAND 上最脆弱的那条 wordline。这是 daemon 自己认出了这支盘的物理版图。',
    'structure.captionFlat': '按 offset mod {seg} 分组的最慢延迟。目前没有明显峰值，说明退化还没有沿物理版图排开。',
    'structure.stubborn': '顽固区',
    'structure.stubbornNote': '连续多轮偏慢、且从未回探成功的段。99% 的退化块读一次就自愈，剩下这些才值得占用注意力——考虑对它们跑一次 reclaimd refresh -range=。',
    'structure.stubbornNone': '没有顽固区，所有退化块都自愈了。',
    'structure.streak': '连续 {n} 轮',

    'stack.title': '治愈瀑布',
    'stack.lede': '每一行是一轮扫描，时间从上往下。坏区表现为逐渐褪色的竖条纹——那就是 read reclaim 在把块搬走。',
    'trends.title': '历史趋势', 'trends.aria': '每轮掉线数与慢块数',
    'trends.note': '横轴是真实时间，不是轮次序号：间隔被放宽时，柱子之间的空隙也会跟着拉开。',
    'trends.slow': '慢块', 'trends.drop': '掉线',

    'events.title': '事件日志',
    'event.DROPOUT': '掉线', 'event.SLOW': '慢块', 'event.NEAR_HANG': '接近卡死',
    'event.HEALED': '已治愈', 'event.STILL_SLOW': '仍然慢', 'event.MEDIA_ERROR': '介质错误',
    'event.DEFER': '退避', 'event.ROUND': '轮次结束', 'event.INTERVAL_CHANGE': '间隔变更',
    'event.ADOPTED': '纳入维护', 'event.PASS_RESUMED': '续扫',
    'event.at': '位于 {off}',

    'disk.absent': '未插入', 'disk.scanning': '扫描中', 'disk.disabled': '已排除',
    'disk.enable': '纳入维护', 'disk.probation': '观察中', 'disk.scanNow': '立即扫描',
    'foot.method': '读取走 O_DIRECT 直接访问裸设备，绕开页缓存——走缓存的话读到的是缓存，什么也刷新不了。设备通过 sysfs 里的 USB 序列号识别，掉线改名后能自动重新绑定。',
    'foot.written': '本工具至今向此盘写入 {written}',
    'toast.enabled': '已纳入维护', 'toast.disabled': '已排除',
    'toast.suppressed': '掉线冷却中，暂不扫描',
    'unit.d': '{n} 天', 'unit.h': '{n} 小时', 'unit.m': '{n} 分', 'unit.s': '{n} 秒',
    'none': '无',
  },
  en: {
    'app.tagline': 'USB flash retention upkeep',
    'a11y.skip': 'Skip to content', 'a11y.theme': 'Toggle theme',
    'a11y.langGroup': 'Language', 'a11y.diskbar': 'Disks',
    'verdict.eyebrow': 'Current state',
    'kpi.next': 'Next scan', 'kpi.last': 'Last scan',
    'kpi.interval': 'Interval', 'kpi.healed': 'Blocks healed',

    'grade.good': 'This drive is healthy', 'grade.watch': 'Worth watching',
    'grade.degraded': 'Degrading', 'grade.failing': 'In trouble',
    'grade.unknown': 'No complete pass yet',

    'rule.NO_COMPLETE_PASS': 'No full pass has finished yet, so there is nothing to judge on.',
    'rule.RECENT_DROPOUT': 'The last pass (#{seq}) dropped off the bus {drop} time(s).',
    'rule.REPEATED_DROPOUTS': '{drop} dropouts across the last {passes} passes.',
    'rule.NEAR_HANG_PRESENT': '{danger} block(s) came close to hanging, past the {dangerThreshold} danger line.',
    'rule.SLOW_OVER_BUDGET': '{slow} of {read} blocks read slow, over the 0.1% budget.',
    'rule.SLOW_PRESENT_UNDER_THRESHOLD': '{slow} slow block(s), none past the {dangerThreshold} danger line.',
    'rule.BASELINE_DRIFT': 'Baseline latency drifted from its best {best} to {now}, up {drift}.',
    'rule.LAST_PASS_CLEAN': 'The last pass (#{seq}) was clean end to end.',

    'adaptive.title': 'Decision desk',
    'adaptive.lede': 'Every number here was derived from this drive, not chosen by hand. Expand a row for the formula, the inputs, and where the next pass would take it.',
    'ctrl.scan_interval': 'Scan interval', 'ctrl.thresholds': 'Slow-block thresholds',
    'ctrl.duty_cycle': 'Duty cycle', 'ctrl.dropout_cooldown': 'Dropout cooldown',
    'ctrl.formula': 'Formula', 'ctrl.whatif': 'Next pass if',
    'whatif.clean': 'still clean → {v}', 'whatif.slow': 'slow blocks → {v}', 'whatif.dropout': 'dropout → {v}',

    'reason.INIT': 'Starting value; no history to go on yet.',
    'reason.SCAN_CLEAN_BACKOFF': 'Last {cleanPasses} pass(es) clean, so the interval widened ×{factor}: {prev} → {value}.',
    'reason.SCAN_SLOW_TIGHTEN': 'Slow blocks last pass, so the interval tightened ×{factor}: {prev} → {value}.',
    'reason.SCAN_DANGER_TIGHTEN': 'Near-hang seen; interval ×{factor} and a {suppress} cooldown.',
    'reason.SCAN_DROPOUT_HALVE': 'Dropout; interval ×{factor} and a {suppress} cooldown.',
    'reason.SCAN_NEUTRAL_RETRY': 'Nothing was measured this pass, so the interval stays put.',
    'reason.CLOCK_REBASED': 'Recomputed after the clock was corrected.',
    'reason.LEARNED_FROM_DISK': 'Baseline {base} → slow {slow} / danger {danger}, both multiples of this drive’s own p50.',
    'reason.DRIFT_RUNNING': 'Drift {drift}× (threshold {threshold}×), running.',
    'reason.DRIFT_RESTING': 'Drift {drift}× (threshold {threshold}×), resting.',
    'reason.NOT_TRIGGERED': 'Not triggered.',
    'reason.SUPPRESSED_AFTER_DROPOUT': 'Cooling down after a dropout.',

    'live.title': 'Scanning',
    'live.pos': 'Position', 'live.speed': 'Speed', 'live.drift': 'Latency drift',
    'live.eta': 'Remaining', 'live.found': 'Found so far',
    'live.dutyAria': 'Duty-cycle gauge',
    'live.dutyCaveat': 'This thermometer is the read latency itself. It rises when the drive heats up and it rises when the sweep enters a degraded region, and the two cannot be told apart here. Both call for slowing down, so the conflation is harmless — but it is a conflation.',
    'live.found.fmt': '{slow} slow · {danger} danger · {defer} segments deferred',

    'map.title': 'Whole-drive latency map',
    'map.note': 'Each column shows the WORST block in its range, never the mean — one 1792 ms block averaged with 153 healthy ones reads as 21.6 ms and disappears.',
    'map.legend.normal': 'normal', 'map.legend.slow': 'slow',
    'map.legend.bad': 'very slow', 'map.legend.danger': 'danger',
    'map.legend.drop': 'dropout', 'map.legend.skip': 'not measured',
    'map.stat': 'pass #{seq} · {slow} slow · {drop} dropouts · baseline {base}',
    'map.none': 'No scan data yet',

    'stack.abs': 'Absolute', 'stack.prev': 'vs previous', 'stack.first': 'vs first',
    'stack.legend.better': 'better', 'stack.legend.same': 'both normal', 'stack.legend.worse': 'worse',
    'stack.diffNote': 'Diff mode masks cells that were normal in both passes. Without that, the ordinary 8-12 ms jitter of a healthy drive fills the chart and buries the handful of cells that actually changed. "Better" here is inferred; healing confirmed by re-probe is in the event log.',

    'fresh.title': 'Freshness map',
    'fresh.oldest': 'The stalest data has gone {age} without a read',
    'fresh.never': '{n} segments have never been read',
    'fresh.caveat': 'This counts reads by this tool, so it is a lower bound on freshness and never an upper one: the filesystem on the drive refreshes data too, and that is invisible from the raw device. The scale is keyed to this drive\u2019s own current interval, so the map keeps reading as "are we behind?" however the schedule adapts.',
    'fresh.legend.0': 'just read', 'fresh.legend.1': 'aging normally',
    'fresh.legend.2': 'overdue', 'fresh.legend.3': 'stale / never read',

    'structure.title': 'Physical structure',
    'structure.aria': 'Worst latency by offset within a superblock',
    'structure.caption': 'Worst latency grouped by offset mod {seg}. The peak sits at offset {peak} ({peakMs}) \u2014 the last position in every erase block, the most fragile wordline on the die. This is the daemon working out the physical layout of the drive in front of it.',
    'structure.captionFlat': 'Worst latency grouped by offset mod {seg}. No clear peak yet, so degradation has not lined up with the physical layout.',
    'structure.stubborn': 'Stubborn regions',
    'structure.stubbornNote': 'Segments that read slow for several passes running and never came back healed. 99% of degraded blocks fix themselves on the next read; these are the ones worth your attention \u2014 consider reclaimd refresh -range= over them.',
    'structure.stubbornNone': 'No stubborn regions: every degraded block healed itself.',
    'structure.streak': '{n} passes running',

    'stack.title': 'Healing waterfall',
    'stack.lede': 'One row per pass, time running downward. Bad regions show up as vertical streaks that fade — that is read reclaim moving the blocks.',
    'trends.title': 'History', 'trends.aria': 'Dropouts and slow blocks per pass',
    'trends.note': 'The x axis is real time, not pass number: when the interval widens, the gaps between bars widen with it.',
    'trends.slow': 'slow', 'trends.drop': 'dropouts',

    'events.title': 'Event log',
    'event.DROPOUT': 'Dropout', 'event.SLOW': 'Slow block', 'event.NEAR_HANG': 'Near hang',
    'event.HEALED': 'Healed', 'event.STILL_SLOW': 'Still slow', 'event.MEDIA_ERROR': 'Media error',
    'event.DEFER': 'Deferred', 'event.ROUND': 'Pass finished', 'event.INTERVAL_CHANGE': 'Interval changed',
    'event.ADOPTED': 'Adopted', 'event.PASS_RESUMED': 'Pass resumed',
    'event.at': 'at {off}',

    'disk.absent': 'not present', 'disk.scanning': 'scanning', 'disk.disabled': 'excluded',
    'disk.enable': 'Maintain', 'disk.probation': 'on probation', 'disk.scanNow': 'Scan now',
    'foot.method': 'Reads go through O_DIRECT straight to the raw device, bypassing the page cache — a buffered read would be served from cache and refresh nothing. Devices are identified by their USB serial in sysfs, so a rename after a dropout rebinds automatically.',
    'foot.written': 'This tool has written {written} to this drive so far',
    'toast.enabled': 'Now maintained', 'toast.disabled': 'Excluded',
    'toast.suppressed': 'In post-dropout cooldown; not scanning',
    'unit.d.one': '{n} day', 'unit.d.other': '{n} days',
    'unit.h.one': '{n} hour', 'unit.h.other': '{n} hours',
    'unit.m.one': '{n} min', 'unit.m.other': '{n} min',
    'unit.s.one': '{n} sec', 'unit.s.other': '{n} sec',
    'none': 'none',
  }
};

let LANG = window.__RCL_LANG__ === 'zh' ? 'zh' : 'en';
let NF0, NF1, PCT, DT, RT, PR;

function buildFormatters(lang) {
  const loc = lang === 'zh' ? 'zh-CN' : 'en';
  /* Built once per language switch rather than per call: on a router-class CPU
     constructing an Intl formatter in a render loop is genuinely measurable. */
  NF0 = new Intl.NumberFormat(loc, { maximumFractionDigits: 0 });
  NF1 = new Intl.NumberFormat(loc, { maximumFractionDigits: 1 });
  PCT = new Intl.NumberFormat(loc, { style: 'percent', maximumFractionDigits: 1 });
  DT = new Intl.DateTimeFormat(loc, { dateStyle: 'medium', timeStyle: 'short' });
  RT = new Intl.RelativeTimeFormat(loc, { numeric: 'auto' });
  PR = new Intl.PluralRules(loc);
}
buildFormatters(LANG);

export function lang() { return LANG; }

export function setLang(next) {
  if (next !== 'zh' && next !== 'en') return;
  LANG = next;
  buildFormatters(LANG);
  document.documentElement.lang = LANG === 'zh' ? 'zh-CN' : 'en';
  /* Written only on an explicit switch, so "follow the browser" stays the
     default until the user actually says otherwise. */
  try { localStorage.setItem('rcl.lang', LANG); } catch (e) {}
}

export function t(key, params) {
  let s = DICT[LANG][key];
  if (s === undefined) s = DICT.en[key];
  if (s === undefined) return key;
  return interpolate(s, params);
}

export function tp(key, n, params) {
  const cat = PR.select(n);
  const d = DICT[LANG];
  const s = d[key + '.' + cat] ?? d[key + '.other'] ?? d[key] ?? DICT.en[key + '.other'] ?? key;
  return interpolate(s, Object.assign({ n: NF0.format(n) }, params));
}

function interpolate(s, params) {
  if (!params) return s;
  return s.replace(/\{(\w+)\}/g, (m, k) => (k in params ? params[k] : m));
}

/* Suffix-typed formatting. The backend tags each number with its unit in the
   field name, so the frontend can format everything correctly without a single
   line of per-field configuration. */
export function fmtParams(params) {
  const out = {};
  if (!params) return out;
  for (const [k, v] of Object.entries(params)) {
    const m = /^(.*?)_(ms|s|h|ts|mib|n|pct)$/.exec(k);
    if (!m) { out[camel(k)] = v; continue; }
    const [, base, unit] = m;
    const name = camel(base);
    switch (unit) {
      case 'ms': out[name] = fmtLatency(v); break;
      case 's': out[name] = fmtDur(v); break;
      case 'h': out[name] = fmtDur(v * 3600); break;
      case 'ts': out[name] = fmtAbs(v); break;
      case 'mib': out[name] = fmtMiB(v); break;
      case 'pct': out[name] = PCT.format(v); break;
      default: out[name] = typeof v === 'number' ? NF0.format(v) : v;
    }
    /* Also expose the base name for templates that were written against the
       un-suffixed key, e.g. {slow} for slow_n. */
    if (!(name in out)) out[name] = v;
  }
  return out;
}

function camel(s) { return s.replace(/_([a-z])/g, (m, c) => c.toUpperCase()); }

export function fmtNum(v, digits) { return (digits ? NF1 : NF0).format(v); }
export function fmtPct(v) { return PCT.format(v); }

export function fmtLatency(ms) {
  if (ms == null) return '—';
  return ms < 100 ? NF1.format(ms) + ' ms' : NF0.format(Math.round(ms)) + ' ms';
}

/* IEC units, untranslated in both languages: GiB reads as GiB to a Chinese
   audience too, and inventing a translation would only add ambiguity. */
export function fmtMiB(mib) {
  if (mib == null) return '—';
  if (mib >= 1024) return NF1.format(mib / 1024) + ' GiB';
  return NF0.format(mib) + ' MiB';
}

export function fmtBytes(b) { return fmtMiB(b / (1024 * 1024)); }

/* Durations are hand-rolled rather than using Intl.DurationFormat: it is too
   new to rely on, and we want full control over the two-unit compact form. */
export function fmtDur(sec) {
  if (sec == null || !isFinite(sec)) return '—';
  sec = Math.max(0, Math.round(sec));
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  if (d > 0) return h > 0 ? tp('unit.d', d) + ' ' + tp('unit.h', h) : tp('unit.d', d);
  if (h > 0) return m > 0 ? tp('unit.h', h) + ' ' + tp('unit.m', m) : tp('unit.h', h);
  if (m > 0) return tp('unit.m', m);
  return tp('unit.s', s);
}

export function fmtAbs(ts) {
  if (!ts) return '—';
  return DT.format(new Date(ts * 1000));
}

/* Log contexts get an unambiguous fixed format in both languages; only prose
   goes through Intl.DateTimeFormat. */
export function fmtStamp(ts) {
  const d = new Date(ts * 1000);
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

export function fmtRel(ts) {
  if (!ts) return '—';
  const diff = ts - Date.now() / 1000;
  const abs = Math.abs(diff);
  const units = [[86400, 'day'], [3600, 'hour'], [60, 'minute'], [1, 'second']];
  for (const [sec, unit] of units) {
    if (abs >= sec || unit === 'second') {
      return RT.format(Math.round(diff / sec), unit);
    }
  }
  return '—';
}

export function applyStatic(root = document) {
  root.querySelectorAll('[data-i18n]').forEach((el) => {
    el.textContent = t(el.getAttribute('data-i18n'));
  });
  root.querySelectorAll('[data-i18n-attr]').forEach((el) => {
    el.getAttribute('data-i18n-attr').split(',').forEach((pair) => {
      const [attr, key] = pair.split(':');
      if (attr && key) el.setAttribute(attr.trim(), t(key.trim()));
    });
  });
}
