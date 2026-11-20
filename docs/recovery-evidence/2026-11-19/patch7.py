# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
old = """@@
 	// 这一行是**单行字面量**（现长度 ~600 字符）：按字段名插入，不要重排。
-	bs = &bridgeSession{id: id, runtime: ws, s: s, [既有字段全部保留]}
+	bs = &bridgeSession{id: id, runtime: ws, s: s, /* 既有字段原样保留 */ selectionScope: scope}
```"""
new = """```

**第二处**：同一函数末尾的 `bs = &bridgeSession{...}` 是一个**单行**字面量（现长约 600 字符，
字段由 `gofmt` 不换行）。不要重排它，只在 `s: s,` 之后插入一个字段：

```go
selectionScope: scope,
```

> 为什么不做成第二个 hunk：把 600 字符的行拆成多行会产生一片与本次改动无关的 diff，
> review 时看不出真正加了什么。""" + "\n```diff"
# The result would create an empty diff fence; instead just close and drop the trailing fence.
new = new.replace("\n```diff","")
assert old in s
s=s.replace(old,new,1)
open(p,'w').write(s)
print('ok')
