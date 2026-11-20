# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
L=open(p).read().split('\n')
def block_after_heading(heading, lang='tsx'):
    a=next(i for i,l in enumerate(L) if heading in l)
    h=next(i for i in range(a,a+20) if L[i].startswith('```'+lang) or L[i].startswith('```'))
    g=next(i for i in range(h+1,len(L)) if L[i].startswith('```'))
    return h,g

h,g=block_after_heading("#### E-Narrative.tsx-1：把 bundles 传进产出卡")
L[h+1:g]='''    if (b.kind === 'artifacts') {
      return (
        <div className="msg artifacts" key={b.id}>
          {/* 产出卡照 parts/artifacts.js 的形态（主卡 + 另外 N 个 + 展开剩余） */}
          <ArtifactsBlock
            items={b.items}
            {...(b.truncated ? { truncated: true } : {})}
            {...(b.bundles ? { bundles: b.bundles } : {})}
            onPreview={(a) => openFile(a.path)}
            onOpenBundle={(x) => void openArtifactPreview(x, { activate: true })}
            onReveal={(a) => {
              const d = bridge
              if (!d?.showItemInFolder) return
              void d.showItemInFolder(absArtifactPath(a.path, workspace))
            }}
          />
        </div>
      )
    }'''.split('\n')

h,g=block_after_heading("#### E-ArtifactsBlock.tsx-1：`bundles` prop")
L[h+1:g]='''export function ArtifactsBlock({
  items,
  truncated,
  bundles,
  wall,
  onPreview,
  onReveal,
  onOpenBundle,
}: {
  items: ArtifactItem[]
  truncated?: boolean
  /** 产物包（有则先渲染 BundlesBlock，再渲染下面的文件清单） */
  bundles?: ArtifactBundleItem[]
  /** 这一轮的用时（毫秒）；没有就不画 */
  wall?: number
  onPreview?: (a: ArtifactItem) => void
  onReveal?: (a: ArtifactItem) => void
  onOpenBundle?: (b: ArtifactBundleItem) => void
}) {'''.split('\n')

h,g=block_after_heading("#### E-ArtifactsBlock.tsx-2：渲染位置")
L[h+1:g]='''  return (
    <section className="artifacts-card" aria-label={t('artifacts.aria', { n: items.length })}>
      {bundles?.length ? <BundlesBlock bundles={bundles} onOpen={onOpenBundle} /> : null}
      {/* 以下是既有主卡 + 「另外 N 个」，一行不动 */}'''.split('\n')

open(p,'w').write('\n'.join(L))
print('ok')
