package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceRegistryAddListRemove(t *testing.T) {
	dir := t.TempDir()
	reg := newWorkspaceRegistry(filepath.Join(dir, "workspaces.json"))

	a := filepath.Join(dir, "proj-a")
	b := filepath.Join(dir, "proj-b")
	mkdir(t, a)
	mkdir(t, b)

	e, err := reg.add(a)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if e.Title != "proj-a" || e.Path != a {
		t.Fatalf("entry = %+v", e)
	}
	// 幂等：重复添加不报错、不重复
	if _, err := reg.add(a); err != nil {
		t.Fatalf("重复添加应幂等：%v", err)
	}
	if got := len(reg.list()); got != 1 {
		t.Fatalf("重复添加后应为 1 条，得到 %d", got)
	}

	if _, err := reg.add(b); err != nil {
		t.Fatal(err)
	}
	if got := len(reg.list()); got != 2 {
		t.Fatalf("应为 2 条，得到 %d", got)
	}

	// 持久化
	reg2 := newWorkspaceRegistry(reg.path)
	if got := len(reg2.list()); got != 2 {
		t.Fatalf("重启后应为 2 条，得到 %d", got)
	}

	if !reg.remove(a) {
		t.Fatal("remove 应返回 true")
	}
	if reg.remove(a) {
		t.Fatal("重复 remove 应返回 false")
	}
	if got := len(reg.list()); got != 1 {
		t.Fatalf("删除后应为 1 条，得到 %d", got)
	}
}

func TestWorkspaceRegistryRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	reg := newWorkspaceRegistry(filepath.Join(dir, "workspaces.json"))

	cases := []string{"", "relative/path", filepath.Join(dir, "nope")}
	for _, c := range cases {
		if _, err := reg.add(c); err == nil {
			t.Fatalf("%q 应被拒绝", c)
		}
	}
	// 文件不是目录
	f := filepath.Join(dir, "file.txt")
	writeFile(t, f, "x")
	if _, err := reg.add(f); err == nil {
		t.Fatal("普通文件应被拒绝")
	}
}

func TestWorkspaceRegistrySyncIsAddOnly(t *testing.T) {
	dir := t.TempDir()
	reg := newWorkspaceRegistry(filepath.Join(dir, "workspaces.json"))

	// 手机先加了一个
	phone := filepath.Join(dir, "phone-added")
	mkdir(t, phone)
	if _, err := reg.add(phone); err != nil {
		t.Fatal(err)
	}

	// 桌面 localStorage 的旧清单不含它 → sync 不能把它删掉
	old := filepath.Join(dir, "desktop-old")
	mkdir(t, old)
	list := reg.sync([]string{old, phone})
	if len(list) != 2 {
		t.Fatalf("sync 后应为 2 条（只增不减），得到 %d", len(list))
	}
	paths := map[string]bool{}
	for _, e := range list {
		paths[e.Path] = true
	}
	if !paths[phone] || !paths[old] {
		t.Fatalf("sync 结果缺项：%+v", paths)
	}
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

// 落盘失败必须回滚内存条目：否则 list() 会列出重启后并不存在的幽灵工作区。
func TestWorkspaceRegistryAddRollsBackOnSaveFailure(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	mkdir(t, proj)
	// path 指向一个目录：writeMobileAtomic 的 rename 必失败。
	badPath := filepath.Join(dir, "is-a-dir")
	mkdir(t, badPath)
	reg := newWorkspaceRegistry(badPath)

	if _, err := reg.add(proj); err == nil {
		t.Fatal("save failure must surface an error")
	}
	if got := len(reg.list()); got != 0 {
		t.Fatalf("failed add left %d phantom entries: %+v", got, reg.list())
	}
}

// sync/update/remove 的落盘失败也必须回滚内存（不能列得出来、重启就没了）。
func TestWorkspaceRegistryMutationsRollBackOnSaveFailure(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	mkdir(t, proj)
	bad := filepath.Join(dir, "is-a-dir")
	mkdir(t, bad)
	reg := newWorkspaceRegistry(filepath.Join(dir, "workspaces.json"))
	if _, err := reg.add(proj); err != nil {
		t.Fatal(err)
	}
	// 让后续 save 必失败。
	reg.path = bad

	title := "新标题"
	if _, ok := reg.update(proj, &title, nil); ok {
		t.Fatal("update must fail when save fails")
	}
	if got := reg.list()[0].Title; got != "proj" {
		t.Fatalf("update rollback failed: title = %q", got)
	}
	reg.sync([]string{filepath.Join(dir, "other")})
	if got := len(reg.list()); got != 1 {
		t.Fatalf("sync rollback failed: entries = %d", got)
	}
	if reg.remove(proj) {
		t.Fatal("remove must fail when save fails")
	}
	if got := len(reg.list()); got != 1 {
		t.Fatalf("remove rollback failed: entries = %d", got)
	}
}
