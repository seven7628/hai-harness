import { useT } from '../i18n'
import { useHostCapabilities } from '../host/panels'
import { useHubStore } from './useHubStore'
import type { HubTaskArtifact } from './types'

// 任务卡里的产物区：demo 的 artifact-row 解剖（图标 + 名称 + meta）。
// 有 taskId + path 时整行是真正的 <button>（可键盘聚焦、aria-haspopup=dialog），
// 点击打开 App 级产物预览弹窗；缺寻址信息时保持纯展示 div，不放假按钮。

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

export default function TaskArtifacts({
  artifacts,
  taskId,
  onOpen,
}: {
  artifacts: HubTaskArtifact[]
  /** 任务 id：有它 + 产物 path 才可打开预览。 */
  taskId?: string
  /** 调用方自定义打开动作（如任务板需要额外上下文）；缺省走 store。 */
  onOpen?: (artifact: HubTaskArtifact) => void
}) {
  const t = useT()
  const openArtifactPreview = useHubStore((s) => s.openArtifactPreview)
  // 手机（P8-01b）：hub_task_artifact_preview 不在 mobile_server 的 allowlist 内 →
  // 产物行退回纯展示（不放假按钮、点了不 403）。桌面能力全开，行为不变。
  const canPreview = useHostCapabilities().artifactPreview
  if (artifacts.length === 0) return null

  const row = (a: HubTaskArtifact, i: number) => {
    const path = a.path ?? ''
    const name = a.name || path.split('/').pop() || '文件'
    const created = a.kind === 'created'
    const modified = a.kind === 'modified'
    const body = (
      <>
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
      </>
    )
    const key = `${path || name}-${i}`
    const previewable = canPreview && Boolean(taskId && path)
    if (!previewable) {
      return (
        <div className="artifact-line" key={key} title={path || name}>
          {body}
        </div>
      )
    }
    return (
      <button
        type="button"
        className="artifact-line artifact-line--action"
        key={key}
        title={`${path || name} · ${t('hub.artifact.preview')}`}
        aria-haspopup="dialog"
        aria-label={t('hub.artifact.open').replace('{name}', name)}
        onClick={() => {
          if (onOpen) onOpen(a)
          else void openArtifactPreview(taskId as string, a)
        }}
      >
        {body}
      </button>
    )
  }

  return (
    <section className="event-artifacts" aria-label={t('artifacts.aria').replace('{n}', String(artifacts.length))}>
      {artifacts.map(row)}
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
