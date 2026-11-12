package main

// 内置技能同步（ego-browser / 办公技能）的符号链接口径（2026-09）：
// 源侧**跟随链接、复制内容**（副本自包含：照抄链接会指向 app bundle 内部路径，
// 换版本/换机器即断链）；断链跳过；链接环报错而不无限递归。
// 修复前 DirEntry.IsDir() 对 symlink 恒 false → 链接子目录被当文件 os.ReadFile
// → EISDIR 让整次同步失败。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustSymlinkBridge 建符号链接；平台不支持时跳过用例。
func mustSymlinkBridge(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Skipf("symlink 不可用（Windows 权限等）: %v", err)
	}
}

// TestCopyDirTreeFollowsSymlinkedDir 链接子目录按内容复制成真实目录（副本自包含）。
func TestCopyDirTreeFollowsSymlinkedDir(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := os.MkdirAll(filepath.Join(src, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 单一副本目录（真实内容）+ 源里的链接子目录
	shared := filepath.Join(base, "vendor", "references")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "guide.md"), []byte("guide\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSymlinkBridge(t, shared, filepath.Join(src, "references"))
	// 链接文件同样按内容复制，且保留可执行位
	script := filepath.Join(base, "vendor", "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustSymlinkBridge(t, script, filepath.Join(src, "scripts", "run.sh"))

	dst := filepath.Join(base, "dst")
	if err := copyDirTree(src, dst); err != nil {
		t.Fatalf("同步应跟随链接: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(dst, "references"))
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		t.Fatalf("链接子目录应复制成真实目录: %v (%v)", fi, err)
	}
	data, err := os.ReadFile(filepath.Join(dst, "references", "guide.md"))
	if err != nil || string(data) != "guide\n" {
		t.Fatalf("链接子目录内容未复制: %q (%v)", data, err)
	}
	if fi, err := os.Lstat(filepath.Join(dst, "scripts", "run.sh")); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("链接文件应复制成真实文件: %v (%v)", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "scripts", "run.sh")); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("可执行位应保留（脚本不能被复制成不可执行）: %v (%v)", fi, err)
	}
}

// TestCopyDirTreeSkipsBrokenSymlink 断链跳过，不影响其余内容。
func TestCopyDirTreeSkipsBrokenSymlink(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSymlinkBridge(t, filepath.Join(base, "gone"), filepath.Join(src, "broken"))

	dst := filepath.Join(base, "dst")
	if err := copyDirTree(src, dst); err != nil {
		t.Fatalf("断链不应让同步失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "SKILL.md")); err != nil {
		t.Fatalf("正常文件应复制: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "broken")); !os.IsNotExist(err) {
		t.Fatalf("断链不应复制: %v", err)
	}
}

// TestCopyDirTreeSymlinkLoopErrors 目录链接指回祖先 → 报错（而非无限递归挂死）。
func TestCopyDirTreeSymlinkLoopErrors(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	sub := filepath.Join(src, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	mustSymlinkBridge(t, src, filepath.Join(sub, "loop"))

	err := copyDirTree(src, filepath.Join(base, "dst"))
	if err == nil || !strings.Contains(err.Error(), "链接环") {
		t.Fatalf("链接环应报错并停止递归: %v", err)
	}
}

// TestEnsureOfficeSkillsSyncedFollowsSymlinkedSubdir 端到端：源技能的子目录是链接
// → 目标技能仍拿到完整内容（旧实现在这里 EISDIR 整次同步失败）。
func TestEnsureOfficeSkillsSyncedFollowsSymlinkedSubdir(t *testing.T) {
	home := t.TempDir()
	base := t.TempDir()
	src := filepath.Join(base, "office-skills")
	mkOfficeSkill(t, src, "docx")
	shared := filepath.Join(base, "vendor", "references")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "guide.md"), []byte("guide\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSymlinkBridge(t, shared, filepath.Join(src, "docx", "references"))

	t.Setenv("HOME", home)
	t.Setenv(officeSkillsDirEnv, src)
	if err := ensureOfficeSkillsSynced(); err != nil {
		t.Fatalf("sync err: %v", err)
	}
	dst := filepath.Join(home, ".agents", "skills", "docx", "references", "guide.md")
	if data, err := os.ReadFile(dst); err != nil || string(data) != "guide\n" {
		t.Fatalf("链接子目录内容应落到目标技能里: %q (%v)", data, err)
	}
}
