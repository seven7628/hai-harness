package agenthub

import (
	"context"
	"testing"
)

// P3b-01a 复审 N1：agent_context_seq 必须字段级推进，不能整行 SaveCursor
// 覆盖掉 human_read_seq（两个独立写者）。
func TestAdvanceAgentContextSeqPreservesHumanRead(t *testing.T) {
	fs, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stores := map[string]Store{"file": fs, "memory": NewMemoryStore()}
	for name, st := range stores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := st.SaveCursor(ctx, &Cursor{
				GroupID: "grp_cursor", AgentID: "ag_cursor",
				HumanReadSeq: 5, AgentContextSeq: 1,
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.AdvanceAgentContextSeq(ctx, "grp_cursor", "ag_cursor", 3); err != nil {
				t.Fatal(err)
			}
			cur, err := st.LoadCursor(ctx, "grp_cursor", "ag_cursor")
			if err != nil {
				t.Fatal(err)
			}
			if cur.HumanReadSeq != 5 || cur.AgentContextSeq != 3 {
				t.Fatalf("cursor = %+v, want human=5 agent=3", cur)
			}
			// 只前移：回退调用不能把游标改小。
			if err := st.AdvanceAgentContextSeq(ctx, "grp_cursor", "ag_cursor", 2); err != nil {
				t.Fatal(err)
			}
			cur, _ = st.LoadCursor(ctx, "grp_cursor", "ag_cursor")
			if cur.AgentContextSeq != 3 {
				t.Fatalf("agent_context_seq moved backwards to %d", cur.AgentContextSeq)
			}
		})
	}
}
