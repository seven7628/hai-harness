// memory_dream：离线记忆盘点（P4 v1，**只报不改**）。
//
// 用法：
//
//	go run ./memory/cmd/memory_dream                      # 盘点全部仓库
//	go run ./memory/cmd/memory_dream --repo <repo-id>     # 只看一个
//	go run ./memory/cmd/memory_dream --root <dir>         # 换记忆根（默认 ~/.go-code/memory/repos）
//
// 为什么单独一个命令行工具而不是接进 GUI：v1 的目的是"让人先看几轮报告，再决定
// 哪一类可以自动化"。做成 GUI 按钮等于提前承诺它会定期跑、结果会被当真；而一份
// 没人看的报告比没有报告更糟。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/seven7628/hai-harness/memory"
)

func main() {
	var (
		root = flag.String("root", "", "记忆根目录（默认 ~/.go-code/memory/repos）")
		repo = flag.String("repo", "", "只盘点这个 repo-id")
	)
	flag.Parse()

	base := *root
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fail("取不到 home 目录：%v", err)
		}
		base = filepath.Join(home, ".go-code", "memory", "repos")
	}

	entries, err := os.ReadDir(base)
	if err != nil {
		fail("读记忆根失败：%v", err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && (*repo == "" || e.Name() == *repo) {
			ids = append(ids, e.Name())
		}
	}
	if len(ids) == 0 {
		fail("在 %s 下没有找到匹配的记忆仓库", base)
	}
	sort.Strings(ids)

	now := time.Now()
	cfg := memory.Config{}
	for i, id := range ids {
		if i > 0 {
			fmt.Println("\n---\n")
		}
		rep, err := cfg.Dream(memory.Dir{ID: id, Path: filepath.Join(base, id)})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[dream] %s 读取失败：%v\n", id, err)
			continue
		}
		fmt.Print(rep.Markdown(now))
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
