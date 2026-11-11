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
}

func (c checkpointSettings) backgroundCompact() bool {
	return c.BackgroundCompact == nil || *c.BackgroundCompact
}

// keepHistory 默认 false（裁剪）。置 true 可完全回退到旧行为。
func (c checkpointSettings) keepHistory() bool {
	return c.KeepHistory != nil && *c.KeepHistory
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
