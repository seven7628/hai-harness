package builtin

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/tools"
)

// 三个文件工具的 path 参数说明必须与 resolve 的**实际行为**一致。
//
// 回归动机：说明原写 "relative to workspace root"，而 resolve 还接受绝对路径。
// 两条契约不一致时，Project Memory 写入会以
//
//	path "../…/mem/repos/<id>/x.md" escapes workspace root
//
// 的形式失败——2026-09-30 的真实 provider 跑批里，每 7 次写尝试就有 1 次这样失败。
// 宿主给绝对路径、schema 说相对，模型只能二选一，而它每次都选了被拒的那个。
func TestFileToolPathParamDescribesActualContract(t *testing.T) {
	f := NewFileTools(t.TempDir())
	want := map[string]bool{"read_file": true, "write_file": true, "edit_file": true}
	checked := 0

	for _, tl := range f.Tools() {
		if !want[tl.Name()] {
			continue
		}
		checked++
		desc := pathParamDescription(t, tl)

		low := strings.ToLower(desc)
		if !strings.Contains(low, "absolute") {
			t.Fatalf("%s 的 path 说明没有提绝对路径：%q", tl.Name(), desc)
		}
		if !strings.Contains(low, "escape") {
			t.Fatalf("%s 的 path 说明没有说相对路径不得逃逸：%q", tl.Name(), desc)
		}
		// 旧文案只剩这一种说法，必须被替换掉，否则模型仍会以为只能用相对路径。
		if strings.Contains(desc, "(relative to workspace root)") {
			t.Fatalf("%s 的 path 说明仍宣称只支持相对路径：%q", tl.Name(), desc)
		}
	}
	if checked != len(want) {
		t.Fatalf("只检查到 %d 个文件工具，期望 %d", checked, len(want))
	}
}

// TestFileToolAcceptsAbsolutePathOutsideWorkspace 把说明与行为钉在一起：
// 说明里承诺的"工作区外用绝对路径"，必须真的能写。
func TestFileToolAcceptsAbsolutePathOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir() + "/memory/topic.md"
	f := NewFileTools(root)

	var writeTool tools.Tool
	for _, tl := range f.Tools() {
		if tl.Name() == "write_file" {
			writeTool = tl
		}
	}
	if writeTool == nil {
		t.Fatal("没有 write_file")
	}
	ctx := context.Background()
	args := `{"path":` + strconv.Quote(outside) + `,"content":"x\n"}`
	if err := writeTool.ValidParams(ctx, "write_file", args); err != nil {
		t.Fatalf("参数非法: %v", err)
	}
	if _, err := writeTool.Call(ctx, "write_file", args); err != nil {
		t.Fatalf("绝对路径（工作区外）应可写：%v", err)
	}
	// 反面：相对路径逃逸**必须**仍然被拒——说明改的是文案，不是行为。
	esc := `{"path":` + strconv.Quote("../escape.md") + `,"content":"x\n"}`
	if err := writeTool.ValidParams(ctx, "write_file", esc); err != nil {
		t.Fatalf("参数非法: %v", err)
	}
	if _, err := writeTool.Call(ctx, "write_file", esc); err == nil {
		t.Fatal("相对路径逃逸工作区仍应被拒")
	}
}

// pathParamDescription 从工具 schema 的 JSON 里取出 path 参数的 description。
func pathParamDescription(t *testing.T, tl tools.Tool) string {
	t.Helper()
	raw, err := json.Marshal(tl.Parameters())
	if err != nil {
		t.Fatalf("序列化 %s 的参数 schema 失败: %v", tl.Name(), err)
	}
	var m struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("解析 %s 的参数 schema 失败: %v\n%s", tl.Name(), err, raw)
	}
	p, ok := m.Properties["path"]
	if !ok {
		t.Fatalf("%s 的 schema 没有 path 参数：%s", tl.Name(), raw)
	}
	return p.Description
}
