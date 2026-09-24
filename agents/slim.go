package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/seven7628/hai-harness/core"
	"os"
	"path/filepath"
)

// 工具结果瘦身（C）：控制上下文增长斜率的三个零 LLM 成本手段——去重替换 /
// 占位替换 / spill 溢出写文件。摘要化（LLM）不做常态手段（留给全量压缩兜底）。
//
// 与引擎截断的关系：引擎 truncate（默认 20KB）先于本层执行——spill 阈值默认
// 64KB 在默认组合下是 no-op；要 spill 生效需产品配置引擎 maxResponse 高于
// SpillMaxBytes（或 0 不截断，bash 大输出场景）。文档见 docs/PLAN.md C 节。
//
// 零成本语义：替换/占位/spill 不产生任何 LLM 调用与事件（压缩有成本故有事件；
// slim 无成本静默执行）。替换 = 修改 Messages 元素（不增删，下标不失效）→
// 下轮请求字节一次断层 → 此后稳定（缓存命中不受持续影响）。

// SlimConfig 工具结果瘦身配置（WithToolResultSlim 启用；默认关——行为变化项
// 产品验证后开）。字段 0 值 = 使用默认。
type SlimConfig struct {
	DedupMinBytes int    // 只读结果去重的参与下限（默认 2048：小结果不值得断层代价）
	StaleRounds   int    // 占位替换的轮龄阈值（默认 20 轮）
	StaleMinBytes int    // 占位替换的字节下限（默认 8192：只占位超龄大结果）
	SpillMaxBytes int    // spill 溢出阈值（默认 65536；> 阈值写文件 + 历史留预览）
	SpillDir      string // spill 文件目录（空 = 不 spill；建议置于文件工具工作区内，模型可 read_file 检索）
}

func (c *SlimConfig) normalize() {
	if c.DedupMinBytes <= 0 {
		c.DedupMinBytes = 2048
	}
	if c.StaleRounds <= 0 {
		c.StaleRounds = 20
	}
	if c.StaleMinBytes <= 0 {
		c.StaleMinBytes = 8192
	}
	if c.SpillMaxBytes <= 0 {
		c.SpillMaxBytes = 65536
	}
}

// WithToolResultSlim 启用工具结果瘦身（默认关）；空参 = 全默认配置。
func WithToolResultSlim(cfg ...SlimConfig) Option {
	return func(c *Config) {
		c.Slim = &SlimConfig{}
		if len(cfg) > 0 {
			*c.Slim = cfg[0]
		}
		c.Slim.normalize()
	}
}

// slimRead 一次 read_file 结果的索引记录。
type slimRead struct {
	index int    // 结果消息在 ac.Messages 中的下标
	hash  string // 结果文本 sha256
}

// slimState 每次 Run 的工具结果瘦身状态（AgentContext.slim；AgentLoop 不可变
// 约束 → 状态必须 per-Run）。下标只增不减（替换不增删元素），压缩整体替换
// Messages 后必须 reset（否则按失效下标改写错误消息，可能改写摘要/system）。
type slimState struct {
	cfg        SlimConfig
	readIndex  map[string]slimRead // path → 最近一次 read 结果（去重匹配锚）
	roundStart []int               // 每轮循环顶部的消息数（轮龄判定：距尾 N 块）
}

func newSlimState(cfg SlimConfig) *slimState {
	return &slimState{
		cfg:       cfg,
		readIndex: make(map[string]slimRead),
	}
}

// reset 压缩成功后调用：Messages 整体替换，全部下标失效 → 清空重建。
func (s *slimState) reset() {
	s.readIndex = make(map[string]slimRead)
	s.roundStart = nil
}

// spillResult 溢出写文件（入历史前）：> SpillMaxBytes 且配置了目录时，完整输出
// 写 <SpillDir>/spill-<runId>-<callId>.txt，历史留 path + 前 2048 字节预览。
// 写文件失败静默保留原文（引擎截断兜底，spill 是优化非正确性）。
func (a *AgentLoop) spillResult(ac *AgentContext, tc core.ToolCall, text string) string {
	s := ac.slim
	if len(text) <= s.cfg.SpillMaxBytes || s.cfg.SpillDir == "" {
		return text
	}
	if err := os.MkdirAll(s.cfg.SpillDir, 0o755); err != nil {
		return text
	}
	rel := fmt.Sprintf("spill-%s-%s.txt", ac.RunId, tc.Id)
	if err := os.WriteFile(filepath.Join(s.cfg.SpillDir, rel), []byte(text), 0o644); err != nil {
		return text
	}
	preview := text
	if len(preview) > 2048 {
		preview = preview[:2048]
	}
	return fmt.Sprintf("[完整输出已写 %s，以下为前 2048 字节预览]\n%s", rel, preview)
}

// recordRead 记录 read_file 结果并做去重替换（回写循环内、append 前调用；
// newIdx = 本次结果消息将占据的下标）。同 path 同 hash 且达参与下限时，
// 旧结果消息替换为短引用（指向本次新消息）——新结果正常入历史（保持
// 「最近一次读取」可检索）；LLM 看到引用可回看 #N。
func (a *AgentLoop) recordRead(ac *AgentContext, tc core.ToolCall, text string, newIdx int) {
	if tc.Name != "read_file" {
		return
	}
	path := readPathFromArgs(tc.Arguments)
	if path == "" {
		return
	}
	sum := sha256.Sum256([]byte(text))
	h := hex.EncodeToString(sum[:])
	if prev, ok := ac.slim.readIndex[path]; ok && prev.hash == h && len(text) >= ac.slim.cfg.DedupMinBytes {
		// 同 path 同内容：旧结果替换短引用（历史 ToolCallId 保留，协议配对不变）
		ac.Messages[prev.index] = core.NewToolMessage(
			ac.Messages[prev.index].ToolCallId,
			fmt.Sprintf("[内容与消息 #%d 相同，已去重]", newIdx),
		)
	}
	ac.slim.readIndex[path] = slimRead{index: newIdx, hash: h}
}

// applyStale 结算段（每轮循环顶部、maybeCompact 前）占位替换：记录本轮起始
// 下标（roundStart），轮龄 > StaleRounds 且 > StaleMinBytes 的旧工具结果 →
// 占位符（microcompact 式——超龄大结果价值衰减，保守阈值代替精确缓存账）。
// 在压缩前执行（减少压缩输入体积）。
func (a *AgentLoop) applyStale(ac *AgentContext) {
	s := ac.slim
	s.roundStart = append(s.roundStart, len(ac.Messages))
	if len(s.roundStart) <= s.cfg.StaleRounds {
		return // 轮龄未达阈值
	}
	cutoff := s.roundStart[len(s.roundStart)-1-s.cfg.StaleRounds]
	placeholder := fmt.Sprintf("[旧工具结果已占位（超过 %d 轮），可要求重新执行工具获取最新内容]", s.cfg.StaleRounds)
	for i := 0; i < cutoff; i++ {
		m := &ac.Messages[i]
		if m.Role != core.Tool || len(m.Content) == 0 {
			continue
		}
		if len(m.Content[0].Content) > s.cfg.StaleMinBytes {
			m.Content[0].Content = placeholder
		}
	}
}

// readPathFromArgs 提取 read_file 参数中的 path（JSON）。
func readPathFromArgs(arguments string) string {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil || args.Path == "" {
		return ""
	}
	return args.Path
}
