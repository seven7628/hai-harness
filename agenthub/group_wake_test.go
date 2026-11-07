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
