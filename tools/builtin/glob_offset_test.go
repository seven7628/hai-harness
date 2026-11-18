package builtin

// glob 的 offset 分页测试（照 grep 的分页实现补）。
//
// 分页要解决的痛点：改造前 glob 只有 200 条硬上限且**没有翻页口** ——
// 模型看到「showing 200 file(s)」后只能改 pattern 重搜一遍。
// 有了 offset，模型可以把表头里报的 offset 传回来拿下一页。

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/tools"
)

// buildPagedGlobWorkspace 造 n 个匹配文件（名字按字典序 ⇒ 遍历顺序确定）。
func buildPagedGlobWorkspace(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	for i := 0; i < n; i++ {
		name := filepath.Join(dir, fmt.Sprintf("f%04d.txt", i))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// globPaths 从 glob 输出里抽出所有文件路径（表头之后、以 .txt 结尾的行）。
func globPaths(out string) []string {
	var got []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasSuffix(line, ".txt") {
			continue
		}
		got = append(got, line)
	}
	return got
}

// TestGlobOffsetPaging 逐页翻完，结果既不重也不漏。
func TestGlobOffsetPaging(t *testing.T) {
	const total = 250
	dir := buildPagedGlobWorkspace(t, total)
	g := newGlobTool(NewFileTools(dir))

	seen := map[string]bool{}
	for offset := 0; offset < total; offset += 200 {
		out, err := g.Call(t.Context(), "", fmt.Sprintf(`{"pattern":"**/*.txt","offset":%d}`, offset))
		if err != nil {
			t.Fatalf("offset=%d: %v", offset, err)
		}
		got := globPaths(out)
		if len(got) == 0 {
			t.Fatalf("offset=%d: 一条都没返回\n%s", offset, out)
		}
		for _, p := range got {
			if seen[p] {
				t.Errorf("offset=%d: %s 重复出现（分页有重叠）", offset, p)
			}
			seen[p] = true
		}
		if len(got) > 200 {
			t.Errorf("offset=%d: 本页返回 %d 条，超过上限 200", offset, len(got))
		}
	}
	if len(seen) != total {
		t.Errorf("翻完只拿到 %d 条，应为 %d 条", len(seen), total)
	}
}

// TestGlobOffsetHeader 表头要报出「下一页的 offset」，模型才知道怎么翻。
func TestGlobOffsetHeader(t *testing.T) {
	dir := buildPagedGlobWorkspace(t, 250)
	g := newGlobTool(NewFileTools(dir))

	// 第一页：截断 + 指引
	out, err := g.Call(t.Context(), "", `{"pattern":"**/*.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "showing first 200 file(s)") {
		t.Errorf("第一页表头不对:\n%s", out)
	}
	if !strings.Contains(out, "offset=200") {
		t.Errorf("第一页应报出下一页的 offset:\n%s", out)
	}

	// 第二页：offset=200 → 报出 files 201-250
	out, err = g.Call(t.Context(), "", `{"pattern":"**/*.txt","offset":200}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "files 201-250 (offset 200)") {
		t.Errorf("第二页表头不对:\n%s", out)
	}
	if !strings.Contains(out, "f0249.txt") {
		t.Errorf("第二页应含最后一个文件:\n%s", out)
	}
	if !strings.Contains(out, "f0200.txt") {
		t.Errorf("第二页应含第 201 个文件:\n%s", out)
	}
	if strings.Contains(out, "f0199.txt") {
		t.Errorf("第二页不该含第 200 个文件:\n%s", out)
	}
}

// TestGlobOffsetBeyondTotal offset 越过总数 ⇒ 明确说「没有更多了」。
func TestGlobOffsetBeyondTotal(t *testing.T) {
	dir := buildPagedGlobWorkspace(t, 30)
	g := newGlobTool(NewFileTools(dir))

	out, err := g.Call(t.Context(), "", `{"pattern":"**/*.txt","offset":100}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no files at offset 100") {
		t.Errorf("表头应说明 offset 越界:\n%s", out)
	}
	if !strings.Contains(out, "30 file(s) in total") {
		t.Errorf("表头应报出总数:\n%s", out)
	}
	if len(globPaths(out)) != 0 {
		t.Errorf("越界时不该返回文件列表:\n%s", out)
	}
}

// TestGlobOffsetNegative offset < 0 ⇒ 报错（模型可纠正）。
// 走引擎：参数校验在 ValidParams 里（引擎在执行前调用它）。
func TestGlobOffsetNegative(t *testing.T) {
	dir := buildPagedGlobWorkspace(t, 10)
	f := NewFileTools(dir)
	eng := tools.NewToolEngine()
	f.Register(eng)

	r := seq(t, eng, core.ToolCall{Id: "1", Name: "glob", Arguments: `{"pattern":"**/*.txt","offset":-1}`})
	if !r[0].IsError {
		t.Fatalf("offset=-1 应当报错: %+v", r[0])
	}
	if !strings.Contains(r[0].Result, "invalid_offset") {
		t.Errorf("错误码不对: %v", r[0].Result)
	}
}

// TestGlobOffsetViaEngine 走引擎（模型真实调用路径）也能翻页。
func TestGlobOffsetViaEngine(t *testing.T) {
	dir := buildPagedGlobWorkspace(t, 250)
	f := NewFileTools(dir)
	eng := tools.NewToolEngine()
	f.Register(eng)

	r := seq(t, eng, core.ToolCall{Id: "1", Name: "glob", Arguments: `{"pattern":"**/*.txt","offset":200}`})
	if r[0].IsError {
		t.Fatalf("glob failed: %+v", r[0])
	}
	if !strings.Contains(r[0].Result, "f0200.txt") {
		t.Errorf("offset=200 应从第 201 个文件开始:\n%s", r[0].Result)
	}
	if strings.Contains(r[0].Result, "f0199.txt") {
		t.Errorf("offset=200 不该含第 200 个文件:\n%s", r[0].Result)
	}
}

// TestGlobOffsetDoesNotChangeMatchSemantics 分页只是「取哪一段」，
// 匹配判定本身不能变（同一 pattern，不同 offset，命中的文件集合一致）。
func TestGlobOffsetDoesNotChangeMatchSemantics(t *testing.T) {
	dir := buildPagedGlobWorkspace(t, 250)
	g := newGlobTool(NewFileTools(dir))

	all := map[string]bool{}
	for offset := 0; offset < 250; offset += 200 {
		out, err := g.Call(t.Context(), "", fmt.Sprintf(`{"pattern":"**/*.txt","offset":%d}`, offset))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range globPaths(out) {
			all[p] = true
		}
	}
	if len(all) != 250 {
		t.Errorf("翻页后应拿到 250 个文件，实际 %d", len(all))
	}
	// 每一页都必须落在自己的窗口里
	out, err := g.Call(t.Context(), "", `{"pattern":"**/*.txt","offset":200}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range globPaths(out) {
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(p, "f"), ".txt"))
		if err != nil {
			t.Fatalf("路径格式不对: %q", p)
		}
		if n < 200 || n > 249 {
			t.Errorf("offset=200 的页里出现了第 %d 个文件", n)
		}
	}
}
