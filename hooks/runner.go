package hooks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	defaultHandlerLimit = 8192
	maxEventInjectBytes = 16384
	maxTimeout          = 120 * time.Second
)

// Outcome 一次事件派发的聚合结果。聚合按**配置声明顺序**（不看完成顺序，抄 Crush）。
type Outcome struct {
	Decision     Decision
	Reason       string
	UpdatedInput json.RawMessage
	ToolOutput   string // PostToolUse：可替换工具输出（可选能力）
	Contexts     []string
	SystemMsgs   []string
	Runs         []HookRun
	Warnings     []string
}

// Runner 执行外部命令 hook。并发安全（一次 Run 内多个工具可能同时派发）。
type Runner struct {
	cfg   *Config
	trust *TrustStore
	audit func(HookRun)

	mu   sync.Mutex
	seen map[string]struct{} // 会话级注入去重
}

// NewRunner 构造执行器；audit 可为 nil。
func NewRunner(cfg *Config, trust *TrustStore, audit func(HookRun)) *Runner {
	if audit == nil {
		audit = func(HookRun) {}
	}
	return &Runner{cfg: cfg, trust: trust, audit: audit, seen: map[string]struct{}{}}
}

// Dispatch 派发一个事件：matcher 过滤 → 信任/开关过滤 → 去重 → 并行执行 → 按配置顺序聚合。
func (r *Runner) Dispatch(ctx context.Context, ev Event, p Payload, mv MatchValue) Outcome {
	var out Outcome
	anchor, ok := ev.Anchor()
	if !ok {
		out.Warnings = append(out.Warnings, fmt.Sprintf("unknown event %q", ev))
		return out
	}
	if r == nil || r.cfg == nil || r.cfg.DisableAll {
		return out
	}

	type plan struct {
		handler Handler
	}
	var plans []plan
	seenCmd := map[string]struct{}{}
	for _, g := range r.cfg.Events[ev] {
		if !matches(g.Matcher, mv, anchor.MatcherField, g.re) {
			continue
		}
		for _, h := range g.Hooks {
			if h.Enabled != nil && !*h.Enabled {
				continue
			}
			if h.Kind() != "command" {
				continue // 预留类型：validate 已告警
			}
			// 信任闸门（规范 §6）：用户级配置默认可信（写这个文件本身就是用户行为）；
			// 项目级配置（随仓库分发）必须显式信任，否则"打开别人的仓库即执行其命令"。
			// cfg.RequireTrust 可要求全部显式信任（高安全场景）。
			if !r.cfg.trustedByScope(g.scope) {
				if st := r.trust.Check(h.Command); st != TrustTrusted && st != TrustManaged {
					out.Warnings = append(out.Warnings, fmt.Sprintf(
						"hook %q not trusted (%s, scope=%s) — skipped", displayName(h), st, scopeLabel(g.scope)))
					continue
				}
			}
			key := string(ev) + "\x00" + h.Command
			if _, dup := seenCmd[key]; dup {
				continue // 同事件同命令只跑一次
			}
			seenCmd[key] = struct{}{}
			plans = append(plans, plan{handler: h})
		}
	}
	if len(plans) == 0 {
		return out
	}

	results := make([]Outcome, len(plans))
	var wg sync.WaitGroup
	for i := range plans {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = r.runOne(ctx, anchor, ev, p, plans[i].handler)
		}(i)
	}
	wg.Wait()

	// 按配置顺序聚合
	var injectedTotal int
	for i := range plans {
		res := results[i]
		h := plans[i].handler
		out.Runs = append(out.Runs, res.Runs...)
		out.Warnings = append(out.Warnings, res.Warnings...)
		out.SystemMsgs = append(out.SystemMsgs, res.SystemMsgs...)

		if res.Decision > out.Decision {
			out.Decision = res.Decision
			if res.Reason != "" {
				out.Reason = res.Reason
			}
		}
		if len(res.UpdatedInput) > 0 && len(out.UpdatedInput) == 0 {
			out.UpdatedInput = res.UpdatedInput
		}
		if res.ToolOutput != "" && out.ToolOutput == "" {
			out.ToolOutput = res.ToolOutput
		}
		for _, c := range res.Contexts {
			if strings.TrimSpace(c) == "" {
				continue
			}
			limit := h.AdditionalContextLimit
			if limit <= 0 {
				limit = defaultHandlerLimit
			}
			c, truncated := truncateBytes(c, limit)
			if truncated {
				out.Warnings = append(out.Warnings,
					fmt.Sprintf("hook %q additionalContext truncated at %dB", displayName(h), limit))
			}
			if injectedTotal+len(c) > maxEventInjectBytes {
				out.Warnings = append(out.Warnings, "event additionalContext budget exhausted — remaining dropped")
				break
			}
			injectedTotal += len(c)
			out.Contexts = append(out.Contexts, markContext(displayName(h), c))
		}
	}
	return out
}

// runOne 执行单个 handler。
// out 用命名返回值：defer 里要把本次 audit 记录回填进 out.Runs，命名返回才能让改动对调用方可见。
func (r *Runner) runOne(ctx context.Context, a Anchor, ev Event, p Payload, h Handler) (out Outcome) {
	timeout, _ := h.TimeoutDuration(a)

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdin, _ := json.Marshal(p)
	cmd := commandFor(cctx, h.Command) // 关键：必须用 CommandContext，否则超时只影响 cctx.Err()、杀不掉子进程
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Env = append(os.Environ(),
		"GOCODE_PROJECT_DIR="+p.Cwd,
		"CLAUDE_PROJECT_DIR="+p.Cwd, // 兼容：按 Claude 写的 shim（如 Graft）无需改动
		"GOCODE_SESSION_ID="+p.SessionID,
		"GOCODE_EVENT="+string(ev),
	)

	start := time.Now()
	err := cmd.Run()
	dur := time.Since(start).Milliseconds()
	exitCode, timedOut := 0, false
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
		if cctx.Err() == context.DeadlineExceeded {
			timedOut = true
			exitCode = -1
		}
	}

	run := HookRun{
		Event: ev, Name: h.Name, Command: h.Command, ExitCode: exitCode,
		TimedOut: timedOut, DurationMs: dur, FailureMode: h.FailureMode, Scope: "runtime",
	}
	defer func() {
		out.Runs = append(out.Runs, run)
		r.audit(run)
	}()

	// 权限类事件（PreToolUse/UserPromptSubmit）：非法输出 = 阻断；观测类：fail-open（规范 §4.4）
	strict := ev == EventPreToolUse || ev == EventUserPromptSubmit

	res, parseWarn := parseResult(stdout.Bytes())
	if parseWarn != "" {
		if strict {
			run.Status = "invalid_output_blocked"
			out.Decision = DecisionBlock
			out.Reason = "hook produced invalid output: " + parseWarn
			return out
		}
		run.Status = "invalid_output_ignored"
		out.Warnings = append(out.Warnings, parseWarn)
	}

	switch {
	case timedOut:
		run.Status = "timeout"
		if h.FailureClosed() {
			out.Decision = DecisionBlock
			out.Reason = fmt.Sprintf("hook %q timed out after %s", displayName(h), timeout)
		} else {
			out.Warnings = append(out.Warnings,
				fmt.Sprintf("hook %q timed out after %s — ignored", displayName(h), timeout))
		}
		return out
	case exitCode == 2:
		run.Status = "blocked"
		out.Decision = DecisionBlock
		out.Reason = strings.TrimSpace(stderr.String())
		if out.Reason == "" {
			out.Reason = fmt.Sprintf("hook %q blocked (exit 2)", displayName(h))
		}
		return out
	case exitCode != 0:
		run.Status = "error"
		msg := fmt.Sprintf("hook %q exited %d: %s", displayName(h), exitCode, firstLine(stderr.String()))
		if h.FailureClosed() {
			out.Decision = DecisionBlock
			out.Reason = msg
		} else {
			out.Warnings = append(out.Warnings, msg)
		}
		return out
	}

	if res == nil {
		run.Status = "ok_no_output"
		return out
	}
	if res.SystemMessage != "" {
		out.SystemMsgs = append(out.SystemMsgs, res.SystemMessage)
	}
	if res.Decision == "block" || res.Decision == "deny" {
		run.Status = "blocked_json"
		out.Decision = DecisionBlock
		out.Reason = res.Reason
		return out
	}
	if so := res.HookSpecificOutput; so != nil {
		if so.HookEventName != "" && so.HookEventName != string(ev) {
			run.Status = "mismatched_event"
			out.Warnings = append(out.Warnings,
				fmt.Sprintf("hook %q hookEventName=%q mismatches event %q — fields ignored", displayName(h), so.HookEventName, ev))
			return out
		}
		if so.AdditionalContext != "" {
			out.Contexts = append(out.Contexts, so.AdditionalContext)
			run.InjectedBytes = len(so.AdditionalContext)
		}
		switch strings.ToLower(so.PermissionDecision) {
		case "deny":
			out.Decision = DecisionBlock
			out.Reason = firstNonEmpty(so.PermissionDecisionReason, res.Reason, "denied by hook")
		case "ask":
			if out.Decision < DecisionAsk {
				out.Decision = DecisionAsk
				out.Reason = so.PermissionDecisionReason
			}
		case "allow":
			if out.Decision < DecisionAllow {
				out.Decision = DecisionAllow
			}
		}
		if len(so.UpdatedInput) > 0 {
			out.UpdatedInput = so.UpdatedInput
		}
	}
	run.Status = "ok"
	return out
}

// parseResult 解析 stdout：必须是单个 JSON 对象；否则返回告警字符串（调用方按事件分档处理）。
func parseResult(b []byte) (*Result, string) {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 {
		return nil, ""
	}
	if trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return nil, "stdout is not a JSON object"
	}
	var res Result
	if err := json.Unmarshal(trimmed, &res); err != nil {
		return nil, "stdout JSON parse error: " + err.Error()
	}
	return &res, ""
}

// DedupInject 会话级注入去重（Graft 的 novelty gate 同理）；返回 true = 此前未注入过。
func (r *Runner) DedupInject(ev Event, ctx string) bool {
	sum := sha256.Sum256([]byte(ctx))
	key := string(ev) + ":" + hex.EncodeToString(sum[:8])
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.seen[key]; ok {
		return false
	}
	r.seen[key] = struct{}{}
	return true
}

func markContext(name, body string) string { return "[hook:" + name + "]\n" + body }

// scopeLabel 告警里展示的来源层级（空 = 代码内构造的配置）。
func scopeLabel(scope string) string {
	if scope == "" {
		return "inline"
	}
	return scope
}

func displayName(h Handler) string {
	if strings.TrimSpace(h.Name) != "" {
		return h.Name
	}
	return firstLine(h.Command)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
