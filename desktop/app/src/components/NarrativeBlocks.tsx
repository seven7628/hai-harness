// 叙述块渲染原语：主对话（Narrative）与子 agent 转录（AgentTranscript）**共用同一份实现**。
//
// 为什么单独成文件（2026-09-23 用户报障「右栏子 agent 卡片里的样式和主会话不一样」）：
// AgentTranscript 原本自带一套精简渲染（.at-text / .at-tool），于是同一份数据在两处
// 长得不一样 —— 转录正文是裸文本（列表/行内码不渲染），工具行恒 accent 色、无族色、
// 无行首图标，字号也比主对话小一号；更麻烦的是转录的字体/颜色靠**继承宿主**
// （主对话卡 .agent-card 是 mono + text-2，右栏卡当时是另一套 .agent-view：sans + text），
// 连自己的两处宿主都不同。
// 现在的约定：数据来源可以有两处（MsgBlock / AgentItem），**渲染只能有一套**。
// 新增块类型/改样式时改这里，两处同时生效；皮肤层（narrative-skin.css）也因此一次覆盖两处。
//
// 宿主差异只允许一项：主 run 专属动作（工具行的「后台/停止」）—— 见 ToolRow 的 host。

import { memo, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useAppStore } from '../store/useAppStore'
import type { CompressionInfo, Diff, QueuedContent, ToolErrorInfo, TodoItem, UsageAgg } from '../store/useAppStore'
import { Markdown } from './markdown'
import { Bot, CheckList, FileIcon, PencilIcon, PlayIcon, SearchIcon, Sparkles, TerminalIcon } from './icons'
import { fmtUsd, UsageChip, UnifiedDiff } from './ui'
import { shortPath } from '../lib/shortPath'
import { fmtMs, toolArgsSummary, toolMetric, toolVerb } from '../lib/toolSummary'
import { useSkin } from '../lib/useSkin'
import { FoldIcon } from './runtime/FoldIcon'
import { toolFamily, fileTypeOf } from '../lib/palette'
import { splitThinkingParagraphs } from '../lib/thinkingText'
import { Face } from './runtime/Face'
import { Compact, type CompactHead } from './runtime/blocks/Compact'
import { TodoBlock, type TodoWrite } from './runtime/blocks/Todo'
import JsonView from './JsonView'
import { ElapsedTimer } from './ElapsedTimer'
import { ToolImages } from './ToolImages'
import { ToolLiveTail, ToolNestedCalls, ToolOutputPreview, ToolScriptRun } from './ToolNestedCalls'
import { codemodeTier, hasScriptOutput, isCodemodeTool, scriptSourceOf, scriptSummary, type HostOpsView, type NestedCallView } from '../lib/codemodeNested'
import { splitOutput } from '../lib/logLines'
import { useT } from '../i18n'

/** 工具行渲染的统一数据：主对话 MsgBlock.tool 与子 agent AgentItem.tool 的共同子集。
 *  「出错」两边表达不同（MsgBlock 用 isError，AgentItem 用 status==='error'），
 *  在 ToolRow 里归一化 —— 渲染分支只有一套。 */
export interface ToolRowData {
  toolId: string
  name: string
  args?: string
  result?: string
  status: 'running' | 'done' | 'error'
  isError?: boolean
  errorInfo?: ToolErrorInfo
  diff?: Diff
  images?: QueuedContent[]
  durMs?: number
  startedAt?: number
  taskId?: string
  promoted?: boolean
  interrupting?: boolean
  todos?: TodoItem[]
  usage?: UsageAgg
  /** 编排型工具（codemode）脚本内的嵌套调用清单（外层结果自带；子调用没有自己的行） */
  nestedCalls?: NestedCallView[]
  /** 完整输出落盘路径（结果里点名；缺失 = 不显示该行，见 lib/codemodeNested.ts） */
  spillPath?: string
  /** 编排型工具（codemode）脚本内的保留操作计数（后端 HostOps）——档位判据 */
  hostOps?: HostOpsView
  /** 运行中的输出尾巴（tail -f，**只给人看**；工具结束即清除 —— 见 store 的 tool_output） */
  liveTail?: string
  /** 已累计输出字节数（真值，尾巴只是窗口） */
  liveTotal?: number
  /** 主对话块 id（React key / 定位用；AgentItem 没有） */
  id?: number
}

/** 工具行宿主：main = 主对话（可提供主 run 专属动作）；transcript = 子 agent 转录（只读展示） */
export type ToolRowHost = 'main' | 'transcript'

// 行内码（`code`）渲染：主对话的用户/系统/错误文本与思考段落共用。
// 做成组件（而不是返回 JSX 数组的函数）：与全仓「组件文件只导出组件」的约定一致，
// 且调用点写起来与普通文本节点一样。
export const InlineCode = memo(function InlineCode({ text }: { text: string }) {
  const parts = text.split('`')
  return (
    <>
      {parts.map((p, i) => (i % 2 === 1 ? <code key={i}>{p}</code> : <span key={i}>{p}</span>))}
    </>
  )
})

// 思考过程：流式中完整可见（长文本在自身容器内滚动）；结束后超长则折叠。
// 思考耗时实时计时（本地基准起跳，流式中每秒刷新）+ 结束耗时（thinkMs）。
// 两种形态：还在生成（live 且未结算）= 计时器从 0 逐秒累加；结束 = 关计时器，显示总耗时 thinkMs。
//
// live = 「这一段还在生成」：主对话传 streaming；子 agent 转录传「子 agent 仍在跑且它是
// 最后一项」。它同时决定 完整可见 / 自动贴底 / 计时器走 —— 两处宿主语义不同，
// 判断留在调用点，渲染只在这里。
export const Thinking = memo(function Thinking({ text, live, thinkMs, thinkStart }: { text: string; live: boolean; thinkMs?: number; thinkStart?: number }) {
  const t = useT()
  const rt = useSkin() === 'runtime'
  const [open, setOpen] = useState(false)
  const running = live && thinkStart != null && thinkMs == null // 思考中：还在生成、已开始且未结算
  // 时钟：仅思考中挂（ElapsedTimer 订阅，每秒走）；结束 timer=thinkMs 显示总耗时
  const bodyRef = useRef<HTMLDivElement>(null)
  const long = text.length > 200
  // 设计里思考行永远可开可合（不是「短就不用折」）：短文本也走同一个折叠，
  // 否则这一行的折角在第一帧就是个死图标。流式中保持展开（正在写）。
  const showFull = live || open
  // 折叠交给 CSS 的高度动画（设计 .collapse = grid-template-rows 0fr↔1fr 380ms）：
  // 正文始终整段渲染，折叠只是把它夹住 —— 换文本的话没有过渡，点击会「跳」一下。
  const shown = text
  // 段落切分：思考正文是纯文本 + white-space: pre-wrap，段落之间的空行会渲染成**整个行盒**
  // （行高 21px → 段落间隔 ≈ 2 倍行高），比正文段落间距（.msg .body p 的 4px）松得多。
  // 拆成 <p class="tk-p"> 后间距交给 CSS：默认皮肤沿用原空行高度（观感不变），V3 收紧到正文一致。
  // 切分规则（含围栏代码块边界）见 lib/thinkingText.ts。
  const paras = useMemo(() => splitThinkingParagraphs(shown), [shown])
  // 流式中：内容增长自动滚到自身容器底部（内滚动，不带动整体窗口）
  useEffect(() => {
    if (live && bodyRef.current) bodyRef.current.scrollTop = bodyRef.current.scrollHeight
  }, [text, live])
  return (
    <div className="thinking">
      <div className="thinking-hd">
        <button className="thinking-toggle" onClick={() => setOpen((v) => !v)} aria-expanded={showFull} aria-label={showFull ? t('thinking.collapse') : t('thinking.expand')}>
          {/* V3 皮肤的行首图标：thinking=灵感（默认皮肤下 .ic 不渲染 → 现状不变） */}
          {/* 运行时皮肤的脸：状态由这一段自己的状态给（生成中/展开/折叠），
              不另设一套。`.ic` 留给 v3 皮肤，默认皮肤下它整片 display:none。 */}
          <Face state={running ? 'run' : showFull ? 'engaged' : 'done'} />
          <span className="ic" aria-hidden="true">{<Sparkles size={16} />}</span>
          <span className="mono">{rt ? null : '▸ '}{t('thinking.label')}{<ElapsedTimer startAt={thinkStart} running={running} durMs={thinkMs} />}</span>
          {rt ? <FoldIcon open={showFull} /> : null}
        </button>
        {long && !live && (
          <button className="thinking-more" onClick={() => setOpen((v) => !v)}>
            {open ? t('thinking.collapseAll') : t('thinking.expandAll')}
          </button>
        )}
      </div>
      <div className={`rt-collapse${showFull ? ' open' : ''}`}>
        <div>
          {/* data-live：思考正在写的时候这一块是有界的（设计 .think-body[data-live]：
              只留最后几行 + 顶部渐隐），跑完才按折叠行展开 */}
          <div className={`thinking-body ${long ? 'scroll' : ''}`} data-live={running ? '' : undefined} ref={bodyRef}>
            {paras.map((para, i) => (
              <p className="tk-p" key={i}><InlineCode text={para} /></p>
            ))}
          </div>
        </div>
      </div>
    </div>
  )
})

// 工具调用行：名称(按族着色) + 参数摘要 + 耗时/状态；点击可展开查看完整参数与结果。
// 文件工具（read/write/edit）额外提供路径预览 + 变更查看入口。
// host 只挡一件事：主 run 专属动作（「后台」promote / 「停止」interrupt）——
// 它们作用于**主 run 的工具任务**，子 agent 转录里既不该出现也没有对应语义
//（后台工具任务在右栏「任务」tab 有自己的卡与精确停止）。其余一切（族色 / 行首图标 /
// 参数文件名着色 / 点击打开文件 / 计划卡 / 错误恢复提示）两处完全一致。
export const ToolRow = memo(function ToolRow({ b, host = 'main' }: { b: ToolRowData; host?: ToolRowHost }) {
  const [open, setOpen] = useState(false)
  const [diffOpen, setDiffOpen] = useState(true) // 行内 diff 默认展开（运行时变更立即可见）
  const setRightTab = useAppStore((s) => s.setRightTab)
  const setRightExpanded = useAppStore((s) => s.setRightExpanded)
  const openFile = useAppStore((s) => s.openFile)
  const openHtmlInBrowser = useAppStore((s) => s.openHtmlInBrowser)
  const promoteTask = useAppStore((s) => s.promoteTask)
  const t = useT()
  // 出错归一化：主对话块用 isError，子 agent 项用 status==='error'（两边同义）
  const err = b.isError ?? b.status === 'error'
  // 运行时皮肤的工具行是六列（折角 · 图标 · 动词 · 对象 · 度量 · 时长）：动词用工具名
  // 的大写形式、度量从结果算；另两档皮肤保留原名与旧版面（.t 里仍是原始工具名）。
  const rt = useSkin() === 'runtime'
  const metric = rt ? toolMetric(b.name, b.args || '', b.result, b.diff) : null
  const changed = b.status === 'done' && !err
  const summary = toolArgsSummary(b.name, b.args || '', t)
  // 编排型工具（codemode）：档位徽标（检索 / 执行）——判据见 lib/codemodeNested.ts 的 codemodeTier。
  // 判据用「名字命中 **或** 带编排型字段」：跑着的行还没有结果（hasScriptOutput 为 false），
  // 但徽标该立刻出现 —— 它标的是「这次调用在干什么」，不是「结果长什么样」。
  const isCodemode = isCodemodeTool(b.name) || hasScriptOutput(b)
  // 脚本源码只解析一次（头部摘要、档位兜底判据、折叠块的「查看脚本」三处共用）。
  const scriptSrc = isCodemode ? scriptSourceOf(b.args) : undefined
  // 执行中一律按「执行」渲染：此刻还没有 host_ops / nested_calls，codemodeTier 的兜底
  // 启发式（源码里有 searchTools 且无嵌套 → search）会把一个正要大干一场的脚本标成「检索」——
  // 那是**谎报**。档位是「结果的性质」，没结果时只有「在跑」这一个事实。
  const tier = b.status === 'running' ? 'execute' : codemodeTier(b.hostOps, b.nestedCalls, scriptSrc)
  // codemode 行没有 toolArgsSummary（那是别的工具的参数摘要）——头部摘要改显示脚本干了什么。
  const scriptLine = scriptSrc ? scriptSummary(scriptSrc) : ''
  // SKILL = 命令结果合成的技能加载块（/skill 面板 → SDK /skills load）：渲染成 SKILL(code-review) 工具调用样式
  const isSkill = b.name === 'SKILL'
  // 文件工具：从参数提路径，可点开文件预览（read/write/edit 行内）
  const isFileTool = b.name === 'read_file' || b.name === 'write_file' || b.name === 'edit_file'
  let path = ''
  let startLine: number | undefined
  if (isFileTool) {
    try {
      const parsed = JSON.parse(b.args || '{}') as { path?: string; start_line?: number }
      path = parsed.path ?? ''
      // read_file 带区间：右侧「最终文件」定位滚动到起始行（1-based，负数忽略）
      if (b.name === 'read_file' && typeof parsed.start_line === 'number' && parsed.start_line > 0) {
        startLine = parsed.start_line
      }
    } catch {
      /* 非 JSON 参数：无路径 */
    }
  }
  const toggle = () => setOpen((v) => !v)
  const isWrite = b.name === 'write_file' || b.name === 'edit_file'
  // write_file 落盘 .html → 「打开」在右侧内嵌浏览器以 file:// 加载（内容由用户/
  // LLM 决定落盘位置；代码块本身不猜路径 —— 只有工具卡知道真实 path）。
  const isHtmlWrite = b.name === 'write_file' && /\.html?$/i.test(path)
  // 工具时钟（时钟方案 A）：仅「异步 Task」（promoted / 有 taskId）走实时计时（ElapsedTimer 订阅）；
  // 普通短工具不挂时钟，结束后显示 durMs。
  const running = b.status === 'running'
  const asyncTask = running && (b.promoted || Boolean(b.taskId))
  const interruptTask = useAppStore((s) => s.interruptTask)
  return (
    // data-family：分类色板的挂钩（看=蓝/跑=青/写=紫/托=橙…，规则在 index.css）。
    // 组件只声明"这是什么工具"，不声明颜色 —— 换皮肤时不碰 .tsx。
    // 编排型工具（codemode）按**档位**取分类色：检索=看（蓝）/ 执行=跑（青）——「它这一次到底
    // 在干嘛」比「它的工具名」重要；data-cm 再叠一条档位竖脊（见 index.css 的 .tool-row[data-cm]），
    // 让一排 codemode 调用在长转录里连成可数的节奏，而不是混进 bash 行里。
    <div
      className={`tool-row ${changed ? 'changed' : ''} ${open ? 'open' : ''}`}
      data-s={err ? 'fail' : running ? 'warn' : undefined}
      data-family={isCodemode ? (tier === 'search' ? 'read' : 'cmd') : toolFamily(b.name)}
      data-cm={isCodemode ? tier : undefined}
    >
      <div
        className="tool-row-hd"
        role="button"
        aria-expanded={open}
        tabIndex={0}
        onClick={toggle}
        onKeyDown={(e) => {
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault()
            toggle()
          }
        }}
      >
        {/* 展开箭头在图标之前：一列行读下来，左侧是「可点开」，右侧才是「这是什么」。 */}
        {rt ? <FoldIcon open={open} /> : <span className="chev">{open ? '▾' : '▸'}</span>}
        {/* V3 / 运行时皮肤的行首图标（默认皮肤不渲染）：读=文档 · 检索=放大镜 · 执行=终端 · 改=笔 */}
        <span className="ic" data-s={err ? 'fail' : running ? 'warn' : undefined} aria-hidden="true"><ToolIcon name={isCodemode ? (tier === 'search' ? 'search' : 'bash') : b.name} /></span>
        {isSkill ? (
          <span className={`t ${err ? 'err' : ''}`}>SKILL({b.args})</span>
        ) : (
          <>
            <span className={`t ${err ? 'err' : ''}`}>{rt ? toolVerb(b.name) : b.name}</span>
            {/* 档位标签：只找工具（检索）还是真干活（执行）。判据来自后端 HostOps 计数 ——
                保留操作不进 nested_calls，前端无法自行判定「纯检索」（见 codemodeTier）。
                放在名字**后面**而不是行尾：行尾被时间/用量/结果占着，一眼扫过去只剩药丸的边。 */}
            {isCodemode && (
              <span
                className={`cm-mode ${tier}`}
                title={tier === 'search' ? t('tool.codemode.searchHint') : t('tool.codemode.executeHint')}
              >
                <span className="cm-mode-mark" aria-hidden="true">{tier === 'search' ? '⌕' : '❯'}</span>
                <span className="cm-mode-word">{tier === 'search' ? t('tool.codemode.search') : t('tool.codemode.execute')}</span>
              </span>
            )}
            {/* 编排型工具（codemode）显示「脚本干了什么」；其余工具显示 toolArgsSummary。 */}
            {(scriptLine || summary) && (
              <span
                className={`args ${path ? 'file' : ''}`}
                // data-ft：文件引用按**文件类型**着色（与产出卡图标同一套色），
                // 且与"普通参数文本"一眼分开（同色虚线，见 index.css）
                data-ft={path ? fileTypeOf(path) : undefined}
                role={path ? 'button' : undefined}
                tabIndex={path ? 0 : undefined}
                title={path ? t('tool.preview').replace('{path}', path) : undefined}
                onClick={(e) => {
                  if (path) {
                    e.stopPropagation()
                    // read_file → source 模式（「最终文件」全量视图 + 定位到 start_line）；
                    // write/edit → operation 模式（定位本次变更）
                    if (b.name === 'read_file') openFile(path, undefined, 'source', startLine)
                    else openFile(path, b.toolId)
                  }
                }}
                onKeyDown={(e) => {
                  if (path && (e.key === 'Enter' || e.key === ' ')) {
                    e.preventDefault()
                    e.stopPropagation()
                    if (b.name === 'read_file') openFile(path, undefined, 'source', startLine)
                    else openFile(path, b.toolId)
                  }
                }}
              >
                {/* 超长路径先收成 `…/最后两段`：完整路径在 title 里（点击也用它），
                    这里只保证度量与耗时留在可视区（用户 2026-10） */}
                {scriptLine || (path ? shortPath(summary) : summary)}
              </span>
            )}
          </>
        )}
        <span className="grow" />
        {host === 'main' && b.promoted && b.taskId && (b.status === 'running' || b.interrupting) && (
          <button
            className="tool-promote"
            disabled={b.interrupting}
            onClick={(e) => {
              e.stopPropagation() // 避免触发展开/收起
              interruptTask(b.taskId!)
            }}
            title={b.interrupting ? t('task.stopping') : t('task.stop')}
          >
            {b.interrupting ? t('task.interrupting') : t('task.stopBtn')}
          </button>
        )}
        {host === 'main' && b.promoted && b.status === 'running' && (
          <span className="badge bg" title={b.taskId || ''}>{t('tool.promoted')}</span>
        )}
        {host === 'main' && !b.promoted && b.status === 'running' && b.taskId && (
          <button
            className="tool-promote"
            onClick={(e) => {
              e.stopPropagation()
              promoteTask(b.taskId!)
            }}
            title={t('tool.promote.hint')}
          >
            {t('tool.promote')}
          </button>
        )}
        {b.status === 'running' && !isCodemode && <span className="run">…</span>}
        {/* 度量列：+a −b / N lines / N hits / N files / x.x KB —— 算不出就不画 */}
        {rt && metric ? (
          <span className={`m${err ? ' bad' : ''}`}>
            {metric.kind === 'diff' ? (
              <>
                <span className="p">+{metric.added}</span> <span className="d">−{metric.removed}</span>
              </>
            ) : (
              metric.text
            )}
          </span>
        ) : null}
        {rt ? (
          <span className={`dur${err ? ' bad' : ''}`}>
            {b.status === 'running' ? <ElapsedTimer startAt={b.startedAt} running durMs={undefined} /> : fmtMs(b.durMs ?? undefined)}
          </span>
        ) : (
          ((b.status === 'running' && !isCodemode) || b.durMs != null) && (
            <ElapsedTimer className="ms" startAt={b.startedAt} running={asyncTask} durMs={asyncTask ? undefined : b.durMs} />
          )
        )}
        <UsageChip className="mono tu" usage={b.usage} title={t('usage.tip').replace('{tok}', String((b.usage?.input ?? 0) + (b.usage?.output ?? 0))).replace('{cost}', fmtUsd(b.usage?.costUsd ?? 0))} />
        <span className={`st ${err ? 'err' : changed ? 'ok' : ''}`}>{err ? '✗' : changed ? '✓' : ''}</span>
      </div>
      {/* —— 编排型工具（codemode）的两块附加渲染（设计文档 §17）—— */}
      {/* 1) 脚本内的嵌套调用清单：**只挂在外面这一行**下面（子调用不占独立工具行）。
             折叠态 = 最后 8 条 + 「…N earlier calls」，展开态 = 全部 —— 组件自带折叠交互。 */}
      {/* 运行中的输出尾巴（tail -f）：**只给人看** —— bash 照旧阻塞到命令结束，
          模型只拿最终结果。文本是替换语义（后端每 250ms 一帧窗口），不做追加。 */}
      {b.status === 'running' && b.liveTail && <ToolLiveTail text={b.liveTail} total={b.liveTotal} />}
      {/* 0) 执行中：脚本控制台条（已用/预算 + 脚本内的调用点）。只有 codemode 有预算可画。 */}
      {isCodemode && b.status === 'running' && <ToolScriptRun script={scriptSrc} startedAt={b.startedAt} />}
      {b.nestedCalls && b.nestedCalls.length > 0 && (
        <ToolNestedCalls calls={b.nestedCalls} discover={(b.hostOps?.search ?? 0) + (b.hostOps?.describe ?? 0)} />
      )}
      {/* 2) 脚本输出的折叠预览：按**视觉行**限 5 行（不是按 \n 数），下方点名 spill 路径。
             展开行时不再重复预览 —— 展开体里的「结果」区已经是全文（限高内滚）。 */}
      {!open && hasScriptOutput(b) && (
        <ToolOutputPreview
          text={b.result ?? ''}
          spillPath={b.spillPath}
          script={scriptSrc}
          nestedCount={b.nestedCalls?.length ?? 0}
          tier={tier}
        />
      )}
      {/* 行内 diff 预览（运行时变更可见性，2026-08-22）：write/edit 完成即自动展开，
          限高 + 内滚 + 可折叠（+n −m 摘要）；与「查看变更」的右侧完整视图并存。
          独立于行详情（open）的展开状态 —— 不点行也能看到改了什么。 */}
      {b.diff && (
        <div className="tool-diff">
          <div
            className="tool-diff-hd"
            role="button"
            tabIndex={0}
            aria-expanded={diffOpen}
            onClick={() => setDiffOpen((v) => !v)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault()
                setDiffOpen((v) => !v)
              }
            }}
          >
            <span className="chev">{diffOpen ? '▾' : '▸'}</span>
            <span className="td-label">{t('tool.changed')}</span>
            <span className="td-stats mono"><b className="add">+{b.diff.added}</b><b className="del">−{b.diff.removed}</b></span>
            {b.diff.path && <span className="td-path mono" title={b.diff.path}>{b.diff.path}</span>}
            <span className="grow" />
            {isHtmlWrite && (
              <button
                className="td-more"
                title={t('tool.openInBrowser').replace('{path}', path)}
                onClick={(e) => {
                  e.stopPropagation()
                  void openHtmlInBrowser(path)
                }}
              >
                {t('tool.openInBrowserBtn')}
              </button>
            )}
            <button
              className="td-more"
              title={b.name === 'write_file' ? t('tool.fullView') : t('tool.locateChange')}
              onClick={(e) => {
                e.stopPropagation()
                // write_file：统一 open-file —— 跳右侧「最终文件」全量视图（与 read_file 一致）；
                // edit_file：保留定位本次变更（视图不变）；diff 无路径时兜底切到变更 tab
                if (b.diff?.path) {
                  if (b.name === 'write_file') openFile(b.diff.path, undefined, 'source')
                  else openFile(b.diff.path, b.toolId)
                } else {
                  setRightTab('git')
                  setRightExpanded(true)
                }
              }}
            >
              {t('tool.fullView')}
            </button>
          </div>
          {diffOpen && (
            <div className="tool-diff-body">
              {b.diff.unified
                ? <UnifiedDiff unified={b.diff.unified} />
                : <div className="tool-diff-empty">{t('tool.noTextDiff')}</div>}
            </div>
          )}
        </div>
      )}
      {open && (
        <div className="tool-row-body">
          {b.args && (
            <div className="tr-sec">
              <div className="tr-sec-h">{t('tool.args')}</div>
              <JsonView text={b.args} className="json-box" />
            </div>
          )}
          {err && b.errorInfo && (
            <div className="tr-sec tool-error-info" role="alert">
              <div className="tr-sec-h">{t('tool.recovery')}{b.errorInfo.code ? ` · ${b.errorInfo.code}` : ''}</div>
              {/* 问题二：只保留一行 fix + 下一步；「未修改文件」仅对写类副作用工具有信息量 */}
              {b.errorInfo.changed === 'false' && (b.name === 'write_file' || b.name === 'edit_file' || b.name === 'bash') && <div>{t('tool.unchanged')}</div>}
              {b.errorInfo.recovery && <div>{b.errorInfo.recovery}</div>}
              {b.errorInfo.nextAction && b.errorInfo.recovery && !b.errorInfo.recovery.includes(b.errorInfo.nextAction) && <div className="muted">{t('tool.nextStep').replace('{action}', b.errorInfo.nextAction)}</div>}
            </div>
          )}
          {b.result && (
            <div className="tr-sec">
              <div className="tr-sec-h">{t('tool.result')}</div>
              <ToolImages images={b.images ?? []} label={t('tool.resultImages')} />
              <ToolOutput text={b.result} name={b.name} />
            </div>
          )}
          {!b.result && b.status === 'running' && (
            <div className="tr-sec">
              <div className="tr-sec-h">{t('tool.executing')}</div>
            </div>
          )}
          {isFileTool && (
            <div className="tr-sec tr-actions">
              {path && (
                <button className="tr-act" onClick={() => openFile(path, undefined, 'source', startLine)} title={t('tool.preview').replace('{path}', path)}>
                  {t('tool.preview').replace('{path}', path)}
                </button>
              )}
              {/* 统一 open-file：read_file 完整视图（全量内容，支持工作区外）——
                  与 write/edit 的「完整视图」一致：跳右侧文件栏最终文件 */}
              {changed && b.name === 'read_file' && path && (
                <button
                  className="tr-act"
                  onClick={() => openFile(path, undefined, 'source', startLine)}
                >
                  {t('tool.fullView')}
                </button>
              )}
              {changed && isWrite && (
                <button
                  className="tr-act"
                  onClick={() => {
                    setRightTab('git')
                    setRightExpanded(true)
                  }}
                >
                  {t('tool.viewChange')}
                </button>
              )}
              {changed && isHtmlWrite && (
                <button className="tr-act" onClick={() => void openHtmlInBrowser(path)}>
                  {t('tool.openInBrowserBtn')}
                </button>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  )
})

// 连续工具块 → 纵向分组卡片（并行工具上下排；单工具 = 单行卡片）
// 待办（todo_*）不在这里出现：连续的写会先被并成一张表（见下方 ToolGroup）。
/** V3 皮肤的行首图标：按工具名取语义（与 docs/agent-ui-lab 的 ICON 映射一致）。
 *  默认皮肤下 .ic 被 CSS 隐藏（html:not([data-skin='v3']) .ic { display: none }），零影响。
 *  工具行与任务卡（TaskCard 的 .ic）共用这一份映射。 */
export const ToolIcon = memo(function ToolIcon({ name }: { name: string }) {
  if (/^(read|view|open)/.test(name)) return <FileIcon size={16} />
  if (/^(grep|search|find|glob|list)/.test(name)) return <SearchIcon size={16} />
  if (/^(edit|write|create|patch|apply)/.test(name)) return <PencilIcon size={16} />
  if (/^(todo|plan|update_plan)/.test(name)) return <CheckList size={16} />
  if (/^(task|agent|spawn)/.test(name)) return <Bot size={16} />
  if (/^(async|background|promote)/.test(name)) return <PlayIcon size={16} />
  return <TerminalIcon size={16} />
})

/* todo_* 的契约是「一次调用 = 整张表」，所以连续几次写是**同一张表的几张快照**，
   不是几行转录。这里把连续的写并成一个块：列表取最后一张，变化由组件自己比出来。 */
const isTodoTool = (b: ToolRowData) => (b.name === 'todo_add' || b.name === 'todo_update')

export const ToolGroup = memo(function ToolGroup({ tools, host = 'main' }: { tools: ToolRowData[]; host?: ToolRowHost }) {
  const sessionTodos = useAppStore((s) => s.todos)
  const nodes: ReactNode[] = []
  for (let i = 0; i < tools.length; i++) {
    const t = tools[i]
    // 快照为空且已结束（历史会话 todos 未随恢复带出）→ 回落通用工具行，保底可展开看参数
    const hasTodo = isTodoTool(t) && (t.status === 'running' || (t.todos ?? sessionTodos).length > 0)
    if (!hasTodo) {
      nodes.push(<ToolRow key={t.id ?? t.toolId ?? i} b={t} host={host} />)
      continue
    }
    const writes: TodoWrite[] = []
    let j = i
    while (j < tools.length && isTodoTool(tools[j]) && (tools[j].status === 'running' || (tools[j].todos ?? sessionTodos).length > 0)) {
      writes.push({ items: tools[j].todos ?? sessionTodos, running: tools[j].status === 'running' })
      j++
    }
    nodes.push(<TodoBlock key={t.id ?? t.toolId ?? i} writes={writes} />)
    i = j - 1
  }
  return <div className="tool-group">{nodes}</div>
})

// 任务结果（卡片底部）：主对话卡（AgentCard）与右栏任务卡（AgentView /
// AsyncTaskPlaceholder）共用 —— 同一条结果在两处必须同一形态。
// 历史：右栏曾渲染成 mono 11px 的 <pre>（.av-out），主对话渲染 Markdown，
// 同一条「已完成」的交付结果两处长得不一样（2026-09-23 一并统一）。
/* 命令输出：逐行定级后着色（lib/logLines.ts）。着色只认行首能认出来的东西，
   认不出的行保持正文色 —— 宁可少染，不要猜错。超过 300 行整段不染（大结果里
   逐行 span 的收益远不上开销）。
   只对「输出本身就是日志」的工具做：bash / 以及名字里带 shell/exec 的。 */
function isLogTool(name: string): boolean {
  return /^(bash|sh|shell|exec|run|run_python|terminal)$/i.test(name) || /shell|bash/i.test(name)
}

export function ToolOutput({ text, name }: { text: string; name: string }) {
  if (!isLogTool(name)) return <pre className="tr-code">{text}</pre>
  const lines = splitOutput(text)
  if (!lines) return <pre className="tr-code">{text}</pre>
  return (
    <pre className="tr-code rt-out">
      {lines.map((l, i) => (
        <span className="rt-out-ln" key={i}>
          {l.stamp ? <span className="rt-out-ts">{l.stamp}</span> : null}
          {l.level ? <span className={`rt-out-${l.level}`}>{l.text}</span> : l.text}
          {i < lines.length - 1 ? '\n' : null}
        </span>
      ))}
    </pre>
  )
}

export const TaskResultInline = memo(function TaskResultInline({ delivered, result, error }: { delivered?: boolean; result?: string; error?: string }) {
  const t = useT()
  if (!result && !error) return null
  return (
    <div className="task-result-inline">
      <div className="task-result-inline-title">{delivered ? t('task.result.delivered') : t('task.result.title')}</div>
      {error ? <div className="task-result-err">{error}</div> : <Markdown text={result || ''} />}
    </div>
  )
})

// 上下文压缩块：主对话（本 run 的自动/手动压缩）与子 agent 转录（子 agent 自己的压缩）
// 共用同一张卡。active = 压缩正在进行（子 agent 侧会带这个态；主 run 的进行中压缩只进
// 右栏任务区，所以主对话看不到 active 卡）。
/* 上下文压缩块：主对话（本 run 的自动/手动压缩）与子 agent 转录（子 agent 自己的
   压缩）共用同一张卡 —— 现在是同**一行**。v2 的 Compact 移植：30px 折叠行，
   摘要/配比/事实都在折叠里（见 runtime/blocks/Compact.tsx）。 */
export const CompressionBlock = memo(function CompressionBlock({
  b,
  heads,
}: {
  b: CompressionInfo
  heads?: CompactHead[]
}) {
  return <Compact b={b} heads={heads} />
})
