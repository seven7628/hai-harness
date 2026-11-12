package main

// 端到端验证：真实 compress_end 事件（含 analysis）→ ViewReducer 重建 → Snapshot JSON 保留 analysis。
// 覆盖「重启后客户端看不到分析过程」的恢复链路。

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/events"
)

// findEventLogWithCompress 在本机事件日志里找**任意一份**含 compress_end 的会话日志。
// 不写死工作区 key / 会话 id（那些是可识别信息，不入库），也让测试在别的机器上能跑。
func findEventLogWithCompress(t *testing.T) []byte {
	t.Helper()
	base := filepath.Join(os.Getenv("HOME"), ".go-code", "events")
	var found []byte
	_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil || !bytes.Contains(b, []byte(`"compress_end"`)) {
			return nil
		}
		found = b
		return filepath.SkipAll
	})
	return found
}

func TestAnalysisSurvivesCheckpointReplay(t *testing.T) {
	data := findEventLogWithCompress(t)
	if len(data) == 0 {
		t.Skip("本机没有含 compress_end 的事件日志（$HOME/.go-code/events/*/*.jsonl）")
	}

	// 提取真实 compress_end（含 Analysis）
	var real *events.CompressEnd
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		var et string
		json.Unmarshal(raw["event_type"], &et)
		if et == "compress_end" {
			var ev events.CompressEnd
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				continue
			}
			if ev.Analysis != "" {
				real = &ev
				break
			}
		}
	}
	if real == nil {
		t.Skip("真实事件无 analysis 样本")
	}

	r := NewViewReducer()
	// 主 agent run 前置（compress_end 主路径需要）
	mustApply(t, r, &events.AgentStart{RunId: real.RunId, Name: "main", Timestamp: time.Now(), EventType: events.AgentStartType})
	mustApply(t, r, &events.CompressStart{Before: real.Before, RunId: real.RunId, EventType: events.CompressStartType})
	mustApply(t, r, real)

	cp := r.Snapshot()
	raw, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	// 反解析快照，从 compression block 取 analysis 比对（避免转义匹配问题）
	var snap struct {
		Blocks []struct {
			Kind     string `json:"kind"`
			Analysis string `json:"analysis,omitempty"`
			Summary  string `json:"summary,omitempty"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	var got string
	for _, b := range snap.Blocks {
		if b.Kind == "compression" {
			got = b.Analysis
		}
	}
	if got != real.Analysis {
		t.Fatalf("checkpoint 快照 compression block analysis 不匹配: got_len=%d want_len=%d", len(got), len(real.Analysis))
	}
	t.Logf("OK: 真实 compress_end analysis(%d 字符) 已保留在 checkpoint 快照", len(got))
}
