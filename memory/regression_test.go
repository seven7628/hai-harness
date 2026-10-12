package memory

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 2026-10-09 审查发现并修复的两类硬约束：目录不能逃逸、索引归因要准确。

// TestProjectDirNameCannotEscapeRepos：CLAUDE_CODE_PROJECT_DIR_NAME / Config.ProjectDirName
// 不能把记忆目录指到 repos/ 之外。
//
// 背景：sanitizeSegment 保留 '.' 且只 trim 两端的 '-'，于是 "." / ".." 原样通过。
// filepath.Join(root, reposDirName, "..") 会把 .. 吃掉 → 落到 <root>（整个记忆根）。
// 后果是 IsInMemoryDir 对 <root> 下任意文件返回 true，List 与保留期清理把整个记忆根
// 当成一个仓库的记忆目录遍历。subagent.go 的 SubagentDir 早已拒过同形态的输入，
// 现在两边共用 safeIDSegment。
func TestProjectDirNameCannotEscapeRepos(t *testing.T) {
	root := t.TempDir()
	// 只有 "." / ".." 这类**能让 Join 退层**的输入才必须拒；
	// 纯空白与 "///" 被 sanitizeSegment 压成空串，走 ErrNoIdentity。
	for _, bad := range []string{".", ".."} {
		if _, err := resolveDir(context.Background(), "/whatever",
			Config{Directory: root, ProjectDirName: bad}); !errors.Is(err, ErrNoIdentity) {
			t.Errorf("ProjectDirName %q 应被拒（ErrNoIdentity），实际放行", bad)
		}
	}
	// 纯空白与 "///" 化简后为空 —— 语义是"用户没设过"，于是走常规 git/路径派生
	//（projectDirName() 先 TrimSpace，空串 → 环境变量 → 空）。这不是逃逸：
	// 它们仍落在 <root>/repos/ 下，只是 ID 由工作目录派生。
	// 注意 "///" 不在此列：sanitizeSegment 把它压成字面 "-"，而 projectDirName()
	// 只 TrimSpace，于是 "-" 会走 safeIDSegment 并被拒（化简后为空）——那也正确。
	for _, blank := range []string{"   ", "\t\n "} {
		d, err := resolveDir(context.Background(), "/whatever", Config{Directory: root, ProjectDirName: blank})
		if err != nil {
			t.Fatalf("ProjectDirName %q 应解析成功（空 = 未设置）: %v", blank, err)
		}
		if want := filepath.Join(root, reposDirName); !strings.HasPrefix(d.Path, want+string(filepath.Separator)) {
			t.Errorf("ProjectDirName %q → %q 不在 %s/ 下", blank, d.Path, want)
		}
	}
	// 正向仍必须可用
	d, err := resolveDir(context.Background(), "/whatever",
		Config{Directory: root, ProjectDirName: "shared-team"})
	if err != nil {
		t.Fatalf("正常名应成功: %v", err)
	}
	if want := filepath.Join(root, reposDirName, "shared-team"); d.Path != want {
		t.Fatalf("Path = %q, want %q", d.Path, want)
	}
}

// TestSubagentDirRejectsDotSegments：与上一条同源，钉住子 agent 侧的同一判据。
// "<root>/agents/<repo-id>/.." = "<root>/agents"，即子 agent 的记忆落到整个
// agents 命名空间上——postWrite 会把那个目录当成某一个子 agent 的记忆目录补戳。
func TestSubagentDirRejectsDotSegments(t *testing.T) {
	c := Config{Directory: t.TempDir()}
	repo := Dir{ID: "repo-abc", Path: filepath.Join(c.root2(t), "repos", "repo-abc")}
	for _, bad := range []string{".", "..", "", "   ", "///"} {
		if d, ok := c.SubagentDir(repo, bad); ok {
			t.Errorf("SubagentDir(%q) 应被拒，得到 %+v", bad, d)
		}
	}
	// "..." 不是 POSIX 特殊分量（Join 不会化简它），落点仍在 <root>/agents/<id>/ 下：
	// 这是可接受的字面目录名，不是逃逸。记录下来以免后来者误判成漏洞。
	if d, ok := c.SubagentDir(repo, "..."); !ok || !strings.HasPrefix(d.Path, repo.Path[:0]+filepath.Dir(repo.Path)+string(filepath.Separator)+"agents") {
		t.Logf("「...」落点（字面目录名，非逃逸）：ok=%v path=%q", ok, d.Path)
	}
}

func (c Config) root2(t *testing.T) string { //nolint:revive // 测试助手
	t.Helper()
	r, err := c.root()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestBoundIndexBlamesTheEarlierLimit：两个限额都超时，TruncatedBy 必须是**先到**的那个。
//
// 背景：实现是"先按字节切、再按行切"，而行限额此前后被无条件赋值 TruncatedBy = "line"，
// 把先到的事实覆盖掉。实测 500 行 × ~107 字节的索引被报成 "line"，而 25KB 才是先到
// 的那一个。归因错会把模型引向无效动作：该缩短行的时候被告知去合并条目
// （truncationNotice 用这个维度名给指引）。
func TestBoundIndexBlamesTheEarlierLimit(t *testing.T) {
	// 300 行 × 95 字节 = 28500 字节 > 25000（字节先到）；
	// 按字节切完还剩 250 行 > 200（行限额随后也到）。
	var b strings.Builder
	for i := 0; i < 300; i++ {
		b.WriteString(strings.Repeat("z", 94))
		b.WriteString("\n")
	}
	raw := []byte(b.String())
	if len(raw) <= MaxIndexBytes {
		t.Fatalf("前提失败：字节限额应先到，len = %d", len(raw))
	}

	load, _ := boundIndex(memDir(t), raw)
	if load.TruncatedBy != "byte" {
		t.Fatalf("TruncatedBy = %q, want \"byte\"（25KB 先到，行限额不得覆盖它）", load.TruncatedBy)
	}
	if load.LoadedLines > MaxIndexLines {
		t.Fatalf("LoadedLines = %d 超过 %d", load.LoadedLines, MaxIndexLines)
	}
	if load.LoadedBytes > MaxIndexBytes {
		t.Fatalf("LoadedBytes = %d 超过 %d", load.LoadedBytes, MaxIndexBytes)
	}
}

// TestBoundIndexLineOnlyStillSaysLine：只有行限额超时仍报 "line"（反向保护）。
func TestBoundIndexLineOnlyStillSaysLine(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 250; i++ {
		b.WriteString("- " + strings.Repeat("x", 40) + "\n")
	}
	load, _ := boundIndex(memDir(t), []byte(b.String()))
	if load.TotalBytes > MaxIndexBytes {
		t.Fatalf("前提失败：字节限额不应先到，TotalBytes = %d", load.TotalBytes)
	}
	if load.TruncatedBy != "line" {
		t.Fatalf("TruncatedBy = %q, want \"line\"", load.TruncatedBy)
	}
}

// TestIndexLoadTextMatchesReturnedBody：IndexLoad.Text 是注释承诺过的字段
// （"未发生截断时与注入段落去掉标题后的内容逐字节相等"），实现却从未赋值、
// 恒为空串。补上赋值，并用这条断言防再次漂移。
func TestIndexLoadTextMatchesReturnedBody(t *testing.T) {
	d := memDir(t)
	raw := []byte("# Project Memory\n\n- a.md\n- b.md\n")
	load, text := boundIndex(d, raw)
	if load.Text != text {
		t.Fatalf("Load.Text = %q, want %q（与 boundIndex 返回的正文一致）", load.Text, text)
	}
	if load.Text == "" {
		t.Fatal("Load.Text 为空：注释承诺的'截断后索引正文'没有实现")
	}
	if strings.Contains(load.Text, "# Project Memory") {
		t.Fatalf("Load.Text 应已剥掉索引自带标题：%q", load.Text)
	}
}

// TestCheckIndexReadErrorIsNotTooLarge：读盘失败回传**原始**错误而不是 ErrIndexTooLarge。
// postWrite 把 err 原样回给模型，若判成超限，模型会收到"请重写索引"的误导指引，
// 而真实原因是权限/IO。
func TestCheckIndexReadErrorIsNotIndexTooLarge(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "- a.md\n")
	boom := errors.New("模拟权限拒绝")
	c := Config{ReadFile: func(string) ([]byte, error) { return nil, boom }}
	err := c.CheckIndex(d)
	if err == nil {
		t.Fatal("读盘失败必须回传错误")
	}
	if errors.Is(err, ErrIndexTooLarge) {
		t.Fatalf("读盘失败不得报成索引超限（会误导模型去重写索引）：%v", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want 包装 %v", err, boom)
	}
	// 索引不存在仍为 nil（官方 "If a file doesn't exist, no error"）
	if err := (Config{}).CheckIndex(memDir(t)); err != nil {
		t.Fatalf("索引不存在应为 nil：%v", err)
	}
}

// TestListSkipsDirsIndexAndNonMD：/memory 浏览面的过滤规则。
// list.go 此前 0% 覆盖（唯一的三处调用在 desktop/bridge，与包一起编译失败 = 零保护）。
func TestListSkipsDirsIndexAndNonMD(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "- a.md\n")
	writeTopic(t, d, "a.md", "---\ntype: user\n---\n\nx\n")
	writeTopic(t, d, "notes.txt", "---\ntype: user\n---\n")
	writeTopic(t, d, "READ.md.txt", "---\ntype: user\n---\n")
	if err := os.MkdirAll(filepath.Join(d.Path, "subdir.md"), 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := (Config{}).List(d)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got.Files) != 1 || got.Files[0].Name != "a.md" {
		t.Fatalf("清单应只含 a.md，得到 %+v", got.Files)
	}
	if !got.IndexPresent {
		t.Fatal("IndexPresent 应为 true")
	}
}

// TestListIsSortedByName：sort.Slice 是唯一的顺序保证。
// List 的两个来源（索引 map 与 ReadDir）都 append 进同一 slice，而 map 遍历序随机；
// 删掉排序会让 /memory 面板的文件顺序变成随机序——UI 直接渲染它。
func TestListIsSortedByName(t *testing.T) {
	d := memDir(t)
	for _, n := range []string{"zz", "mm", "aa", "q7", "b2", "c3"} {
		writeTopic(t, d, n+".md", "---\ntype: user\n---\n\nx\n")
	}
	// 索引里再放三个（含一个磁盘上没有的），让两个来源都参与排序
	writeIndex(t, d, "- n1.md\n- m2.md\n- ghost.md\n- zz.md\n")

	got, err := (Config{}).List(d)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	names := make([]string, len(got.Files))
	for i, f := range got.Files {
		names[i] = f.Name
	}
	if !sort_StringsAreSorted(names) {
		t.Fatalf("清单必须按名字排序（map 遍历序随机，靠 sort.Slice 兜）：%v", names)
	}
}

func sort_StringsAreSorted(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] > s[i] {
			return false
		}
	}
	return true
}

// TestListUsesInjectedReadDir：列举必须走 Config.ReadDir 出口。
// 此前 list.go 直接 os.ReadDir(d.Path)，于是 Config{Directory: <tmp>} 的测试里
// ReadFile 指向临时目录、目录列举却列用户真实的 ~/.go-code/memory（方向相反的两个根）。
func TestListUsesInjectedReadDir(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "- a.md\n")

	var listed string
	c := Config{ReadDir: func(p string) ([]fs.DirEntry, error) {
		listed = p
		// 故意只返回一个条目：若 List 仍走 os.ReadDir，清单会多出真实目录里的东西
		return nil, fs.ErrNotExist
	}}
	got, err := c.List(d)
	if err != nil {
		t.Fatalf("目录不存在应按'没有记忆'处理，不得报错: %v", err)
	}
	if listed != d.Path {
		t.Fatalf("ReadDir 收到 %q, want %q", listed, d.Path)
	}
	// 只有索引里那两个（a.md + READ 时不存在的那条）
	if len(got.Files) == 0 {
		t.Fatal("索引条目应仍在清单里")
	}

	// 非 not-exist 的列举错误必须回传（此前被 err == nil 静默吞掉）
	c2 := Config{ReadDir: func(string) ([]fs.DirEntry, error) { return nil, errors.New("模拟 EACCES") }}
	if _, err := c2.List(d); err == nil {
		t.Fatal("列举失败必须回传错误（调用点要能区分'没有记忆'与'读不了目录')")
	}
}

// TestParseIndexEntries 宽松取条目的规则（模型可能写散文前言、分组小标题、非 .md 行）。
// 宁可多列也不漏列——漏列会让用户以为记忆丢了。
func TestParseIndexEntries(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"带 - 前缀", "- a.md\n- b.md\n", "a.md,b.md"},
		{"无 - 前缀也算", "a.md\nb.md\n", "a.md,b.md"},
		{"标题与空行跳过", "# Project Memory\n\n- a.md\n", "a.md"},
		{"含空格的散文行跳过", "Notes live in topic files.\n- a.md\n", "a.md"},
		{"MEMORY.md 自身排除", "- MEMORY.md\n- a.md\n", "a.md"},
		{"大小写不敏感地排除索引", "- memory.md\n- a.md\n", "a.md"},
		{"子目录路径保留", "sub/nested.md\n", "sub/nested.md"},
		{"空正文", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := strings.Join(parseIndexEntries(c.body), ","); got != c.want {
				t.Fatalf("parseIndexEntries(%q) = %q, want %q", c.body, got, c.want)
			}
		})
	}
}

// TestListToleratesBrokenFrontmatter：list.go 刻意不做 YAML 解析，就是为了
// "frontmatter 坏了也照常列出文件"（用户自己手改过是常态）。
func TestListToleratesBrokenFrontmatter(t *testing.T) {
	d := memDir(t)
	writeTopic(t, d, "broken.md", "---\ntype: [unclosed\n")
	writeTopic(t, d, "nofm.md", "纯正文\n")
	got, err := (Config{}).List(d)
	if err != nil {
		t.Fatalf("损坏 frontmatter 不该让 List 失败: %v", err)
	}
	if len(got.Files) != 2 {
		t.Fatalf("两个文件都应列出，得到 %+v", got.Files)
	}
	for _, f := range got.Files {
		if f.Type != "" || f.Modified != "" {
			t.Fatalf("%s 不应解析出字段：%+v", f.Name, f)
		}
	}
}
