import { useMemo } from 'react'
import { Markdown } from './markdown'
import { useT } from '../i18n'
import { buildCsvTable, toGfmTable, type CsvTable } from '../lib/csv'

// CSV/TSV 表格视图（右栏「最终文件」的渲染形态之一，见 RightPanel.renderFinalFile）。
//
// 为什么复用 <Markdown> 而不是自己拼 <table>：
//   ① 零新依赖：CSV → GFM 表格 markdown → 既有的 react-markdown + remark-gfm 管线；
//   ② 观感天然一致：对话区 Markdown 里的表格、Markdown 文件预览里的表格，与本视图
//      是**同一套 DOM 与 CSS**（.md table / .md-table-wrap），不存在三套表格样式各自演化；
//   ③ 宽表横滚（.md-table-wrap 的 overflow-x）是 2026-09 刚修过的既有能力，白捡。
// 代价：markdown 解析有成本，故在 csv.ts 里按**实测**设了行/列/总格数三重闸门。
//
// 本组件只负责「表格 + 说明」；CSV 的解析与转义全在 lib/csv.ts（纯函数、可单测）。
// 参数只有 content 与 path：内容来自既有 file_preview 流程（见 RightPanel），
// 本组件不自己读文件 —— 读文件的权限/上限（1 MiB）已由 bridge 侧统一把关。

/** 表格视图的渲染（内容已就绪时调用；空文件给明确提示，而不是渲染一张空表）。 */
export default function CsvPreview({ content, path }: { content: string; path: string }) {
  const t = useT()
  // useMemo：filePreview 内容不变时不重解析（父组件每次渲染都重建，不值当重复解析大表；
  // Markdown 组件的 memo 也只挡住下游，解析本身在这里）。
  const table = useMemo(() => buildCsvTable(content, path), [content, path])
  const md = useMemo(() => (table ? toGfmTable(table) : ''), [table])

  if (!table) return <div className="file-activity-empty">{t('rp.file.csvEmpty')}</div>

  return (
    <div className="csv-preview">
      <CsvNotes t={t} table={table} />
      {/* 复用对话区 Markdown 组件：GFM 表格 + 宽表横滚一次到位。
          外层 .csv-doc 只做「文档页」排版（与 Markdown 预览的 .md-preview-doc 同口径），
          表格元素级样式继续走 .md 的既有规则。 */}
      <div className="csv-doc"><Markdown text={md} /></div>
    </div>
  )
}

// 表格上方的说明条。**必须解释三件事**（每件都是「用户会以为是 bug」的点）：
//   ① 首行当表头：CSV 没有表头约定，也无法可靠推断 → 明说，否则用户以为首行数据被吃；
//   ② ↵：单元格内的换行被折叠（GFM 单元格放不下真换行）；
//   ③ 行/列被截断与脏数据的提示，绝不静默丢数据。
function CsvNotes({ t, table }: { t: (k: string) => string; table: CsvTable }) {
  const notes: string[] = [t('rp.file.csvHeaderNote')]
  if (table.multiline) notes.push(t('rp.file.csvMultiNote'))
  if (table.ragged) notes.push(t('rp.file.csvRaggedNote'))
  if (table.wide) notes.push(t('rp.file.csvDroppedCols').replace('{n}', String(table.cols)))
  return (
    <div className="csv-notes">
      <span>{notes.join(' · ')}</span>
      {table.hiddenRows > 0 && (
        <span className="warn">⚠ {t('rp.file.csvMoreRows').replace('{n}', String(table.hiddenRows))}</span>
      )}
    </div>
  )
}
