package main

// checkpoint 持久化相关的设置（settings.json 的 "checkpoint" 段）。
//
// 目前只有一项：sidecar 回收是否放后台。默认开 —— 同步回收会在提交路径上
// 重写整份 sidecar（真实会话 3.45GB 约 7s），是 30s 超时的最大单次尖峰之一。

import (
	"encoding/json"
	"os"
)

type checkpointSettings struct {
	// BackgroundCompact 为 nil 表示用默认（开）。
	BackgroundCompact *bool `json:"background_compact"`
	// KeepHistory 为 true 时**不裁剪**「压缩窗口之外」的会话历史行（默认 false = 裁）。
	//
	// 裁剪语义（2026-11-11，CHECKPOINT_V3_DESIGN §9「P1-1」）：会话 jsonl 只保留
	// 「最后一个压缩点及其之后」+ 首行（标题来源）。恢复只用最后一条 state 行，
	// 完整消息在 events.jsonl（唯一事实来源）里 → history 行是冗余副本
	// （实测 86.3MB / 21,154 行 / 会话）。
	KeepHistory *bool `json:"keep_history"`
	// SweepJournals 为 false 时**不做**启动时的会话文件维护清扫。
	// nil = 默认（做）。
	//
	// 清扫 = 对所有超过阈值的会话文件跑一次既有的原子压缩（子智能体 journal
	// 13.4GB / 主会话 jsonl 6.8GB → 约 14GB），后台限速执行。
	SweepJournals *bool `json:"sweep_journals"`
	// MemLimitMB 是 bridge 的**堆软上限**（MB）。
	//
	// 为什么需要（2026-11-12 实测）：启动清扫那 5 分钟的 IO 风暴把 Go 堆
	// 冲到 3 GB，其中 2.8 GB 被系统换出到 swap —— 高水位再也回不来
	// （Go 默认 GOGC=100 ⇒ 堆目标是活堆的 2 倍，没人逼它还）。
	// 设软上限 ⇒ GC 提前介入；清扫结束后再主动 FreeOSMemory 把内存还给 OS。
	//
	// nil = 默认（2048 MB）；0 或负数 = **不设上限**（回到 Go 默认行为）。
	// GOMEMLIMIT 环境变量存在时**优先于本项**（运行时的显式设置不被覆盖）。
	MemLimitMB *int `json:"mem_limit_mb"`
}

func (c checkpointSettings) backgroundCompact() bool {
	return c.BackgroundCompact == nil || *c.BackgroundCompact
}

// keepHistory 默认 false（裁剪）。置 true 可完全回退到旧行为。
func (c checkpointSettings) keepHistory() bool {
	return c.KeepHistory != nil && *c.KeepHistory
}

// sweepJournals 默认 true（做）。置 false 可关掉启动清扫。
func (c checkpointSettings) sweepJournals() bool {
	return c.SweepJournals == nil || *c.SweepJournals
}

// memLimitMB 返回配置的堆软上限（MB）与「是否显式配置」。
//
// 未配置 → (0, false) → 调用方用默认值；显式配置 0 或负数 → 不设上限。
func (c checkpointSettings) memLimitMB() (int64, bool) {
	if c.MemLimitMB == nil {
		return 0, false
	}
	return int64(*c.MemLimitMB), true
}

func loadCheckpointSettings() checkpointSettings {
	var out checkpointSettings
	raw, err := os.ReadFile(homeCfgPath("settings.json"))
	if err != nil {
		return out
	}
	var root struct {
		Checkpoint *checkpointSettings `json:"checkpoint"`
	}
	if json.Unmarshal(raw, &root) == nil && root.Checkpoint != nil {
		out = *root.Checkpoint
	}
	return out
}
