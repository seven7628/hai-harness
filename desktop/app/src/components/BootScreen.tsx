import { useEffect, useRef, useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'

// 品牌时刻逐笔路径（brand/hai-logo.html 定稿）：H 左竖 + 横杠（浓墨）、A 右腿 + I（淡墨）、
// 朱砂共用斜边最后落下 —— 这一笔让 H 和 A 同时成立。
// 时序目标：逐笔描画 ~0.5s 内完成；文本（REVEAL）提前与 A 笔画交叠入场；
// done（TOTAL）触发朱砂脉冲后**定格**（SPLASH_HOLD）：logo + 文本呈现完整、可读清楚，
// 再进入主界面交叉淡化（总计 ≈1.35s）。
const GEO = {
  h:   'M100 300 L132 120',
  bar: 'M115 215 L286 215',
  sh:  'M210 300 L251 70',
  a:   'M251 70 L345 300',
  i:   'M430 300 L462 120',
}
const DUR = [120, 120, 140, 120, 140]
const DEL = [0, 120, 240, 380, 500]
const REVEAL_T = 260 // 文本提前入场（与 A 后半段交叠）
const TOTAL = 500 // done：共用斜边起笔时触发朱砂脉冲（与最后一笔落地交叠）
// done 后再定格 ~500ms：朱砂脉冲过半、文本已读清，然后才进入交叉淡化（总计 ≈1.35s）
const SPLASH_HOLD = 500

export default function BootScreen({ onEnter }: { onEnter: () => void }) {
  const t = useT()
  const start = useAppStore((s) => s.start)
  const autoBoot = useAppStore((s) => s.autoBoot)
  const status = useAppStore((s) => s.bridgeStatus)
  const workspace = useAppStore((s) => s.workspace)
  const error = useAppStore((s) => s.bridgeError)
  const [reveal, setReveal] = useState(false)
  const [done, setDone] = useState(false)
  const logoRef = useRef<SVGSVGElement>(null)
  const onEnterRef = useRef(onEnter)
  onEnterRef.current = onEnter

  // 启动即提前恢复（开屏动画期间后台加载）：
  // 挂载（动画开始）即 autoBoot——有持久工作区 → 立即起 bridge + list + 恢复上次会话，
  // 动画播完（~1.35s）时恢复大概率已完成，进入主界面直接看到内容（无等待）。
  // autoBoot 幂等（booting/bridgeStatus 检查）+ 内部 !workspace 检查 → 首次启动（无工作区）
  // 或重复调用均安全 no-op。done 后不再重复调（动画期间已发起）。
  useEffect(() => {
    autoBoot()
    // 模块初始化时已设 workspace（持久项目）→ start 已跑；无 → no-op（等用户选目录）
  }, [autoBoot])

  // 动画播完（done）后决定去向：
  //   Electron + 已有持久工作区 → 自动恢复上次工作区并进入（多停 SPLASH_HOLD 让朱砂脉冲露出）；
  //   Electron + 无工作区（首次）→ 留在启动屏，淡入「选择工作区」按钮；
  //   浏览器 mock → 自动进假工作区（同样停 SPLASH_HOLD，保持一致体验）。
  useEffect(() => {
    if (!done) return
    if (window.desktop) {
      if (!workspace) return // 首次：等用户选目录
      autoBoot() // 兜底：挂载时若 workspace 未就绪（no-op），此处再试（幂等）
      const timer = setTimeout(() => onEnterRef.current(), SPLASH_HOLD)
      return () => clearTimeout(timer)
    }
    if (workspace) {
      // mock 环境：autoBoot 已在挂载时调用（start 幂等），此处仅保证进入
      const timer = setTimeout(() => onEnterRef.current(), SPLASH_HOLD)
      return () => clearTimeout(timer)
    }
    void start('/mock/workspace')
    const timer = setTimeout(() => onEnterRef.current(), SPLASH_HOLD)
    return () => clearTimeout(timer)
  }, [done])

  // 品牌时刻：逐笔描画 HAI，描完（TOTAL）入场下方文本与按钮。
  // 先全部隐藏（offset=len）并关闭 transition，强制一次布局提交「from」态；
  // 再逐个开描画 transition，rAF 置 0 → 按 DUR/DEL 依次展开（with 斜边最后落下）。
  useEffect(() => {
    const svg = logoRef.current
    if (!svg) return
    const paths = Array.from(svg.querySelectorAll<SVGPathElement>('.b-draw'))
    paths.forEach((p) => {
      const len = p.getTotalLength()
      p.style.strokeDasharray = `${len}`
      p.style.strokeDashoffset = `${len}`
      p.style.transition = 'none'
    })
    void svg.getBoundingClientRect() // 强制 style flush：确保 offset=len 已提交（否则 transition 直接跳终态）
    paths.forEach((p, i) => {
      p.style.transition = `stroke-dashoffset ${DUR[i]}ms ${DEL[i]}ms cubic-bezier(.45,0,.2,1)`
    })
    const raf = requestAnimationFrame(() => {
      paths.forEach((p) => {
        p.style.strokeDashoffset = '0'
      })
    })
    const revealTimer = setTimeout(() => setReveal(true), REVEAL_T)
    const doneTimer = setTimeout(() => setDone(true), TOTAL)
    return () => {
      cancelAnimationFrame(raf)
      clearTimeout(revealTimer)
      clearTimeout(doneTimer)
    }
  }, [])

  const pick = async () => {
    if (!window.desktop) {
      void start('/mock/workspace')
      onEnterRef.current()
      return
    }
    const ws = await window.desktop.pickWorkspace()
    if (ws) {
      void start(ws)
      onEnterRef.current()
    }
  }

  return (
    <div className={`boot${reveal ? ' reveal' : ''}${done ? ' done' : ''}`}>
      <svg ref={logoRef} className="boot-logo" viewBox="0 0 560 380" aria-label="HAI">
        <g fill="none" strokeWidth={22} strokeLinecap="round" strokeLinejoin="round">
          <path className="b-draw b-ink"  stroke="var(--hai-ink)"      d={GEO.h} />
          <path className="b-draw b-ink"  stroke="var(--hai-ink)"      d={GEO.bar} />
          <path className="b-draw b-soft" stroke="var(--hai-ink-soft)" d={GEO.a} />
          <path className="b-draw b-soft" stroke="var(--hai-ink-soft)" d={GEO.i} />
          <path className="b-draw b-with" stroke="var(--hai-with)"     d={GEO.sh} />
        </g>
      </svg>

      <div className="boot-tag" aria-label="嗨 · Human with AI">
        <span className="bt-hi">嗨</span>
        <span className="bt-sub">
          <span className="bt-human">human</span>
          <span className="bt-with">with</span>
          <span className="bt-ai">AI</span>
        </span>
      </div>

      {!workspace && !!window.desktop && (
        <div className="boot-cta">
          <p className="boot-desc">{t('boot.desc')}</p>
          <button className="btn primary" onClick={pick} disabled={status === 'starting'}>
            {status === 'starting' ? t('boot.starting') : t('boot.pick')}
          </button>
          {error && <p className="boot-err">{error}</p>}
        </div>
      )}
    </div>
  )
}
