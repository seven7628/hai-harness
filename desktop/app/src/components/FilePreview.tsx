// 文件预览面板（原 RightPanel 内联，PR-04 provider 化时平移出来）
// 依赖：store（filePreviews/fileFocus/git 快照）、markdown/CsvPreview/UnifiedDiff 渲染器、lib 里的格式化与文件活动聚合。
import { useEffect, useMemo, useRef, useState } from 'react'
import { useAppStore, fileKeyOf } from '../store/useAppStore'
import type { FileTabInstance } from '../store/useAppStore'
import { useT } from '../i18n'
import { Markdown } from './markdown'
import { withAgentToolBlocks } from '../lib/agentSession'
import { isCsvPath } from '../lib/csv'
import { visualKindOf } from '../lib/visualFile'
import { baseDirFor } from '../lib/localAsset'
import { fileStatusLabel } from '../lib/fileStatus'
import { normalizeFilePath, sameFilePath } from '../lib/filePaths'
import { isEditorEnabled, isTextLikePath } from '../lib/editorState'
import { UnifiedDiff, fmtDuration } from './ui'
import { aggregateFileActivity } from '../lib/fileActivity'
import type { FileActivity, FileOperation } from '../lib/fileActivity'
// R08a：纯文本与 CSV 两条分支已迁到渲染器注册表（text / csv），这里只留宿主。
// markdown 预览那条**没动**（本轮三个渲染器不含 markdown）。
import { PreviewHost } from '../surfaces/preview/PreviewHost'
import { useCodeHtml } from '../surfaces/preview/useCodeHtml'
import { fileRefOf } from '../surfaces/preview/registry'
import { preloadRenderers } from '../surfaces/preview/loader'

// Markdown 文件判定（「最终文件」视图据此提供预览/纯文本双模式）：
// .md / .markdown / .mdx（MDX 也走同一套渲染 —— 它的 JSX 部分会按 HTML 处理，
// 比纯源码视图更接近作者意图；真要看源码切「纯文本」即可）。
function isMarkdownPath(p: string): boolean {
  const ext = (p.split('.').pop() || '').toLowerCase()
  return ext === 'md' || ext === 'markdown' || ext === 'mdx'
}

type FileFilter = 'all' | 'changed' | 'read'



// sameFilePath 路径同一性比较（容忍「绝对 vs 工作区相对」两种写法）。
//
// 背景（2026-09-15 实测缺陷）：文件活动列表（aggregateFileActivity）的 path 来自工具
// args、是工作区相对路径；而产出卡片（Artifacts）等外部入口可能给出工作区绝对路径。
// 直接用 `===` 比较会让同一文件被判为不同 → 右侧详情永远停在「正在读取文件…」。
// 归一化：去掉 ./、统一分隔符、绝对路径在工作区内时折叠为相对，再比较。

// toRelIfInside 工作区内的绝对路径 → 相对；其余原样（仅归一化分隔符与 ./ 前缀）。

function formatToolTimestamp(timestamp: number | undefined, t: (k: string) => string): string {
  if (timestamp == null || !Number.isFinite(timestamp)) return t('rp.file.timeUnknown')
  const date = new Date(timestamp)
  const pad = (value: number) => String(value).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`
}

function operationLocation(operation: FileOperation): string {
  const unified = operation.diff?.unified || ''
  const hunk = unified.match(/^@@\s+[^+]*\+(\d+)(?:,(\d+))?\s+@@(?:\s*(.*))?/m)
  if (!hunk) return ''
  const context = (hunk[3] || '').trim()
    .replace(/^(?:func|function|class|type|const|let|var)\s+/, '')
    .replace(/\s*[{(:].*$/, '')
    .trim()
  if (context) return context.slice(0, 30)
  const start = Number(hunk[1])
  const count = Number(hunk[2] || 1)
  return count > 1 ? `L${start}–${start + count - 1}` : `L${start}`
}


type FileDetailMode = 'operation' | 'file' | 'source'

// 文件双栏分栏条：master（列表）最小/默认宽度与 detail（全文/变更）最小宽度（px）。
// 默认 44% 由 CSS grid 承担（minmax(200px, 44%)），拖拽后以 px 覆盖。
const FILE_MASTER_MIN = 200
const FILE_DETAIL_MIN = 320

function OperationDiff({ operation }: { operation?: FileOperation }) {
  const t = useT()
  if (!operation) return <div className="file-activity-empty">{t('rp.file.noOperation')}</div>
  if (operation.diff?.unified) return <UnifiedDiff unified={operation.diff.unified} />
  return <div className="file-activity-empty">{t('rp.file.noDiff')}</div>
}

// —— 每个 file 标签自己的滚动位置（阶段 2「浏览器式标签」）——
//
// 为什么需要：切标签时本组件因 key={tab.id} 重挂载（见 RightPanel 的渲染点），DOM 是新的
// → 浏览器不会替我们记住滚动位置。而「关掉一个标签不影响另一个的滚动位置」是本阶段的
// 验收口径之一，所以按**标签 id** 存一份（模块级：组件实例会随标签切换而重建）。
//
// 存「容器角色 → scrollTop」而不是元素引用：重挂载后元素是新的；同一标签的 path/mode 不变，
// 容器角色也就稳定（.fp-code / .md-preview-doc / .file-inspector-body / 列表）。
// 记录方式是**滚动时写**（capture 监听），不是卸载时读 DOM —— 卸载时机在 React 里不保证
// ref 还在（commitDeletion 会先 detach ref），读 DOM 会拿到 null 而静默丢掉位置。
const FILE_TAB_SCROLL_MAX = 32
const fileTabScroll = new Map<string, Record<string, number>>()
const SCROLL_ROLES: Array<[string, string]> = [
  ['master', '.file-activity-master'],
  ['body', '.file-inspector-body'],
  ['code', '.file-source .fp-code'],
  ['md', '.file-source .md-preview-doc'],
]
const scrollRoleOf = (el: HTMLElement): string | undefined =>
  SCROLL_ROLES.find(([, sel]) => el.matches(sel))?.[0]

// —— 每个 file 标签自己的「视图模式」（模块级，理由同上）——
// 用户在某个标签里选过「纯文本 / 表格」后切走再切回，期望的是**还在原来那个视图**（浏览器式
// 标签）。只记用户显式选过的两个模式开关；列表选中/展开/分隔条宽度这类**派生态**不记 ——
// 它们由标签 payload 派生（见下面的定位 effect），记了反而多一份真相源。
// 语义边界：**重新打开**同一个文件（openSeq 自增）仍然回默认视图（用户决策「不记住」），
// 只有「切走再切回」才恢复 —— 见下面 openSeq 的 effect。
type FileViewModes = { mdMode: 'preview' | 'plain'; csvMode: 'table' | 'plain' }
const FILE_TAB_VIEW_MAX = 32
const fileTabView = new Map<string, FileViewModes>()
const recallViewModes = (tabId: string): FileViewModes => fileTabView.get(tabId) ?? { mdMode: 'preview', csvMode: 'table' }
function rememberViewModes(tabId: string, patch: Partial<FileViewModes>): void {
  fileTabView.delete(tabId) // 先删再插 → 最近用过的排末尾，超出上限淘汰最久没动过的
  fileTabView.set(tabId, { ...recallViewModes(tabId), ...patch })
  while (fileTabView.size > FILE_TAB_VIEW_MAX) {
    const oldest = fileTabView.keys().next().value
    if (oldest === undefined) break
    fileTabView.delete(oldest)
  }
}

// 文件活动：按路径聚合工具调用与 Git 状态；点击对话中的文件可精确定位并高亮。
//
// 阶段 2（内容按标签隔离）：本组件的**数据源是激活的 file 标签**，不再是全局单例
// `filePreview` / `fileFocus`。三处口径随之改变：
//   · 内容 → `filePreviews[fileKeyOf(tab.path)]`（按归一化路径索引的缓存；同一文件的
//     diff / source 两个标签共享同一份内容，互不覆盖）；
//   · 「每次打开回默认视图」的触发器 → `tab.openSeq`（阶段 1 是全局 fileFocus.seq，
//     那会让重开 A 把 B 的视图一起复位）；
//   · 视图状态 → 由 `key={tab.id}`（见 RightPanel 的渲染点）保证每个标签一份实例。

export function FilePreviewTab({ tab }: { tab: FileTabInstance }) {
  const t = useT()
  const workspace = useAppStore((s) => s.workspace) // fileKeyOf / sameFilePath 判工作区内外用
  const filePreviews = useAppStore((s) => s.filePreviews)
  const ensureFilePreview = useAppStore((s) => s.ensureFilePreview)
  const tabPath = tab.path
  // 内容 = 本标签路径对应的缓存条目。缓存缺失时由下面的 effect 补拉（切工作区清空过 /
  // 被上限淘汰 / 从「最近关闭」重开都会走到这条）。
  const filePreview = useMemo(
    () => (tabPath ? filePreviews[fileKeyOf(tabPath, workspace)] : undefined),
    [filePreviews, tabPath, workspace],
  )
  const files = useAppStore((s) => s.gitFiles) ?? []
  const blocks = useAppStore((s) => s.blocks) ?? []
  const openFile = useAppStore((s) => s.openFile)
  const closeFile = useAppStore((s) => s.closeFile)
  // PR-05b：「编辑」入口。开关（settings.editor）与文件类型两道门都在这里，
  // 按钮**不出现**而不是禁用 —— 回滚开关的意义就是界面上看不到这条能力。
  const editorOn = useAppStore((s) => isEditorEnabled(s.settings))
  const openFileForEdit = useAppStore((s) => s.openFileForEdit)
  // 二进制预览入口（见 renderBinaryActions）：与产出卡走**同一条**通道 ——
  // pdf/图片/html/svg 用内嵌浏览器，xlsx/docx/pptx 先经 bridge 转 HTML 再进同一条浏览器。
  // 复用 store 动作而非在文件栏新写渲染，是为了「卡片能看 → 文件栏也能看」不会各自演化。
  const openHtmlInBrowser = useAppStore((s) => s.openHtmlInBrowser)
  const previewOfficeFile = useAppStore((s) => s.previewOfficeFile)
  // 转换在途的文件路径（xlsx/docx/pptx 任一）——驱动按钮「转换中…」与禁用，见 renderBinaryActions。
  const officeBusy = useAppStore((s) => s.officeBusy)
  const showToast = useAppStore((s) => s.showToast)
  // 高亮 HTML（text 渲染器的输入）：抽到 surfaces/preview/useCodeHtml，与独立预览面板
  // 共用同一实现 —— 包括「外观 → 代码主题」换到同侧另一个主题时必须重算这一条
  //（高亮结果是字符串，不像 CSS 变量会自动跟着走；此前踩过的缺陷见该文件顶部注释）。
  // filePreview 缺失 → ''（与原先 setHtml('') 的分支等价）。
  const html = useCodeHtml(filePreview?.content, filePreview?.path)
  // 预热本面板用到的两个渲染器模块：动态 import 是异步的，而下面「定位到读过的行」的
  // 锚点（.fp-code）最好与 html 同一次提交落地 —— 预热后 loader 命中缓存、同步挂载。
  useEffect(() => { preloadRenderers(['text', 'csv']) }, [])
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const [filter, setFilter] = useState<FileFilter>('all')
  const [highlightPath, setHighlightPath] = useState('')
  const [highlightTool, setHighlightTool] = useState('')
  const [selectedPath, setSelectedPath] = useState('')
  const [selectedTool, setSelectedTool] = useState('')
  const [detailMode, setDetailMode] = useState<FileDetailMode>('operation')
  // Markdown「最终文件」双模式（仅独立大视图 standalone 生效）：'preview' = 渲染后的
  // 文档（复用对话区 Markdown 管线：GFM/表格/mermaid/KaTeX/代码高亮），'plain' = shiki
  // 高亮源码（含行号）。用户决策：默认预览、不持久化（每次打开回到默认）、只做大视图。
  // 按标签记忆（阶段 2）：切走再切回恢复用户选过的模式（见 fileTabView）；**重新打开**同一文件
  // （openSeq 自增）仍回默认 —— 用户决策「不记住」的口径没变，变的是「谁算一次打开」。
  const [mdMode, setMdMode] = useState<'preview' | 'plain'>(() => recallViewModes(tab.id).mdMode)
  // CSV/TSV 双模式：'table' = 表格（默认，走 CsvPreview → GFM 表格），'plain' = shiki 高亮源码。
  // 与 mdMode 分开存，而不是合并成一个「显示模式」枚举：两者的默认值与可用取值不同
  // （Markdown 默认预览、CSV 默认表格），合并后判定「当前该显示什么」要多绕一层条件。
  const [csvMode, setCsvMode] = useState<'table' | 'plain'>(() => recallViewModes(tab.id).csvMode)
  // 双栏分隔条拖拽（master 像素宽度；不持久化，默认 44%，与 grid 初始一致）
  const [masterW, setMasterW] = useState<number | null>(null)
  const masterWStart = useRef<{ startX: number; startW: number; maxW: number } | null>(null)
  const panelRef = useRef<HTMLDivElement>(null)
  const rowRefs = useRef(new Map<string, HTMLDivElement>())
  const highlightTimer = useRef<number | undefined>(undefined)
  // 子智能体的工具调用同样要进文件活动（用户 2026-10 报障：子会话里点 write/edit 的路径
  // 「查看变更」与主会话不一致）。
  //
  // 机制：文件面板按 toolId 找 operation 来定位本次变更（见 FilePreviewTab 的
  // item.operations.find(op => op.toolId === selectedTool)）。而子运行的转录不在
  // v.blocks 里 —— 它在 agent 卡片的 items 上。只喂 v.blocks 的话，子会话里点开文件
  // 找不到那条调用，只能退回「最终文件」，看起来就像"主会话能看变更、子会话不能"。
  const blocksWithSubTools = useMemo(() => withAgentToolBlocks(blocks), [blocks])
  const activities = useMemo(() => aggregateFileActivity(blocksWithSubTools, files), [blocksWithSubTools, files])
  const changed = useMemo(() => activities.filter((f) => f.changed), [activities])
  const readOnly = useMemo(() => activities.filter((f) => !f.changed), [activities])
  const visibleGroups = filter === 'changed'
    ? [{ label: t('rp.file.changedFiles'), items: changed }]
    : filter === 'read'
      ? [{ label: t('rp.file.readOnly'), items: readOnly }]
      : [{ label: t('rp.file.changedFiles'), items: changed }, { label: t('rp.file.readOnly'), items: readOnly }]
  const additions = activities.reduce((sum, f) => sum + f.additions, 0)
  const deletions = activities.reduce((sum, f) => sum + f.deletions, 0)

  // 缓存缺失 → 按需补拉（阶段 2）。什么时候会缺：切工作区把缓存清了、被 FILE_PREVIEWS_MAX
  // 淘汰、标签从「最近关闭」重开。没有这条兜底，用户会永久停在「正在读取文件…」（阶段 1
  // 靠「打开即清空 + 立即请求」顺带保证了这一点，阶段 2 内容可以跨标签复用就必须显式补）。
  // 依赖里带 filePreviews：响应写回缓存后 effect 重跑一次（此时已有内容 → 直接返回）。
  useEffect(() => { if (tabPath && !filePreview) ensureFilePreview(tabPath) }, [tabPath, filePreview, filePreviews, ensureFilePreview])

  // 每次打开文件 → Markdown 模式回到默认（预览）。用户决策「不记住」：语义是
  // **每次打开**都回默认，不只是「换到另一个文件」时。（openSeq 每次命中该标签自增，
  // 故重开同一文件也会重置。）
  //
  // 为什么要 lastOpenSeq 这层判断：切标签会让组件重挂载（key={tab.id}），挂载时 effect 同样
  // 会跑一遍 —— 没有它，每次切回来都被复位成默认，「切走再切回还在原视图」就不成立（那是
  // 浏览器式标签的基本预期）。判据是「openSeq 变了」而不是「跑了几次」，故与 StrictMode 的
  // 双调用无关。
  const lastOpenSeq = useRef(tab.openSeq)
  useEffect(() => {
    if (lastOpenSeq.current === tab.openSeq) return
    lastOpenSeq.current = tab.openSeq
    setMdMode('preview')
    setCsvMode('table')
  }, [tab.openSeq])

  // 用户选过的模式按标签记下来（切走再切回时由上面的 lazy init 取回）。写回放在 effect 里，
  // 调用点（两个切换按钮）不必各写一遍。
  useEffect(() => { rememberViewModes(tab.id, { mdMode }) }, [tab.id, mdMode])
  useEffect(() => { rememberViewModes(tab.id, { csvMode }) }, [tab.id, csvMode])


  // isMarkdownFile：path 指向的文件当前是否已加载且为 Markdown（决定是否给切换按钮、
  // 是否走预览渲染）。**必须按「正在渲染的那个路径」逐处判定**，不能用某个全局布尔 ——
  // 「最终文件」有两条渲染路径（活动列表内嵌 item.path / 独立大视图 standalonePath），
  // 用全局布尔正是 2026-09 的漏改原因：只有独立大视图满足条件，用户在活动列表点开
  // 完全没有切换按钮。
  const isMarkdownFile = (p: string) => isMarkdownPath(p) && Boolean(filePreview && sameFilePath(filePreview.path, p, workspace))

  // 同上，CSV 版：判定也按「正在渲染的那个路径」，供两条渲染路径共用。
  const isCsvFile = (p: string) => isCsvPath(p) && Boolean(filePreview && sameFilePath(filePreview.path, p, workspace))

  // 二进制文件的类型化预览入口（见 renderFinalFile ⓪）。
  // 为什么需要它：bridge 对二进制只回 binary=true + 空 content（xlsx/docx/pdf/图片），
  // 文件栏此前只给一句「不适用文本预览」，**没有任何出口** —— 用户点开 .pdf/.xlsx 只能
  // 看到一句话，而产出卡那条路早就有预览按钮。同一个文件，从卡片进能看、从文件栏进看不了。
  // 这里按类型分派到**既有**通道（不新增读文件/渲染机制），与产出卡主按钮同一口径：
  //   pdf/图片/html/svg → openHtmlInBrowser（内嵌浏览器 file://，Chromium 原生渲染）
  //   xlsx/xls/xlsm     → previewOfficeFile(p,'xlsx')（受管 Python + openpyxl 转 HTML）
  //   docx/docm/doc     → previewOfficeFile(p,'docx')（受管 Python + mammoth 转 HTML）
  //   pptx/pptm/ppt     → previewOfficeFile(p,'pptx')（受管 Python + python-pptx 近似版式）
  // 后三条拿到产物路径后走的都是 openHtmlInBrowser —— 与 PDF 同一条内嵌浏览器。
  // 其余二进制（zip/音视频等）**确实没有**应用内预览：只给「用默认程序打开」这条真实出口。
  const renderBinaryActions = (p: string) => {
    const kind = visualKindOf(p)
    // 需要经 bridge 转换的三种格式 → 主按钮文案与动作。表在这里（而不是嵌套三元）：
    // 下面的按钮文案/动作/次按钮判定都要问「这是不是转换类」，一份表比三处条件可靠。
    const convertLabel = kind === 'xlsx' ? 'art.previewXlsx'
      : kind === 'docx' ? 'art.previewDocx'
        : kind === 'pptx' ? 'art.previewPptx' : ''
    // 转换在途反馈（与 ArtifactCard 同一口径）：首次转换可能先 bootstrap 受管运行时，
    // 分钟级 —— 按钮没有可见反馈时，用户会以为没点到而反复点。只标**这一个文件**在途，
    // 同栏其他文件的按钮照常可用（store 侧 busy 是单路径槽，见 previewOfficeFile 注释）。
    const thisBusy = !!kind && officeBusy === p
    return <div className="file-binary-actions">
      {kind && <button className="btn primary" disabled={thisBusy} onClick={() => {
        if (kind === 'xlsx' || kind === 'docx' || kind === 'pptx') void previewOfficeFile(p, kind)
        else void openHtmlInBrowser(p)
      }}>
        {thisBusy ? t('art.converting') : convertLabel ? t(convertLabel) : kind === 'pdf' ? t('art.previewPdf') : t('art.openBrowser')}
      </button>}
      {/* 次按钮保留「用默认程序打开」：内嵌预览看不了批注/图表/条件格式/修订/动画，系统应用是
          必要出口；转换失败（.xls/.doc/.ppt 老格式不支持 / 运行时未就绪）也靠它兜底。
          与产出卡（ArtifactCard.openDefault）同一实现口径 —— openPath 由 electron 侧做路径校验。 */}
      <button className="btn" onClick={() => void openDefault(p)}>{t('art.openDefault')}</button>
    </div>
  }
  // 「用默认程序打开」：与 ArtifactCard.openDefault 同实现（绝对化由 electron 侧兜底校验）。
  const openDefault = async (p: string) => {
    const d = window.desktop
    if (!d?.openPath) { showToast(t('art.unsupported'), 'error'); return }
    const abs = p.startsWith('/') ? p : workspace ? `${workspace}/${p}` : p
    const r = await d.openPath(abs)
    if (!r?.ok) showToast(t('art.openFail').replace('{err}', r?.error || ''), 'error')
  }

  // read_file start_line → 「最终文件」渲染完成后定位滚动到对应行：
  // html 刚更新时 DOM 未提交，用 rAF 双帧等 shiki 结构稳定后再滚。
  // 只对 source 模式（无 toolId，统一 open-file）生效；operation 模式不动。
  // 兼容两种渲染路径：独立文件视图（standalone，file-side-detail）与
  // 文件活动列表内嵌 source（detailMode==='source'，file-inline-detail）。
  // 阶段 2：定位锚来自**本标签**的 payload（tab.startLine / tab.toolId），不再读 fileFocus ——
  // 否则打开 B 时 A 标签（已挂载在别处？）会跟着滚。openSeq 参与依赖：重开同一文件要重新定位。
  // R08a：锚点现在由 text 渲染器挂载（经 lazy loader），**首次打开**时它与 html 谁先落地
  // 没有保证（模块 import 是异步的）→ 双帧等不到就再等，上限 1.5s。原来「DOM 已就绪」
  // 时行为逐字不变（第 2 帧就滚），只是不再依赖「一定在第 2 帧之前就绪」这个巧合。
  useEffect(() => {
    if (!html || tab.startLine == null || tab.toolId) return
    // 可视容器：standalone 优先（file-side-detail 显示中），否则找 detailMode==='source'
    // 的内嵌容器（窄屏 file-inline-detail 显示中）。列表容器 display:none 时
    // querySelector 也会命中 → 用 closest 判断是否可见（offsetParent!==null）。
    // 范围限定在本面板内（panelRef）：同一页面上只挂一个 FilePreviewTab，但把查询收窄
    // 到自己的容器能防住将来「多面板并存」时的误定位。
    const findLine = (): Element | null => {
      const containers = Array.from((panelRef.current ?? document).querySelectorAll<HTMLElement>('.fp-code'))
      const visible = containers.find((c) => c.offsetParent !== null) ?? containers[0]
      if (!visible) return null
      const target = visible.querySelector<HTMLElement>(`.fp-ln[data-line="${tab.startLine}"]`)
      return target?.closest('.line') ?? null
    }
    const deadline = Date.now() + 1500
    let frame = 0
    const tick = () => {
      const lineEl = findLine()
      // 用 scrollIntoView 定位该行到容器中部（block:'center'）；smooth 与整体 UX 一致。
      if (lineEl) { lineEl.scrollIntoView({ block: 'center', behavior: 'smooth' }); return }
      if (Date.now() < deadline) frame = requestAnimationFrame(tick)
    }
    frame = requestAnimationFrame(() => { frame = requestAnimationFrame(tick) })
    return () => cancelAnimationFrame(frame)
  }, [html, tab.openSeq, tab.startLine, tab.toolId])

  // 定位本标签的文件：把文件活动列表选中/展开/高亮到 tab 指向的那一项，并按 mode 落视图模式
  //（diff → 该次操作；source → 最终文件）。触发器 = tab.openSeq（每次打开该标签自增）+
  // payload 三元组；组件因 key={tab.id} 在切标签时重挂载，故激活一个标签也会跑到这里
  //（= 每个标签的列表选中态由**它自己的 payload** 派生，不继承上一个标签）。
  useEffect(() => {
    if (!tab.path) return
    const path = normalizeFilePath(tab.path)
    // source 模式（统一 open-file，无 toolId）：打开最终文件全量视图。
    // 文件若在活动列表 → 选中它 + 切 source 模式（不滚动不高亮，独立文件由 standalone 接管）
    if (tab.mode !== 'diff' || !tab.toolId) {
      setSelectedPath(path)
      setDetailMode('source')
      return
    }
    setFilter('all')
    setExpanded((prev) => new Set(prev).add(path))
    setHighlightPath(path)
    setHighlightTool(tab.toolId)
    setSelectedPath(path)
    setSelectedTool(tab.toolId)
    setDetailMode('operation')
    if (highlightTimer.current) window.clearTimeout(highlightTimer.current)
    let frame2 = 0
    const frame1 = requestAnimationFrame(() => {
      frame2 = requestAnimationFrame(() => rowRefs.current.get(path)?.scrollIntoView({ block: 'center', behavior: 'smooth' }))
    })
    highlightTimer.current = window.setTimeout(() => { setHighlightPath(''); setHighlightTool('') }, 1400)
    return () => { cancelAnimationFrame(frame1); cancelAnimationFrame(frame2) }
  }, [tab.openSeq, tabPath, tab.mode, tab.toolId])

  useEffect(() => () => {
    if (highlightTimer.current) window.clearTimeout(highlightTimer.current)
  }, [])

  // 滚动位置：① 监听本面板内所有滚动容器的 scroll（capture，因为 scroll 不冒泡）→ 记到
  // fileTabScroll[tab.id]；② 内容就绪后把上次的位置放回去（重挂载后 DOM 是新的）。
  // 为什么放在内容之后恢复：html 为空时容器还很矮，直接写 scrollTop 会被浏览器夹到 0。
  useEffect(() => {
    const root = panelRef.current
    if (!root) return
    const onScroll = (e: Event) => {
      const el = e.target as HTMLElement | null
      if (!el || el === root) return
      const role = scrollRoleOf(el)
      if (!role) return
      const snap = { ...(fileTabScroll.get(tab.id) ?? {}) }
      if (el.scrollTop > 0) snap[role] = el.scrollTop
      else delete snap[role]
      // 先删再插 → 最近用过的标签排在末尾；超出上限淘汰最久没动过的（防无界增长）
      fileTabScroll.delete(tab.id)
      fileTabScroll.set(tab.id, snap)
      while (fileTabScroll.size > FILE_TAB_SCROLL_MAX) {
        const oldest = fileTabScroll.keys().next().value
        if (oldest === undefined) break
        fileTabScroll.delete(oldest)
      }
    }
    root.addEventListener('scroll', onScroll, true)
    return () => root.removeEventListener('scroll', onScroll, true)
  }, [tab.id])

  // 滚动位置恢复（R08a 起要**等渲染器挂载**）：内容容器由 surfaces/preview 的 lazy loader
  // 挂载，它与 html 不是同一次提交 —— 「元素还没出现/还没有内容」时不能当成「已经恢复到 0」，
  // 否则切回标签会丢掉滚动位置（实测：e2e:md-file-modes 的「切回来仍在原视图」变红）。
  // 判据是「元素在且已经有子节点」（.fp-code 的内容来自 shiki，.md-preview-doc 来自 Markdown），
  // 找不到就按帧重试，上限 1.5s（与 startLine 定位同一套做法）。
  useEffect(() => {
    const root = panelRef.current
    const snap = fileTabScroll.get(tab.id)
    if (!root || !snap) return
    // 有 read_file 定位锚时不恢复：那一次打开的意图是「滚到那一行」（见上面 startLine 的 effect）
    if (tab.startLine != null && tab.mode !== 'diff') return
    const pending = new Set(Object.keys(snap))
    const apply = (): boolean => {
      for (const [role, sel] of SCROLL_ROLES) {
        if (!pending.has(role)) continue
        const want = snap[role]
        if (want == null) { pending.delete(role); continue }
        const el = root.querySelector<HTMLElement>(sel)
        if (!el || (want > 0 && el.childElementCount === 0)) continue
        el.scrollTop = want
        pending.delete(role)
      }
      return pending.size === 0
    }
    if (apply()) return
    const deadline = Date.now() + 1500
    let frame = 0
    const tick = () => {
      if (apply()) return
      if (Date.now() < deadline) frame = requestAnimationFrame(tick)
    }
    frame = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(frame)
  }, [tab.id, tab.startLine, tab.mode, html, filePreview?.content])

  const toggleFile = (item: FileActivity) => {
    const opening = !expanded.has(item.path)
    setExpanded((prev) => {
      const next = new Set(prev)
      opening ? next.add(item.path) : next.delete(item.path)
      return next
    })
    setSelectedPath(item.path)
    if (opening) {
      const latestChange = [...item.operations].reverse().find((op) => op.tool !== 'read_file' && op.diff?.unified)
      setSelectedTool(latestChange?.toolId || '')
      setDetailMode(latestChange ? 'operation' : 'source')
      openFile(item.path)
    }
  }

  const selectOperation = (item: FileActivity, operation: FileOperation) => {
    setSelectedPath(item.path)
    setSelectedTool(operation.toolId)
    setDetailMode('operation')
  }

  // 文件活动列表内嵌的「最终文件」（detailMode==='source'）：与独立大视图共用同一
  // 份双模式渲染（Markdown 预览/纯文本、CSV 表格/纯文本）—— 两处若各写一遍，正是
  // 「改了一处漏另一处」的温床（2026-09 实测踩过：只在独立大视图加了切换按钮，
  // 用户在活动列表点开完全没有）。
  // matchPath 为目标文件的路径（列表内嵌传 item.path，独立大视图传 standalonePath）。
  const renderFinalFile = (matchPath: string) => {
    if (!(filePreview && sameFilePath(filePreview.path, matchPath, workspace))) {
      return <div className="file-activity-empty">{t('rp.file.reading')}</div>
    }
    return <div className="file-source">
      <div className="file-source-meta mono">
        {filePreview.binary
          ? '' // 二进制没有行数概念（bridge 返回 lines=0），显示 "0 行" 只会让人困惑
          : t('rp.file.lines').replace('{n}', String(filePreview.lines)) + (filePreview.truncated ? t('rp.file.truncated') : '')}
      </div>
      {/* 四条渲染路径（择一），都复用既有能力，不新增读文件通道：
          ⓪ binary（bridge 判定：xlsx/docx/pdf/图片等，content 为空）→ 类型化预览入口。
             为什么必须单独挡：这条入口是**文件树/工具行**的 open-file，产出卡那条路有 xlsx/pdf
             专门分支，而这里此前没有——二进制会被当字符串喂给 shiki，结果是大段乱码 + 白耗
             渲染（渲染成本随内容线性增长）。
             binary=true 时**没有文本内容可展示**，故此处给的是「用哪条既有通道能真的看到内容」
             （pdf/图片/html/svg → 内嵌浏览器；xlsx/docx/pptx → bridge 转 HTML 后同一条浏览器；
             见 renderBinaryActions），而不是一句指不到按钮的提示。zip 等确实无应用内预览的
             格式 → 只留「用默认程序打开」这条真实出口，文案如实说明（rp.file.binaryNoPreview）。
          ① CSV/TSV 且表格模式 → csv 渲染器（R08a 起经注册表挂载；渲染仍是 CsvPreview：
             内容 → lib/csv.ts 转 GFM 表格 → 同一套 Markdown 管线）；内容来自上面这条
             file_preview 流程，**不在前端另读文件**（读文件的 workspace 越界校验与
             1 MiB 上限在 bridge 侧，绕过它等于开洞）。
          ② Markdown 且预览模式 → 对话区 Markdown 组件（GFM/表格/mermaid/KaTeX）。**本轮没动**。
          ③ 其余（含 ①② 的纯文本模式）→ text 渲染器（R08a 起经注册表挂载，DOM 仍是
             .fp-code + shiki 高亮源码，既有行为不变）。
          html（shiki 结果）对 CSV 也照常算着——切「纯文本」时立刻可用，省一次切换等待。
          ①②③ 的注册表分支都带 only：文件面板的「表格/纯文本」是 UI 决策，不能让
          weight 更高的渲染器把它抢走（见 registry.pickRenderer 的 only 说明）。 */}
      {filePreview.binary
        ? (visualKindOf(matchPath)
          // 可视类：内容已由上面的自动打开送进预览通道 —— 这里只留一行状态，**不再有提示段落
          // 与主按钮**（用户口径：那是多余的中间态）。文件动作（用默认程序打开／复制路径／
          // 在访达中显示）移到预览标签的右键菜单里（见 BrowserTab），需要时才出现。
          ? <div className="file-binary-status">
            <span className="file-binary-ok" aria-hidden="true">✓</span>
            {officeBusy === matchPath ? t('art.converting') : t('rp.file.previewOpened')}
          </div>
          // 无应用内预览（zip/音视频等）：保留「用默认程序打开」这条真实出口，文案如实说明
          //（不把「预览」写进没有预览的格式里 —— 那正是当初要修的旧文案缺陷）。
          : <div className="file-binary-hint">
            <div className="file-activity-empty">{t('rp.file.binaryNoPreview')}</div>
            {renderBinaryActions(matchPath)}
          </div>)
        : isCsvFile(matchPath) && csvMode === 'table'
          ? <PreviewHost file={fileRefOf(filePreview.path, filePreview.content.length)} content={filePreview.content} only={['csv']} />
          : isMarkdownFile(matchPath) && mdMode === 'preview'
            ? <div className="md-preview-doc"><Markdown text={filePreview.content} baseDir={baseDirFor(filePreview.path, workspace)} /></div>
            : <PreviewHost file={fileRefOf(filePreview.path, filePreview.content.length)} content={filePreview.content} html={html} only={['text']} />}
    </div>
  }

  const renderDetail = (item: FileActivity, compact = false) => {
    const operation = item.operations.find((op) => op.toolId === selectedTool)
    const fileDiff = item.git?.diff || [...item.operations].reverse().find((op) => op.diff?.unified)?.diff?.unified
    return <div className={`file-inspector ${compact ? 'compact' : ''}`}>
      <div className="file-inspector-head">
        <span className="file-inspector-path mono" title={item.path}>{item.path}</span>
        {operation && detailMode === 'operation' && <span className="file-inspector-operation mono">
          {operation.tool} · {formatToolTimestamp(operation.time, t)}
          {operation.diff
            ? <span className="op-stats"> · <b className="add">+{operation.diff.added}</b> <b className="del">−{operation.diff.removed}</b></span>
            : ' · +0 −0'}
        </span>}
        <div className="file-detail-tabs">
          {/* Markdown 双模式切换：见 renderFinalFile —— 「最终文件」在两个分支
              （本处内嵌 / 独立大视图）都要能切，故两处共用同一份状态与判定。 */}
          {isMarkdownFile(item.path) && <span className="md-view-switch" role="group" aria-label={t('rp.file.mdTabsAria')}>
            <button className={mdMode === 'preview' ? 'on' : ''} onClick={() => setMdMode('preview')} aria-pressed={mdMode === 'preview'}>{t('rp.file.mdPreview')}</button>
            <button className={mdMode === 'plain' ? 'on' : ''} onClick={() => setMdMode('plain')} aria-pressed={mdMode === 'plain'}>{t('rp.file.mdSource')}</button>
          </span>}
          {/* CSV/TSV 同款双模式（表格 / 纯文本）：与 Markdown 那条并列、互斥出现
              （一个文件不可能既是 .md 又是 .csv）。复用 .md-view-switch 的样式 ——
              它是「内容形态切换器」的既有样式，改名会牵连 e2e 选择器且毫无收益。 */}
          {isCsvFile(item.path) && <span className="md-view-switch" role="group" aria-label={t('rp.file.csvTabsAria')}>
            <button className={csvMode === 'table' ? 'on' : ''} onClick={() => setCsvMode('table')} aria-pressed={csvMode === 'table'}>{t('rp.file.csvPreview')}</button>
            <button className={csvMode === 'plain' ? 'on' : ''} onClick={() => setCsvMode('plain')} aria-pressed={csvMode === 'plain'}>{t('rp.file.csvSource')}</button>
          </span>}
          <button className={detailMode === 'operation' ? 'on' : ''} onClick={() => setDetailMode('operation')}>{t('rp.file.thisChange')}</button>
          <button className={detailMode === 'file' ? 'on' : ''} onClick={() => setDetailMode('file')}>{t('rp.file.fileChange')}</button>
          <button className={detailMode === 'source' ? 'on' : ''} onClick={() => { setDetailMode('source'); openFile(item.path) }}>{t('rp.file.finalFile')}</button>
        </div>
      </div>
      <div className="file-inspector-body">
        {detailMode === 'operation' && <OperationDiff operation={operation} />}
        {detailMode === 'file' && (fileDiff ? <UnifiedDiff unified={fileDiff} /> : <div className="file-activity-empty">{t('rp.file.noFileDiff')}</div>)}
        {detailMode === 'source' && renderFinalFile(item.path)}
      </div>
    </div>
  }

  const renderItem = (item: FileActivity) => {
    const isOpen = expanded.has(item.path)
    const changeOps = item.operations.filter((op) => op.tool !== 'read_file')
    const failedOps = changeOps.filter((op) => op.isError)
    const shownOps = changeOps.filter((op) => !op.isError)
    const readCount = item.operations.filter((op) => op.tool === 'read_file').length
    return <div
      className={`file-activity-item status-${item.status} ${isOpen ? 'is-open' : ''} ${highlightPath === item.path ? 'is-target' : ''}`}
      key={item.path}
      ref={(node) => { if (node) rowRefs.current.set(item.path, node); else rowRefs.current.delete(item.path) }}
    >
      <button className="file-activity-row" onClick={() => toggleFile(item)} aria-expanded={isOpen} aria-current={highlightPath === item.path ? 'true' : undefined}>
        <span className="file-activity-chevron">{isOpen ? '⌄' : '›'}</span>
        <span className="file-activity-status mono" title={item.status}>{fileStatusLabel(item.status)}</span>
        <span className="file-activity-path mono" title={item.path}>{item.path}</span>
        {item.changed
          ? <span className="file-activity-count mono">{t('rp.file.changedCount').replace('{n}', String(shownOps.length))}{failedOps.length > 0 ? t('rp.file.failedCount').replace('{n}', String(failedOps.length)) : ''}</span>
          : readCount > 1 && <span className="file-activity-count mono">{t('rp.file.readCount').replace('{n}', String(readCount))}</span>}
        <span className="grow" />
        <span className="file-activity-stat mono">
          {item.additions > 0 && <b className="add">+{item.additions}</b>}
          {item.deletions > 0 && <b className="del">−{item.deletions}</b>}
          {!item.changed && <span>{t('rp.file.read')}</span>}
        </span>
      </button>
      {isOpen && <div className="file-activity-detail">
        <div className="file-operation-list">
          {shownOps.map((op) => {
            const location = operationLocation(op)
            const noChange = !op.diff || (op.diff.added === 0 && op.diff.removed === 0)
            return <button
              className={`file-operation is-change ${noChange ? 'no-change' : ''} ${selectedTool === op.toolId ? 'is-selected' : ''} ${highlightTool === op.toolId ? 'is-target' : ''}`}
              key={op.toolId}
              onClick={() => selectOperation(item, op)}
              aria-pressed={selectedTool === op.toolId}
            >
              <span className="operation-status">✓</span>
              <span className="operation-chevron">{selectedTool === op.toolId && detailMode === 'operation' ? '⌄' : '›'}</span>
              <span className="operation-name mono">{op.tool}</span>
              <span className="operation-location mono" title={location || (noChange ? t('rp.file.noNetChange') : undefined)}>{location || (noChange ? t('rp.file.noNetChange') : '')}</span>
              <span className="operation-called-at mono" title={formatToolTimestamp(op.time, t)}>{formatToolTimestamp(op.time, t)}</span>
              {op.diff && !noChange && <span className="operation-stat mono"><b className="add">+{op.diff.added}</b><b className="del">−{op.diff.removed}</b></span>}
              {op.durMs != null && <span className="operation-time mono">{fmtDuration(op.durMs)}</span>}
            </button>
          })}
          {failedOps.length > 0 && <details className="failed-operation-group">
            <summary><span className="operation-status error">✗</span><span>{t('rp.file.failedCalls').replace('{n}', String(failedOps.length))}</span></summary>
            {failedOps.map((op) => <div className="file-operation failed-operation" key={op.toolId}>
              <span className="operation-name mono">{op.tool}</span>
              <span className="operation-called-at mono" title={formatToolTimestamp(op.time, t)}>{formatToolTimestamp(op.time, t)}</span>
              {op.durMs != null && <span className="operation-time mono">{fmtDuration(op.durMs)}</span>}
            </div>)}
          </details>}
          {changeOps.length === 0 && <div className="file-activity-empty">{t('rp.file.gitChanges')}</div>}
        </div>
        {selectedPath === item.path && <div className="file-inline-detail">{renderDetail(item, true)}</div>}
      </div>}
    </div>
  }

  const selectedActivity = activities.find((item) => item.path === selectedPath)

  // 独立文件完整视图（统一 open-file 能力）：**本标签**指向的文件不在活动列表
  //（工作区外绝对路径 / 尚未在会话里操作过的文件）→ 直接渲染全量内容（source 模式）。
  // 阶段 2：判据从全局 fileFocus 换成标签 payload —— mode='source' 且带 path 才是完整预览
  //（mode 缺省 = closeFile 清过视图，回落文件活动列表；此时保留 path 是为了标签标题不变）。
  const standalonePath = tab.mode === 'source' && tabPath ? normalizeFilePath(tabPath) : undefined
  // 同一性比较用 sameFilePath（容忍绝对 vs 工作区相对）——见其注释的实测缺陷说明
  const standalone = standalonePath && (!selectedActivity || !sameFilePath(selectedActivity.path, standalonePath, workspace))
    ? { path: standalonePath }
    : undefined

  // 注：「打开即见内容」（可视类文件点开即自动进预览）挂在 **store.openFile**（用户点击入口），
  // 不在这里做 —— 组件会在切走时卸载，去重状态随之丢失，会导致「切回文件栏又被弹回预览」的拉锯
  //（第一版实现实测：4ms 内 rightTab 从 file 被顶回 web）。本组件只负责把结果显示出来。

  // 整文件（standalone 全文，或列表里选中文件的「最终文件」）→ **独占整宽**：不渲染左侧活动列表
  // 与分隔条。用户口径（2026-09）：「完整视图就是整个文件，就不需要左侧的变更记录了，而查看变更
  // 的时候才会展示变更列表，否则会导致文件预览不全」。展示**变更**（operation / file）时保持两栏。
  // 两条渲染路径都算整文件：文件不在活动列表 → standalone；在列表里但切到「最终文件」→ detailMode。
  const wholeFile = Boolean(standalone || (selectedActivity && detailMode === 'source'))
  // 双列 = 列表 + 右侧详情/全文（选中文件或 standalone 全文都算，缺一不可否则 detail 无列可放）
  const splitVisible = Boolean(selectedActivity || standalone) && !wholeFile
  // 用户拖过分隔条 → px 宽度覆盖默认 44%；双击分隔条复位（masterW=null）
  const panelStyle = masterW != null && splitVisible ? { gridTemplateColumns: `${masterW}px 9px minmax(0, 1fr)` } : undefined
  const onSplitterDown = (e: React.PointerEvent<HTMLDivElement>) => {
    if (e.button !== 0 || !panelRef.current) return
    e.preventDefault()
    const master = panelRef.current.querySelector<HTMLElement>('.file-activity-master')
    if (!master) return
    e.currentTarget.setPointerCapture(e.pointerId)
    masterWStart.current = {
      startX: e.clientX,
      startW: master.getBoundingClientRect().width,
      maxW: panelRef.current.getBoundingClientRect().width - FILE_DETAIL_MIN,
    }
  }
  const onSplitterMove = (e: React.PointerEvent<HTMLDivElement>) => {
    const d = masterWStart.current
    if (!d) return
    const w = Math.max(FILE_MASTER_MIN, Math.min(d.maxW, d.startW + e.clientX - d.startX))
    setMasterW(Math.round(w))
  }
  const endSplit = () => { masterWStart.current = null }

  return <div
    className={`file-activity-panel ${splitVisible ? 'has-selection' : ''} ${wholeFile ? 'whole-file' : ''}`}
    style={panelStyle}
    ref={panelRef}
  >
    {/* 整文件态整块不渲染（不只是隐藏）：工具条/筛选/空态都在里面，留着只会白占宽度 */}
    {!wholeFile && <div className="file-activity-master">
      <div className="file-activity-toolbar">
        <div className="file-activity-summary">
          <div className="file-summary-main"><strong>{activities.length}</strong><span>{t('rp.file.files')}</span><span className="file-summary-split">·</span><span>{changed.length} {t('rp.file.changed')}</span><span>{readOnly.length} {t('rp.file.readOnly2')}</span></div>
          <div className="file-summary-stat mono"><b className="add">+{additions}</b><b className="del">−{deletions}</b></div>
        </div>
        {activities.length > 0 && <div className="file-filter" role="group" aria-label={t('rp.file.filterAria')}>
          {([['all', t('rp.file.all')], ['changed', t('rp.file.changed')], ['read', t('rp.file.readOnly2')]] as const).map(([key, label]) => <button key={key} className={filter === key ? 'on' : ''} onClick={() => setFilter(key)} aria-pressed={filter === key}>{label}</button>)}
        </div>}
      </div>
      {visibleGroups.map((group) => group.items.length > 0 && <section className="file-activity-group" key={group.label}>
        <div className="file-activity-group-title">{group.label}<span className="mono">{group.items.length}</span></div>
        <div className="file-activity-list">{group.items.map(renderItem)}</div>
      </section>)}
      {activities.length === 0 && !standalone && <div className="empty"><div className="big">{t('rp.file.noActivity')}</div>{t('rp.file.noActivityHint')}</div>}
    </div>}
    {splitVisible && (
      <div
        className="file-splitter"
        role="separator"
        aria-orientation="vertical"
        title={t('rp.file.dragSplit')}
        onPointerDown={onSplitterDown}
        onPointerMove={onSplitterMove}
        onPointerUp={endSplit}
        onPointerCancel={endSplit}
        onDoubleClick={() => setMasterW(null)}
      />
    )}
    {standalone && (
      <div className="file-side-detail">
        <div className="file-inspector">
          <div className="file-inspector-head">
            <span className="file-inspector-path mono" title={standalone.path}>{standalone.path}</span>
            <div className="file-detail-tabs">
              {isMarkdownFile(standalone.path) && <span className="md-view-switch" role="group" aria-label={t('rp.file.mdTabsAria')}>
                <button className={mdMode === 'preview' ? 'on' : ''} onClick={() => setMdMode('preview')} aria-pressed={mdMode === 'preview'}>{t('rp.file.mdPreview')}</button>
                <button className={mdMode === 'plain' ? 'on' : ''} onClick={() => setMdMode('plain')} aria-pressed={mdMode === 'plain'}>{t('rp.file.mdSource')}</button>
              </span>}
              {/* CSV/TSV 表格切换：与上面 Markdown 那条并列（见 renderDetail 处的说明）。
                  独立大视图这一支必须**独立存在** —— 2026-09 的漏改正是「只在其中一条
                  渲染路径加了切换按钮」，用户从产出卡片进来（走本分支）永远切不了。 */}
              {isCsvFile(standalone.path) && <span className="md-view-switch" role="group" aria-label={t('rp.file.csvTabsAria')}>
                <button className={csvMode === 'table' ? 'on' : ''} onClick={() => setCsvMode('table')} aria-pressed={csvMode === 'table'}>{t('rp.file.csvPreview')}</button>
                <button className={csvMode === 'plain' ? 'on' : ''} onClick={() => setCsvMode('plain')} aria-pressed={csvMode === 'plain'}>{t('rp.file.csvSource')}</button>
              </span>}
              <button className="on">{t('rp.file.finalFile')}</button>
              {/* 「编辑」只在独立大视图给（分栏内嵌那份是列表里的活动详情，语义是「看」）。
                  只读判定刻意留给**编辑器自己**：binary/截断由 file_open 说了算，
                  这里按扩展名先筛一道只是省一次必然失败的打开。 */}
              {editorOn && isTextLikePath(standalone.path) && (
                <button className="file-edit-entry" onClick={() => openFileForEdit(standalone.path)} title={t('editor.editHint')}>
                  {t('editor.edit')}
                </button>
              )}
              <button className="file-detail-close" onClick={() => closeFile()} title={t('rp.file.closeView')} aria-label={t('rp.file.closeView')}>✕</button>
            </div>
          </div>
          <div className="file-inspector-body">
            {/* 与文件活动列表内嵌视图共用 renderFinalFile（含 Markdown 双模式 +
                加载中占位），避免两处实现漂移。 */}
            {standalone ? renderFinalFile(standalone.path) : null}
          </div>
        </div>
      </div>
    )}
    {selectedActivity && !standalone && <div className="file-side-detail">{renderDetail(selectedActivity)}</div>}
  </div>
}
// M5/M6：内嵌视图状态类型与 AI 动作描述已上移至 store（全局订阅，tab 未开也更新）
// 浏览器面板（右栏 web tab）：插件私有浏览器连接的状态 + 页列表 + 当前页截图。// 浏览器面板（右栏 web tab）：插件私有浏览器连接的状态 + 页列表 + 当前页截图。
// 数据经 bridge browser_* 命令按需拉取（截图重内容不推送事件通道）；页面切换调截图。
// 沉浸态（immersive）：收起**浏览器自己的工具条**，把垂直空间让给内容；☰ 唤回。
// 注意与 M6 原语义的差别：M6 收的是**面板级 tab 行**，而阶段 1 起面板级标签条是常驻的
//（它是唯一的标签入口，收起它就等于切不走标签）→ 这里只收浏览器自己的 chrome。
// —— R4：预览标签条的展示口径（原型 docs/demo/right-panel-preview.html 的 .pv-tab）——
//
// 「标签上该写什么」此前是 `title || url`，PDF 恰好暴露了它的缺陷：Chromium 的 PDF 页
// title 常常是空的 → 退化成整条百分号编码的长 URL（`file:///Users/…/%E4%B8%AD%E6%96%87…`），
// 用户完全认不出来是哪个文件。改为按 URL 形态分派（见 tabLabel）。


