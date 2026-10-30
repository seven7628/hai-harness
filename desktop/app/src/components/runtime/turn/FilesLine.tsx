import { FileIcon } from '../../icons'
import { shortPath } from '../../../lib/shortPath'
import './files-line.css'

/* 这一轮改了哪个文件：一行，带框。设计里它与「视觉产出」是两种东西 ——
   文件是记录（1px 卡、r9、card-2、padding 8/11），产出行是无框的一句话。
   点它展开做出这次改动的工作行（data-cy-open）。

   路径取**第一个被改的文件**，增删是这一轮所有改动的总和（设计 ui.js fileHTML）。 */

export function FilesLine({
  path,
  added,
  removed,
  onOpen,
}: {
  path: string
  added: number
  removed: number
  onOpen?: () => void
}) {
  return (
    <div
      className="cy-file"
      data-slot="files"
      role={onOpen ? 'button' : undefined}
      tabIndex={onOpen ? 0 : undefined}
      onClick={onOpen}
      onKeyDown={(e) => {
        if (!onOpen) return
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault()
          onOpen()
        }
      }}
    >
      <FileIcon size={13} />
      {/* 长路径先短掉，否则 +N −N 被挤出可视区（用户 2026-10）；完整路径在 title 里 */}
      <span className="n" title={path}>
        {shortPath(path)}
      </span>
      <span className="cy-dstat">
        <span className="p">+{added}</span>
        <span className="m">−{removed}</span>
      </span>
      {onOpen ? (
        <svg className="chev" viewBox="0 0 12 12" aria-hidden="true">
          <path d="M4.2 2.6 8.6 6.8l-4.4 4.2" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      ) : null}
    </div>
  )
}
