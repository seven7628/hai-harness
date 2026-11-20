package workdir

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestNewNormalizesRoot New 归一化工作区根（相对路径按进程 cwd 展开）。
func TestNewNormalizesRoot(t *testing.T) {
	wd := New("relative-root")
	if !filepath.IsAbs(wd.Root()) {
		t.Fatalf("root 必须是绝对路径：%q", wd.Root())
	}
	if wd.Root() != filepath.Clean(wd.Root()) {
		t.Fatalf("root 必须是 Clean 过的：%q", wd.Root())
	}
}

// TestCurrentDefaultsToRoot 没有 cd 过的分区，当前目录 = 工作区根。
func TestCurrentDefaultsToRoot(t *testing.T) {
	root := t.TempDir()
	wd := New(root)
	if got := wd.Current(MainKey); got != root {
		t.Fatalf("初始 cwd = %q，want %q", got, root)
	}
	// 空键等价于主分区（归一化口径）。
	if got := wd.Current(""); got != root {
		t.Fatalf("空键应等价主分区：%q，want %q", got, root)
	}
}

// TestCurrentIsPerPartition 分区之间互不干扰：主 agent 的 cd 不影响子 agent。
//
// 这条是本包存在的理由：主 agent 与子 agent 共用同一个工具引擎，
// cwd 若只有一份，两个 agent 会互相踩。
func TestCurrentIsPerPartition(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := New(root)

	wd.Set(MainKey, sub)
	if got := wd.Current(MainKey); got != sub {
		t.Fatalf("主分区 cwd = %q，want %q", got, sub)
	}
	if got := wd.Current("task-123"); got != root {
		t.Fatalf("子 agent 分区被主 agent 影响了：%q，want %q", got, root)
	}
}

// TestCurrentFallsBackWhenDirGone 记录过的目录被删掉 ⇒ 回落到工作区根
// （而不是让后续每条命令都以"目录不存在"失败）。
func TestCurrentFallsBackWhenDirGone(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := New(root)
	wd.Set(MainKey, sub)
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	if got := wd.Current(MainKey); got != root {
		t.Fatalf("目录没了应回落到根：%q，want %q", got, root)
	}
}

// TestSetIgnoresEmpty 空目录 = "什么都不记"（回到根用 Reset）。
func TestSetIgnoresEmpty(t *testing.T) {
	root := t.TempDir()
	wd := New(root)
	wd.Set(MainKey, "")
	if got := wd.Current(MainKey); got != root {
		t.Fatalf("空目录不应改变 cwd：%q，want %q", got, root)
	}
}

// TestReset 把分区复位回工作区根。
func TestReset(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := New(root)
	wd.Set(MainKey, sub)
	wd.Reset(MainKey)
	if got := wd.Current(MainKey); got != root {
		t.Fatalf("Reset 后 cwd = %q，want %q", got, root)
	}
}

// TestResolveAnchorsToPartitionCwd 相对路径锚到**该分区的**当前目录。
func TestResolveAnchorsToPartitionCwd(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := New(root)
	wd.Set(MainKey, sub)

	if got := wd.Resolve(MainKey, "a/b.txt"); got != filepath.Join(sub, "a/b.txt") {
		t.Fatalf("相对路径应锚到分区 cwd：%q，want %q", got, filepath.Join(sub, "a/b.txt"))
	}
	// 绝对路径原样 Clean（边界由工具层管，本包只管锚点）。
	if got := wd.Resolve(MainKey, "/tmp/x/../y.txt"); got != "/tmp/y.txt" {
		t.Fatalf("绝对路径应原样 Clean：%q", got)
	}
	// 另一个分区不受影响。
	if got := wd.Resolve("task-9", "a/b.txt"); got != filepath.Join(root, "a/b.txt") {
		t.Fatalf("别的分区应锚到根：%q，want %q", got, filepath.Join(root, "a/b.txt"))
	}
}

// TestKeyFromContext ctx 里的分区键：没设置 ⇒ MainKey（永远非空）。
func TestKeyFromContext(t *testing.T) {
	if got := KeyFrom(context.Background()); got != MainKey {
		t.Fatalf("未设置时应为 MainKey：%q", got)
	}
	ctx := WithKey(context.Background(), "task-123")
	if got := KeyFrom(ctx); got != "task-123" {
		t.Fatalf("WithKey/KeyFrom 往返失败：%q", got)
	}
	// 空键 ⇒ 主分区。
	if got := KeyFrom(WithKey(context.Background(), "")); got != MainKey {
		t.Fatalf("空键应归一化成主分区：%q", got)
	}
}
