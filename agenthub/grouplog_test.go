package agenthub

// P3a-02：群日志/活动/digest/seq/恢复。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newGroup(t *testing.T, st *FileStore, name string) *Group {
	t.Helper()
	ctx := context.Background()
	id, err := st.AllocateID(ctx, PrefixGroup)
	if err != nil {
		t.Fatal(err)
	}
	g := &Group{
		ID: GroupID(id), Name: name, OwnerAgentID: "ag_aaaaaaaa",
		Members: []Member{{AgentID: "ag_aaaaaaaa", Role: RoleOwner}},
	}
	if err := g.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGroupLogSeqAndPagination(t *testing.T) {
	ctx := context.Background()
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g := newGroup(t, st, "日志群")
	for i := 0; i < 5; i++ {
		m := &Message{GroupID: g.ID, Sender: Sender{Kind: SenderHuman}, Text: "消息" + string(rune('A'+i))}
		seq, err := AppendGroupMessage(ctx, st, m)
		if err != nil || seq != int64(i+1) || m.ID != MessageID(int64(i+1)) {
			t.Fatalf("append %d: seq=%d id=%s err=%v", i, seq, m.ID, err)
		}
	}
	// 重开：seq 从日志重建，不重复/id 不回退。
	st2, err := OpenFileStore(st.Root())
	if err != nil {
		t.Fatal(err)
	}
	seq, err := AppendGroupMessage(ctx, st2, &Message{GroupID: g.ID, Sender: Sender{Kind: SenderHuman}, Text: "重开后"})
	if err != nil || seq != 6 {
		t.Fatalf("seq after reopen = %d, %v", seq, err)
	}
	// 分页：beforeSeq=4 → seq 1..3。
	older, err := ReadGroupLog(ctx, st2, g.ID, 4, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(older) != 3 || older[0].Seq != 1 || older[2].Seq != 3 {
		t.Fatalf("older = %+v", older)
	}
	// 最新一页 2 条：seq 5,6。
	latest, err := ReadGroupLog(ctx, st2, g.ID, 0, 2)
	if err != nil || len(latest) != 2 || latest[1].Seq != 6 {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
	// 已删除群不能再追加/读。
	g.Deleted = true
	if err := st2.SaveGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendGroupMessage(ctx, st2, &Message{GroupID: g.ID, Text: "x"}); err == nil {
		t.Fatal("append to deleted group must fail")
	}
	if _, err := ReadGroupLog(ctx, st2, g.ID, 0, 10); err == nil {
		t.Fatal("read deleted group must fail")
	}
}

func TestGroupActivityIndependent(t *testing.T) {
	ctx := context.Background()
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g := newGroup(t, st, "活动群")
	if _, err := AppendGroupMessage(ctx, st, &Message{GroupID: g.ID, Sender: Sender{Kind: SenderHuman}, Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendActivity(ctx, &Activity{GroupID: g.ID, AgentID: "ag_1", AfterSeq: 1, Kind: "tool", Name: "bash", Status: "done", Ms: 12}); err != nil {
		t.Fatal(err)
	}
	acts, err := ReadGroupActivity(ctx, st, g.ID, 0, 10)
	if err != nil || len(acts) != 1 || acts[0].AfterSeq != 1 {
		t.Fatalf("activity = %+v, %v", acts, err)
	}
	// 活动不占消息 seq。
	seq, err := AppendGroupMessage(ctx, st, &Message{GroupID: g.ID, Sender: Sender{Kind: SenderHuman}, Text: "again"})
	if err != nil || seq != 2 {
		t.Fatalf("seq = %d, %v", seq, err)
	}
	msgs, _ := ReadGroupLog(ctx, st, g.ID, 0, 10)
	for _, m := range msgs {
		if m.Kind == "activity" {
			t.Fatal("activity leaked into message log")
		}
	}
}

func TestGroupLogRecoversTruncatedTail(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := OpenFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	g := newGroup(t, st, "崩溃群")
	if _, err := AppendGroupMessage(ctx, st, &Message{GroupID: g.ID, Sender: Sender{Kind: SenderHuman}, Text: "完整"}); err != nil {
		t.Fatal(err)
	}
	// 模拟崩溃：最后一行只写了一半。
	path := filepath.Join(root, "groups", string(g.ID), "log.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":2,"group_id":"` + string(g.ID) + `","text":"半`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	st2, err := OpenFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	seq, err := AppendGroupMessage(ctx, st2, &Message{GroupID: g.ID, Sender: Sender{Kind: SenderHuman}, Text: "恢复后"})
	if err != nil {
		t.Fatal(err)
	}
	if seq != 2 {
		t.Fatalf("seq after truncated tail = %d, want 2（残行不占 seq，且不回退）", seq)
	}
	msgs, err := ReadGroupLog(ctx, st2, g.ID, 0, 10)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("msgs = %+v, %v", msgs, err)
	}
}

func TestBuildRoundSummaryDeterministic(t *testing.T) {
	items := []RoundItem{
		{AgentID: "ag_b", Tool: "list", Status: "done", Summary: "52 accounts"},
		{AgentID: "ag_a", Tool: "bash", Status: "error", Summary: "boom", Ms: 30},
		{AgentID: "ag_a", Tool: "read", Status: "done", Summary: "README"},
	}
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	first := BuildRoundSummary("grp_aaaaaa", "rnd_1", items, at)
	// 打乱输入顺序，输出必须一致。
	shuffled := []RoundItem{items[2], items[0], items[1]}
	second := BuildRoundSummary("grp_aaaaaa", "rnd_1", shuffled, at)
	if first.Text != second.Text {
		t.Fatalf("summary not deterministic:\n%s\n---\n%s", first.Text, second.Text)
	}
	if first.Kind != KindRoundSummary || first.Sender.Kind != SenderSystem {
		t.Fatalf("summary = %+v", first)
	}
	lines := strings.Split(first.Text, "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "ag_a · bash") || !strings.HasPrefix(lines[2], "ag_b · list") {
		t.Fatalf("lines = %#v", lines)
	}
}

func TestBuildDigestDeterministic(t *testing.T) {
	msgs := []Message{
		{Seq: 1, Sender: Sender{Kind: SenderHuman}, Text: "第一行\n第二行"},
		{Seq: 2, Sender: Sender{Kind: SenderAgent, AgentID: "ag_a"}, Text: "好的"},
		{Seq: 3, Sender: Sender{Kind: SenderSystem}, Text: "系统提示"},
	}
	first := BuildDigest(msgs, 2, 0)
	second := BuildDigest(msgs, 2, 0)
	if first != second || strings.Contains(first, "第一行") {
		t.Fatalf("digest = %q / %q", first, second)
	}
	if !strings.Contains(first, "#2 ag_a: 好的") || !strings.Contains(first, "#3 系统: 系统提示") {
		t.Fatalf("digest = %q", first)
	}
	clipped := BuildDigest(msgs, 3, 5)
	if len([]rune(clipped)) != 6 || !strings.HasSuffix(clipped, "…") {
		t.Fatalf("clipped = %q", clipped)
	}
}

// 并发追加：seq 必须唯一且连续（Store 的 mu 覆盖 seq 分配+落盘）。
func TestGroupLogConcurrentAppend(t *testing.T) {
	ctx := context.Background()
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g := newGroup(t, st, "并发群")
	var wg sync.WaitGroup
	seqs := make([]int64, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			seq, err := AppendGroupMessage(ctx, st, &Message{GroupID: g.ID, Sender: Sender{Kind: SenderHuman}, Text: "并发"})
			if err != nil {
				t.Errorf("append: %v", err)
				return
			}
			seqs[i] = seq
		}(i)
	}
	wg.Wait()
	seen := map[int64]bool{}
	for _, s := range seqs {
		if s == 0 || seen[s] {
			t.Fatalf("bad seq: %v", seqs)
		}
		seen[s] = true
	}
}
