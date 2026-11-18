package builtin

// diff 计算的**成本与正确性**测试（2026-11-17，第 9 步）。
//
// 两条改造：
//  1. **只匹配一次**：统计从 unified 文本数（不再单独跑一遍 GetGroupedOpCodes）；
//  2. **大文件保护**：掐掉公共头尾后，中间仍超上限 ⇒ 不跑 O(N²) 的匹配器，
//     改报「整块替换」的粗粒度事实。

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestDiffStatsMatchOpcodes 新的统计口径与旧的 opcodes 口径必须**逐例一致**。
func TestDiffStatsMatchOpcodes(t *testing.T) {
	cases := []struct {
		name     string
		old, new string
	}{
		{"单行替换", "a\nb\nc\n", "a\nB\nc\n"},
		{"删一行", "a\nb\nc\n", "a\nc\n"},
		{"加一行", "a\nb\n", "a\nX\nb\n"},
		{"空→多行", "", "x\ny\n"},
		{"多行→空", "x\ny\n", ""},
		{"完全相同", "same\n", "same\n"},
		{"整块替换", "a\nb\nc\nd\n", "x\ny\n"},
		{"内容以 + 开头", "a\n", "a\n+plus\n"},
		{"内容以 - 开头", "a\n", "a\n-minus\n"},
		{"内容以 --- 开头", "a\n", "a\n--- tricky\n"},
	}
	for _, c := range cases {
		al, bl := splitLines(c.old), splitLines(c.new)
		wantAdded, wantRemoved := diffStats(al, bl)
		d := computeFileDiff("x", c.old, c.new)
		if d.Added != wantAdded || d.Removed != wantRemoved {
			t.Errorf("%s: 统计不一致 new=+%d -%d，opcodes 口径=+%d -%d",
				c.name, d.Added, d.Removed, wantAdded, wantRemoved)
		}
	}
}

// TestDiffTooLargeIsCoarseAndFast 大文件 ⇒ 粗粒度事实 + Truncated + 很快。
func TestDiffTooLargeIsCoarseAndFast(t *testing.T) {
	const n = 30000
	var a, b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&a, "old line %d\n", i)
		fmt.Fprintf(&b, "new line %d\n", i)
	}
	start := time.Now()
	d := computeFileDiff("x", a.String(), b.String())
	elapsed := time.Since(start)

	if !d.Truncated {
		t.Errorf("超大 diff 应标 Truncated")
	}
	if d.Added != n || d.Removed != n {
		t.Errorf("粗粒度事实不对：+%d -%d，want +%d -%d", d.Added, d.Removed, n, n)
	}
	// 关键：不跑 O(N²) 的匹配器（30k 行跑匹配器要 ~150 ms，两遍更久）
	if elapsed > 100*time.Millisecond {
		t.Errorf("大 diff 计算太慢: %v（应直接报粗粒度事实）", elapsed)
	}
	t.Logf("30k 行整块替换 → %v（+ %d - %d, truncated=%v）", elapsed, d.Added, d.Removed, d.Truncated)
}

// TestDiffSmallChangeInBigFileKeepsPrecision 大文件里的小改动仍要**精确**统计
// （掐头去尾后中间很小 ⇒ 照常跑匹配器）。
func TestDiffSmallChangeInBigFileKeepsPrecision(t *testing.T) {
	const n = 30000
	var a, b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&a, "line %d\n", i)
		if i == 15000 {
			fmt.Fprintf(&b, "line %d CHANGED\n", i)
		} else {
			fmt.Fprintf(&b, "line %d\n", i)
		}
	}
	d := computeFileDiff("x", a.String(), b.String())
	if d.Added != 1 || d.Removed != 1 {
		t.Errorf("大文件里的小改动应精确统计：+%d -%d，want +1 -1", d.Added, d.Removed)
	}
	if d.Truncated {
		t.Errorf("小改动不该标 Truncated")
	}
}
