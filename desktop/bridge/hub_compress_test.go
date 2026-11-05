package main

// P1-07：每智能体压缩阈值 + loop 重建。
//
// 约定（方案 §6.11）：阈值是 Profile 字段，装配时显式传给 loop；改阈值走
// hub_agent_update → 重建该智能体的 loop（下一个 Run 生效）；**不**做全局广播
// （set_compress_threshold 只影响工作区会话，不能顺手改掉智能体的阈值）。

import (
	"math"
	"testing"
)

func TestHubAgentCompressThresholdRebuild(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Compress")
	agentID := string(p.ID)
	bs, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	if got := bs.loop.CompressThreshold(); math.Abs(got-0.8) > 1e-9 {
		t.Fatalf("default threshold = %v, want 0.8", got)
	}
	before := bs.loop
	if ok, errMsg, _ := hubCmd(t, m, buf, 2, "hub_agent_update", map[string]any{
		"agent_id": agentID, "patch": map[string]any{"compress_threshold": 0.62},
	}); !ok {
		t.Fatalf("update failed: %s", errMsg)
	}
	if bs.loop == before {
		t.Fatal("changing compress_threshold must rebuild the loop (下一个 Run 生效)")
	}
	if got := bs.loop.CompressThreshold(); math.Abs(got-0.62) > 1e-9 {
		t.Fatalf("threshold after update = %v, want 0.62", got)
	}
	if bs.s.Loop() != bs.loop {
		t.Fatal("session still runs the old loop")
	}

	// 越界值被 Normalize 钳到 [0.5, 0.95]，不是原样或报错。
	for _, tc := range []struct{ in, want float64 }{{0.99, 0.95}, {0.2, 0.5}} {
		if ok, errMsg, _ := hubCmd(t, m, buf, 3, "hub_agent_update", map[string]any{
			"agent_id": agentID, "patch": map[string]any{"compress_threshold": tc.in},
		}); !ok {
			t.Fatalf("update %v failed: %s", tc.in, errMsg)
		}
		if got := bs.loop.CompressThreshold(); math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("threshold for input %v = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// 全局 set_compress_threshold 不能改智能体的阈值（它只该影响工作区会话）。
func TestHubCompressThresholdNotGlobal(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Local")
	bs, err := m.hubDMSession(string(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 2, "hub_agent_update", map[string]any{
		"agent_id": string(p.ID), "patch": map[string]any{"compress_threshold": 0.66},
	}); !ok {
		t.Fatalf("update failed: %s", errMsg)
	}
	m.dispatch(command{Id: 3, Type: "set_compress_threshold", Payload: map[string]any{
		"compress_threshold": 0.31,
	}})
	if got := bs.loop.CompressThreshold(); math.Abs(got-0.66) > 1e-9 {
		t.Fatalf("global set_compress_threshold leaked into hub loop: %v, want 0.66", got)
	}
	// 而同一次广播确实会改工作区会话（证明命令生效，不是失败静默）。
	ws := m.runtime(t.TempDir())
	plain, err := m.createIn(ws, "s-global", "", sessionOpts{})
	if err != nil {
		t.Fatal(err)
	}
	plain.loop.SetCompressThreshold(0.8)
	m.setCompressThreshold(0.31)
	if got := plain.loop.CompressThreshold(); math.Abs(got-0.31) > 1e-9 {
		t.Fatalf("project loop threshold = %v, want 0.31 (broadcast broken?)", got)
	}
}
