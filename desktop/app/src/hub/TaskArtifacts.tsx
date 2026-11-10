import { useT } from '../i18n'
import type { HubTaskArtifact } from './types'

// 任务卡里的产物区：与运行时的 ArtifactsBlock 同一套「文件图标 + 名称 + 路径 + 计量」语言，
// 但用 Hub 的 token 重新排版（Hub 不挂 data-skin=runtime，不能直接复用那份作用域 CSS）。
// 这里只做展示；预览/定位动作由外层决定，避免出现无 handler 的假按钮。
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
    <section className="hub-artifacts" aria-label={t('artifacts.aria').replace('{n}', String(artifacts.length))}>
      <header className="hub-artifacts-head">
        <span className="hub-artifacts-title">
          {t('artifacts.head')} <b>{artifacts.length}</b> {t('artifacts.files')}
        </span>
      </header>
      <ul className="hub-artifacts-list">
        {artifacts.map((a, i) => {
          const path = a.path ?? ''
          const name = a.name || path.split('/').pop() || '文件'
          const created = a.kind === 'created'
          const modified = a.kind === 'modified'
          return (
            <li className="hub-artifact" key={`${path || a.name}-${i}`} title={path || name}>
              <span className="hub-artifact-badge" aria-hidden="true">{badgeOf(name)}</span>
              <span className="hub-artifact-info">
                <span className="hub-artifact-name">{name}</span>
                {path && path !== name ? <span className="hub-artifact-path">{path}</span> : null}
              </span>
              <span className="hub-artifact-meta">
                {created || modified ? (
                  <span className={`hub-artifact-kind ${created ? 'created' : 'modified'}`}>
                    {t(created ? 'artifacts.created' : 'artifacts.modified')}
                  </span>
                ) : null}
                {a.size ? <span>{bytesText(a.size)}</span> : null}
                {a.lines ? <span>{t('artifacts.lines').replace('{n}', String(a.lines))}</span> : null}
              </span>
            </li>
          )
        })}
      </ul>
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
