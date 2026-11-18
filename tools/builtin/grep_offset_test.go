package builtin

// grep 的 offset 分页测试。
//
// 分页要解决的痛点：改造前有 200 条硬上限且**没有翻页口** —— 模型看到
// 「more than 200 matches」后只能改 pattern 再搜一遍，或者放弃。
// 有了 offset，模型可以把表头里报的 offset 传回来拿下一页。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/tools"
)

// buildPagedWorkspace 造一个含 n 个匹配行的文件（每个文件一行一个匹配）。
func buildPagedWorkspace(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "match line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "many.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// matchLines 从 grep 的输出里抽出所有命中的行号（形如 "many.txt:12:..."）。
func matchLines(out string) []int {
	var got []int
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 || parts[0] != "many.txt" {
			continue
		}
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		got = append(got, n)
	}
	return got
}

// TestGrepOffsetPaging 逐页翻完，结果既不重也不漏。
func TestGrepOffsetPaging(t *testing.T) {
	const total = 250
	dir := buildPagedWorkspace(t, total)
	ft := NewFileTools(dir)
	g := newGrepTool(ft)

	seen := map[int]bool{}
	for offset := 0; offset < total; offset += 200 {
		out, err := g.Call(t.Context(), "", fmt.Sprintf(`{"pattern":"match","offset":%d}`, offset))
		if err != nil {
			t.Fatalf("offset=%d: %v", offset, err)
		}
		got := matchLines(out)
		if len(got) == 0 {
			t.Fatalf("offset=%d: 一条都没返回\n%s", offset, out)
		}
		for _, ln := range got {
			if seen[ln] {
				t.Errorf("offset=%d: 第 %d 行重复出现（分页有重叠）", offset, ln)
			}
			seen[ln] = true
		}
		// 本页的行号必须落在 [offset+1, offset+200]
		for _, ln := range got {
			if ln <= offset || ln > offset+200 {
				t.Errorf("offset=%d: 第 %d 行不在本页窗口内", offset, ln)
			}
		}
	}
	if len(seen) != total {
		t.Errorf("翻完只拿到 %d 条，应为 %d 条", len(seen), total)
	}
}

// TestGrepOffsetHeader 表头要报出「下一页的 offset」，模型才知道怎么翻。
func TestGrepOffsetHeader(t *testing.T) {
	dir := buildPagedWorkspace(t, 250)
	ft := NewFileTools(dir)
	g := newGrepTool(ft)

	// 第一页：截断 + 指引
	out, err := g.Call(t.Context(), "", `{"pattern":"match"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "more than 200 matches, showing first 200") {
		t.Errorf("第一页表头不对:\n%s", out)
	}

	// 第二页：offset=200 → 报出 matches 201-250
	out, err = g.Call(t.Context(), "", `{"pattern":"match","offset":200}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "matches 201-250 (offset 200)") {
		t.Errorf("第二页表头不对:\n%s", out)
	}
	if !strings.Contains(out, "many.txt:250:") {
		t.Errorf("第二页应含第 250 行:\n%s", out)
	}
	if strings.Contains(out, "many.txt:200:") {
		t.Errorf("第二页不该含第 200 行:\n%s", out)
	}
}

// TestGrepOffsetBeyondTotal offset 越过总数 ⇒ 明确说「没有更多了」。
func TestGrepOffsetBeyondTotal(t *testing.T) {
	dir := buildPagedWorkspace(t, 30)
	ft := NewFileTools(dir)
	g := newGrepTool(ft)

	out, err := g.Call(t.Context(), "", `{"pattern":"match","offset":100}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no matches at offset 100") {
		t.Errorf("表头应说明 offset 越界:\n%s", out)
	}
	if !strings.Contains(out, "30 match(es)") {
		t.Errorf("表头应报出总数:\n%s", out)
	}
	if len(matchLines(out)) != 0 {
		t.Errorf("越界时不该返回匹配行:\n%s", out)
	}
}

// TestGrepOffsetNegative offset < 0 ⇒ 报错（模型可纠正）。
// 走引擎：参数校验在 ValidParams 里（引擎在执行前调用它）。
func TestGrepOffsetNegative(t *testing.T) {
	dir := buildPagedWorkspace(t, 10)
	f := NewFileTools(dir)
	eng := tools.NewToolEngine()
	f.Register(eng)

	r := seq(t, eng, core.ToolCall{Id: "1", Name: "grep", Arguments: `{"pattern":"match","offset":-1}`})
	if !r[0].IsError {
		t.Fatalf("offset=-1 应当报错: %+v", r[0])
	}
	if !strings.Contains(r[0].Result, "invalid_offset") {
		t.Errorf("错误码不对: %v", r[0].Result)
	}
}

// TestGrepOffsetViaEngine 走引擎（模型真实调用路径）也能翻页。
func TestGrepOffsetViaEngine(t *testing.T) {
	dir := buildPagedWorkspace(t, 250)
	f := NewFileTools(dir)
	eng := tools.NewToolEngine()
	f.Register(eng)

	r := seq(t, eng, core.ToolCall{Id: "1", Name: "grep", Arguments: `{"pattern":"match","offset":200}`})
	if r[0].IsError {
		t.Fatalf("grep failed: %+v", r[0])
	}
	if !strings.Contains(r[0].Result, "many.txt:201:") {
		t.Errorf("offset=200 应从第 201 行开始:\n%s", r[0].Result)
	}
	if strings.Contains(r[0].Result, "many.txt:200:") {
		t.Errorf("offset=200 不该含第 200 行:\n%s", r[0].Result)
	}
}

// TestGrepOffsetDoesNotChangeMatchSemantics 分页只是「取哪一段」，
// 命中判定本身不能变（同一 pattern，不同 offset，命中的**总数**一致）。
func TestGrepOffsetDoesNotChangeMatchSemantics(t *testing.T) {
	dir := buildPagedWorkspace(t, 250)
	re := regexp.MustCompile("match")

	col := &grepCollector{max: 1000}
	sc := newGrepScanner()
	lines, count, total, capped, err := grepCountFile(t.Context(), re, sc, filepath.Join(dir, "many.txt"), "many.txt", 1000)
	if err != nil {
		t.Fatal(err)
	}
	col.addFile(grepCountResult{rel: "many.txt", lines: lines, count: count, total: total, capped: capped})
	if col.total != 250 {
		t.Errorf("命中总数应为 250，实际 %d", col.total)
	}
}
