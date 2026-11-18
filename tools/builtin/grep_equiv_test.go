package builtin

// grep 改造（bufio.Scanner → bufio.Reader 流式 + 两遍扫描 + 并行扫描）的**输出等价性**对照。
//
// 参照实现 legacyGrepFile 是改造前的逐字复制（bufio.Scanner + 稠密 lines 切片），
// 放在测试里当 oracle：本测试逐字节比对两者的渲染结果，任何语义漂移都会被抓住。
//
// **并行改造后仍然逐字节一致** —— 这是并行化的安全网：
// 遍历保持串行（顺序确定），并行的是「读文件内容」，收集时按遍历序号排回来。
//
// 唯一的**有意**差异在 TestGrepLongLineBeyondCap：改造前超过 4 MB 的单行会让
// bufio.Scanner 报 ErrTooLong → 整个文件被丢弃（连已经找到的命中一起丢）；
// 改造后只是那一行被截断，文件其余部分照常搜。

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// legacyResult 改造前的 grepFileResult（稠密 lines）。
type legacyResult struct {
	rel     string
	lines   []string
	matches []int
}

// legacyGrepFile 改造前的 grepFile 逐字复制。
func legacyGrepFile(re *regexp.Regexp, abs, rel string, ctx int, total *int, max int) (legacyResult, bool, error) {
	var fr legacyResult
	fr.rel = rel
	f, err := os.Open(abs)
	if err != nil {
		return fr, false, err
	}
	defer f.Close()

	head := make([]byte, 8192)
	n, _ := f.Read(head)
	if bytesContainsZero(head[:n]) {
		return fr, false, errors.New("binary file")
	}
	if _, err := f.Seek(0, 0); err != nil {
		return fr, false, err
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	ln := 0
	for sc.Scan() {
		ln++
		content := sc.Text()
		if len(content) > maxGrepLineLength {
			content = content[:maxGrepLineLength] + "..."
		}
		fr.lines = append(fr.lines, content)
		if !re.Match(sc.Bytes()) {
			continue
		}
		*total++
		if *total > max {
			return fr, true, nil
		}
		fr.matches = append(fr.matches, ln)
	}
	return fr, false, sc.Err()
}

// legacyRender 改造前的 renderGrepResult。
func legacyRender(fr legacyResult, ctx int, b *strings.Builder) {
	if len(fr.matches) == 0 {
		return
	}
	type group struct{ start, end int }
	var groups []group
	for _, m := range fr.matches {
		s, e := m-ctx, m+ctx
		if s < 1 {
			s = 1
		}
		if e > len(fr.lines) {
			e = len(fr.lines)
		}
		if n := len(groups); n > 0 && s <= groups[n-1].end+1 {
			if e > groups[n-1].end {
				groups[n-1].end = e
			}
		} else {
			groups = append(groups, group{s, e})
		}
	}
	matchSet := make(map[int]bool, len(fr.matches))
	for _, m := range fr.matches {
		matchSet[m] = true
	}
	for gi, g := range groups {
		if gi > 0 {
			b.WriteString("--\n")
		}
		for ln := g.start; ln <= g.end; ln++ {
			if matchSet[ln] {
				fmt.Fprintf(b, "%s:%d:%s\n", fr.rel, ln, fr.lines[ln-1])
			} else {
				fmt.Fprintf(b, "%s-%d-%s\n", fr.rel, ln, fr.lines[ln-1])
			}
		}
	}
}

// legacyGrepAll 改造前的**整次调用**（多文件，按遍历顺序）。
// 遍历顺序 = 文件名字典序（filepath.WalkDir 按字典序遍历）。
func legacyGrepAll(re *regexp.Regexp, dir string, names []string, ctx, max int) (string, int) {
	var total int
	var b strings.Builder
	for _, name := range names {
		ofr, _, err := legacyGrepFile(re, filepath.Join(dir, name), name, ctx, &total, max)
		if err == nil {
			legacyRender(ofr, ctx, &b)
		}
	}
	return b.String(), total
}

// stripHeader 去掉 grep 结果首行的表头（表头是给人/模型看的，不参与等价比对）。
func stripHeader(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return ""
}

// TestGrepOutputEquivalence 逐字节比对改造前后的渲染输出。
func TestGrepOutputEquivalence(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", 100*1024) // 100 KB 单行（> 64 KB 缓冲，< 4 MB 上限）
	cases := map[string]string{
		"plain.txt":      "alpha\nbeta MATCH\ngamma\n",
		"crlf.txt":       "alpha\r\nbeta MATCH\r\ngamma\r\n",
		"noeol.txt":      "alpha\nbeta MATCH",
		"empty.txt":      "",
		"onlynl.txt":     "\n",
		"longline.txt":   "head\n" + long + " MATCH tail\nafter\n",
		"utf8.txt":       "中文行\n第二行 MATCH\n",
		"multi.txt":      "one MATCH\ntwo\nthree MATCH\nfour\nfive MATCH\n",
		"blanklines.txt": "a\n\n\nb MATCH\n\n\n",
	}
	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	sort.Strings(names) // 遍历顺序 = 字典序
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(cases[name]), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, ctx := range []int{0, 1, 3} {
		for _, pat := range []string{"MATCH", "a", "^$", "x{100}"} {
			re := regexp.MustCompile(pat)

			// 参照实现（改造前，整次调用）
			want, totalOld := legacyGrepAll(re, dir, names, ctx, 1000)

			// 新实现（并行）
			g := newGrepTool(NewFileTools(dir))
			g.maxResults = 1000
			out, err := g.Call(context.Background(), "", fmt.Sprintf(`{"pattern":%q,"context":%d}`, pat, ctx))
			if err != nil {
				t.Fatalf("pat=%q ctx=%d: %v", pat, ctx, err)
			}
			got := stripHeader(out)

			if got != want {
				t.Errorf("pat=%q ctx=%d 输出不一致:\n--- new ---\n%s\n--- old ---\n%s",
					pat, ctx, got, want)
			}
			if !strings.Contains(out, fmt.Sprintf("[grep: %d match(es) in", totalOld)) {
				t.Errorf("pat=%q ctx=%d: 命中总数不一致 new 表头=%q old=%d", pat, ctx, out, totalOld)
			}
		}
	}
}

// TestGrepParallelEquivalence 单线程 vs 多线程：输出必须逐字节一致。
//
// 这是并行化的核心安全网：worker 是乱序返回的，收集时按遍历序号排回来，
// 所以并行与串行的输出必须完全一样（含文件顺序、行号、-- 分隔）。
func TestGrepParallelEquivalence(t *testing.T) {
	dir := t.TempDir()
	// 多文件、多命中：让分页窗口（200）真的被填满，触发「截断 + 早停」路径。
	for f := 0; f < 12; f++ {
		var b strings.Builder
		for i := 0; i < 60; i++ {
			fmt.Fprintf(&b, "file%d line %d MATCH\n", f, i)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.txt", f)), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 一个命中很多的文件（考验 budget 封顶路径）。
	var many strings.Builder
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&many, "many %d MATCH\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "many.txt"), []byte(many.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, offset := range []int{0, 200, 400, 700, 1000} {
		for _, ctx := range []int{0, 2} {
			var outs [2]string
			for wi, workers := range []string{"1", "4"} {
				t.Setenv(grepWorkersEnv, workers)
				g := newGrepTool(NewFileTools(dir))
				out, err := g.Call(context.Background(), "", fmt.Sprintf(`{"pattern":"MATCH","context":%d,"offset":%d}`, ctx, offset))
				if err != nil {
					t.Fatalf("offset=%d ctx=%d workers=%s: %v", offset, ctx, workers, err)
				}
				outs[wi] = out
			}
			if outs[0] != outs[1] {
				t.Errorf("offset=%d ctx=%d: 单线程与多线程输出不一致:\n--- 1 worker ---\n%s\n--- 4 workers ---\n%s",
					offset, ctx, outs[0], outs[1])
			}
		}
	}
}

// TestGrepLongLineBeyondCap 超长单行（> 4 MB）的行为差异（有意为之）。
//
// 改造前：bufio.Scanner 的 maxTokenSize=4MB ⇒ 超长行报 ErrTooLong ⇒
//
//	grepFile 返回错误 ⇒ **整个文件被丢弃**（连已经找到的命中一起丢）。
//
// 改造后：只截断那一行，文件其余部分照常搜。
func TestGrepLongLineBeyondCap(t *testing.T) {
	dir := t.TempDir()
	huge := strings.Repeat("y", 5*1024*1024) // 5 MB 单行
	p := filepath.Join(dir, "huge.txt")
	if err := os.WriteFile(p, []byte("head MATCH\n"+huge+"\ntail\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile("MATCH")

	sc := newGrepScanner()
	lines, count, _, _, err := grepCountFile(context.Background(), re, sc, p, "huge.txt", 1000)
	if err != nil {
		t.Fatalf("新实现不该报错: %v", err)
	}
	if count != 1 || len(lines) != 1 || lines[0] != 1 {
		t.Errorf("新实现应命中第 1 行, got lines=%v count=%d", lines, count)
	}

	// 参照实现：整个文件被丢弃（错误）
	var totalOld int
	_, _, oerr := legacyGrepFile(re, p, "huge.txt", 0, &totalOld, 1000)
	if oerr == nil {
		t.Errorf("参照实现（改造前）应因超长行报错（整文件丢弃），实际没报错")
	}
}
