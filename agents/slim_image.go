package agents

import (
	"fmt"
	"strings"

	"github.com/seven7628/hai-harness/core"
)

// 图片瘦身（F 段）：上下文里**图片**才是真正的大头。
//
// 实测（tools/bench/export_real_sessions.py 导出的 6 个真实会话）：
// base64 截图占上下文 **93%**（单张 0.2~2MB，一个会话累积 18~56 张），而
// C/D/E 三段按设计只处理文本，全会话口径的降幅因此只有 -0.2%。
//
// 为什么图片必须单独治理：视觉模型按**图像分块**计费（单图约 1600 visual
// tokens），但传输/存储成本按 base64 字符算（一张 1MB PNG ≈ 1.4MB base64）。
// 两头都贵。而更关键的是**生命周期**：文本结果会因压缩/折叠/占位被回收，
// 图片却一直留在 ac.Messages 里直到整段会话被压缩掉 —— 一张 30 轮前的截图
// 几乎不可能再被引用，却仍在每轮请求里全额计费。
//
// 策略（与 C 段的 applyStale 同源，作用于图片）：
//  1. **保最近 N 张**（默认 4，与 tools 层单次结果上限一致）：视觉上模型只
//     会在意「刚看到的画面」，更早的截图对它已无意义。
//  2. **超龄淘汰**（默认 30 轮，与 StaleRounds 对齐）：即使总数没超，轮龄到
//     头也替换 —— 一张放了 50 轮的截图不如一条「重新截图」的提示有用。
//  3. 替换为**可寻址的文本占位**：写明「第 N 张截图已移出上下文（工具/参数/轮
//     龄）+ 如何重新获取」。模型知道图存在过、也知道怎么再拿到，而不是静默
//     消失（静默会让模型以为自己看过了，实际它已经看不到了 —— 那是最坏的
//     失败模式：模型基于「我看到过」的错误记忆继续推理）。
//
// 不做的事（明确的边界）：
//   - 不做图片重编码/降采样。tools/builtin/image.go 的 loadImageForModel 已经
//     在**单次内联**时按 maxImageBytes 降采样压缩过（3MiB 上限）；F 段管的是
//     「已经进了上下文之后」的跨轮累积，再压画质只会损伤视觉判断。
//   - 不动 user 消息里的图片（用户自己发的截图，是需求本身）。
//   - 不动最近 N 张（模型正在看它们）。
//
// 与文本占位的关系：两者共用 slimPlaceholderPrefix，E 段（工具对合并）会把
// 已被 F 段占位的图片条目整条删掉 —— 文本占位与图片占位在同一条消息里时，
// 该消息的其余信息量也已由代表条目承载。

// imageSlimNote 图片占位前缀（同样以 slim: 开头，便于 E 段把已占位的图片
// 条目整条删除 —— 那条消息的其余信息量也已由代表条目承载）。
const imageSlimNote = slimPlaceholderPrefix + "image "

// 默认阈值。
const (
	defaultImageKeep      = 4
	defaultImageMaxRounds = 30
)

// ImageSlimConfig 图片瘦身（F 段）配置。Keep < 0 = 关闭；0 = 用默认。
type ImageSlimConfig struct {
	Keep      int // 保留最近 N 张图片（默认 4）
	MaxRounds int // 轮龄上限，超龄即替换（默认 30；<0 = 不按轮龄淘汰）
}

// WithImageSlim 启用图片瘦身（F 段）。空参 = 默认（保最近 4 张 + 30 轮淘汰）。
//
// 单独提供构造函数而非塞进 SlimTextOptions：图片治理的收益量级（实测占上下文
// 93%）与风险（动视觉内容）都显著高于文本瘦身，值得宿主显式决策。
func WithImageSlim(cfg ...ImageSlimConfig) Option {
	return func(c *Config) {
		if c.Slim == nil {
			c.Slim = &SlimConfig{}
		}
		c.Slim.normalize()
		c.Slim.Images = &ImageSlimConfig{}
		if len(cfg) > 0 {
			*c.Slim.Images = cfg[0]
		}
	}
}

// imageInfo 一次图片结果的记账。
type imageInfo struct {
	toolIdx int    // tool 消息在 ac.Messages 的下标
	tool    string // 工具名（占位文案用）
	age     int    // 入场时的轮龄计数（递增用）
}

// noteForImage 渲染图片占位文案。
func noteForImage(tool string, idx, keep, age int) string {
	return fmt.Sprintf("%solder screenshot removed from context (message #%d, %s, %d rounds old; kept the most recent %d). Re-capture it if you need to see it again — do NOT assume you still remember its contents.]",
		imageSlimNote, idx, tool, age, keep)
}

// applyImageSlim 图片瘦身（结算段，压缩前执行）：保最近 keep 张 + 超龄淘汰。
// 替换不增删消息（下标不失效），与 C/D 段一致。
func (a *AgentLoop) applyImageSlim(ac *AgentContext) {
	s := ac.slim
	if s == nil || s.cfg.Images == nil {
		return
	}
	keep := s.cfg.Images.Keep
	if keep < 0 {
		return // 负数 = 关闭
	}
	if keep == 0 {
		keep = defaultImageKeep
	}
	maxRounds := s.cfg.Images.MaxRounds
	if maxRounds == 0 {
		maxRounds = defaultImageMaxRounds
	}

	// 轮龄：该图片之后还剩多少条 tool 结果。cutoff 之前的图片轮龄已超上限。
	age := map[int]int{}
	if maxRounds > 0 {
		cutoff := 0
		if n := len(s.roundStart); n > 0 {
			if k := n - 1 - maxRounds; k >= 0 && k < len(s.roundStart) {
				cutoff = s.roundStart[k]
			}
		}
		var order []int
		for i := cutoff; i < len(ac.Messages); i++ {
			if ac.Messages[i].Role == core.Tool {
				order = append(order, i)
			}
		}
		for n, i := range order {
			age[i] = len(order) - n - 1
		}
		for i := 0; i < cutoff; i++ {
			if ac.Messages[i].Role == core.Tool {
				age[i] = maxRounds // cutoff 之前：必然超龄
			}
		}
	}

	// 找出所有带图消息，从后往前数：最近 keep 张无条件保留
	var imgIdx []int
	for i, m := range ac.Messages {
		if m.Role != core.Tool || !hasImage(m) {
			continue
		}
		imgIdx = append(imgIdx, i)
	}
	if len(imgIdx) == 0 {
		return
	}
	protectedFrom := len(imgIdx) - keep // imgIdx[protectedFrom:] 是受保护区间
	if protectedFrom < 0 {
		protectedFrom = 0
	}
	removed := 0
	for n, i := range imgIdx {
		if n >= protectedFrom {
			continue // 最近 keep 张：模型正在看，永不替换
		}
		// 不在保护区时，还需再过轮龄闸：轮龄未到的也留（避免刚截的图被立刻抹掉）
		if maxRounds > 0 && age[i] < maxRounds {
			continue
		}
		m := ac.Messages[i]
		m.Content = stripImages(m.Content, noteForImage(imageToolName(ac, m), i, keep, age[i]))
		ac.Messages[i] = m
		removed++
	}
	if removed > 0 {
		s.imagesRemoved += removed
	}
}

// hasImage 消息是否携带图片块。
func hasImage(m core.Message) bool {
	for _, c := range m.Content {
		if c.Type == core.ContentTypeImage && c.Content != "" {
			return true
		}
	}
	return false
}

// stripImages 去掉图片块，改为在文本末尾追加说明（模型知道图存在过且如何重取）。
func stripImages(content []core.Content, note string) []core.Content {
	out := make([]core.Content, 0, len(content)+1)
	hadText := false
	for _, c := range content {
		if c.Type == core.ContentTypeImage {
			continue
		}
		out = append(out, c)
		if strings.TrimSpace(c.Content) != "" {
			hadText = true
		}
	}
	if hadText {
		out[len(out)-1].Content += "\n" + note
	} else {
		out = append(out, core.Content{Type: core.ContentTypeText, Content: note})
	}
	return out
}

// imageToolName 取图片所属工具名（占位文案用；找不到时给通用措辞）。
func imageToolName(ac *AgentContext, m core.Message) string {
	for i := len(ac.Messages) - 1; i >= 0; i-- {
		if ac.Messages[i].Role == core.Assistant {
			for _, tc := range ac.Messages[i].ToolCalls {
				if tc.Id == m.ToolCallId {
					return tc.Name
				}
			}
		}
	}
	return "tool"
}
