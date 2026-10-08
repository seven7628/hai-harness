//go:build darwin

package execproc

// script_seatbelt_darwin_test.go：ExecuteScript 在 **Seatbelt 后端**下的端到端回归。
//
// 为什么单独立一条：桌面端 `sandbox mode = seatbelt` 时，codemode 与 bash 共用同一个
// sandbox 实例（execproc 包注释的前提），所以这条路径就是生产路径之一。它同时压住三件
// 只在 Seatbelt 下才成立的事：
//  1. **stdin 必须被转发**（sandbox.ExecSpec.Stdin → runExec）：桥接的 host→sandbox 通道
//     就是 stdin，Seatbelt 曾静默丢弃它（那时只改了 NoSandbox.Run）—— 本测试会在那个
//     回归下失败（脚本收不到 init → 桥接 die）；
//  2. **临时资材必须在 /tmp**：策略里 `file-read*` 与 `file-write*` 唯一同时放行的位置
//     （$TMPDIR=/var/folders 能读不能写 → 帧 spool 写失败）；
//  3. node 可执行文件与 data: URL 形式的 prelude 在策略内可执行/可加载。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/sandbox"
)

func TestExecuteScriptUnderSeatbelt(t *testing.T) {
	if !sandbox.Available() {
		t.Skip("sandbox-exec 不可用（非 darwin 或未安装），跳过真实沙箱用例")
	}
	ws := t.TempDir()
	sb, err := sandbox.NewSeatbelt(ws)
	if err != nil {
		t.Skipf("Seatbelt 构造失败（本环境不支持真实沙箱）: %v", err)
	}
	requireNode(t)

	e := New(sb, nil)
	res, err := e.ExecuteScript(context.Background(), ScriptOpts{
		Cwd:     ws,
		Timeout: 30 * time.Second,
		// 注意：这里**不用** 顶层 return —— 当前桥接把脚本作为 ESM 入口模块求值，
		// `return` 是 SyntaxError（pi 的「顶层 return 即结果」语义需要 AsyncFunction
		// 求值，见 IMPLEMENTATION-SPEC §7.8 的待办）。本用例只压 Seatbelt 通路本身。
		Script: `
const got = await tools.echo({n: 42});
console.log("answer=" + got.n);
`,
		Init: json.RawMessage(`{"tools":[{"name":"echo"}]}`),
		OnCall: func(context.Context, ScriptCall) ScriptResult {
			return ScriptResult{Value: json.RawMessage(`{"n":42}`)}
		},
	})
	if err != nil {
		t.Fatalf("ExecuteScript(Seatbelt): %v (raw=%q)", err, res.Raw)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0（Seatbelt 下桥接没能跑通？）\nText=%q\nRaw=%q",
			res.ExitCode, res.Text, res.Raw)
	}
	if !strings.Contains(res.Text, "answer=42") {
		t.Fatalf("脚本输出/工具回执没回来:\nText=%q\nRaw=%q", res.Text, res.Raw)
	}
	if strings.Contains(res.Text, `"notify"`) {
		t.Fatalf("协议帧泄进模型可见文本:\n%s", res.Text)
	}
	// 握手成功（prelude 起来了）—— Seatbelt 下 prelude 走 data: URL + --import。
	if res.Hello.Major != protoMajor {
		t.Fatalf("Hello.Major = %d, want %d", res.Hello.Major, protoMajor)
	}
}
