package codemode

import (
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/tools"
)

// 检索与命名（2026-10-28）：真机排障的三条 ——
//  1. 命中只给脚本名（带 fnv32 后缀），而系统提示里的目录是原始名 → 模型拿目录名比对
//     命中名字，32 个工具全 NOT FOUND；
//  2. 多台 MCP 在册时没有 namespace 过滤，查一个名字会把别的 server 的片段一起捞回来；
//  3. 拼错（或多写/少写一段）时只有 "unknown tool"，模型只能再猜一次。

// nsStub 带 namespace 的工具替身（tierStub 不实现 NamespaceProvider，而 namespace
// 过滤正是本次要钉住的行为）。
type nsStub struct {
	*tierStub
	ns *tools.ToolNamespace
}

func (n *nsStub) Namespace() *tools.ToolNamespace { return n.ns }

func newNamingBridge(t *testing.T) *bridge {
	t.Helper()
	mk := func(name, desc, ns string, exp tools.ToolExposure) tools.Tool {
		base := &tierStub{
			BaseTool: tools.BaseTool{Name_: name, Description_: desc, Params_: map[string]any{}},
			exp:      exp,
		}
		if ns == "" {
			return base
		}
		return &nsStub{tierStub: base, ns: &tools.ToolNamespace{Name: ns}}
	}
	list := []tools.Tool{
		mk("mcp__ai-test__search_scenarios", "搜索场景", "ai-test", tools.ExposureDeferred),
		mk("mcp__ai-test__run_scenarios", "批量跑场景", "ai-test", tools.ExposureDeferred),
		mk("mcp__ai-deployment__get_stream", "读流", "ai-deployment", tools.ExposureDeferred),
		mk("read_file", "Read a file", "", tools.ExposureDirect),
	}
	tool := New(Options{Tools: func() []tools.Tool { return list }})
	snap := tool.snapshot()
	cat, err := BuildCatalog(DescribeOptions{Tools: snap.catalog})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	return &bridge{cat: cat, byRaw: snap.byRaw, ns: snap.ns}
}

// TestSearchHitCarriesCanonicalName 命中同时给脚本名与原始名。
func TestSearchHitCarriesCanonicalName(t *testing.T) {
	b := newNamingBridge(t)
	hits := b.search("search_scenarios", defaultSearchLimit, "", false)
	if len(hits) == 0 {
		t.Fatal("search 无命中")
	}
	h := hits[0]
	if h.Canonical != "mcp__ai-test__search_scenarios" {
		t.Fatalf("canonical_name = %q，应给引擎原始名（模型手里的目录名就是它）", h.Canonical)
	}
	if h.Name == "" || h.Name == h.Canonical {
		t.Fatalf("脚本名应另有其值（归一化 + 后缀）: %q", h.Name)
	}
	// 两个名字都必须真的能调用（resolve 是唯一权威）。
	for _, name := range []string{h.Name, h.Canonical} {
		if _, err := b.resolve(name); err != nil {
			t.Fatalf("命中里的名字 %q 解析不了: %v", name, err)
		}
	}
}

// TestSearchExactOnly 精确模式只回名字完全相等的那一条。
func TestSearchExactOnly(t *testing.T) {
	b := newNamingBridge(t)
	hits := b.search("mcp__ai-test__run_scenarios", defaultSearchLimit, "", true)
	if len(hits) != 1 || hits[0].Canonical != "mcp__ai-test__run_scenarios" {
		t.Fatalf("exact 命中 = %+v，应只有 run_scenarios 一条", hits)
	}
	if hits := b.search("run", defaultSearchLimit, "", true); len(hits) != 0 {
		t.Fatalf("exact 模式下模糊查询应无命中: %+v", hits)
	}
}

// TestSearchNamespaceFilter 只在该 namespace 里检索（多 server 场景）。
func TestSearchNamespaceFilter(t *testing.T) {
	b := newNamingBridge(t)
	all := b.search("stream", defaultSearchLimit, "", false)
	if len(all) == 0 {
		t.Fatal("无 namespace 过滤时应有命中")
	}
	none := b.search("stream", defaultSearchLimit, "ai-test", false)
	if len(none) != 0 {
		t.Fatalf("ai-test 里没有 stream 工具，应 0 命中: %+v", none)
	}
	hit := b.search("stream", defaultSearchLimit, "ai-deployment", false)
	if len(hit) == 0 || hit[0].Canonical != "mcp__ai-deployment__get_stream" {
		t.Fatalf("ai-deployment 里应有 get_stream: %+v", hit)
	}
}

// TestSearchExactMatchRanksFirst 拼对名字却排在模糊命中之后 = 精确匹配形同不存在。
func TestSearchExactMatchRanksFirst(t *testing.T) {
	b := newNamingBridge(t)
	hits := b.search("read_file", defaultSearchLimit, "", false)
	if len(hits) == 0 || hits[0].Canonical != "read_file" {
		t.Fatalf("精确命中的 read_file 应排第一: %+v", hits)
	}
}

// TestResolveSuggestsNearNames 未知工具要带候选（两种名字都列，因为都能调用）。
func TestResolveSuggestsNearNames(t *testing.T) {
	b := newNamingBridge(t)
	// 少了归一化后缀的写法（模型最容易手拼出来的形态）。
	_, err := b.resolve("mcp__ai_test__search_scenarios")
	if err == nil {
		t.Fatal("这个名字不在目录里，应报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "did you mean") {
		t.Fatalf("错误里应带候选: %v", msg)
	}
	if !strings.Contains(msg, "mcp__ai-test__search_scenarios") {
		t.Fatalf("候选里应含原始名（目录里就是这么写的）: %v", msg)
	}
}

// TestSearchToolsScaffoldAcceptsOptions 沙箱侧 searchTools 的第二参数两种形态都认
// （数字 = 老写法，对象 = {limit, namespace, exact}），且把三个字段都送进宿主。
func TestSearchToolsScaffoldAcceptsOptions(t *testing.T) {
	for _, want := range []string{"namespace:", "exact:", "typeof options === 'number'"} {
		if !strings.Contains(scaffoldSource, want) {
			t.Errorf("scaffold 应支持 %q（searchTools(query, options)）", want)
		}
	}
}
