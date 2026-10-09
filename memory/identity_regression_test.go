package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ID 派生的 rune 安全性与形状（2026-10-09 审查发现并修复）。
//
// 背景：三处截断此前都是**字节**切片（id[:maxIDLen] 一类）。非 ASCII 仓库目录名
// （中文/日文路径在 macOS/Linux 上常见）被切在多字节 rune 中间，产出含半个 rune 的
// 目录名，os.MkdirAll 直接报 "illegal byte sequence"——该仓库的记忆整体不可用，
// 且故障是沉默的（模型写文件失败，Step 3/4 全链路无声失效）。
//
// 现有测试全部用 ASCII 名字，覆盖不到这一形态，所以这里专门钉住。

// TestIDTruncationKeepsValidUTF8 三个派生入口都不得产出非法 UTF-8，且不超 maxIDLen。
func TestIDTruncationKeepsValidUTF8(t *testing.T) {
	longCJK := strings.Repeat("仓", 30)
	mixed := "项目-日本語-レポジトリ-" + strings.Repeat("x", 60)

	for _, name := range []string{longCJK, mixed, strings.Repeat("項", 40)} {
		// ① 无 remote：由 git common dir 派生
		id := idFromGitDir("/work", filepath.Join("/work", name, ".git"))
		if strings.ToValidUTF8(id, "?") != id {
			t.Errorf("idFromGitDir(%q) = %q 含非法 UTF-8", name, id)
		}
		if len(id) > maxIDLen {
			t.Errorf("idFromGitDir(%q) = %q 长度 %d 超过 maxIDLen %d", name, id, len(id), maxIDLen)
		}
		// ② 由绝对路径派生
		if id := idFromPath("/Users/me/" + name); strings.ToValidUTF8(id, "?") != id {
			t.Errorf("idFromPath(%q) 含非法 UTF-8", name)
		}
	}

	for _, u := range []string{
		"https://gitlab.com/" + strings.Repeat("项", 30) + "/repo.git",
		"git@github.com:" + strings.Repeat("日本", 15) + "/repo.git",
		"ssh://git@example.com:2222/" + strings.Repeat("倉", 20) + "/" + strings.Repeat("庫", 20) + ".git",
	} {
		id := idFromRemote(u)
		if id == "" {
			continue // host/path 无法规范化时回落，不是本测试关注点
		}
		if strings.ToValidUTF8(id, "?") != id {
			t.Errorf("idFromRemote(%q) = %q 含非法 UTF-8", u, id)
		}
		if len(id) > maxIDLen {
			t.Errorf("idFromRemote(%q) = %q 长度 %d 超过 maxIDLen %d", u, id, len(id), maxIDLen)
		}
	}
}

// TestNonASCIIIDCanCreateDir 非法 UTF-8 的真实后果是"目录建不出来"，
// 所以断言 MkdirAll 成功，而不只是断言字符串合法。
func TestNonASCIIIDCanCreateDir(t *testing.T) {
	dir := t.TempDir()
	id := idFromGitDir("/work", filepath.Join("/work", strings.Repeat("仓", 30), ".git"))
	// 完整形态 <root>/repos/<id>：MkdirAll 会一路建齐，非法 UTF-8 会在这一步炸
	// "illegal byte sequence"，而那时模型其实是看不到任何错误的（写入失败发生在
	// write_file 内部，而钩子与注入路径都是静默的）。
	memDir := filepath.Join(dir, reposDirName, id)
	if err := os.MkdirAll(memDir, ModeMemoryDir); err != nil {
		t.Fatalf("记忆目录建不出来（%q）：%v", id, err)
	}
	// 文件也要能建：写不进文件同样等于记忆不可用。
	if err := os.WriteFile(filepath.Join(memDir, IndexFileName), []byte("- a.md\n"), ModeMemoryFile); err != nil {
		t.Fatalf("索引写不进去（%q）：%v", id, err)
	}
}

// TestTruncateToIsRuneSafe 直接钉住助手本身：截断点落在多字节 rune 中间时必须回退。
func TestTruncateToIsRuneSafe(t *testing.T) {
	for _, s := range []string{"中文abc", "abc中文", strings.Repeat("中", 20), "abcdefghij"} {
		for n := 0; n <= len(s)+2; n++ {
			got := truncateTo(s, n)
			if strings.ToValidUTF8(got, "?") != got {
				t.Errorf("truncateTo(%q, %d) = %q 含非法 UTF-8", s, n, got)
			}
			if len(got) > n {
				t.Errorf("truncateTo(%q, %d) = %q 长度 %d 超过 %d", s, n, got, len(got), n)
			}
			// ASCII 长串必须完整保留到限额（截断不是"整个丢掉"）
			if s == "abcdefghij" && n >= 10 && got != s {
				t.Errorf("truncateTo(%q, %d) = %q, want 原串", s, n, got)
			}
		}
	}
	if got := truncateTo("abc", 0); got != "" {
		t.Errorf("truncateTo(_, 0) = %q, want 空串", got)
	}
	if got := truncateTo("abc", -1); got != "" {
		t.Errorf("truncateTo(_, -1) = %q, want 空串", got)
	}
}
