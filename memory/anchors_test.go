package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Dream v1.5 的锚点核对：只判"引用的文件还在不在"，且**只读**。

// bindRepoForTest 直接写绑定文件（跳过 git：测试不该依赖真实仓库身份）。
func bindRepoForTest(t *testing.T, d Dir, root string) {
	t.Helper()
	dir := filepath.Join(d.Path, stateDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repo.json"),
		[]byte(`{"repo_id":"`+d.ID+`","root":"`+root+`","bound":"2026-11-09T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func repoCfg(files []string) Config {
	return Config{RepoFiles: func(string) ([]string, error) { return files, nil }}
}

func TestCheckAnchorsResolvesThreeWays(t *testing.T) {
	d := memDir(t)
	repo := t.TempDir()
	bindRepoForTest(t, d, repo)
	writeTopicFile(t, d, "a.md", "---\ntype: project\n---\n\n"+
		"改动在 `internal/api/router.go:66`，另见 `router.go`（唯一同名）。\n")
	writeTopicFile(t, d, "b.md", "---\ntype: project\n---\n\n"+
		"这里引用了 `gone/file.go` 与 `nope.go`。\n")

	cfg := repoCfg([]string{"internal/api/router.go", "cmd/x/router.go", "main.go"})
	rep, err := cfg.CheckAnchors(d)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unbound || rep.RepoFiles != 3 {
		t.Fatalf("绑定/文件数异常：%+v", rep)
	}
	// a.md：完整路径命中；basename `router.go` 有两个同名 ⇒ 仍算命中（歧义不是失效证据）。
	// b.md：两条都该判失效。
	var deadFiles []string
	for _, x := range rep.Dead {
		deadFiles = append(deadFiles, x.File+"|"+x.Ref)
	}
	got := strings.Join(deadFiles, ",")
	if strings.Contains(got, "a.md|router.go") {
		t.Fatalf("同名文件存在时不该判失效：%s", got)
	}
	if !strings.Contains(got, "b.md|gone/file.go") || !strings.Contains(got, "b.md|nope.go") {
		t.Fatalf("缺失引用应判失效：%s", got)
	}
	if strings.Contains(got, "internal/api/router.go") {
		t.Fatalf("存在的完整路径不该判失效：%s", got)
	}
}

// 没绑定仓库时如实报 Unbound，**不能**报"全部正常"。
func TestCheckAnchorsUnboundIsExplicit(t *testing.T) {
	d := memDir(t)
	writeTopicFile(t, d, "a.md", "---\ntype: project\n---\n\n引用 `x.go`\n")
	rep, err := Config{}.CheckAnchors(d)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Unbound {
		t.Fatal("没绑定时应报 Unbound")
	}
	r, _ := Config{}.Dream(d)
	if !strings.Contains(r.Markdown(timeZero()), "未核对") {
		t.Fatalf("报告必须写明未核对，而不是沉默：\n%s", r.Markdown(timeZero()))
	}
}

// 绑定 + 全部命中 → 报告说"全部可核对"。
func TestDreamAnchorAllGood(t *testing.T) {
	d := memDir(t)
	bindRepoForTest(t, d, t.TempDir())
	writeIndex(t, d, "# Memory Index\n- [a.md](a.md) — x\n")
	writeTopicFile(t, d, "a.md", "---\ntype: project\nmodified: 2026-11-01T00:00:00+08:00\n---\n\n见 `main.go`\n")

	r, err := repoCfg([]string{"main.go"}).Dream(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Anchors.Dead) != 0 {
		t.Fatalf("不该有失效锚点：%v", r.Anchors.Dead)
	}
	if !strings.Contains(r.Markdown(timeZero()), "全部可核对") {
		t.Fatalf("应写明全部可核对：\n%s", r.Markdown(timeZero()))
	}
}

// 锚点核对同样不许写盘。
func TestCheckAnchorsWritesNothing(t *testing.T) {
	d := memDir(t)
	repo := t.TempDir()
	bindRepoForTest(t, d, repo)
	writeTopicFile(t, d, "a.md", "---\ntype: project\n---\n\n`main.go`\n")
	before := snapshot(t, d.Path)
	if _, err := repoCfg([]string{"main.go"}).CheckAnchors(d); err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, d.Path)
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("%s 被改了", k)
		}
	}
}

// BindRepo 幂等：同一工作区绑两次结果一致（不产生第二份文件）。
func TestBindRepoIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := t.TempDir()
	cfg := Config{Resolve: &Resolver{Run: func(ctx context.Context, dir string, args ...string) (string, error) {
		return "", os.ErrNotExist // 让 git 解析失败，用 ProjectDirName 兜底
	}}, ProjectDirName: "probe"}
	d, err := cfg.BindRepo(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	root, ok := cfg.RepoRoot(d)
	if !ok || root != work {
		t.Fatalf("绑定读回失败：%q %v", root, ok)
	}
	if _, err := cfg.BindRepo(context.Background(), work); err != nil {
		t.Fatalf("第二次绑定应幂等：%v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(d.Path, stateDirName))
	n := 0
	for _, e := range entries {
		if e.Name() == "repo.json" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("repo.json 应只有一份，得到 %d", n)
	}
}

// timeZero 给 Markdown 一个固定时间，避免测试与时钟耦合。
func timeZero() time.Time { return time.Unix(0, 0).UTC() }
