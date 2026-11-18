package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/seven7628/hai-harness/core"

	"github.com/pmezard/go-difflib/difflib"
)

// maxUnifiedDiffBytes 统一 diff 文本的最大字节数：超限截断（Truncated=true，
// 仅保留统计与头部预览）。宿主 UI 以 Added/Removed 统计为主，unified 全文
// 用于小改动的精确渲染；大 diff（如整文件重写）只渲染行数 + 预览即可。
const maxUnifiedDiffBytes = 8 << 10

// computeFileDiff 计算对文件的一次变更 diff（old → new，工具执行时调用）：
//   - Added/Removed：行级统计（difflib opcodes，r 替换 = 删除+新增）
//   - Unified：标准 unified diff 文本（---/+++/@@ hunks，context=3），有界截断
//
// path 为相对工作区根的路径（写入 FileDiff.Path，宿主展示用）。
// 大 diff 截断不丢统计：Truncated=true 时宿主仍可渲染 +/- 行数。
func computeFileDiff(path, old, new string) *core.FileDiff {
	d := &core.FileDiff{Path: path}
	if old == new {
		return d // 无变更（0/0，无 unified）
	}
	al := splitLines(old)
	bl := splitLines(new)

	// 大文件保护（2026-11-17，第 9 步）：go-difflib 的匹配是 **O(N²) 量级**
	// （实测：1k 行 0.4 ms、5k 行 3.4 ms、20k 行 35 ms、50k 行 309 ms；
	// 而一次编辑的文件可以到 16 MiB ≈ 50 万行 ⇒ 单次调用要跑几十秒）。
	//
	// 判据用「掐掉公共头尾后的中间段」：小改动落在中间 ⇒ 中间很小 ⇒ 照常算；
	// 整文件重写 ⇒ 中间就是全文 ⇒ 直接报粗粒度事实，不跑匹配器。
	prefix := commonPrefixLines(al, bl)
	suffix := commonSuffixLines(al[prefix:], bl[prefix:])
	middleOld := len(al) - prefix - suffix
	middleNew := len(bl) - prefix - suffix
	if middleOld+middleNew > maxDiffMiddleLines {
		d.Added, d.Removed = middleNew, middleOld
		d.Truncated = true
		d.Unified = fmt.Sprintf("[diff too large to compute: %d line(s) replaced]", middleOld)
		return d
	}

	// **只匹配一次**：unified 文本本身包含全部增删行 ⇒ 统计直接从文本数，
	// 不再单独跑一遍 GetGroupedOpCodes（改造前是两次 O(N²) 匹配 = 纯浪费 2×）。
	unified, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        al,
		B:        bl,
		FromFile: path,
		ToFile:   path,
		Context:  3,
	})
	if err != nil {
		d.Truncated = true // 算法失败：无统计、无 unified
		return d
	}
	d.Added, d.Removed = statsFromUnified(unified)
	if len(unified) > maxUnifiedDiffBytes {
		d.Truncated = true
		d.Unified = truncateUTF8(unified, maxUnifiedDiffBytes) +
			fmt.Sprintf("\n[file diff truncated: +%d -%d lines]\n", d.Added, d.Removed)
		return d
	}
	d.Unified = unified
	return d
}

// maxDiffMiddleLines 参与 diff 匹配的行数上限（掐掉公共头尾后的中间段，两边合计）。
//
// 取值依据：20k 行 ≈ 35 ms 一次匹配（实测）⇒ 上限处的单次调用约 35 ms，
// 对工具调用完全可接受；再大就退化成「整块替换」的粗粒度事实。
const maxDiffMiddleLines = 20000

// commonPrefixLines 两边公共前缀的行数。
func commonPrefixLines(a, b []string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// commonSuffixLines 两边公共后缀的行数（**不与前缀重叠**）。
func commonSuffixLines(a, b []string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[len(a)-1-i] == b[len(b)-1-i] {
		i++
	}
	return i
}

// statsFromUnified 从 unified diff 文本数出新增/删除行数。
//
// 为什么可以：从文本数与从 opcodes 数**等价** —— unified 文本的每个 hunk 都
// 覆盖了全部增删行（context=3 只影响未改动行的上下文，不影响增删行本身）。
// 文件头（"--- path" / "+++ path"）带空格，按「--- 」「+++ 」前缀排除，
// 不会与内容行（"-"+内容，无空格）混淆。
func statsFromUnified(unified string) (added, removed int) {
	for _, line := range strings.Split(unified, "\n") {
		switch {
		case strings.HasPrefix(line, "--- "), strings.HasPrefix(line, "+++ "):
			continue // 文件头
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return added, removed
}

// maxArtifactHashBytes 计算内容指纹的字节上限：超过则不哈希（大文件哈希收益低、
// CPU 成本高；Size 仍由 len() 给出，恒 O(1)）。
const maxArtifactHashBytes = 8 << 20 // 8 MiB

// finalizeDiff 用**内存中的最终内容**补齐产物事实（size / lines / 指纹）。
// 零额外 IO：写的那一刻工具已持有完整新内容（信息源头原则），不读磁盘。
//
// 语义：content 必须是「写入后的完整文件内容」。对既有文件做 append 时
// content 仅为增量——那种场景不要调用本函数（Size/Lines 会失真），
// 见 writeFileTool.Call 的 append 分支。
func finalizeDiff(d *core.FileDiff, content string) *core.FileDiff {
	if d == nil {
		return nil
	}
	d.Size = int64(len(content))
	d.Lines = int64(strings.Count(content, "\n"))
	if len(content) <= maxArtifactHashBytes {
		d.SHA256 = contentFingerprint(content)
	}
	return d
}

// contentFingerprint 内容指纹（前 16 字节 hex）；超上限返回空。
// append 场景单独用（content 为增量，不能走 finalizeDiff）。
//
// 用 sha256.New() + io.WriteString 而不是 sha256.Sum256([]byte(content))：
// 后者会把**整个字符串拷一份**进 []byte（8 MiB 上限 ⇒ 每次写文件多拷 8 MiB）。
func contentFingerprint(content string) string {
	if len(content) > maxArtifactHashBytes {
		return ""
	}
	h := sha256.New()
	_, _ = io.WriteString(h, content)
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16])
}

// diffStats 从 difflib opcodes 统计新增/删除行数。
// go-difflib Tag 为字母：'e' 相等、'i' 插入、'd' 删除、'r' 替换（删除+新增同时）。
func diffStats(a, b []string) (added, removed int) {
	groups := difflib.NewMatcher(a, b).GetGroupedOpCodes(3)
	for _, ops := range groups {
		for _, op := range ops {
			switch op.Tag {
			case 'i':
				added += op.J2 - op.J1
			case 'd':
				removed += op.I2 - op.I1
			case 'r':
				removed += op.I2 - op.I1
				added += op.J2 - op.J1
			}
		}
	}
	return added, removed
}

// splitLines 按行切分并保留行尾 \n：
//   - 结尾有 \n 时丢弃 SplitAfter 产生的末尾空元素（末个 \n 后的空余）；
//   - 结尾无 \n 时末行保留（difflib.SplitLines 会丢弃末尾无换行的内容，
//     而文件常无结尾换行——不可直接使用）。
//
// 空串返回 nil。
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// truncateUTF8 按字节上限截断且不劈裂多字节字符（预览截断的安全边界）。
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
