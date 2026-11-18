package builtin

// 搜索工具（grep / glob）的可复现基准。
//
// 为什么用合成工作区而不是真实仓库：真实仓库的扫描成本受机器负载、页缓存、
// 文件系统状态影响（2026-11-17 实测：同一份代码量到 3m18s 和 10m30s 两种值），
// 没法当回归基线。合成树的**形状**取自真实仓库的实测比例，问题结构才复现得出来：
//
//	真实仓库（go-code，2026-11-17 实测）      合成树
//	─────────────────────────────            ─────────────────────
//	node_modules  72,829 文件 / 2.06 GB       node_modules/ 1,000 文件 / 8 MB
//	.go-code      7,929 文件 / 2.65 GB        .state/       150 文件 / 1.2 MB
//	.gocache      5,080 文件 / 1.01 GB        vendor/       300 文件 / 2.4 MB
//	源码          43,602 文件 / ~3.6 GB        src/          500 文件 / 1.2 MB
//	合计         116,431 文件 / 9.3 GB        合计         ~2,000 文件 / 13 MB
//
// 合成树的 .gitignore 覆盖 .state/ 与 vendor/（与真实仓库一致：依赖目录进
// 内置黑名单，本机状态目录与 vendor 靠仓库自己的 .gitignore）。
//
// 判据以 **allocs/op 与 B/op** 为主（与机器负载无关，逐字节可复现），
// ns/op 只作参考 —— 这台机器常年 load 13–47，墙钟数字噪声可达 3×。
//
// 合成树**全局只建一次**（TestMain 前置 + 退出时清理）：每建一次要写+删 2000 个文件，
// 实测占整个基准运行时间的一半。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	benchSrcFiles   = 500
	benchDepPkgs    = 25
	benchDepPerPkg  = 40
	benchVendorFile = 300
	benchStateFiles = 150
	benchBigLines   = 50000
)

var (
	benchOnce sync.Once
	benchRoot string
)

// TestMain 建一次合成工作区，跑完清掉。
func TestMain(m *testing.M) {
	code := m.Run()
	if benchRoot != "" {
		_ = os.RemoveAll(benchRoot)
	}
	os.Exit(code)
}

// buildSyntheticWorkspaceInto 在 dir 下造一棵形状接近真实仓库的树。
func buildSyntheticWorkspaceInto(tb testing.TB, dir string) {
	tb.Helper()
	write := func(rel, content string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	// 源码树
	body := strings.Repeat("package main\n// filler line to make the file realistic in size\n", 60)
	for i := 0; i < benchSrcFiles; i++ {
		write(fmt.Sprintf("src/pkg%02d/file%03d.go", i%20, i), body)
	}
	// 依赖树（node_modules 形状：文件多、内容小）
	for p := 0; p < benchDepPkgs; p++ {
		for i := 0; i < benchDepPerPkg; i++ {
			write(fmt.Sprintf("node_modules/pkg%02d/node%03d/index.js", p, i), strings.Repeat("var x=1;\n", 220))
		}
	}
	// vendor
	for i := 0; i < benchVendorFile; i++ {
		write(fmt.Sprintf("vendor/lib%02d/lib.go", i%10), body)
	}
	// 本机状态目录（.go-code / .gocache 形状）
	for i := 0; i < benchStateFiles; i++ {
		write(fmt.Sprintf(".state/blob%03d", i), strings.Repeat("payload ", 500))
	}
	// 大文件（会话 transcript 形状）
	var big strings.Builder
	for i := 0; i < benchBigLines; i++ {
		fmt.Fprintf(&big, "session event %d {\"payload\":\"....\"}\n", i)
	}
	write("logs/big.txt", big.String())
	// .gitignore：声明两个忽略规则
	write(".gitignore", "ignored/\n*.log\n.state/\nvendor/\n")
	write("ignored/a.txt", "needle in an ignored file\n")
	write("debug.log", "needle in an ignored log\n")
}

// benchWorkspace 返回全局唯一的合成工作区（首次调用时建）。
func benchWorkspace(tb testing.TB) string {
	tb.Helper()
	benchOnce.Do(func() {
		dir, err := os.MkdirTemp("", "hai-search-bench-")
		if err != nil {
			tb.Fatal(err)
		}
		buildSyntheticWorkspaceInto(tb, dir)
		benchRoot = dir
	})
	return benchRoot
}

func benchGrepTool(tb testing.TB, root string) *grepTool {
	tb.Helper()
	return newGrepTool(NewFileTools(root))
}

func benchGlobTool(tb testing.TB, root string) *globTool {
	tb.Helper()
	return newGlobTool(NewFileTools(root))
}

// BenchmarkGrepNoMatch 必然不命中 → 强制走完整个文件树（最坏路径）。
func BenchmarkGrepNoMatch(b *testing.B) {
	root := benchWorkspace(b)
	g := benchGrepTool(b, root)
	args := `{"pattern":"zzz_no_such_token_zzz"}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := g.Call(context.Background(), "", args); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGrepMatch 命中少量文件 → 走「早退出」路径。
func BenchmarkGrepMatch(b *testing.B) {
	root := benchWorkspace(b)
	g := benchGrepTool(b, root)
	args := `{"pattern":"needle"}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := g.Call(context.Background(), "", args); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGrepInclude include 过滤（只开匹配文件，但不读被排除文件的内容）。
func BenchmarkGrepInclude(b *testing.B) {
	root := benchWorkspace(b)
	g := benchGrepTool(b, root)
	args := `{"pattern":"zzz_no_such_token_zzz","include":"*.go"}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := g.Call(context.Background(), "", args); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGlobAll glob 遍历全树（不命中 → 完整遍历）。
func BenchmarkGlobAll(b *testing.B) {
	root := benchWorkspace(b)
	g := benchGlobTool(b, root)
	args := `{"pattern":"**/*.nomatch"}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := g.Call(context.Background(), "", args); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGrepNoMatchNoIgnore 对照组：no_ignore=true（不跳过任何目录）。
// 与 BenchmarkGrepNoMatch 的差值 = 忽略规则（内置黑名单 + .gitignore）的净收益。
func BenchmarkGrepNoMatchNoIgnore(b *testing.B) {
	root := benchWorkspace(b)
	g := benchGrepTool(b, root)
	args := `{"pattern":"zzz_no_such_token_zzz","no_ignore":true}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := g.Call(context.Background(), "", args); err != nil {
			b.Fatal(err)
		}
	}
}

// TestSearchWorkspaceShape 把合成树的形状钉住（防止基准悄悄变形）。
func TestSearchWorkspaceShape(t *testing.T) {
	root := benchWorkspace(t)
	var files, bytes int64
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		files++
		if fi, err := os.Stat(p); err == nil {
			bytes += fi.Size()
		}
		return nil
	})
	t.Logf("合成工作区：%d 文件 / %.1f MB", files, float64(bytes)/(1<<20))
	if files < 1500 {
		t.Fatalf("合成树文件数异常：%d", files)
	}
}
