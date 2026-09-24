import { useEffect, useRef, useState } from 'react'
import type { ClipboardEvent as ReactClipboardEvent, DragEvent as ReactDragEvent } from 'react'
import { useAppStore, modelWindowOf, QUEUE_CAP, queuedText, queuedHasAttach, modelSupportsImage, inputTypesForProvider, BUILTIN_SLASH, mainRunActive, isSubAgentRun } from '../store/useAppStore'
import type { QueuedContent } from '../store/useAppStore'
import { SendIcon, StopIcon, ChevronDown, ChevronLeft, ChevronRight, ImageIcon, AccessIcon, EffortIcon, Sparkles, MicIcon, PaperclipIcon } from './icons'
import { CtxRing, fmtTokens, ChipMenu } from './ui'
import CompactConfirm from './CompactConfirm'
import CtxBreakdownPanel from './CtxBreakdown'
import FileRefPanel from './FileRefPanel'
import { AgentRunningBar } from './AgentRunningBar'
import { useT } from '../i18n'
import { getTransport } from '../transport'
import {
  acceptImages, dataUrlBytes, fmtBytes,
  MAX_IMAGE_ATTACHMENTS, MAX_IMAGE_DATA_URL_BYTES,
} from '../lib/imageAttach'

const EFFORTS = [
  { key: 'none', label: 'None' },
  { key: 'minimal', label: 'Minimal' },
  { key: 'low', label: 'Low' },
  { key: 'medium', label: 'Medium' },
  { key: 'high', label: 'High' },
  { key: 'xhigh', label: 'XHigh' },
  { key: 'max', label: 'Max' },
]

// 斜杠命令（/ 前缀即命令）：/技能名 → 加载技能；/summary → 压缩；/clear → 清空上下文；
// /reload_skills → 重扫 .agents/skills + .go-code/skills 技能注册表（无需重启）。
// 命令列表 = 启用技能（/技能名）+ summary + clear + reload_skills；输入框首字符 / 时自动展示 + 前缀过滤。
// hasFileDrag 判定拖拽是否携带文件：只处理 files 类拖拽，文本/链接/页内元素拖拽
// 不拦截（保持浏览器默认行为——拖文本进输入框是正常操作）。
// 文档附件（本地文件）不复制、不上传：只记路径，发送时拼成既有 @引用 语法，
// 由服务端 fileref 机制展开内容给模型（展开/截断检测/二进制检测/敏感路径闸门都已实现）。
// 这里只保留展示所需的最小字段（name/size 仅用于附件条渲染）。
type DocRef = { path: string; name: string; size: number }
/** 单条消息的文档引用数量上限（再多模型也读不完；与图片附件同样给出明确提示）。 */
const MAX_DOC_REFS = 10
/** 「添加文件」/拖拽目标的类型过滤（引用不复制，所以放宽到常用文档而非全类型）。 */
const DOC_ACCEPT = 'image/*,.pdf,.docx,.xlsx,.xlsm,.xls,.pptx,.csv,.html,.htm,.txt,.md,.json'
/** DOC_ACCEPT 的扩展名部分（原生对话框 filters 用；不带 * 与点）。 */
const DOC_EXTENSIONS = DOC_ACCEPT.split(',').filter((s) => s.startsWith('.')).map((s) => s.slice(1))

export function hasFileDrag(dt: DataTransfer | null): boolean {
  if (!dt) return false
  return Array.from(dt.types ?? []).includes('Files')
}

// 拖拽内容分类（决定提示文案）：image=全是图片；doc=全是非图片（含文件夹：其 type 为空串）；
// mix=图片与非图片混拖。DataTransferItem.type 在 dragenter 阶段即可读，不必等 drop。
function dragKindOf(dt: DataTransfer | null): 'image' | 'doc' | 'mix' {
  const items = Array.from(dt?.items ?? []).filter((i) => i.kind === 'file')
  if (items.length === 0) return 'doc'
  const hasImage = items.some((i) => i.type.startsWith('image/'))
  const hasDoc = items.some((i) => !i.type.startsWith('image/'))
  return hasImage && hasDoc ? 'mix' : hasImage ? 'image' : 'doc'
}

export function insertTextAtSelection(value: string, inserted: string, start: number, end: number) {
  const next = value.slice(0, start) + inserted + value.slice(end)
  return { value: next, caret: start + inserted.length }
}

// 输入框高亮 token 化：/命令（第一行行首）渲染为命令徽章；@引用（词边界起始的 @xxx）渲染为
// 引用徽章；其余原样（含换行）——textarea 透明文字 + 背景 overlay 显徽章。
type HL = { t: string; k: 'plain' | 'cmd' | 'ref' }
function tokenizeInput(text: string, commands: string[]): HL[] {
  const out: HL[] = []
  // 第一行行首 /命令 → 命令徽章
  const nl = text.indexOf('\n')
  const first = nl >= 0 ? text.slice(0, nl) : text
  const m = first.match(/^(\/\S*)/)
  let cmdPrefix = ''
  if (m && m[1] && commands.some((c) => c.toLowerCase() === m[1].toLowerCase())) {
    cmdPrefix = m[1]
    out.push({ t: m[1], k: 'cmd' })
  }
  const rest = text.slice(cmdPrefix.length)
  // @引用：仅词边界（行首或前导空白）后的 @xxx（邮箱 foo@bar 不误伤）
  const re = /(^|[\s])@[^\s]+/g
  let i = 0
  let mm: RegExpExecArray | null
  while ((mm = re.exec(rest))) {
    if (mm.index > i) out.push({ t: rest.slice(i, mm.index), k: 'plain' })
    if (mm[1]) out.push({ t: mm[1], k: 'plain' }) // 前导空白保持普通文本
    out.push({ t: mm[0].slice(mm[1].length), k: 'ref' })
    i = mm.index + mm[0].length
  }
  if (i < rest.length) out.push({ t: rest.slice(i), k: 'plain' })
  return out.filter((s) => s.t !== '')
}
function highlightInput(text: string, commands: string[]): React.ReactNode {
  return (
    <>
      {tokenizeInput(text, commands).map((s, i) =>
        s.k === 'cmd' ? (
          <span className="input-cmd" key={i}>{s.t}</span>
        ) : s.k === 'ref' ? (
          <span className="input-ref" key={i}>{s.t}</span>
        ) : (
          <span key={i}>{s.t}</span>
        ),
      )}
    </>
  )
}

export default function Composer() {
  const ask = useAppStore((s) => s.ask)
  const interrupt = useAppStore((s) => s.interrupt)
  // 流式 = 有 LLM 流式（主会话正在产出）→ 发送钮变「停止」。不把后台子 agent
  //（agent_spawn 派生的异步任务，脱离主 run 存活）算进来 —— 否则 abort 主 run 后
  // 子 agent 仍在跑时发送钮卡「停止」、主会话无法输入。停止子 agent 走其 AgentCard。
  const streaming = useAppStore((s) => s.streaming)
  // 主 run 运行中（agent_start→agent_end 全程：流式/工具/压缩/审批/重试）。
  // 与 AgentRunningBar 同源（mainRunActive）：停止按钮与运行指示共用同一事实源。
  // 排除 aborted：点击停止后 interrupt() 乐观置位 aborted，按钮立即回「发送」态
  //（不等 agent_end 往返）；agent_end(abort) 到达后幂等覆盖。
  const runActive = useAppStore((s) => (s.activeSessionId ? mainRunActive(s.views[s.activeSessionId]) && !s.views[s.activeSessionId]?.aborted : false))
  const compressing = useAppStore((s) => Boolean(s.activeSessionId && s.views[s.activeSessionId]?.compressing))
  const ctxTokens = useAppStore((s) => s.ctxTokens) // 当前上下文占用（事件推导，压缩后反映压后大小）
  const ctxBreakdown = useAppStore((s) => s.ctxBreakdown) // 上下文构成分区（ctx 环 hover 面板）
  const fetchCtxBreakdown = useAppStore((s) => s.fetchCtxBreakdown)
  const usage = useAppStore((s) => s.usage) // 会话累计用量（缓存命中率两个口径都取它 + turns）
  const turns = useAppStore((s) => s.turns) // 每轮指标（llm_end 一条）：当前上下文命中率取末条
  const activeView = useAppStore((s) => (s.activeSessionId ? s.views[s.activeSessionId] : undefined))
  const effort = useAppStore((s) => s.effort)
  const switchEffort = useAppStore((s) => s.switchEffort)
  const t = useT()
  // Effort 菜单项：'none' 的说明文案走 i18n（其余为英文品牌名，无需翻译）
  const effortItems = EFFORTS.map((e) =>
    e.key === 'none' ? { ...e, desc: t('composer.effort.none') } : e,
  )
  const mode = useAppStore((s) => s.mode)
  const switchMode = useAppStore((s) => s.switchMode)
  const model = useAppStore((s) => s.model)
  const switchModel = useAppStore((s) => s.switchModel)
  const pendingQueue = useAppStore((s) => s.pendingQueue) // 暂存队列（未推送 / 已推送待确认；逐条可编辑）
  const pendingQueueError = useAppStore((s) => s.pendingQueueError) // 推送失败提示（ask_batch ok:false 回滚，问题十）
  const updatePending = useAppStore((s) => s.updatePending)
  const removePending = useAppStore((s) => s.removePending)
  const queueFull = pendingQueue.length >= QUEUE_CAP
  const [editingId, setEditingId] = useState<number | null>(null) // 正在编辑的暂存队列项 id
  const [editText, setEditText] = useState('')
  const skills = useAppStore((s) => s.skills)
  const fetchSkills = useAppStore((s) => s.fetchSkills)
  // 权限/行为模式（三种）：会话中可随时切换，下一轮生效。
  // hitl=用户确认（带风险的工具执行前需确认）；auto=自动执行（不弹确认，工具层
  // 静态拦截/沙箱仍生效）；full-access=完全访问（无需任何确认，所有工具直接执行）。
  const modes = [
    { key: 'hitl', label: t('composer.mode.hitl'), desc: t('composer.mode.hitl.desc') },
    { key: 'auto', label: t('composer.mode.auto'), desc: t('composer.mode.auto.desc') },
    { key: 'full-access', label: t('composer.mode.full-access'), desc: t('composer.mode.full-access.desc') },
  ]
  const activeSessionId = useAppStore((s) => s.activeSessionId)
  const workspace = useAppStore((s) => s.workspace) ?? ''
  // 草稿属于 session，而不是 Composer 组件。Composer 在切换会话时不会卸载，因此用
  // session key 保存各自的文本和附件；尚未创建会话时则按 workspace 隔离。
  const draftKey = activeSessionId ? `session:${workspace}\0${activeSessionId}` : `workspace:${workspace}`
  const [draftTexts, setDraftTexts] = useState<Record<string, string>>({})
  const text = draftTexts[draftKey] ?? ''
  const setText = (next: string | ((prev: string) => string)) => {
    setDraftTexts((drafts) => {
      const current = drafts[draftKey] ?? ''
      const value = typeof next === 'function' ? next(current) : next
      if (value === current && Object.prototype.hasOwnProperty.call(drafts, draftKey)) return drafts
      return { ...drafts, [draftKey]: value }
    })
  }
  const [menu, setMenu] = useState(false)
  const [providerFilter, setProviderFilter] = useState('all')
  const [cmdMenu, setCmdMenu] = useState(false)
  // @ 引用面板：atOpen=展开；atStart=输入框中 @ 的位置（替换 token 起点）；atFilter=@ 后已输入的前缀过滤
  const [atOpen, setAtOpen] = useState(false)
  const [atStart, setAtStart] = useState(0)
  const [atFilter, setAtFilter] = useState('')
  // 图片附件（模型支持图片输入时允许上传；随消息一起发送，块结构对齐 ask_batch image 块）
  const [draftAttachments, setDraftAttachments] = useState<Record<string, QueuedContent[]>>({})
  const attachments = draftAttachments[draftKey] ?? []
  // 停止态 = 主 run 运行中（且草稿为空——防误触：有草稿时按钮是「发送」，点按不中断）
  // 或压缩中：压缩时同样允许终止（用户此前只要求压缩期间禁止【输入】，并未禁止
  // 【终止】——不提供停止会把 5 分钟超时的压缩卡死）。
  // 回车永远走 submit()（入暂存队列，不中断）；Abort 只经「草稿为空时的停止按钮」。
  const stopping = (runActive && !text.trim() && attachments.length === 0) || compressing
  const setAttachments = (next: QueuedContent[] | ((prev: QueuedContent[]) => QueuedContent[])) => {
    setDraftAttachments((drafts) => {
      const current = drafts[draftKey] ?? []
      const value = typeof next === 'function' ? next(current) : next
      if (value === current) return drafts
      return { ...drafts, [draftKey]: value }
    })
  }
  const [attachmentNotice, setAttachmentNotice] = useState('')
  const [attachmentBusy, setAttachmentBusy] = useState(false) // 图片压缩中（大图降采样耗时）
  // 文档引用（本地文件路径 → @引用，不复制内容）：与图片 attachments 并列。
  // 不复用 QueuedContent（它直接对齐服务端 core.Content，加新 type 会动协议层）。
  const [docRefs, setDocRefs] = useState<DocRef[]>([])
  const [expanding, setExpanding] = useState(false)
  const [confirmCompact, setConfirmCompact] = useState(false)
  const fileInputRef = useRef<HTMLInputElement>(null)
  const dragDepthRef = useRef(0) // 拖拽进入/离开计数（子元素穿梭不闪烁）
  const [dragActive, setDragActive] = useState(false)
  // 拖拽内容类型（提示文案按内容区分：图片 → 内联张数；文档 → @引用；混合 → 两者都可能）
  const [dragKind, setDragKind] = useState<'image' | 'doc' | 'mix'>('doc')
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const refPanelRef = useRef<HTMLDivElement>(null)
  const chipRef = useRef<HTMLDivElement>(null)
  const cmdRef = useRef<HTMLDivElement>(null)
  const hlRef = useRef<HTMLDivElement>(null)
  const popupBodyRef = useRef<HTMLDivElement>(null) // 模型下拉滚动容器
  const filterRef = useRef<HTMLDivElement>(null) // Provider 筛选横向滚动条（展开时定位选中 Provider）
  const ctxUsed = ctxTokens ?? 0 // 无事件（旧会话/新会话）→ 0
  // ctx 占用口径：true = 压缩后的字符估算（尚无新一轮真实 usage）→ 读数前缀 ≈，
  // 与 hover 面板底部的口径说明同源（bridge CtxAnchor / checkpoint ctx_tokens_estimated）。
  const ctxEstimated = useAppStore((s) => s.ctxTokensEstimated) === true
  const settings = useAppStore((s) => s.settings)
  const contextWindow = useAppStore((s) => s.contextWindow)
  // 当前模型上下文窗口：优先 harness 上报，本地表兜底（Provider 面板可标 1M；问题六）
  const window = modelWindowOf(settings, model, contextWindow)
  const pct = Math.round((ctxUsed / window) * 100)
  // 缓存命中率两个口径（ctx 面板同时展示，用户 2026-09-18 明确「两个都要」）：
  //  · 当前上下文 = 最近一轮主 agent 调用的 cacheRead/input（turns 末条，跳过子 agent 轮）
  //    —— 面板其余内容（容量/构成）描述的都是「当前上下文」，这个数才与之同源：
  //       成熟会话通常 95~99%（缓存命中的就是系统提示词+工具+历史这一整段前缀）。
  //  · 会话累计   = 会话累计 cacheRead/input（= 状态栏「缓存」同一个数）
  //    —— 含全部历史轮次，会被「早期冷启动轮 / 缓存过期轮 / 子 agent 冷前缀 / 曾用不支持
  //       缓存的模型」拉低，因此可能远低于当前上下文的命中率（用户反馈的「不对」即此）。
  const sessionCachePct = usage.input > 0 ? (usage.cacheRead / usage.input) * 100 : undefined
  // 本轮写入缓存量（最近一轮主 agent 调用 cache_write；与命中率同一条 turn 数据源）——
  // 诊断「缓存没命中」时它与命中率配对看：命中 0 + 写入=整段上下文 ⇒ 前缀被重写。
  const ctxCacheWrite = (() => {
    for (let i = turns.length - 1; i >= 0; i--) {
      const tn = turns[i]
      if (activeView && isSubAgentRun(activeView, tn.runId ?? '')) continue
      return tn.cacheWrite
    }
    return undefined
  })()
  const ctxCachePct = (() => {
    for (let i = turns.length - 1; i >= 0; i--) {
      const tn = turns[i]
      if (activeView && isSubAgentRun(activeView, tn.runId ?? '')) continue // 子 agent 轮不属于「当前上下文」
      return tn.input > 0 ? (tn.cacheRead / tn.input) * 100 : undefined
    }
    return undefined
  })()
  // ctx 环 hover：上下文构成面板。140ms 延迟——快速划过（如去点发送）不弹面板；
  // 面板是 ctx 的同级兄弟节点（不是 .ctx 的子节点），所以点面板不会误触「点击压缩」。
  const [ctxPanelOpen, setCtxPanelOpen] = useState(false)
  const ctxHoverTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  useEffect(() => () => { if (ctxHoverTimer.current) clearTimeout(ctxHoverTimer.current) }, [])
  const openCtxPanel = () => {
    if (ctxHoverTimer.current) clearTimeout(ctxHoverTimer.current)
    ctxHoverTimer.current = setTimeout(() => {
      setCtxPanelOpen(true)
      fetchCtxBreakdown() // 每次悬停刷新（一次毫秒级往返）：占比不展示陈旧值
    }, 140)
  }
  const closeCtxPanel = () => {
    if (ctxHoverTimer.current) clearTimeout(ctxHoverTimer.current)
    setCtxPanelOpen(false)
  }
  // 面板数据只认当前会话（切会话后迟到的响应已在 store 丢弃，此处再兜一层）
  const ctxPanelData = ctxBreakdown && ctxBreakdown.sessionId === activeSessionId ? ctxBreakdown : undefined
  // 模型下拉数据源：全部 Provider × 模型（分组渲染，组头 = Provider 名）。
  // 多 Provider 生效（设置里配置的都在此列出）；选中其他 Provider 的模型会连带切 Provider。
  // 显示名对齐 SettingsModal PROVIDER_DISPLAY（19 家内置 + 自定义回退原始 key）。
  const PROVIDER_LABELS: Record<string, string> = {
    deepseek: t('composer.providerDisplay.deepseek'), openai: t('composer.providerDisplay.openai'), opencode: t('composer.providerDisplay.opencode'), kimi: t('composer.providerDisplay.kimi'), zhipu: t('composer.providerDisplay.zhipu'),
    anthropic: 'Anthropic', groq: 'Groq', xai: 'xAI (Grok)', mistral: 'Mistral', minimax: 'MiniMax',
    qwen: t('composer.providerDisplay.qwen'), xiaomi: t('composer.providerDisplay.xiaomi'), together: t('composer.providerDisplay.together'), fireworks: t('composer.providerDisplay.fireworks'),
    cerebras: 'Cerebras', baseten: 'Baseten', nvidia: 'NVIDIA NIM', huggingface: 'Hugging Face', openrouter: 'OpenRouter',
  }
  const activeProv = settings.provider.active
  // 模型下拉数据源：全部 Provider × 模型（分组渲染，组头 = Provider 名）。
  // 对缺 models（含纯字符串/畸形条目）的 Provider 容错：跳过或当空模型处理，
  // 避免 Object.entries(undefined) 抛错拖垮整个 Composer（模式/模型/Effort 一起失效）。
  const providers = (Object.entries(settings.provider) as [string, { base_url?: string; models?: Record<string, number> } | string][])
    .filter(([key]) => key !== 'active')
    .map(([key, cfg]) => ({
      key,
      label: PROVIDER_LABELS[key] ?? key,
      models: Object.entries((cfg && typeof cfg === 'object' && cfg.models) || {}).map(([name, w]) => ({
        name,
        cap: fmtTokens(w),
        image: inputTypesForProvider(settings, key, name).includes('image'),
      })),
    }))
  const activeProvLabel = PROVIDER_LABELS[activeProv] ?? activeProv
  const visibleProviders = providerFilter === 'all' ? providers : providers.filter((provider) => provider.key === providerFilter)
  // 当前模型是否支持图片输入（Provider 设置里该模型 input_types 含 image）→ 控制上传按钮显隐
  const canImage = modelSupportsImage(settings, model)
  // 拖拽/粘贴落图的统一前置条件（模型支持 + 未满 + 队列未满 + 非压缩中 + 未在压图中）
  const canAcceptImageDrop = canImage && attachments.length < MAX_IMAGE_ATTACHMENTS && !queueFull && !compressing && !attachmentBusy
  // 拖拽落地总闸门（2026-09-17 扩展文档引用）：图片可收 或 文档引用未满，都算可落地
  //（模型不支持图片时仍可拖入文档——文档走 @引用，与模型输入类型无关）。
  const canAcceptDrop = canAcceptImageDrop || (!queueFull && !compressing && docRefs.length < MAX_DOC_REFS)

  // —— 语音输入（话筒）：录音 → STT 转写 → 文本插入输入框 ——
  // 配置齐全（enabled + base_url + model + api_key）才显示话筒；录音用 MediaRecorder，
  // 转写经主进程转发 OpenAI 兼容端点（Electron net.fetch，无 CORS、key 不进网页层）。
  const sttCfg = settings.stt
  const sttEnabled = Boolean(sttCfg?.enabled && sttCfg?.base_url?.trim() && sttCfg?.model?.trim() && sttCfg?.api_key?.trim())
  const [recording, setRecording] = useState(false)
  const [sttBusy, setSttBusy] = useState(false)
  const [sttError, setSttError] = useState('')
  const mediaRecorderRef = useRef<MediaRecorder | null>(null)
  const mediaChunksRef = useRef<Blob[]>([])
  const mediaMimeRef = useRef('audio/webm')

  const toggleRecording = async () => {
    if (recording) {
      // 停止录音 → 转写
      try { mediaRecorderRef.current?.stop() } catch { /* ignore */ }
      return
    }
    setSttError('')
    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true })
      // 优先选浏览器/Electron 支持的编码；webm 为 Chromium 系（Electron）通用
      const mime = ['audio/webm;codecs=opus', 'audio/webm', 'audio/ogg;codecs=opus', 'audio/mp4'].find((m) => MediaRecorder.isTypeSupported(m)) ?? ''
      mediaMimeRef.current = mime || 'audio/webm'
      const mr = new MediaRecorder(stream, mime ? { mimeType: mime } : undefined)
      mediaChunksRef.current = []
      mr.ondataavailable = (e) => { if (e.data.size > 0) mediaChunksRef.current.push(e.data) }
      mr.onstop = () => {
        // 停止后释放麦克风
        for (const t of stream.getTracks()) t.stop()
        const blob = new Blob(mediaChunksRef.current, { type: mediaMimeRef.current })
        if (blob.size === 0) { setRecording(false); return }
        setRecording(false)
        void transcribeBlob(blob)
      }
      mr.start()
      mediaRecorderRef.current = mr
      setRecording(true)
    } catch (e) {
      setSttError(t('composer.mic.denied'))
      console.warn('mic getUserMedia failed:', e)
    }
  }

  const transcribeBlob = async (blob: Blob) => {
    setSttBusy(true)
    setSttError('')
    try {
      const buf = await blob.arrayBuffer()
      const r = await getTransport().sttTranscribe(buf, blob.type || 'audio/webm')
      if (!r.ok) {
        setSttError(r.error ?? t('composer.mic.failed'))
        return
      }
      const text = (r.text ?? '').trim()
      if (!text) { setSttError(t('composer.mic.empty')); return }
      // 转写文本插入输入框：追加到现有草稿末尾（若末尾无空白则补一个空格）
      setText((prev) => {
        const sep = !prev || /\s$/.test(prev) ? '' : ' '
        return prev + sep + text
      })
      requestAnimationFrame(() => {
        textareaRef.current?.focus()
        const el = textareaRef.current
        if (el) el.setSelectionRange(el.value.length, el.value.length)
      })
    } catch (e) {
      setSttError(e instanceof Error ? e.message : String(e))
    } finally {
      setSttBusy(false)
    }
  }

  useEffect(() => {
    // 会话切换时关闭只对原草稿有意义的临时面板；草稿正文和附件由 draftKey 自动恢复。
    setCmdMenu(false)
    setAtOpen(false)
    setAttachmentNotice('')
  }, [draftKey])

  useEffect(() => {
    if (!menu) return
    const close = (e: MouseEvent) => {
      if (chipRef.current && !chipRef.current.contains(e.target as Node)) setMenu(false)
    }
    document.addEventListener('click', close)
    return () => document.removeEventListener('click', close)
  }, [menu])

  // @ 引用面板：点击外部 / Esc 关闭
  useEffect(() => {
    if (!atOpen) return
    const close = (e: MouseEvent) => {
      if (refPanelRef.current && !refPanelRef.current.contains(e.target as Node)) setAtOpen(false)
    }
    const esc = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setAtOpen(false)
    }
    document.addEventListener('mousedown', close)
    document.addEventListener('keydown', esc)
    return () => {
      document.removeEventListener('mousedown', close)
      document.removeEventListener('keydown', esc)
    }
  }, [atOpen])

  // 模型下拉展开时：把当前选中的那一行固定到滚动容器顶部（列表固定高度可滚动，选中项不会丢在视野外）
  useEffect(() => {
    if (!menu) return
    const body = popupBodyRef.current
    if (!body) return
    const cur = body.querySelector<HTMLElement>('.mi.cur')
    if (cur) body.scrollTop = cur.offsetTop // offsetTop 相对 .popup-body（position:relative）→ 选中项固定到顶部
  }, [menu])

  // Provider 筛选条：展开时把「当前活跃 Provider」的 tab 滚动定位到条内可见位置
  //（左右滑动条 + 自动定位；19 家时选中项可能被裁在视野外）
  useEffect(() => {
    if (!menu) return
    const bar = filterRef.current
    if (!bar) return
    const tab = bar.querySelector<HTMLElement>('button.cur')
    if (!tab) return
    const barRect = bar.getBoundingClientRect()
    const tabRect = tab.getBoundingClientRect()
    if (tabRect.left < barRect.left) {
      bar.scrollLeft += tabRect.left - barRect.left - 4 // 左侧被裁 → 向左滚回
    } else if (tabRect.right > barRect.right) {
      bar.scrollLeft += tabRect.right - barRect.right + 4 // 右侧被裁 → 向右滚出
    }
  }, [menu])

  // Provider 筛选条：滚动位置 → wrap 上的 has-left/has-right 类（驱动 ‹ › 箭头显隐与两侧渐隐）
  useEffect(() => {
    if (!menu) return
    const bar = filterRef.current
    const wrap = bar?.parentElement
    if (!bar || !wrap) return
    const sync = () => {
      const max = bar.scrollWidth - bar.clientWidth
      wrap.classList.toggle('has-left', bar.scrollLeft > 2)
      wrap.classList.toggle('has-right', bar.scrollLeft < max - 2)
    }
    sync()
    bar.addEventListener('scroll', sync, { passive: true })
    const ro = new ResizeObserver(sync)
    ro.observe(bar)
    return () => {
      bar.removeEventListener('scroll', sync)
      ro.disconnect()
    }
  }, [menu])

  // Provider 筛选条：鼠标拖拽滑动（按住左右拖；grab/grabbing 光标由 CSS 提供）。
  // 与箭头/滚动共存：pointerdown 记录起点，move 时 scrollLeft 跟随，超过阈值才视为拖拽
  //（避免误吞 tab 的 click）。
  useEffect(() => {
    if (!menu) return
    const bar = filterRef.current
    if (!bar) return
    let down = false
    let startX = 0
    let startLeft = 0
    let moved = false
    const onDown = (e: PointerEvent) => {
      if (e.button !== 0) return
      down = true
      moved = false
      startX = e.clientX
      startLeft = bar.scrollLeft
    }
    const onMove = (e: PointerEvent) => {
      if (!down) return
      const dx = e.clientX - startX
      if (Math.abs(dx) > 3) moved = true
      bar.scrollLeft = startLeft - dx
    }
    const onUp = (e: PointerEvent) => {
      down = false
      // 拖拽过的本次按下不再触发 tab 点击（click 在 pointerup 后派发）
      if (moved) {
        e.preventDefault()
        const cancel = (ev: MouseEvent) => {
          ev.stopPropagation()
          ev.preventDefault()
          document.removeEventListener('click', cancel, true)
        }
        document.addEventListener('click', cancel, true)
      }
    }
    bar.addEventListener('pointerdown', onDown)
    bar.addEventListener('pointermove', onMove)
    bar.addEventListener('pointerup', onUp)
    bar.addEventListener('pointerleave', onUp)
    return () => {
      bar.removeEventListener('pointerdown', onDown)
      bar.removeEventListener('pointermove', onMove)
      bar.removeEventListener('pointerup', onUp)
      bar.removeEventListener('pointerleave', onUp)
    }
  }, [menu])

  useEffect(() => {
    if (!cmdMenu) return
    const close = (e: MouseEvent) => {
      if (cmdRef.current && !cmdRef.current.contains(e.target as Node)) setCmdMenu(false)
    }
    document.addEventListener('click', close)
    return () => document.removeEventListener('click', close)
  }, [cmdMenu])

  // 斜杠命令列表：启用技能（/技能名）+ 内置（/summary /clear）；按输入前缀过滤。
  // 补全只在「正在输命令前缀」时展示：第一行首个词是某命令的严格前缀（未完整、后无内容）。
  // 命令完整（/code-review）或命令后已跟正文（/code-review 帮我…）→ 菜单关闭，不误报「无匹配」。
  const slashCommands = [
    ...skills.filter((s) => s.enabled).map((s) => `/${s.name}`),
    ...BUILTIN_SLASH,
  ]
  const firstLine = text.split('\n')[0]
  const firstToken = (firstLine.split(/\s/)[0] ?? '').trim()
  const completing = firstToken.startsWith('/') && slashCommands.some((c) => c.toLowerCase().startsWith(firstToken.toLowerCase()) && c.toLowerCase() !== firstToken.toLowerCase())
  const filtered = slashCommands.filter((c) => c.toLowerCase().startsWith(firstToken.toLowerCase()))
  // 只有存在匹配项且仍在输入命令前缀时显示补全菜单；未知的 /path
  //（例如 /api-test/xxx）保持普通文本，不显示“未识别 command”。
  const cmdOpen = cmdMenu && completing && filtered.length > 0

  const completeCommand = () => {
    if (!cmdOpen || filtered.length === 0) return false
    const selected = filtered[0]
    const prefixLen = firstToken.length
    setText((prev) => selected + prev.slice(prefixLen))
    setCmdMenu(false)
    requestAnimationFrame(() => {
      const input = textareaRef.current
      if (input) {
        input.focus()
        const pos = selected.length
        input.setSelectionRange(pos, pos)
      }
    })
    return true
  }

  // —— @ 引用工具：token → 绝对路径 / 展示路径；提交时展开为 FilePath / Dir 文本块 ——
  // @xxx（相对）以工作区为基准解析；绝对路径（/ 或 X:\）原样；%20 解码为空格
  const resolveRef = (pathTok: string): string => {
    const tok = pathTok.replace(/%20/g, ' ')
    if (tok.startsWith('/') || /^[A-Za-z]:[\\/]/.test(tok)) return tok.replace(/\/$/, '')
    return (workspace ? `${workspace.replace(/\/+$/, '')}/` : '') + tok.replace(/^\.\//, '')
  }
  // 展示路径：工作区内 → ./相对；工作区外 → 绝对。路径含空格时 %20 转义（@token 以空白为界）
  const displayPath = (abs: string): string => {
    let p: string
    if (workspace && abs.startsWith(`${workspace}/`)) {
      const rel = abs.slice(workspace.length).replace(/^\/+/, '')
      p = rel ? `./${rel}` : '.'
    } else {
      p = abs
    }
    return p.replace(/ /g, '%20')
  }

  // 面板选中文件/目录 → 在 atStart 处替换成 @展示路径，并自动在 token 后补一个空格
  //（防止后续继续输入时与前边的 @引用 合并成一个 token：@./src文件名 → @./src 文件名）
  const pickRef = (abs: string, _isDir: boolean) => {
    const target = `@${displayPath(abs)}`
    setText((prev) => {
      // token 结束：@ 之后直到空白
      let end = atStart + 1
      while (end < prev.length && !/\s/.test(prev[end])) end++
      // 已有分隔空白则复用；否则补单一空格
      const sep = end < prev.length && /\s/.test(prev[end]) ? '' : ' '
      return prev.slice(0, atStart) + target + sep + prev.slice(end)
    })
    setAtOpen(false)
    requestAnimationFrame(() => textareaRef.current?.focus())
  }

  // 文件选择 / 剪贴板粘贴 / 拖拽共用同一条校验+压缩+读取链。
  // 体积口径集中在 lib/imageAttach（与 bridge/tools 三层闸门同口径）：超限图在
  // 采集入口即降采样压缩，避免整批 ask_batch 撞 stdin 单行上限而失败。
  const addImageFiles = async (files: File[]) => {
    if (queueFull || files.length === 0) return
    if (!canImage) {
      setAttachmentNotice(t('composer.attach.noImage'))
      return
    }
    const images = files.filter((f) => f.type.startsWith('image/'))
    const skipped = files.length - images.length
    const existingBytes = attachments.reduce((n, a) => n + dataUrlBytes(a.content), 0)
    const currentCount = attachments.length
    setAttachmentBusy(true)
    let res: Awaited<ReturnType<typeof acceptImages>>
    try {
      res = await acceptImages(images, existingBytes, MAX_IMAGE_ATTACHMENTS, currentCount)
    } finally {
      setAttachmentBusy(false)
    }
    if (res.accepted.length) {
      setAttachments((prev) => {
        const room = Math.max(0, MAX_IMAGE_ATTACHMENTS - prev.length)
        return [...prev, ...res.accepted.slice(0, room).map((a) => ({ type: 'image' as const, content: a.dataUrl, mimeType: a.mimeType }))]
      })
    }
    // 提示优先级：数量满 > 压缩过 > 部分被拒（含体积超限）。
    const compressed = res.accepted.filter((a) => a.compressed)
    if (currentCount >= MAX_IMAGE_ATTACHMENTS) {
      setAttachmentNotice(t('composer.attach.max').replace('{n}', String(MAX_IMAGE_ATTACHMENTS)))
    } else if (res.tooLarge > 0) {
      setAttachmentNotice(t('composer.attach.tooLarge').replace('{n}', fmtBytes(MAX_IMAGE_DATA_URL_BYTES)))
    } else if (compressed.length > 0) {
      const total = compressed.reduce((n, a) => n + a.originalBytes, 0)
      setAttachmentNotice(t('composer.attach.compressed').replace('{n}', fmtBytes(total)))
    } else if (skipped > 0 || res.rejected > 0) {
      setAttachmentNotice(t('composer.attach.someFailed'))
    } else {
      setAttachmentNotice('')
    }
  }

  // <input> 的 onChange：图片走内联（data URL），其余类型走 @引用（不复制）。
  const onFiles = (files: FileList | null) => {
    const all = Array.from(files ?? [])
    if (all.length) {
      const images = all.filter((f) => f.type.startsWith('image/'))
      const docs = all.filter((f) => !f.type.startsWith('image/'))
      if (images.length) void addImageFiles(images)
      if (docs.length) void addDocFiles(docs)
    }
    if (fileInputRef.current) fileInputRef.current.value = ''
  }

  // —— 文档附件（本地文件引用，不复制、不上传）——
  // 单一路径入口：所有来源（「添加文件」按钮 / <input> / 拖拽）都先拿到**绝对路径**，
  // 再经 displayPath() 拼成 @token（工作区内 → ./相对；外 → 绝对；空格 → %20）。
  // 这样工作区外的 ~/Desktop、~/Downloads 文件也能被 fileref 展开（闸门 1 已放开）。
  const docTokenOf = (abs: string): string => `@${displayPath(abs)}`
  const basenameOf = (abs: string): string => abs.split(/[\\/]/).filter(Boolean).pop() ?? abs

  // 批量加入文档引用：去重（同路径不重复引）、截断到 MAX_DOC_REFS，逐条取 size 展示。
  // 提示优先级（都不静默）：数量超限 > 已加入过（重复）> stat 失败（大小显示 —）。
  const addDocPaths = async (paths: string[]) => {
    const clean = paths.filter((p) => typeof p === 'string' && p.trim())
    if (clean.length === 0) return
    const existing = new Set(docRefs.map((d) => d.path))
    const fresh: string[] = []
    let dup = 0
    for (const p of clean) {
      if (existing.has(p)) { dup++; continue }
      existing.add(p)
      fresh.push(p)
    }
    const room = Math.max(0, MAX_DOC_REFS - docRefs.length)
    const take = fresh.slice(0, room)
    if (take.length === 0) {
      setAttachmentNotice(
        docRefs.length >= MAX_DOC_REFS
          ? t('composer.attach.docMax').replace('{n}', String(MAX_DOC_REFS))
          : t('composer.attach.docDupe'),
      )
      return
    }
    const stats = await Promise.all(take.map(async (p) => {
      try {
        const r = await getTransport().fsStat(p)
        return { path: p, name: basenameOf(p), size: r.ok && typeof r.size === 'number' ? r.size : -1 }
      } catch {
        return { path: p, name: basenameOf(p), size: -1 } // stat 不可用不阻断引用（大小显示 —）
      }
    }))
    setDocRefs((prev) => [...prev, ...stats.slice(0, Math.max(0, MAX_DOC_REFS - prev.length))])
    if (fresh.length > take.length || docRefs.length + take.length >= MAX_DOC_REFS) {
      setAttachmentNotice(t('composer.attach.docMax').replace('{n}', String(MAX_DOC_REFS)))
    } else if (dup > 0) {
      setAttachmentNotice(t('composer.attach.docDupe'))
    } else {
      setAttachmentNotice('')
    }
  }

  // File（拖拽 / <input>）→ 绝对路径：Electron 的 webUtils.getPathForFile 只对**磁盘文件**
  // 返回路径（JS 构造的 File 返回空串）。空串必须提示而不是静默丢弃（旧实现只收图片、
  // 非图片静默丢弃，用户无从知道为什么没反应）。
  const addDocFiles = async (files: File[]) => {
    const tr = getTransport()
    const paths: string[] = []
    let noPath = ''
    for (const f of files) {
      let p: string
      try { p = tr.getPathForFile(f) ?? '' } catch { p = '' }
      if (p) paths.push(p)
      else noPath = f.name || 'file'
    }
    if (paths.length) await addDocPaths(paths)
    if (noPath) setAttachmentNotice(t('composer.attach.noPath').replace('{name}', noPath))
  }

  // 「添加文件」按钮：原生多选对话框（fs:pick-file + multiSelections）。
  const pickDocFiles = async () => {
    if (queueFull || compressing) return
    let picked: string[]
    try {
      picked = await getTransport().pickFsFiles({ filters: [{ name: 'Documents', extensions: DOC_EXTENSIONS }] })
    } catch (e) {
      setAttachmentNotice(t('composer.attach.docFailed'))
      console.warn('pickFsFiles failed:', e)
      return
    }
    if (picked.length) await addDocPaths(picked)
  }

  const onPaste = (event: ReactClipboardEvent<HTMLTextAreaElement>) => {
    const imageFiles = Array.from(event.clipboardData.items)
      .filter((item) => item.kind === 'file' && item.type.startsWith('image/'))
      .map((item) => item.getAsFile())
      .filter((file): file is File => Boolean(file))
    if (imageFiles.length === 0) return // 纯文本保持浏览器原生粘贴

    const pastedText = event.clipboardData.getData('text/plain')
    const canAcceptImage = canImage && attachments.length < MAX_IMAGE_ATTACHMENTS && !queueFull && !compressing
    if (!canAcceptImage) {
      if (!canImage) setAttachmentNotice(t('composer.attach.noImage'))
      else if (attachments.length >= MAX_IMAGE_ATTACHMENTS) setAttachmentNotice(t('composer.attach.max').replace('{n}', String(MAX_IMAGE_ATTACHMENTS)))
      // 有文本时不阻止原生粘贴；纯图片无可插入内容，阻止浏览器产生无意义内容。
      if (!pastedText) event.preventDefault()
      return
    }

    event.preventDefault()
    if (pastedText) {
      const input = event.currentTarget
      const start = input.selectionStart ?? text.length
      const end = input.selectionEnd ?? start
      const inserted = insertTextAtSelection(text, pastedText, start, end)
      setText(inserted.value)
      requestAnimationFrame(() => {
        textareaRef.current?.focus()
        textareaRef.current?.setSelectionRange(inserted.caret, inserted.caret)
      })
    }
    void addImageFiles(imageFiles)
  }

  // —— 拖拽上传（P2-6）：窗口内任意位置拖入图片即添加为附件 ——
  // 拖拽态用计数器（dragenter/dragleave 在子元素间穿梭会成对抖动，布尔量会闪）。
  // 只接受含文件的拖拽（拖文本/链接不拦截，保持浏览器默认行为）。
  const onDragEnter = (event: ReactDragEvent<HTMLDivElement>) => {
    if (!hasFileDrag(event.dataTransfer)) return
    event.preventDefault()
    dragDepthRef.current += 1
    setDragKind(dragKindOf(event.dataTransfer))
    setDragActive(true)
  }
  const onDragOver = (event: ReactDragEvent<HTMLDivElement>) => {
    if (!hasFileDrag(event.dataTransfer)) return
    // 必须 preventDefault：否则浏览器把拖入当导航（直接打开本地文件、丢掉当前页面）。
    event.preventDefault()
    event.dataTransfer.dropEffect = canAcceptDrop ? 'copy' : 'none'
  }
  const onDragLeave = (event: ReactDragEvent<HTMLDivElement>) => {
    if (!hasFileDrag(event.dataTransfer)) return
    dragDepthRef.current = Math.max(0, dragDepthRef.current - 1)
    if (dragDepthRef.current === 0) setDragActive(false)
  }
  const onDrop = (event: ReactDragEvent<HTMLDivElement>) => {
    if (!hasFileDrag(event.dataTransfer)) return
    event.preventDefault()
    dragDepthRef.current = 0
    setDragActive(false)
    const all = Array.from(event.dataTransfer.files ?? [])
    if (all.length === 0) return
    const images = all.filter((f) => f.type.startsWith('image/'))
    const docs = all.filter((f) => !f.type.startsWith('image/'))
    // 图片与文档分别校验（两者额度独立）：图片不行不代表文档也不行，反之亦然。
    // 旧实现在这里把非图片**静默丢弃**，用户拖了文件却毫无反馈——现在非图片走 @引用，
    // 只有在真的收不下（额度满/处理中）时才提示。
    if (images.length && !canAcceptImageDrop) {
      setAttachmentNotice(!canImage ? t('composer.attach.noImage') : t('composer.attach.busy'))
    } else if (images.length) {
      void addImageFiles(images)
    }
    if (docs.length) {
      if (queueFull || compressing) setAttachmentNotice(t('composer.attach.busy'))
      else void addDocFiles(docs)
    }
  }

  const submit = async () => {
    const raw = text
    const trimmed = raw.trim()
    if (!trimmed && attachments.length === 0 && docRefs.length === 0) return
    if (queueFull || expanding || compressing) return
    const attach = attachments.length ? [...attachments] : undefined
    if (attach && !canImage) return // 模型不支持图片输入：图被禁掉后的兜底（按钮已隐藏）
    // 文档引用拼成一行 @引用（既有语法，服务端 fileref 会自动展开内容给模型）。
    // 为什么用 @引用 而不是新协议块：fileref 已实现展开 + 截断检测 + 二进制检测 +
    // 敏感路径闸门，复用即零协议改动；且用户能在历史里看到自己引了什么（可读、可编辑）。
    const refsLine = docRefs.map((d) => docTokenOf(d.path)).join(' ')
    const withDocRefs = (message: string) => {
      const body = message.trim()
      if (!refsLine) return body
      return body ? `${body}\n${refsLine}` : refsLine
    }
    // 图文/文档消息必须把展开后的文本块与图片块放在同一条消息中；ask 的 contents 参数
    // 表示完整内容块，不能只传附件，否则文本会被附件块替换掉。
    const withAttachments = (message: string, imageBlocks?: QueuedContent[]) => {
      const body = withDocRefs(message)
      if (!imageBlocks?.length) {
        // 无图片：只有「本次真有文档引用」才需要显式传 contents（否则保持 undefined 的既有链路）
        return refsLine && body ? [{ type: 'text' as const, content: body }] : undefined
      }
      return [
        ...(body ? [{ type: 'text' as const, content: body }] : []),
        ...imageBlocks,
      ]
    }
    setExpanding(true)
    const lines = raw.split('\n')
    const first = lines[0].trim()
    try {
      if (slashCommands.some((c) => c.toLowerCase() === first.toLowerCase())) {
        // 仅完整匹配已知命令时才按命令发送；未知 /path 一律作为普通文本。
        ask(first)
        const body = lines.slice(1).join('\n').trim()
        if (body || attach || refsLine) ask(body ? body : t('composer.attach.pleaseView'), withAttachments(body ? body : t('composer.attach.pleaseView'), attach))
      } else {
        // @引用展开已下沉到 AgentHarness 层（Go agents）：此处保留原文（@./path）发送，
        // 历史回显即原文；展开由服务端在拼接 LLM Messages 前完成（refs_loaded 事件反馈）。
        ask(withDocRefs(raw), withAttachments(raw, attach))
      }
    } finally {
      setExpanding(false)
      setText('')
      setCmdMenu(false)
      setAttachments([])
      setDocRefs([]) // 与图片附件一致：发出即清空（引用已进消息正文，不再重复携带）
    }
  }

  return (
    <div className="composer-wrap">
      <AgentRunningBar />
      <div
        className={`composer${compressing ? ' compressing' : ''}${dragActive ? ' drag-active' : ''}`}
        aria-busy={compressing}
        onDragEnter={onDragEnter}
        onDragOver={onDragOver}
        onDragLeave={onDragLeave}
        onDrop={onDrop}
      >
        {dragActive && (
          <div className="drop-hint" role="status" aria-live="polite">
            {dragKind === 'image'
              ? (canAcceptImageDrop
                ? t('composer.attach.drop').replace('{n}', String(MAX_IMAGE_ATTACHMENTS - attachments.length))
                : !canImage ? t('composer.attach.noImage') : t('composer.attach.busy'))
              // 文档（或混合）拖拽：@引用与模型输入类型无关，只要队列/压缩态允许就收
              : (queueFull || compressing)
                ? t('composer.attach.busy')
                : docRefs.length >= MAX_DOC_REFS
                  ? t('composer.attach.docMax').replace('{n}', String(MAX_DOC_REFS))
                  : t('composer.attach.docDrop')}
          </div>
        )}
        {compressing && (
          <div className="composer-status" role="status" aria-live="polite">
            <span className="composer-status-spinner" aria-hidden="true" />
            <strong>{t('composer.compressing')}</strong>
            <span className="composer-status-detail">{t('composer.compressing.detail')}</span>
          </div>
        )}
        {pendingQueue.length > 0 && (
          <div className="pending-queue" role="status" aria-label={t('composer.pending')}>
            <div className={`pq-head ${queueFull ? 'full' : ''}`}>
              <span>{t('composer.pending')} {pendingQueue.length}/{QUEUE_CAP}</span>
              <span className="pq-sub">{queueFull ? t('composer.pq.full') : t('composer.pq.busy')}</span>
            </div>
            {pendingQueueError && (
              <div className="pq-error" role="alert" style={{ color: 'var(--error, #e5484d)', padding: '2px 8px', fontSize: 12 }}>
                {pendingQueueError}{t('composer.pq.returned')}
              </div>
            )}
            <div className="pq-list">
              {pendingQueue.map((q, i) => (
                <div className={`pq-item ${editingId === q.id ? 'editing' : ''}`} key={q.id}>
                  <span className="pq-no">{i + 1}</span>
                  {editingId === q.id ? (
                    <>
                      <textarea
                        className="pq-edit-input"
                        rows={2}
                        value={editText}
                        autoFocus
                        onChange={(e) => setEditText(e.target.value)}
                        onKeyDown={(e) => {
                          if (e.key === 'Enter' && !e.shiftKey) {
                            // 输入法组合态（中文等用 Enter 选字/上屏）不得提交编辑
                            if (e.nativeEvent.isComposing || e.keyCode === 229) return
                            e.preventDefault()
                            updatePending(q.id, editText)
                            setEditingId(null)
                          }
                        }}
                      />
                      <span className="pq-actions">
                        <button className="pq-btn primary" onClick={() => { updatePending(q.id, editText); setEditingId(null) }}>{t('composer.pq.confirm')}</button>
                        <button className="pq-btn" onClick={() => setEditingId(null)}>{t('composer.pq.cancel')}</button>
                      </span>
                    </>
                  ) : (
                    <>
                      <span className="pq-text" title={queuedText(q) || t('composer.pq.attach')}>
                        {queuedHasAttach(q) && <span className="pq-attach">📎</span>}
                        {queuedText(q) || t('composer.pq.attach')}
                      </span>
                      <span className="pq-actions">
                        {q.pushed ? (
                          <span className="pq-pushed" title={t('composer.pq.pushedHint')}>{t('composer.pq.pushed')}</span>
                        ) : (
                          <>
                            <button className="pq-btn" onClick={() => { setEditingId(q.id); setEditText(queuedText(q)) }}>{t('composer.pq.edit')}</button>
                            <button className="pq-btn" onClick={() => removePending(q.id)} title={t('composer.pq.remove')}>×</button>
                          </>
                        )}
                      </span>
                    </>
                  )}
                </div>
              ))}
            </div>
          </div>
        )}
        <div ref={cmdRef}>
          {cmdOpen && (
            <div className="cmd-menu" role="listbox" aria-label={t('composer.cmdMenuAria')} onClick={(e) => e.stopPropagation()}>
              {filtered.length === 0 ? (
                <div className="hint">{t('composer.noCmd')}</div>
              ) : (
                filtered.map((c) => (
                  <div key={c} className="mi" role="option" onClick={() => { setText(c + ' '); setCmdMenu(false); requestAnimationFrame(() => textareaRef.current?.focus()) }} title={c}>
                    <span className="spark">✦</span>
                    <span className="nm">{c}</span>
                  </div>
                ))
              )}
            </div>
          )}
        </div>
        {atOpen && workspace && (
          <div ref={refPanelRef}>
            <FileRefPanel
              workspace={workspace}
              filter={atFilter}
              onPick={pickRef}
              onClose={() => setAtOpen(false)}
            />
          </div>
        )}
        {(attachments.length > 0 || docRefs.length > 0) && (
          <div className="attach-strip" role="list" aria-label={t('composer.attach.strip')}>
            {attachments.map((a, i) =>
              a.type === 'image' ? (
                <div className="attach-chip" role="listitem" key={i} title={t('composer.attach.remove')}>
                  <img src={a.content} alt="" />
                  <button
                    className="attach-x"
                    onClick={() => setAttachments((prev) => prev.filter((_, j) => j !== i))}
                    aria-label={t('composer.attach.remove')}
                  >×</button>
                </div>
              ) : null,
            )}
            {/* 文档附件：文件名 + 大小 + 移除（不复制，仅记路径 → 发送时进 @引用）。
                size < 0 = stat 不可用，显示 — 而不是假造 0B。 */}
            {docRefs.map((d, i) => (
              <div className="attach-chip doc-chip" role="listitem" key={`doc-${d.path}`} title={`${d.path}\n${t('composer.attach.docHint')}`}>
                <span className="doc-chip-name">{d.name}</span>
                <span className="doc-chip-size">{d.size >= 0 ? fmtBytes(d.size) : '—'}</span>
                <button
                  className="attach-x"
                  onClick={() => setDocRefs((prev) => prev.filter((_, j) => j !== i))}
                  aria-label={t('composer.attach.docRemove')}
                >×</button>
              </div>
            ))}
          </div>
        )}
        {attachmentNotice && <div className="attach-notice" role="status" aria-live="polite">{attachmentNotice}</div>}
        <div className="input-wrap">
          <div className="input-hl" aria-hidden>
            <div className="input-hl-inner" ref={hlRef}>{text ? highlightInput(text, slashCommands) : ''}</div>
          </div>
          <textarea
            ref={textareaRef}
            name="message"
            className="input"
            rows={1}
            placeholder={compressing ? t('composer.compressing.wait') : queueFull ? t('composer.queueFull').replace('{n}', String(QUEUE_CAP)) : t('composer.placeholder')}
            value={text}
            readOnly={queueFull || compressing}
            disabled={compressing}
            onPaste={onPaste}
            onChange={(e) => {
              const v = e.target.value
              const pos = e.target.selectionStart ?? v.length
              setText(v)
              if (v.trim().startsWith('/')) {
                if (!cmdMenu) fetchSkills() // 命令列表数据源：技能清单（启用过滤；仅首开拉取）
                setCmdMenu(true)
              } else {
                setCmdMenu(false)
              }
              // @ 引用（2026-08-30 正则化，输入与删除对称）：基于光标前文本识别
              // 「正在输入的 @token」——词边界（行首/空白）起始的 @ + 其后无空白。
              // 覆盖：输入 @ 打开（token 空）、token 内继续过滤、空白关闭；
              // 新增：面板关闭后回删 token 中间（@./doc → @./do）也重新唤醒
              //（旧实现只认光标紧贴 @，删除场景无法重开）。
              const m = v.slice(0, pos).match(/(^|[\s])@([^\s]*)$/)
              if (m) {
                const atIdx = pos - (m[2]?.length ?? 0) - 1 // @ 恒在 token 前 1 位（(^|[\s]) 前导不影响索引）
                if (!atOpen || atStart !== atIdx) setAtStart(atIdx)
                setAtFilter(m[2] ?? '')
                setAtOpen(true)
              } else {
                setAtOpen(false)
              }
            }}
            onKeyDown={(e) => {
              // Tab：/ 命令菜单 → 选中第一个补全（completeCommand 此前从未接线，
              // 2026-08-30 恢复）；@ 面板 → FileRefPanel 的 document capture 监听处理。
              if (e.key === 'Tab') {
                if (completeCommand()) {
                  e.preventDefault()
                  e.stopPropagation()
                  return
                }
                if (atOpen) {
                  e.preventDefault()
                  e.stopPropagation()
                  return
                }
                return
              }
              if (e.key === 'Enter' && !e.shiftKey) {
                // 输入法组合态（中文等用 Enter 选字/上屏）不得触发发送；
                // isComposing + keyCode 229 双保险兼容各平台 IME。
                if (e.nativeEvent.isComposing || e.keyCode === 229) return
                e.preventDefault()
                void submit()
              }
            }}
            onScroll={(e) => {
              // 背景 overlay 滚动同步（textarea 透明文字 + 徽章高亮）
              if (hlRef.current) hlRef.current.style.transform = `translateY(${-e.currentTarget.scrollTop}px)`
            }}
          />
        </div>
        <div className="row2">
          {/* 模式恒在最左（P 布局决策）；模型/思考强度/上下文/发送归右组 */}
          <ChipMenu
            className="mode-chip"
            label={<><span className="compact-chip-icon"><AccessIcon size={14} /></span><span className="chip-full-label"><span style={{ color: 'var(--text-3)' }}>{t('composer.mode')}</span> <span className="mono">{modes.find((m) => m.key === mode)?.label ?? mode}</span></span></>}
            items={modes}
            value={mode}
            onSelect={compressing ? () => undefined : switchMode}
          />
          <div className="row2-right">
            <div
              ref={chipRef}
              className="model-chip"
              role="combobox"
              aria-expanded={menu}
              aria-haspopup="listbox"
              aria-disabled={compressing}
              title={`${activeProvLabel} · ${model}`} /* label 过长时省略号截断 → hover 可见全名 */
              onClick={() => { if (!compressing) setMenu((v) => !v) }}
            >
              <span className="spark"><Sparkles size={14} /></span><span className="model-chip-label">{activeProvLabel} · {model}</span><ChevronDown size={12} />
              {menu && (
                <div className="popup open" role="listbox" onClick={(e) => e.stopPropagation()}>
                  <div className="model-provider-filter-wrap" id="provider-filter-wrap">
                    {/* Provider 筛选条：横向滑动（滚轮/拖拽/‹ › 箭头），19 家不溢出；
                        选中 Provider 自动滚动定位（useEffect 按 activeProv 对齐） */}
                    <button
                      className="provider-scroll-cue cue-left"
                      aria-label={t('common.scrollLeft')}
                      onClick={(e) => {
                        e.stopPropagation()
                        const el = filterRef.current
                        if (el) el.scrollBy({ left: -140, behavior: 'smooth' })
                      }}
                    >
                      <ChevronLeft size={12} />
                    </button>
                    <div className="model-provider-filter" ref={filterRef}>
                      <button className={providerFilter === 'all' ? 'on' : ''} onClick={() => setProviderFilter('all')}>{t('common.all')}</button>
                      {providers.map((provider) => (
                        <button
                          key={provider.key}
                          className={`${providerFilter === provider.key ? 'on' : ''} ${activeProv === provider.key ? 'cur' : ''}`}
                          onClick={() => setProviderFilter(providerFilter === provider.key ? 'all' : provider.key)}
                        >
                          {provider.label}
                        </button>
                      ))}
                    </div>
                    <button
                      className="provider-scroll-cue cue-right"
                      aria-label={t('common.scrollRight')}
                      onClick={(e) => {
                        e.stopPropagation()
                        const el = filterRef.current
                        if (el) el.scrollBy({ left: 140, behavior: 'smooth' })
                      }}
                    >
                      <ChevronRight size={12} />
                    </button>
                  </div>
                  <div className="popup-body" ref={popupBodyRef}>
                    {visibleProviders.map((prov) => (
                      <div className="model-provider-group" key={prov.key}>
                        <div className="popup-group-hd">{prov.label}</div>
                        {prov.models.map((m) => {
                          const current = activeProv === prov.key && m.name === model
                          return <div
                            key={m.name}
                            className={`mi model-option ${current ? 'cur' : ''}`}
                            onClick={() => {
                              switchModel(m.name, prov.key)
                              setMenu(false)
                            }}
                          >
                            <span className="model-check">{current ? '✓' : ''}</span>
                            <span className="nm">{m.name}</span>
                            {m.image && <span className="model-vision" title={t('common.imageInput')}><ImageIcon size={12} /> {t('common.image')}</span>}
                            <span className="cap">{m.cap}</span>
                          </div>
                        })}
                      </div>
                    ))}
                  </div>
                  <div className="hint">{t('composer.modelHint')}</div>
                </div>
              )}
            </div>
            <ChipMenu
              className="effort-chip"
              label={<><span className="compact-chip-icon"><EffortIcon size={14} /></span><span className="chip-full-label"><span style={{ color: 'var(--text-3)' }}>Effort</span> <span className="mono">{EFFORTS.find((e) => e.key === effort)?.label ?? effort}</span></span></>}
              items={effortItems}
              value={effort}
              onSelect={compressing ? () => undefined : switchEffort}
            />
            {/* hover / 键盘聚焦都能展开构成面板（focus 走同一条路径：键盘用户也能看到占比） */}
            <div
              className="ctx-wrap"
              onMouseEnter={openCtxPanel}
              onMouseLeave={closeCtxPanel}
              onFocus={openCtxPanel}
              onBlur={closeCtxPanel}
            >
              <div
                className={`ctx ${pct >= 95 ? 'red' : pct >= 90 ? 'amber' : ''}`}
                role="button"
                tabIndex={0}
                aria-label={t('composer.ctxAria').replace('{used}', fmtTokens(ctxUsed)).replace('{total}', fmtTokens(window)).replace('{pct}', String(pct))}
                title={ctxEstimated ? t('composer.ctxTipEstimate') : pct >= 90 ? t('composer.ctxNear') : t('composer.ctxTip')}
                aria-disabled={compressing}
                onClick={() => { if (!compressing) setConfirmCompact(true) }}
                onKeyDown={(e) => {
                  if (compressing) return
                  if (e.key === 'Enter' || e.key === ' ') {
                    e.preventDefault()
                    setConfirmCompact(true)
                  }
                }}
              >
                <CtxRing pct={pct} />
                <span>{ctxEstimated ? '≈' : ''}{fmtTokens(ctxUsed)}/{fmtTokens(window)}</span>
                <span className="pct">{pct}%</span>
              </div>
              {/* hover 面板：上下文构成分区（Tools/Skills/MCP/系统提示词/消息/其他） */}
              {ctxPanelOpen && (
                <CtxBreakdownPanel
                  used={ctxUsed}
                  window={window}
                  pct={pct}
                  data={ctxPanelData}
                  ctxCachePct={ctxCachePct}
                  sessionCachePct={sessionCachePct}
                  ctxCacheWrite={ctxCacheWrite}
                  sessionCacheWrite={usage.cacheWrite}
                />
              )}
            </div>
            {/* 图片上传 / 添加文件：图片走内联 data URL（当前模型支持 image 才显示按钮）；
                其他类型一律走 @引用（不复制）——引用与模型输入类型无关，所以「添加文件」恒可见。 */}
            {canImage && (
              <>
                <input
                  ref={fileInputRef}
                  type="file"
                  accept={DOC_ACCEPT}
                  multiple
                  hidden
                  onChange={(e) => onFiles(e.target.files)}
                />
                <button
                  className={`attach-btn${attachmentBusy ? ' busy' : ''}`}
                  onClick={() => fileInputRef.current?.click()}
                  disabled={queueFull || compressing || attachmentBusy}
                  title={attachmentBusy ? t('composer.attach.compressing') : compressing ? t('composer.compressing.wait') : t('composer.attach.hint')}
                  aria-label={t('composer.attach.hint')}
                >
                  <ImageIcon size={15} />
                </button>
              </>
            )}
            {/* 添加文件（本地文件引用）：原生多选对话框 → @引用进消息，不复制、不上传 */}
            <button
              className="attach-btn doc-btn"
              onClick={() => void pickDocFiles()}
              disabled={queueFull || compressing}
              title={t('composer.attach.docHint')}
              aria-label={t('composer.attach.addFile')}
            >
              <PaperclipIcon size={15} />
            </button>
            {/* 语音输入（话筒）：设置里配置了语音模型才显示；按住说话 → 松手转写 → 文字入输入框 */}
            {sttEnabled && (
              <button
                className={`mic-btn ${recording ? 'recording' : ''}`}
                onClick={() => void toggleRecording()}
                disabled={sttBusy || queueFull || compressing}
                title={recording ? t('composer.mic.stop') : sttBusy ? t('composer.mic.transcribing') : t('composer.mic.hint')}
                aria-label={recording ? t('composer.mic.stop') : t('composer.mic.hint')}
                aria-pressed={recording}
              >
                {sttBusy ? <span className="mic-spinner" aria-hidden="true" /> : <MicIcon size={15} />}
              </button>
            )}
            {sttError && <span className="mic-error" role="alert">{sttError}</span>}
            <button
              className="send-btn"
              onClick={stopping ? interrupt : () => void submit()}
              disabled={expanding || (queueFull && !stopping)}
              title={compressing ? t('composer.compressing.abort') : expanding ? t('composer.expanding') : stopping ? t('composer.stop') : t('composer.send')}
              aria-label={compressing ? t('composer.compressing.abort') : expanding ? t('composer.expanding') : stopping ? t('composer.stop') : t('composer.send')}
            >
              {stopping ? <StopIcon /> : <SendIcon />}
            </button>
          </div>
        </div>
      </div>
      {confirmCompact && <CompactConfirm onClose={() => setConfirmCompact(false)} />}
    </div>
  )
}
