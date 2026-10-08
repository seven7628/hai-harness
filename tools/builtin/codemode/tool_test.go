package codemode

// tool_test.go：工具**元数据与投影**的单元测试（不跑 node）。
//
// 这一层的价值：凡是「模型看得到的东西」都是字节敏感的（描述进每次请求的
// 工具 schema、片段追加到别的工具的描述上），所以每条渲染路径都要有回归钉住
// 「同输入 ⇒ 同字节」。清单：
//
//   - 可选接口契约（model-only / -1 时限 / 跳过引擎截断 / 单字段 schema）
//   - 目录投影（properties 是 map，必须排序后才渲染）
//   - loadout 片段（只给可编排队列追加、只依赖该条自己的名字、mode:only 不追加）
//   - 描述硬失败时的兜底文本（不能返回空串）
//   - 入参信封（被 JSON 字符串/围栏包住的源码要给出可行动的报错）
//   - §6.5 的结局措辞纪律（拿不到输出时不得承诺 PARTIAL）

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/tools"
)

// ---- 可选接口契约 ----

func TestToolMetadataContract(t *testing.T) {
	tool := New(Options{Engine: nil, Tools: nil})
	if got := tool.Name(); got != ToolName {
		t.Fatalf("Name() = %q, want %q", got, ToolName)
	}
	if got := tool.Exposure(); got != tools.ExposureModelOnly {
		t.Fatalf("Exposure() = %q, want model-only（禁自嵌套：脚本不能再起脚本）", got)
	}
	if !tools.DeclaredToModel(tool) {
		t.Fatal("codemode 必须声明给模型（它是模型写脚本的入口）")
	}
	if tools.CallableFromScript(tool) {
		t.Fatal("codemode 不可被脚本调用（model-only）")
	}
	if got := tool.ToolTimeout(); got != -1 {
		t.Fatalf("ToolTimeout() = %v, want -1（生命周期自管：墙钟 owner 是宿主）", got)
	}
	if !tool.SkipTruncate() {
		t.Fatal("SkipTruncate() = false：输出预算自管的工具会被引擎再砍一刀到 20 KB")
	}
	if tool.CanParallel() {
		t.Fatal("CanParallel() = true：脚本会调写类工具，脚本之间必须保守串行")
	}
	if tool.ReadOnly() {
		t.Fatal("ReadOnly() = true：脚本有真实副作用")
	}
}

// TestParametersSingleCodeField schema 与设计文档 §13 一致：单字段 code；
// 且**禁止**出现 pi 原文那句做不到的隔离承诺（§7.1-3 只能说实话）。
func TestParametersSingleCodeField(t *testing.T) {
	tool := New(Options{})
	schema, ok := tool.Parameters().(map[string]any)
	if !ok {
		t.Fatalf("Parameters() 不是对象 schema: %T", tool.Parameters())
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok || len(props) != 1 {
		t.Fatalf("schema 必须是单字段: %+v", schema)
	}
	code, ok := props["code"].(map[string]any)
	if !ok {
		t.Fatal("缺少 code 字段")
	}
	desc, _ := code["description"].(string)
	for _, want := range []string{
		"Raw JavaScript source", "Top-level await and return work",
		"the value you return becomes this tool's result", "// @options:",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("code 字段文案缺少 %q: %q", want, desc)
		}
	}
	// 隔离承诺：schema 与描述里都**不许**出现 pi 原文的谎言；实话在 DESCRIPTION_INTRO 里。
	whole := strings.ToLower(desc + "\n" + tool.Description())
	for _, lie := range []string{"no file system", "no filesystem", "no network access", "fully isolated"} {
		if strings.Contains(whole, lie) {
			t.Errorf("出现了做不到的隔离承诺 %q（Phase 2 不加 node --permission）", lie)
		}
	}
	if !strings.Contains(tool.Description(), "its own process with an allowlisted environment") {
		t.Error("描述里应如实写明「独立进程 + 环境白名单 + 配额」")
	}
}

// TestOutputSchemaShape 结构化契约要有宿主需要的字段（spill 路径点名/结局/用量）。
func TestOutputSchemaShape(t *testing.T) {
	tool := New(Options{})
	out, ok := tool.OutputSchema().(map[string]any)
	if !ok {
		t.Fatalf("OutputSchema() 不是对象: %T", tool.OutputSchema())
	}
	props, _ := out["properties"].(map[string]any)
	for _, key := range []string{"ok", "truncated", "full_output_path", "exit_code", "timed_out", "canceled", "wall_time_seconds", "nested_calls"} {
		if _, ok := props[key]; !ok {
			t.Errorf("OutputSchema 缺少字段 %q（宿主/前端按它取数）", key)
		}
	}
}

// ---- 目录投影 ----

func TestSnapshotProjectsParamsInStableOrder(t *testing.T) {
	t.Parallel()
	multi := &tierStub{
		BaseTool: tools.BaseTool{
			Name_: "multi", Description_: "first line summary\nsecond line ignored",
			Params_: tools.Obj(map[string]any{
				"zeta":  tools.Str("z"),
				"alpha": tools.Str("a"),
				"mid":   tools.Int("m"),
			}, "zeta", "alpha"),
		},
		exp: tools.ExposureCodemode,
	}
	tool := New(Options{Tools: func() []tools.Tool { return []tools.Tool{multi} }})

	snap := tool.snapshot()
	if len(snap.catalog) != 1 {
		t.Fatalf("catalog = %+v", snap.catalog)
	}
	got := snap.catalog[0]
	// zeta 与 alpha 在 required 里（Optional=false），mid 不在（Optional=true）。
	want := []Param{{Name: "alpha"}, {Name: "mid", Optional: true}, {Name: "zeta"}}
	if len(got.Params) != len(want) {
		t.Fatalf("params = %+v, want %+v", got.Params, want)
	}
	for i := range want {
		if got.Params[i] != want[i] {
			t.Fatalf("params[%d] = %+v, want %+v（properties 是 map，必须先排序）", i, got.Params[i], want[i])
		}
	}
	if got.Summary != "first line summary" {
		t.Fatalf("摘要应取首行: %q", got.Summary)
	}
	// 两次投影必须逐字节一致（描述进每次请求的 schema，抖动即缓存击穿）。
	first, err := tool.catalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	second, err := tool.catalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if first.Description != second.Description {
		t.Fatalf("两次描述不同:\n--- 1\n%s\n--- 2\n%s", first.Description, second.Description)
	}
}

// TestSnapshotSkipsCodemodeItself 目录不得包含 codemode 自己（列举自己在语义上是循环）。
func TestSnapshotSkipsCodemodeItself(t *testing.T) {
	t.Parallel()
	self := New(Options{})
	other := &tierStub{BaseTool: tools.BaseTool{Name_: "other", Description_: "other", Params_: map[string]any{}},
		exp: tools.ExposureDirect}
	tool := New(Options{Tools: func() []tools.Tool { return []tools.Tool{self, other} }})
	cat, err := tool.catalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if _, ok := cat.Names[ToolName]; ok {
		t.Fatal("目录表含 codemode 自己")
	}
}

// ---- loadout 片段 ----

func TestPrepareLoadoutAppendsOnlyToCallableTools(t *testing.T) {
	t.Parallel()
	direct := &tierStub{BaseTool: tools.BaseTool{Name_: "read_file", Description_: "read", Params_: map[string]any{}},
		exp: tools.ExposureDirect}
	deferred := &tierStub{BaseTool: tools.BaseTool{Name_: "mcp__dev__x", Description_: "mcp", Params_: map[string]any{}},
		exp: tools.ExposureDeferred}
	hidden := &tierStub{BaseTool: tools.BaseTool{Name_: "gone", Description_: "gone", Params_: map[string]any{}},
		exp: tools.ExposureHidden}
	modelOnly := &tierStub{BaseTool: tools.BaseTool{Name_: "ghost", Description_: "ghost", Params_: map[string]any{}},
		exp: tools.ExposureModelOnly}
	hyphen := &tierStub{BaseTool: tools.BaseTool{Name_: "read-file", Description_: "hyphen", Params_: map[string]any{}},
		exp: tools.ExposureDirect}
	list := []tools.Tool{direct, deferred, hidden, modelOnly, hyphen}
	tool := New(Options{Tools: func() []tools.Tool { return list }})

	declared := []core.ToolSchema{
		{Name: "read_file", Description: "read"},
		{Name: "mcp__dev__x", Description: "mcp"},
		{Name: "gone", Description: "gone"},
		{Name: "ghost", Description: "ghost"},
		{Name: "read-file", Description: "hyphen"},
		{Name: ToolName, Description: DESCRIPTION_INTRO},
	}
	out := tool.PrepareLoadout(context.Background(), declared)
	if len(out) != len(declared) {
		t.Fatalf("loadout 不得增删条目: %d → %d", len(declared), len(out))
	}
	got := map[string]string{}
	for _, s := range out {
		got[s.Name] = s.Description
	}
	if !strings.Contains(got["read_file"], "tools.read_file({...})") {
		t.Errorf("direct 工具必须被追加脚本调用片段: %q", got["read_file"])
	}
	if !strings.Contains(got["mcp__dev__x"], "tools.mcp__dev__x({...})") {
		t.Errorf("deferred 工具也可编排，描述同样要追加: %q", got["mcp__dev__x"])
	}
	// 需归一化的名字：片段里用归一 + 后缀后的脚本名（与目录表一致）。
	want := scriptNameFor("read-file")
	if want == "read-file" || !strings.Contains(got["read-file"], "tools."+want+"({...})") {
		t.Errorf("连字符名字的片段应写脚本名 %q: %q", want, got["read-file"])
	}
	for _, name := range []string{"gone", "ghost", ToolName} {
		if got[name] != descOf(declared, name) {
			t.Errorf("%s 不可编排，描述不该被改: %q", name, got[name])
		}
	}
	// 幂等/字节稳定：两次调用结果相同。
	again := tool.PrepareLoadout(context.Background(), declared)
	if !equalSchemas(out, again) {
		t.Fatal("PrepareLoadout 两次结果不同")
	}
}

// TestPrepareLoadoutModeOnlyAppendsNothing mode:"only" 下模型只拿 codemode，
// 已声明工具（即将被隐藏）不该再被追加片段。
func TestPrepareLoadoutModeOnlyAppendsNothing(t *testing.T) {
	t.Parallel()
	direct := &tierStub{BaseTool: tools.BaseTool{Name_: "read_file", Description_: "read", Params_: map[string]any{}},
		exp: tools.ExposureDirect}
	tool := New(Options{
		Tools: func() []tools.Tool { return []tools.Tool{direct} },
		Mode:  func() Mode { return ModeOnly },
	})
	if out := tool.PrepareLoadout(context.Background(), []core.ToolSchema{{Name: "read_file", Description: "read"}}); out != nil {
		t.Fatalf("mode:only 不该改写任何描述: %+v", out)
	}
	// 而且 only 模式下 direct 工具要进描述（模型看不到它们了，目录是唯一入口）。
	cat, err := tool.catalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if !strings.Contains(cat.Description, "read_file(") {
		t.Fatalf("mode:only 的描述必须列举 direct 工具:\n%s", cat.Description)
	}
}

// TestScriptNameForMatchesCatalogNames 片段里写的脚本名必须与目录表给的名字一致
// （唯一例外是 BuildCatalog 的计数器兜底，那条另有说明）。
func TestScriptNameForMatchesCatalogNames(t *testing.T) {
	t.Parallel()
	names := []string{"read_file", "read-file", "mcp__dev-radius__get", "1password", "a.b", "x"}
	var list []tools.Tool
	for _, n := range names {
		list = append(list, &tierStub{
			BaseTool: tools.BaseTool{Name_: n, Description_: n, Params_: map[string]any{}},
			exp:      tools.ExposureDirect,
		})
	}
	tool := New(Options{Tools: func() []tools.Tool { return list }})
	cat, err := tool.catalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if len(cat.RawNames) != len(names) {
		t.Fatalf("目录表条目数 = %d, want %d（归一撞名必须各自可调）", len(cat.RawNames), len(names))
	}
	for raw, script := range cat.RawNames {
		if want := scriptNameFor(raw); script != want {
			t.Errorf("raw %q 的目录名 = %q, 片段会写 %q —— 两者必须一致（除计数器兜底外）", raw, script, want)
		}
	}
}

// ---- 描述硬失败 ----

// TestDescriptionFallsBackWhenCatalogFails：BuildCatalog 硬失败（raw 名重复）时，
// 描述不能返回空串（模型会看到一个没说明的工具），Call 侧则必须硬失败（那里有 error 通道）。
func TestDescriptionFallsBackWhenCatalogFails(t *testing.T) {
	t.Parallel()
	dup := func() []tools.Tool {
		return []tools.Tool{
			&tierStub{BaseTool: tools.BaseTool{Name_: "same", Description_: "a", Params_: map[string]any{}},
				exp: tools.ExposureDirect},
			&tierStub{BaseTool: tools.BaseTool{Name_: "same", Description_: "b", Params_: map[string]any{}},
				exp: tools.ExposureDirect},
		}
	}
	tool := New(Options{Tools: dup, Exec: nil})
	desc := tool.Description()
	if !strings.HasPrefix(desc, DESCRIPTION_INTRO) || !strings.Contains(desc, "the tool catalog is unavailable") {
		t.Fatalf("硬失败时的描述兜底不对:\n%s", desc)
	}
	if _, err := tool.Call(context.Background(), ToolName, `{"code":"return 1;"}`); err == nil {
		t.Fatal("目录建不起来时 Call 必须硬失败（不能进沙箱）")
	}
}

// ---- 入参信封 ----

func TestParseInputRejectsWrappedSource(t *testing.T) {
	t.Parallel()
	// parseInput 只负责「信封」：合法 JSON 解出 code 就算过（空 code 由 ValidParams/Call 拦）。
	for _, c := range []struct {
		name string
		args string
		want string
	}{
		{"对象里塞字符串（正常形态）", `{"code":"return 1;"}`, ""},
		{"空对象（信封对、内容空）", `{}`, ""},
		{"源码被 JSON 字符串包住", `"return 1;"`, "JSON object"},
		{"整体是数组", `["return 1;"]`, "JSON object"},
		{"顶层就是源码", `return 1;`, "JSON object"},
		{"围栏包住（非 JSON）", "```js\nreturn 1;\n```", "JSON object"},
	} {
		_, err := parseInput(c.args)
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: 不该报错: %v", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: 应当报错", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: 报错文案缺少 %q: %v", c.name, c.want, err)
		}
	}
}

func TestValidParams(t *testing.T) {
	t.Parallel()
	tool := New(Options{})
	if err := tool.ValidParams(context.Background(), ToolName, `{"code":"return 1;"}`); err != nil {
		t.Fatalf("合法入参被判非法: %v", err)
	}
	for _, args := range []string{`{}`, `{"code":"   "}`, `not json`} {
		if err := tool.ValidParams(context.Background(), ToolName, args); err == nil {
			t.Errorf("%s 应当判为非法参数", args)
		}
	}
}

// ---- 结局措辞（§6.5）----

func TestRetimeoutBodyKeepsWordingDiscipline(t *testing.T) {
	t.Parallel()
	note := stripCancelledNote("[CANCELLED — the script was interrupted before finishing; the output below is PARTIAL]\nreal output")
	if !strings.HasPrefix(note, "real output") {
		t.Fatalf("剥前缀失败: %q", note)
	}
	// 没有真拿到输出：不得承诺 PARTIAL。
	noOut := retimeoutBody("[CANCELLED — …nothing could be recovered]\n[exit: context canceled]", 700*time.Millisecond, false)
	if !strings.HasPrefix(noOut, "[TIMEOUT after 700ms") {
		t.Fatalf("首行必须是 TIMEOUT 声明: %q", noOut)
	}
	if strings.Contains(noOut, "the output below is PARTIAL") {
		t.Fatalf("拿不到输出时不得承诺 PARTIAL: %q", noOut)
	}
	if !strings.Contains(noOut, "nothing could be recovered") || !strings.Contains(noOut, "[exit: context canceled]") {
		t.Fatalf("说明与 sandbox 注记都该保留: %q", noOut)
	}
	// 真拿到了输出：可以承诺 PARTIAL。
	withOut := retimeoutBody("[CANCELLED — …the output below is PARTIAL]\npartial line", time.Second, true)
	if !strings.Contains(withOut, "the output below is PARTIAL") || !strings.Contains(withOut, "partial line") {
		t.Fatalf("有输出时应承诺 PARTIAL 并保留输出: %q", withOut)
	}
	if !strings.HasPrefix(withOut, "[TIMEOUT after 1s") {
		t.Fatalf("首行必须是 TIMEOUT 声明: %q", withOut)
	}
}

func TestRecoveredOutputReadsResultFrames(t *testing.T) {
	t.Parallel()
	frame := func(out, value string) string {
		f := map[string]any{"notify": "result", "out": out}
		if value != "" {
			f["value"] = json.RawMessage(value)
		}
		b, _ := json.Marshal(f)
		return frameSentinelForTest + string(b) + "\n"
	}
	if recoveredOutput("[exit: 0]\n") {
		t.Fatal("只有 sandbox 注记时不该认为拿到了输出")
	}
	if !recoveredOutput(frame("hello", "") + "[exit: 0]\n") {
		t.Fatal("result 帧带 out 时应认为拿到了输出")
	}
	if !recoveredOutput(frame("", `{"a":1}`)) {
		t.Fatal("result 帧带 value 时也应认为拿到了成果")
	}
}

// ---- 渲染 ----

func TestRenderValue(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{``, ``},
		{`"plain"`, `plain`},
		{`42`, `42`},
		{`true`, `true`},
		{`null`, `null`},
		{`{"a":1}`, "{\n  \"a\": 1\n}"},
	}
	for _, c := range cases {
		if got := renderValue(json.RawMessage(c.in)); got != c.want {
			t.Errorf("renderValue(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- 描述正文与代码默认值必须一致（D3 的收口）----

// TestDescriptionIntroTimeoutDefaultMatchesCode 教学正文里声明的 timeout_ms 默认值必须
// 与 DefaultScriptTimeoutMs 一致。
//
// 背景（D3）：`timeout_ms` 未声明 = 不限时 是**真缺陷**，已修为「默认 30s」；但正文里
// 那句 "default: unlimited" 当时漏改 —— 模型会以为自己没有墙钟上限，而宿主 30s 就杀。
// 模型面的文案与代码不一致，属于「说实话」这条底线的破坏，故钉住。
func TestDescriptionIntroTimeoutDefaultMatchesCode(t *testing.T) {
	t.Parallel()
	ms := DefaultScriptTimeoutMs // 无类型常量，单位是毫秒（用它的地方都写成 … × time.Millisecond）
	if ms != 30000 || time.Duration(ms)*time.Millisecond != 30*time.Second {
		t.Fatalf("DefaultScriptTimeoutMs 变了（%dms）：请同步 DESCRIPTION_INTRO 的措辞与本用例", ms)
	}
	if got := (ScriptOptions{}).Timeout(); got != 30*time.Second {
		t.Fatalf("未声明 timeout_ms 时生效的墙钟 = %v, want 30s", got)
	}
	if !strings.Contains(DESCRIPTION_INTRO, "default: 30000") {
		t.Fatalf("DESCRIPTION_INTRO 未声明真实默认值（%dms）—— 模型会以为自己没有墙钟上限", ms)
	}
	if strings.Contains(DESCRIPTION_INTRO, "default: unlimited") {
		t.Fatal("DESCRIPTION_INTRO 仍写着 default: unlimited（代码是 30s —— 模型面文案不许说谎）")
	}
}

// ---- 检索（宿主侧，纯函数）----

func TestSearchScoringIsDeterministicAndWeighted(t *testing.T) {
	t.Parallel()
	read := &tierStub{BaseTool: tools.BaseTool{Name_: "read_file", Description_: "Read a file", Params_: map[string]any{}},
		exp: tools.ExposureDirect}
	write := &tierStub{BaseTool: tools.BaseTool{Name_: "write_file", Description_: "Write a file", Params_: map[string]any{}},
		exp: tools.ExposureDirect}
	remote := &tierStub{BaseTool: tools.BaseTool{Name_: "mcp__dev__read-thing", Description_: "remote read", Params_: map[string]any{}},
		exp: tools.ExposureDeferred}
	gone := &tierStub{BaseTool: tools.BaseTool{Name_: "read_gone", Description_: "Read (retired)", Params_: map[string]any{}},
		exp: tools.ExposureHidden}
	list := []tools.Tool{read, write, remote, gone}
	tool := New(Options{Tools: func() []tools.Tool { return list }})
	snap := tool.snapshot()
	cat, err := BuildCatalog(DescribeOptions{Tools: snap.catalog})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	b := &bridge{cat: cat, byRaw: snap.byRaw, ns: snap.ns}

	hits := b.search("read", defaultSearchLimit)
	if len(hits) == 0 {
		t.Fatal("search(read) 无命中")
	}
	for _, h := range hits {
		if h.Name == "read_gone" {
			t.Fatal("检索不得返回 hidden 工具")
		}
	}
	if hits[0].Name != "read_file" {
		t.Fatalf("命中排序 = %+v, want read_file 第一（名字命中权重最高）", hits)
	}
	if again := b.search("read", defaultSearchLimit); len(again) != len(hits) || again[0].Name != hits[0].Name {
		t.Fatal("检索结果不稳定（必须确定性排序）")
	}
	if got := b.search("read", 1); len(got) != 1 {
		t.Fatalf("limit 未生效: %+v", got)
	}
	if got := b.search("", defaultSearchLimit); len(got) != 0 {
		t.Fatalf("空查询不该有命中: %+v", got)
	}
	if got := b.search("read_file", defaultSearchLimit); len(got) == 0 || got[0].Name != "read_file" {
		t.Fatalf("下划线应作为分隔符切分: %+v", got)
	}
}

// frameSentinelForTest 协议行哨兵（与 execproc/prelude.js 同源的三处之一；测试里自己
// 拼一行帧来喂 recoveredOutput）。
const frameSentinelForTest = "\x1e"

// descOf 取 declared 里某条的名字对应描述（测试断言用）。
func descOf(list []core.ToolSchema, name string) string {
	for _, s := range list {
		if s.Name == name {
			return s.Description
		}
	}
	return ""
}
