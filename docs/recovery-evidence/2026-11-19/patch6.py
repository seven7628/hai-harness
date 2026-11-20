# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
L=open(p).read().split('\n')

i=[k for k,l in enumerate(L) if l.startswith('@@ -512,6 +512,9 @@ type bridgeSession struct {')]
assert i, 'no dup block'
i=i[0]
assert L[i-1].startswith('```')
j=i+1
while not L[j].startswith('```'): j+=1
del L[i-1:j+1]

target = "-	bs = &bridgeSession{id: id, runtime: ws, s: s, ...}"
replacement = "-	bs = &bridgeSession{id: id, runtime: ws, s: s, " + "[既有字段全部保留]}"
for k,l in enumerate(L):
    if l == target:
        L[k]=replacement
        break

for k,l in enumerate(L):
    if l == "\t// 与 Ask 入口都会清（见 selection_guard.go）。":
        L[k]="\t// 都会清（见 selection_guard.go）。"
        break

for k,l in enumerate(L):
    if l.startswith("### 21.1 新增文件（14 个）"):
        L[k]="### 21.1 新增文件（24 项，含测试与脚本）"
        break

open(p,'w').write('\n'.join(L))
print('ok')
