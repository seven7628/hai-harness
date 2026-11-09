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
}

func (c checkpointSettings) backgroundCompact() bool {
	return c.BackgroundCompact == nil || *c.BackgroundCompact
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
