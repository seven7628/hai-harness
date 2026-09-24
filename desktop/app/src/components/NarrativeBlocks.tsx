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

import { memo, useEffect, useMemo, useRef, useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import type { CompressionInfo, Diff, QueuedContent, ToolErrorInfo, TodoItem, UsageAgg } from '../store/useAppStore'
import { Markdown } from './markdown'
import { Bot, CheckList, FileIcon, MinimizeIcon, PencilIcon, PlayIcon, SearchIcon, Sparkles, TerminalIcon } from './icons'
import { fmtTokens, fmtDuration, fmtUsd, UsageChip, UnifiedDiff } from './ui'
import { toolArgsSummary } from '../lib/toolSummary'
import { toolFamily, fileTypeOf } from '../lib/palette'
import { splitThinkingParagraphs } from '../lib/thinkingText'
import JsonView from './JsonView'
import { ElapsedTimer } from './ElapsedTimer'
import { ToolImages } from './ToolImages'
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
  const [open, setOpen] = useState(false)
  const running = live && thinkStart != null && thinkMs == null // 思考中：还在生成、已开始且未结算
  // 时钟：仅思考中挂（ElapsedTimer 订阅，每秒走）；结束 timer=thinkMs 显示总耗时
  const bodyRef = useRef<HTMLDivElement>(null)
  const long = text.length > 200
  const showFull = live || open || !long
  const shown = showFull ? text : text.slice(0, 160) + '…'
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
          <span className="ic" aria-hidden="true">{<Sparkles size={16} />}</span>
          <span className="mono">▸ {t('thinking.label')}{<ElapsedTimer startAt={thinkStart} running={running} durMs={thinkMs} />}</span>
        </button>
        {long && !live && (
          <button className="thinking-more" onClick={() => setOpen((v) => !v)}>
            {open ? t('thinking.collapseAll') : t('thinking.expandAll')}
          </button>
        )}
      </div>
      <div className={`thinking-body ${long ? 'scroll' : ''} ${showFull ? '' : 'collapsed'}`} ref={bodyRef}>
        {paras.map((para, i) => (
          <p className="tk-p" key={i}><InlineCode text={para} /></p>
        ))}
      </div>
    </div>
  )
})

// 待办工具卡片（todo_add / todo_update）：对话内「计划」视图 —— 头部 = 清单图标 +
// 进度（done/total · 已完成/全部完成）+ 耗时，可折叠；展开 = 全量待办清单：
// ✓ 已完成（绿 · 灰显删除线）/ ◎ 进行中（accent 朱砂高亮，= 第一个未完成项）/
// ○ 未开始（弱化），被依赖阻塞的项带「阻塞 tN」标签。数据源 = 会话级 todos 快照
// （桥接层随 todo_* 工具结束事件携带，与右栏待办面板同源）；仅最新一张卡展开
// （live），新卡到达后旧卡自动折叠，保持对话时间线整洁。
// 子 agent 转录里的 todo_* 调用同样走这张卡（同一份数据 → 同一套渲染）：转录没有
// 「最新一张」的上下文，live 恒 false → 默认折叠，点开才看清单。
const TodoToolCard = memo(function TodoToolCard({ b, live }: { b: ToolRowData; live: boolean }) {
  const [open, setOpen] = useState(live)
  const [raw, setRaw] = useState(false) // 原始参数/结果详情（「详情」按钮切换）
  const todos = useAppStore((s) => s.todos)
  const t = useT()
  // 新计划卡出现（live true→false）：旧卡自动折叠（默认态管理，用户可再手动展开）
  useEffect(() => {
    if (!live) setOpen(false)
  }, [live])
  const running = b.status === 'running'
  const err = b.isError ?? b.status === 'error'
  // 渲染列表：优先用「本次调用完成时刻」的定格快照（bridge 随 tool_response 携带，
  // 历史卡显示当时状态而非最终态）；无存档（运行中 / 旧日志）回退当前 store 快照。
  const list = b.todos ?? todos
  const done = list.filter((x) => x.done).length
  const total = list.length
  const allDone = total > 0 && done === total
  const activeId = list.find((x) => !x.done)?.id
  // 每次调用的参数摘要：区分多次 todo 调用（如批量更新时 N 张卡各改了什么一目了然）
  let callSummary = ''
  try {
    const a = JSON.parse(b.args || '{}') as { id?: string; title?: string; done?: boolean }
    if (b.name === 'todo_update' && a.id) {
      callSummary = `#${a.id} ${a.done === true ? t('todo.updated') : a.done === false ? t('todo.reopened') : t('todo.updated2')}`
    } else if (b.name === 'todo_add' && a.title) {
      callSummary = `+ ${a.title.slice(0, 14)}`
    }
  } catch { /* 非 JSON 参数：无摘要 */ }
  return (
    <div className={`plan-card${err ? ' error' : ''}${open ? ' open' : ''}`}>
      <div
        className="plan-hd"
        role="button"
        aria-expanded={open}
        tabIndex={0}
        onClick={() => setOpen((v) => !v)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); setOpen((v) => !v) }
        }}
      >
        <span className="chev" aria-hidden="true">{open ? '▾' : '▸'}</span>
        <CheckList size={13} />
        <span className="plan-title">{t('plan.title')}</span>
        {running ? (
          <span className="plan-meta">…</span>
        ) : err ? (
          <span className="plan-meta err">{t('plan.failed')}</span>
        ) : (
          <>
            <span className={`plan-count mono${allDone ? ' ok' : ''}`}>{done}/{total}</span>
            <span className="plan-meta">{allDone ? t('plan.allDone') : t('plan.done')}</span>
          </>
        )}
        {callSummary && <span className="plan-call mono">{callSummary}</span>}
        <span className="grow" />
        {(b.args || b.result) && !running && (
          <button
            className="plan-raw-toggle"
            onClick={(e) => { e.stopPropagation(); setRaw((v) => !v) }}
            title={t('tool.rawDetail')}
          >
            {raw ? t('json.collapse') : t('tool.rawDetailToggle')}
          </button>
        )}
        {b.durMs != null && <span className="mono plan-meta">{fmtDuration(b.durMs)}</span>}
        {running && <ElapsedTimer className="ms" startAt={b.startedAt} running />}
      </div>
      {open && !err && total > 0 && (
        <ul className="plan-list">
          {list.map((todo) => {
            const blocked = !todo.done && todo.blockedBy && todo.blockedBy.length > 0
            return (
              <li key={todo.id} className={`plan-item${todo.done ? ' done' : ''}${todo.id === activeId ? ' active' : ''}`}>
                <span className="plan-st" aria-hidden="true">{todo.done ? '✓' : todo.id === activeId ? '◎' : '○'}</span>
                <span className="plan-txt" title={todo.title}>{todo.title}</span>
                {blocked && <span className="plan-blk mono">{t('todo.blockedBy').replace('{ids}', todo.blockedBy!.join(','))}</span>}
              </li>
            )
          })}
        </ul>
      )}
      {open && err && <div className="plan-err" role="alert">{b.result || t('todo.updateFailed')}</div>}
      {raw && (
        <div className="plan-raw">
          {b.args && <div className="tr-sec"><div className="tr-sec-h">{t('tool.args')}</div><JsonView text={b.args} className="json-box" /></div>}
          {b.result && <div className="tr-sec"><div className="tr-sec-h">{t('tool.result')}</div><pre className="tr-code">{b.result}</pre></div>}
        </div>
      )}
    </div>
  )
})

// 工具调用行：名称(按族着色) + 参数摘要 + 耗时/状态；点击可展开查看完整参数与结果。
// 文件工具（read/write/edit）额外提供路径预览 + 变更查看入口。
// host 只挡一件事：主 run 专属动作（「后台」promote / 「停止」interrupt）——
// 它们作用于**主 run 的工具任务**，子 agent 转录里既不该出现也没有对应语义
//（后台工具任务在右栏「任务」tab 有自己的卡与精确停止）。其余一切（族色 / 行首图标 /
// 参数文件名着色 / 点击打开文件 / 计划卡 / 错误恢复提示）两处完全一致。
export const ToolRow = memo(function ToolRow({ b, live = false, host = 'main' }: { b: ToolRowData; live?: boolean; host?: ToolRowHost }) {
  const [open, setOpen] = useState(false)
  const [diffOpen, setDiffOpen] = useState(true) // 行内 diff 默认展开（运行时变更立即可见）
  const setRightTab = useAppStore((s) => s.setRightTab)
  const setRightExpanded = useAppStore((s) => s.setRightExpanded)
  const openFile = useAppStore((s) => s.openFile)
  const openHtmlInBrowser = useAppStore((s) => s.openHtmlInBrowser)
  const todos = useAppStore((s) => s.todos)
  const promoteTask = useAppStore((s) => s.promoteTask)
  const t = useT()
  // 出错归一化：主对话块用 isError，子 agent 项用 status==='error'（两边同义）
  const err = b.isError ?? b.status === 'error'
  const changed = b.status === 'done' && !err
  const summary = toolArgsSummary(b.name, b.args || '', t)
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
  // 待办工具（todo_add / todo_update）→ 计划卡片（对话内计划视图）。
  // 快照为空且已结束（如历史会话 todos 未随恢复带出）→ 回落通用工具行，保底可展开看参数。
  if ((b.name === 'todo_add' || b.name === 'todo_update') && (running || todos.length > 0)) {
    return <TodoToolCard b={b} live={live} />
  }
  return (
    // data-family：分类色板的挂钩（看=蓝/跑=青/写=紫/托=橙…，规则在 index.css）。
    // 组件只声明"这是什么工具"，不声明颜色 —— 换皮肤时不碰 .tsx。
    <div className={`tool-row ${changed ? 'changed' : ''} ${open ? 'open' : ''}`} data-family={toolFamily(b.name)}>
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
        {/* V3 行首图标（默认皮肤不渲染）：读=文档 · 检索=放大镜 · 执行=终端 · 改=笔 */}
        <span className="ic" aria-hidden="true"><ToolIcon name={b.name} /></span>
        <span className="chev">{open ? '▾' : '▸'}</span>
        {isSkill ? (
          <span className={`t ${err ? 'err' : ''}`}>SKILL({b.args})</span>
        ) : (
          <>
            <span className={`t ${err ? 'err' : ''}`}>{b.name}</span>
            {summary && (
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
                {summary}
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
        {b.status === 'running' && <span className="run">…</span>}
        {(b.status === 'running' || b.durMs != null) && (
          <ElapsedTimer className="ms" startAt={b.startedAt} running={asyncTask} durMs={asyncTask ? undefined : b.durMs} />
        )}
        <UsageChip className="mono tu" usage={b.usage} title={t('usage.tip').replace('{tok}', String((b.usage?.input ?? 0) + (b.usage?.output ?? 0))).replace('{cost}', fmtUsd(b.usage?.costUsd ?? 0))} />
        <span className={`st ${err ? 'err' : changed ? 'ok' : ''}`}>{err ? '✗' : changed ? '✓' : ''}</span>
      </div>
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
              <pre className="tr-code">{b.result}</pre>
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
// liveTodoId：最新一张待办工具卡（todo_*）的 toolId —— 仅它展开实时清单，旧卡折叠。
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

export const ToolGroup = memo(function ToolGroup({ tools, liveTodoId = '', host = 'main' }: { tools: ToolRowData[]; liveTodoId?: string; host?: ToolRowHost }) {
  return (
    <div className="tool-group">
      {tools.map((t, i) => (
        <ToolRow key={t.id ?? t.toolId ?? i} b={t} live={t.toolId === liveTodoId} host={host} />
      ))}
    </div>
  )
})

// 任务结果（卡片底部）：主对话卡（AgentCard）与右栏任务卡（AgentView /
// AsyncTaskPlaceholder）共用 —— 同一条结果在两处必须同一形态。
// 历史：右栏曾渲染成 mono 11px 的 <pre>（.av-out），主对话渲染 Markdown，
// 同一条「已完成」的交付结果两处长得不一样（2026-09-23 一并统一）。
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
export const CompressionBlock = memo(function CompressionBlock({ b }: { b: CompressionInfo }) {
  const t = useT()
  const [open, setOpen] = useState(false)
  const failed = Boolean(b.error)
  const aborted = Boolean(b.aborted)
  const active = Boolean(b.active) && !failed && !aborted
  const usage = b.usage
  const total = usage ? usage.input + usage.output : 0
  const saved = Math.max(0, b.before - b.after)
  const toggle = () => setOpen((v) => !v)
  return (
    <div className={`compression-card ${failed ? 'error' : 'success'} ${open ? 'open' : ''}`}>
      <div className="compression-card-hd" role="button" tabIndex={0} aria-expanded={open} onClick={toggle} onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle() }
      }}>
        <span className="ic" aria-hidden="true">{<MinimizeIcon size={16} />}</span>
        <span className="chev">{open ? '▾' : '▸'}</span>
        <span className={`compression-icon${active ? ' compression-active' : ''}`} aria-hidden="true">{failed ? '✗' : aborted ? '■' : active ? '◐' : '✓'}</span>
        <strong className={active ? 'compression-active' : undefined}>{failed ? t('compression.failed') : aborted ? t('compression.aborted') : active ? t('compression.inProgress') : t('compression.done')}</strong>
        <span className="compression-summary">{active ? '' : failed || aborted ? t('compression.kept') : t('compression.summary').replace('{before}', String(b.before)).replace('{after}', String(b.after))}</span>
        <span className="grow" />
        {b.durMs != null && <span className="mono compression-meta">{fmtDuration(b.durMs)}</span>}
        {usage && <UsageChip className="mono compression-meta" usage={usage} title={t('usage.tip').replace('{tok}', String(total)).replace('{cost}', fmtUsd(usage.costUsd ?? 0))} />}
      </div>
      {open && (
        <div className="compression-card-body">
          {failed ? <div className="compression-error" role="alert">{b.error}</div> : aborted ? (
            <div className="compression-error">{t('compression.abortedDesc')}</div>
          ) : (
            <>
              {b.summary && <div className="compression-summary-preview"><div className="compression-summary-label">{t('compression.summaryLabel')}</div><Markdown text={b.summary} /></div>}
              {b.analysis && <AnalysisBlock analysis={b.analysis} />}
              <div className="compression-grid">
              <span>{t('compression.history')}</span><b>{b.before} → {b.after}（{t('compression.reduced').replace('{n}', String(saved))}）</b>
              {b.ctxTokens != null && <><span>{t('compression.afterCtx')}</span><b>{fmtTokens(b.ctxTokens)} tok</b></>}
              {b.durMs != null && <><span>{t('compression.dur')}</span><b>{fmtDuration(b.durMs)}</b></>}
              {b.model && <><span>{t('compression.model')}</span><b className="mono">{b.model}</b></>}
              {usage && <>
                <span>{t('compression.inputOutput')}</span><b>{fmtTokens(usage.input)} / {fmtTokens(usage.output)} tok</b>
                {usage.cacheRead > 0 && <><span>{t('compression.cacheRead')}</span><b>{fmtTokens(usage.cacheRead)} tok</b></>}
                {usage.cacheWrite > 0 && <><span>{t('compression.cacheWrite')}</span><b>{fmtTokens(usage.cacheWrite)} tok</b></>}
                {usage.reasoning > 0 && <><span>{t('compression.reasoning')}</span><b>{fmtTokens(usage.reasoning)} tok</b></>}
                {usage.costUsd ? <><span>{t('compression.cost')}</span><b className="mono">{fmtUsd(usage.costUsd)}</b></> : null}
              </>}
              {b.reason && <><span>{t('compression.reason')}</span><b>{b.reason === 'automatic' ? t('compression.auto') : t('compression.manual')}</b></>}
              </div>
            </>
          )}
        </div>
      )}
    </div>
  )
})

// 压缩器 <analysis> 块展示：摘要的决策过程（状态迁移/决策/预算/锚点），默认折叠。
// 仅供观测压缩 Prompt 决策——与摘要正文分开，不参与续跑上下文。
const AnalysisBlock = memo(function AnalysisBlock({ analysis }: { analysis: string }) {
  const t = useT()
  const [open, setOpen] = useState(false)
  return (
    <div className={`compression-analysis ${open ? 'open' : ''}`}>
      <div className="compression-analysis-hd" role="button" tabIndex={0} aria-expanded={open}
        onClick={() => setOpen((v) => !v)} onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); setOpen((v) => !v) } }}>
        <span className="chev">{open ? '▾' : '▸'}</span>
        <span>{t('compression.analysisLabel')}</span>
      </div>
      {open && <pre className="compression-analysis-body mono">{analysis}</pre>}
    </div>
  )
})
