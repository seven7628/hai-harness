p = 'docs/AGENT_CWD_DESIGN_2026-11-19.md'
doc = open(p, encoding='utf-8').read()
lines = doc.split('\n')

def find(pred, start=0):
    for i in range(start, len(lines)):
        if pred(lines[i]):
            return i
    raise SystemExit('not found: ' + repr(pred))

# ---- 1) §0 结论：补 v3 载体结论 ----
i = find(lambda l: l.startswith('6. **⛔ 复审发现'))
lines.insert(i + 1, '7. **模型可见 cwd 的载体（v3，§15）**：**① 工具描述里固定输出当前工作目录**（tools schema 每请求重算 ⇒ 压缩/恢复都存活、最稳）'
                        ' + **② cd 生效时工具结果回执一行新目录**（位置最显著、进 history）；'
                        '**③ 环境块默认砍掉**（已被前两者覆盖）。⇒ 比"每轮插一条消息"更简单、更稳、不动 checkpoint 结构。')

# ---- 2) §9 V5 表述修正 ----
i = find(lambda l: l.startswith('| V5 |'))
lines[i] = ('| V5 | **重放一致性**（不是"checkpoint 不变"） | 同一事件流重放两次 ⇒ 逐字节相同的 checkpoint。'
            '⚠️ v3：工具回执会进 history ⇒ **新会话的 checkpoint 会变（预期内）** | §12.6 |')

# ---- 3) §14.12 落地顺序：S4 改写 ----
i = find(lambda l: l.startswith('| **S4** |'))
lines[i] = ('| **S4** | **bash 描述带 cwd（D\'）** + **工具结果回执（B）**；**不再做环境块**（§15） | 模型侧 |')

# ---- 4) §12.4 整节替换：env 注入测试 → D'/B 测试 ----
i0 = find(lambda l: l.startswith('### 12.4 '))
i1 = find(lambda l: l.startswith('### 12.5'), i0)
new124 = '''### 12.4 D' / B 测试（V2 / V8 / V9 —— v3 取代原"环境块注入"测试）

> v3：模型可见 cwd 的载体改成 **D'（工具描述固定输出 cwd）** + **B（工具结果回执）**，
> **环境块取消** ⇒ 原 §12.4 的三个测试相应替换。

```go
// tools/builtin/bash_describe_test.go
package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/workdir"
)

// TestDescribeForIncludesWorkingDir 工具描述里**固定输出当前工作目录**（D'）。
func TestDescribeForIncludesWorkingDir(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	tool := NewBashTool(wd.Root(), 0, nil, nil)
	tool.wd = wd

	ctx := workdir.WithKey(context.Background(), workdir.MainKey)
	desc := tool.DescribeFor(ctx)
	if !strings.Contains(desc, "Working directory: "+root) {
		t.Fatalf("描述里没有当前目录：\\n%s", desc)
	}

	// cd 之后，描述要跟着变（每请求重算）
	if _, _, err := wd.Set(workdir.MainKey, sub); err != nil {
		t.Fatal(err)
	}
	desc2 := tool.DescribeFor(ctx)
	if !strings.Contains(desc2, "Working directory: "+sub) {
		t.Fatalf("cd 后描述没更新：\\n%s", desc2)
	}
}

// TestDescribeForPerCaller 子 agent 拿到**自己的** cwd（不是主 agent 的）。
func TestDescribeForPerCaller(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	tool := NewBashTool(wd.Root(), 0, nil, nil)
	tool.wd = wd

	if _, _, err := wd.Set(workdir.MainKey, root); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wd.Set("task-1", sub); err != nil {
		t.Fatal(err)
	}
	mainDesc := tool.DescribeFor(workdir.WithKey(context.Background(), workdir.MainKey))
	subDesc := tool.DescribeFor(workdir.WithKey(context.Background(), "task-1"))
	if !strings.Contains(mainDesc, "Working directory: "+root) {
		t.Fatalf("主 agent 描述错：\\n%s", mainDesc)
	}
	if !strings.Contains(subDesc, "Working directory: "+sub) {
		t.Fatalf("子 agent 描述错（应为自己的 cwd）：\\n%s", subDesc)
	}
}

// TestBashResultCarriesCwdNotice cd 生效时，**工具结果**里回执新目录（B）。
func TestBashResultCarriesCwdNotice(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	tool := NewBashTool(wd.Root(), 0, nil, nil)
	tool.wd = wd
	ctx := bashCtx("run-1", "")

	out, err := tool.Call(ctx, "", `{"command":"cd `+sub+` && echo hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Working directory is now "+sub) {
		t.Fatalf("结果里没有 cwd 回执：\\n%s", out)
	}
}

// TestBashResultCarriesResetNotice 越界 ⇒ 回执重置（Claude Code 同款语义）。
func TestBashResultCarriesResetNotice(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	wd := workdir.New(root)
	tool := NewBashTool(wd.Root(), 0, nil, nil)
	tool.wd = wd
	ctx := bashCtx("run-1", "")

	out, err := tool.Call(ctx, "", `{"command":"cd `+outside+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Working directory was reset to "+root) {
		t.Fatalf("结果里没有重置回执：\\n%s", out)
	}
}
```

```go
// tools/engine_test.go（追加）—— 引擎优先用 ctx 版描述
func TestToolParamsPrefersContextDescriber(t *testing.T) {
	e := NewEngine()
	e.RegisterTool(context.Background(), &ctxDescTool{})
	schemas, err := e.ToolParams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(schemas[0].Description, "ctx-aware") {
		t.Fatalf("引擎没优先用 DescribeFor：%q", schemas[0].Description)
	}
}
```

'''
lines[i0:i1] = new124.split('\n')

open(p, 'w', encoding='utf-8').write('\n'.join(lines))
print('ok, lines =', len(lines))
