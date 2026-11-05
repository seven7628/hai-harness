package agenthub

// P1-04：DM 索引与未读游标的存储层语义。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func dmMsg(kind MessageKind, sender SenderKind, text string) *Message {
	return &Message{Kind: kind, Sender: Sender{Kind: sender}, Text: text}
}

// seq 在每个 DM 内单调递增，ID 由 seq 决定；GroupID 恒为空（DM 没有群）。
func TestAppendDirectMessageSeqAndID(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		m := dmMsg(KindText, SenderHuman, "hi")
		seq, err := st.AppendDirectMessage(ctx, "ag_dm", m)
		if err != nil {
			t.Fatal(err)
		}
		if seq != int64(i) || m.Seq != int64(i) || m.ID != MessageID(int64(i)) {
			t.Fatalf("seq/id mismatch at %d: seq=%d id=%q", i, seq, m.ID)
		}
		if m.GroupID != "" {
			t.Fatalf("DM message must not carry group_id, got %q", m.GroupID)
		}
		if m.CreatedAt.IsZero() {
			t.Fatal("created_at not filled")
		}
	}
	// 两个 DM 各自从 1 开始（维度是 agentID）。
	if seq, err := st.AppendDirectMessage(ctx, "ag_other", dmMsg(KindText, SenderHuman, "x")); err != nil || seq != 1 {
		t.Fatalf("other DM seq = %d, %v; want 1", seq, err)
	}
}

// 分页：seq < beforeSeq，旧→新返回。
func TestReadDirectMessagesPaging(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if _, err := st.AppendDirectMessage(ctx, "ag_page", dmMsg(KindText, SenderHuman, "m")); err != nil {
			t.Fatal(err)
		}
	}
	page, err := st.ReadDirectMessages(ctx, "ag_page", 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 3 || page[0].Seq != 8 || page[2].Seq != 10 {
		t.Fatalf("latest page wrong: %+v", seqs(page))
	}
	older, err := st.ReadDirectMessages(ctx, "ag_page", page[0].Seq, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(older) != 3 || older[2].Seq != 7 {
		t.Fatalf("older page wrong: %+v", seqs(older))
	}
	// 越界 beforeSeq = 空页（不是错误）。
	none, err := st.ReadDirectMessages(ctx, "ag_page", 1, 3)
	if err != nil || len(none) != 0 {
		t.Fatalf("beforeSeq=1 should be empty, got %+v, %v", seqs(none), err)
	}
}

func seqs(msgs []Message) []int64 {
	out := make([]int64, len(msgs))
	for i, m := range msgs {
		out[i] = m.Seq
	}
	return out
}

// 未读 = seq > human_read_seq；游标只前进不后退；无游标 = 全未读。
func TestDMUnreadAndCursorMonotonic(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	unread, last, err := st.DMUnread(ctx, "ag_unread")
	if err != nil || unread != 0 || last != nil {
		t.Fatalf("empty DM: unread=%d last=%v err=%v", unread, last, err)
	}
	for i := 0; i < 4; i++ {
		if _, err := st.AppendDirectMessage(ctx, "ag_unread", dmMsg(KindText, SenderHuman, "m")); err != nil {
			t.Fatal(err)
		}
	}
	unread, last, err = st.DMUnread(ctx, "ag_unread")
	if err != nil || unread != 4 || last == nil || last.Seq != 4 {
		t.Fatalf("fresh DM: unread=%d last=%+v err=%v", unread, last, err)
	}
	if err := st.SaveDMCursor(ctx, "ag_unread", 3); err != nil {
		t.Fatal(err)
	}
	if unread, _, err = st.DMUnread(ctx, "ag_unread"); err != nil || unread != 1 {
		t.Fatalf("after cursor=3 unread=%d err=%v, want 1", unread, err)
	}
	// 倒退请求被忽略（乱序/并发命令不能把已读变未读）。
	if err := st.SaveDMCursor(ctx, "ag_unread", 1); err != nil {
		t.Fatal(err)
	}
	cur, err := st.LoadDMCursor(ctx, "ag_unread")
	if err != nil || cur.HumanReadSeq != 3 {
		t.Fatalf("cursor regressed: %+v, %v", cur, err)
	}
}

// 重启后 seq 从磁盘重建（不能从 1 重来，否则前端游标会把旧消息当新消息）。
func TestDirectMessageSeqRebuiltOnOpen(t *testing.T) {
	root := t.TempDir()
	st, err := OpenFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := st.AppendDirectMessage(ctx, "ag_rebuild", dmMsg(KindText, SenderHuman, "m")); err != nil {
			t.Fatal(err)
		}
	}
	// 同一个 root 重开（模拟进程重启）。
	st2, err := OpenFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	seq, err := st2.AppendDirectMessage(ctx, "ag_rebuild", dmMsg(KindText, SenderHuman, "after restart"))
	if err != nil {
		t.Fatal(err)
	}
	if seq != 4 {
		t.Fatalf("seq after restart = %d, want 4", seq)
	}
	if _, err := os.Stat(filepath.Join(root, "dms", "ag_rebuild", "log.jsonl")); err != nil {
		t.Fatalf("dm log missing: %v", err)
	}
}
