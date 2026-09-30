package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeResolver 返回一个固定 ID 的身份解析器（不走真实 git，让 Bootstrap 测试自足）。
func fakeResolver(id string) *Resolver {
	return &Resolver{Run: func(_ context.Context, _ string, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "rev-parse --show-toplevel":
			return "/tmp/proj\n", nil
		case "remote get-url origin":
			return "https://github.com/o/" + id + ".git\n", nil
		}
		return "", nil
	}}
}

// bootConfig 一次组装好：固定根目录、固定身份、固定时间、计数 I/O。
func bootConfig(t *testing.T, reads *int32) Config {
	t.Helper()
	clearEnv(t)
	c := Config{Resolve: fakeResolver("proj"), Directory: t.TempDir()}.withDefaults()
	c.Now = func() time.Time { return fixedNow }
	real := c.ReadFile
	if reads != nil {
		c.ReadFile = func(p string) ([]byte, error) {
			atomic.AddInt32(reads, 1)
			return real(p)
		}
	}
	return c
}

// 正常路径：解析目录 + 注入索引文本。
func TestBootstrap_InjectsIndex(t *testing.T) {
	var reads int32
	c := bootConfig(t, &reads)
	res := Bootstrap(context.Background(), t.TempDir(), c)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.Resolved || res.Dir == nil {
		t.Fatalf("应解析出目录：%+v", res)
	}
	if res.Dir.ID != "github.com-o-proj" {
		t.Fatalf("ID = %q", res.Dir.ID)
	}
	// 目录由覆盖后的根 + repos/<id> 组成。
	if want := filepath.Join(c.Directory, "repos", "github.com-o-proj"); res.Dir.Path != want {
		t.Fatalf("Path = %q, want %q", res.Dir.Path, want)
	}
	// 先在那个目录里放一份索引，再跑一次：应当注入。
	if err := os.MkdirAll(res.Dir.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(res.Dir.Path, IndexFileName), []byte("- user_role.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res2 := Bootstrap(context.Background(), t.TempDir(), c)
	if res2.Err != nil {
		t.Fatal(res2.Err)
	}
	if !strings.Contains(res2.Injection.Text, "user_role.md") {
		t.Fatalf("未注入索引：%+v", res2.Injection)
	}
	if res2.Injection.Reason != "" {
		t.Fatalf("Reason = %q, want 空", res2.Injection.Reason)
	}
}

// 目录不存在（首次使用的仓库）→ **注入目录说明**、不报错。
//
// 2026-09-30：这一条从"不注入"翻转过来。首次使用正是唯一一次能把记忆目录告诉模型的
// 时机——没有 MEMORY.md 就没有"第一条"，而"第一条"只可能靠这段说明写出来。
// 判据是"说明里含目录路径"，不是"注入了索引"。
func TestBootstrap_NoMemoryYet(t *testing.T) {
	c := bootConfig(t, nil)
	res := Bootstrap(context.Background(), t.TempDir(), c)
	if res.Err != nil {
		t.Fatalf("没有记忆不该报错：%v", res.Err)
	}
	if !res.Resolved || res.Dir == nil {
		t.Fatal("目录身份仍应解析出来（Step 4 需要它）")
	}
	if res.Injection.Empty() || !strings.Contains(res.Injection.Text, res.Dir.Path) {
		t.Fatalf("首次使用须注入目录说明：%+v", res.Injection)
	}
	if res.Injection.Reason != "no-index" {
		t.Fatalf("Reason = %q", res.Injection.Reason)
	}
	// 官方**不创建**这个目录：无记忆与有记忆必须可区分。
	// （首次写入时由 write_file 的 replaceLocked 建父目录，见 tools/builtin/lock.go。）
	if _, err := os.Stat(res.Dir.Path); err == nil {
		t.Fatal("Bootstrap 凭空建出了记忆目录")
	}
}

// 身份解析失败（无 git）→ 不注入；错误带回给调用方，但不阻断会话。
func TestBootstrap_NoIdentity(t *testing.T) {
	clearEnv(t)
	c := Config{Directory: t.TempDir(), Resolve: &Resolver{
		Run: func(context.Context, string, ...string) (string, error) {
			return "", errNotARepo
		},
	}}
	res := Bootstrap(context.Background(), t.TempDir(), c)
	if res.Injection.Text != "" {
		t.Fatalf("无身份时不得注入：%q", res.Injection.Text)
	}
	if res.Injection.Reason != "no-identity" {
		t.Fatalf("Reason = %q", res.Injection.Reason)
	}
	if res.Resolved || res.Dir != nil {
		t.Fatal("无身份时不应有 Dir")
	}
	if !errors.Is(res.Err, ErrNoGit) {
		t.Fatalf("err = %v, 期望包装 ErrNoGit", res.Err)
	}
}

// 开关关闭的**完整链路**：不解析身份、不读盘、不注入、不写盘。
//
// 这一条是"用户关掉记忆"的全部语义，必须整体成立：
// 分两次断言是为了同时证明「没有读盘」与「没有 git 子进程」。
func TestBootstrap_DisabledFullChain(t *testing.T) {
	clearEnv(t)
	var gitCalls, reads, writes int32
	cfg := Config{
		Enabled:   Bool(false),
		Directory: t.TempDir(),
		Resolve:   fakeResolverWithCounter("proj", &gitCalls),
		ReadFile:  func(string) ([]byte, error) { atomic.AddInt32(&reads, 1); return nil, nil },
		WriteFile: func(string, []byte, os.FileMode) error { atomic.AddInt32(&writes, 1); return nil },
		Rename:    func(string, string) error { return nil },
		Remove:    func(string) error { return nil },
		Now:       func() time.Time { return fixedNow },
	}

	res := Bootstrap(context.Background(), t.TempDir(), cfg)
	if res.Err != nil {
		t.Fatalf("关闭开关不该报错：%v", res.Err)
	}
	if res.Injection.Text != "" || !res.Injection.Empty() {
		t.Fatalf("关闭开关时 Text 必须为空：%q", res.Injection.Text)
	}
	if res.Injection.Reason != "disabled" || res.Injection.Enabled {
		t.Fatalf("inj = %+v", res.Injection)
	}
	if res.Resolved || res.Dir != nil {
		t.Fatalf("关闭开关时不该解析目录：%+v", res)
	}
	if gitCalls != 0 {
		t.Fatalf("关闭开关时仍跑了 %d 次 git（身份解析必须前置短路）", gitCalls)
	}
	if reads != 0 || writes != 0 {
		t.Fatalf("关闭开关时读 %d / 写 %d（必须都是 0）", reads, writes)
	}

	// 关闭状态下 Step 4 同样整条不跑。
	if cfg.Disabled() != true {
		t.Fatal("Disabled() 应为 true")
	}
	dir := Dir{Path: filepath.Join(cfg.Directory, "repos", "id")}
	if r := cfg.StampModified(dir, filepath.Join(dir.Path, "x.md")); r.Stamped != StampedSkipped || r.Err != nil {
		t.Fatalf("关闭开关时 Step 4 = %+v", r)
	}
	if r := cfg.SetMemoryField(dir, filepath.Join(dir.Path, "x.md"), "type", "user"); r.Stamped != StampedSkipped {
		t.Fatalf("关闭开关时 SetMemoryField = %+v", r)
	}
	if writes != 0 {
		t.Fatalf("关闭开关时发生 %d 次写盘（必须为 0）", writes)
	}
}

// 关闭开关**不删除**已存在的记忆文件（官方语义：开关只控制加载）。
func TestBootstrap_DisabledKeepsExistingFiles(t *testing.T) {
	clearEnv(t)
	var writes int32
	c := Config{Enabled: Bool(false), Directory: t.TempDir(),
		WriteFile: func(string, []byte, os.FileMode) error { atomic.AddInt32(&writes, 1); return nil }}
	dir := Dir{Path: filepath.Join(c.Directory, "repos", "id")}
	if err := os.MkdirAll(dir.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	idx := filepath.Join(dir.Path, IndexFileName)
	topic := filepath.Join(dir.Path, "user_role.md")
	if err := os.WriteFile(idx, []byte("- user_role.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(topic, []byte(topicWithFM), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := Bootstrap(context.Background(), t.TempDir(), c); !res.Injection.Empty() {
		t.Fatalf("关闭开关仍注入了：%q", res.Injection.Text)
	}
	for _, p := range []string{idx, topic} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("关闭开关删除了记忆文件 %q：%v（官方语义：删数据是用户的显式操作）", p, err)
		}
	}
	// 关闭状态下补时间戳也不许改内容。
	before := mustRead(t, topic)
	if r := c.StampModified(dir, topic); r.Stamped != StampedSkipped {
		t.Fatalf("关闭开关仍打了时间戳：%+v", r)
	}
	if mustRead(t, topic) != before {
		t.Fatal("关闭开关却改了记忆文件")
	}
	if writes != 0 {
		t.Fatalf("关闭开关时写盘 %d 次", writes)
	}
}

// 环境变量 autoMemoryEnabled=0 走同一条关闭链路（CI 用它固定空状态）。
func TestBootstrap_DisabledByEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAutoMemoryEnabled, "0")
	var gitCalls int32
	c := Config{Directory: t.TempDir(), Resolve: fakeResolverWithCounter("proj", &gitCalls)}
	res := Bootstrap(context.Background(), t.TempDir(), c)
	if res.Injection.Reason != "disabled" || gitCalls != 0 {
		t.Fatalf("inj = %+v, gitCalls = %d", res.Injection, gitCalls)
	}
}

// CLAUDE_CODE_PROJECT_DIR_NAME：强制 <project> 名，多仓库共用一份记忆。
// 官方用途就是跨仓库共享，这里固定它的语义不被目录覆盖破坏。
func TestBootstrap_ProjectDirNameForcesSharedMemory(t *testing.T) {
	clearEnv(t)
	root := t.TempDir()
	env := Config{Directory: root, ProjectDirName: "whole-product"}
	got, err := resolveDir(context.Background(), "/whatever", env)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "repos", "whole-product"); got.Path != want {
		t.Fatalf("Path = %q, want %q", got.Path, want)
	}
	// 换工作目录（= 换仓库）仍是同一份记忆。
	t.Setenv(EnvProjectDirName, "whole-product")
	got2, err := resolveDir(context.Background(), t.TempDir(), Config{Directory: root})
	if err != nil {
		t.Fatal(err)
	}
	if got2.Path != got.Path {
		t.Fatalf("强制名下不同仓库应共用一份记忆：%q vs %q", got2.Path, got.Path)
	}
	// 环境变量名（官方拼写）也要生效——这是 CI 与调试实际会设的那个名字。
	t.Setenv(EnvProjectDirName, "from-env")
	fromEnv, err := resolveDir(context.Background(), t.TempDir(), Config{Directory: root})
	if err != nil {
		t.Fatal(err)
	}
	if fromEnv.ID != "from-env" {
		t.Fatalf("环境变量未生效：ID = %q", fromEnv.ID)
	}
	// Config 优先于环境变量。
	both, err := resolveDir(context.Background(), t.TempDir(),
		Config{Directory: root, ProjectDirName: "from-cfg"})
	if err != nil {
		t.Fatal(err)
	}
	if both.ID != "from-cfg" {
		t.Fatalf("Config.ProjectDirName 应优先：ID = %q", both.ID)
	}

	// 名字里的路径分隔符被压掉（目录名不能带分隔符，否则会逃出 repos/）。
	// 注意 `..` 保留为合法字符（sanitizeSegment 的规则），所以结果落在
	// repos/a-..-..-b 这样一个**字面**目录里——仍然不会逃出 repos/。
	slash, err := resolveDir(context.Background(), "/x", Config{Directory: root, ProjectDirName: "a/../../b"})
	if err != nil {
		t.Fatal(err)
	}
	if slash.Path != filepath.Join(root, "repos", "a-..-..-b") {
		t.Fatalf("Path = %q（分隔符未被压掉）", slash.Path)
	}
	// 全是分隔符 → 无法构成 ID。
	if _, err := resolveDir(context.Background(), "/x", Config{Directory: root, ProjectDirName: "///"}); err == nil {
		t.Fatal("非法名字应报错")
	}
}

// autoMemoryDirectory 改根时，索引仍从**新的**根下读（不是旧的 ~/.go-code）。
func TestBootstrap_DirectoryOverrideMovesIndex(t *testing.T) {
	var reads int32
	c := bootConfig(t, &reads)
	dir, err := resolveDir(context.Background(), t.TempDir(), c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir.Path, IndexFileName), []byte("- only-in-alt-root.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := Bootstrap(context.Background(), t.TempDir(), c)
	if !strings.Contains(res.Injection.Text, "only-in-alt-root.md") {
		t.Fatalf("未从覆盖根读到索引：%+v", res.Injection)
	}
}

// 索引读盘失败（权限/IO）→ 错误回传，且注入为空。
func TestBootstrap_IndexReadErrorPropagates(t *testing.T) {
	clearEnv(t)
	var reads int32
	c := bootConfig(t, &reads)
	dir, err := resolveDir(context.Background(), t.TempDir(), c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	c.ReadFile = func(string) ([]byte, error) { atomic.AddInt32(&reads, 1); return nil, errReadFailed }
	res := Bootstrap(context.Background(), t.TempDir(), c)
	if res.Err == nil {
		t.Fatal("读盘失败必须回传（不能静默当成'没有记忆'）")
	}
	if res.Injection.Text != "" {
		t.Fatalf("读盘失败时不得注入：%q", res.Injection.Text)
	}
	// 目录仍解析出来（Step 4 还需要它判路径）。
	if res.Dir == nil {
		t.Fatal("读索引失败时 Dir 仍应返回")
	}
}

// ——— 测试脚手架 ———

type constErr string

func (e constErr) Error() string { return string(e) }

const (
	errNotARepo   = constErr("fatal: not a git repository")
	errReadFailed = constErr("读盘失败（模拟 IO 故障）")
)

func fakeResolverWithCounter(id string, calls *int32) *Resolver {
	return &Resolver{Run: func(_ context.Context, _ string, args ...string) (string, error) {
		atomic.AddInt32(calls, 1)
		if strings.Join(args, " ") == "rev-parse --show-toplevel" {
			return "/tmp/proj\n", nil
		}
		return "https://github.com/o/" + id + ".git\n", nil
	}}
}
