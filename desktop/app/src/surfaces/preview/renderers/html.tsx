// html 渲染器：把 html/htm/svg 的**内容**当页面渲染（`<iframe srcDoc>` 静态快照）
//
// 为什么需要它（2026-11 用户报的回归）：注册表 R08b 只落了 pdf/docx/pptx/xlsx，html 文件
// 落到 weight 10 的 text 兜底 —— 用户点「预览」看到的是一坨源码。01-design §4.7 的渲染器
// 清单里本来就有 `html`，此前只是没接上。
//
// 与「在内嵌浏览器中打开」的分工（谁是谁的默认）：
//   · **默认通道是内嵌浏览器**（store.openInBrowserTab → WebContentsView + file://）：openFile
//     与 openPreviewTab 对 BROWSER_EXTS 都直接分过去（见 useAppStore 两处注释）。理由很直接：
//     用户点 html 要看的是**效果**，而脚本、交互、字体、相对路径资源都是效果的一部分。
//   · 本组件是**静态快照**：内容来自 file_preview 已拿到的 content（不新开读文件通道 ——
//     bridge 的越界校验与 1 MiB 上限必须在链上），喂给 srcDoc。它接住的是仍然落进
//     mode='preview' 的那些路径（旧版本持久化恢复出来的标签、调用方直接 openTab 的），
//     并给一条明显出口回到可交互的那条路。
//
// 安全（硬约束，不是取舍）：sandbox **既不开 allow-scripts 也不开 allow-same-origin**。
//   · 不开 allow-scripts → <script>、onerror=、onload=、javascript: URL 全都不执行；
//   · 不开 allow-same-origin → 唯一源：读不到父文档 / localStorage / cookie，发不出带凭证的
//     请求。两者同时开会被判「同源」从而自解沙箱 —— 那是本项目明确禁止的组合（见 office.tsx
//     的反面：那支只开 allow-same-origin，因为它要读产物 DOM 且产物是数据）。
// 代价：相对路径资源（./style.css、同级图片）取不到（沙箱里是唯一源 + 无 base）。这是静态
// 快照的固有边界，所以顶部那条出口必须显眼 —— 要完整效果就走内嵌浏览器。
//
// truncated：bridge 的 1 MiB 闸截断后，页面尾部（常常正是最想看的图表）会缺 —— 如实说，
// 不静默（延续 pptx_preview.go 的「诚实比假装像更重要」）。
import { useT } from '../../../i18n'
import { displayPath } from '../../../lib/localAsset'
import { useAppStore } from '../../../store/useAppStore'
import type { PreviewProps } from '../types'

export default function HtmlPreview({ file, content, truncated }: PreviewProps) {
  const t = useT()
  const workspace = useAppStore((s) => s.workspace)
  const openInBrowserTab = useAppStore((s) => s.openInBrowserTab)
  const shown = displayPath(file.path, workspace)
  // 空内容不是「白屏」：说明 + 出口（00 §4.3 空态必须有引导）。
  const empty = content.trim() === ''

  return (
    <div className="pv-html" data-html-truncated={truncated ? '1' : '0'}>
      <div className="pv-html-bar">
        <span className="pv-html-badge">{t('rp.preview.htmlStatic')}</span>
        {truncated && <span className="pv-html-warn">{t('rp.preview.htmlTruncated')}</span>}
        <button
          className="pv-html-open"
          // activate:true = 展开右栏 + 切到 web 标签：用户点了这条出口就该看见那个页面。
          onClick={() => void openInBrowserTab(file.path, { activate: true })}
        >
          {t('rp.preview.htmlOpen')}
        </button>
      </div>
      {empty
        ? (
          <div className="pv-html-empty" role="status">
            <div>{t('rp.preview.htmlEmpty')}</div>
            <div className="pv-html-path mono">{shown}</div>
          </div>
        )
        : (
          <iframe
            className="pv-html-frame"
            // 见头注释：**不可**加 allow-scripts / allow-same-origin。静态预览只摆版式。
            sandbox=""
            // 静态预览没有理由把来源送出去（内容里的外链图片会带 Referer）。
            referrerPolicy="no-referrer"
            title={t('rp.preview.htmlFrameTitle').replace('{path}', shown)}
            srcDoc={content}
          />
        )}
    </div>
  )
}
