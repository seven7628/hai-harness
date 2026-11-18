package sandbox

// 「跑命令的地方必须封顶输出」——**全仓不变式**（第 5 轮爬山第 5 步）。
//
// 为什么要有这条测试：第 4 轮给 bash / run_python 的输出加了窗口（OutputLimit），
// 但**兄弟没治** —— refextract（PDF 抽取）、officekit 的 convert/render、codemode
// 的脚本执行，全都还在走**无界**缓冲（扩容 ~2× + String() 再拷 1×）⇒ 一条输出
// 很大的命令就能把 bridge 的堆顶穿。而模型侧最终只看得到 20KB ⇒ 中间那一大段
// **从来没人看**。
//
// 教训（写进记忆的那条）：「修了一个工具，忘了它的兄弟」是常态 ⇒ 靠人记不住 ⇒
// 靠**测试**钉住：以后谁再加一处 ExecSpec 而忘了封顶，这条测试就红。
//
// 例外（显式列出，不许偷偷加）：
//   - preview_worker 的 Start（常驻进程的 stdout 是协议通道，不是「输出」）；
//   - 测试文件（_test.go）—— 测试里的假沙箱不受生产内存约束。

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// execSpecRe 找 ExecSpec 字面量。
var execSpecRe = regexp.MustCompile(`(?s)ExecSpec\{`)

// outputLimitRe 找字面量里的 OutputLimit 字段。
var outputLimitRe = regexp.MustCompile(`OutputLimit\s*:`)

// execSpecBounds 找字面量的结束位置（配对大括号，跳过字符串/注释里的括号）。
func execSpecBounds(src string, open int) int {
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// stripComments 去掉注释（只求「这段代码里有没有 ExecSpec{」，注释里提到的
// ——比如架构说明里的 `sandbox.Run(ExecSpec{Stdin: ...})`——不算数）。
//
// 粗暴但够用：行注释到行尾，块注释整段抹掉（保留换行，行号不变）。
func stripComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case strings.HasPrefix(src[i:], "/*"):
			i += 2
			for i < len(src) && !strings.HasPrefix(src[i:], "*/") {
				if src[i] == '\n' {
					b.WriteByte('\n')
				}
				i++
			}
			if i < len(src) {
				i += 2
			}
		default:
			b.WriteByte(src[i])
			i++
		}
	}
	return b.String()
}

// repoRoot 找仓库根（从本文件所在目录往上找 go.mod）。
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("找不到仓库根（go.mod）")
	return ""
}

// TestEveryExecSpecBoundsItsOutput：每一处 ExecSpec 都必须封顶输出。
func TestEveryExecSpecBoundsItsOutput(t *testing.T) {
	root := repoRoot(t)
	var offenders []string
	checked := 0

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			// 跳过隐藏目录（.git、.go-code/worktrees/** ← 别的会话的 worktree！）与产物目录。
			if strings.HasPrefix(info.Name(), ".") {
				return filepath.SkipDir
			}
			switch info.Name() {
			case "node_modules", "dist", "release", "out":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		text := stripComments(string(src))
		for _, loc := range execSpecRe.FindAllStringIndex(text, -1) {
			open := loc[0] + len("ExecSpec") - 1 // 指向 '{'
			end := execSpecBounds(text, open)
			if end < 0 {
				continue
			}
			literal := text[open : end+1]
			rel, _ := filepath.Rel(root, path)
			line := 1 + strings.Count(text[:loc[0]], "\n")
			checked++
			if outputLimitRe.MatchString(literal) {
				continue
			}
			// 常驻进程的 Start：stdout 是协议通道，不受「输出封顶」约束。
			if strings.Contains(literal, "Start(") || strings.Contains(text[max(0, loc[0]-40):loc[0]], "Start(") {
				continue
			}
			offenders = append(offenders, rel+":"+strconv.Itoa(line))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("这些 ExecSpec 没有封顶输出（会走无界缓冲）：\n  %s\n"+
			"给它们加 OutputLimit（或在 sandbox.ExecSpec 的注释里写明为什么不需要）",
			strings.Join(offenders, "\n  "))
	}
	t.Logf("已检查 %d 处 ExecSpec，全部封顶 ✓", checked)
}
