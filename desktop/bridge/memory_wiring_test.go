package main

// Project Memory 接线的端到端验证（Step 3 注入 + Step 4 modified 钩子）。
//
// 这些测试存在的理由：接线本身没有别的护栏。memory 包的 61 项单测全绿、agents 与
// tools 的既有测试也全绿，但它们各自都**不知道对方存在**——一个接线漏字、闸门写反、
// 或路径比较写错，都会呈现为"测试全过、记忆功能完全不工作"。
//
// 所以这里测的是**跨包行为**：真实 write_file 写到真实记忆目录里，frontmatter 的
// modified 必须被补上；关掉开关，同一条路径必须一个字节都不动。

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/memory"
	"github.com/seven7628/hai-harness/tools/builtin"
)

// newTestMemHost 造一个指向临时记忆根的 memoryHost（不依赖真实 ~/.go-code）。
func newTestMemHost(t *testing.T, root string, enabled bool) *memoryHost {
	t.Helper()
	cfg := memory.Config{Directory: root}
	if !enabled {
		cfg.Enabled = memory.Bool(false)
	}
	return newMemoryHost(cfg)
}

// memTopicDir 造一个 memoryHost 与它**真实解析出来**的记忆目录。
//
// 2026-09-30 改写。此前这里硬塞 `h.dir = &memory.Dir{Path: root/repos/testid}` 并靠
// 「Bootstrap 在非 git 目录会失败」来保证预设不被覆盖。非 git 回落项目根（官方规定，
// 见 memory/identity.go）之后，那条隐含前提失效，Bootstrap 会用路径派生的 ID 覆盖
// 预设目录，于是钩子开始往另一个目录写——6 个测试同时变红。
//
// 现在改成**走真实解析**：用与生产同一条路径（memory.Bootstrap）拿到目录，再建它。
// 代价是这个辅助函数不再"跳过 git 解析"，收益是它测的就是产品行为。
func memTopicDir(t *testing.T) (*memoryHost, *memory.Dir, string) {
	t.Helper()
	root := t.TempDir()
	ws := t.TempDir()
	h := newTestMemHost(t, root, true)
	res := memory.Bootstrap(context.Background(), ws, h.cfg)
	if !res.Resolved || res.Dir == nil {
		t.Fatalf("解析记忆目录失败：%v", res.Err)
	}
	if err := os.MkdirAll(res.Dir.Path, 0o700); err != nil {
		t.Fatalf("建记忆目录: %v", err)
	}
	h.once.Do(func() {}) // 锁住 once，避免后续 resolve 再跑一次并覆盖
	h.dir = res.Dir
	return h, res.Dir, root
}

// TestPostWriteStampsTopicFrontmatter：模型用标准 write_file 写记忆 topic，
// modified 必须被补上。这是 Step 4 的**唯一**存在理由。
func TestPostWriteStampsTopicFrontmatter(t *testing.T) {
	h, dir, _ := memTopicDir(t)
	topic := filepath.Join(dir.Path, "relay_tunnel_id.md")
	body := "---\ntype: project\n---\n\nThe tunnel identifier is `q7-tunnel-corp`.\n"
	if err := os.WriteFile(topic, []byte(body), 0o600); err != nil {
		t.Fatalf("预置 topic: %v", err)
	}

	ws := t.TempDir()
	ft := builtin.NewFileTools(ws).
		WithPostWrite(func(p string) (string, error) { return h.postWrite(context.Background(), ws, p) })
	// 记忆目录在工作区之外 —— 必须用**绝对路径**。这正是官方形态：模型被告知索引在
	// 工作区外某处，用标准 write_file 写绝对路径过去。若这里改用相对路径而写到工作区
	// 内的同名文件，测试会假通过而生产仍然坏。
	if err := callWriteFile(ft, topic, body); err != nil {
		t.Fatalf("write_file 应成功: %v", err)
	}

	got, err := os.ReadFile(topic)
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if !strings.Contains(string(got), "modified:") {
		t.Fatalf("frontmatter 未被补 modified：\n%s", got)
	}
	// 原有字段必须原样保留（行级原地编辑，不是 yaml 往返）
	if !strings.Contains(string(got), "type: project") {
		t.Fatalf("原有 type 字段丢失：\n%s", got)
	}
	if !strings.Contains(string(got), "q7-tunnel-corp") {
		t.Fatalf("正文被改写：\n%s", got)
	}
}

// TestMemoryPathIsReachableByFileTools：记忆目录必须对标准文件工具**可达**。
//
// 这条是接线的前提，缺了它上面那条测试也会假通过（测试用的临时目录不在
// ~/.go-code 下，绕过了生产里那道保护）。官方形态没有记忆专用工具，模型只有
// read_file / write_file / edit_file —— 记忆目录被 deny 罩住的话，模型既读不到
// 注入索引指向的 topic，也写不进新记忆，auto memory 整个不成立。
func TestMemoryPathIsReachableByFileTools(t *testing.T) {
	home := t.TempDir()
	topic := filepath.Join(home, ".go-code", "memory", "repos", "id", "t.md")
	if err := os.MkdirAll(filepath.Dir(topic), 0o700); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(topic, []byte("---\ntype: project\n---\n\nx\n"), 0o600); err != nil {
		t.Fatalf("写: %v", err)
	}
	t.Setenv("HOME", home)
	ft := builtin.NewFileTools(t.TempDir())
	for _, tl := range ft.Tools() {
		if tl.Name() != "read_file" {
			continue
		}
		_, err := tl.Call(context.Background(), "read_file", `{"path":`+jsonQuote(topic)+`}`)
		if err != nil {
			t.Fatalf("read_file 读不到记忆目录（deny 名单未放行 memory 子树）: %v", err)
		}
		return
	}
	t.Fatal("未注册 read_file")
}

// TestPostWriteDisabledIsInert：开关关闭时，同一条写入路径必须**一个字节都不动**。
//
// 这是"关闭 = 完全不生效"的全部语义。只接了读侧（不注入）而漏了写侧，用户关了
// 记忆却在背后继续生成文件 —— 比不实现更糟。
func TestPostWriteDisabledIsInert(t *testing.T) {
	root := t.TempDir()
	h := newTestMemHost(t, root, false)
	dir := memory.Dir{Path: filepath.Join(root, "repos", "testid")}
	if err := os.MkdirAll(dir.Path, 0o700); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	topic := filepath.Join(dir.Path, "x.md")
	body := "---\ntype: project\n---\n\nbody\n"
	if err := os.WriteFile(topic, []byte(body), 0o600); err != nil {
		t.Fatalf("预置: %v", err)
	}
	before, _ := os.Stat(topic)
	beforeBody, _ := os.ReadFile(topic)

	// 开关关闭时，路径判定也必须直接短路（不建 .lock、不解析身份）
	if _, err := h.postWrite(context.Background(), t.TempDir(), topic); err != nil {
		t.Fatalf("关闭时不该报错: %v", err)
	}

	after, _ := os.Stat(topic)
	afterBody, _ := os.ReadFile(topic)
	if string(beforeBody) != string(afterBody) {
		t.Fatalf("关闭开关时正文被改了")
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("关闭开关时文件被重写（mtime 变化）")
	}
	entries, _ := os.ReadDir(dir.Path)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".lock") {
			t.Fatalf("关闭开关时仍建了锁侧车文件：%s", e.Name())
		}
	}
}

// TestPostWriteIgnoresNonMemoryPaths：工作区内的普通写入不触发任何记忆逻辑。
func TestPostWriteIgnoresNonMemoryPaths(t *testing.T) {
	h, dir, _ := memTopicDir(t)
	ws := t.TempDir()
	// ① 工作区内的 .md
	if _, err := h.postWrite(context.Background(), ws, filepath.Join(ws, "README.md")); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	// ② 记忆目录内的 MEMORY.md —— **不再被忽略**（2026-09-30）。
	//
	// 它是索引而不是 topic，所以仍然**不补 modified**；但官方明确要求写完索引后
	// 测量限额（"After Claude writes to `MEMORY.md`…"），所以它走的是另一条分支。
	// 限额内时既不报错也不提醒，与"忽略"的表现一致——但那是因为额度够，不是因为
	// 没接。接线本身由 TestPostWriteIndexLimitsAreWired 断言。
	idx := filepath.Join(dir.Path, "MEMORY.md")
	if err := os.WriteFile(idx, []byte("- a.md\n"), 0o600); err != nil {
		t.Fatalf("预置索引: %v", err)
	}
	notice, err := h.postWrite(context.Background(), ws, idx)
	if err != nil {
		t.Fatalf("限额内的索引不该报错: %v", err)
	}
	if notice != "" {
		t.Fatalf("限额内不该有提醒: %q", notice)
	}
	// ③ 相对路径直接拒
	if _, err := h.postWrite(context.Background(), ws, "relative/x.md"); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	// 记忆目录里除 MEMORY.md 外不应多出任何文件（索引是我们自己预置的）
	entries, _ := os.ReadDir(dir.Path)
	if len(entries) != 1 || entries[0].Name() != "MEMORY.md" {
		t.Fatalf("非记忆写入产生了文件：%v", entries)
	}
}

// TestPostWriteEditFileAlsoStamps：edit_file 也走同一个钩子。
//
// 官方说的是"文件被写入时补 modified"，edit_file 同样是写入。若只接 write_file，
// 模型用 edit_file 改记忆时时间戳会静默停更，而索引和 topic 看上去完全正常。
func TestPostWriteEditFileAlsoStamps(t *testing.T) {
	h, dir, _ := memTopicDir(t)
	topic := filepath.Join(dir.Path, "y.md")
	if err := os.WriteFile(topic, []byte("---\ntype: project\n---\n\nold body\n"), 0o600); err != nil {
		t.Fatalf("预置: %v", err)
	}
	ws := t.TempDir()
	ft := builtin.NewFileTools(ws).
		WithPostWrite(func(p string) (string, error) { return h.postWrite(context.Background(), ws, p) })
	if err := callEditFile(ft, topic, "old body", "new body"); err != nil {
		t.Fatalf("edit_file 应成功: %v", err)
	}
	got, _ := os.ReadFile(topic)
	if !strings.Contains(string(got), "modified:") {
		t.Fatalf("edit_file 未触发补戳：\n%s", got)
	}
	if !strings.Contains(string(got), "new body") {
		t.Fatalf("edit 未生效：\n%s", got)
	}
}

// TestNoMemoryLeavesPromptByteIdentical：没有记忆时，系统提示词逐字节不变。
//

// TestInjectionEmptyWithoutIndex：无索引时注入的**只有目录说明**。
//
// 2026-09-30 从"完全不注入"翻转过来。冷启动是唯一一次能把记忆目录告诉模型的时机：
// 没有 MEMORY.md 就没有第一条记忆，而第一条只可能靠这段说明写出来。见
// memory/guidance.go 顶部。仍然为空的情形只有两种：开关关闭、无 git 身份。
func TestInjectionEmptyWithoutIndex(t *testing.T) {
	h, dir, _ := memTopicDir(t) // 目录存在但没有 MEMORY.md
	got := h.injection(context.Background(), t.TempDir())
	if got == "" {
		t.Fatal("无索引时仍须注入目录说明，否则模型永远写不出第一条记忆")
	}
	if !strings.Contains(got, dir.Path) {
		t.Fatalf("说明段必须含记忆目录路径 %q：\n%s", dir.Path, got)
	}
	if strings.Contains(got, "MEMORY.md") && strings.Contains(got, ".md\n-") {
		t.Fatalf("无索引时不得出现索引条目：\n%s", got)
	}
}

// TestInjectionReturnsIndexWhenPresent：有索引时必须注入其内容。
func TestInjectionReturnsIndexWhenPresent(t *testing.T) {
	h, dir, _ := memTopicDir(t)
	idx := filepath.Join(dir.Path, "MEMORY.md")
	if err := os.WriteFile(idx, []byte("- relay_tunnel_id.md\n- release_gate.md\n"), 0o600); err != nil {
		t.Fatalf("写索引: %v", err)
	}
	got := h.injection(context.Background(), t.TempDir())
	for _, want := range []string{"relay_tunnel_id.md", "release_gate.md"} {
		if !strings.Contains(got, want) {
			t.Fatalf("注入缺少 %q：\n%s", want, got)
		}
	}
}

// TestInjectionDisabledReturnsEmpty：开关关闭时注入为空。
func TestInjectionDisabledReturnsEmpty(t *testing.T) {
	h := newTestMemHost(t, t.TempDir(), false)
	if got := h.injection(context.Background(), t.TempDir()); got != "" {
		t.Fatalf("关闭开关时不该注入，得到 %q", got)
	}
}

// ---------- 小工具 ----------

// TestPostWriteIndexLimitsAreWired：写完 MEMORY.md 后，官方那一对限额行为必须真的发生。
//
// 这是 2026-09-30 补的回归。此前 CheckIndex 与 NearIndexNotice 都实现完整、测试齐全，
// 却没有调用点——因为 index.go 的注释断言"官方文档没有规定触发点"。原文第一句就写着
// "After Claude writes to `MEMORY.md`, Claude Code measures the file against the
// 200-line and 25KB read limits"（Wiki §5.3）。本测试把那条链钉死。
//
// 两条通道分开断言，因为官方用词不同：
//
//	接近限额 → "reminds Claude to shorten it"  → notice（非错误）
//	已经超限 → "returns an error…"              → err（且写本身仍然成功）
//
// 混用会被本测试抓住：把提醒塞进 err 会让模型以为写失败并重试。
func TestPostWriteIndexLimitsAreWired(t *testing.T) {
	h, dir, _ := memTopicDir(t)
	ws := t.TempDir()
	ft := builtin.NewFileTools(ws).
		WithPostWrite(func(p string) (string, error) { return h.postWrite(context.Background(), ws, p) })
	idx := filepath.Join(dir.Path, "MEMORY.md")

	// —— 接近限额（160 行 ≥ 200×3/4）：提醒走 notice，**不报错** ——
	var near strings.Builder
	for i := 0; i < 160; i++ {
		near.WriteString("- t" + strconv.Itoa(i) + ".md\n")
	}
	res, err := callWriteFileResult(ft, idx, near.String())
	if err != nil {
		t.Fatalf("接近限额时写入不该失败: %v", err)
	}
	if !strings.Contains(res, "接近 200 行 / 25KB") {
		t.Fatalf("接近限额应给出精简提醒，得到: %q", res)
	}

	// —— 超过限额（250 行）：**写仍然成功**，但回错误要求重写索引 ——
	var over strings.Builder
	for i := 0; i < 250; i++ {
		over.WriteString("- t" + strconv.Itoa(i) + ".md\n")
	}
	res, err = callWriteFileResult(ft, idx, over.String())
	if err == nil {
		t.Fatalf("超限应回错误要求重写索引，得到成功: %q", res)
	}
	if !strings.Contains(err.Error(), "MEMORY.md") || !strings.Contains(err.Error(), "重写索引") {
		t.Fatalf("错误信息应说明是索引超限: %v", err)
	}
	// 官方："the write still succeeds" —— 报错之后文件必须**真的写进去了**。
	body, rerr := os.ReadFile(idx)
	if rerr != nil {
		t.Fatalf("超限时写入仍应成功: %v", rerr)
	}
	if !strings.Contains(string(body), "- t249.md") {
		t.Fatal("超限时索引没有真正落盘——官方明确 the write still succeeds")
	}
}

func callWriteFileResult(ft *builtin.FileTools, abs, content string) (string, error) {
	for _, tl := range ft.Tools() {
		if tl.Name() != "write_file" {
			continue
		}
		args := `{"path":` + jsonQuote(abs) + `,"content":` + jsonQuote(content) + `}`
		ctx := context.Background()
		if err := tl.ValidParams(ctx, tl.Name(), args); err != nil {
			return "", err
		}
		return tl.Call(ctx, tl.Name(), args)
	}
	return "", os.ErrNotExist
}

func callWriteFile(ft *builtin.FileTools, abs, content string) error {
	for _, tl := range ft.Tools() {
		if tl.Name() != "write_file" {
			continue
		}
		args := `{"path":` + jsonQuote(abs) + `,"content":` + jsonQuote(content) + `}`
		ctx := context.Background()
		if err := tl.ValidParams(ctx, tl.Name(), args); err != nil {
			return err
		}
		_, err := tl.Call(ctx, tl.Name(), args)
		return err
	}
	return os.ErrNotExist
}

func callEditFile(ft *builtin.FileTools, abs, oldS, newS string) error {
	for _, tl := range ft.Tools() {
		if tl.Name() != "edit_file" {
			continue
		}
		args := `{"path":` + jsonQuote(abs) +
			`,"old_string":` + jsonQuote(oldS) + `,"new_string":` + jsonQuote(newS) + `}`
		ctx := context.Background()
		if err := tl.ValidParams(ctx, tl.Name(), args); err != nil {
			return err
		}
		_, err := tl.Call(ctx, tl.Name(), args)
		return err
	}
	return os.ErrNotExist
}

func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// TestEmptyStateAssertionSkipsOnlyForWiredPackage：基线断言的跳过是**有条件的**。
//
// Step 3/4 接线后 desktop/bridge 成为 importer，基线在产品树上不成立是已知且有意的，
// 装置应当跳过。但护栏要防的是"不知道自己在测什么"，所以**出现任何其他 importer
// 仍然必须硬失败**。这条测试把两个分支都钉住 —— 否则将来某次意外接线会被这个
// 跳过静默吞掉，而那正是"基线数字悄悄变成测实现"的路径。
func TestEmptyStateAssertionSkipsOnlyForWiredPackage(t *testing.T) {
	// 前提：接线已完成，desktop/bridge 就是那个 importer
	offenders := baselineMemoryImporters(t)
	if len(offenders) == 0 {
		t.Skip("当前树未接线，本测试的前提不成立")
	}
	// 只允许 desktop/bridge 一个 importer
	if len(offenders) != 1 || offenders[0] != "github.com/seven7628/hai-harness/desktop/bridge" {
		t.Fatalf("接线白名单被打破，importer = %v", offenders)
	}
}
