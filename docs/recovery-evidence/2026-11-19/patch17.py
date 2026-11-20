# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
L=open(p).read().split('\n')
def block_after_heading(heading):
    a=next(i for i,l in enumerate(L) if heading in l)
    h=next(i for i in range(a,a+20) if L[i].startswith('```'))
    g=next(i for i in range(h+1,len(L)) if L[i].startswith('```'))
    return a,h,g

a,h,g=block_after_heading("#### E-ArtifactsBlock.tsx-1：`bundles` prop")
# fix 定位 line just before fence
for i in range(a,h):
    if L[i].startswith('**定位**'):
        L[i]="**定位**：`components/runtime/artifacts/ArtifactsBlock.tsx:108-121`（组件签名与内联 props 类型）。"
        break

a,h,g=block_after_heading("#### E-ArtifactsBlock.tsx-2：渲染位置")
for i in range(a,h):
    if L[i].startswith('**定位**'):
        L[i]="**定位**：同文件两处 —— `:126`（空判）与 `:150`（卡片根节点）。两处都要改，**只改渲染会让「本轮只有 bundle」的一轮整块消失**。"
        break
L[h+1:g]='''  // 只有 bundle、没有文件清单的一轮（publish 了此前已存在且本轮没改的入口）也必须画：
  // 原来只判 list.length，会让这种轮次整块消失（卡片没出现，用户以为没产出）。
  if (!list.length && !bundles?.length) return null

  // …（total/add/del/wallText 的计算不动）…

  return (
    <div className="ar" data-trunc={truncated ? '1' : '0'} role="group" aria-label={t('artifacts.aria').replace('{n}', String(total))}>
      {bundles?.length ? <BundlesBlock bundles={bundles} onOpen={onOpenBundle} /> : null}
      <div className="ar-h">'''.split('\n')

open(p,'w').write('\n'.join(L))
print('ok')
