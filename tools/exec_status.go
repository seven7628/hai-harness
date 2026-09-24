package tools

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
)

// ExecSink 单次调用的命令执行结局收集槽（每次调用独立，放 ctx；与 ImageSink 同范式，
// 理由见 tools/types.go:70-77：工具实例在注册表里共享，并行批下暂存字段会张冠李戴）。
type ExecSink struct {
	mu       sync.Mutex
	set      bool
	cmd      string
	exitCode int
	timedOut bool
	canceled bool
}

// SetExit 由命令类工具在 Call 内调用（命令结束后一次）。
// timedOut 与 canceled 互斥（见 sandbox.ExecSpec.Canceled）：前者 = 时限到点，
// 后者 = 父 ctx 被取消（用户中断/会话关闭）；两者都必须判失败（输出是部分输出）。
func (s *ExecSink) SetExit(cmd string, exitCode int, timedOut, canceled bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.set, s.cmd, s.exitCode, s.timedOut, s.canceled = true, cmd, exitCode, timedOut, canceled
	s.mu.Unlock()
}

// Exit 读取（engine 在 Call 返回后读取）。ok=false 表示该工具未上报（非命令类）。
func (s *ExecSink) Exit() (cmd string, exitCode int, timedOut, canceled, ok bool) {
	if s == nil {
		return "", 0, false, false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd, s.exitCode, s.timedOut, s.canceled, s.set
}

type execSinkKey struct{}

func WithExecSink(ctx context.Context, s *ExecSink) context.Context {
	return context.WithValue(ctx, execSinkKey{}, s)
}

// ExecSinkFrom 取出本次调用的执行结局槽；未注入（直接调用/单测）返回 nil（nil 安全）。
func ExecSinkFrom(ctx context.Context) *ExecSink {
	s, _ := ctx.Value(execSinkKey{}).(*ExecSink)
	return s
}

// benignNonZeroExit 判断「非零退出是正常结果」的命令：退出码 1 对这些命令表示
// 「无匹配 / 有差异」而非失败（grep 无匹配、diff 有差异、test 为假）。
// 只豁免 1：exit 2 对 grep 是真实错误，exit 1 对 go build 是编译失败——都必须判为失败。
func benignNonZeroExit(cmd string, exitCode int) bool {
	if exitCode != 1 {
		return false
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	switch filepath.Base(fields[0]) {
	case "grep", "egrep", "fgrep", "rg", "rgrep", "diff", "cmp", "test", "[", "[[":
		return true
	}
	return false
}
