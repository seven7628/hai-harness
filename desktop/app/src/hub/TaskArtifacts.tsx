import type { HubTaskArtifact } from './types'

// 任务卡里的产物区：与运行时的 ArtifactsBlock 同一套「文件图标 + 名称 + 路径 + 计量」语言，
// 但用 Hub 的 token 重新排版（Hub 不挂 data-skin=runtime，不能直接复用那份作用域 CSS）。
// 这里只做展示；预览/定位动作由外层决定，避免出现无 handler 的假按钮。
export default function TaskArtifacts({ artifacts }: { artifacts: HubTaskArtifact[] }) {
  if (artifacts.length === 0) return null
  return (
    <section className="hub-artifacts" aria-label={`产物 ${artifacts.length} 个`}>
      <header className="hub-artifacts-head">
        <span className="hub-artifacts-title">产出 <b>{artifacts.length}</b> 个文件</span>
        <span className="hub-artifacts-hint">任务执行结果</span>
      </header>
      <ul className="hub-artifacts-list">
        {artifacts.map((a, i) => {
          const path = a.path ?? ''
          const name = a.name || path.split('/').pop() || '文件'
          const kind = a.kind === 'created' ? '新建' : a.kind === 'modified' ? '修改' : (a.kind || '')
          return (
            <li className="hub-artifact" key={`${path || a.name}-${i}`} title={path || name}>
              <span className="hub-artifact-badge" aria-hidden="true">{badgeFor(name)}</span>
              <span className="hub-artifact-info">
                <span className="hub-artifact-name">{name}</span>
                {path && path !== name ? <span className="hub-artifact-path">{path}</span> : null}
              </span>
              <span className="hub-artifact-meta">
                {kind ? <span className={`hub-artifact-kind ${a.kind === 'created' || a.kind === 'modified' ? a.kind : ''}`}>{kind}</span> : null}
                {a.size ? <span>{sizeText(a.size)}</span> : null}
                {a.lines ? <span>{a.lines} 行</span> : null}
              </span>
            </li>
          )
        })}
      </ul>
    </section>
  )
}

function badgeFor(name: string): string {
  const ext = name.split('.').pop()?.toLowerCase() ?? ''
  const map: Record<string, string> = {
    md: 'MD', markdown: 'MD', txt: 'TXT', csv: 'CSV', tsv: 'TSV',
    json: 'JSON', jsonl: 'JSONL', yaml: 'YAML', yml: 'YAML',
    ts: 'TS', tsx: 'TSX', js: 'JS', jsx: 'JSX', go: 'GO', py: 'PY',
    html: 'HTML', css: 'CSS', sql: 'SQL', sh: 'SH',
    png: 'PNG', jpg: 'JPG', jpeg: 'JPG', svg: 'SVG', webp: 'WEBP',
    pdf: 'PDF', xlsx: 'XLSX', docx: 'DOCX', pptx: 'PPTX',
  }
  return map[ext] || (ext ? ext.slice(0, 4).toUpperCase() : 'FILE')
}

function sizeText(bytes: number): string {
  if (bytes >= 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)}MB`
  if (bytes >= 1024) return `${(bytes / 1024).toFixed(bytes >= 10 * 1024 ? 0 : 1)}KB`
  return `${bytes}B`
}
