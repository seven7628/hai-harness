package memory

// Stamp 的两条回归防线（2026-09-29，均为**静默**失效：无报错、无日志、测试全绿）。
//
// ① frontmatter 缩进：嵌套结构下把 modified 写进了错误的 YAML 节点，
//    产出**非法 YAML**，连原有的 `type` 一起丢。
// ② 判定口径：hasFrontmatter 判定"有"，setFrontmatterField 却认为"没有"，
//    于是前导空行的记忆文件**静默不补戳**。
//
// 共同点值得记：两者都不是"功能没做"，而是"做了但在一类输入上默默做错"。
// 官方的核心机制依赖时间戳（"The timestamp shows how current the fact is,
// both to you and to Claude"），时间戳读不回来 = 这条记忆的时效信息归零，
// 而模型与用户都看不出出了什么事。

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// readStr 读回文件内容。
func readStr(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读回 %s: %v", p, err)
	}
	return string(b)
}

// parseFM 解析 frontmatter 内的 YAML；非法则报错。
func parseFM(t *testing.T, doc string) map[string]any {
	t.Helper()
	s := strings.TrimPrefix(doc, "---\n")
	if i := strings.Index(s, "\n---"); i >= 0 {
		s = s[:i+1]
	}
	var m map[string]any
	if err := yaml.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("产出非法 YAML: %v\n---\n%s\n---", err, doc)
	}
	return m
}

// TestStampKeepsNestedFrontmatterValid：含嵌套结构时补戳后 YAML 仍合法、字段不丢。
//
// 修复前：`tags:` 是列表时，modified 被按"最后一行缩进"写成列表的同级项，
// 整个 frontmatter 不可解析。
func TestStampKeepsNestedFrontmatterValid(t *testing.T) {
	cases := []struct {
		name   string
		doc    string
		indent string // 期望 modified 的缩进
	}{
		{"嵌套列表", "---\ntype: user\ntags:\n  - alpha\n  - beta\n---\n\n内容。\n", ""},
		{"嵌套对象", "---\ntype: user\nmeta:\n  owner: x\n---\n\n内容。\n", ""},
		{"平坦多键", "---\ntype: user\nscope: a\n---\n\n内容。\n", ""},
		{"整块缩进两格", "---\n  type: user\n  scope: a\n---\n\n内容。\n", "  "},
		{"整块缩进且含列表", "---\n  type: user\n  tags:\n    - x\n---\n\n内容。\n", "  "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := memDir(t)
			p := writeTopic(t, dir, c.name+".md", c.doc)
			if w := stampConfig().StampModified(dir, p); w.Err != nil {
				t.Fatalf("补戳报错: %v", w.Err)
			}
			got := readStr(t, p)
			fm := parseFM(t, got)
			if _, ok := fm["type"]; !ok {
				t.Fatalf("type 字段丢失:\n%s", got)
			}
			if _, ok := fm["modified"]; !ok {
				t.Fatalf("modified 未写入:\n%s", got)
			}
			// modified 必须在**顶层**（与 type 同级）：检查它不在某个嵌套值里
			var probe map[string]any
			body := strings.TrimPrefix(got, "---\n")
			body = body[:strings.Index(body, "\n---")+1]
			if err := yaml.Unmarshal([]byte(body), &probe); err != nil {
				t.Fatalf("非法 YAML: %v", err)
			}
			// tags/meta 等嵌套结构必须原样保留
			if strings.Contains(c.doc, "tags:") {
				if _, ok := fm["tags"]; !ok {
					t.Fatalf("tags 丢失:\n%s", got)
				}
			}
			// 缩进风格与既有顶层键一致
			if want := "modified:"; !strings.Contains(got, c.indent+want) {
				t.Fatalf("modified 缩进不符（期望 %q）:\n%s", c.indent, got)
			}
		})
	}
}

// TestStampHandlesLeadingBlankLine：前导空行的文件仍要补戳。
//
// 修复前：hasFrontmatter 容忍前导空行（判 true），setFrontmatterField 只看
// lines[0]（判没有）→ 静默跳过，且 Err 为 nil，宿主以为成功。
func TestStampHandlesLeadingBlankLine(t *testing.T) {
	docs := map[string]string{
		"前导空行":       "\n---\ntype: user\n---\n\n内容。\n",
		"BOM + 前导空行": "\ufeff\n---\ntype: user\n---\n\n内容。\n",
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			dir := memDir(t)
			p := writeTopic(t, dir, "lead.md", doc)
			if !hasFrontmatter([]byte(doc)) {
				t.Skip("本形态未被判定为有 frontmatter（行为已变，测试需更新）")
			}
			w := stampConfig().StampModified(dir, p)
			if w.Err != nil {
				t.Fatalf("补戳报错: %v", w.Err)
			}
			got := readStr(t, p)
			if !strings.Contains(got, "modified:") {
				t.Fatalf("前导空行导致**静默不补戳**:\n%q", got)
			}
			if !strings.Contains(got, "type: user") {
				t.Fatalf("原有字段丢失:\n%q", got)
			}
		})
	}
}
