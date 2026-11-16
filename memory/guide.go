package memory

import (
	"path/filepath"
	"strings"
)

// 记忆写作指南：渐进披露的"按需"那一半。
//
// 背景（2026-11）：常驻说明段此前把"形态 + type 取值域"全塞进去，约 600 字节，
// 每个会话无条件付。而它真正被需要，只发生在**模型准备写记忆**的那一刻。
// 对照它山之石（Skill 的形态：常驻只有 name + description，正文按需加载），
// 这里把详细部分搬进记忆目录下的一个文件，常驻段只留"是什么 + 去哪读"。
//
// 为什么放在记忆目录而不是嵌在二进制里：模型要用**标准文件工具**去读它，
// 那就必须是一个真实路径；放在 .state/ 下是因为它不是一条记忆（不进索引、
// 不参与热点排序、不会被 Dream 当成待归档条目）。
//
// 内容来源：type 四类的语义照抄官方原文（guidance.go 顶部 §2.2）；
// 「先查重」「引用前核对」不是劝告，而是宿主**确实会**付出的代价——
// 重复条目会稀释启动注入的预算并被 Dream 列成 duplicates，失效锚点会被报出来。

// GuideFileName 按需指南的文件名（位于记忆目录的 .state/ 下）。
const GuideFileName = "GUIDE.md"

// guideDoc 生成指南正文。
//
// 正文用英文：这份文件是**给模型读的写作规范**（与源码注释、产品文案不同），
// 而模型的系统提示与工具描述都是英文语境，混排会让"读一遍就能照做"的那部分变慢。
// 中文只留在源码注释与给人看的界面上。
func guideDoc(dir string) string {
	var b strings.Builder
	b.WriteString("# Memory writing guide\n\n")
	b.WriteString("The host does exactly three things with what you write: it parses index lines, ranks\n")
	b.WriteString("entries into the session-startup injection by read count, and resolves code anchors in\n")
	b.WriteString("the body. What to record and how to phrase it is yours. The four sections below cover\n")
	b.WriteString("the parts the host actually checks or consumes.\n\n")

	b.WriteString("## 1. What a memory is\n\n")
	b.WriteString("- Memory directory: ")
	b.WriteString(dir)
	b.WriteString("\n")
	b.WriteString("- One `.md` file per memory; short kebab-case filename.\n")
	b.WriteString("- One line per memory in `" + IndexFileName + "`: `- [Title](file.md) — one-line hook`\n")
	b.WriteString("- **The filename must appear on that line** (inside `](...)` or backticks). A line\n")
	b.WriteString("  without a filename is not recognised as an entry: it never appears in the startup\n")
	b.WriteString("  injection, and the archive command cannot see it. That is a parser rule, not a\n")
	b.WriteString("  style preference.\n\n")

	b.WriteString("## 2. type (only when the file has frontmatter)\n\n")
	b.WriteString("| value | what belongs there |\n| --- | --- |\n")
	b.WriteString("| `user` | who the user is: role, expertise, working preferences |\n")
	b.WriteString("| `feedback` | corrections the user gave and approaches they confirmed; say why it matters |\n")
	b.WriteString("| `project` | ongoing work, goals or constraints not derivable from code or git history; use absolute dates |\n")
	b.WriteString("| `reference` | pointers to external resources: docs, dashboards, tickets |\n\n")
	b.WriteString("A value outside this table is reported by the post-write audit. Files without\n")
	b.WriteString("frontmatter are not subject to this check.\n\n")

	b.WriteString("## 3. Before you write\n\n")
	b.WriteString("- **Check for an existing entry first.** If one already covers the topic, update it\n")
	b.WriteString("  instead of adding a second one. Duplicates dilute the startup budget (the list is\n")
	b.WriteString("  capped by entry count) and Dream reports them as duplicates.\n")
	b.WriteString("- **A memory is a snapshot of what was true when it was written.** Verify that a file,\n")
	b.WriteString("  function or flag it names still exists before relying on it. Dream reports dead code\n")
	b.WriteString("  anchors, but only after the fact — not at the moment you need them.\n\n")

	b.WriteString("## 4. Limits\n\n")
	b.WriteString("- The startup injection reads the first ")
	b.WriteString(strconvItoa(MaxIndexLines))
	b.WriteString(" lines / ")
	b.WriteString(strconvItoa(MaxIndexBytes))
	b.WriteString(" bytes of `" + IndexFileName + "`, whichever comes first; truncation is stated in\n")
	b.WriteString("  the injection itself.\n")
	b.WriteString("- The limit applies to the index only. Bodies can be long — they are not loaded\n")
	b.WriteString("  automatically.\n")
	return b.String()
}

// strconvItoa 避免为两个数字引入 strconv（guide.go 保持零依赖，便于单测断言）。
func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// guidePath 指南文件路径。
func guidePath(d Dir) string { return filepath.Join(d.Path, stateDirName, GuideFileName) }

// ensureGuide 在指南缺失或内容过期时补写一份。best-effort：失败不影响注入。
//
// 为什么要"过期就重写"而不是"有就不动"：指南内容属于宿主版本，用户可能手改过
// （允许），但那会让下一版说明与旧指南对不上。这里用"内容不一致才重写"折中——
// 手改过的会被覆盖，这是刻意取舍：指南的正确性优先于可定制性，用户想留笔记
// 应该写成一条记忆（那才是可检索、可归档的）。
func (c Config) ensureGuide(d Dir) {
	c = c.withDefaults()
	// 目录不存在时**什么都不建**：Bootstrap 的契约是"不凭空造出记忆目录"
	// （TestBootstrap_NoMemoryYet 钉着），而注入发生在 Bootstrap 之后。
	// 新项目的第一条记忆由模型写出来后，下一轮注入会把指南补上。
	if _, err := c.ReadDir(d.Path); err != nil {
		return
	}
	want := guideDoc(d.Path)
	if raw, err := c.ReadFile(guidePath(d)); err == nil && string(raw) == want {
		return
	}
	if err := c.MkdirAll(filepath.Join(d.Path, stateDirName), 0o755); err != nil {
		return
	}
	_ = c.atomicWrite(guidePath(d), []byte(want))
}
