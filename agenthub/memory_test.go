package agenthub

// P1-05：记忆目录与路径规则（每智能体隔离 + 面板路径不得逃逸）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryDirsPerAgent(t *testing.T) {
	root := t.TempDir()
	a, err := MemoryAgentDir(root, "ag_aaaaaaa1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := MemoryAgentDir(root, "ag_bbbbbbb2")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two agents share memory dir: %s", a)
	}
	if !strings.HasSuffix(a, filepath.Join("agents", "ag_aaaaaaa1", "memory", "repos", "ag_aaaaaaa1")) {
		t.Fatalf("unexpected memory dir layout: %s", a)
	}
	if _, err := MemoryAgentDir(root, "../etc"); err == nil {
		t.Fatal("invalid id must be rejected")
	}
	if _, err := MemoryAgentDir("", "ag_aaaaaaa1"); err == nil {
		t.Fatal("empty hub root must be rejected")
	}
}

// 面板路径：只允许记忆目录内的 .md；绝对路径/.. /反斜杠/非 md 一律拒绝。
func TestResolveMemoryPathRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	dir, err := MemoryAgentDir(root, "ag_aaaaaaa1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// ResolveMemoryPath 会解析符号链接（/tmp → /private/tmp 在 macOS 上就是链接）。
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := ResolveMemoryPath(root, "ag_aaaaaaa1", "MEMORY.md")
	if err != nil {
		t.Fatalf("MEMORY.md should be allowed: %v", err)
	}
	if ok != filepath.Join(realDir, "MEMORY.md") {
		t.Fatalf("resolved path = %q, want %q", ok, filepath.Join(realDir, "MEMORY.md"))
	}
	if got, err := ResolveMemoryPath(root, "ag_aaaaaaa1", "topics/pricing.md"); err != nil ||
		got != filepath.Join(realDir, "topics", "pricing.md") {
		t.Fatalf("nested md should be allowed, got %q err=%v", got, err)
	}

	// 每条坏路径都要**按自己的理由**被拒：只要断言「有错」，删掉 `..` 那条检查时
	// evalWithin 会用 (symlink) 文案兜住，测试仍然全绿（评审 B2 的 mutation b）。
	for _, tc := range []struct {
		path string
		want string
		deny string
	}{
		{"", "empty path", ""},
		{"../escape.md", "path escapes memory dir", "(symlink)"},
		{"..", "path escapes memory dir", "(symlink)"},
		{"../../etc/passwd.md", "path escapes memory dir", "(symlink)"},
		{"/etc/passwd.md", "path must be relative", ""},
		{`..\escape.md`, "must not contain backslashes", ""},
		{"notes.txt", "only .md files are allowed", ""},
		{"MEMORY.md.bak", "only .md files are allowed", ""},
		{".backups/x.md", "is reserved for backups", ""},
		{"topics/.backups/x.md", "is reserved for backups", ""},
	} {
		_, err := ResolveMemoryPath(root, "ag_aaaaaaa1", tc.path)
		if err == nil {
			t.Errorf("path %q must be rejected", tc.path)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("path %q: err = %v, want %q", tc.path, err, tc.want)
		}
		if tc.deny != "" && strings.Contains(err.Error(), tc.deny) {
			t.Errorf("path %q rejected by %s instead of its own branch: %v", tc.path, tc.deny, err)
		}
	}
}

// 符号链接逃逸：目录里放一个指向外部的 .md 链接，解出来必须不在记忆目录内 → 拒绝。
func TestResolveMemoryPathRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	dir, err := MemoryAgentDir(root, "ag_aaaaaaa1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "escape.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	if _, err := ResolveMemoryPath(root, "ag_aaaaaaa1", "escape.md"); err == nil {
		t.Fatal("symlink escaping the memory dir must be rejected")
	}
}

// 清单：只列顶层 .md、按名排序、目录不存在返回空表。
func TestListMemory(t *testing.T) {
	root := t.TempDir()
	entries, err := ListMemory(root, "ag_aaaaaaa1")
	if err != nil {
		t.Fatalf("missing dir should be empty list: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %v, want empty", entries)
	}
	dir, err := MemoryAgentDir(root, "ag_aaaaaaa1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"zeta.md": "z", "MEMORY.md": "m", "ignore.txt": "t"}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entries, err = ListMemory(root, "ag_aaaaaaa1")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "MEMORY.md" || entries[1].Name != "zeta.md" {
		t.Fatalf("entries = %+v, want [MEMORY.md zeta.md]", entries)
	}
	if entries[1].Size != 1 || entries[1].Modified.IsZero() || time.Since(entries[1].Modified) > time.Hour {
		t.Fatalf("entry metadata wrong: %+v", entries[1])
	}
}

// A/B 隔离：两个智能体的记忆文件互不可见（同一套 API，不同目录）。
func TestMemoryIsolationBetweenAgents(t *testing.T) {
	root := t.TempDir()
	aDir, _ := MemoryAgentDir(root, "ag_aaaaaaa1")
	bDir, _ := MemoryAgentDir(root, "ag_bbbbbbb2")
	for _, d := range []string{aDir, bDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(aDir, "MEMORY.md"), []byte("A 的记忆"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bDir, "MEMORY.md"), []byte("B 的记忆"), 0o600); err != nil {
		t.Fatal(err)
	}
	aPath, err := ResolveMemoryPath(root, "ag_aaaaaaa1", "MEMORY.md")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(aPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "A 的记忆" {
		t.Fatalf("A read %q", body)
	}
	// A 的路径绝不能解析到 B 的目录。
	if strings.Contains(aPath, "ag_bbbbbbb2") {
		t.Fatalf("A path leaked into B dir: %s", aPath)
	}
}

// 写入：旧内容必须留备份；新文件无备份；内容可读回；备份裁剪到上限。
func TestWriteMemoryFileBacksUp(t *testing.T) {
	root := t.TempDir()
	id := AgentID("ag_aaaaaaa1")
	if _, err := ReadMemoryFile(root, id, "MEMORY.md"); !errorsIs(err, ErrNotFound) {
		t.Fatalf("missing file should be ErrNotFound, got %v", err)
	}
	first, err := WriteMemoryFile(root, id, "MEMORY.md", "v1\n")
	if err != nil {
		t.Fatal(err)
	}
	if first.Backup != "" || first.Bytes != 3 || first.Path != "MEMORY.md" {
		t.Fatalf("first write result = %+v", first)
	}
	second, err := WriteMemoryFile(root, id, "MEMORY.md", "v2\n")
	if err != nil {
		t.Fatal(err)
	}
	if second.Backup == "" {
		t.Fatal("overwrite must create a backup")
	}
	body, err := ReadMemoryFile(root, id, "MEMORY.md")
	if err != nil || body != "v2\n" {
		t.Fatalf("read back = %q, %v", body, err)
	}
	dir, _ := MemoryAgentDir(root, id)
	backup, err := os.ReadFile(filepath.Join(dir, ".backups", second.Backup))
	if err != nil {
		t.Fatalf("backup unreadable: %v", err)
	}
	if string(backup) != "v1\n" {
		t.Fatalf("backup content = %q, want v1", backup)
	}
	// 备份不进清单（只列顶层 .md）。
	entries, err := ListMemory(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "MEMORY.md" {
		t.Fatalf("list = %+v, want only MEMORY.md", entries)
	}
}

func TestWriteMemoryFilePrunesBackups(t *testing.T) {
	root := t.TempDir()
	id := AgentID("ag_aaaaaaa1")
	for i := 0; i < MaxMemoryBackups+5; i++ {
		if _, err := WriteMemoryFile(root, id, "MEMORY.md", strings.Repeat("x", i+1)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	dir, _ := MemoryAgentDir(root, id)
	ents, err := os.ReadDir(filepath.Join(dir, ".backups"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != MaxMemoryBackups {
		t.Fatalf("backups = %d, want %d", len(ents), MaxMemoryBackups)
	}
}

// 写入路径同样受 ResolveMemoryPath 约束（面板不能借写接口逃逸）。
func TestWriteMemoryFileRejectsEscape(t *testing.T) {
	root := t.TempDir()
	if _, err := WriteMemoryFile(root, "ag_aaaaaaa1", "../evil.md", "x"); err == nil ||
		!strings.Contains(err.Error(), "path escapes memory dir") || strings.Contains(err.Error(), "(symlink)") {
		t.Fatalf("escape path must be rejected by the clean-path branch, err = %v", err)
	}
	if _, err := WriteMemoryFile(root, "ag_aaaaaaa1", "MEMORY.txt", "x"); err == nil {
		t.Fatal("non-md must be rejected")
	}
}

func errorsIs(err, target error) bool {
	return err != nil && (err == target || strings.Contains(err.Error(), target.Error()))
}
