---

## 15. 模型可见 cwd 的载体（v3）—— 工具描述固定输出 + 工具结果回执

> 2026-11-19 第三轮（用户提出）：**不要靠环境块这种"补丁式"通道**，
> 应该**在工具描述里明确、固定地输出当前工作目录**；并且**在对应的工具输出里带上路径**。
> 本节是**模型可见性部分**的权威版本（优先于 §3 / §7.3 / §11.2 B7 / §11.4 的对应条目）。

### 15.1 三条载体的取舍

| 载体 | 放哪 | 模型何时看到 | 压缩后存活 | 恢复会话后存活 | 成本 |
|---|---|---|---|---|---|
| **D' 工具描述** | **tools schema**（每请求重建） | **一直在**（每个请求都带） | ✅ 压缩不动 schema | ✅ 每请求重算 | ~10 token/请求；cd 时 schema 变 |
| **B 工具输出** | **tool result**（进 history） | **cd 那一刻最显著** | ⚠️ 可能被摘要掉 | ✅ | 一次 cd 一行 |
| **A 环境块** | 对话尾部（ephemeral） | 请求尾部 | ❌ | ❌ | 变化时一块 |

⇒ **D' + B 已经覆盖全部职责，A（环境块）可以砍掉**：

| 职责 | 原以为要 A | 实际由谁承担 |
|---|---|---|
| 首次 cd 之前知道起点 | A | **D'**（schema 一直带着 cwd） |
| cd 之后立刻知道"变了" | B/事件 | **B**（工具结果那一行） |
| 压缩后恢复 cwd 认知 | A | **D'**（schema 不参与压缩） |
| 恢复会话后纠正历史旧语境 | A | **D'**（每请求按当前 State 重算） |
| 越界被重置 | 工具结果 | **B**（同一行） |

⇒ **方案变简单**：砍掉 A ⇒ 少一个注入点、少一份 diff 状态、少一处 checkpoint 顾虑。

### 15.2 D' 的实现：工具描述里固定输出当前工作目录

**为什么可行（已验证）**：
- `tools/engine.go:620` `ToolParams(ctx)` —— **每请求调用**（唯一调用点 `agents/agent_loop.go:2802`），
  **无任何缓存**（每次遍历 `e.tools` 重建 schema）；
- `ToolParams(ctx)` **已经有 ctx** ⇒ 不用改 `ToolEngine` 接口；
- `agents/agent_loop.go` 的请求装配处 `req.Tools = a.toolSchemas(streamCtx, ac.Mode)` ⇒ **每次请求都重算**。

**① `tools/runtime.go`：加一个可选接口**
```go
// ContextDescriber 允许工具按**调用方上下文**给出描述。
//
// 用途：bash 工具的描述里要带上**当前调用方**的工作目录
// （主 agent 与子 agent 各有自己的 cwd ⇒ 静态 Description() 给不出正确的值）。
//
// 引擎侧：ToolParams(ctx) 优先用 DescribeFor(ctx)，没有则退回 Description()。
type ContextDescriber interface {
	DescribeFor(ctx context.Context) string
}
```

**② `tools/engine.go:620` `ToolParams`：优先用 ctx 版描述**
```go
	for _, name := range names {
		t := e.tools[name]
		...
		desc := t.Description()
		if cd, ok := t.(ContextDescriber); ok {
			desc = cd.DescribeFor(ctx)
		}
		schemas = append(schemas, core.ToolSchema{
			Name:        t.Name(),
			Description: desc,
			Parameters:  t.Parameters(),
		})
```

**③ `workdir`：分区键走 ctx**
```go
// workdir/ctx.go
package workdir

import "context"

type keyCtx struct{ key Key }

// WithKey 把分区键挂到 ctx 上（工具描述按调用方取值时用）。
func WithKey(ctx context.Context, key Key) context.Context {
	return context.WithValue(ctx, keyCtx{}, key)
}

// KeyFrom 取 ctx 上的分区键；没有 ⇒ MainKey（主 agent）。
func KeyFrom(ctx context.Context) Key {
	if ctx == nil {
		return MainKey
	}
	if k, ok := ctx.Value(keyCtx{}).(Key); ok {
		return k
	}
	return MainKey
}
```

**④ `agents/agent_loop.go`：请求装配处带上分区键**
```go
// req.Tools = a.toolSchemas(streamCtx, ac.Mode)     ← 改前
req.Tools = a.toolSchemas(workdir.WithKey(streamCtx, ac.workKey), ac.Mode)   ← 改后
```
> `toolSchemas` 内部还会用这个 ctx 调 `GetTool` / `mode.AllowTool` ⇒ **挂个 value 无副作用** ✓

**⑤ `tools/builtin/bash.go`：描述里固定输出当前工作目录**
```go
// DescribeFor 按调用方给出描述（含**该调用方当前的工作目录**）。
//
// 为什么必须带 cwd：模型每轮都看得到工具 schema ⇒ 这是**最稳的 cwd 载体**
// —— 压缩不动它、会话恢复后每请求重算、首轮就有。
func (t *BashTool) DescribeFor(ctx context.Context) string {
	cwd := t.Cwd
	if t.wd != nil {
		cwd = t.wd.Current(workdir.KeyFrom(ctx))
	}
	return t.describe(cwd)
}

// describe 拼描述。**格式固定**，只有 cwd 的值在变。
func (t *BashTool) describe(cwd string) string {
	return fmt.Sprintf(
		"Executes a given bash command and returns its output.\n"+
			"Working directory: %s\n"+
			"Each command runs in a separate process: environment variables and other shell state do not persist between calls. "+
			"The working directory persists between calls in this session — a successful `cd` changes it for later commands "+
			"(the new directory is reported in this tool's output).",
		cwd)
}

// Description 静态版（无 ctx 时用）：给根目录。
func (t *BashTool) Description() string { return t.describe(t.Cwd) }
```

**给模型看到的样子**：
```
Executes a given bash command and returns its output.
Working directory: /Users/admin/workspace/hai-super-assistant
Each command runs in a separate process: environment variables and other shell state do not persist between calls. The working directory persists between calls in this session — a successful `cd` changes it for later commands (the new directory is reported in this tool's output).
```

> ⚠️ **子 agent 的 cwd 不一样** ⇒ `DescribeFor` 必须按 `workdir.KeyFrom(ctx)` 取，**不能**用静态 `t.Cwd`。
> ⇒ 这是"静态描述做不到、必须走 ctx"的原因，也是加 `ContextDescriber` 接口的唯一理由。

### 15.3 B 的实现：cd 生效时，工具结果里回执一行

**位置**：`tools/builtin/bash.go` 的 cd 处理处（§14.2 B4 的代码块）——
**`setCurrentCwd` 成功且 cwd 真的变了** ⇒ 在**最终返回给模型的文本**末尾追加一行。

```go
// workdir/notice.go
package workdir

import "fmt"

// Notice 生成给模型看的 cwd 变更回执（空 = 无变化，不需要追加）。
//
// 措辞对齐 Claude Code 的 "Shell cwd was reset to <dir>"（越界重置那条），
// 但正常切换用 "Working directory is now ..."（不叫 "shell cwd" ——
// shell 的其它状态确实不保持，只有 cwd 保持，避免误导）。
func Notice(old, cur string) string {
	switch {
	case cur == "" || cur == old:
		return ""
	case old == "":
		return fmt.Sprintf("\nWorking directory is now %s", cur)
	default:
		return fmt.Sprintf("\nWorking directory changed to %s (was %s)", cur, old)
	}
}

// NoticeReset 生成越界重置的回执。
func NoticeReset(root string) string {
	return fmt.Sprintf("\nWorking directory was reset to %s (the requested path is outside the project root)", root)
}
```

**在 bash 工具里接上**：
```go
// cd 生效时：
old, changed, err := t.wd.Set(cwdKey(ctx), abs)
switch {
case err != nil:
	// 越界 ⇒ 重置 + 回执（Claude Code 同款语义）
	notice = workdir.NoticeReset(t.projectRoot())
default:
	if changed {
		notice = workdir.Notice(old, abs)
	}
}
```

**追加到哪里**：必须是**最终交给模型的那份文本**（`Call` 的返回值），
且要在 **spill / slim 之后**追加（输出超限被落盘时，回执仍要可见）：
```go
// tools/builtin/bash.go（Call 的收尾处）
out = spillOrSlim(out)
if notice != "" {
    out += notice        // ← 必须在裁剪之后追加
}
return out, nil
```

**模型看到的样子**：
```
$ cd /repo/src && ls
README.md  main.go

Working directory is now /repo/src
```

**顺带的好处**：这行也会出现在**前端 UI** 的工具输出里 ⇒ 用户也能看到 agent 换目录了（透明度）。

### 15.4 A（环境块）的处置：**默认砍掉**

原 §11.4 计划的"每请求重算 + 差分注入环境块"——**本期不做**。

理由：D' + B 已覆盖 §15.1 表里的全部五项职责；
而 A 有两个实打实的成本：
1. **多一个注入点**（`agents/agent_loop.go` 请求装配处）+ 多一份 diff 状态；
2. **ephemeral 消息的位置尴尬** —— 追加在请求尾部、下一轮即消失，
   模型可能压根没"注意到"它变了（Claude Code 靠 `isMeta` 标记 + 专门的渲染，hai 没有这一层）。

⇒ 若日后仍觉得模型反应不够，**再把 A 加回来作为第三层**（改动是局部的：加一个注入点 + 一个 `sent` 快照），
不影响 D'/B 的设计。

### 15.5 对方案其余部分的影响

| 影响点 | 变化 |
|---|---|
| **§11.4（agent loop 注入环境块）** | **删除** ⇒ 少一个注入点、少 `ac.envSent`、少 `workdir.Env` 的 diff 逻辑 |
| **§12.4（env 注入测试）** | 改为测 **D'**（描述带 cwd、per-agent 正确、cd 后描述更新）+ **B**（回执文本） |
| **§14 R2（Config 双源）** | 仍然成立（`Config.WorkingDir` vs `WorkDir.State`）⇒ 断言方案不变 |
| **V5（checkpoint 逐字节）** | ⚠️ **表述要改**：B 会把回执写进工具结果 ⇒ **进 history ⇒ 进 checkpoint** ⇒ 新会话的 checkpoint **会变**（这是**预期的**）。真正的不变量是「**同一事件流重放两次 ⇒ 逐字节相同的 checkpoint**」，不是「cwd 变化不改 checkpoint」 |
| **§14.12 落地顺序** | S4 从"注入环境块"改为"**bash 描述带 cwd + 工具回执**"，依赖更少（不需要 `workdir.Env`/diff） |
| **缓存** | tools schema 在 cd 时会变 ⇒ 该次请求的 **tools 段缓存**失效（消息前缀缓存不受影响）⇒ 频率 = cd 次数，可接受 |
| **前端** | 工具输出多一行回执 ⇒ UI 直接显示即可（前端**零改动**，因为它只渲染文本） |

### 15.6 修订后的"模型如何知道 cwd 变了"（最终答案）

```
① D' 工具描述：每个请求的 tools schema 里固定一行 "Working directory: <当前cwd>"
   ├─ 压缩不影响（schema 不进 history）
   ├─ 恢复会话后自动正确（每请求重算）
   ├─ 主/子 agent 各自正确（按 ctx 分区键取）
   └─ 首次 cd 之前模型就知道起点

② B 工具回执：cd 生效的那一次，工具结果末尾追加 "Working directory is now <新目录>"
   ├─ 位置最显著（模型刚读完的工具结果）
   ├─ 进 history ⇒ 后续轮次不用回忆也能看到
   └─ 越界重置用同一行告知

③ （已砍）环境块
```

⇒ 与 Claude Code 的差异（明确记录，避免误以为"没对齐"）：

| | Claude Code | hai（v3） |
|---|---|---|
| 载体 | 对话里的 `isMeta` user 消息 | **工具 schema（每请求）+ 工具结果** |
| 在 system prompt 里？ | ❌ 明确不放 | ❌ 不放（本来就不在 system 层） |
| 谁被通知 | 模型（对话层） | 模型（schema + 结果） |
| 压缩后 | 靠每请求重算的附件 | **靠 schema**（更稳） |
| 恢复会话后 | 靠 transcript 的 cwd + 每请求重算 | **靠 schema**（更稳） |
| 子 agent | 不继承父 cwd | 不继承（分区键）✓ 一致 |
