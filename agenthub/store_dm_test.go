package agenthub

// P1-04：DM 索引与未读游标的存储层语义。

import (
	"context"
	"errors"
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

// P1-04：client_msg_id 去重查询不设窗口（旧实现只扫最近 100 条，超出就重复入上下文）。
func TestFindDirectMessageByClientID(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, found, err := st.FindDirectMessageByClientID(ctx, "ag_dedupe", ""); err != nil || found {
		t.Fatalf("empty client id: found=%v err=%v; want false,nil", found, err)
	}
	if _, found, err := st.FindDirectMessageByClientID(ctx, "ag_dedupe", "missing"); err != nil || found {
		t.Fatalf("unknown client id: found=%v err=%v; want false,nil", found, err)
	}
	if _, _, err := st.FindDirectMessageByClientID(ctx, "../escape", "x"); err == nil {
		t.Fatal("invalid agent id must fail")
	}

	first := dmMsg(KindText, SenderHuman, "old")
	first.Refs = map[string]any{"client_msg_id": "c-old"}
	seq, err := st.AppendDirectMessage(ctx, "ag_dedupe", first)
	if err != nil {
		t.Fatal(err)
	}
	// 150 条之后（远超旧的 100 条窗口）仍要能查到。
	for i := 0; i < 150; i++ {
		m := dmMsg(KindText, SenderAgent, "later")
		m.Refs = map[string]any{"client_msg_id": "c-filler"}
		if _, err := st.AppendDirectMessage(ctx, "ag_dedupe", m); err != nil {
			t.Fatal(err)
		}
	}
	got, found, err := st.FindDirectMessageByClientID(ctx, "ag_dedupe", "c-old")
	if err != nil || !found || got != seq {
		t.Fatalf("outside window: seq=%d found=%v err=%v; want %d,true", got, found, err, seq)
	}
	// 重复写入同一 id 时返回最新那条（重发判定以最后落盘的为准）。
	dup := dmMsg(KindText, SenderHuman, "old again")
	dup.Refs = map[string]any{"client_msg_id": "c-old"}
	dupSeq, err := st.AppendDirectMessage(ctx, "ag_dedupe", dup)
	if err != nil {
		t.Fatal(err)
	}
	if got, found, _ := st.FindDirectMessageByClientID(ctx, "ag_dedupe", "c-old"); !found || got != dupSeq {
		t.Fatalf("duplicate lookup = %d,%v; want %d,true", got, found, dupSeq)
	}
	// 末尾半行（没有换行 = 那次追加没落盘）丢掉，不影响查询。
	path := st.dmLogPath("ag_dedupe")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":151,"text":"half`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got, found, err := st.FindDirectMessageByClientID(ctx, "ag_dedupe", "c-old"); err != nil || !found || got != dupSeq {
		t.Fatalf("after torn tail: seq=%d found=%v err=%v", got, found, err)
	}
}

// 完整但坏的行必须报 ErrCorrupt，而不是静默跳过 —— 静默跳过会让去重永远失效。
func TestFindDirectMessageByClientIDCorruptLine(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	m := dmMsg(KindText, SenderHuman, "hi")
	m.Refs = map[string]any{"client_msg_id": "c-1"}
	if _, err := st.AppendDirectMessage(ctx, "ag_corrupt", m); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(st.dmLogPath("ag_corrupt"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, _, err := st.FindDirectMessageByClientID(ctx, "ag_corrupt", "c-1"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}
