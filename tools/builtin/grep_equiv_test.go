package builtin

// grep 改造（bufio.Scanner → bufio.Reader 流式 + 两遍扫描）的**输出等价性**对照。
//
// 参照实现 legacyGrepFile 是改造前的逐字复制（bufio.Scanner + 稠密 lines 切片），
// 放在测试里当 oracle：本文件逐字节比对两者的渲染结果，任何语义漂移都会被抓住。
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
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(cases[name]), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, ctx := range []int{0, 1, 3} {
		for _, pat := range []string{"MATCH", "a", "^$", "x{100}"} {
			re := regexp.MustCompile(pat)
			var totalOld int
			sc := newGrepScanner()
			win := &grepMatchWindow{max: 1000} // 跨文件共用（全局命中序号）
			for _, name := range names {
				abs := filepath.Join(dir, name)

				// 新实现
				nfr, ntrunc, nerr := grepFile(context.Background(), re, sc, abs, name, ctx, win)
				var nb strings.Builder
				if nerr == nil && len(nfr.matches) > 0 {
					renderGrepResult(nfr, ctx, &nb)
				}

				// 参照实现
				ofr, otrunc, oerr := legacyGrepFile(re, abs, name, ctx, &totalOld, 1000)
				var ob strings.Builder
				if oerr == nil && len(ofr.matches) > 0 {
					legacyRender(ofr, ctx, &ob)
				}

				if (nerr == nil) != (oerr == nil) {
					t.Errorf("%s pat=%q ctx=%d: err 不一致 new=%v old=%v", name, pat, ctx, nerr, oerr)
					continue
				}
				if ntrunc != otrunc {
					t.Errorf("%s pat=%q ctx=%d: truncated 不一致 new=%v old=%v", name, pat, ctx, ntrunc, otrunc)
				}
				if nb.String() != ob.String() {
					t.Errorf("%s pat=%q ctx=%d 输出不一致:\n--- new ---\n%s\n--- old ---\n%s",
						name, pat, ctx, nb.String(), ob.String())
				}
			}
			if win.total != totalOld {
				t.Errorf("pat=%q ctx=%d: 命中计数不一致 new=%d old=%d", pat, ctx, win.total, totalOld)
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

	var totalOld int
	sc := newGrepScanner()
	win := &grepMatchWindow{max: 1000}
	nfr, _, nerr := grepFile(context.Background(), re, sc, p, "huge.txt", 0, win)
	if nerr != nil {
		t.Fatalf("新实现不该报错: %v", nerr)
	}
	if len(nfr.matches) != 1 || nfr.matches[0] != 1 {
		t.Errorf("新实现应命中第 1 行, got %v", nfr.matches)
	}

	// 参照实现：整个文件被丢弃（错误）
	_, _, oerr := legacyGrepFile(re, p, "huge.txt", 0, &totalOld, 1000)
	if oerr == nil {
		t.Errorf("参照实现（改造前）应因超长行报错（整文件丢弃），实际没报错")
	}
}
