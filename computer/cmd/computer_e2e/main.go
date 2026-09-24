// computer_e2e：Computer Use 真机 E2E 剧本工具（真实 Swift helper + 完整
// Executor 决策链）。用于 P0 真机验证与开发期交互调试。
//
// 用法：
//
//	go run ./computer/cmd/computer_e2e -helper desktop/computer-helper/computer-helper open 日历
//	go run ./computer/cmd/computer_e2e ... snapshot
//	go run ./computer/cmd/computer_e2e ... press -uid a:1:0:1
//	go run ./computer/cmd/computer_e2e ... type -text "你好"
//	go run ./computer/cmd/computer_e2e ... key -keys cmd+n
//	go run ./computer/cmd/computer_e2e ... verify -text 保存
//	go run ./computer/cmd/computer_e2e ... menu 日历 文件 新建事件
//
// 每个动作自动走 Executor（含验证闭环/错误文本化），输出模型同款文本——
// 即会话里模型调用工具所见。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/seven7628/hai-harness/computer"
)

func main() {
	fs := flag.NewFlagSet("computer_e2e", flag.ExitOnError)
	helper := fs.String("helper", "desktop/computer-helper/computer-helper", "helper 路径")
	_ = fs.Parse(os.Args[1:])
	rest := fs.Args()
	if len(rest) < 1 {
		usage()
		os.Exit(2)
	}

	be := computer.NewStdioBackend(*helper)
	defer be.Close()
	ps, err := be.PermStatus()
	if err != nil {
		fmt.Println("helper 启动失败:", err)
		os.Exit(1)
	}
	if !ps.Accessibility {
		fmt.Println("✗ 辅助功能未授权。请在 系统设置→隐私与安全性→辅助功能 允许本终端。")
		os.Exit(1)
	}

	ex := computer.NewExecutor(be, computer.ExecutorConfig{
		StabilizeDelay: 150 * time.Millisecond,
	})
	ctx := context.Background()

	// 需要 uid 的动作先确保快照索引（模拟真实模型「先 snapshot 再动作」习惯）。
	needsIndex := map[string]bool{"press": true, "type": true}
	if needsIndex[rest[0]] && (flagArg(rest, "uid") != "" || flagArg(rest, "target_uid") != "") {
		if _, err := ex.Snapshot(ctx, "", 8000); err != nil {
			printResult("", err)
			os.Exit(1)
		}
	}

	cmd := rest[0]
	switch cmd {
	case "open":
		if len(rest) < 2 {
			usage()
			os.Exit(2)
		}
		out, err := ex.OpenApp(ctx, rest[1])
		printResult(out, err)
	case "snapshot":
		out, err := ex.Snapshot(ctx, "", 8000)
		printResult(out, err)
	case "press":
		uid, title := flagArg(rest, "uid"), flagArg(rest, "title")
		var out string
		var err error
		switch {
		case uid != "":
			out, err = ex.Press(ctx, uid)
		case title != "":
			out, err = ex.PressTitle(ctx, title)
		default:
			fmt.Println("press 需要 -uid <uid> 或 -title <文本>")
			os.Exit(2)
		}
		printResult(out, err)
	case "menu":
		if len(rest) < 3 {
			usage()
			os.Exit(2)
		}
		out, err := ex.PressMenu(ctx, rest[1], rest[2:])
		printResult(out, err)
	case "type":
		text := flagArg(rest, "text")
		target := flagArg(rest, "target_uid")
		if text == "" {
			fmt.Println("type 需要 -text")
			os.Exit(2)
		}
		out, err := ex.Type(ctx, text, target)
		printResult(out, err)
	case "key":
		keys := flagArg(rest, "keys")
		if keys == "" {
			fmt.Println("key 需要 -keys")
			os.Exit(2)
		}
		out, err := ex.Key(ctx, keys)
		printResult(out, err)
	case "verify":
		text := flagArg(rest, "text")
		gone := flagBool(rest, "gone")
		out, err := ex.WaitVerify(ctx, text, gone, 5000)
		printResult(out, err)
	case "find":
		text := flagArg(rest, "text")
		out, err := ex.Find(ctx, text, "", "")
		printResult(out, err)
	case "click":
		// 像素点击（坐标由 AX frame 换算；开发调试用）
		x := flagFloat(rest, "x")
		y := flagFloat(rest, "y")
		if err := be.Click(computer.Point{X: x, Y: y}); err != nil {
			fmt.Println("ERROR:", err)
			os.Exit(1)
		}
		fmt.Printf("✅ 已点击 (%.0f, %.0f)\n", x, y)
	case "script":
		// script calendar / reminder / wps 等真机剧本（单进程连跑）
		if len(rest) < 2 {
			fmt.Println("script 需要剧本名：calendar | reminder | wps")
			os.Exit(2)
		}
		var runErr error
		switch rest[1] {
		case "calendar":
			runErr = runCalendarScript(ctx, ex, be)
		case "reminder":
			runErr = runReminderScript(ctx, ex, be)
		case "wps":
			runErr = runWPSScript(ctx, ex, be)
		default:
			fmt.Println("未知剧本:", rest[1])
			os.Exit(2)
		}
		if runErr != nil {
			fmt.Println(runErr)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func printResult(out string, err error) {
	if err != nil {
		fmt.Println("ERROR:", err)
		os.Exit(1)
	}
	fmt.Println(out)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: computer_e2e -helper <path> <cmd> [args]
cmds:
  open <app名>              打开/激活应用
  snapshot                  语义快照（uid 树文本）
  press -uid <uid> | -title <文本>   语义按压
  menu <app> <菜单路径...>   菜单按压（如 menu 日历 文件 新建事件）
  type -text <文本> [-target_uid <uid>]   键入
  key -keys <组合>          按键（cmd+n / Return / Tab…）
  verify -text <文本> [-gone]   等待出现/消失
  find -text <文本>         增量检索`)
}

// flagArg 从 rest 中解析 -key value（命令后参数）。
func flagArg(rest []string, key string) string {
	for i := 1; i < len(rest)-1; i++ {
		if rest[i] == "-"+key {
			return rest[i+1]
		}
	}
	return ""
}

func flagBool(rest []string, key string) bool {
	for _, a := range rest[1:] {
		if a == "-"+key {
			return true
		}
	}
	return false
}

func flagFloat(rest []string, key string) float64 {
	v := flagArg(rest, key)
	if v == "" {
		return 0
	}
	var f float64
	_, _ = fmt.Sscanf(v, "%f", &f)
	return f
}
