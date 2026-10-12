package memory

import (
	"path/filepath"
	"testing"
)

func TestIsInMemoryDir(t *testing.T) {
	root := filepath.Join(string(filepath.Separator)+"home", "u", ".go-code", "memory")
	yes := []string{
		filepath.Join(root, "repos"),
		filepath.Join(root, "repos", "id"),
		filepath.Join(root, "repos", "id", "MEMORY.md"),
		filepath.Join(root, "repos", "id", "sub", "topic.md"),
		// 词法上等价写法也算（写文件工具给的路径未必是 Clean 过的）。
		filepath.Join(root, "repos", "..", "repos", "id", "topic.md"),
	}
	for _, p := range yes {
		if !IsInMemoryDir(root, p) {
			t.Fatalf("IsInMemoryDir(%q) = false, want true", p)
		}
	}
	no := []string{
		"",
		filepath.Join(root, "..", "..", "..", "settings.json"), // 逃出根
		filepath.Join(root, "..", "memory2", "id", "topic.md"), // 逃出根（前缀相同）
		filepath.Join(string(filepath.Separator)+"tmp", "x.md"),
		"relative/path.md", // 非绝对路径
		"",
	}
	for _, p := range no {
		if IsInMemoryDir(root, p) {
			t.Fatalf("IsInMemoryDir(%q) = true, want false", p)
		}
	}
	if IsInMemoryDir("", filepath.Join(root, "x")) {
		t.Fatal("root 为空应返回 false")
	}
	// 根的上一级里只有前缀相同的兄弟目录时不算（repos 与 repos2 必须区分开）。
	repos := filepath.Join(root, "repos")
	if IsInMemoryDir(repos, filepath.Join(root, "repos2", "id", "topic.md")) {
		t.Fatal("repos2 不应被判定在 repos 之下")
	}
}

func TestDirOwnsIsIndexIsTopic(t *testing.T) {
	d := Dir{ID: "github.com-a-b", Path: filepath.Join(string(filepath.Separator)+"mem", "repos", "github.com-a-b")}

	if !d.Owns(d.Path) {
		t.Fatal("目录自身应属于记忆目录")
	}
	if d.Owns(filepath.Join(filepath.Dir(d.Path), "other", "x.md")) {
		t.Fatal("兄弟目录不应属于记忆目录")
	}

	index := filepath.Join(d.Path, IndexFileName)
	topic := filepath.Join(d.Path, "user_role.md")
	nested := filepath.Join(d.Path, "sub", "build.md")
	notMD := filepath.Join(d.Path, "notes.txt")

	if !d.IsIndex(index) {
		t.Fatal("MEMORY.md 应识别为索引")
	}
	if d.IsIndex(topic) {
		t.Fatal("topic 不应是索引")
	}
	if d.IsTopic(index) {
		t.Fatal("索引不是记忆条目，不应触发补 modified")
	}
	for _, p := range []string{topic, nested} {
		if !d.IsTopic(p) {
			t.Fatalf("IsTopic(%q) = false, want true", p)
		}
	}
	if d.IsTopic(notMD) {
		t.Fatal("非 .md 不是记忆条目")
	}
	if d.IsTopic(filepath.Join(filepath.Dir(d.Path), "elsewhere", "x.md")) {
		t.Fatal("记忆目录之外的路径不应是 topic")
	}
}

// 索引名比对放宽到大小写不敏感是刻意的：macOS/Windows 的文件系统本身就是大小写不敏感，
// 严格比对会让这些平台上的 MEMORY.md 被当成 topic 而补 `modified`（官方只对 memory file 补）。
func TestIsIndexCaseFolding(t *testing.T) {
	d := Dir{Path: filepath.Join(string(filepath.Separator)+"mem", "r", "id")}
	if !d.IsIndex(filepath.Join(d.Path, "memory.md")) {
		t.Fatal("大小写不同的 memory.md 也应被识别为索引")
	}
	if d.IsTopic(filepath.Join(d.Path, "memory.md")) {
		t.Fatal("索引不应被识别为记忆条目")
	}
}
