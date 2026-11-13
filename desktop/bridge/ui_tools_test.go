// ui_tools_test.go —— `ui_inspect` 的工具契约 + **真跑 CodeMode**（PR-12 D3）。
//
// 为什么这条测试在 desktop/bridge 而不是 tools/builtin/codemode：
// `desktop/bridge` 是 `package main`，Go 不允许其它包 import 它 —— 而 ui_inspect 与
// 快照缓存都住在这里（03-code-plan §3.4 的注册点也在装配层）。把工具搬到可 import 的
// 包，就得为测试另建一份缓存替身，那正是 00-conventions §4.7 明令不算证据的东西。
// 所以这里用 codemode 包**同一套测试法**（tools.NewToolEngine + codemode.New +
// RunBatch），但注册走生产入口：m.dispatch 写快照 → m.registerUITools 注册工具。
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/execproc"
	"github.com/seven7628/hai-harness/runtime"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/tools/builtin/codemode"
)

// callUIInspect 直接调工具的 Call（非脚本路径：不注入 StructuredSink）。
func callUIInspect(t *testing.T, m *manager, args string) map[string]any {
	t.Helper()
	out, err := newUIInspectTool(&m.ui).Call(context.Background(), "ui-1", args)
	if err != nil {
		t.Fatalf("ui_inspect(%s) 返回错误（应当用 ok:false 表达状态）: %v", args, err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("ui_inspect 返回的不是 JSON 对象: %v\n%s", err, out)
	}
	return got
}

// ---- schema 成本约束（常驻工具：≤3 字段、描述 ≤120 字）----

func TestUIInspectSchemaIsCheap(t *testing.T) {
	tl := newUIInspectTool(&uiState{})
	if got := tl.Name(); got != "ui_inspect" {
		t.Fatalf("Name() = %q", got)
	}
	if n := len([]rune(tl.Description())); n > 120 {
		t.Fatalf("描述 %d 字，超过 120 字的常驻成本约束", n)
	}
	if !strings.Contains(tl.Description(), "stale") {
		t.Fatalf("描述必须写明 stale（>5s），实得 %q", tl.Description())
	}
	params, ok := tl.Parameters().(map[string]any)
	if !ok {
		t.Fatalf("Parameters() 类型 = %T", tl.Parameters())
	}
	props := params["properties"].(map[string]any)
	if len(props) > 3 {
		t.Fatalf("参数 %d 个，超过 3 个的上限", len(props))
	}
	what, ok := props["what"].(map[string]any)
	if !ok {
		t.Fatalf("缺 what 参数：%v", props)
	}
	enum := what["enum"].([]string)
	for _, want := range []string{"all", "layout", "surfaces", "tabs", "editors", "theme", "viewport"} {
		found := false
		for _, e := range enum {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("enum 缺 %q：%v", want, enum)
		}
	}
	if !tl.CanParallel() || !tl.ReadOnly() {
		t.Fatal("ui_inspect 必须 CanParallel=true / ReadOnly=true（只读缓存，不该进串行门）")
	}
}

func TestUIInspectValidParams(t *testing.T) {
	tl := newUIInspectTool(&uiState{})
	ctx := context.Background()
	for _, ok := range []string{``, `{}`, `{"what":"all"}`, `{"what":"tabs"}`, `null`} {
		if err := tl.ValidParams(ctx, "ui_inspect", ok); err != nil {
			t.Fatalf("合法参数 %q 被拒: %v", ok, err)
		}
	}
	for _, bad := range []string{`{"what":"nope"}`, `{"what":`} {
		if err := tl.ValidParams(ctx, "ui_inspect", bad); err == nil {
			t.Fatalf("非法参数 %q 被接受", bad)
		}
	}
}

// ---- 无快照：ok:false（不是错误、不占失败轮次）----

func TestUIInspectNoSnapshotIsStateNotError(t *testing.T) {
	m, _ := testManager(t)
	got := callUIInspect(t, m, `{"what":"tabs"}`)
	if got["ok"] != false || got["reason"] != "no_snapshot" {
		t.Fatalf("无快照返回 %v，want {ok:false, reason:no_snapshot}", got)
	}
}

// ---- 有快照：字段齐全、data 按 what 取子对象 ----

func TestUIInspectReturnsSectionAndAlias(t *testing.T) {
	m, _ := testManager(t)
	dispatchUISnapshot(t, m, uiSnapshotPayload())

	all := callUIInspect(t, m, `{"what":"all"}`)
	if all["ok"] != true || all["stale"] != false {
		t.Fatalf("all: ok/stale = %v/%v", all["ok"], all["stale"])
	}
	if _, has := all["ageMs"]; !has {
		t.Fatal("缺 ageMs")
	}
	data := all["data"].(map[string]any)
	for _, k := range []string{"layout", "surfaces", "tabs", "editors", "theme", "viewport", "at"} {
		if _, has := data[k]; !has {
			t.Fatalf("all 缺字段 %q：%v", k, data)
		}
	}
	// value 是 data 的别名（03-code-plan §3.4 的旧名）：两份必须逐字节一致。
	a, _ := json.Marshal(all["data"])
	b, _ := json.Marshal(all["value"])
	if string(a) != string(b) {
		t.Fatalf("data 与 value 不一致：%s vs %s", a, b)
	}

	tabs := callUIInspect(t, m, `{"what":"tabs"}`)
	sec := tabs["data"].(map[string]any)
	list := sec["tabs"].([]any)
	if len(list) != 2 || sec["activeId"] != "t1" {
		t.Fatalf("tabs 段 = %v", sec)
	}
	// what 非法时退化到 all（ValidParams 已拦；直接调用不制造第二种失败形态）。
	fallback := callUIInspect(t, m, `{"what":"nope"}`)
	if fallback["data"].(map[string]any)["layout"] == nil {
		t.Fatal("非法 what 没退化到 all")
	}
}

// ---- 无条件注册（与 office 的「kit 不可用就不注册」正好相反）----

func TestRegisterUIToolsIsUnconditional(t *testing.T) {
	m, _ := testManager(t)
	eng := tools.NewToolEngine()
	m.registerUITools(eng) // 没快照、没渲染层、没 kit
	found := false
	for _, tl := range eng.Tools() {
		if tl.Name() == "ui_inspect" {
			found = true
		}
	}
	if !found {
		t.Fatal("ui_inspect 没被注册 —— 它必须无条件注册（快照缺失用 ok:false 表达）")
	}
}

// ---- D3：run_code 里拿到的是**对象**，不是 JSON 字符串 ----

func TestUIInspectInRunCodeReturnsObject(t *testing.T) {
	// 本机没有 node 时跳过（与 tools/builtin/codemode 的 requireNode 同一判据）；
	// CI 用 GO_CODE_REQUIRE_NODE=1 把「跳过」变成失败，禁止静默绿。
	{
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := runtime.NewNodeRuntime(runtime.NodeConfig{}).Ensure(ctx)
		cancel()
		if err != nil {
			if v := os.Getenv("GO_CODE_REQUIRE_NODE"); v != "" {
				t.Fatalf("本机没有可用 node，且 GO_CODE_REQUIRE_NODE=%q（CI 不允许静默跳过）: %v", v, err)
			}
			t.Skipf("跳过：本机没有可用 node（%v）", err)
		}
	}

	m, _ := testManager(t)
	// 真缓存写入走生产入口（渲染层推快照走的是同一条 dispatch）。
	dispatchUISnapshot(t, m, uiSnapshotPayload())

	dir := t.TempDir()
	eng := tools.NewToolEngine()
	m.registerUITools(eng) // 生产注册函数（不是测试里手搓的工具实例）
	st, err := codemode.OpenStore(filepath.Join(dir, codemode.StoreFileName))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	// 装配与 registerCodemode 一致（Engine/Tools/Exec/Store/Cwd 四项契约）。
	eng.RegisterTool(context.Background(), codemode.New(codemode.Options{
		Engine: eng,
		Tools:  func() []tools.Tool { return eng.VisibleTools() },
		Exec:   execproc.New(nil, codemodeNodeRuntime),
		Store:  st,
		Cwd:    dir,
	}))

	code := `
const s = await tools.ui_inspect({ what: "tabs" });
return {
  isObj: typeof s === "object" && s !== null && typeof s.then !== "function",
  isString: typeof s === "string",
  ok: s.ok === true,
  stale: s.stale,
  n: Array.isArray(s.value?.tabs ?? s.tabs),
  dataIsArray: Array.isArray(s.data?.tabs),
  active: (s.data?.tabs ?? []).filter((t) => t.active).map((t) => t.id),
};
`
	args, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		t.Fatal(err)
	}
	handler := func(context.Context, events.Event) {}
	results, err := eng.RunBatch(context.Background(),
		[]core.ToolCall{{Id: "outer-1", Name: codemode.ToolName, Arguments: string(args)}},
		handler, nil, nil, 0)
	if err != nil {
		t.Fatalf("run_code 执行失败: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d", len(results))
	}
	if results[0].IsError {
		t.Fatalf("脚本被判为错误：%q", results[0].Result)
	}

	got := lastJSONFromText(t, results[0].Result)
	if got["isObj"] != true || got["isString"] != false {
		t.Fatalf("脚本拿到的不是对象：%v（这是最关键的退化形态 —— 返回值变成 JSON 字符串）", got)
	}
	if got["ok"] != true || got["stale"] != false {
		t.Fatalf("脚本侧 ok/stale = %v/%v", got["ok"], got["stale"])
	}
	if ok, _ := got["n"].(bool); !ok {
		t.Fatalf("s.value.tabs 不是数组（n=%v）：结构化结果没走通，退化成字符串了", got["n"])
	}
	if n, _ := got["dataIsArray"].(bool); !n {
		t.Fatal("s.data.tabs 不是数组 —— data 没有按 what 取子对象")
	}
	act, _ := got["active"].([]any)
	if len(act) != 1 || act[0] != "t1" {
		t.Fatalf("active = %v，want [t1]（快照里的 active 标记没传到脚本）", got["active"])
	}
}

// lastJSONFromText 取脚本文本里**整段合法**的 JSON 对象（脚本 return 值的渲染形态）。
//
// 为什么不能按行解析：返回对象是**多行缩进**的（`"isObj": true` 单独一行），
// 按行 Unmarshal 必然找不到。这里做括号配平扫描（字符串内的括号不计），
// 与 tools/builtin/codemode 的 lastJSONObject 同一判据。
func lastJSONFromText(t *testing.T, text string) map[string]any {
	t.Helper()
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		if raw, ok := jsonObjectAt(text[i:]); ok {
			var out map[string]any
			if json.Unmarshal([]byte(raw), &out) == nil {
				return out
			}
		}
	}
	t.Fatalf("结果文本里找不到 JSON 对象:\n%s", text)
	return nil
}

// jsonObjectAt 从 s[0]（'='）起取第一个配平的 JSON 对象。
func jsonObjectAt(s string) (string, bool) {
	depth, inStr, esc := 0, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[:i+1], true
			}
		}
	}
	return "", false
}

// ---- P06 性能预算：ui_inspect 快照生成 ≤ 20ms @ 20 tabs ----
//
// 预算来自 progress.html 的 P06（how: Go bridge 侧打点 + e2e 断言）。这里量的是
// **工具这条路**（引擎实际付的代价：解析 what → 取段 → 序列化），20ms 是 3 个数量级的
// 余量：快照是纯元数据（20 个标签约 2–3 KB），任何接近 20ms 的结果都说明实现变成了
// 别的东西（例如每次调用重新打一遍全量 surfaces 或者把快照落盘）。
func TestUIInspectLatencyBudgetP06(t *testing.T) {
	m, _ := testManager(t)
	payload := uiSnapshotPayload()
	tabs := make([]any, 0, 20)
	for i := 0; i < 20; i++ {
		tabs = append(tabs, map[string]any{
			"id": "t" + strconv.Itoa(i), "kind": "file", "title": "file" + strconv.Itoa(i) + ".go",
			"active": i == 0,
		})
	}
	payload["tabs"] = map[string]any{"scope": "S1", "activeId": "t0", "tabs": tabs}
	dispatchUISnapshot(t, m, payload)

	tl := newUIInspectTool(&m.ui)
	const runs = 200
	durs := make([]time.Duration, 0, runs)
	for i := 0; i < runs; i++ {
		start := time.Now()
		if _, err := tl.Call(context.Background(), "ui-1", `{"what":"all"}`); err != nil {
			t.Fatalf("Call: %v", err)
		}
		durs = append(durs, time.Since(start))
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	p95 := durs[int(float64(len(durs))*0.95)]
	t.Logf("P06 ui_inspect 快照生成：p95 = %v @ 20 tabs（预算 20ms）", p95)
	if p95 > 20*time.Millisecond {
		t.Fatalf("P06 超预算：p95 = %v > 20ms @ 20 tabs", p95)
	}
}
