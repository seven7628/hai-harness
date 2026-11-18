package builtin

// 尾巴窗口的环形缓冲（第 5 轮爬山第 3 步）。
//
// 改造前：窗口 = `append` + 前移切片（`w = w[len-4096:]`）+ rune 边界修剪。
// 改造后：固定 4KB 的环形缓冲（写入零分配）。
//
// 这条测试是**差分**测试：把改造前的实现原样搬进来当对照，随机灌入各种块
// （含多字节字符、不同长度），**每写一块都比一次窗口内容** ⇒ 逐字节一致才算过。
// （环形缓冲最容易错的地方就是「绕回」与「左边界落在 rune 中间」两处。）

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// oldTailWindow 改造前的窗口实现（原样搬来，只作对照用）。
type oldTailWindow struct {
	buf []byte
}

func (w *oldTailWindow) write(p []byte) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > maxTailBytes {
		w.buf = w.buf[len(w.buf)-maxTailBytes:]
		// 窗口左边界可能落在多字节字符中间：往后挪到最近的 rune 边界。
		for len(w.buf) > 0 && !utf8.RuneStart(w.buf[0]) {
			w.buf = w.buf[1:]
		}
	}
}

func (w *oldTailWindow) window() string { return string(w.buf) }

// TestTailRingMatchesOldWindowByteForByte：环形缓冲 ≡ 改造前（逐字节）。
func TestTailRingMatchesOldWindowByteForByte(t *testing.T) {
	rng := rand.New(rand.NewSource(20261118))
	// 素材：ASCII + 中文 + emoji（多字节，长度不一 ⇒ 能撞出各种 rune 边界）
	alphabet := []string{"a", "b", " ", "\n", "中", "文", "错", "误", "🙂", "→", "ok"}

	var ring tailRing
	var old oldTailWindow

	for i := 0; i < 400; i++ {
		// 随机块大小：1 ~ 3*maxTailBytes（必须能一次塞满并绕回多圈）
		n := rng.Intn(3*maxTailBytes) + 1
		var sb strings.Builder
		for sb.Len() < n {
			sb.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		chunk := []byte(sb.String())

		ring.write(chunk)
		old.write(chunk)

		got, want := ring.window(), old.window()
		if got != want {
			t.Fatalf("第 %d 块之后窗口不一致：\n got=%q\nwant=%q", i, got, want)
		}
	}
}

// TestTailRingWriteIsAllocationFree：写入路径**零分配**（这是本次改造的目的）。
func TestTailRingWriteIsAllocationFree(t *testing.T) {
	var ring tailRing
	chunk := make([]byte, 32<<10)

	// 先把环撑满（第一次会分配那块 4KB 的数组）。
	ring.write(chunk)

	allocs := testing.AllocsPerRun(200, func() {
		ring.write(chunk)
	})
	if allocs > 0 {
		t.Fatalf("环形缓冲的写入路径不该有分配，实测每 100 次 %v 次", allocs)
	}
}

// TestTailRingWindowNeverExceedsLimit：窗口永远不超过上限，且内容总是最新。
func TestTailRingWindowNeverExceedsLimit(t *testing.T) {
	var ring tailRing
	var all []byte
	for i := 0; i < 50; i++ {
		chunk := []byte(strings.Repeat("x", 1000))
		ring.write(chunk)
		all = append(all, chunk...)
		if w := ring.window(); len(w) > maxTailBytes {
			t.Fatalf("窗口 %d 字节，超过上限 %d", len(w), maxTailBytes)
		}
	}
	got := ring.window()
	want := string(all[len(all)-maxTailBytes:])
	if got != want {
		t.Fatalf("窗口应是最后 %d 字节", maxTailBytes)
	}
}
