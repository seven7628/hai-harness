// computer_check：Computer Use M0 端到端自检工具（真实 Swift helper）。
//
// 用法：go run ./computer/cmd/computer_check <helper-路径>
// 依次验证：hello 握手 / perm_status / screen_info / snapshot，全部通过后
// 打印前台 App 树深度摘要。用于 M0 打通 Go stdio 客户端 ↔ Swift helper，
// 以及后续开发期手动冒烟（对齐 im_send_check 先例）。
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/seven7628/hai-harness/computer"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: computer_check <helper-路径>")
		fmt.Fprintln(os.Stderr, "  例: go run ./computer/cmd/computer_check desktop/computer-helper/computer-helper")
		os.Exit(2)
	}
	helper := os.Args[1]
	be := computer.NewStdioBackend(helper)
	defer be.Close()

	// perm_status（首次调用内含 hello 握手）
	ps, err := be.PermStatus()
	if err != nil {
		fatal("perm_status", err)
	}
	fmt.Printf("perm_status: accessibility=%v screenCapture=%v trustedApp=%q\n",
		ps.Accessibility, ps.ScreenCapture, ps.TrustedApp)
	if !ps.Accessibility {
		fmt.Fprintln(os.Stderr, "提示：辅助功能未授权——在 系统设置→隐私与安全性→辅助功能 中允许本终端/宿主，再重试")
		os.Exit(1)
	}

	si, err := be.ScreenInfo()
	if err != nil {
		fatal("screen_info", err)
	}
	fmt.Printf("screen_info: %.0fx%.0f scale=%.1f screens=%d\n",
		si.Size.W, si.Size.H, si.ScaleFactor, len(si.Screens))

	sn, err := be.Snapshot(computer.SnapshotOpts{MaxDepth: 5})
	if err != nil {
		fatal("snapshot", err)
	}
	fmt.Printf("snapshot: frontmost=%s pid=%d windows=%d\n", sn.FrontmostApp, sn.Pid, len(sn.Windows))
	if sn.Tree != nil {
		interactive, nodes := countTree(sn.Tree)
		fmt.Printf("  tree: 节点=%d 可交互=%d 示例: %s\n", nodes, interactive, sample(sn.Tree))
	}
	if b, err := json.Marshal(sn); err == nil {
		fmt.Printf("  snapshot JSON: %d bytes\n", len(b))
	}
	fmt.Println("E2E OK")
}

func fatal(step string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", step, err)
	os.Exit(1)
}

func countTree(e *computer.Element) (interactive, nodes int) {
	nodes = 1
	if len(e.Actions) > 0 || e.Focusable {
		interactive = 1
	}
	for _, c := range e.Children {
		i, n := countTree(c)
		interactive += i
		nodes += n
	}
	return
}

func sample(e *computer.Element) string {
	if e.Title != "" || len(e.Actions) > 0 {
		return fmt.Sprintf("[%s %q actions=%v]", e.Role, e.Title, e.Actions)
	}
	for _, c := range e.Children {
		if s := sample(c); s != "" {
			return s
		}
	}
	return ""
}
