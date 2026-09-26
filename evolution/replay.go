package evolution

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/seven7628/hai-harness/memory"
)

// ErrNoStructuralHooks 结构 Replayer 缺少渲染/解析钩子（内部接线错误）。
var ErrNoStructuralHooks = errors.New("evolution: structural replayer requires render/parse hooks")

// Replay 判定口径。
const (
	// ReplayPass 单次 Replay 通过。
	ReplayPass = "pass"
	// ReplayFail 单次 Replay 判定不通过。
	ReplayFail = "fail"
	// ReplayInconclusive Replay 没能得出结论（执行错误、超时、注入方拒答）。
	ReplayInconclusive = "inconclusive"
)

// ReplayRequest 一次 Replay 的输入。
//
// 只有"要检查的东西"和"对照基线"，**不**夹带任何执行权限、路径或凭证：
// Replayer 是宿主注入的实现，签名里没有可以让它顺手改仓库的句柄。
type ReplayRequest struct {
	// Candidate 待验证候选。
	Candidate *Candidate
	// Baseline 同一主题名的既有内容（无则 nil）。用于"这条记忆是否与现有事实冲突"。
	Baseline *TopicSnapshot
	// RepoRoot 仓库根目录（只读定位，宿主可用可不用）。
	RepoRoot string
	// Budget 单次 Replay 的时间预算（宿主可据此截断；<=0 表示不限制）。
	Budget time.Duration
}

// TopicSnapshot 既有主题的只读快照（Replay 对照用）。
type TopicSnapshot struct {
	Name    string
	Title   string
	Body    string
	Updated time.Time
}

// ReplayResult 一次 Replay 的结果（被写进候选的验证报告）。
type ReplayResult struct {
	// Replayer 执行者名（结构 Replayer 固定为 "structural"）。
	Replayer string `json:"replayer"`
	// Verdict 判定（ReplayPass / ReplayFail / ReplayInconclusive）。
	Verdict string `json:"verdict"`
	// Reason 人可读理由。
	Reason string `json:"reason,omitempty"`
	// At 执行时间。
	At time.Time `json:"at"`
	// DurationMs 耗时（毫秒）。真实 provider 的耗时是这个包唯一能提供的成本读数。
	DurationMs int64 `json:"duration_ms"`
	// UsedRealProvider 为真表示本次确实调用了外部 LLM。
	//
	// 这个字段的作用是让"默认路径没有真实调用"这件事**可审计**而不是口头保证：
	// 报告被存档后，任何人读它都能知道这次 Replay 到底花没花模型调用。
	UsedRealProvider bool `json:"used_real_provider"`
}

// Replayer 单次 Replay 执行器（可注入）。
//
// Phase B 硬性要求：**默认实现不执行任何真实 LLM 调用**。原因是可测性——门禁链
// 必须能在没有 provider、没有网络、没有 budget 的环境里给出确定结论；真实
// provider 的成本、抖动、不可复现性会污染"验证是否有效"这个问题本身。
//
// 真实接入路径：宿主实现本接口并在 Config.Replayer 注入。此时才产生模型调用。
type Replayer interface {
	// Name 执行者名（写进报告，用于区分"谁判的"）。
	Name() string
	// Replay 单次执行。返回 error 视为 inconclusive（不判失败）——
	//	执行基础设施坏了不该被算成"候选内容有问题"。
	Replay(ctx context.Context, req ReplayRequest) (*ReplayResult, error)
}

// ReplayerFunc 把普通函数适配成 Replayer（宿主与测试的最小接缝）。
type ReplayerFunc struct {
	ReplayerName string
	Fn           func(ctx context.Context, req ReplayRequest) (*ReplayResult, error)
}

func (f ReplayerFunc) Name() string { return f.ReplayerName }

func (f ReplayerFunc) Replay(ctx context.Context, req ReplayRequest) (*ReplayResult, error) {
	if f.Fn == nil {
		return nil, errors.New("evolution: replayer has nil function")
	}
	return f.Fn(ctx, req)
}

// StructuralReplayerName 默认确定性 Replayer 名。
const StructuralReplayerName = "structural"

// structuralReplayer 默认 Replayer：纯确定性的结构回放，零外部调用。
//
// 它检查什么：候选内容经过 Phase A 的渲染/解析往返之后，**关键信息是否还在**。
// 这类失败是真实存在的（frontmatter 字段名写错、正文被 frontmatter 吞掉、
// 换行结构让标题落进正文），而它们恰好是最容易被模型写出来、又在人工阅读时
// 最不容易发现的错误——文件看起来"有内容"，但 memory_view 拿到的东西已经变了。
//
// 它明确**不**检查什么：事实是否正确（需要真实验证，那是宿主 Replayer 的事）。
// 所以它的 pass 只意味着"这条记忆能被如实写进文件并被如实读回来"。
type structuralReplayer struct {
	hooks StructuralHooks
}

// DefaultReplayer 返回默认 Replayer：确定性结构回放，**不调用任何 LLM**。
func DefaultReplayer(hooks StructuralHooks) Replayer {
	return structuralReplayer{hooks: hooks}
}

// StructuralHooks 结构 Replay 需要的渲染/解析钩子。
//
// 做成显式依赖而不是直接调用 Phase A 的未导出函数：既保持 evolution 不改 memory/
// 内部（Phase A 契约要求"不重构它"），也允许宿主在测试里替换成自己的实现。
// Service 构造时会用 memory 包的公开入口填上默认实现。
type StructuralHooks struct {
	// Render 把主题渲染成将要落盘的 Markdown。
	Render func(t *memory.Topic) string
	// Parse 把 Markdown 解析回主题（往返一致性检查）。
	Parse func(raw string) *memory.Topic
}

func (r structuralReplayer) Name() string { return StructuralReplayerName }

func (r structuralReplayer) Replay(ctx context.Context, req ReplayRequest) (*ReplayResult, error) {
	start := time.Now()
	res := &ReplayResult{Replayer: r.Name(), At: start.UTC().Truncate(time.Second)}
	if r.hooks.Render == nil || r.hooks.Parse == nil {
		return nil, ErrNoStructuralHooks
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Candidate == nil {
		res.Verdict = ReplayInconclusive
		res.Reason = "no candidate"
		return res, nil
	}
	res.DurationMs = time.Since(start).Milliseconds()

	if req.Candidate.Kind == KindSkill {
		res.Verdict = ReplayPass
		res.Reason = "skill candidate replayed structurally (no model call)"
		return res, nil
	}

	// 往返：渲染 → 解析 → 比对正文。任何一步不一致都说明这条记忆"写进文件后
	// 就不再是原来写的样子了"，属于必须在进索引前拦掉的错误。
	raw := r.hooks.Render(&req.Candidate.Topic)
	back := r.hooks.Parse(raw)
	if back == nil {
		res.Verdict = ReplayFail
		res.Reason = "rendered topic could not be parsed back"
		return res, nil
	}
	orig := &req.Candidate.Topic
	if orig.Body != "" && back.Body != orig.Body {
		res.Verdict = ReplayFail
		res.Reason = fmt.Sprintf("round-trip body mismatch (rendered %d bytes, parsed %d bytes)", len(orig.Body), len(back.Body))
		return res, nil
	}
	if orig.Title != "" && back.Title != orig.Title {
		res.Verdict = ReplayFail
		res.Reason = fmt.Sprintf("round-trip title mismatch (%q -> %q)", orig.Title, back.Title)
		return res, nil
	}
	if req.Baseline != nil && req.Baseline.Body != "" && orig.Body == req.Baseline.Body {
		res.Verdict = ReplayInconclusive
		res.Reason = "candidate is byte-identical to the existing topic; replay has nothing to exercise"
		return res, nil
	}
	res.Verdict = ReplayPass
	res.Reason = "topic round-trips through the Phase A format unchanged"
	return res, nil
}
