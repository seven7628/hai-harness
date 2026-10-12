package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 9, 27, 15, 4, 5, 0, time.UTC)

// stampConfig 默认时间固定，便于逐字节断言。
func stampConfig() Config {
	c := Config{}.withDefaults()
	c.Now = func() time.Time { return fixedNow }
	return c
}

func writeTopic(t *testing.T, d Dir, name, content string) string {
	t.Helper()
	p := filepath.Join(d.Path, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const topicWithFM = "---\ntype: user\n---\n\n用户是做分布式存储的。\n"

// 官方主路径：有 frontmatter → 补 modified，正文一个字节不动。
func TestStampModified_AddsField(t *testing.T) {
	d := memDir(t)
	p := writeTopic(t, d, "user_role.md", topicWithFM)

	res := stampConfig().StampModified(d, p)
	if res.Err != nil {
		t.Fatalf("补 modified 失败：%v", res.Err)
	}
	if res.Stamped != StampedWritten {
		t.Fatalf("Stamped = %v, want StampedWritten", res.Stamped)
	}
	got := mustRead(t, p)
	want := "---\ntype: user\nmodified: 2026-09-27T15:04:05Z\n---\n\n用户是做分布式存储的。\n"
	if got != want {
		t.Fatalf("内容不符：\ngot  %q\nwant %q", got, want)
	}
	// 官方明确是 ISO 8601 时间戳。
	if !strings.Contains(got, "modified: 2026-09-27T15:04:05Z") {
		t.Fatalf("时间戳不是 ISO 8601：%q", got)
	}
	// 索引不受影响（官方只对 memory file 补）。
	if _, err := os.Stat(filepath.Join(d.Path, IndexFileName)); err == nil {
		t.Fatal("不该凭空建出索引文件")
	}
}

// 官方第 ② 条：**从不为无 frontmatter 的文件添加 frontmatter**。
// 这是本文件最容易被"顺手优化"掉的一条，所以单独测，且覆盖各种无 frontmatter 形态。
func TestStampModified_NeverAddsFrontmatter(t *testing.T) {
	d := memDir(t)
	cases := map[string]string{
		"纯正文":       "用户是做分布式存储的。\n",
		"空文件":       "",
		"只有换行":      "\n\n\n",
		"分隔线不在首行":   "标题\n---\ntype: user\n---\n",
		"有开无闭":      "---\ntype: user\n\n没有结束线。\n",
		"前导空行后才是正文": "\n\n# 笔记\n",
		"Setext 标题": "Project Memory\n===\n- a.md\n",
	}
	for name, content := range cases {
		p := writeTopic(t, d, "t_"+sanitizeSegment(name)+".md", content)
		res := stampConfig().StampModified(d, p)
		if res.Err != nil {
			t.Fatalf("%s: %v", name, res.Err)
		}
		if res.Stamped != StampedSkipped {
			t.Fatalf("%s: Stamped = %v, want StampedSkipped", name, res.Stamped)
		}
		if after := mustRead(t, p); after != content {
			t.Fatalf("%s: 文件被改动了：\n%q\nwant\n%q", name, after, content)
		}
	}
}

// 写入不破坏原有 frontmatter 字段：字段顺序、注释、缩进、其它自定义字段全保留。
// 这里刻意不用 yaml 往返（往返必然丢注释与引号风格，那正是本实现要避免的）。
func TestStampModified_PreservesFrontmatter(t *testing.T) {
	d := memDir(t)
	original := "---\n" +
		"# 这条记忆的来源\n" +
		"type: project\n" +
		"tags: [db, sharding]\n" +
		"owner: \"zhang san\"\n" +
		"---\n" +
		"\n" +
		"正文里有 --- 横线，和 modified: 假字段。\n"
	p := writeTopic(t, d, "decision.md", original)

	if res := stampConfig().StampModified(d, p); res.Err != nil {
		t.Fatal(res.Err)
	}
	got := mustRead(t, p)
	want := "---\n" +
		"# 这条记忆的来源\n" +
		"type: project\n" +
		"tags: [db, sharding]\n" +
		"owner: \"zhang san\"\n" +
		"modified: 2026-09-27T15:04:05Z\n" +
		"---\n" +
		"\n" +
		"正文里有 --- 横线，和 modified: 假字段。\n"
	if got != want {
		t.Fatalf("frontmatter 被破坏：\ngot  %q\nwant %q", got, want)
	}
	// 幂等：同一时刻再写一次不产生额外写盘（Unchanged），内容也不变。
	before := mustRead(t, p)
	res := stampConfig().StampModified(d, p)
	if res.Err != nil || res.Stamped != StampedUnchanged {
		t.Fatalf("重复补写 = %+v, want Unchanged", res)
	}
	if after := mustRead(t, p); after != before {
		t.Fatal("重复补写改动了文件")
	}
}

// 已有 modified 时**就地替换**，不新增第二行。
func TestStampModified_ReplacesExistingField(t *testing.T) {
	d := memDir(t)
	p := writeTopic(t, d, "user_role.md",
		"---\ntype: user\nmodified: 2020-01-01T00:00:00Z\n---\n\n旧内容。\n")
	if res := stampConfig().StampModified(d, p); res.Err != nil {
		t.Fatal(res.Err)
	}
	got := mustRead(t, p)
	if n := strings.Count(got, "modified:"); n != 1 {
		t.Fatalf("modified 字段出现 %d 次：%q", n, got)
	}
	if !strings.Contains(got, "modified: 2026-09-27T15:04:05Z") {
		t.Fatalf("旧的 modified 没被替换：%q", got)
	}
	// 正文里的 "modified:" 假字段不受影响（只动 frontmatter 区段）。
	p2 := writeTopic(t, d, "fake.md",
		"---\ntype: user\n---\n\n正文提到 modified: 2020-01-01 这件事。\n")
	if res := stampConfig().StampModified(d, p2); res.Err != nil {
		t.Fatal(res.Err)
	}
	if n := strings.Count(mustRead(t, p2), "modified:"); n != 2 {
		t.Fatalf("正文字段被误改（应为 1 处 frontmatter + 1 处正文）：%q", mustRead(t, p2))
	}
}

// 缩进过的 frontmatter 跟随原风格（有人会整体缩进两格）。
func TestStampModified_KeepsIndentStyle(t *testing.T) {
	d := memDir(t)
	p := writeTopic(t, d, "indented.md", "---\n  type: user\n---\n\n内容。\n")
	if res := stampConfig().StampModified(d, p); res.Err != nil {
		t.Fatal(res.Err)
	}
	if !strings.Contains(mustRead(t, p), "\n  modified: 2026-09-27T15:04:05Z\n") {
		t.Fatalf("未跟随缩进风格：%q", mustRead(t, p))
	}
}

// 开关关闭时**不写盘**（config.go：否则就是"不读但写入"，比不实现更糟）。
func TestStampModified_DisabledDoesNotWrite(t *testing.T) {
	d := memDir(t)
	p := writeTopic(t, d, "user_role.md", topicWithFM)
	before := mustRead(t, p)
	fiBefore, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	var writes int32
	base := countingConfig(t, nil, &writes)
	base.Now = func() time.Time { return fixedNow }
	cfg := base
	cfg.Enabled = Bool(false)

	res := cfg.StampModified(d, p)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.Stamped != StampedSkipped {
		t.Fatalf("关闭开关时 Stamped = %v, want Skipped", res.Stamped)
	}
	if n := atomic.LoadInt32(&writes); n != 0 {
		t.Fatalf("关闭开关时发生 %d 次写盘（必须为 0）", n)
	}
	if after := mustRead(t, p); after != before {
		t.Fatalf("关闭开关却改了文件：\n%q", after)
	}
	// 也不该留下锁侧车文件（连"准备写"的动作都不该有）。
	if _, err := os.Stat(p + lockSuffix); err == nil {
		t.Fatal("关闭开关时不应创建锁文件")
	}
	fiAfter, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !fiAfter.ModTime().Equal(fiBefore.ModTime()) {
		t.Fatal("关闭开关却改了文件 mtime")
	}
}

// 非 topic 路径一律跳过：索引文件、非 .md、目录之外。
func TestStampModified_SkipsNonTopicPaths(t *testing.T) {
	d := memDir(t)
	index := writeIndex(t, d, "---\ntype: index\n---\n")
	notMD := writeTopic(t, d, "notes.txt", "---\ntype: user\n---\n")
	outside := filepath.Join(t.TempDir(), "user_role.md")
	if err := os.WriteFile(outside, []byte(topicWithFM), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{index, notMD, outside, filepath.Join(d.Path, "missing.md")} {
		res := stampConfig().StampModified(d, p)
		if res.Err != nil {
			t.Fatalf("%s: %v", p, res.Err)
		}
		if res.Stamped != StampedSkipped {
			t.Fatalf("%s: Stamped = %v, want Skipped", p, res.Stamped)
		}
	}
	if strings.Contains(mustRead(t, index), "modified") {
		t.Fatal("索引被补了 modified（官方只对 memory file 补）")
	}
	if strings.Contains(mustRead(t, notMD), "modified") {
		t.Fatal("非 .md 被补了 modified")
	}
	if strings.Contains(mustRead(t, outside), "modified") {
		t.Fatal("记忆目录外的文件被补了 modified")
	}
}

// 写入失败**不吞错误**（§5.3）：错误要让调用方有机会回给模型。
func TestStampModified_WriteFailureIsReported(t *testing.T) {
	d := memDir(t)
	p := writeTopic(t, d, "user_role.md", topicWithFM)

	c := stampConfig()
	c.Rename = func(string, string) error { return errors.New("rename 失败：模拟只读文件系统") }
	res := c.StampModified(d, p)
	if res.Err == nil {
		t.Fatal("写盘失败必须返回错误（吞掉会留下'看起来成功但时间戳不更新'的文件）")
	}
	if !errors.Is(res.Err, ErrStamped) {
		t.Fatalf("err = %v, 期望包装 ErrStamped", res.Err)
	}
	// 失败不得留下半截文件或临时文件。
	assertNoTempLeftovers(t, d.Path)
	if got := mustRead(t, p); got != topicWithFM {
		t.Fatalf("失败后原文件应完好：%q", got)
	}
}

func TestStampModified_ReadFailureIsReported(t *testing.T) {
	d := memDir(t)
	p := writeTopic(t, d, "user_role.md", topicWithFM)
	c := stampConfig()
	calls := 0
	c.ReadFile = func(string) ([]byte, error) {
		calls++
		return nil, fmt.Errorf("第 %d 次读失败", calls)
	}
	res := c.StampModified(d, p)
	if res.Err == nil || !errors.Is(res.Err, ErrStamped) {
		t.Fatalf("err = %v, 期望包装 ErrStamped", res.Err)
	}
}

// 并发写同一个 topic：必须串行化，且**不丢 frontmatter 字段**。
//
// 记忆目录是共享的（同一仓库的所有 worktree 与并行会话写同一个 <id>/ 目录，§3.2），
// 没有串行化时 read-modify-write 会丢字段：两个写入方都读到 `type: user`，
// 各自补一个 `modified`，后写的把先写的整段覆盖。
//
// 每个写入方调 SetMemoryField 写**自己那一个**字段——这正是锁要保护的形态：
// 16 个写入方各自读-改-写同一个文件，16 个字段必须全部留存。
// 如果锁失效（`go test -race` 之外，靠这里的字段留存断言抓），必然有字段被覆盖。
func TestStampModified_ConcurrentDoesNotLoseFields(t *testing.T) {
	d := memDir(t)
	p := writeTopic(t, d, "shared.md", "---\ntype: project\nnote: 原有内容\n---\n\n正文。\n")

	const writers = 16
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := stampConfig()
			c.Now = func() time.Time { return fixedNow }
			res := c.SetMemoryField(d, p, fmt.Sprintf("w%d", i), "x")
			if res.Err != nil {
				errs[i] = res.Err
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("写入方 %d: %v", i, err)
		}
	}
	got := mustRead(t, p)
	// 每个写入方写下的字段都必须还在（丢一个就是丢字段的回归）。
	for i := 0; i < writers; i++ {
		if !strings.Contains(got, fmt.Sprintf("\nw%d: x\n", i)) {
			t.Fatalf("字段 w%d 丢失（并发写没串行化）：\n%s", i, got)
		}
	}
	for _, want := range []string{"type: project", "note: 原有内容", "modified: 2026-09-27T15:04:05Z"} {
		if !strings.Contains(got, want) {
			t.Fatalf("原有字段 %q 丢失：\n%s", want, got)
		}
	}
	// modified 只应有一行（重复补写不得堆叠）。
	if n := strings.Count(got, "modified:"); n != 1 {
		t.Fatalf("modified 出现 %d 次：\n%s", n, got)
	}
	if !strings.HasSuffix(got, "正文。\n") {
		t.Fatalf("正文被破坏：%q", got)
	}
}

// SetMemoryField 自身的形态：写入自己的字段 + 同一时刻只留一个 modified。
func TestSetMemoryField(t *testing.T) {
	d := memDir(t)
	p := writeTopic(t, d, "user_role.md", topicWithFM)
	c := stampConfig()
	if res := c.SetMemoryField(d, p, "type", "feedback"); res.Err != nil {
		t.Fatal(res.Err)
	}
	got := mustRead(t, p)
	if !strings.Contains(got, "\ntype: feedback\n") {
		t.Fatalf("type 未被替换：%q", got)
	}
	if n := strings.Count(got, "modified:"); n != 1 {
		t.Fatalf("modified 出现 %d 次：%q", n, got)
	}
	// 无 frontmatter 的文件依然不加（官方第 ② 条）。
	p2 := writeTopic(t, d, "plain.md", "没有 frontmatter。\n")
	if res := c.SetMemoryField(d, p2, "type", "user"); res.Stamped != StampedSkipped {
		t.Fatalf("Stamped = %+v, want Skipped", res)
	}
	if mustRead(t, p2) != "没有 frontmatter。\n" {
		t.Fatal("无 frontmatter 的文件被改动了")
	}
	// 开关关闭时不写。
	off := c
	off.Enabled = Bool(false)
	p3 := writeTopic(t, d, "off.md", topicWithFM)
	if res := off.SetMemoryField(d, p3, "type", "project"); res.Stamped != StampedSkipped {
		t.Fatalf("关闭开关时 Stamped = %+v", res)
	}
	if strings.Contains(mustRead(t, p3), "type: project") {
		t.Fatal("关闭开关却写了字段")
	}
}

// 并发写**不同**的 topic 文件互不干扰（锁的粒度是文件，不是目录）。
func TestStampModified_ConcurrentDistinctTopics(t *testing.T) {
	d := memDir(t)
	const n = 12
	paths := make([]string, n)
	for i := range paths {
		paths[i] = writeTopic(t, d, fmt.Sprintf("t%02d.md", i), topicWithFM)
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res := stampConfig().StampModified(d, paths[i])
			if res.Err != nil {
				errs[i] = res.Err
				return
			}
			if res.Stamped != StampedWritten {
				errs[i] = fmt.Errorf("Stamped = %v", res.Stamped)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("topic %d: %v", i, err)
		}
	}
	for i, p := range paths {
		if !strings.Contains(mustRead(t, p), "modified: 2026-09-27T15:04:05Z") {
			t.Fatalf("topic %d 未被打上时间戳", i)
		}
	}
}

// 原子写不得留下临时文件（失败路径尤其重要：写一半就崩会留下垃圾，官方目录是
// 用户可见的 ~/.go-code/memory/，垃圾会被用户当成记忆）。
func assertNoTempLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("留下临时文件 %q", e.Name())
		}
	}
}

// 写入后目录里不得出现半截文件：每个 .md 都能被完整读出 frontmatter。
func TestStampModified_ResultIsAlwaysWellFormed(t *testing.T) {
	d := memDir(t)
	for i := 0; i < 30; i++ {
		name := fmt.Sprintf("t%02d.md", i)
		p := writeTopic(t, d, name, topicWithFM)
		if res := stampConfig().StampModified(d, p); res.Err != nil {
			t.Fatalf("%s: %v", name, res.Err)
		}
	}
	assertNoTempLeftovers(t, d.Path)
}

// 权限位不被无声改掉：用户手工建的文件（比 0600 更宽）保持原样。
func TestStampModified_KeepsFileMode(t *testing.T) {
	d := memDir(t)
	p := filepath.Join(d.Path, "user_role.md")
	if err := os.WriteFile(p, []byte(topicWithFM), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if res := stampConfig().StampModified(d, p); res.Err != nil {
		t.Fatal(res.Err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("权限位被改成了 %v", fi.Mode().Perm())
	}
}
