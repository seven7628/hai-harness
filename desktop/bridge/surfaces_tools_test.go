// surfaces_tools_test.go —— `surface_install` / `surface_remove` / `surface_list`（PR-13 B）。
//
// 与 ui_tools_test.go 同一套测试法，理由也一样：`desktop/bridge` 是 `package main`，
// 别的包 import 不了它，而工具与 store 都住在这里。所以这里用真引擎
// （tools.NewToolEngine + codemode.New + RunBatch），注册走生产入口
// （m.registerSurfaceTools），磁盘走生产入口（HOME → appDataDir → surfaces/）。
//
// 三条最容易退化的路径各有一条断言：
//  1. 脚本里拿到的是**对象**不是 JSON 字符串（结构化槽没写 → 退化成 string）；
//  2. 校验失败是 `status:"rejected"` 的**正常返回**，不是 Go error（模型不会白重试一轮）；
//  3. 审批拒绝时工具**根本没被调用**（index 不存在 = 磁盘上没发生任何事）。
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

// ---- schema / 审批声明（常驻工具的成本与安全契约）----

func TestSurfaceToolsSchemaAndApproval(t *testing.T) {
	st := newSurfaceStore("")
	install := newSurfaceInstallTool(st, nil)
	remove := newSurfaceRemoveTool(st, nil)
	list := newSurfaceListTool(st)

	for _, c := range []struct {
		name    string
		tl      tools.Tool
		wantPar bool
		wantRO  bool
		approve bool
	}{
		{"surface_install", install, false, false, true},
		{"surface_remove", remove, false, false, true},
		{"surface_list", list, true, true, false},
	} {
		if got := c.tl.Name(); got != c.name {
			t.Fatalf("Name() = %q，want %q", got, c.name)
		}
		if n := len([]rune(c.tl.Description())); n > 120 {
			t.Fatalf("%s 描述 %d 字，超过 120 字常驻成本约束：%q", c.name, n, c.tl.Description())
		}
		params, _ := c.tl.Parameters().(map[string]any)
		props, _ := params["properties"].(map[string]any)
		if len(props) > 3 {
			t.Fatalf("%s 参数 %d 个，超过 3 个上限", c.name, len(props))
		}
		if c.tl.CanParallel() != c.wantPar {
			t.Fatalf("%s CanParallel = %v，want %v", c.name, c.tl.CanParallel(), c.wantPar)
		}
		ro, ok := c.tl.(tools.ReadOnlyTool)
		if !ok {
			t.Fatalf("%s 没实现 ReadOnlyTool（模式过滤要靠它）", c.name)
		}
		if ro.ReadOnly() != c.wantRO {
			t.Fatalf("%s ReadOnly = %v，want %v", c.name, ro.ReadOnly(), c.wantRO)
		}
		ar, ok := c.tl.(tools.ApprovalRequired)
		if !ok {
			t.Fatalf("%s 没实现 ApprovalRequired（审批是这条工具的边界）", c.name)
		}
		if got := ar.RequiresApproval(context.Background(), core.ToolCall{Name: c.name}); got != c.approve {
			t.Fatalf("%s RequiresApproval = %v，want %v", c.name, got, c.approve)
		}
		if _, ok := c.tl.(tools.OutputSchemaProvider); !ok {
			t.Fatalf("%s 没实现 OutputSchemaProvider（脚本路径拿不到对象）", c.name)
		}
	}
}

func TestSurfaceToolsRegisteredUnconditionally(t *testing.T) {
	m, _ := testManager(t)
	eng := tools.NewToolEngine()
	m.registerSurfaceTools(eng) // 没有渲染层、没有 kit、没有快照
	got := map[string]bool{}
	for _, tl := range eng.Tools() {
		got[tl.Name()] = true
	}
	for _, want := range []string{"surface_install", "surface_remove", "surface_list"} {
		if !got[want] {
			t.Fatalf("%s 没被注册 —— 三个工具必须无条件注册（开关留给 PR-14）", want)
		}
	}
}

// ---- 脚本路径：拿到对象、失败不抛异常 ----

func requireNodeForSurfaces(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := runtime.NewNodeRuntime(runtime.NodeConfig{}).Ensure(ctx); err != nil {
		if v := os.Getenv("GO_CODE_REQUIRE_NODE"); v != "" {
			t.Fatalf("本机没有可用 node，且 GO_CODE_REQUIRE_NODE=%q（CI 不允许静默跳过）: %v", v, err)
		}
		t.Skipf("跳过：本机没有可用 node（%v）", err)
	}
}

// runSurfaceScript 用真引擎跑一段 run_code（注册走 m.registerSurfaceTools）。
func runSurfaceScript(t *testing.T, m *manager, code string) map[string]any {
	t.Helper()
	eng := tools.NewToolEngine()
	m.registerSurfaceTools(eng)
	st, err := codemode.OpenStore(filepath.Join(t.TempDir(), codemode.StoreFileName))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	eng.RegisterTool(context.Background(), codemode.New(codemode.Options{
		Engine: eng,
		Tools:  func() []tools.Tool { return eng.VisibleTools() },
		Exec:   execproc.New(nil, codemodeNodeRuntime),
		Store:  st,
		Cwd:    t.TempDir(),
	}))
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
		t.Fatalf("脚本被判为错误（校验失败不该抛异常）：%q", results[0].Result)
	}
	return lastJSONFromText(t, results[0].Result)
}

func TestSurfaceInstallInRunCodeReturnsObject(t *testing.T) {
	requireNodeForSurfaces(t)
	m, _ := testManager(t)
	src := surfaceFixture(t, "cost-board", "0.1.0", "export default () => null\n")

	code := `
const r = await tools.surface_install({ target: ` + quoteJS(src) + ` });
return {
  isObj: typeof r === "object" && r !== null && typeof r.then !== "function",
  isString: typeof r === "string",
  status: r.status,
  id: r.id,
  hasHash: typeof r.hash === "string" && r.hash.length === 64,
};
`
	got := runSurfaceScript(t, m, code)
	if got["isObj"] != true || got["isString"] != false {
		t.Fatalf("脚本拿到的不是对象：%v（结构化槽没写 → 退化成字符串）", got)
	}
	if got["status"] != string(surfaceApplied) {
		t.Fatalf("status = %v，want applied", got["status"])
	}
	if got["id"] != "cost-board" {
		t.Fatalf("id = %v", got["id"])
	}
	if got["hasHash"] != true {
		t.Fatalf("回执缺 sha256：%v", got)
	}
	// 真的写进了磁盘（不是只在内存里回了一个对象）。
	idx := readBytes(t, filepath.Join(m.surfaces.Dir(), "index.json"))
	if !strings.Contains(string(idx), "cost-board") {
		t.Fatalf("index.json 里没有 cost-board：%s", idx)
	}
}

func TestSurfaceInstallRejectedInRunCodeDoesNotThrow(t *testing.T) {
	requireNodeForSurfaces(t)
	m, _ := testManager(t)

	code := `
const r = await tools.surface_install({ target: "/nonexistent/surface-fixture" });
return { status: r.status, reason: r.reason, isObj: typeof r === "object" };
`
	got := runSurfaceScript(t, m, code)
	if got["status"] != string(surfaceRejected) {
		t.Fatalf("status = %v，want rejected（校验失败是可编程状态，不是抛异常）", got["status"])
	}
	if reason, _ := got["reason"].(string); reason == "" {
		t.Fatalf("rejected 必须带可读 reason：%v", got)
	}
	// 失败没写 index：文件都不该存在。
	notExist(t, filepath.Join(m.surfaces.Dir(), "index.json"))
}

// ---- 审批：真引擎的审批路径（拒绝 → 工具没被调用、index 没写）----

// surfaceDenyApprover 复刻会话侧的「用户拒绝」：wait 返回 (false, nil)。
type surfaceDenyApprover struct{}

func (surfaceDenyApprover) BeginApproval(core.ToolCall) func(context.Context) (bool, error) {
	return func(context.Context) (bool, error) { return false, nil }
}

// surfaceAllowApprover 批准（证明审批不是「永久拒绝」）。
type surfaceAllowApprover struct{}

func (surfaceAllowApprover) BeginApproval(core.ToolCall) func(context.Context) (bool, error) {
	return func(context.Context) (bool, error) { return true, nil }
}

func TestSurfaceInstallApprovalDeniedDoesNotWriteIndex(t *testing.T) {
	m, _ := testManager(t)
	src := surfaceFixture(t, "cost-board", "0.1.0", "export default () => null\n")

	eng := tools.NewToolEngine()
	m.registerSurfaceTools(eng) // 生产注册函数（不是测试里手搓的工具实例）

	handler := func(context.Context, events.Event) {}
	ctx := events.WithToolContext(context.Background(), "run-approve", 0, handler, surfaceDenyApprover{}, nil)
	args, _ := json.Marshal(map[string]any{"target": src})
	res, err := eng.Sequence(ctx, []core.ToolCall{{Id: "c1", Name: "surface_install", Arguments: string(args)}}, handler)
	if err != nil {
		t.Fatalf("Sequence: %v", err)
	}
	if len(res) != 1 || !res[0].IsError {
		t.Fatalf("拒绝审批后仍然执行了：%+v", res)
	}
	if !strings.Contains(res[0].Result, "rejected") {
		t.Fatalf("拒绝措辞必须含 rejected（脚本按这个子串分支）：%q", res[0].Result)
	}
	// 工具没被调用 = 磁盘上什么都没发生。
	notExist(t, filepath.Join(m.surfaces.Dir(), "index.json"))
	notExist(t, filepath.Join(m.surfaces.Dir(), "cost-board"))

	// 同一条路径批准后必须真的装上（别把审批做成永久拒绝）。
	ctx2 := events.WithToolContext(context.Background(), "run-approve", 0, handler, surfaceAllowApprover{}, nil)
	res2, err := eng.Sequence(ctx2, []core.ToolCall{{Id: "c2", Name: "surface_install", Arguments: string(args)}}, handler)
	if err != nil {
		t.Fatalf("Sequence(allow): %v", err)
	}
	if len(res2) != 1 || res2[0].IsError {
		t.Fatalf("批准后仍失败：%+v", res2)
	}
	if _, err := os.Stat(filepath.Join(m.surfaces.Dir(), "index.json")); err != nil {
		t.Fatalf("批准后 index 没写: %v", err)
	}
}

func TestSurfaceRemoveRequiresApprovalToo(t *testing.T) {
	m, _ := testManager(t)
	src := surfaceFixture(t, "cost-board", "0.1.0", "export default () => null\n")
	if r := m.surfaces.Install(src, true); r.Status != surfaceApplied {
		t.Fatalf("安装: %+v", r)
	}
	eng := tools.NewToolEngine()
	m.registerSurfaceTools(eng)
	handler := func(context.Context, events.Event) {}
	ctx := events.WithToolContext(context.Background(), "run-remove", 0, handler, surfaceDenyApprover{}, nil)
	res, err := eng.Sequence(ctx, []core.ToolCall{
		{Id: "c1", Name: "surface_remove", Arguments: `{"id":"cost-board"}`},
	}, handler)
	if err != nil {
		t.Fatalf("Sequence: %v", err)
	}
	if len(res) != 1 || !res[0].IsError {
		t.Fatalf("拒绝审批后仍然卸载了：%+v", res)
	}
	if _, err := os.Stat(filepath.Join(m.surfaces.Dir(), "cost-board")); err != nil {
		t.Fatalf("拒绝审批却动了目录: %v", err)
	}
}

// ---- 事件：装/卸后推 surfaces_changed（PR-12 通道，形状不改）----

func TestSurfaceInstallPushesSurfacesChanged(t *testing.T) {
	m, buf := testManager(t)
	src := surfaceFixture(t, "cost-board", "0.1.0", "export default () => null\n")

	eng := tools.NewToolEngine()
	m.registerSurfaceTools(eng)
	handler := func(context.Context, events.Event) {}
	args, _ := json.Marshal(map[string]any{"target": src})
	if _, err := eng.Sequence(context.Background(), []core.ToolCall{
		{Id: "c1", Name: "surface_install", Arguments: string(args)},
	}, handler); err != nil {
		t.Fatalf("Sequence: %v", err)
	}

	found := false
	for _, line := range readLines(buf) {
		if line["type"] == "ui" && line["op"] == "surfaces_changed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("装上之后没推 surfaces_changed（PR-12 通道是唯一通知路径）")
	}
}

// quoteJS 把字符串编成 JS 字面量（脚本里当 target 用）。
func quoteJS(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
