package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 详细规则搬到了按需文件里，但内容不能缩水：这是"渐进披露"能成立的前提——
// 常驻段说"去读它"，它就得真的有东西可读。
func TestGuideDocCarriesTheDetails(t *testing.T) {
	d := memDir(t)
	doc := guideDoc(d.Path)

	mustContain := []string{
		d.Path,                                     // 目录
		IndexFileName,                              // 索引名
		"[Title](file.md)",                         // 索引行样例（与 guidance 的硬规则同源）
		"user", "feedback", "project", "reference", // 四种 type 的语义
		"Check for an existing entry first", // 查重
		"snapshot of what was true",         // 时效性
		"200", "25000",                      // 限额
	}
	for _, want := range mustContain {
		if !strings.Contains(doc, want) {
			t.Fatalf("指南缺少 %q：\n%s", want, doc)
		}
	}
	// 四种 type 必须**各自带一句语义**，否则又退化成盲枚举。
	for _, typ := range []string{"`user`", "`feedback`", "`project`", "`reference`"} {
		i := strings.Index(doc, typ)
		if i < 0 {
			t.Fatalf("指南缺少 %s", typ)
		}
		if rest := doc[i+len(typ):]; len(strings.TrimSpace(rest)) < 8 {
			t.Fatalf("%s 后面没有说明：%q", typ, rest)
		}
	}
}

// 注入时若指南不存在会补写一份——否则常驻段里那句"去读 GUIDE.md"就是死指针。
func TestEnsureGuideWrittenOnInjection(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "- [a.md](a.md) — 甲\n")

	cfg := fullCfg()
	cfg.ensureGuide(d)
	if _, err := os.Stat(guidePath(d)); err != nil {
		t.Fatalf("注入后指南应存在：%v", err)
	}
	// 幂等：内容一致时不再重写（用 mtime 判断会被秒级精度骗，这里直接比内容）。
	raw, _ := os.ReadFile(guidePath(d))
	if string(raw) != guideDoc(d.Path) {
		t.Fatal("指南内容与 guideDoc 不一致")
	}
}

// 目录不存在时不建任何东西：Bootstrap 的契约是"不凭空造出记忆目录"。
func TestEnsureGuideDoesNotCreateMemoryDir(t *testing.T) {
	root := t.TempDir()
	d := Dir{ID: "x", Path: filepath.Join(root, "repos", "x")}
	cfg := fullCfg()
	cfg.ensureGuide(d)
	if _, err := os.Stat(d.Path); err == nil {
		t.Fatal("不该凭空建出记忆目录")
	}
}
