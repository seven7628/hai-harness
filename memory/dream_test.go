package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 做梦 v1 的回归测试。两件事必须钉住：**分桶对不对**、**一个字节都不写**。

// 造一个"什么毛病都有"的记忆目录。
func dreamFixture(t *testing.T) Dir {
	t.Helper()
	d := memDir(t)
	writeIndex(t, d, strings.Join([]string{
		"# Memory Index",
		"- [good.md](good.md) — 正常",
		"- [notype.md](notype.md) — 缺 type",
		"- [ghost.md](ghost.md) — 磁盘上已经没有这个文件",
		"",
	}, "\n"))
	writeTopicFile(t, d, "good.md", "---\ntype: project\nmodified: 2026-11-01T00:00:00+08:00\n---\n\n# 同一个标题\n\n正文\n")
	writeTopicFile(t, d, "notype.md", "---\nmodified: 2026-11-01T00:00:00+08:00\n---\n\n# 缺 type\n\nx\n")
	writeTopicFile(t, d, "nometa.md", "---\ntype: user\n---\n\n# 缺 modified\n\ny\n")
	writeTopicFile(t, d, "orphan.md", "---\ntype: feedback\n---\n\n# 没登记\n\nz\n")
	writeTopicFile(t, d, "plain.md", "没有 frontmatter 的普通笔记\n")
	writeTopicFile(t, d, "dup1.md", "---\ntype: project\n---\n\n# 同一个标题\n\n复述\n")
	writeTopicFile(t, d, "dup2.md", "---\ntype: project\n---\n\n# 同一个 标题！\n\n再复述\n")
	return d
}

func TestDreamBuckets(t *testing.T) {
	d := dreamFixture(t)
	r, err := dreamCfg(t).Dream(d)
	if err != nil {
		t.Fatal(err)
	}

	if r.Topics != 7 {
		t.Fatalf("Topics = %d, want 7", r.Topics)
	}
	has := func(list []string, want string) bool {
		for _, s := range list {
			if s == want {
				return true
			}
		}
		return false
	}
	if !has(r.NotIndexed, "orphan.md") || !has(r.NotIndexed, "plain.md") {
		t.Fatalf("未登记应含 orphan/plain：%v", r.NotIndexed)
	}
	if has(r.NotIndexed, "good.md") {
		t.Fatalf("已登记的 good.md 不该在未登记里：%v", r.NotIndexed)
	}
	if !has(r.Stale, "ghost.md") {
		t.Fatalf("陈旧条目应含 ghost.md：%v", r.Stale)
	}
	if !has(r.MissingType, "notype.md") {
		t.Fatalf("缺 type 应含 notype.md：%v", r.MissingType)
	}
	if !has(r.MissingMeta, "nometa.md") {
		t.Fatalf("缺 modified 应含 nometa.md：%v", r.MissingMeta)
	}
	// 无 frontmatter 的文件不进 MissingType/MissingMeta（官方从不为它加字段）。
	if has(r.MissingType, "plain.md") || has(r.MissingMeta, "plain.md") {
		t.Fatalf("无 frontmatter 的文件不该被报成字段缺失：%v / %v", r.MissingType, r.MissingMeta)
	}
	if len(r.Duplicates) != 1 {
		t.Fatalf("标题相同应聚成 1 对，得到 %v", r.Duplicates)
	}
	if !r.HasIssues() {
		t.Fatal("有毛病时 HasIssues 应为 true")
	}
}

// 最关键的一条：做梦 v1 **只读**。
func TestDreamWritesNothing(t *testing.T) {
	d := dreamFixture(t)
	before := snapshot(t, d.Path)

	if _, err := dreamCfg(t).Dream(d); err != nil {
		t.Fatal(err)
	}

	after := snapshot(t, d.Path)
	if len(before) != len(after) {
		t.Fatalf("文件数变了：%d → %d", len(before), len(after))
	}
	for name, b := range before {
		a, ok := after[name]
		if !ok {
			t.Fatalf("%s 被删了", name)
		}
		if a != b {
			t.Fatalf("%s 内容被改了：\n前：%s\n后：%s", name, b, a)
		}
	}
}

// 干净的目录不该报问题（否则报告会被无视）。
func TestDreamCleanRepoHasNoIssues(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "# Memory Index\n- [a.md](a.md) — 说明\n")
	writeTopicFile(t, d, "a.md", "---\ntype: project\nmodified: 2026-11-01T00:00:00+08:00\n---\n\n# 标题\n\n正文\n")
	r, err := dreamCfg(t).Dream(d)
	if err != nil {
		t.Fatal(err)
	}
	if r.HasIssues() {
		t.Fatalf("干净目录不该报问题：未登记=%v 陈旧=%v 缺type=%v 缺meta=%v 重复=%v",
			r.NotIndexed, r.Stale, r.MissingType, r.MissingMeta, r.Duplicates)
	}
	if !strings.Contains(r.Markdown(time.Now()), "只报不改") {
		t.Fatal("报告必须自带「只报不改」的说明")
	}
}

// "从未被读"要在报告里，且必须写明它不是删除依据。
func TestDreamNeverReadIsNotADeletionSignal(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "# Memory Index\n- [a.md](a.md) — 说明\n")
	writeTopicFile(t, d, "a.md", "---\ntype: project\nmodified: 2026-11-01T00:00:00+08:00\n---\n\nx\n")

	r, _ := dreamCfg(t).Dream(d)
	if len(r.NeverRead) != 1 {
		t.Fatalf("NeverRead = %v", r.NeverRead)
	}
	md := r.Markdown(time.Now())
	if !strings.Contains(md, "从未被读过") || !strings.Contains(md, "不是删除依据") {
		t.Fatalf("报告必须把这条讲清楚：\n%s", md)
	}

	// 读过一次之后就不该再列。
	if err := (Config{}).RecordRead(d, filepath.Join(d.Path, "a.md")); err != nil {
		t.Fatal(err)
	}
	r2, _ := dreamCfg(t).Dream(d)
	if len(r2.NeverRead) != 0 {
		t.Fatalf("读过之后不该再列：%v", r2.NeverRead)
	}
}

// 索引超限要被单独标出来——那是唯一"内容还在、却已经不在注入里"的状态。
func TestDreamFlagsOversizeIndex(t *testing.T) {
	d := memDir(t)
	var b strings.Builder
	b.WriteString("# Memory Index\n")
	for i := 0; i < MaxIndexLines+5; i++ {
		b.WriteString("- f" + itoa(i) + ".md\n")
	}
	writeIndex(t, d, b.String())

	r, _ := dreamCfg(t).Dream(d)
	if !r.Index.OverLine {
		t.Fatalf("应判定超行：%d 行", r.Index.Lines)
	}
	if !strings.Contains(r.Markdown(time.Now()), "索引已超限") {
		t.Fatal("超限应在报告里单独一节")
	}
}

// snapshot 记录目录里每个文件的内容，用于证明"没写"。
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			// .state 也是我们可能误写的地方，一并记录
			sub, err := os.ReadDir(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			for _, s := range sub {
				raw, _ := os.ReadFile(filepath.Join(dir, e.Name(), s.Name()))
				out[e.Name()+"/"+s.Name()] = string(raw)
			}
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(raw)
	}
	return out
}

// dreamCfg 给 Dream 一个**空的**转录根：单元测试不该去扫真实的 5GB 事件目录。
func dreamCfg(t *testing.T) Config {
	t.Helper()
	return Config{EventsRoot: t.TempDir()}
}
