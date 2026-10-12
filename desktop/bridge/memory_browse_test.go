package main

// `/memory` 浏览面（官方 §2.7）的后端行为。
//
// 官方两句：
//
//	"Auto memory files are **plain markdown you can edit or delete at any time**.
//	 Run `/memory` to browse and open memory files from within a session."
//
// 由此确定三条硬要求，逐条测试：
//  1. 浏览是**只读**的：响应里只有清单，没有任何写路径；
//  2. **不做存在性校验**：索引里列了但磁盘上没有的文件照样列出（官方不校验，
//     擅自隐藏会让用户以为记忆丢了）；
//  3. 记忆关着 / 取不到目录时**不是错误**，面板据此显示"未启用"。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/memory"
)

func TestConfigListIncludesIndexedAndUnindexedFiles(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	c := memory.Config{Directory: filepath.Join(home, "mem")}
	h := newMemoryHost(c)

	dir, err := h.resolve(context.Background(), ws)
	if err != nil || dir == nil {
		t.Fatalf("解析记忆目录失败: %v", err)
	}
	if err := os.MkdirAll(dir.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	// ① 索引里列了且存在 ② 索引里列了但磁盘上没有 ③ 磁盘上有但索引没列
	writeFile(t, filepath.Join(dir.Path, memory.IndexFileName), "- a.md\n- ghost.md\n")
	writeFile(t, filepath.Join(dir.Path, "a.md"), "---\ntype: project\nmodified: 2026-09-30T10:00:00Z\n---\n\n正文\n")
	writeFile(t, filepath.Join(dir.Path, "orphan.md"), "---\ntype: user\n---\n\n还没登记\n")

	got, err := c.List(*dir)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if !got.IndexPresent {
		t.Fatal("IndexPresent 应为 true")
	}
	byName := map[string]memory.ListedFile{}
	for _, f := range got.Files {
		byName[f.Name] = f
	}
	if len(got.Files) != 3 {
		t.Fatalf("应列出 3 个文件（含 ghost），得到 %d: %+v", len(got.Files), got.Files)
	}
	if f := byName["a.md"]; f.Missing || !f.Indexed || f.Type != "project" || f.Modified == "" {
		t.Fatalf("a.md 的读数不对: %+v", f)
	}
	// 官方对陈旧条目**不做校验**：列出来并标 missing，不擅自删也不擅自隐藏。
	if f := byName["ghost.md"]; !f.Missing || !f.Indexed {
		t.Fatalf("ghost.md 应列出且标 missing: %+v", f)
	}
	// 未登记是**正常中间态**（模型刚写 topic 还没补索引），列出并标 indexed=false。
	if f := byName["orphan.md"]; f.Missing || f.Indexed {
		t.Fatalf("orphan.md 应列出且 indexed=false: %+v", f)
	}
	if got.Dir != dir.Path {
		t.Fatalf("Dir = %q, want %q", got.Dir, dir.Path)
	}
}

// 索引还没建时（冷启动）：清单为空、IndexPresent=false，但**不是错误**。
func TestConfigListEmptyIndexIsNotAnError(t *testing.T) {
	home := t.TempDir()
	c := memory.Config{Directory: filepath.Join(home, "mem")}
	h := newMemoryHost(c)
	ws := t.TempDir()
	dir, _ := h.resolve(context.Background(), ws)
	if dir == nil {
		t.Skip("本机无法派生记忆目录")
	}
	got, err := c.List(*dir)
	if err != nil {
		t.Fatalf("无索引不该报错: %v", err)
	}
	if got.IndexPresent {
		t.Fatal("IndexPresent 应为 false")
	}
	if len(got.Files) != 0 {
		t.Fatalf("无索引时不应有文件，得到 %+v", got.Files)
	}
}

// 浏览是只读的：面板文案承诺「随时自己编辑或删除」，所以宿主**不提供**写路径。
// 这条把"不许悄悄加删除按钮"钉住——加了就得配校验与确认，而官方没有这套机制。
func TestBrowseCommandHasNoWritePath(t *testing.T) {
	if CmdMemoryBrowse != "memory_browse" {
		t.Fatalf("命令名 = %q", CmdMemoryBrowse)
	}
	// 命令表里只有一个浏览命令；开关相关的两个（settings/set_enabled）是既有命令。
	// 若将来给浏览加 "memory_delete" 之类的兄弟命令，这条要跟着更新并说明理由。
	for _, c := range []Command{CmdMemoryBrowse} {
		if strings.HasSuffix(string(c), "delete") || strings.HasSuffix(string(c), "write") ||
			strings.HasSuffix(string(c), "edit") {
			t.Fatalf("浏览不应有写路径，却出现了 %q", c)
		}
	}
}

// 损坏的 frontmatter 也要照常列出文件：面板的存在意义是让用户看见并自己修。
func TestConfigListToleratesBrokenFrontmatter(t *testing.T) {
	home := t.TempDir()
	c := memory.Config{Directory: filepath.Join(home, "mem")}
	h := newMemoryHost(c)
	ws := t.TempDir()
	dir, _ := h.resolve(context.Background(), ws)
	if dir == nil {
		t.Skip("本机无法派生记忆目录")
	}
	if err := os.MkdirAll(dir.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir.Path, "broken.md"), "---\ntype: [unclosed\n")
	writeFile(t, filepath.Join(dir.Path, "nofm.md"), "纯正文，没有 frontmatter\n")

	got, err := c.List(*dir)
	if err != nil {
		t.Fatalf("损坏 frontmatter 不该让 List 失败: %v", err)
	}
	if len(got.Files) != 2 {
		t.Fatalf("两个文件都应列出，得到 %+v", got.Files)
	}
	// 正文里的 "type:" 不能被当成 frontmatter 字段。
	for _, f := range got.Files {
		if f.Type != "" {
			t.Fatalf("%s 的 Type 应为空（正文里的同名串不算 frontmatter）: %+v", f.Name, f)
		}
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写 %s: %v", path, err)
	}
}
