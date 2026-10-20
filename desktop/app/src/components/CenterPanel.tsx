import { memo, useEffect, useRef, useState } from 'react'
import Narrative from './Narrative'
import Composer from './Composer'
import ApprovalPanel from './ApprovalPanel'
import AskUserQuestionPanel from './AskUserQuestionPanel'
import { useAppStore, titleKey, skeletonFinishExit } from '../store/useAppStore'
import { useT } from '../i18n'

// 顶部「最近会话」横条（类似 VS Code 最近打开文件）：最近点开的 session 标签，
// 点击切换；⌘/Ctrl+1..5 快捷键切换（见 App.tsx 全局快捷键）。上限 5（与视图缓存一致）。
function RecentBar() {
  const t = useT()
  const recentSessions = useAppStore((s) => s.recentSessions)
  const activeSessionId = useAppStore((s) => s.activeSessionId)
  const workspace = useAppStore((s) => s.workspace)
  const projects = useAppStore((s) => s.projects)
  const titleOverrides = useAppStore((s) => s.titleOverrides)
  const setActiveSession = useAppStore((s) => s.setActiveSession)
  const removeRecentSession = useAppStore((s) => s.removeRecentSession)
  if (recentSessions.length === 0) return null
  // 标题：优先 titleOverrides，否则 project.sessions 里的 title，否则会话 id 短尾
  const titleOf = (path: string, id: string): string => {
    const key = titleKey(path, id)
    const ov = titleOverrides[key]
    if (ov) return ov
    const proj = projects.find((p) => p.path === path)
    const meta = proj?.sessions.find((x) => x.id === id)
    if (meta?.title) return meta.title
    return id.length > 12 ? `…${id.slice(-10)}` : id
  }
  return (
    <div className="recent-bar" role="tablist" aria-label={t('recent.sessions')}>
      {recentSessions.map((r, i) => {
        const active = r.id === activeSessionId && r.path === workspace
        const hotkey = i === 9 ? '⌘0' : `⌘${i + 1}` // 第 10 个用 ⌘0
        return (
          <button
            key={`${r.path}:${r.id}`}
            className={`recent-tab ${active ? 'active' : ''}`}
            role="tab"
            aria-selected={active}
            title={`${titleOf(r.path, r.id)} (${t('recent.switch')} ${hotkey})`}
            onClick={() => setActiveSession(r.id, r.path)}
          >
            <span className="recent-idx mono">{i === 9 ? '0' : i + 1}</span>
            <span className="recent-title">{titleOf(r.path, r.id)}</span>
            <span
              className="recent-close"
              role="button"
              tabIndex={0}
              title={t('recent.close')}
              aria-label={t('recent.close')}
              onClick={(e) => { e.stopPropagation(); removeRecentSession(r.path, r.id) }}
              onKeyDown={(e) => {
                if (e.key === 'Enter' || e.key === ' ') {
                  e.preventDefault(); e.stopPropagation()
                  removeRecentSession(r.path, r.id)
                }
              }}
            >
              ✕
            </span>
          </button>
        )
      })}
    </div>
  )
}

// SessionSkeleton 切换会话时的骨架屏（替代原先的纯模糊遮罩）。
//
// ## 形状依据（读真实数据得出，不是凭感觉编）
//
// 统计真实会话（3520 块）的 block kind 分布：
//   tool        49.5%   ← 近一半！工具块是**带边框的卡片组**，不是一段文字
//   assistant   46.2%
//   agent        2.0%   子智能体卡（也是卡片）
//   user         1.0%
//   artifacts    0.9%
//   compression / divider / system  各 ~0.1%
//
// 第一版骨架屏的错误：几乎全是正文条、工具块只占一两行。真实对话里
// **每两条消息就夹一组工具卡片**，这才是它的视觉指纹。
//
// ## 自适应高度
//
// 行数不能写死：1000px 窗口里 7 行会留一大片空白（用户实机截图反馈），
// 而 600px 窗口里 14 行又会被截断。
// 这里用 ResizeObserver 量容器真实高度，按「估算块高」生成**刚好铺满 + 溢出一屏**
// 的行，超出部分由底部遮罩柔化 —— 无论窗口多高都不会露硬边界。
//
// 为什么不精确测量：此时没有任何真实内容，精确测量必然错。目标是「密度和
// 形态像」，不是「尺寸对得上」。
// —— 骨架屏的行生成器 ——
//
// 依据真实会话的 block kind 分布（tool 49.5% / assistant 46.2% / user 1% /
// agent 2% / artifacts 1%），按**接近真实的顺序与比例**铺行：
//   问答轮通常长这样：user → assistant(长) → tool组(若干行) → assistant(继续)
// 而不是「正文、正文、正文、工具」这种均质排列。
type SkRow =
  | { kind: 'text'; who: number; widths: number[] }
  | { kind: 'tool'; lines: number; widths?: number[] }
  // 子智能体卡（真实占比 2%，但很显眼）：带边框卡片 + 头部行 + 内部若干条转录
  | { kind: 'agent'; lines: number }
  // 分隔线（divider / compression）：一条细横线，真实占比 ~0.2%
  | { kind: 'divider' }

// 工具名称占位宽度（px）：模拟 bash / read_file / TaskOutput 等长短不一的工具名。
const SK_TOOL_NAME_W = [34, 62, 48, 78, 40, 56, 88, 30, 68, 44, 52, 74]
// 每块的大致高度（px），用于把「容器高度」换算成「需要几块」。
// 数值来自真实 CSS：.msg 内外边距 + 行高；工具组是卡片，更高一些。
const SK_H_TEXT_1 = 58
const SK_H_TEXT_N = 30 // 每多一行正文 +30
const SK_H_TOOL_BASE = 22 // 卡片内边距 + 边框
const SK_H_TOOL_LINE = 26 // 每行工具调用
const SK_ROW_GAP = 22 // .ss-rows 的 gap

// 一个「巡回复」句式：模拟真实对话的节奏，循环使用。
const SK_PATTERN: SkRow[] = [
  { kind: 'text', who: 28, widths: [58] },                       // user 提问（短）
  { kind: 'text', who: 24, widths: [92, 88, 64] },               // assistant 回答
  { kind: 'tool', lines: 1 },                                    // 单条工具
  { kind: 'text', who: 24, widths: [90, 94, 76, 52] },           // assistant 继续
  { kind: 'tool', lines: 3 },                                    // 并行工具组（常见）
  { kind: 'text', who: 24, widths: [88, 70] },
  { kind: 'tool', lines: 2 },
  { kind: 'agent', lines: 3 },                                   // 子智能体卡（2%，显眼）
  { kind: 'text', who: 26, widths: [91, 84, 93, 62] },           // 长回答
  { kind: 'divider' },                                           // 分隔线（压缩/重启）
  { kind: 'tool', lines: 1 },
  { kind: 'text', who: 28, widths: [56] },                       // 下一轮 user
]

function skRowHeight(r: SkRow): number {
  if (r.kind === 'tool') return SK_H_TOOL_BASE + r.lines * SK_H_TOOL_LINE + SK_ROW_GAP
  if (r.kind === 'agent') return SK_H_TOOL_BASE + 24 + r.lines * SK_H_TOOL_LINE + SK_ROW_GAP
  if (r.kind === 'divider') return 40 + SK_ROW_GAP
  const n = r.widths.length
  return SK_H_TEXT_1 + (n > 1 ? (n - 1) * SK_H_TEXT_N : 0) + SK_ROW_GAP
}

// buildSkRows 按容器高度生成「刚好铺满 + 溢出一屏」的行。
//
// 溢出是刻意的：底部有遮罩淡出，溢出让视觉上「内容还在继续」，
// 不会在高窗口里出现「加载到一半就没了」的断头感。
function buildSkRows(height: number): SkRow[] {
  // 溢出量 1.35：足够让遮罩完全吃掉最后一行的边缘。
  //
  // 1.15 的问题（实机截图发现）：最后一个元素可能恰好是一张**工具卡片**，
  // 被遮罩从中间切断 → 上半张卡片悬在半空，看起来像渲染出错。
  // 1.35 保证「溢出部分远超一个块高」（最大块约 130px，遮罩起始在 62%），
  // 切割点总是落在已充分淡出的区域。
  const target = Math.max(240, height) * 1.35
  const out: SkRow[] = []
  let used = 0
  let i = 0
  let nameIdx = 0
  while (used < target && out.length < 40) {
    const tpl = SK_PATTERN[i % SK_PATTERN.length]
    // 拷贝一份：工具名宽度按下标轮换，让每组工具卡长得不一样
    let row: SkRow
    if (tpl.kind === 'tool') {
      row = {
        ...tpl,
        widths: Array.from({ length: tpl.lines }, () => SK_TOOL_NAME_W[nameIdx++ % SK_TOOL_NAME_W.length]),
      }
    } else if (tpl.kind === 'text') {
      row = { ...tpl, widths: [...tpl.widths] }
    } else {
      row = { ...tpl } // agent / divider：无可变量
    }
    out.push(row)
    used += skRowHeight(row)
    i++
  }
  return out
}

const SessionSkeleton = memo(function SessionSkeleton({
  label,
  leaving,
}: {
  label: string
  // leaving：数据已到位，正在淡出。此时下方真实内容已同时挂载并开始渐入，
  // 两者交叉淡化 —— 不给用户看「骨架消失 → 空白 → 内容出现」的三段跳。
  leaving?: boolean
}) {
  const hostRef = useRef<HTMLDivElement>(null)
  const [rows, setRows] = useState<SkRow[]>(() => buildSkRows(720))

  // 量容器高度 → 重新生成行。用 ResizeObserver 而非只在 mount 时读一次：
  // 窗口拖拽/侧栏折叠都会改变叙述区高度，骨架屏停留期间也要跟着变。
  useEffect(() => {
    const el = hostRef.current
    if (!el) return
    const measure = () => {
      const h = el.clientHeight
      if (h > 0) setRows(buildSkRows(h))
    }
    measure()
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  return (
    <div
      ref={hostRef}
      className={`session-skeleton${leaving ? ' is-leaving' : ''}`}
      role="status"
      aria-label={label}
      aria-busy={!leaving}
      // 退场动画播完 → 通知 store 卸载。只在 leaving 时响应：进场动画
      // （ssFadeIn）与高光带的无限动画也会冒泡到 animationend，必须过滤。
      onAnimationEnd={(e) => {
        if (leaving && e.target === e.currentTarget) skeletonFinishExit()
      }}
    >
      <div className="ss-sheen" aria-hidden="true" />
      <div className="ss-rows" aria-hidden="true">
        {rows.map((r, i) => (
          <div
            key={i}
            className="ss-row"
            style={{ animationDelay: `${Math.min(i, 14) * 34}ms` }}
          >
            {r.kind === 'divider' ? (
              <div className="ss-divider" />
            ) : r.kind === 'agent' ? (
              // 子智能体卡：卡片外壳 + 头部（状态点 + 标题）+ 内部转录行
              <div className="ss-tool-group ss-agent-card">
                <div className="ss-tool-line">
                  <span className="ss-dot" />
                  <span className="ss-bar ss-bar-toolname" style={{ width: '92px' }} />
                  <span className="ss-bar ss-bar-toolarg" style={{ flex: 1 }} />
                </div>
                {Array.from({ length: r.lines }, (_, j) => (
                  <div className="ss-tool-line" key={j}>
                    <span className="ss-bar" style={{ width: `${88 - j * 14}%`, height: 9 }} />
                  </div>
                ))}
              </div>
            ) : r.kind === 'tool' ? (
              // 工具卡片组：与真实 .tool-group 同构（边框 + 圆角 + 内部若干行）
              <div className="ss-tool-group">
                {Array.from({ length: r.lines }, (_, j) => (
                  <div className="ss-tool-line" key={j}>
                    <span className="ss-bar ss-bar-toolname" style={{ width: `${r.widths?.[j] ?? 46}px` }} />
                    <span className="ss-bar ss-bar-toolarg" style={{ flex: 1 }} />
                  </div>
                ))}
              </div>
            ) : (
              <>
                {/* who 占位（`用户` / `HAI` / 子智能体标签） */}
                <span className="ss-bar ss-bar-who" style={{ width: r.who }} />
                {r.widths?.map((w, j) => (
                  <span key={j} className="ss-bar" style={{ width: `${w}%` }} />
                ))}
              </>
            )}
          </div>
        ))}
      </div>
    </div>
  )
})

export default function CenterPanel() {
  const t = useT()
  // issue 4：切换会话时的短暂加载态 → 叙述区模糊遮罩（有缓存约一帧、无缓存等激活，超时兜底）
  // 骨架屏状态机：pending 时不渲染（还没到显示阈值），visible/exit 才渲染。
  //
  // pending 阶段刻意什么都不显示：那一刻内容还没到，但加载也很快（<120ms），
  // 用户视线里保持上一帧/空白，比闪一下骨架屏更平顺。
  const skeletonPhase = useAppStore((s) => s.skeleton.phase)
  const showSkeleton = skeletonPhase === 'visible' || skeletonPhase === 'exit'
  return (
    <section className="center">
      <RecentBar />
      <div className="narrative-wrap">
        <Narrative />
        {showSkeleton && <SessionSkeleton label={t('common.loading')} leaving={skeletonPhase === 'exit'} />}
      </div>
      {/* 决策停靠区（HITL，非模态）：审批 + ask_user 提问都停靠在输入框上方 —— 待决策时叙述区
          照常滚动/选中/切会话，用户可以先翻对话历史、看清 diff 再拍板（用户 2026-09-20 报障）。 */}
      <ApprovalPanel />
      <AskUserQuestionPanel />
      <Composer />
    </section>
  )
}
