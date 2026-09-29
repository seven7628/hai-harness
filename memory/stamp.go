package memory

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// `modified` 时间戳（技术方案 §5.3，Step 4）。
//
// 官方（memory#how-it-works）原文：
//
//	"When Claude writes a memory file that begins with YAML frontmatter, Claude Code
//	 records the write time in a modified frontmatter field as an ISO 8601 timestamp.
//	 The timestamp shows how current the fact is, both to you and to Claude when it
//	 reads the memory back. Any file that has frontmatter gets the field the next time
//	 Claude writes it, including files created on earlier versions;
//	 Claude Code never adds frontmatter to a file that has none."
//
// 三条逐字遵守（尤其第 ②③ 条，它们是"不得自行发明"的地方）：
//
//	① 只对**已有 frontmatter** 的文件补/更新 modified；
//	② 从不给无 frontmatter 的文件添加 frontmatter（哪怕文件是空的）；
//	③ 时间戳由系统写入，模型不参与——正文一个字节都不动。
//
// **不做的**（官方明确没有）：过期、淘汰、自动清理、回滚、访问时间统计、
// 读时打时间戳。官方 memory#storage-location："MEMORY.md and topic files stay until
// you or Claude edits or deletes them"。因此本文件不会因为某条记忆太久没碰而删它。
const (
	// ModifiedField frontmatter 字段名（官方拼写）。
	ModifiedField = "modified"
	// FMOpen 是 YAML frontmatter 的开定界符。结束定界符同样是 `---`（或 `...`），
	// 见 isFMDelimiter。
	FMOpen = "---"
)

// ErrStamped 标识"补 modified 这件事失败"（本包不吞错，见 StampModified 的说明）。
//
// 错误用 %w 包装路径与底层原因，调用点用 errors.Is 判别、不解析错误字符串。
var ErrStamped = errors.New("memory: 补 modified 失败")

// Stamped 是 stampWith 的三态结果。取值语义是"这次写入做了什么"。
type Stamped int

const (
	// StampedSkipped 未改动原字节（不适用 / 开关关 / 无 frontmatter / 索引文件）。
	StampedSkipped Stamped = iota
	// StampedUnchanged 内容里的 modified 本来就是这一时刻，无需写盘。
	StampedUnchanged
	// StampedWritten 已按新时间戳重写文件。
	StampedWritten
)

// WriteStamped 是一次写入的完整结果。Stamped 为 StampedSkipped 时 err 必为 nil。
type WriteStamped struct {
	Stamped Stamped
	Err     error
}

// 记忆目录在**工作区之外**（~/.go-code/memory/），模型对它只有标准
// read_file / write_file / edit_file（官方没有记忆专用工具，§1）。本文件不做任何
// 工具注册、也不写正文——WriteStamped 不接受模型给的内容，它只重写已落盘文件的
// frontmatter 时间戳。

const (
	// ModeMemoryFile 记忆文件权限（0600：auto memory 是机器本地的，§3.4）。
	ModeMemoryFile os.FileMode = 0o600
	// ModeMemoryDir 记忆目录权限（0700）。本包不建目录（官方没有"启动即建目录"），
	// 导出它是为了让接线处建目录时不必自己猜一个权限位。
	ModeMemoryDir os.FileMode = 0o700
)

// stampWith 在 **atomicWrite 的锁内**被调用，不自己加锁。
func stampWith(c Config, d Dir, path string, raw []byte, now time.Time) WriteStamped {
	if !c.enabled() {
		// 关闭开关时**不写盘**（config.go 的开关语义：不读也不写）。
		return WriteStamped{Stamped: StampedSkipped}
	}
	if !d.IsTopic(path) {
		// 非记忆条目：非 .md、索引 MEMORY.md、目录之外的路径。官方只对 memory file 补。
		return WriteStamped{Stamped: StampedSkipped}
	}
	if !hasFrontmatter(raw) {
		// 官方第 ② 条：Claude Code never adds frontmatter to a file that has none.
		return WriteStamped{Stamped: StampedSkipped}
	}
	stamp := now.Format(time.RFC3339)
	out, changed, err := setFrontmatterField(raw, ModifiedField, stamp)
	if err != nil {
		return WriteStamped{Stamped: StampedSkipped, Err: fmt.Errorf("%w：%q：%v", ErrStamped, path, err)}
	}
	if !changed {
		// 同一时刻的重复写入（同一秒内两次工具调用）不产生额外写盘。
		return WriteStamped{Stamped: StampedUnchanged}
	}
	// 写入**不得失败**（§4.3）：正文已经由标准 write_file 落盘了，这里只是补时间戳。
	// 失败一律上报而不是吞掉——吞掉会留下"看起来成功但时间戳不更新"的文件，
	// 那是《技术方案》§6 列出的"自创且无效"那一类问题的温和版本。
	if err := c.atomicWrite(path, out); err != nil {
		return WriteStamped{Stamped: StampedSkipped, Err: fmt.Errorf("%w：%q：%v", ErrStamped, path, err)}
	}
	return WriteStamped{Stamped: StampedWritten}
}

// hasFrontmatter 判定内容是否以 YAML frontmatter 开头**且闭合**。
//
// 判定口径：首个非空行是 `---`，且后面还有一个 `---`（或 `...`）作为结束线。
// 要求闭合是因为"只有开线没有闭线"的文件**没有** frontmatter——官方说的
// "a memory file that begins with YAML frontmatter" 指的是合法的 frontmatter 区块。
// 对这种文件补 `modified` 只能把垃圾加到垃圾上。
//
// 容许 BOM 与前导空行是因为用户手工编辑的记忆文件常常从空行开始；判定放宽不会给
// "无 frontmatter 的文件"凭空加 frontmatter（那种文件首行不会恰好是 `---`），
// 因此不违反官方第 ② 条。
func hasFrontmatter(raw []byte) bool {
	_, _, ok := findFrontmatter(raw)
	return ok
}

// findFrontmatter 定位 frontmatter 的开/闭定界行，判定与编辑**共用**这一份实现。
//
// 为什么要共用（2026-09-29 修复）：此前 hasFrontmatter 容忍前导空行与 BOM，而
// setFrontmatterField 只看 lines[0]。于是同一个文件在���一个函数里"有 frontmatter"、
// 在另一个函数里"没有"—— 结果是**静默不补戳且不报错**（宿主只看 StampModified 的
// Err，看到 nil 就当成功）。用户手工编辑的记忆文件常常从空行开头，这条路径命中率
// 不低。一个函数判定为 true 的输入，另一个必须能处理。
func findFrontmatter(raw []byte) (open, close int, ok bool) {
	s, _ := splitBOM(raw)
	lines := strings.Split(s, "\n")
	if len(lines) == 0 {
		return 0, 0, false
	}
	i := 0
	for ; i < len(lines); i++ {
		t := strings.TrimSpace(strings.TrimRight(lines[i], " \t\r"))
		if t == "" {
			continue
		}
		if !isFMDelimiter(t) || t != FMOpen {
			return 0, 0, false
		}
		break
	}
	if i >= len(lines) {
		return 0, 0, false
	}
	for j := i + 1; j < len(lines); j++ {
		if isFMDelimiter(lines[j]) {
			return i, j, true
		}
	}
	return 0, 0, false // 有开无闭：不是合法 frontmatter（官方第 ② 条当作没有）
}

// setFrontmatterField 写入或更新 frontmatter 里的一个标量字段。
//
// 关键约束：**不破坏任何其它 frontmatter 字段**（官方 topic 文件有 `type`，用户在
// 编辑器里还会加自己的字段；把 frontmatter 重新序列化一遍必然丢注释、丢字段顺序、
// 丢引号风格）。所以这里做的是**行级原地编辑**：只动 `key: value` 那一行。
//
// 字段名的匹配刻意宽松（合法 YAML 标量 key 的等价写法都认）：`key:`、`key :`、
// 带引号的 `"key":` 都算命中。宽松是安全的——命中时替换的就是那条键值；
// 没命中时只在 frontmatter 里追加一行，不会误伤别的字段。
func setFrontmatterField(raw []byte, key, value string) (out []byte, changed bool, err error) {
	s, bom := splitBOM(raw)
	lines := strings.Split(s, "\n")
	openIdx, closeIdx, ok := findFrontmatter(raw)
	if !ok {
		// 防御：调用方已判过 frontmatter 存在，这里不该发生。返回原内容而不是
		// 凭空造一个 frontmatter（官方第 ② 条）。口径与 hasFrontmatter 共用
		// findFrontmatter，所以"判定有"与"能编辑"不会再出现分歧。
		return raw, false, nil
	}
	close := closeIdx
	// 扫描范围必须**跳过开定界行**：它自身缩进为 0，会把最小缩进拖到 0，
	// 于是"整块缩进两格"的 frontmatter 会被写回顶格的 modified（修复前后的行为要一致）。
	indent := frontmatterIndent(lines[openIdx+1 : close])
	newLine := indent + key + ": " + value
	for i := 1; i < close; i++ {
		if fmKey(lines[i]) != key {
			continue
		}
		if strings.TrimRight(lines[i], " \t\r") == strings.TrimRight(newLine, " \t\r") {
			return raw, false, nil
		}
		out = joinLines(lines[:i], []string{newLine}, lines[i+1:])
		return appendBOM(string(out), bom), true, nil
	}
	// 没有该字段：在结束线**之前**追加，保留 frontmatter 内的缩进风格。
	trimmed := strings.TrimRight(newLine, " \t\r")
	out = joinLines(lines[:close], []string{trimmed}, lines[close:])
	return appendBOM(string(out), bom), true, nil
}

// frontmatterIndent 取 frontmatter 内**顶层键**的缩进（0 = 顶格）。
//
// 官方示例是顶格 `type: user`；`modified` 与 `type` 同级。跟随用户已有的写法
// （有人会整体缩进两格），所以不能写死 0。
//
// 为什么取**最小**缩进而不是「最后一条有效记录」的缩进（2026-09-29 修复）：
// 嵌套结构下两者不等价。以下输入曾产出**非法 YAML** ——
//
//	---
//	type: user
//	tags:
//	  - alpha
//	  - beta
//	  modified: 2026-09-29T...   <- 挂进了 tags 的列表里
//	---
//
// 结果是整个 frontmatter 不可解析（`type` 一并丢失），而官方把 `modified` 的用途
// 正是"让模型读回记忆时知道这条有多新"—— 时间戳读不回来，这条记忆的时效信息归零。
// 该损坏**静默**：不报错、不打日志、测试全绿。修复前的测试恰好只覆盖了"整块缩进"
// 这一种能工作的形态，把会坏的形态漏掉了。
//
// 最小缩进即顶层键的缩进：嵌套列表/嵌套对象的子行更深，取 min 必然落在顶层；
// 整块被缩进时所有键同深，min 仍是它们的共同缩进，行为与修复前一致。
func frontmatterIndent(lines []string) string {
	indent := ""
	found := false
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		cur := ln[:len(ln)-len(strings.TrimLeft(ln, " \t"))]
		if !found || len(cur) < len(indent) {
			indent, found = cur, true
		}
	}
	return indent
}

// isFMDelimiter 判定 frontmatter 定界行：`---` 或 `...`（两者都是 YAML 的定界标记）。
func isFMDelimiter(line string) bool {
	t := strings.TrimSpace(strings.TrimRight(line, " \t\r"))
	return t == FMOpen || t == "..."
}

// fmKey 取一行的 YAML 标量键；不是键值行则返回 ""。
func fmKey(line string) string {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") {
		return "" // 空行 / 注释行
	}
	i := strings.IndexByte(t, ':')
	if i <= 0 {
		return ""
	}
	k := strings.TrimSpace(t[:i])
	if k == "" {
		return ""
	}
	if strings.ContainsAny(k, " \t") && !isQuotedKey(k) {
		// 含空格且不是带引号的键 → 更像一整句话而不是键值行（值里的冒号也不会走到这）。
		return ""
	}
	return strings.Trim(k, `"'`)
}

func isQuotedKey(k string) bool {
	return (strings.HasPrefix(k, `"`) && strings.HasSuffix(k, `"`) && len(k) >= 2) ||
		(strings.HasPrefix(k, "'") && strings.HasSuffix(k, "'") && len(k) >= 2)
}

// splitBOM 拆出 UTF-8 BOM，返回无 BOM 的内容与 BOM 本身（写回时原样带上）。
func splitBOM(raw []byte) (s string, bom []byte) {
	if bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) {
		return string(raw[3:]), []byte{0xEF, 0xBB, 0xBF}
	}
	return string(raw), nil
}

// appendBOM 把 BOM 放回内容开头。
func appendBOM(s string, bom []byte) []byte {
	if len(bom) == 0 {
		return []byte(s)
	}
	out := make([]byte, 0, len(bom)+len(s))
	out = append(out, bom...)
	return append(out, s...)
}

// joinLines 拼回多行内容（行数组末项为 "" 即保留结尾换行）。
func joinLines(parts ...[]string) []byte { return []byte(strings.Join(concatLines(parts), "\n")) }

func concatLines(parts [][]string) []string {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]string, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// ——— 对外入口：write_file / edit_file 钩子调用这一个 ———

// StampModified 补 `modified` 时间戳：模型写完一条记忆文件后由宿主调用（§5.3）。
//
// 返回值三态：
//
//	Err == nil  → StampedSkipped / StampedUnchanged / StampedWritten，都是成功
//	Err != nil  → 补写失败，**不吞**：宿主应把它作为工具结果的附加错误回给模型
//
// **宿主必须保证调用顺序：文件内容先由标准 write_file 落盘，再调本函数。**
// 本函数只重写 frontmatter 里的时间戳，不负责写正文。
func (c Config) StampModified(d Dir, path string) WriteStamped {
	c = c.withDefaults()
	if !c.enabled() {
		// 关闭开关：不读也不写（config.go：否则就变成"不读但写入"）。
		return WriteStamped{Stamped: StampedSkipped}
	}
	// IsTopic 已排除索引、非 .md 与目录外的路径，先判可以省掉一次读盘。
	if !d.IsTopic(path) {
		return WriteStamped{Stamped: StampedSkipped}
	}
	raw, err := c.ReadFile(path)
	if err != nil {
		if isNotExist(err) {
			// 文件已被删（或调用顺序反了）：没有文件可打时间戳，不补也不算错误。
			return WriteStamped{Stamped: StampedSkipped}
		}
		return WriteStamped{Err: fmt.Errorf("%w：读 %q：%v", ErrStamped, path, err)}
	}
	rel, err := lockTopic(path)
	if err != nil {
		return WriteStamped{Err: fmt.Errorf("%w：取写锁 %q：%v", ErrStamped, path, err)}
	}
	defer rel()

	// 拿到锁后**重读**：可能是在等锁期间被另一个会话/另一个 worktree 改过了。
	// 在旧内容上打时间戳会把对方的改动整段覆盖掉——丢的是正文，不是时间戳。
	raw, err = c.ReadFile(path)
	if err != nil {
		if isNotExist(err) {
			return WriteStamped{Stamped: StampedSkipped}
		}
		return WriteStamped{Err: fmt.Errorf("%w：读 %q：%v", ErrStamped, path, err)}
	}
	return stampWith(c, d, path, raw, c.Now())
}

// ——— 写盘：临时文件 + 同目录 rename ———

// atomicWrite 原子替换文件内容。
//
// 临时文件必须建在**目标同目录**（不是 /tmp）：POSIX 的 rename 只在同一文件系统内
// 原子，跨文件系统的 rename 会失败。记忆目录在 ~/.go-code 下，同目录建临时文件
// 是唯一能同时拿到"原子"和"不被写一半"的做法。
//
// 保留原权限位而不是无条件 0600：记忆文件可能被用户手工建过、带了别样的权限，
// 无声改掉用户文件的权限位是宿主不该做的事。
func (c Config) atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	mode := ModeMemoryFile
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	} else if !isNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = c.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = c.Remove(name)
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		_ = c.Remove(name)
		return err
	}
	if err := c.Rename(name, path); err != nil {
		_ = c.Remove(name)
		return err
	}
	return nil
}

// ——— 写锁：一个 topic 文件一次重写的串行化 ———

const (
	// lockSuffix 锁侧车文件后缀。放在**被锁文件同目录**（`<topic>.md.lock`）而不是
	// /tmp：跨目录锁对"机器本地记忆"这个场景没有必要，同目录能让锁的生存期与
	// 文件同生共死，也不需要任何全局锁表。
	lockSuffix = ".lock"
	// lockTimeout 取锁等待上限。超时即报错：宁可让这一次工具调用失败并把错误回给
	// 模型，也不能在没有串行化的情况下写（丢字段比写不进去更难被发现）。
	lockTimeout = 2 * time.Second
	// lockPoll 取锁的重试间隔。
	lockPoll = 2 * time.Millisecond
)

// lockTopic 取 topic 文件的写锁，返回释放函数。
//
// 为什么需要：记忆目录是**共享**的——同一仓库的所有 worktree、所有并行会话写的都是
// 同一个 <id>/ 目录（§3.2）。没有串行化时 read-modify-write 会丢字段：两个写入方
// 都读到 `type: user`，各自补一个 `modified`，后写的把先写的整段覆盖。
func lockTopic(path string) (func(), error) {
	f, err := os.OpenFile(path+lockSuffix, os.O_CREATE|os.O_RDWR, ModeMemoryFile)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(lockTimeout)
	for {
		err := flockFile(f, deadline)
		if err == nil {
			return func() {
				_ = unlockFile(f)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, errLockBusy) || time.Now().After(deadline) {
			_ = f.Close()
			return nil, err
		}
		time.Sleep(lockPoll)
	}
}

// unlockFile 释放 flock（非 unix 平台下是空操作，见 lock_other.go 的降级说明）。
func unlockFile(f *os.File) error {
	if !flockSupported {
		return nil
	}
	return syscallFlockUnlock(f)
}

// SetMemoryField 写一个记忆文件的 frontmatter 标量字段（如 `type`），并补 `modified`。
//
// **本轮刻意不接线。** 官方没有"宿主替模型写记忆"这个机制——记忆内容由模型用标准
// write_file 写，宿主只在写后补时间戳（§5.3，官方原文的动词就是 "When Claude writes"）。
// 官方也没规定 `type` 由谁写：模型自己在 frontmatter 里写即可。
//
// 之所以提供它而不是等到需要时再写：并发的 read-modify-write 逻辑已经因为
// StampModified 的锁而存在，复制一份到接线时必然会出现两份各自漂移的实现。
// 它的测试（TestSetMemoryField）就是"锁的语义本身"的测试。
//
// 调用约定与 StampModified 一致：**宿主先让内容落盘，再调本函数**。
func (c Config) SetMemoryField(d Dir, path, key, value string) WriteStamped {
	c = c.withDefaults()
	if !c.enabled() || !d.IsTopic(path) {
		return WriteStamped{Stamped: StampedSkipped}
	}
	raw, err := c.ReadFile(path)
	if err != nil {
		if isNotExist(err) {
			return WriteStamped{Stamped: StampedSkipped}
		}
		return WriteStamped{Err: fmt.Errorf("%w：读 %q：%v", ErrStamped, path, err)}
	}
	rel, err := lockTopic(path)
	if err != nil {
		return WriteStamped{Err: fmt.Errorf("%w：取写锁 %q：%v", ErrStamped, path, err)}
	}
	defer rel()
	raw, err = c.ReadFile(path) // 拿锁后重读，避免覆盖等锁期间的改动
	if err != nil {
		if isNotExist(err) {
			return WriteStamped{Stamped: StampedSkipped}
		}
		return WriteStamped{Err: fmt.Errorf("%w：读 %q：%v", ErrStamped, path, err)}
	}
	if !hasFrontmatter(raw) {
		// 官方第 ② 条：绝不给无 frontmatter 的文件添加 frontmatter。
		return WriteStamped{Stamped: StampedSkipped}
	}
	stamped, changed, err := setFrontmatterField(raw, key, value)
	if err != nil {
		return WriteStamped{Err: fmt.Errorf("%w：%q：%v", ErrStamped, path, err)}
	}
	out, changed2, err := setFrontmatterField(stamped, ModifiedField, c.Now().Format(time.RFC3339))
	if err != nil {
		return WriteStamped{Err: fmt.Errorf("%w：%q：%v", ErrStamped, path, err)}
	}
	if !changed && !changed2 {
		return WriteStamped{Stamped: StampedUnchanged}
	}
	if err := c.atomicWrite(path, out); err != nil {
		return WriteStamped{Err: fmt.Errorf("%w：%q：%v", ErrStamped, path, err)}
	}
	return WriteStamped{Stamped: StampedWritten}
}
