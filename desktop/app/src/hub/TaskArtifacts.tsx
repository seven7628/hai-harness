import { useT } from '../i18n'
import type { HubTaskArtifact } from './types'

// 任务卡里的产物区：demo 的 artifact-row 解剖（图标 + 名称 + meta），
// 不再套一层「Artifacts 标题 + 大卡片」；动作由外层决定，不放假按钮。

const EXT: Record<string, string> = {
  py: 'PY', go: 'GO', rs: 'RS', java: 'JAVA',
  ts: 'TS', tsx: 'TS', js: 'JS', jsx: 'JS', mjs: 'JS', cjs: 'JS',
  css: 'CSS', scss: 'CSS', html: 'HTML', vue: 'VUE',
  md: 'MD', mdx: 'MD', txt: 'TXT', json: 'JSON', yaml: 'YML', yml: 'YML',
  toml: 'TML', sql: 'SQL', sh: 'SH', bash: 'SH', mod: 'MOD', lock: 'LOCK',
  csv: 'CSV', xml: 'XML', proto: 'PROTO',
  png: 'IMG', jpg: 'IMG', jpeg: 'IMG', gif: 'IMG', webp: 'IMG', svg: 'IMG',
  ico: 'IMG', pdf: 'PDF', zip: 'ZIP',
}

export default function TaskArtifacts({ artifacts }: { artifacts: HubTaskArtifact[] }) {
  const t = useT()
  if (artifacts.length === 0) return null
  return (
    <section className="event-artifacts" aria-label={t('artifacts.aria').replace('{n}', String(artifacts.length))}>
      {artifacts.map((a, i) => {
        const path = a.path ?? ''
        const name = a.name || path.split('/').pop() || '文件'
        const created = a.kind === 'created'
        const modified = a.kind === 'modified'
        return (
          <div className="artifact-line" key={`${path || a.name}-${i}`} title={path || name}>
            <span className="artifact-badge" aria-hidden="true">{badgeOf(name)}</span>
            <span className="artifact-copy">
              <span className="artifact-name">{name}</span>
              <span className="artifact-meta">
                {path && path !== name ? <span className="artifact-path">{path}</span> : null}
                {created || modified ? <span>{t(created ? 'artifacts.created' : 'artifacts.modified')}</span> : null}
                {a.size ? <span>{bytesText(a.size)}</span> : null}
                {a.lines ? <span>{t('artifacts.lines').replace('{n}', String(a.lines))}</span> : null}
              </span>
            </span>
          </div>
        )
      })}
    </section>
  )
}

function badgeOf(name: string): string {
  const dot = name.lastIndexOf('.')
  const ext = dot > 0 ? name.slice(dot + 1).toLowerCase() : ''
  if (!ext) return 'FILE'
  return EXT[ext] || ext.slice(0, 4).toUpperCase()
}

function bytesText(bytes: number): string {
  if (bytes <= 0) return ''
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`
}
