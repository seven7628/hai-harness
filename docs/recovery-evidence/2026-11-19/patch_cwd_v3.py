import re

p = 'docs/AGENT_CWD_DESIGN_2026-11-19.md'
doc = open(p, encoding='utf-8').read()
lines = doc.split('\n')

def find(pred, start=0):
    for i in range(start, len(lines)):
        if pred(lines[i]):
            return i
    raise SystemExit('not found')

# ---- 1) §3 的 D 行：追加 v3 提示 ----
i = find(lambda l: l.startswith('- **D. 工具描述里教规则**'))
lines.insert(i + 1, '')
lines.insert(i + 2, '> ⚠️ **v3 修订（§15）**：D 这条被升级为**主载体 D\'** —— 工具描述里**固定输出当前工作目录**（不是一句规则说明）；')
lines.insert(i + 3, '> 同时新增 **B**（工具结果里回执新目录）；**A（环境块）默认砍掉**。§15 是模型可见性部分的权威版本。')

# ---- 2) §7.3 通道落地表：整表替换 ----
i0 = find(lambda l: l.startswith('| 通道 | 落地方式 | 落点（已有代码位） | 状态 |'))
i1 = find(lambda l: l.startswith('| **E 文件工具锚点**'), i0)
new73 = [
 '| 通道 | 落地方式 | 落点（已有代码位） | 状态 |',
 '|---|---|---|---|',
 '| **D\' 工具描述（主）** | bash 描述里**固定输出当前工作目录**（每请求重算、按调用方分区） | `tools/builtin/bash.go`（`DescribeFor(ctx)`）+ `tools/engine.go:640`（`ToolParams` 优先用 ctx 版）+ `agents/agent_loop.go`（`toolSchemas` 传分区键） | ✅ **v3 主力**（§15.2） |',
 '| **B 工具结果回执（辅）** | cd 生效时，工具结果末尾追加「Working directory is now X」 | `tools/builtin/bash.go`（cd 处理处，**spill/slim 之后**追加） | ✅ **v3 辅力**（§15.3） |',
 '| ~~A 环境块~~ | ~~每请求重算 + 差分注入对话~~ | ~~`agents/agent_loop.go` 请求装配处~~ | ❌ **v3 砍掉**（D\'+B 已覆盖全部职责，§15.4） |',
 '| **C 返回值自证** | `cd` 的工具结果里回 `pwd` | — | ⊂ B（回执里已含新目录） |',
 '| **E 文件工具锚点** | `FileTools.root` 改为**可变**，跟随 session cwd | `tools/builtin/builtin.go:28`（字段）+ `:201`（唯一入口 `resolveFor`） | ⚠️ 行为变更，见 D3；**注意 ignore 匹配器不跟**（§14 R4） |',
]
lines[i0:i1+1] = new73

# ---- 3) §11.2 B7 文案：整块替换 ----
i0 = find(lambda l: l.startswith('**B7 工具描述改动**'))
i1 = find(lambda l: l.startswith('### 11.3'), i0)
newb7 = '''**B7 工具描述改动** —— ⚠️ **已被 §15.2 取代**：不再靠"规则说明"，
而是**固定格式 + 实际当前工作目录**：

```go
// tools/builtin/bash.go
func (t *BashTool) DescribeFor(ctx context.Context) string {
	cwd := t.Cwd
	if t.wd != nil {
		cwd = t.wd.Current(workdir.KeyFrom(ctx))   // ← 按调用方分区取（主/子 agent 不同）
	}
	return t.describe(cwd)
}

func (t *BashTool) describe(cwd string) string {
	return fmt.Sprintf(
		"Executes a given bash command and returns its output.\\n"+
			"Working directory: %s\\n"+
			"Each command runs in a separate process: environment variables and other shell state do not persist between calls. "+
			"The working directory persists between calls in this session — a successful `cd` changes it for later commands "+
			"(the new directory is reported in this tool's output).",
		cwd)
}
```

> 落地需要三处配套：`tools.ContextDescriber` 接口 + `tools/engine.go:640` 优先用它 +
> `agents/agent_loop.go` 调 `toolSchemas` 时用 `workdir.WithKey(ctx, ac.workKey)` 带上分区键。详见 §15.2。

'''
lines[i0:i1] = newb7.split('\n')

# ---- 4) §11.4 加警示 ----
i = find(lambda l: l.startswith('### 11.4 改：`agents/agent_loop.go`'))
lines.insert(i + 1, '')
lines.insert(i + 2, '> ⚠️ **v3 修订（§15.4）**：本节计划的「环境块注入」**默认砍掉**（D\'+B 已覆盖全部职责）。')
lines.insert(i + 3, '> 本节保留作为**可选增强**的参考实现；若日后要加回来，改动是局部的。')

doc = '\n'.join(lines)

# ---- 5) 追加 §15 ----
s15 = open('/tmp/cwd_s15.md', encoding='utf-8').read()
doc = doc.rstrip('\n') + '\n\n' + s15.rstrip('\n') + '\n'

open(p, 'w', encoding='utf-8').write(doc)
print('ok, lines =', len(doc.split('\n')))
