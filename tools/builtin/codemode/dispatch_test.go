package codemode

// dispatch_test.go：**分派与命名解析**的单元测试（不需要 node）。
//
// 单列一文件的理由：这里是本波次最容易被写错、且错了最难查的一条纪律 ——
// 「分派只认目录表，绝不 GetTool 兜底」。
//
// 端到端用例（bridge_test.go 的 ⑥）只能证明「脚本调用被拒」，而脚本的表层拒绝发生在
// **沙箱侧**（tools 的 Proxy 只认 init 里给的名字）—— 实测：把 GetTool 兜底加回宿主侧，
// 那条端到端用例**仍然绿**（帧根本不会发出来）。故纪律本身必须在宿主侧直接钉住：
// resolve / onCall / initPayload 各自断言一遍，并用计数探针证明「不在表里的名字
// 一次都没到达引擎」。

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/execproc"
	"github.com/seven7628/hai-harness/tools"
)

// countingEngine 记下每一次 ExecuteOne —— 分派纪律的**探针**。
//
// 为什么需要它：脚本侧的「未知工具」拒绝发生在沙箱（tools 的 Proxy 只认 init 里给的名字），
// 所以「宿主侧有没有偷偷把 hidden 工具放进引擎」这件事，端到端用例看不出来。这里直接数
// ExecuteOne：一个不在目录表里的名字，**一次都不该**到达引擎。
type countingEngine struct {
	tools.ExecuteOneEngine
	mu    sync.Mutex
	calls []string
}

func (c *countingEngine) ExecuteOne(ctx context.Context, call core.ToolCall, opts tools.ExecuteOpts) core.ToolResult {
	c.mu.Lock()
	c.calls = append(c.calls, call.Name)
	c.mu.Unlock()
	return c.ExecuteOneEngine.ExecuteOne(ctx, call, opts)
}

func (c *countingEngine) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, got := range c.calls {
		if got == name {
			n++
		}
	}
	return n
}

// TestDispatchMapIsTheOnlySourceOfTruth 分派表只认 Catalog.Names / RawNames：
// hidden 与 model-only 一律拒绝，绝不去引擎里 GetTool 兜底。
func TestDispatchMapIsTheOnlySourceOfTruth(t *testing.T) {
	t.Parallel()
	direct := newTierStub("read_file", tools.ExposureDirect)
	deep := newTierStub("deep_thing", tools.ExposureCodemode)
	latent := newTierStub("mcp__dev__latent", tools.ExposureDeferred)
	gone := newTierStub("gone_tool", tools.ExposureHidden)
	ghost := newTierStub("ghost_tool", tools.ExposureModelOnly)
	self := newTierStub(ToolName, tools.ExposureModelOnly)

	engine := tools.NewToolEngine()
	for _, tl := range []tools.Tool{direct, deep, latent, gone, ghost, self} {
		engine.RegisterTool(context.Background(), tl)
	}
	spy := &countingEngine{ExecuteOneEngine: engine}
	b, _ := newOpBridge(t, []tools.Tool{direct, deep, latent, gone, ghost, self}...)
	b.engine = spy

	// 可编排档：解析成引擎内 raw 名（脚本名与原始名双键都要认）。
	for _, c := range []struct{ script, want string }{
		{"read_file", "read_file"},
		{"deep_thing", "deep_thing"},
		{"mcp__dev__latent", "mcp__dev__latent"}, // deferred：可编排、不列举
		{"mcp__dev__latent", "mcp__dev__latent"},
	} {
		got, err := b.resolve(c.script)
		if err != nil || got != c.want {
			t.Fatalf("resolve(%q) = (%q, %v), want (%q, nil)", c.script, got, err, c.want)
		}
	}
	// 归一化名字的服务（连字符等）：目录表给什么就用什么。
	hyphen, _ := b.resolve(scriptNameFor("mcp__dev__latent"))
	if hyphen != "mcp__dev__latent" {
		t.Fatalf("脚本名解析 = %q, want mcp__dev__latent", hyphen)
	}

	// 不可编排档 + 未知名字：**一律拒绝**（哪怕引擎里真有这个工具）。
	for _, name := range []string{"gone_tool", "ghost_tool", ToolName, "does_not_exist"} {
		if got, err := b.resolve(name); err == nil {
			t.Fatalf("resolve(%q) = %q，但它不在目录表里 —— 分派必须只认目录表"+
				"（hidden 撤下 / model-only 禁自嵌套都靠这条）", name, got)
		} else if !strings.Contains(err.Error(), "not in this call's codemode tool table") {
			t.Fatalf("resolve(%q) 的拒绝原因不可读: %v", name, err)
		}
		// 走**真正的分派入口**：拒绝必须发生在宿主侧，且绝不把名字交给引擎。
		r := b.onCall(context.Background(), execproc.ScriptCall{Id: "1", Name: name, Args: json.RawMessage(`{}`)})
		if !r.IsError {
			t.Fatalf("onCall(%q) 没被拒: %s", name, r.Value)
		}
		if !strings.Contains(string(r.Value), "not in this call's codemode tool table") {
			t.Fatalf("onCall(%q) 的拒绝文案不是「不在工具表里」: %s —— 分派兜底了？", name, r.Value)
		}
	}
	if len(spy.calls) != 0 {
		t.Fatalf("不在目录表里的名字到达了引擎: %v（分派兜底 = hidden/model-only 可编排）", spy.calls)
	}
	if gone.runs.Load() != 0 || ghost.runs.Load() != 0 || self.runs.Load() != 0 {
		t.Fatalf("不可编排工具被执行: gone=%d ghost=%d self=%d",
			gone.runs.Load(), ghost.runs.Load(), self.runs.Load())
	}
}

// TestInitPayloadCarriesOnlyCallableTiers 沙箱的工具表同样只含可编排档 ——
// 这是「分派只认目录表」在沙箱侧的物理形态（Proxy 也只认它）。
func TestInitPayloadCarriesOnlyCallableTiers(t *testing.T) {
	t.Parallel()
	list := []tools.Tool{
		newTierStub("read_file", tools.ExposureDirect),
		newTierStub("deep_thing", tools.ExposureCodemode),
		newTierStub("mcp__dev__latent", tools.ExposureDeferred),
		newTierStub("gone_tool", tools.ExposureHidden),
		newTierStub("ghost_tool", tools.ExposureModelOnly),
		newTierStub(ToolName, tools.ExposureModelOnly),
	}
	b, _ := newOpBridge(t, list...)
	raw := b.initPayload(nil)
	var payload initPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("init 载荷不是合法 JSON: %v (%s)", err, raw)
	}
	got := map[string]string{}
	for _, tl := range payload.Tools {
		got[tl.Name] = tl.RawName
	}
	want := map[string]string{
		"read_file":        "read_file",
		"deep_thing":       "deep_thing",
		"mcp__dev__latent": "mcp__dev__latent",
	}
	if len(got) != len(want) {
		t.Fatalf("沙箱工具表 = %v, want %v（只含可编排档）", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("工具表项 %q = %q, want %q", k, got[k], v)
		}
	}
	// 限额也随 init 送进沙箱（数值只有一份真源：store.go 的常量）。
	if payload.StoreLimits == nil || payload.StoreLimits.ValueBytes != MaxStoreValueBytes ||
		payload.StoreLimits.TotalBytes != MaxStoreTotalBytes {
		t.Fatalf("init 载荷的 store 限额不对: %+v", payload.StoreLimits)
	}
	// 确定性：两次组装必须逐字节一致（工具表进 init 载荷，抖动会让脚本看到的表变来变去）。
	if again := b.initPayload(nil); string(again) != string(raw) {
		t.Fatalf("init 载荷不稳定:\n%s\n%s", raw, again)
	}
}

// TestChildIDsAreUniqueAndAnchored 子调用 id 必须唯一且看得出归属（记录/审批锚都靠它）。
func TestChildIDsAreUniqueAndAnchored(t *testing.T) {
	t.Parallel()
	tool := New(Options{})
	a1, a2 := tool.nextAnchor(), tool.nextAnchor()
	if a1 == a2 || a1 == "" {
		t.Fatalf("锚必须唯一且非空: %q %q", a1, a2)
	}
	seen := map[string]bool{}
	for i := int64(1); i <= 5; i++ {
		id := childID(a1, i)
		if seen[id] {
			t.Fatalf("子调用 id 重复: %q", id)
		}
		seen[id] = true
		if !strings.Contains(id, a1) {
			t.Fatalf("子调用 id %q 看不出归属（缺锚 %q）", id, a1)
		}
	}
}
