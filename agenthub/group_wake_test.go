package agenthub

import (
	"strings"
	"testing"
)

func TestGroupWakeHeaderRoundTrip(t *testing.T) {
	h := &GroupWakeHeader{
		GroupID: "grp_abc123", AgentID: "ag_abc123",
		RoundID: "rnd_01J", TriggerSeq: 42,
	}
	encoded, err := EncodeGroupWake(h, "群：复盘群\n#42 人: @B 看一下")
	if err != nil {
		t.Fatal(err)
	}
	got, snapshot, err := ParseGroupWake(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got.GroupID != h.GroupID || got.AgentID != h.AgentID ||
		got.RoundID != h.RoundID || got.TriggerSeq != h.TriggerSeq {
		t.Fatalf("header = %+v, want %+v", got, h)
	}
	if !strings.Contains(snapshot, "#42 人: @B 看一下") {
		t.Fatalf("snapshot = %q", snapshot)
	}
}

// TestGroupWakeHeaderOldBlockCompat 锁住兼容：P3b-04 之前落在 transcript 里的
// hub_group_wake 块没有 depth/turn，必须继续能解析（按 0 处理），不能当成坏块。
func TestGroupWakeHeaderOldBlockCompat(t *testing.T) {
	old := `{"group_id":"grp_abc123","agent_id":"ag_abc123","round_id":"rnd_01J","trigger_seq":7}` +
		"\n\n群：复盘群\n#7 人: 开始"
	h, snap, err := ParseGroupWake(old)
	if err != nil {
		t.Fatalf("旧块必须可解析: %v", err)
	}
	if h.Depth != 0 || h.Turn != 0 {
		t.Fatalf("旧块 depth/turn 应为 0，得到 %+v", h)
	}
	if !strings.Contains(snap, "#7 人: 开始") {
		t.Fatalf("snapshot = %q", snap)
	}
	// 新块带 depth/turn：编码后能读回，且 0 值不会污染旧格式（omitempty）。
	withDepth := &GroupWakeHeader{
		GroupID: "grp_abc123", AgentID: "ag_abc123", RoundID: "rnd_01J", TriggerSeq: 7,
		Depth: 2, Turn: 3,
	}
	encoded, err := EncodeGroupWake(withDepth, "snap")
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := ParseGroupWake(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got.Depth != 2 || got.Turn != 3 {
		t.Fatalf("depth/turn 往返失败: %+v", got)
	}
	zero, err := EncodeGroupWake(&GroupWakeHeader{
		GroupID: "grp_abc123", AgentID: "ag_abc123", RoundID: "rnd_01J", TriggerSeq: 7,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	firstLine, _, _ := strings.Cut(zero, "\n")
	if strings.Contains(firstLine, "depth") || strings.Contains(firstLine, "turn") {
		t.Fatalf("0 值不该出现在 header JSON 里（omitempty）：%s", firstLine)
	}
}

func TestGroupWakeHeaderRejectsBadFields(t *testing.T) {
	good := func() *GroupWakeHeader {
		return &GroupWakeHeader{GroupID: "grp_abc123", AgentID: "ag_abc123", RoundID: "rnd_01J", TriggerSeq: 1}
	}
	cases := []struct {
		name string
		mu   func(*GroupWakeHeader)
	}{
		{"缺群", func(h *GroupWakeHeader) { h.GroupID = "" }},
		{"群前缀错", func(h *GroupWakeHeader) { h.GroupID = "ag_abc123" }},
		{"缺智能体", func(h *GroupWakeHeader) { h.AgentID = "" }},
		{"缺轮次", func(h *GroupWakeHeader) { h.RoundID = "" }},
		{"触发 seq 非正", func(h *GroupWakeHeader) { h.TriggerSeq = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := good()
			tc.mu(h)
			if _, err := EncodeGroupWake(h, ""); err == nil {
				t.Fatal("EncodeGroupWake must reject bad header")
			}
		})
	}
	if _, _, err := ParseGroupWake("not-json\nsnapshot"); err == nil {
		t.Fatal("ParseGroupWake must reject bad JSON")
	}
	if _, _, err := ParseGroupWake(`{"group_id":"bad","agent_id":"ag_x","round_id":"r","trigger_seq":1}` + "\n\nsnap"); err == nil {
		t.Fatal("ParseGroupWake must reject missing group prefix")
	}
}
