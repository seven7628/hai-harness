package memory

// `/memory` 浏览面（官方 §2.7，2026-09-30 补）。
//
// 官方原文：
//
//	"Auto memory files are **plain markdown you can edit or delete at any time**.
//	 Run **`/memory`** to browse and open memory files from within a session."
//
// 形态对应：官方是会话内斜杠命令，本产品对应设置面板里的浏览区（与 memory_settings
// 同一通路）。两件事必须对齐：
//
//  1. **只读快照**，不做 watcher。记忆是模型按需写的低频内容，轮询换来的"实时性"
//     不值那个复杂度；用户重开面板就是最新的。
//  2. **不做写保护、不做准入校验。** 官方承诺"随时可编辑或删除"——文件就是普通
//     markdown，宿主只负责**列出来**。这一条同时否掉了"面板提供删除按钮时要不要
//     过一遍校验"这类设计：官方没有校验，我们也不发明。

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Listing 浏览面板的只读快照。
type Listing struct {
	// Dir 记忆目录。面板要显示"存在哪里"，用户才能自己找到、备份、删除。
	Dir string `json:"dir"`
	// IndexPresent MEMORY.md 是否存在。为 false 时 Files 仍可能有内容——
	// 模型先写 topic 后补索引是正常中间态（那正是 "Saved N memories" 之后的样子）。
	IndexPresent bool `json:"index_present"`
	// Files 记忆文件清单（不含 MEMORY.md 本身）。
	Files []ListedFile `json:"files"`
}

// ListedFile 一个记忆文件的展示信息。
type ListedFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// Modified frontmatter 的 modified 时间戳（官方 §2.6：宿主在写入后补）。
	// 空 = 该文件没有这个字段。
	Modified string `json:"modified,omitempty"`
	// Type frontmatter 的 type（官方四种之一）。空 = 没有该字段。
	Type string `json:"type,omitempty"`
	// Indexed 该文件是否登记在 MEMORY.md 里。为 false 是**正常中间态**，
	// 不是错误：模型刚写完 topic 还没补索引。
	Indexed bool `json:"indexed"`
	// Missing 索引里列了但磁盘上没有。官方对陈旧条目**不做存在性校验**
	// （见 memory/index.go 关于"不逐条校验索引里的文件是否存在"的说明），
	// 这里如实标出给用户看，不擅自隐藏也不擅自删。
	Missing bool `json:"missing,omitempty"`
}

// List 读出目录下的记忆文件清单。
//
// 读盘全走 Config 的 I/O 出口（withDefaults），与其他函数同一口径。
func (c Config) List(d Dir) (Listing, error) {
	c = c.withDefaults()
	out := Listing{Dir: d.Path, Files: []ListedFile{}}

	indexed := map[string]bool{}
	if raw, err := c.ReadFile(joinIndex(d)); err == nil {
		out.IndexPresent = true
		for _, name := range parseIndexEntries(string(raw)) {
			indexed[name] = true
		}
	}

	for name := range indexed {
		f := c.listedFile(d, name)
		f.Indexed = true // 来自索引，即便文件已不在磁盘上（Missing 与 Indexed 可同时为真）
		out.Files = append(out.Files, f)
	}
	if entries, err := os.ReadDir(d.Path); err == nil {
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || indexed[name] ||
				!strings.EqualFold(filepath.Ext(name), ".md") ||
				strings.EqualFold(name, IndexFileName) {
				continue
			}
			out.Files = append(out.Files, c.listedFile(d, name))
		}
	}

	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Name < out.Files[j].Name })
	return out, nil
}

// listedFile 读一个记忆文件的展示信息。文件不存在时只填 Name + Missing（不报错）：
// 面板要显示的就是"索引里有但磁盘上没有"这个事实本身。
func (c Config) listedFile(d Dir, name string) ListedFile {
	f := ListedFile{Name: name}
	raw, err := c.ReadFile(filepath.Join(d.Path, name))
	if err != nil {
		f.Missing = true
		return f
	}
	f.Size = int64(len(raw))
	// 只取 frontmatter 段内的字段：正文里出现 "type:" 不算。
	for _, kv := range frontmatterFields(string(raw)) {
		switch kv[0] {
		case "type":
			if f.Type == "" {
				f.Type = kv[1]
			}
		case "modified":
			if f.Modified == "" {
				f.Modified = kv[1]
			}
		}
	}
	return f
}

// parseIndexEntries 从索引正文里取出条目名。
//
// 官方索引是"一行一条记忆"，但**官方不校验行的形态**，模型可能写出散文前言、
// 分组小标题、非 .md 的行。所以这里宽松取：去掉行首 "- " 前缀与空白，跳过标题行、
// 空行与含空格的散文行。宁可多列也不漏列——漏列会让用户以为记忆丢了。
func parseIndexEntries(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		t = strings.TrimSpace(strings.TrimPrefix(t, "- "))
		if t == "" || strings.ContainsAny(t, " \t") {
			continue
		}
		if strings.EqualFold(filepath.Base(t), IndexFileName) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// frontmatterFields 取出 YAML frontmatter 段里的 `key: value`（逐行、不做 YAML 解析）。
//
// 不引入 YAML 解析是刻意的：官方 frontmatter 只有 type 与 modified 两个字段，
// 逐行取足够；而完整 YAML 解析会让一个损坏的 frontmatter 直接失败——而
// 「frontmatter 坏了也要照常列出文件」正是面板该有的行为（用户自己手改过）。
func frontmatterFields(body string) [][2]string {
	s := strings.TrimLeft(body, "\uFEFF \t\r\n")
	if !strings.HasPrefix(s, "---") {
		return nil
	}
	rest := s[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil
	}
	var out [][2]string
	for _, line := range strings.Split(rest[:end], "\n") {
		line = strings.TrimRight(line, " \t\r")
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") ||
			strings.HasPrefix(line, "#") {
			continue // 嵌套项与注释：官方只有两个平铺字段，嵌套的（如 description）不取
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		if k == "" {
			continue
		}
		out = append(out, [2]string{k, v})
	}
	return out
}
