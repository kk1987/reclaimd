/* The daemon emits no prose, only enum codes and numbers whose key carries a
   unit suffix. This module turns them into Chinese or English. Keeping that
   boundary here is what keeps the API translatable. */

export const DICT = {
  zh: {
    'app.tagline': 'USB 闪存保持力维护',
    'sys.host': '主机', 'sys.build': '提交 {commit}，构建于 {date}',
    'a11y.skip': '跳到正文',
    'a11y.langGroup': '语言', 'a11y.diskbar': '磁盘列表', 'a11y.unitGroup': '容量单位',
    'units.mib': '二进制：1 MiB = 1024² 字节',
    'units.mb': '十进制：1 MB = 1000² 字节，盘上标的容量和速度用的就是它',
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
    'adaptive.lede': '这里的每个数都是从这支盘自己的数据算出来的。点任意一行查看细节。',
    'ctrl.scan_interval': '扫描间隔', 'ctrl.thresholds': '慢块阈值',
    'ctrl.duty_cycle': '占空比温控', 'ctrl.cooldown': '事故后冷却',
    'ctrl.read_size': '单次读取大小', 'ctrl.rewrite': '自动重写',
    'conn.live': '实时连接正常，进度是推送来的',
    'conn.down': '事件流断了，已退回轮询',
    'conn.idle': '标签页隐藏超过 1 分钟，已断流',
    'theme.dark': '当前：深色。点击切浅色',
    'theme.light': '当前：浅色。点击切回跟随系统',
    'theme.system': '当前：跟随系统。点击切深色',
    'ctrl.formula': '公式', 'ctrl.whatif': '下一轮若',
    'whatif.clean': '仍干净 → {v}', 'whatif.slow': '有慢块 → {v}', 'whatif.dropout': '掉线 → {v}',

    'reason.INIT': '初始值，还没有历史可依据。',
    'reason.SCAN_CLEAN_BACKOFF': '最近 {cleanPasses} 轮全部干净，间隔按 ×{factor} 放宽：{prev} → {value}。',
    'reason.SCAN_SLOW_TIGHTEN': '上一轮有慢块，间隔按 ×{factor} 收紧：{prev} → {value}。',
    'reason.SCAN_DANGER_TIGHTEN': '出现接近卡死的块，间隔 ×{factor} 并冷却 {suppress}。',
    'reason.SCAN_DROPOUT_HALVE': '发生掉线，间隔 ×{factor} 并冷却 {suppress}。',
    'reason.SCAN_NEUTRAL_RETRY': '这一轮没测到什么，间隔不动。',
    'reason.SCAN_RESUME_PARTIAL': '这趟读到 {covered_pct}% 就熔断了，不算完整的一趟。{resume} 后从断点接着扫，不用等满 {value}。',
    'reason.CLOCK_REBASED': '时钟同步后重算了下次时间。',
    'reason.ONE_SCSI_COMMAND': '内核给这支盘的单条命令上限是 {max_sectors_kb} KiB，所以一次读恰好是一条 SCSI 命令。',
    'reason.SET_IN_CONFIG': '配置里手动指定的值。内核给这支盘的单条命令上限是 {max_sectors_kb} KiB。',
    'reason.LEARNED_FROM_DISK': '基线 {base} → 慢 {slow} / 危险 {danger}。这两个数是这支盘自己的 p50 乘出来的。',
    'reason.DRIFT_RUNNING': '漂移 {drift}×（阈值 {threshold}×），全速。',
    'reason.DRIFT_RESTING': '漂移 {drift}×（阈值 {threshold}×），正在歇。',
    'reason.NOT_TRIGGERED': '未触发。',
    'reason.SUPPRESSED_AFTER_DROPOUT': '上一轮掉了线，正在冷却：盘从 USB 总线上掉下去过。',
    'reason.SUPPRESSED_AFTER_NEAR_HANG': '上一轮读到接近卡死的块，冷却中。',
    'reason.REWRITE_NO_EVIDENCE': '回探样本还不够（{samples} 块、{rounds} 轮，需要 {minSamples} 块、{minRounds} 轮），还说不准读能不能刷新这支盘。',
    'reason.REWRITE_READS_HEAL': '回探过的 {samples} 块里 {healed} 块读一次就恢复了，仍慢的占 {stillSlowPct}。主控自己会回收，读就够了。',
    'reason.REWRITE_NEEDED_OFF': '回探过的 {samples} 块里 {stillSlow} 块回探时仍然慢（{stillSlowPct}）。读刷新不了这支盘，得重写。自动重写没有打开，在配置里把 rewrite.enabled 设为 true 就会开始。',
    'reason.REWRITE_ACTIVE': '回探过的 {samples} 块里 {stillSlow} 块回探时仍然慢（{stillSlowPct}），读刷新不了这支盘。自动重写已打开。上一次重写了 {rewritten} 块，其中 {rewriteHealed} 块随后恢复正常。',

    'live.title': '正在扫描',
    'live.pos': '位置', 'live.speed': '速度', 'live.drift': '延迟漂移',
    'live.eta': '预计剩余', 'live.found': '本轮发现',
    'live.dutyAria': '占空比温控表',
    'live.found.fmt': '慢 {slow} · 危险 {danger} · 退避 {defer} 段',
    'live.rewritten': '已重写 {n}',

    'map.title': '全盘延迟地图',
    'map.note': '每一列取区间内最慢的那一块。',
    'map.legend.normal': '正常', 'map.legend.slow': '慢',
    'map.legend.bad': '很慢', 'map.legend.danger': '危险',
    'map.legend.drop': '掉线', 'map.legend.skip': '未测量',
    'map.stat': '第 {seq} 轮 · 慢 {slow} · 掉线 {drop} · 基线 {base}',
    'map.none': '还没有扫描数据',

    'stack.abs': '绝对', 'stack.prev': '对比上一轮', 'stack.first': '对比首轮',
    'stack.legend.better': '变好', 'stack.legend.same': '两轮都正常', 'stack.legend.worse': '变差',
    'stack.diffNote': '差分模式会隐藏两轮都正常的格子，否则健康盘本身的正常抖动会铺满整张图。这里的「变好」是推算出来的，经回探证实的治愈记在事件日志里。',

    'fresh.title': '新鲜度地图',
    'fresh.oldest': '最老的一块数据已经 {age} 没被读过',
    'fresh.oldestRead': '读到过的数据里，最老的一块已经 {age} 没被读过',
    'fresh.never': '还有 {n} 段从未被读到过',
    'fresh.caveat': '这里只统计本工具自己的读取。',
    'fresh.legend.0': '刚读过', 'fresh.legend.1': '正常老化',
    'fresh.legend.2': '逾期', 'fresh.legend.3': '陈旧/从未读到',

    'structure.title': '物理结构',
    'structure.aria': '按 superblock 内偏移的最慢延迟',
    'structure.caption': '按 offset mod {seg} 分组的最慢延迟。峰值在偏移 {peak}（{peakMs}），也就是每个擦除块的最后一个位置，NAND 上最脆弱的那条 wordline。',
    'structure.captionFlat': '按 offset mod {seg} 分组的最慢延迟。目前没有明显峰值，说明退化还没有沿物理版图排开。',
    'structure.stubborn': '顽固区',
    'structure.stubbornNote': '连续多轮偏慢、且从未回探成功的段。99% 的退化块读一次就自愈，剩下的才值得看。可以考虑对它们跑 reclaimd refresh -range=。',
    'structure.stubbornNone': '没有顽固区。',
    'structure.streak': '连续 {n} 轮',

    'speed.title': '读速测试',
    'speed.lede': '在盘上均匀取 {n} 处，每处顺序全速读 {mib}，不限速也不休息，总共约 {budget}。这类盘的读速跟着数据年龄走：刚写的数据能跑到标称速度，放久了的只剩零头，所以各处的差距就是数据新旧的差距。',
    'speed.run': '开始测速', 'speed.running': '测速中…',
    'speed.none': '还没测过。',
    'speed.headline': '平均 {avg} · 最慢 {min}（{minAt} 处）· 最快 {max}（{maxAt} 处）',
    'speed.when': '{when} · 读了 {read}，用时 {dur} · p50 {p50}，最慢的块 {max}',
    'speed.aria': '各位置的读速',
    'speed.region': '{speed} · p50 {p50} · 最慢块 {max}',
    'speed.stop.budget': '到时间，读到 {mib}',
    'speed.stop.near_hang': '遇到接近卡死的块，读到 {mib} 停下',
    'speed.stop.error': '设备掉线',
    'speed.unreadable': '{n} 块读不出',
    'speed.outcome.dropout': '测到一半掉线，结果只到掉线前。',
    'speed.outcome.cancelled': '测速被中断。',
    'speed.note': '规则和扫描一样：正在扫描或冷却中的盘不测。命令行 reclaimd speed -disk= 也能测，但不记录结果。',
    'toast.speedStarted': '测速开始，约 {budget}',
    'toast.speedRunning': '正在测速',
    'confirm.overrideSpeed': '冷却还剩 {left}。\n\n{why}\n\n测速是全速读，跳过冷却现在就测吗？',

    'rewrite.title': '重写这支盘',
    'rewrite.lede': '读只能治好主控自己判定为边缘的块。重写会把每一块都过一遍新的编程周期，不管主控怎么判定。这个页面不执行重写，只把整盘重写的命令拼好。守护进程自己的自动重写只针对回探后仍慢的块，默认关闭，状态见决策台。',
    'rewrite.dryLabel': '先空跑，确认瞄准的是这支盘',
    'rewrite.runLabel': '确认无误后，真正重写',
    'rewrite.copy': '复制',
    'rewrite.copied': '命令已复制',
    'rewrite.copyFail': '复制不了，请手动选中命令',
    'rewrite.gates': '在这支盘插着的那台机器上跑。跑之前先卸载它的所有分区。命令自己也会拦，但先卸载省得白跑一趟。整盘重写按盘速要跑几十分钟到几小时。',
    'rewrite.noSerial': '这支盘没有报告序列号，拼不出 -confirm。请用 reclaimd list 查看它当前的标识，再手工构造命令。',
    'rewrite.absent': '这支盘现在没插着。插上之后这里会给出命令。',

    'stack.title': '治愈瀑布',
    'stack.lede': '每一行是一轮扫描，时间从上往下。坏区表现为逐渐褪色的竖条纹，褪色就是 read reclaim 在把块搬走。',
    'trends.title': '历史趋势', 'trends.aria': '每轮掉线数与慢块数',
    'trends.note': '横轴是真实时间。间隔放宽后，柱子之间的空隙也跟着拉开。',
    'trends.slow': '慢块', 'trends.drop': '掉线',

    'events.title': '事件日志',
    'event.DROPOUT': '掉线', 'event.SLOW': '慢块', 'event.NEAR_HANG': '接近卡死',
    'event.HEALED': '已治愈', 'event.STILL_SLOW': '仍然慢', 'event.MEDIA_ERROR': '介质错误',
    'event.DEFER': '退避', 'event.ROUND': '轮次结束', 'event.INTERVAL_CHANGE': '间隔变更',
    'event.ADOPTED': '纳入维护', 'event.PASS_RESUMED': '续扫',
    'event.COOLDOWN_OVERRIDDEN': '手动跳过冷却', 'event.PASS_STOPPED': '手动停止',
    'event.OVERWRITTEN': '已被改写', 'event.REWRITE': '重写', 'event.REWRITE_SKIPPED': '重写未执行',
    'event.SPEED_TEST': '测速',
    'event.at': '位于 {off}',

    'disk.absent': '未插入', 'disk.scanning': '扫描中', 'disk.disabled': '已排除',
    'disk.enable': '纳入维护', 'disk.probation': '观察中', 'disk.scanNow': '立即扫描',
    'disk.identBy': '这支盘的序列号，同名的盘靠它区分',
    'disk.scanRunning': '正在扫描中',
    'disk.scanQueued': '已请求，等待开始',
    'disk.scanNeedsMaintain': '已排除，先纳入维护才能扫',
    'disk.maintainAction': '当前：已排除。点击纳入维护',
    'disk.excludeAction': '当前：纳入维护。点击排除',
    'disk.stopAction': '正在扫描。点击停止这一趟，1 小时后从停下的地方接着扫',
    'disk.stopWait': '正在停止，最多读完当前这一段',
    'disk.stopping': '停止中',
    'disk.forget': '删除这支盘的全部记录',
    'disk.forgetScanning': '正在扫描，扫完再删',
    'foot.method': '读取绕过页缓存直达裸设备：走页缓存读到的是缓存，什么也刷新不了。设备按 USB 序列号识别，掉线改名后自动重新绑定。',
    'foot.written': '本工具至今向此盘写入 {written}',
    'toast.enabled': '已纳入维护', 'toast.disabled': '已排除',
    'toast.suppressed': '上一轮之后还在冷却，暂不扫描',
    'toast.overridden': '已跳过冷却，这一轮马上开始',
    'toast.stopping': '正在停止这一趟',
    'confirm.override': '冷却还剩 {left}。\n\n{why}\n\n跳过它，现在就扫吗？',
    'toast.forgotten': '已删除这支盘的全部记录，列表里不再显示它',
    'confirm.forget': '删除「{name}」的全部记录：历史轮次、延迟图、事件日志，全部消失，无法撤销。\n\n继续吗？',
    'confirm.forgetPresent': '删除「{name}」的全部记录：历史轮次、延迟图、事件日志，全部消失，无法撤销。\n\n这支盘还插着，半分钟内会作为一支陌生盘重新出现，观察期从头开始。\n\n继续吗？',
    'unit.d': '{n} 天', 'unit.h': '{n} 小时', 'unit.m': '{n} 分', 'unit.s': '{n} 秒',
    'none': '无',
  },
  en: {
    'app.tagline': 'USB flash retention upkeep',
    'sys.host': 'host', 'sys.build': 'commit {commit}, built {date}',
    'a11y.skip': 'Skip to content',
    'a11y.langGroup': 'Language', 'a11y.diskbar': 'Disks', 'a11y.unitGroup': 'Byte units',
    'units.mib': 'Binary: 1 MiB = 1024² bytes',
    'units.mb': 'Decimal: 1 MB = 1000² bytes, the unit a drive’s label and rated speed use',
    'verdict.eyebrow': 'Current state',
    'kpi.next': 'Next scan', 'kpi.last': 'Last scan',
    'kpi.interval': 'Interval', 'kpi.healed': 'Blocks healed',

    'grade.good': 'This drive is healthy', 'grade.watch': 'Worth watching',
    'grade.degraded': 'Degrading', 'grade.failing': 'In trouble',
    'grade.unknown': 'No complete pass yet',

    'rule.NO_COMPLETE_PASS': 'No full pass has finished yet.',
    'rule.RECENT_DROPOUT': 'The last pass (#{seq}) dropped off the bus {drop} time(s).',
    'rule.REPEATED_DROPOUTS': '{drop} dropouts across the last {passes} passes.',
    'rule.NEAR_HANG_PRESENT': '{danger} block(s) came close to hanging, past the {dangerThreshold} danger line.',
    'rule.SLOW_OVER_BUDGET': '{slow} of {read} blocks read slow, over the 0.1% budget.',
    'rule.SLOW_PRESENT_UNDER_THRESHOLD': '{slow} slow block(s), none past the {dangerThreshold} danger line.',
    'rule.BASELINE_DRIFT': 'Baseline latency drifted from its best {best} to {now}, up {drift}.',
    'rule.LAST_PASS_CLEAN': 'The last pass (#{seq}) was clean end to end.',

    'adaptive.title': 'Decision desk',
    'adaptive.lede': 'Every number here is worked out from this drive’s own data. Click a row for details.',
    'ctrl.scan_interval': 'Scan interval', 'ctrl.thresholds': 'Slow-block thresholds',
    'ctrl.duty_cycle': 'Duty cycle', 'ctrl.cooldown': 'Cooldown',
    'ctrl.read_size': 'Read size', 'ctrl.rewrite': 'Automatic rewrite',
    'conn.live': 'Live: progress is being pushed',
    'conn.down': 'Event stream is down, falling back to polling',
    'conn.idle': 'Stream closed after the tab was hidden for a minute',
    'theme.dark': 'Dark. Click for light',
    'theme.light': 'Light. Click to follow the system',
    'theme.system': 'Following the system. Click for dark',
    'ctrl.formula': 'Formula', 'ctrl.whatif': 'Next pass if',
    'whatif.clean': 'still clean → {v}', 'whatif.slow': 'slow blocks → {v}', 'whatif.dropout': 'dropout → {v}',

    'reason.INIT': 'Starting value. There is no history to go on yet.',
    'reason.SCAN_CLEAN_BACKOFF': 'Last {cleanPasses} pass(es) clean, so the interval widened ×{factor}: {prev} → {value}.',
    'reason.SCAN_SLOW_TIGHTEN': 'Slow blocks last pass, so the interval tightened ×{factor}: {prev} → {value}.',
    'reason.SCAN_DANGER_TIGHTEN': 'A block came close to hanging: interval ×{factor}, plus a {suppress} cooldown.',
    'reason.SCAN_DROPOUT_HALVE': 'The drive dropped out: interval ×{factor}, plus a {suppress} cooldown.',
    'reason.SCAN_NEUTRAL_RETRY': 'Nothing was measured this pass, so the interval stays put.',
    'reason.SCAN_RESUME_PARTIAL': 'This pass stopped at {covered_pct}% of the disk, so it doesn’t count as a full one. It resumes from where it stopped in {resume}, without waiting the full {value}.',
    'reason.CLOCK_REBASED': 'Recomputed after the clock was corrected.',
    'reason.ONE_SCSI_COMMAND': 'The kernel sends this drive at most {max_sectors_kb} KiB per command, so one read is exactly one SCSI command.',
    'reason.SET_IN_CONFIG': 'Set by hand in the config. The kernel sends this drive at most {max_sectors_kb} KiB per command.',
    'reason.LEARNED_FROM_DISK': 'Baseline {base} → slow {slow} / danger {danger}, both multiples of this drive’s own p50.',
    'reason.DRIFT_RUNNING': 'Drift {drift}× (threshold {threshold}×), running.',
    'reason.DRIFT_RESTING': 'Drift {drift}× (threshold {threshold}×), resting.',
    'reason.NOT_TRIGGERED': 'Not triggered.',
    'reason.SUPPRESSED_AFTER_DROPOUT': 'Cooling down after a dropout: the drive left the USB bus.',
    'reason.SUPPRESSED_AFTER_NEAR_HANG': 'Cooling down after a near-hang.',
    'reason.REWRITE_NO_EVIDENCE': 'Not enough re-probe evidence yet ({samples} blocks over {rounds} passes, {minSamples} over {minRounds} needed) to say whether reading refreshes this drive.',
    'reason.REWRITE_READS_HEAL': '{healed} of {samples} re-probed blocks recovered after a single read and {stillSlowPct} stayed slow. The controller reclaims on its own, so reading is enough.',
    'reason.REWRITE_NEEDED_OFF': '{stillSlow} of {samples} re-probed blocks were still slow at the re-probe ({stillSlowPct}). Reading does not refresh this drive, so it needs rewriting. Automatic rewrite is off. Set rewrite.enabled to true in the config to start it.',
    'reason.REWRITE_ACTIVE': '{stillSlow} of {samples} re-probed blocks were still slow at the re-probe ({stillSlowPct}), so reading does not refresh this drive. Automatic rewrite is on. The last rewrite wrote {rewritten} blocks back, and {rewriteHealed} of them read normally afterwards.',

    'live.title': 'Scanning',
    'live.pos': 'Position', 'live.speed': 'Speed', 'live.drift': 'Latency drift',
    'live.eta': 'Remaining', 'live.found': 'Found so far',
    'live.dutyAria': 'Duty-cycle gauge',
    'live.found.fmt': '{slow} slow · {danger} danger · {defer} segments deferred',
    'live.rewritten': '{n} rewritten',

    'map.title': 'Whole-drive latency map',
    'map.note': 'Each column shows the worst block in its range.',
    'map.legend.normal': 'normal', 'map.legend.slow': 'slow',
    'map.legend.bad': 'very slow', 'map.legend.danger': 'danger',
    'map.legend.drop': 'dropout', 'map.legend.skip': 'not measured',
    'map.stat': 'pass #{seq} · {slow} slow · {drop} dropouts · baseline {base}',
    'map.none': 'No scan data yet',

    'stack.abs': 'Absolute', 'stack.prev': 'vs previous', 'stack.first': 'vs first',
    'stack.legend.better': 'better', 'stack.legend.same': 'both normal', 'stack.legend.worse': 'worse',
    'stack.diffNote': 'Diff mode hides cells that were normal in both passes. Otherwise the ordinary jitter of a healthy drive would fill the chart. "Better" here is inferred. Healing confirmed by a re-probe is listed in the event log.',

    'speed.title': 'Read speed',
    'speed.lede': '{n} stretches spread across the drive, {mib} each, read sequentially flat out with no rate limit and no rests, about {budget} in all. On these drives read speed follows the age of the data: a fresh write reads at the rated speed and an old one at a fraction of it, so the spread between positions is the spread in data age.',
    'speed.run': 'Run speed test', 'speed.running': 'Testing…',
    'speed.none': 'Not tested yet.',
    'speed.headline': 'average {avg} · slowest {min} at {minAt} · fastest {max} at {maxAt}',
    'speed.when': '{when} · {read} read in {dur} · p50 {p50}, slowest block {max}',
    'speed.aria': 'Read speed by position',
    'speed.region': '{speed} · p50 {p50} · slowest block {max}',
    'speed.stop.budget': 'out of time after {mib}',
    'speed.stop.near_hang': 'stopped after {mib} on a block that nearly hung',
    'speed.stop.error': 'the device went away',
    'speed.unreadable': '{n} block(s) unreadable',
    'speed.outcome.dropout': 'The drive dropped out mid-test. This is what was read before it did.',
    'speed.outcome.cancelled': 'The test was interrupted.',
    'speed.note': 'Same rules as a scan: not while the drive is scanning or cooling down. reclaimd speed -disk= runs the same test from a shell, without recording it.',
    'toast.speedStarted': 'Speed test started, about {budget}',
    'toast.speedRunning': 'A speed test is running',
    'confirm.overrideSpeed': '{left} of the cooldown left.\n\n{why}\n\nA speed test reads flat out. Skip the cooldown and test now?',

    'rewrite.title': 'Rewrite this drive',
    'rewrite.lede': 'Reading only heals the blocks the controller judges marginal. Rewriting puts every block through a fresh program cycle, whatever the controller makes of it. This page only assembles the whole-drive command and never runs it. The daemon’s own automatic rewrite covers just the blocks that stayed slow after a re-probe, is off by default, and reports in the decision desk.',
    'rewrite.dryLabel': 'Dry run first, to confirm it’s aimed at this drive',
    'rewrite.runLabel': 'Then the real rewrite',
    'rewrite.copy': 'Copy',
    'rewrite.copied': 'Command copied',
    'rewrite.copyFail': 'Could not copy. Select the command by hand',
    'rewrite.gates': 'Run it on the machine this drive is plugged into. Unmount all of its partitions first. The command refuses otherwise, but unmounting first saves a wasted trip. A whole-drive rewrite takes tens of minutes to hours at this drive’s speed.',
    'rewrite.noSerial': 'This drive reports no serial, so -confirm cannot be filled in. Use reclaimd list to see how it is identified now, and build the command by hand.',
    'rewrite.absent': 'This drive is not plugged in. The command appears once it is.',

    'fresh.title': 'Freshness map',
    'fresh.oldest': 'The stalest data has gone {age} without a read',
    'fresh.oldestRead': 'Of what has been read, the stalest has gone {age} without a read',
    'fresh.never': '{n} segments have never been read',
    'fresh.caveat': 'Only reads by this tool are counted.',
    'fresh.legend.0': 'just read', 'fresh.legend.1': 'aging normally',
    'fresh.legend.2': 'overdue', 'fresh.legend.3': 'stale / never read',

    'structure.title': 'Physical structure',
    'structure.aria': 'Worst latency by offset within a superblock',
    'structure.caption': 'Worst latency grouped by offset mod {seg}. The peak sits at offset {peak} ({peakMs}), the last position in every erase block and the most fragile wordline on the die.',
    'structure.captionFlat': 'Worst latency grouped by offset mod {seg}. No clear peak yet, so degradation has not lined up with the physical layout.',
    'structure.stubborn': 'Stubborn regions',
    'structure.stubbornNote': 'Segments that read slow for several passes running and never came back healed. 99% of degraded blocks fix themselves on the next read. These are the rest, so consider running reclaimd refresh -range= over them.',
    'structure.stubbornNone': 'No stubborn regions.',
    'structure.streak': '{n} passes running',

    'stack.title': 'Healing waterfall',
    'stack.lede': 'One row per pass, time running downward. Bad regions show up as vertical streaks that fade. The fading is read reclaim moving the blocks.',
    'trends.title': 'History', 'trends.aria': 'Dropouts and slow blocks per pass',
    'trends.note': 'The x axis is real time. When the interval widens, the gaps between the bars widen with it.',
    'trends.slow': 'slow', 'trends.drop': 'dropouts',

    'events.title': 'Event log',
    'event.DROPOUT': 'Dropout', 'event.SLOW': 'Slow block', 'event.NEAR_HANG': 'Near hang',
    'event.HEALED': 'Healed', 'event.STILL_SLOW': 'Still slow', 'event.MEDIA_ERROR': 'Media error',
    'event.DEFER': 'Deferred', 'event.ROUND': 'Pass finished', 'event.INTERVAL_CHANGE': 'Interval changed',
    'event.ADOPTED': 'Adopted', 'event.PASS_RESUMED': 'Pass resumed',
    'event.COOLDOWN_OVERRIDDEN': 'Cooldown overridden', 'event.PASS_STOPPED': 'Pass stopped',
    'event.OVERWRITTEN': 'Overwritten', 'event.REWRITE': 'Rewrite', 'event.REWRITE_SKIPPED': 'Rewrite skipped',
    'event.SPEED_TEST': 'Speed test',
    'event.at': 'at {off}',

    'disk.absent': 'not present', 'disk.scanning': 'scanning', 'disk.disabled': 'excluded',
    'disk.enable': 'Maintain', 'disk.probation': 'on probation', 'disk.scanNow': 'Scan now',
    'disk.identBy': 'This drive\u2019s serial, which tells two identically named sticks apart',
    'disk.scanRunning': 'Already scanning',
    'disk.scanQueued': 'Requested, waiting to start',
    'disk.scanNeedsMaintain': 'Excluded. Maintain it before scanning',
    'disk.maintainAction': 'Excluded. Click to maintain',
    'disk.excludeAction': 'Maintained. Click to exclude',
    'disk.stopAction': 'Scanning. Click to stop this pass. It picks up where it left off in an hour',
    'disk.stopWait': 'Stopping. At most the current segment is read first',
    'disk.stopping': 'stopping',
    'disk.forget': 'Delete everything stored for this disk',
    'disk.forgetScanning': 'Scanning. It can be deleted once the pass ends',
    'foot.method': 'Reads bypass the page cache and go straight to the raw device: a buffered read would come from cache and refresh nothing. Devices are identified by their USB serial, so a rename after a dropout rebinds automatically.',
    'foot.written': 'This tool has written {written} to this drive so far',
    'toast.enabled': 'Now maintained', 'toast.disabled': 'Excluded',
    'toast.suppressed': 'Still cooling down after the last pass, not scanning yet',
    'toast.overridden': 'Cooldown skipped. The pass starts now',
    'toast.stopping': 'Stopping this pass',
    'confirm.override': '{left} of the cooldown left.\n\n{why}\n\nSkip it and scan now?',
    'toast.forgotten': 'Deleted everything stored for this disk. It is no longer on the list',
    'confirm.forget': 'Delete everything stored for {name}: past rounds, latency maps, the event log. This cannot be undone.\n\nGo ahead?',
    'confirm.forgetPresent': 'Delete everything stored for {name}: past rounds, latency maps, the event log. This cannot be undone.\n\nThe stick is still plugged in, so it comes back within half a minute as a stranger and starts its probation over.\n\nGo ahead?',
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

/* Byte units, switched for the whole page at once. Binary (MiB, GiB) is the
   default because it is what the daemon counts in; decimal (MB, GB) is what a
   drive's label and its rated speed are printed in. The powers of two that
   describe the hardware itself -- a 32 MiB segment, the kernel's per-command
   limit in KiB -- stay binary either way, since in decimal they are only
   awkward numbers for the same thing. */
let UNITS = 'mib';
try { if (localStorage.getItem('rcl.units') === 'mb') UNITS = 'mb'; } catch (e) {}

export function units() { return UNITS; }

export function setUnits(next) {
  if (next !== 'mib' && next !== 'mb') return;
  UNITS = next;
  try { localStorage.setItem('rcl.units', UNITS); } catch (e) {}
}

/* Unit symbols are untranslated in both languages: GiB reads as GiB to a
   Chinese audience too, and inventing a translation would only add ambiguity. */
export function fmtBytes(b) {
  if (b == null) return '—';
  const k = UNITS === 'mb' ? 1000 : 1024;
  const m = b / (k * k);
  if (m >= k) return NF1.format(m / k) + (UNITS === 'mb' ? ' GB' : ' GiB');
  return NF0.format(m) + (UNITS === 'mb' ? ' MB' : ' MiB');
}

export function fmtMiB(mib) { return mib == null ? '—' : fmtBytes(mib * 1024 * 1024); }

export function fmtSpeed(mibs) {
  if (mibs == null || !isFinite(mibs)) return '—';
  return UNITS === 'mb' ? NF1.format(mibs * 1.048576) + ' MB/s' : NF1.format(mibs) + ' MiB/s';
}

/* What a whole-disk axis is labelled in, and how many bytes one of it is. */
export function gigUnit() {
  return UNITS === 'mb' ? { bytes: 1e9, label: 'GB' } : { bytes: 1024 ** 3, label: 'GiB' };
}

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
