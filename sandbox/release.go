package sandbox

import (
	"context"
	"regexp"
	"strings"
)

// 沙箱拦截注释（追加到返回文本，模型与用户均可见；对应 bash 工具「输出与原因并入
// 文本」的语义，模型看到后自行修正）。
const (
	hintSandboxBlocked = "[sandbox: 该操作被沙箱拦截 (Operation not permitted)]"
	hintSandboxRelease = "[sandbox: 已被沙箱拦截，用户放行后绕过沙箱执行]"
	hintSandboxRefused = "[sandbox: 沙箱拦截，用户拒绝放行]"
)

// ReleaseGate 沙箱拦截放行决策，宿主注入（bridge 接桌面确认模态）。私有接口，一期
// 不开事件通道。语义同 events.Approver 的同步阻塞：Run 等待用户决策期间工具调用阻塞。
type ReleaseGate interface {
	// Release 返回是否放行该命令（true = 调用方用 NoSandbox 绕过沙箱重跑一次）。
	Release(ctx context.Context, spec ExecSpec, blockHint string) bool
}

// releaseSandbox 放行包装：命中沙箱拦截标记时经 gate 决策。
type releaseSandbox struct {
	inner Sandbox
	gate  ReleaseGate
}

// WithRelease 包装 inner：命中沙箱拦截标记时
//   - gate 为 nil：返回文本追加拦截注释（只感知不打扰）；
//   - gate 返回 true：NoSandbox 重跑一次，追加放行注释；
//   - gate 返回 false：保留拦截文本，追加拒绝注释。
//
// 库默认 nil gate = 纯注释；bridge 注入桌面确认门后触发交互。BashTool 无感知
// （仍是 Sandbox 接口调用），放行逻辑全部收敛在包装内。
func WithRelease(inner Sandbox, gate ReleaseGate) Sandbox {
	return &releaseSandbox{inner: inner, gate: gate}
}

func (r *releaseSandbox) Run(ctx context.Context, spec ExecSpec) (string, error) {
	out, err := r.inner.Run(ctx, spec)
	if !IsSeatbeltBlocked(out) {
		return out, err
	}
	// 放行门提示：命令 + 从沙箱输出提取的被拒路径/操作（否则用户只看到命令，不知道
	// 具体访问了什么、为何被拒——见 docs/SEATBELT_SANDBOX.md）。
	hint := blockHint(spec.Command)
	if detail := deniedDetail(out); detail != "" {
		hint += " ｜ 被拒访问: " + detail
	}
	if r.gate == nil {
		return appendHint(out, hintSandboxBlocked), err
	}
	if r.gate.Release(ctx, spec, hint) {
		rerun, rerr := (NoSandbox{}).Run(ctx, spec)
		if rerr != nil {
			return rerun, rerr
		}
		return appendHint(rerun, hintSandboxRelease), nil
	}
	return appendHint(out, hintSandboxRefused), err
}

// deniedDetail 从沙箱拦截输出提取被拒访问的具体路径/操作（供用户确认模态与返回文本）。
// seatbelt 拦截时 stderr 形如 "…Operation not permitted: /path…" / "sh: /path: Operation
// not permitted" / "deny file-read* (subpath …)"；提取其中路径类 token，去重保序。
var pathTokenRe = regexp.MustCompile(`/[A-Za-z0-9_\-./][^\s:)]*`)

func deniedDetail(out string) string {
	var got []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if !isDenyLine(line) {
			continue
		}
		for _, tok := range pathTokenRe.FindAllString(line, -1) {
			if !seen[tok] {
				seen[tok] = true
				got = append(got, tok)
			}
		}
	}
	return strings.Join(got, " ")
}

// isDenyLine 是否可能是描述拦截的 stderr 行（非业务输出）。中英文都收：中文 macOS
// 本地化 EPERM，只认英文会漏判（见 seatbelt_common.go seatbeltBlockMarkers 注释）。
func isDenyLine(line string) bool {
	for _, m := range []string{"Operation not permitted", "deny file-", "deny network-", "deny mach-", "操作不被允许", "权限被拒绝"} {
		if strings.Contains(line, m) {
			return true
		}
	}
	return false
}

// appendHint 合并输出末尾追加一行注释（不吞已有内容）。
func appendHint(out, hint string) string {
	return strings.TrimRight(out, "\n") + "\n" + hint
}

// blockHint 截取命令前几字符作为用户确认模态的摘要（rune 安全：多字节命令不切半）。
func blockHint(command string) string {
	const max = 120
	r := []rune(command)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return command
}
