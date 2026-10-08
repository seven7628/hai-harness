package codemode

// descriptions_test.go：描述生成 + 命名映射的回归网。重点是**缓存不变量**与
// **预算不变量**这两类「看起来能跑但会悄悄退化」的行为：
//   - 同一工具集两次生成字节完全一致（provider 前缀缓存的前提）；
//   - deferred 永不进描述、hidden 彻底不可见（pi #10212 那类 bug）；
//   - 加/删一个 MCP 工具不改变其他条目的字节；
//   - 归一撞名必须命中不同实现（pi #10239）；
//   - 超预算按轮转公平装箱裁剪，每个 namespace 至少出一条。

import (
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/tools"
)

func mkTool(name, summary, ns string, exp tools.ToolExposure, params ...Param) CatalogTool {
	return CatalogTool{Name: name, Summary: summary, Namespace: ns, Exposure: exp, Params: params}
}

// ---- 描述文本常量 ----

// TestDescriptionIntroContract 教学正文必须覆盖规格要求它讲的事，且**不许**出现
// pi 原文那句做不到的隔离承诺（§7.1-3）。
func TestDescriptionIntroContract(t *testing.T) {
	must := []struct{ name, needle string }{
		{"说明是 JS 源码而非 JSON", "raw JavaScript source"},
		{"顶层 await/return 合法", "Top-level \"await\" and \"return\" work"},
		{"首行预算声明", `// @options: {"max_output_tokens": 4000, "timeout_ms": 60000}`},
		{"两个字段名与取值域", "max_output_tokens caps the output sent back to you (0-200000, default 10000)"},
		{"时限上限 1 小时", "at most 1 hour"},
		{"副作用不回滚", "NOT\n  rolled back"},
		{"store 用法", "store(key, value)"},
		{"load 用法", "load(key) / load()"},
		{"store 限额", "256 KiB of JSON and the whole store to 1 MiB"},
		{"仅成功脚本保留写入", "Writes are kept only when the script finishes"},
		{"deferred 现查", "searchTools(query)"},
		{"双键（raw 名）", `tools["mcp__some-server__some-tool"](args)`},
		{"说实话的隔离承诺", "its own process with an allowlisted environment"},
	}
	for _, c := range must {
		if !strings.Contains(DESCRIPTION_INTRO, c.needle) {
			t.Errorf("DESCRIPTION_INTRO 缺少 %s（找不到 %q）", c.name, c.needle)
		}
	}
	forbidden := []string{
		"no file system", "no filesystem", "no network access", "no network",
		"cannot access the network", "fully isolated", "sandboxed away",
	}
	for _, f := range forbidden {
		if strings.Contains(strings.ToLower(DESCRIPTION_INTRO), f) {
			t.Errorf("DESCRIPTION_INTRO 出现了做不到的隔离承诺 %q —— Phase 2 不加 node --permission，只能承诺进程隔离+环境白名单+配额", f)
		}
	}
}

// TestDescriptionIntroBudget 教学正文与目录共享 3000 token 预算。正文一旦膨胀，
// 目录就会被悄悄挤空（症状是「模型看不到工具」而不是报错），所以钉一个上限：
// 正文不得超过预算的 25%。
func TestDescriptionIntroBudget(t *testing.T) {
	const maxShare = DefaultInlineBudget / 4
	if got := estimateTokens(DESCRIPTION_INTRO); got > maxShare {
		t.Errorf("DESCRIPTION_INTRO 估算 %d token > 预算的 25%%（%d）；目录只剩 %d token，请精简正文或调大 DefaultInlineBudget",
			got, maxShare, DefaultInlineBudget-got)
	}
	if got := estimateTokens(DEFERRED_TOOLS_GUIDANCE); got > maxShare/2 {
		t.Errorf("DEFERRED_TOOLS_GUIDANCE 估算 %d token，过长（上限 %d）", got, maxShare/2)
	}
}

// ---- normalize ----

func TestNormalize(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"read_file", "read_file"},
		{"mcp__dev-radius__get", "mcp__dev_radius__get"},
		{"read-file", "read_file"},
		{"1password", "_password"}, // 首字符非数字：整体替换（与「非法字符一律变 _」同一条规则）
		{"a b", "a_b"},
		{"$dollar", "$dollar"},
		{"", "_"},
		{"工具名", "工具名"}, // 字母类字符保留（JS 标识符允许 Unicode 字母）
		{"a.b/c:d", "a_b_c_d"},
	}
	for _, c := range cases {
		if got := normalize(c.raw); got != c.want {
			t.Errorf("normalize(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// ---- 撞名（pi #10239）----

// TestBuildCatalogCollisionHitsDifferentImplementations 归一后撞名的两个工具必须
// 分别拿到**不同**的脚本名，且映射表指回各自的 raw 名 —— 否则 read-file 的调用
// 会打到 read_file 的实现上（静默错实现，比报错难查一百倍）。
func TestBuildCatalogCollisionHitsDifferentImplementations(t *testing.T) {
	cat, err := BuildCatalog(DescribeOptions{
		Tools: []CatalogTool{
			mkTool("read-file", "带连字符的实现", "", tools.ExposureCodemode),
			mkTool("read_file", "带下划线的实现", "", tools.ExposureCodemode),
		},
	})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	dash := cat.RawNames["read-file"]
	under := cat.RawNames["read_file"]
	if dash == under {
		t.Fatalf("撞名未消解：read-file 与 read_file 都映射到 %q", dash)
	}
	// 后缀判定与「谁在场」无关（纯函数）：需要归一的 `read-file` 带自己的 fnv32 后缀；
	// 本身就是合法标识符的 `read_file` **保持原名** —— 否则一个 deferred 工具的上线/下线
	// 会给这个无关的已列举条目改名，描述字节随 MCP 连接抖动（复核缺陷 D1）。
	if !strings.HasPrefix(dash, "read_file_") || len(dash) != len("read_file_")+6 {
		t.Errorf("需归一的脚本名 %q 应为 read_file_ + fnv32 前 6 位", dash)
	}
	if under != "read_file" {
		t.Errorf("合法标识符必须保持原名（不随他人进出而变），got %q", under)
	}
	if got := cat.Names[dash]; got != "read-file" {
		t.Errorf("Names[%q] = %q, want read-file", dash, got)
	}
	if got := cat.Names[under]; got != "read_file" {
		t.Errorf("Names[%q] = %q, want read_file", under, got)
	}
	// 未撞名的工具保持归一形态，不白加后缀（否则每个工具的脚本名都变，缓存全废）。
	cat2, err := BuildCatalog(DescribeOptions{
		Tools: []CatalogTool{mkTool("read_file", "唯一的实现", "", tools.ExposureCodemode)},
	})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	if got := cat2.RawNames["read_file"]; got != "read_file" {
		t.Errorf("无撞名时脚本名 = %q, want read_file", got)
	}
}

// TestBuildCatalogNameMapCoversCallableTiers 映射表覆盖「可编排」全集（含 deferred，
// 它可调但不可列举），hidden/model-only 两种都不可编排 —— 表里不该有它们。
func TestBuildCatalogNameMapCoversCallableTiers(t *testing.T) {
	cat, err := BuildCatalog(DescribeOptions{
		Tools: []CatalogTool{
			mkTool("read_file", "读文件", "", tools.ExposureDirect),
			mkTool("mcp__dev-radius__get", "远端工具", "mcp__dev-radius", tools.ExposureDeferred),
			mkTool("secret", "已撤销", "", tools.ExposureHidden),
			mkTool("codemode", "自己", "", tools.ExposureModelOnly),
			mkTool("git_status", "git 状态", "git", tools.ExposureCodemode),
		},
		ListDirect: true,
	})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	if id, ok := cat.RawNames["mcp__dev-radius__get"]; !ok {
		t.Error("deferred 工具必须进映射表（可编排但不可列举）")
	} else if _, ok := cat.Names[id]; !ok {
		t.Errorf("映射表双向不一致：RawNames 给出 %q，Names 里没有", id)
	}
	if _, ok := cat.RawNames["secret"]; ok {
		t.Error("hidden 工具不得进映射表（不可编排）")
	}
	if _, ok := cat.RawNames["codemode"]; ok {
		t.Error("model-only 工具不得进映射表（不可编排：禁自嵌套靠它）")
	}
	if _, ok := cat.Names["read_file"]; !ok {
		t.Error("direct 工具必须进映射表")
	}
}

// ---- 缓存不变量 ----

// TestBuildCatalogBytesStable 同一输入两次生成必须字节完全一致（含 map 迭代顺序、
// 工具注册顺序变化）。这是 provider 前缀缓存的地基。
func TestBuildCatalogBytesStable(t *testing.T) {
	mk := func(tools_ []CatalogTool) DescribeOptions {
		return DescribeOptions{
			Tools: tools_,
			Namespaces: map[string]*tools.ToolNamespace{
				"git":              {Name: "git", Description: "Git 仓库操作"},
				"mcp__dev-radius":  {Name: "mcp__dev-radius", Description: "远端只读数据"},
				"":                 {Name: "", Description: "无归属工具"},
				"core":             {Name: "core", Description: "核心工具"},
				"mcp__dev-radius2": {Name: "mcp__dev-radius2", Description: "第二个远端"},
			},
			ListDirect: true,
		}
	}
	res := []CatalogTool{
		mkTool("read_file", "读文件", "core", tools.ExposureDirect, Param{Name: "path"}, Param{Name: "limit", Optional: true}),
		mkTool("write_file", "写文件", "core", tools.ExposureDirect),
		mkTool("git_status", "状态", "git", tools.ExposureCodemode),
		mkTool("git_diff", "差异", "git", tools.ExposureCodemode),
		mkTool("bash", "跑命令", "", tools.ExposureDirect),
		mkTool("mcp__dev-radius__get", "取一条", "mcp__dev-radius", tools.ExposureDeferred),
		mkTool("mcp__dev-radius__list", "列一批", "mcp__dev-radius", tools.ExposureDeferred),
	}
	first, err := BuildCatalog(mk(res))
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	// 打乱输入顺序（注册顺序抖动是真实存在的：MCP 连接顺序、装配顺序）。
	shuffled := []CatalogTool{res[6], res[2], res[0], res[4], res[3], res[1], res[5]}
	second, err := BuildCatalog(mk(shuffled))
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	if first.Description != second.Description {
		t.Errorf("描述字节不稳定：\n--- first ---\n%s\n--- second ---\n%s", first.Description, second.Description)
	}
}

// TestDeferredChurnKeepsOtherEntryBytes MCP 工具（deferred 档）增删时：
//   - 描述全文不得出现任何 MCP 工具名（pi #10212：MCP 一连接就改写描述 → 击穿缓存）；
//   - 已列举条目的行字节不变（「加一个工具不能动别的条目」）。
func TestDeferredChurnKeepsOtherEntryBytes(t *testing.T) {
	base := []CatalogTool{
		mkTool("read_file", "读文件", "core", tools.ExposureCodemode, Param{Name: "path"}),
		mkTool("bash", "跑命令", "core", tools.ExposureDirect),
	}
	deferred := mkTool("mcp__dev-radius__get", "取一条", "mcp__dev-radius", tools.ExposureDeferred)
	otherDeferred := mkTool("mcp__other__list", "列一批", "mcp__other", tools.ExposureDeferred)

	one, err := BuildCatalog(DescribeOptions{Tools: append(append([]CatalogTool{}, base...), deferred)})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	two, err := BuildCatalog(DescribeOptions{Tools: append(append([]CatalogTool{}, base...), deferred, otherDeferred)})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	none, err := BuildCatalog(DescribeOptions{Tools: base})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}

	for _, c := range []struct {
		name string
		cat  *Catalog
	}{{"one-deferred", one}, {"two-deferred", two}, {"no-deferred", none}} {
		for _, bad := range []string{"mcp__dev-radius__get", "mcp__dev_radius__get", "mcp__other__list", "mcp__other__list"} {
			if strings.Contains(c.cat.Description, bad) {
				t.Errorf("%s：描述里出现了 deferred 工具名 %q", c.name, bad)
			}
		}
	}
	// 非空 → 非空（一次真实的 MCP 连接/断开）：整段描述逐字节相同。
	if one.Description != two.Description {
		t.Errorf("deferred 集合从 1 个变 2 个改变了描述字节 —— 前缀缓存会被击穿")
	}
	// 0 个 → 1 个（用户连上第一个 MCP server）：**整段描述逐字节相同**。
	// 指引段无条件拼接 + 后缀与集合解耦之后，MCP 连接/断开对描述字节零影响
	//（复核缺陷 D1 的两条路径都堵在这里；此前 0→1 会多 172 字节并可能给已列举条目改名）。
	if none.Description != one.Description {
		t.Errorf("首次出现 deferred 工具时描述字节变了 —— 前缀缓存会被击穿：\n--- no-deferred ---\n%s\n--- one-deferred ---\n%s", none.Description, one.Description)
	}
	lines := func(c *Catalog) []string {
		out := make([]string, 0, len(c.Entries))
		for _, e := range c.Entries {
			out = append(out, e.Line)
		}
		return out
	}
	a, b := lines(one), lines(none)
	if strings.Join(a, "") != strings.Join(b, "") {
		t.Errorf("deferred 工具出现/消失改变了条目行：\n%v\n%v", a, b)
	}
}

// TestNewToolInOneNamespaceKeepsOtherEntries 增删一个**被列举**的工具时，别组的
// 条目行与组标题也必须逐字节不变（只有它自己那组多/少一行）。
func TestNewToolInOneNamespaceKeepsOtherEntries(t *testing.T) {
	opts := func(extra bool) DescribeOptions {
		ts := []CatalogTool{
			mkTool("read_file", "读文件", "core", tools.ExposureCodemode),
			mkTool("git_status", "状态", "git", tools.ExposureCodemode),
		}
		if extra {
			ts = append(ts, mkTool("mcp__calc__sum", "求和", "calc", tools.ExposureCodemode))
		}
		return DescribeOptions{Tools: ts}
	}
	without, err := BuildCatalog(opts(false))
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	with, err := BuildCatalog(opts(true))
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	for ns, want := range without.GroupLines {
		got := with.GroupLines[ns]
		if strings.Join(got, "") != strings.Join(want, "") {
			t.Errorf("namespace %q 的条目行被别的组新增工具改动了：\n%v\n%v", ns, want, got)
		}
	}
	if len(with.GroupLines["calc"]) != 1 {
		t.Errorf("新增工具自己的组应当有 1 条，实际 %d", len(with.GroupLines["calc"]))
	}
}

// TestHiddenIsInvisible hidden 档彻底不可见：不进描述、不进映射表、不进条目表。
func TestHiddenIsInvisible(t *testing.T) {
	cat, err := BuildCatalog(DescribeOptions{
		Tools: []CatalogTool{
			mkTool("read_file", "读文件", "", tools.ExposureCodemode),
			mkTool("secret_tool", "不该出现", "", tools.ExposureHidden),
			mkTool("typo_tier", "拼错的档位", "", tools.ToolExposure("typo")),
		},
		ListDirect: true,
	})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	for _, e := range cat.Entries {
		if e.RawName == "secret_tool" || e.RawName == "typo_tier" {
			t.Errorf("hidden/非法档位的工具进了条目表: %s", e.RawName)
		}
	}
	for _, bad := range []string{"secret_tool", "typo_tier"} {
		if strings.Contains(cat.Description, bad) {
			t.Errorf("hidden/非法档位的工具进了描述: %s", bad)
		}
		if _, ok := cat.RawNames[bad]; ok {
			t.Errorf("hidden/非法档位的工具进了映射表: %s", bad)
		}
	}
}

// TestListDirectModes direct 档只在 mode:"only"（ListDirect=true）时列举；
// codemode 档两种模式下都列举。
func TestListDirectModes(t *testing.T) {
	tools_ := []CatalogTool{
		mkTool("read_file", "读文件", "", tools.ExposureDirect),
		mkTool("plain", "未声明档位 = direct", "", ""),
		mkTool("git_status", "状态", "git", tools.ExposureCodemode),
	}
	for _, c := range []struct {
		name        string
		listDirect  bool
		wantEntries int
	}{{"mode:on（默认）", false, 1}, {"mode:only", true, 3}} {
		t.Run(c.name, func(t *testing.T) {
			cat, err := BuildCatalog(DescribeOptions{Tools: tools_, ListDirect: c.listDirect})
			if err != nil {
				t.Fatalf("BuildCatalog: %v", err)
			}
			if len(cat.Entries) != c.wantEntries {
				t.Fatalf("列举条目数 = %d, want %d（描述:\n%s）", len(cat.Entries), c.wantEntries, cat.Description)
			}
			// 无论列举与否，direct 工具都可编排。
			for _, t_ := range tools_ {
				if _, ok := cat.RawNames[t_.Name]; !ok {
					t.Errorf("%s 不在映射表里（可编排性不该受列举范围影响）", t_.Name)
				}
			}
		})
	}
}

// ---- 预算不变量 ----

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},     // 向上取整：1 字符不能算 0 token（0 成本条目会无限塞）
		{"abcd", 1},  // 4 字符 = 1
		{"abcde", 2}, // 5 字符 = 2
		{"中文中文", 1},  // 按字符而非字节（否则中文按 3 倍记账）
	}
	for _, c := range cases {
		if got := estimateTokens(c.in); got != c.want {
			t.Errorf("estimateTokens(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestSelectCatalogRoundRobinFairness 轮转公平装箱的核心不变量：
// **任何 namespace 都不会在任何 namespace 完成前被整体跳过**。
func TestSelectCatalogRoundRobinFairness(t *testing.T) {
	groups := []CatalogGroup{
		{Namespace: "a", Tools: []CatalogTool{mkTool("a1", "x", "a", ""), mkTool("a2", "x", "a", ""), mkTool("a3", "x", "a", "")}},
		{Namespace: "b", Tools: []CatalogTool{mkTool("b1", "x", "b", ""), mkTool("b2", "x", "b", ""), mkTool("b3", "x", "b", "")}},
		{Namespace: "c", Tools: []CatalogTool{mkTool("c1", "x", "c", ""), mkTool("c2", "x", "c", ""), mkTool("c3", "x", "c", "")}},
	}
	entryCost := estimateTokens(renderEntry(groups[0].Tools[0]))
	headerCost := estimateTokens(renderGroupHeader("a", "", 0, 3))
	perGroupFirst := headerCost + entryCost

	// 预算 = 每组各一条：结果必须是每组恰好一条。
	sel := SelectCatalog(groups, 0, 3*perGroupFirst)
	for _, ns := range []string{"a", "b", "c"} {
		got := countSelected(sel, ns, groups)
		if got != 1 {
			t.Errorf("预算 = 每组第一条时，namespace %q 列出 %d 条，want 1（每个 namespace 必须先被代表）", ns, got)
		}
	}

	// 预算再加两条：轮转的第二轮只能轮到前两组，第三组保持 1 条。
	sel = SelectCatalog(groups, 0, 3*perGroupFirst+2*entryCost)
	for ns, want := range map[string]int{"a": 2, "b": 2, "c": 1} {
		if got := countSelected(sel, ns, groups); got != want {
			t.Errorf("预算 +2 条时 namespace %q 列出 %d 条，want %d（轮转而非按组填满）", ns, got, want)
		}
	}
	// 受限时被裁掉的条目必须记在 Omitted 里（渲染组标题要靠它）。
	if sel.Omitted["c"] != 2 {
		t.Errorf("Omitted[c] = %d, want 2", sel.Omitted["c"])
	}

	// 预算不限：全部列出。
	sel = SelectCatalog(groups, 0, -1)
	if len(sel.Selected) != 9 {
		t.Errorf("预算不限时选中 %d 条, want 9", len(sel.Selected))
	}
	if len(sel.Omitted) != 0 {
		t.Errorf("预算不限时不该有 Omitted: %v", sel.Omitted)
	}
}

// TestSelectCatalogStarvedGroupIsDocumentedTradeoff 预算连「每组一条」都放不下时，
// 同轮内按组顺序先到先得 —— 排后的组一条都拿不到。这是刻意的取舍（没有预算就是
// 没有预算），但必须**有测试钉住**，免得以后被当成 bug 悄悄改成「按组循环重试」。
func TestSelectCatalogStarvedGroupIsDocumentedTradeoff(t *testing.T) {
	groups := []CatalogGroup{
		{Namespace: "a", Tools: []CatalogTool{mkTool("a1", "x", "a", "")}},
		{Namespace: "b", Tools: []CatalogTool{mkTool("b1", "x", "b", "")}},
		{Namespace: "c", Tools: []CatalogTool{mkTool("c1", "x", "c", "")}},
	}
	cost := estimateTokens(renderEntry(groups[0].Tools[0])) + estimateTokens(renderGroupHeader("a", "", 0, 1))
	sel := SelectCatalog(groups, 0, 2*cost)
	if len(sel.Selected) != 2 {
		t.Fatalf("选中 %d 条, want 2", len(sel.Selected))
	}
	if sel.Selected["c1"] {
		t.Error("预算只够两组的首条时，第三组不该被选中（顺序先到先得）")
	}
	if sel.Omitted["c"] != 1 {
		t.Errorf("Omitted[c] = %d, want 1", sel.Omitted["c"])
	}
}

func countSelected(sel SelectResult, ns string, groups []CatalogGroup) int {
	n := 0
	for _, g := range groups {
		if g.Namespace != ns {
			continue
		}
		for _, t := range g.Tools {
			if sel.Selected[t.Name] {
				n++
			}
		}
	}
	return n
}

// TestSelectCatalogSkipsEmptyGroups 空分组不占预算、不渲染标题（连标题都是白烧 token）。
func TestSelectCatalogSkipsEmptyGroups(t *testing.T) {
	groups := []CatalogGroup{
		{Namespace: "empty"},
		{Namespace: "a", Tools: []CatalogTool{mkTool("a1", "x", "a", "")}},
	}
	sel := SelectCatalog(groups, 0, -1)
	if len(sel.Selected) != 1 || !sel.Selected["a1"] {
		t.Fatalf("Selected = %v, want 只有 a1", sel.Selected)
	}
	if _, ok := sel.Omitted["empty"]; ok {
		t.Error("空分组不该进 Omitted（它没有条目可裁）")
	}
	// 空分组不花一分钱：与「根本没有这个分组」的成本完全一致。
	selNoEmpty := SelectCatalog(groups[1:], 0, -1)
	if sel.UsedTokens != selNoEmpty.UsedTokens {
		t.Errorf("空分组吃掉了预算：%d vs %d", sel.UsedTokens, selNoEmpty.UsedTokens)
	}
}

// TestBuildCatalogBudgetTrim 超预算时：每个 namespace 至少在描述里出现（标题），
// 被裁的组标题带「有工具没列出」提示，且 Omitted 计数正确。
func TestBuildCatalogBudgetTrim(t *testing.T) {
	ts := []CatalogTool{
		mkTool("read_file", strings.Repeat("长摘要 ", 40), "core", tools.ExposureCodemode),
		mkTool("write_file", strings.Repeat("长摘要 ", 40), "core", tools.ExposureCodemode),
		mkTool("git_status", "状态", "git", tools.ExposureCodemode),
		mkTool("lark_send", "发消息", "lark", tools.ExposureCodemode),
		mkTool("calc", "算数", "util", tools.ExposureCodemode),
	}
	// 预算按**实际成本**算：固定开销（INTRO + 标题）+ core 一条 + git 一条 + lark 一条。
	// 这样第 4 组（util）必然被裁，core 的第二条也放不下 —— 裁剪点确定，断言才有意义。
	// 固定开销 = INTRO + **无条件拼接的**指引段 + 标题（与实现同口径；指引段不再按
	// deferred 数量条件拼接，见 D1 的字节稳定性修复）。
	overhead := estimateTokens(DESCRIPTION_INTRO + "\n\n" + DEFERRED_TOOLS_GUIDANCE + "\n\n" + catalogHeading + "\n")
	entry := func(i int) int { return estimateTokens(renderEntry(ts[i])) }
	header := func(ns string, n int) int { return estimateTokens(renderGroupHeader(ns, "", 0, n)) }
	budget := overhead + header("core", 2) + entry(0) + header("git", 1) + entry(2) + header("lark", 1) + entry(3)

	cat, err := BuildCatalog(DescribeOptions{Tools: ts, InlineBudget: &budget})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	if cat.Omitted != 2 { // core 的第二条 + util 唯一一条
		t.Fatalf("Omitted = %d, want 2（core 裁 1、util 裁 1）\n%s", cat.Omitted, cat.Description)
	}
	for _, ns := range []string{"core", "git", "lark", "util"} {
		if !strings.Contains(cat.Description, "["+ns+"]") {
			t.Errorf("namespace %q 在描述里完全消失（组标题都没渲染）", ns)
		}
	}
	if !strings.Contains(cat.Description, "some tools not listed; use searchTools") {
		t.Error("部分被裁的分组标题必须提示 searchTools（some tools not listed）")
	}
	if !strings.Contains(cat.Description, "[util] (tools not listed; use searchTools)") {
		t.Error("整组都没列出来的分组标题必须用 tools not listed 文案（否则模型以为这组不存在）")
	}
	// 每个 namespace 至少被代表：core/git/lark 各一条。
	for ns, want := range map[string]int{"core": 1, "git": 1, "lark": 1, "util": 0} {
		if got := len(cat.GroupLines[ns]); got != want {
			t.Errorf("namespace %q 列出 %d 条, want %d", ns, got, want)
		}
	}
	// 描述整体（含被裁组标题的尾巴）允许比预算略高几个 token，但不该翻倍。
	if cat.UsedTokens > budget+len(cat.GroupLines)*8 {
		t.Errorf("描述 %d token 超出预算 %d 太多（被裁组标题的尾巴有上限）", cat.UsedTokens, budget)
	}

	// 指引段无条件存在（MCP 一个都没连时也在）：它不提任何工具名，故不泄漏 deferred
	// 的存在；而「有工具没列出来时该用 searchTools」这句话必须始终对模型可见。
	if !strings.Contains(cat.Description, DEFERRED_TOOLS_GUIDANCE) {
		t.Error("DEFERRED_TOOLS_GUIDANCE 必须无条件出现在描述里（否则 0→1 个 deferred 会改字节）")
	}

	// 预算 0：一条都不列，但必须给出可执行的下一步。
	zero := 0
	cat, err = BuildCatalog(DescribeOptions{Tools: ts, InlineBudget: &zero})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	if len(cat.Entries) != 0 {
		t.Errorf("预算 0 时不该列出任何条目，实际 %d 条", len(cat.Entries))
	}
	if cat.Omitted != len(ts) {
		t.Errorf("预算 0 时 Omitted = %d, want %d（统计与渲染无关，必须如实给出）", cat.Omitted, len(ts))
	}
	if !strings.Contains(cat.Description, "searchTools") {
		t.Error("预算 0 时描述必须说明去 searchTools 现查")
	}

	// 不限预算（-1）：全部列出，Omitted 归零。
	unlimited := -1
	cat, err = BuildCatalog(DescribeOptions{Tools: ts, InlineBudget: &unlimited})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	if cat.Omitted != 0 || len(cat.Entries) != len(ts) {
		t.Errorf("不限预算时应当全部列出：Omitted=%d Entries=%d", cat.Omitted, len(cat.Entries))
	}

	// 默认预算 = DefaultInlineBudget。
	cat, err = BuildCatalog(DescribeOptions{Tools: ts})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	if cat.Budget != DefaultInlineBudget {
		t.Errorf("默认预算 = %d, want %d", cat.Budget, DefaultInlineBudget)
	}
	if DefaultInlineBudget != 3000 {
		t.Errorf("DefaultInlineBudget = %d, want 3000（对齐 pi）", DefaultInlineBudget)
	}
}

// ---- namespace 校验 ----

func TestBuildCatalogNamespaceErrors(t *testing.T) {
	cases := []struct {
		name      string
		opts      DescribeOptions
		wantInErr string
	}{
		{
			name: "namespace 仅 -/_ 之差",
			opts: DescribeOptions{
				Tools:      []CatalogTool{mkTool("a", "", "dev-radius", tools.ExposureCodemode)},
				Namespaces: map[string]*tools.ToolNamespace{"dev_radius": {Name: "dev_radius"}},
			},
			wantInErr: "归一后同名",
		},
		{
			name: "namespace 命中沙箱保留全局",
			opts: DescribeOptions{
				Tools: []CatalogTool{mkTool("a", "", "store", tools.ExposureCodemode)},
			},
			wantInErr: "保留全局",
		},
		{
			name: "namespace 命中保留全局（console）",
			opts: DescribeOptions{
				Tools:      []CatalogTool{mkTool("a", "", "x", tools.ExposureCodemode)},
				Namespaces: map[string]*tools.ToolNamespace{"console": {Name: "console"}},
			},
			wantInErr: "保留全局",
		},
		{
			name: "工具 raw 名重复",
			opts: DescribeOptions{
				Tools: []CatalogTool{
					mkTool("dup", "一", "", tools.ExposureCodemode),
					mkTool("dup", "二", "", tools.ExposureCodemode),
				},
			},
			wantInErr: "重复",
		},
		{
			name:      "工具名为空",
			opts:      DescribeOptions{Tools: []CatalogTool{mkTool("", "", "", tools.ExposureCodemode)}},
			wantInErr: "工具名为空",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := BuildCatalog(c.opts)
			if err == nil {
				t.Fatalf("BuildCatalog 应当报错（不静默），实际 nil")
			}
			if !strings.Contains(err.Error(), c.wantInErr) {
				t.Errorf("错误文案 %q 里没有 %q", err.Error(), c.wantInErr)
			}
		})
	}
}

// TestNamespaceInstructionsNeverInDescription Instructions 是长文，进了描述就会
// 把别的工具挤掉（设计 §4.2：它由 describeNamespace() 按需读取）。
func TestNamespaceInstructionsNeverInDescription(t *testing.T) {
	cat, err := BuildCatalog(DescribeOptions{
		Tools: []CatalogTool{mkTool("git_status", "状态", "git", tools.ExposureCodemode)},
		Namespaces: map[string]*tools.ToolNamespace{
			"git": {Name: "git", Description: "Git 仓库操作", Instructions: "这里是绝不进描述的长篇使用指引 SECRET-INSTRUCTIONS"},
		},
	})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	if strings.Contains(cat.Description, "SECRET-INSTRUCTIONS") {
		t.Error("namespace Instructions 进了描述（会吃掉整个目录预算）")
	}
	if !strings.Contains(cat.Description, "Git 仓库操作") {
		t.Error("namespace Description 应当进描述（分组标题说明）")
	}
}

// TestBuildCatalogEntryRendering 条目渲染：签名（含可选标记）、摘要首行、超长裁剪。
func TestBuildCatalogEntryRendering(t *testing.T) {
	cat, err := BuildCatalog(DescribeOptions{
		Tools: []CatalogTool{
			mkTool("read_file", "读文件\n第二行不该出现", "core", tools.ExposureCodemode,
				Param{Name: "path"}, Param{Name: "limit", Optional: true}),
			mkTool("no_params", "无参数工具", "core", tools.ExposureCodemode),
			mkTool("long_summary", strings.Repeat("很", maxEntrySummaryChars+50), "core", tools.ExposureCodemode),
		},
	})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	want := []string{
		"  long_summary(): " + strings.Repeat("很", maxEntrySummaryChars) + "...\n",
		"  no_params(): 无参数工具\n",
		"  read_file(path, limit?): 读文件\n",
	}
	for i, w := range want {
		if i >= len(cat.Entries) {
			t.Fatalf("条目数 = %d, want %d", len(cat.Entries), len(want))
		}
		if got := cat.Entries[i].Line; got != w {
			t.Errorf("条目 %d = %q, want %q", i, got, w)
		}
	}
	if strings.Contains(cat.Description, "第二行不该出现") {
		t.Error("摘要只应取首行（多行描述会吃掉预算）")
	}
	// 组内按名字序（渲染顺序稳定），与成本无关。
	if cat.Entries[0].RawName != "long_summary" || cat.Entries[2].RawName != "read_file" {
		t.Errorf("组内渲染顺序应为名字序，实际 %v", []string{cat.Entries[0].RawName, cat.Entries[1].RawName, cat.Entries[2].RawName})
	}
}

// ---- 首行预算解析 ----

func TestParseOptionsLine(t *testing.T) {
	intPtr := func(v int) *int { return &v }
	cases := []struct {
		name    string
		code    string
		body    string
		out     *int
		timeout *int
		wantErr string
	}{
		{name: "没有声明：正文逐字节不变", code: "const a = 1;", body: "const a = 1;"},
		// —— 近似写法必须报错，不能静默当「没声明」（复核缺陷 D3）——
		{name: "漏冒号报错", code: "// @options {\"timeout_ms\": 5000}\nz()", wantErr: "looks like an options line"},
		{name: "注释符后无空格报错", code: "//@options: {\"timeout_ms\": 5000}\nz()", wantErr: "looks like an options line"},
		{name: "全角冒号报错", code: "// @options：{\"timeout_ms\": 5000}\nz()", wantErr: "looks like an options line"},
		{name: "漏 @ 报错", code: "// options: {\"timeout_ms\": 5000}\nz()", wantErr: "looks like an options line"},
		{name: "单数 @option 报错", code: "// @option: {\"timeout_ms\": 5000}\nz()", wantErr: "looks like an options line"},
		{name: "更长标识符不算选项行（当注释）", code: "// @options_x: 1\nz()", body: "// @options_x: 1\nz()"},
		{name: "散文注释不算选项行", code: "// note: options are documented below\nz()", body: "// note: options are documented below\nz()"},
		// —— BOM：剥掉后照常解析（不剥就会静默丢预算）——
		{name: "BOM + 声明", code: "\ufeff// @options: {\"timeout_ms\": 2500}\nz()", body: "z()", timeout: intPtr(2500)},
		// —— 重复键必须报错（JSON last-wins 会静默吞掉前一个声明）——
		{name: "重复键报错", code: "// @options: {\"timeout_ms\": 1000, \"timeout_ms\": 2000}\nz()", wantErr: "declared twice"},
		{name: "空脚本", code: "", body: ""},
		{name: "声明 + 正文", code: "// @options: {\"max_output_tokens\": 4000}\nconst a = 1;", body: "const a = 1;", out: intPtr(4000)},
		{name: "只有声明行", code: "// @options: {\"timeout_ms\": 90000}", body: "", timeout: intPtr(90000)},
		{name: "两个字段", code: "// @options: {\"max_output_tokens\": 128, \"timeout_ms\": 3600000}\nrun()", body: "run()", out: intPtr(128), timeout: intPtr(3600000)},
		{name: "前导空格/Tab 容忍", code: "  \t// @options: {\"timeout_ms\": 1}\nx()", body: "x()", timeout: intPtr(1)},
		{name: "CRLF", code: "// @options: {\"timeout_ms\": 7}\r\nbody()", body: "body()", timeout: intPtr(7)},
		{name: "max_output_tokens = 0 合法", code: "// @options: {\"max_output_tokens\": 0}\nz()", body: "z()", out: intPtr(0)},
		{name: "上限边界 200000", code: "// @options: {\"max_output_tokens\": 200000}\nz()", body: "z()", out: intPtr(200000)},
		{name: "超上限报错", code: "// @options: {\"max_output_tokens\": 200001}\nz()", wantErr: "max_output_tokens"},
		{name: "负数报错", code: "// @options: {\"max_output_tokens\": -1}\nz()", wantErr: "max_output_tokens"},
		{name: "浮点报错", code: "// @options: {\"timeout_ms\": 1.5}\nz()", wantErr: "must be an integer"},
		{name: "指数记法报错", code: "// @options: {\"timeout_ms\": 1e3}\nz()", wantErr: "must be an integer"},
		{name: "字符串值报错", code: "// @options: {\"timeout_ms\": \"1000\"}\nz()", wantErr: "must be an integer"},
		{name: "max_output_tokens 字符串值也报错", code: "// @options: {\"max_output_tokens\": \"4000\"}\nz()", wantErr: "must be an integer"},
		{name: "bool 值报错", code: "// @options: {\"timeout_ms\": true}\nz()", wantErr: "must be an integer"},
		{name: "null 值报错", code: "// @options: {\"timeout_ms\": null}\nz()", wantErr: "must be an integer"},
		{name: "未知字段报错（不静默忽略）", code: "// @options: {\"maxOutputTokens\": 4000}\nz()", wantErr: "unsupported option"},
		{name: "驼峰以外的未知字段也报错", code: "// @options: {\"verbose\": true}\nz()", wantErr: "unsupported option"},
		{name: "timeout_ms = 0 报错", code: "// @options: {\"timeout_ms\": 0}\nz()", wantErr: "timeout_ms"},
		{name: "timeout_ms 超过 1 小时报错", code: "// @options: {\"timeout_ms\": 3600001}\nz()", wantErr: "at most 1 hour"},
		{name: "options 行不是合法 JSON 对象", code: "// @options: not json\nz()", wantErr: "not a JSON object"},
		{name: "options 行是空对象也报错（没有字段）", code: "// @options: {}\nz()", body: "z()"},
		{name: "options 行缺少内容", code: "// @options:\nz()", wantErr: "not a JSON object"},
		{name: "行尾多余内容报错", code: "// @options: {\"timeout_ms\": 1} trailing\nz()", wantErr: "not a JSON object"},
		{name: "数组不是对象", code: "// @options: [1,2]\nz()", wantErr: "not a JSON object"},
		{name: "null 不是对象（不静默当作没声明）", code: "// @options: null\nz()", wantErr: "not a JSON object"},
		{name: "不是前缀（@options_x）当普通代码", code: "// @options_x: 1\nz()", body: "// @options_x: 1\nz()"},
		{name: "不在首行就当普通代码", code: "// 注释\n// @options: {\"timeout_ms\": 1}\nz()", body: "// 注释\n// @options: {\"timeout_ms\": 1}\nz()"},
		{name: "首行是空行时不认", code: "\n// @options: {\"timeout_ms\": 1}\nz()", body: "\n// @options: {\"timeout_ms\": 1}\nz()"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, body, err := ParseOptionsLine(c.code)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseOptionsLine(%q) 应当报错，实际 nil（opts=%+v body=%q）", c.code, got, body)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("错误文案 %q 里没有 %q", err.Error(), c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseOptionsLine(%q): %v", c.code, err)
			}
			if body != c.body {
				t.Errorf("正文 = %q, want %q", body, c.body)
			}
			if !eqIntPtr(got.MaxOutputTokens, c.out) {
				t.Errorf("MaxOutputTokens = %v, want %v", deref(got.MaxOutputTokens), deref(c.out))
			}
			if !eqIntPtr(got.TimeoutMs, c.timeout) {
				t.Errorf("TimeoutMs = %v, want %v", deref(got.TimeoutMs), deref(c.timeout))
			}
		})
	}
}

// TestScriptOptionsDefaults 未声明时的生效值（默认输出预算 10000、不限时）。
func TestScriptOptionsDefaults(t *testing.T) {
	opts, body, err := ParseOptionsLine("body()")
	if err != nil {
		t.Fatalf("ParseOptionsLine: %v", err)
	}
	if body != "body()" {
		t.Errorf("正文 = %q", body)
	}
	if got := opts.OutputTokens(); got != DefaultScriptOutputTokens {
		t.Errorf("默认输出预算 = %d, want %d", got, DefaultScriptOutputTokens)
	}
	// 未声明 timeout_ms = 30s（设计文档 §14.2 的建议值），**不是**不限时：
	// codemode 工具自身 ToolTimeout()=-1（引擎兜底豁免），墙钟 owner 是宿主 ExecScope ——
	// 这里若返回 0 就完全没有自动终止，`while(true){}` 只能靠用户中断，而教学正文对模型
	// 自称 "enforced quotas (memory, wall clock, output size)"（复核缺陷 D3）。
	if got := opts.Timeout(); got != DefaultScriptTimeoutMs*time.Millisecond {
		t.Errorf("未声明 timeout_ms 应为默认 %v，实际 %v", DefaultScriptTimeoutMs*time.Millisecond, got)
	}
	opts, _, err = ParseOptionsLine("// @options: {\"timeout_ms\": 1500, \"max_output_tokens\": 7}\nx()")
	if err != nil {
		t.Fatalf("ParseOptionsLine: %v", err)
	}
	if got := opts.Timeout().Milliseconds(); got != 1500 {
		t.Errorf("Timeout = %d ms, want 1500", got)
	}
	if got := opts.OutputTokens(); got != 7 {
		t.Errorf("OutputTokens = %d, want 7", got)
	}
}

func eqIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// ---- 字节冻结（golden）----

// TestBuildCatalogGolden 把一段固定输入的最终描述**冻结成字节**（分段拼装：
// INTRO 自身由 TestDescriptionIntroContract 钉内容、TestDescriptionIntroBudget 钉长度，
// 这里钉的是块顺序 + 目录段的每一个字节）。
//
// 为什么值得 freeze：这段文本进的是每次请求都带的工具 schema，属于 provider 前缀
// 缓存的一部分；任何「顺手改一下文案/格式」都会让所有会话的缓存整体失效。所以改动
// 必须是有意为之 —— 更新 golden 时请把 diff 读一遍，确认是一次刻意的契约变更。
func TestBuildCatalogGolden(t *testing.T) {
	budget := 3000
	cat, err := BuildCatalog(DescribeOptions{
		Tools: []CatalogTool{
			mkTool("read_file", "读文件并返回内容", "fs", tools.ExposureCodemode, Param{Name: "path"}, Param{Name: "limit", Optional: true}),
			mkTool("bash", "跑一条命令", "", tools.ExposureDirect),
			mkTool("mcp__dev-radius__get", "远端只读接口", "mcp__dev-radius", tools.ExposureDeferred),
		},
		Namespaces: map[string]*tools.ToolNamespace{
			"fs":              {Name: "fs", Description: "文件系统（只读语义由工具自己声明）"},
			"mcp__dev-radius": {Name: "mcp__dev-radius", Description: "远端只读数据"},
		},
		ListDirect:   true,
		InlineBudget: &budget,
	})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	const wantTail = DEFERRED_TOOLS_GUIDANCE + "\n\n" + `Nested tools:
[default]
  bash(): 跑一条命令
[fs] 文件系统（只读语义由工具自己声明）
  read_file(path, limit?): 读文件并返回内容
`
	want := DESCRIPTION_INTRO + "\n\n" + wantTail
	if cat.Description != want {
		t.Fatalf("描述字节变了（若是有意变更，请更新 golden 并写明理由）：\n%q", cat.Description)
	}
	// deferred 的 namespace 一条都不该出现（它只有 deferred 工具）。
	if strings.Contains(cat.Description, "mcp__dev-radius") {
		t.Error("只有 deferred 工具的 namespace 不该出现在描述里")
	}
	if cat.UsedTokens >= budget {
		t.Errorf("示例描述 %d token 逼近预算 %d，golden 失去了「预算有余量」的意义", cat.UsedTokens, budget)
	}
}
