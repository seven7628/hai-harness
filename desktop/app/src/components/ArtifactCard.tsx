import { memo, useCallback, useMemo, useState } from 'react'
import { useAppStore, type ArtifactItem } from '../store/useAppStore'
import { copyText } from '../lib/clipboard'
import { isCsvPath } from '../lib/csv'
import { BROWSER_EXTS, PDF_EXTS, XLSX_EXTS, DOCX_EXTS, PPTX_EXTS, extOf } from '../lib/visualFile'
import { fileTypeOf } from '../lib/palette'
import { useT } from '../i18n'

// ArtifactCard 本轮产出汇总卡片（AgentEnd.Artifacts）——样式选型 C：
//   hero（首个产出：大图标磁贴 + 大字文件名 + 常显主按钮）
//   + 紧凑列表（其余产出，悬停浮现动作）
//   + 折叠（默认显示 4 条，超出折叠）
//   + 底部汇总（总数 / 目录数 / 批量「全部在 Finder 中显示」）
//
// 数据边界：Artifact 只带路径与统计（name/path/kind/±/size/lines/sha256），
// **不含文件内容**——预览/打开都经既有能力（file_preview 命令 / 内嵌浏览器 /
// 系统默认程序），不在卡片里渲染文件正文。
//
// 与工具行的分工：工具行展示「这一调用改了什么」（含 unified diff）；
// 本卡片展示「本轮产出了什么」（汇总、可操作）。同一信息不做两处入口。

const MAX_ROWS = 4 // 紧凑列表默认显示条数（其余折叠）

// 可预览类型（点击「预览」走 openFile → 右侧栏 FilePreviewTab）。
const TEXT_EXTS = new Set([
  'md', 'markdown', 'txt', 'json', 'jsonc', 'yaml', 'yml', 'toml', 'csv', 'tsv', 'log',
  'go', 'ts', 'tsx', 'js', 'jsx', 'mjs', 'cjs', 'py', 'rb', 'rs', 'java', 'kt', 'swift',
  'c', 'h', 'cc', 'cpp', 'hpp', 'cs', 'php', 'sh', 'bash', 'zsh', 'sql', 'xml', 'css',
  'scss', 'less', 'html', 'htm', 'svg', 'vue', 'svelte', 'ini', 'conf', 'env',
])
// 浏览器 / PDF / Excel 的扩展名集合与判定已抽到 lib/visualFile.ts（单一事实源）：
// 文件栏的预览入口与 store 的「产出可视文件后自动打开」必须与这里**同一口径**，
// 三份各写一遍必然漂移（见该文件注释）。

// Office 三格式的主按钮文案 key（i18n 的中英两份都在 src/i18n.ts）。
// 表格/文档/幻灯片各一个词：用户点之前就该知道会看到什么形态。
const OFFICE_PREVIEW_LABELS: Record<string, string> = {
  xlsx: 'art.previewXlsx',
  xlsm: 'art.previewXlsx',
  xls: 'art.previewXlsx',
  docx: 'art.previewDocx',
  docm: 'art.previewDocx',
  doc: 'art.previewDocx',
  pptx: 'art.previewPptx',
  pptm: 'art.previewPptx',
  ppt: 'art.previewPptx',
}

// 展示路径：与 openHtmlInBrowser 同款约定（以 / 开头即绝对；否则锚定工作区）。
function absPathOf(p: string, ws: string | undefined): string {
  if (p.startsWith('/')) return p
  return ws ? `${ws}/${p}` : p
}

// 人类可读体积（对齐 FileRefPanel.fmtSize 口径）。
function fmtSize(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / (1024 * 1024)).toFixed(1)} MB`
}

// 类型图标：按扩展名分几类（浏览器类 / PDF / 表格类 / 图片类 / 其他），无外部图标依赖。
function TypeGlyph({ ext, size = 15 }: { ext: string; size?: number }) {
  const common = { width: size, height: size, viewBox: '0 0 16 16', fill: 'none', stroke: 'currentColor', strokeWidth: 1.4, strokeLinecap: 'round' as const, strokeLinejoin: 'round' as const }
  if (BROWSER_EXTS.has(ext)) {
    return (
      <svg {...common}>
        <rect x="1.5" y="2.5" width="13" height="11" rx="1.5" />
        <path d="M1.5 5.8h13" />
        <circle cx="3.6" cy="4.15" r=".55" fill="currentColor" stroke="none" />
        <circle cx="5.5" cy="4.15" r=".55" fill="currentColor" stroke="none" />
        <path d="m6.2 10.6 1.5-1.9 1.3 1.4 1-1.2 1.3 1.7" />
      </svg>
    )
  }
  if (PDF_EXTS.has(ext)) {
    // 折角文档（与 default 的「带文本行文档」外形同源，靠折角 + 单条短线区分）：
    // 不画 PDF 字样 —— 15px 下不可辨，反而糊成一团。
    return (
      <svg {...common}>
        <path d="M9.1 1.8H4.2a1.5 1.5 0 0 0-1.5 1.5v9.4a1.5 1.5 0 0 0 1.5 1.5h7.6a1.5 1.5 0 0 0 1.5-1.5V6.1z" />
        <path d="M9.1 1.8v4.3h4.2" />
        <path d="M5.6 11.4h4.8" />
      </svg>
    )
  }
  if (ext === 'xlsx' || ext === 'xls' || ext === 'csv' || ext === 'tsv') {
    return (
      <svg {...common}>
        <rect x="2.5" y="1.8" width="11" height="12.4" rx="1.5" />
        <path d="M2.5 5.6h11M2.5 9h11M6.1 5.6v8.6" />
      </svg>
    )
  }
  if (ext === 'png' || ext === 'jpg' || ext === 'jpeg' || ext === 'gif' || ext === 'webp' || ext === 'bmp') {
    return (
      <svg {...common}>
        <rect x="1.8" y="2.5" width="12.4" height="11" rx="1.5" />
        <circle cx="5.4" cy="6" r="1.2" />
        <path d="m2 11.5 3.4-3 2.6 2.3 2.2-1.9 3.6 3.2" />
      </svg>
    )
  }
  return (
    <svg {...common}>
      <rect x="2.5" y="1.8" width="11" height="12.4" rx="1.5" />
      <path d="M5.4 5.4h5.2M5.4 8h5.2M5.4 10.6h3.2" />
    </svg>
  )
}

// 单个产出的动作组（按扩展名分派主按钮）。
function ArtifactActions({ a, abs, compact = false }: { a: ArtifactItem; abs: string; compact?: boolean }) {
  const t = useT()
  const openFile = useAppStore((s) => s.openFile)
  const openHtmlInBrowser = useAppStore((s) => s.openHtmlInBrowser)
  const showToast = useAppStore((s) => s.showToast)
  // 转换在途（store.previewOfficeFile 置位；首次调用可能先 bootstrap 受管运行时，
  // 分钟级）→ 主按钮改为「转换中…」并禁用，给用户一个「点到了，在算」的可见反馈。
  // 三种格式共用这一个槽位（store 侧的理由见 officeBusy 注释）；只在**这一个文件**在途时置位，
  // 其他卡片的按钮照常可用。
  const officeBusy = useAppStore((s) => s.officeBusy)
  const ext = extOf(a.name)

  // 「用默认程序打开」：默认类型的主按钮 + PDF 的次按钮共用同一实现（useCallback
  // 是为了让下面的 useMemo 依赖稳定；内联两份会在两条路径上各自演化）。
  // PDF 用得上它：内嵌 viewer 只能看，Skim/Preview 的批注、导出等外部能力给不了，
  // 所以主按钮改成预览后系统打开必须仍可达。
  const openDefault = useCallback(async () => {
    const d = window.desktop
    if (!d?.openPath) {
      showToast(t('art.unsupported'), 'error')
      return
    }
    const r = await d.openPath(abs)
    if (!r?.ok) showToast(t('art.openFail').replace('{err}', r?.error || ''), 'error')
  }, [abs, showToast, t])

  // 需要经 bridge 转换的三种格式 → 各自的 store 动作（都是 previewOfficeFile 的薄包装）。
  // 表放在这里（而不是在 useMemo 里逐层 if）：下面主按钮分支与在途态判定都要用同一份映射，
  // 写两处必然漂移 —— 那正是「按钮说预览表格、实际走 docx 转换」这类静默错配的来源。
  const previewByExt = useMemo(() => new Map<string, () => void>([
    ...[...XLSX_EXTS].map((e) => [e, () => void useAppStore.getState().previewXlsx(a.path)] as [string, () => void]),
    ...[...DOCX_EXTS].map((e) => [e, () => void useAppStore.getState().previewDocx(a.path)] as [string, () => void]),
    ...[...PPTX_EXTS].map((e) => [e, () => void useAppStore.getState().previewPptx(a.path)] as [string, () => void]),
  ]), [a.path])

  const primary = useMemo(() => {
    if (BROWSER_EXTS.has(ext)) {
      return {
        label: t('art.openBrowser'),
        run: () => void openHtmlInBrowser(a.path),
      }
    }
    if (PDF_EXTS.has(ext)) {
      return {
        label: t('art.previewPdf'),
        // 渲染通道与 html/svg 完全同一套（openHtmlInBrowser = file:// + focusBrowser +
        // embedShow + embedNavigate）：Electron 的 WebContentsView 自带 Chromium PDF
        // viewer，实测能直接渲染（capturePage 像素非空），**不需要 pdf.js**。
        // 虽名为 openHtmlInBrowser，其实质是「在内嵌浏览器里打开本地文件」，故复用而非
        // 新加一个语义重复的 store action（否则两条路径会各自演化、修一处漏一处）。
        //
        // 失败提示不在这里 try/catch：openHtmlInBrowser 内部已包住 embedShow /
        // embedNavigate 并把错误 toast 出来（文件不存在时 file:// 会导航失败），
        // 此处再兜一层只会弹两次。
        run: () => void openHtmlInBrowser(a.path),
      }
    }
    if (previewByExt.has(ext) && OFFICE_PREVIEW_LABELS[ext]) {
      // Office 三种格式（xlsx/docx/pptx）：**不**走 openFile（那是文本预览，二进制只会显示
      // 乱码），而是「bridge 转 HTML → 内嵌浏览器」（store.previewOfficeFile，见其注释）。
      // 与 PDF 的分工一致：主按钮 = 预览，次按钮仍保留「用默认程序打开」（下方按钮区按
      // OFFICE_EXTS 补一个）—— 内嵌预览看不了图表/条件格式/修订/动画，Excel/Word/PPT 仍是
      // 必要出口；且预览转换失败时（如 .xls/.doc/.ppt 老格式不支持、运行时未就绪）用户有别的路。
      //
      // 传 a.path（相对/绝对原样，与 CSV/文本分支同约定），由 store 连同 workspace
      // 一起交给 bridge —— 路径解析（工作区锚定 + ~/.go-code 保护）在 bridge 侧统一做，
      // 卡片不自己拼绝对路径（拼了反而会绕过越界校验）。
      //
      // 文案按格式分开：用户在点之前就该知道「点了看到的是表格/文档/幻灯片」。
      // pptx 的文案额外点明「近似」—— 那是对这条通路保真度的如实交代（做不到渲染，
      // 见 builtin.PptxPreviewer），藏在页面里不如写在按钮上。
      return {
        label: t(OFFICE_PREVIEW_LABELS[ext]),
        run: previewByExt.get(ext)!,
      }
    }
    if (isCsvPath(a.name)) {
      // CSV/TSV 单独一支（在 TEXT_EXTS 之前接住）：它虽然也是文本，但预览形态是**表格**
      // 而非源码 —— 右栏「最终文件」对 .csv/.tsv 默认渲染 CsvPreview（内容 → GFM 表格），
      // 纯文本只作兜底（切「纯文本」可看原始内容）。故这里给专门的按钮文案，
      // 让用户在点之前就知道「点了是看表格」。
      //
      // 走的路由与下面文本类**完全相同**（openFile + mode='source'）：表格渲染发生在
      // 右栏的渲染分支里，不在这里 —— 卡片拿不到文件内容（Artifact 只带路径与统计），
      // 也不该自己去读文件（读文件的越界校验/1 MiB 上限在 bridge 侧）。
      // 唯一的分支理由是文案；为什么不必传 mode/内容：见下条注释的路径约定。
      return {
        label: t('art.previewTable'),
        run: () => openFile(a.path, undefined, 'source'),
      }
    }
    if (TEXT_EXTS.has(ext)) {
      return {
        label: t('art.preview'),
        // mode='source' = 统一 open-file 完整内容视图（不落 operation/diff 模式）。
        //
        // 必须传 a.path（Harness 已归一化：工作区内→相对，区外→绝对），**不能传 abs**：
        // RightPanel 的选中态匹配是字面比较（`filePreview.path === item.path`），而
        // 文件活动列表（aggregateFileActivity）里的 path 来自工具 args、是**相对路径**。
        // 传绝对路径会导致列表里已存在的文件永不匹配 → 右侧详情永远停在「正在读取文件…」
        //（实测复现：selectedPath 残留时 standalone 不成立，renderDetail 内比较失配）。
        run: () => openFile(a.path, undefined, 'source'),
      }
    }
    return {
      label: t('art.openDefault'),
      run: openDefault,
    }
  }, [ext, a.name, a.path, openFile, openHtmlInBrowser, openDefault, previewByExt, t])

  const reveal = async () => {
    const d = window.desktop
    if (!d?.showItemInFolder) {
      showToast(t('art.unsupported'), 'error')
      return
    }
    const r = await d.showItemInFolder(abs)
    if (!r?.ok) showToast(t('art.revealFail').replace('{err}', r?.error || ''), 'error')
  }

  // 主按钮的在途态：只标「正在转换的这个文件」，其余卡片按钮不受影响。
  const thisBusy = previewByExt.has(ext) && officeBusy === a.path

  return (
    <div className="art-acts">
      <button
        className={`btn ${compact ? '' : 'primary'}`}
        onClick={primary.run}
        title={primary.label}
        {...(thisBusy ? { disabled: true } : {})}
      >{thisBusy ? t('art.converting') : primary.label}</button>
      {/* PDF：主按钮已是「预览 PDF」（内嵌 viewer），用默认程序打开降为次按钮但仍可达 ——
          内嵌 viewer 看不了批注/导出。文案沿用 PDF 专用 key，与 html/svg 的
          「在浏览器打开」区分开。 */}
      {PDF_EXTS.has(ext) && (
        <button className="btn" onClick={() => void openDefault()} title={t('art.openDefault')}>{t('art.openDefault')}</button>
      )}
      {/* Office 三种格式同理（理由见主按钮分支）：内嵌预览看不了图表/条件格式/修订/动画，
          对应的桌面应用仍是必要出口；转换失败时（.xls/.doc/.ppt 老格式、运行时未就绪）
          也靠它兜底。判据与主按钮同源（previewByExt），不会出现「有主按钮却没有次出口」。 */}
      {previewByExt.has(ext) && (
        <button className="btn" onClick={() => void openDefault()} title={t('art.openDefault')}>{t('art.openDefault')}</button>
      )}
      {/* 浏览器类已有主按钮=浏览器打开；文本类给「预览」为主 → 其余动作按需补 */}
      <button className="btn icon" onClick={reveal} title={t('art.reveal')} aria-label={t('art.reveal')}>
        <svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round">
          <path d="M1.8 4.6a1.3 1.3 0 0 1 1.3-1.3h2.6l1.3 1.6h6a1.3 1.3 0 0 1 1.3 1.3v5.6a1.3 1.3 0 0 1-1.3 1.3H3.1a1.3 1.3 0 0 1-1.3-1.3z" />
        </svg>
      </button>
    </div>
  )
}

// 路径行：点击 = 复制完整路径（本地客户端高频动作：拿去终端 / 编辑器）。
function PathLine({ a, abs }: { a: ArtifactItem; abs: string }) {
  const t = useT()
  const showToast = useAppStore((s) => s.showToast)
  const [copied, setCopied] = useState(false)

  const copy = async () => {
    // copyText：浏览器 API + 主进程兜底（Electron 下写剪贴板的权限名随用户手势漂移，
    // 此前真实点击必失败；见 src/lib/clipboard.ts）。返回真写入结果 → 反馈才可信。
    if (await copyText(abs)) {
      setCopied(true)
      showToast(t('art.pathCopied').replace('{path}', abs))
      window.setTimeout(() => setCopied(false), 1200)
    } else {
      // 失败不静默（复制失败必须可见，否则无法区分「没点上」与「坏了」）
      showToast(t('art.copyFail'), 'error')
    }
  }

  // 目录前缀与文件名分色：目录淡、文件名亮（文件名已在上一行加重，此处补目录信息）
  const i = a.name ? a.path.lastIndexOf(a.name) : -1
  const dir = i > 0 ? a.path.slice(0, i) : ''
  const file = i > 0 ? a.path.slice(i) : a.path

  return (
    <button
      className={`art-path ${copied ? 'copied' : ''}`}
      onClick={(e) => { e.stopPropagation(); void copy() }}
      title={`${abs}\n${t('art.copyPath')}`}
    >
      <span className="dir">{dir}</span><span className="fn">{file}</span>
      <span className="cp">{copied ? t('art.copied') : t('art.copyPath')}</span>
    </button>
  )
}

export const ArtifactCard = memo(function ArtifactCard({ items, truncated }: { items: ArtifactItem[]; truncated?: boolean }) {
  const t = useT()
  const ws = useAppStore((s) => s.workspace)
  const [expanded, setExpanded] = useState(false)

  // 同内容去重标注：按 sha256 找同内容的首个产出（不同路径、内容一致 —— 如 a.html 与 copy.html）。
  const dupOf = useMemo(() => {
    const first = new Map<string, string>()
    const out = new Map<number, string>()
    items.forEach((a, i) => {
      if (!a.sha256) return
      const prev = first.get(a.sha256)
      if (prev) out.set(i, prev)
      else first.set(a.sha256, a.name)
    })
    return out
  }, [items])

  const dirCount = useMemo(
    () => new Set(items.map((a) => { const i = a.path.lastIndexOf('/'); return i > 0 ? a.path.slice(0, i) : '' })).size,
    [items],
  )

  if (!items.length) return null

  const hero = items[0]
  const rest = items.slice(1)
  const collapsed = !expanded && rest.length > MAX_ROWS
  const hiddenCount = collapsed ? rest.length - MAX_ROWS : 0

  const row = (a: ArtifactItem, idx: number, compact: boolean) => {
    const abs = absPathOf(a.path, ws)
    const dup = dupOf.get(idx + 1) // items 下标（hero 占了 0）
    return (
      <div className="art" key={`${a.path}-${idx}`}>
        {/* data-ft：类型图标按**文件类型**着色（此前一列全是同一个 --accent，扫不出类型）。
            组件只说"这是什么类型"，颜色在 index.css 里按 [data-ft] 落。 */}
        <span className="fic" data-ft={fileTypeOf(a.name)}><TypeGlyph ext={extOf(a.name)} size={compact ? 14 : 15} /></span>
        <div className="art-main">
          <div className="art-l1">
            <span className="art-name" title={a.name}>{a.name}</span>
            <span className={`badge ${a.kind === 'created' ? 'new' : 'mod'}`}>
              {a.kind === 'created' ? t('art.new') : t('art.modified')}
            </span>
            {(a.added || a.removed) && (
              <span className="art-stat">
                {a.added ? <b className="add">+{a.added}</b> : null}
                {a.removed ? <b className="del">−{a.removed}</b> : null}
              </span>
            )}
            <span className="art-meta">
              {a.size ? fmtSize(a.size) : ''}
              {a.size && a.lines ? ' · ' : ''}
              {a.lines ? t('art.lines').replace('{n}', String(a.lines)) : ''}
            </span>
            {dup && <span className="art-dup">{t('art.sameAs').replace('{name}', dup)}</span>}
          </div>
          <PathLine a={a} abs={abs} />
        </div>
        <ArtifactActions a={a} abs={abs} compact={compact} />
      </div>
    )
  }

  return (
    <div className="artifacts-card">
      <div className="art-hero">
        <div className="art-tile">
          <span className="glyph" data-ft={fileTypeOf(hero.name)}><TypeGlyph ext={extOf(hero.name)} size={32} /></span>
          <span className="ext">{extOf(hero.name) || 'file'}</span>
        </div>
        <div className="art-hero-body">
          <div className="art-l1">
            <span className="art-hero-name" title={hero.name}>{hero.name}</span>
            <span className={`badge ${hero.kind === 'created' ? 'new' : 'mod'}`}>
              {hero.kind === 'created' ? t('art.new') : t('art.modified')}
            </span>
            {(hero.added || hero.removed) && (
              <span className="art-stat">
                {hero.added ? <b className="add">+{hero.added}</b> : null}
                {hero.removed ? <b className="del">−{hero.removed}</b> : null}
              </span>
            )}
          </div>
          <PathLine a={hero} abs={absPathOf(hero.path, ws)} />
          <div className="art-meta hero-meta">
            {hero.size ? fmtSize(hero.size) : ''}
            {hero.size && hero.lines ? ' · ' : ''}
            {hero.lines ? t('art.lines').replace('{n}', String(hero.lines)) : ''}
          </div>
          <ArtifactActions a={hero} abs={absPathOf(hero.path, ws)} />
        </div>
      </div>

      {rest.length > 0 && (
        <div className={`art-rest ${collapsed ? 'collapsed' : ''}`}>
          <div className="art-rest-hd">
            <span>{t('art.more').replace('{n}', String(rest.length))}</span>
            <span className="grow" />
            {rest.length > MAX_ROWS && (
              <span className="art-showing">{t('art.showing').replace('{shown}', String(collapsed ? MAX_ROWS : rest.length)).replace('{total}', String(rest.length))}</span>
            )}
          </div>
          {/* 折叠用 CSS 隐藏（.collapsed .art-more → display:none）而非 slice：
              展开/收起不重建 DOM，也保证「展开后条数」与 DOM 一致（e2e 断言依据）。 */}
          {rest.map((a, i) => (
            <div className={i >= MAX_ROWS ? 'art-more' : ''} key={`${a.path}-${i}`}>
              {row(a, i, true)}
            </div>
          ))}
        </div>
      )}

      <div className="art-ft">
        <span>{t('art.total').replace('{n}', String(items.length))}</span>
        {dirCount > 1 && <><span className="sep">·</span><span>{t('art.dirs').replace('{n}', String(dirCount))}</span></>}
        {truncated && <><span className="sep">·</span><span className="warn">⚠ {t('art.truncated')}</span></>}
        <span className="grow" />
        {hiddenCount > 0 && (
          <button className="btn" onClick={() => setExpanded(true)}>{t('art.expand').replace('{n}', String(hiddenCount))}</button>
        )}
        {expanded && rest.length > MAX_ROWS && (
          <button className="btn" onClick={() => setExpanded(false)}>{t('art.collapse')}</button>
        )}
        {rest.length > 0 && (
          <button className="btn" onClick={() => void revealAll(items, ws, t('art.unsupported'), useAppStore.getState().showToast)}>
            {t('art.revealAll')}
          </button>
        )}
      </div>
    </div>
  )
})

// revealAll 批量在 Finder 中显示（逐个调用；系统会依次高亮/打开各目录）。
async function revealAll(items: ArtifactItem[], ws: string | undefined, unsupported: string, toast: (m: string, kind?: 'ok' | 'error') => void) {
  const d = window.desktop
  if (!d?.showItemInFolder) {
    toast(unsupported, 'error')
    return
  }
  // 同目录只显示一次（避免同一目录被反复打开）
  const seen = new Set<string>()
  for (const a of items) {
    const abs = absPathOf(a.path, ws)
    const dir = abs.slice(0, abs.lastIndexOf('/'))
    if (seen.has(dir)) continue
    seen.add(dir)
    await d.showItemInFolder(abs)
  }
}
