package agenthub

import "testing"

// P3a-09：流式草稿在 AgentStart 预分配 message_id，final 落账必须复用同一个 ID；
// 未预分配的消息仍走确定性的 msg_<seq>。
func TestAppendMessagePreservesStreamID(t *testing.T) {
	ctx := t.Context()
	fs, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stores := map[string]Store{"file": fs, "memory": NewMemoryStore()}
	for name, st := range stores {
		t.Run(name, func(t *testing.T) {
			g := testGroup()
			if err := st.SaveGroup(ctx, g); err != nil {
				t.Fatal(err)
			}
			stream := &Message{
				GroupID: g.ID, Kind: KindText, ID: "msg_Stream1",
				Sender: Sender{Kind: SenderAgent, AgentID: "ag_test"}, Text: "流式 final",
			}
			seq, err := st.AppendMessage(ctx, stream)
			if err != nil {
				t.Fatal(err)
			}
			if stream.ID != "msg_Stream1" {
				t.Fatalf("stream ID = %q, want preserved", stream.ID)
			}
			if seq != 1 {
				t.Fatalf("seq = %d", seq)
			}
			plain := &Message{
				GroupID: g.ID, Kind: KindText,
				Sender: Sender{Kind: SenderHuman}, Text: "普通消息",
			}
			if _, err := st.AppendMessage(ctx, plain); err != nil {
				t.Fatal(err)
			}
			if plain.ID != MessageID(2) {
				t.Fatalf("plain ID = %q, want %q", plain.ID, MessageID(2))
			}
			// 非 msg_ 前缀的调用方 ID 不接受（避免日志里出现任意字符串）。
			bad := &Message{GroupID: g.ID, Kind: KindText, ID: "rnd_not_a_message", Text: "bad"}
			if _, err := st.AppendMessage(ctx, bad); err != nil {
				t.Fatal(err)
			}
			if bad.ID != MessageID(3) {
				t.Fatalf("bad ID = %q, want fallback %q", bad.ID, MessageID(3))
			}
		})
	}
}
