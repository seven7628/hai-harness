package agenthub

// P3a-03：@ 路由 + round + 回执。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func routerFixture(t *testing.T) (*Group, *Router) {
	t.Helper()
	profiles := []*AgentProfile{
		{ID: "ag_aaaaaaaa", Name: "Alice"},
		{ID: "ag_bbbbbbbb", Name: "Bob"},
		{ID: "ag_cccccccc", Name: "Cara"},
		{ID: "ag_dddddddd", Name: "Dan"},
		{ID: "ag_eeeeeeee", Name: "Eve"},
	}
	g := &Group{
		ID: "grp_aaaaaa", Name: "路由群", OwnerAgentID: "ag_aaaaaaaa",
		Members: []Member{
			{AgentID: "ag_aaaaaaaa", Role: RoleOwner, JoinedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			{AgentID: "ag_bbbbbbbb", Role: RoleMember, JoinedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
			{AgentID: "ag_cccccccc", Role: RoleMember, JoinedAt: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)},
			{AgentID: "ag_dddddddd", Role: RoleMember, JoinedAt: time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)},
			{AgentID: "ag_eeeeeeee", Role: RoleObserver, JoinedAt: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)},
		},
	}
	r, err := NewGroupRouter(g, profiles)
	if err != nil {
		t.Fatal(err)
	}
	return g, r
}

func TestRouterNoMentionNoWake(t *testing.T) {
	_, r := routerFixture(t)
	round, err := r.Route("大家好，今天先同步一下进度", "ag_aaaaaaaa", false, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(round.Targets) != 0 {
		t.Fatalf("targets = %v, want none", round.Targets)
	}
}

func TestRouterExplicitMentionsAndSelf(t *testing.T) {
	_, r := routerFixture(t)
	round, err := r.Route("@Bob 看一下日志；@Alice 自己也提一下；@Cara 复核", "ag_aaaaaaaa", false, 2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(round.Targets) != 2 || round.Targets[0] != "ag_bbbbbbbb" || round.Targets[1] != "ag_cccccccc" {
		t.Fatalf("targets = %v", round.Targets)
	}
	// 未知 @ 不唤醒。
	round, err = r.Route("@Nobody 在吗", "ag_aaaaaaaa", false, 3, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(round.Targets) != 0 || len(round.Unknown) != 1 || round.Unknown[0] != "Nobody" {
		t.Fatalf("round = %+v", round)
	}
}

func TestRouterMentionAllAuthorization(t *testing.T) {
	_, r := routerFixture(t)
	// owner 可以 @all：目标是除自己外全部成员（按 JoinedAt/ID 确定）。
	round, err := r.Route("@all 开始", "ag_aaaaaaaa", false, 4, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !round.MentionAll || len(round.Targets) != 4 || round.Targets[0] != "ag_bbbbbbbb" || round.Targets[3] != "ag_eeeeeeee" {
		t.Fatalf("owner @all = %+v", round)
	}
	// 普通成员 @all：拒绝且不降级。
	if _, err := r.Route("@all 开始", "ag_bbbbbbbb", false, 5, time.Now()); !errors.Is(err, ErrMentionAllForbidden) {
		t.Fatalf("member @all err = %v, want ErrMentionAllForbidden", err)
	}
	// 人类可以 @all（sender 空）。
	round, err = r.Route("@all 开会", "", true, 6, time.Now())
	if err != nil || len(round.Targets) != 5 {
		t.Fatalf("human @all = %+v, %v", round, err)
	}
}

func TestRouterMentionLimitAndReceipt(t *testing.T) {
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g, r := routerFixture(t)
	g.Policy.MaxMentions = 2
	if err := g.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveGroup(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	round, err := r.Route("@Bob @Cara @Dan @Eve 四个都来", "ag_aaaaaaaa", false, 7, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(round.Targets) != 2 || round.Dropped != 2 {
		t.Fatalf("limit round = %+v", round)
	}
	if err := AppendRoundReceipt(context.Background(), st, round); err != nil {
		t.Fatal(err)
	}
	acts, err := st.ReadActivity(context.Background(), g.ID, 0, 10)
	if err != nil || len(acts) != 1 {
		t.Fatalf("activity = %+v, %v", acts, err)
	}
	if acts[0].Kind != "routing" || acts[0].AfterSeq != 7 || acts[0].RunID != round.ID {
		t.Fatalf("receipt = %+v", acts[0])
	}
	if acts[0].Summary != "targets=ag_bbbbbbbb,ag_cccccccc dropped=2" {
		t.Fatalf("summary = %q", acts[0].Summary)
	}
	if _, err := round.TargetsOf(); err != nil {
		t.Fatal(err)
	}
	dup := Round{Targets: []AgentID{"ag_aaaaaaaa", "ag_aaaaaaaa"}}
	if _, err := dup.TargetsOf(); err == nil {
		t.Fatal("duplicate targets must be rejected")
	}
}

// P3a-03 B-3：Router 只认「本群成员 ∩ 未删除 profile」，且构造后成员冻结。
func TestRouterMembersSnapshotAndDeletedProfiles(t *testing.T) {
	g, _ := routerFixture(t)
	profiles := []*AgentProfile{
		{ID: "ag_aaaaaaaa", Name: "Alice"},
		{ID: "ag_bbbbbbbb", Name: "Bob", Deleted: true}, // 软删
		{ID: "ag_cccccccc", Name: "Cara"},
		{ID: "ag_dddddddd", Name: "Dan"},
		{ID: "ag_eeeeeeee", Name: "Eve"},
		{ID: "ag_ffffffff", Name: "Outsider"}, // 不在本群
	}
	r, err := NewGroupRouter(g, profiles)
	if err != nil {
		t.Fatal(err)
	}
	// @all 不含软删成员；显式 @ 软删/跨群成员都只算未知。
	round, err := r.Route("@all 开会", "ag_aaaaaaaa", false, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range round.Targets {
		if id == "ag_bbbbbbbb" {
			t.Fatalf("@all woke deleted member: %v", round.Targets)
		}
	}
	if len(round.Targets) != 3 {
		t.Fatalf("@all targets = %v, want Cara/Dan/Eve", round.Targets)
	}
	for _, text := range []string{"@Bob 在吗", "@Outsider 在吗"} {
		round, err = r.Route(text, "ag_aaaaaaaa", false, 2, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(round.Targets) != 0 {
			t.Fatalf("%s targets = %v, want none", text, round.Targets)
		}
	}
	// 构造后改 Group.Members 不影响已构造 Router（N-3）。
	g.Members = append(g.Members, Member{AgentID: "ag_ffffffff", Role: RoleMember, JoinedAt: time.Now()})
	round, err = r.Route("@all 再开会", "ag_aaaaaaaa", false, 3, time.Now())
	if err != nil || len(round.Targets) != 3 {
		t.Fatalf("router did not freeze members: %+v, %v", round, err)
	}
	// 已删除的群不能构造 Router。
	gone := *g
	gone.Deleted = true
	if _, err := NewGroupRouter(&gone, profiles); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted group err = %v, want ErrNotFound", err)
	}
}

// P3a-03 B-1/B-2：消息 + routing_receipt + activity 原子落盘；同 client_msg_id 幂等。
func TestAppendMessageWithRoundAtomicAndDedup(t *testing.T) {
	ctx := context.Background()
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g, _ := routerFixture(t)
	if err := st.SaveGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	msg := &Message{
		GroupID: g.ID, Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: "@Bob hi",
		Refs: map[string]any{"client_msg_id": "dup-1"},
	}
	round := Round{ID: "rnd_test", GroupID: g.ID, Targets: []AgentID{"ag_bbbbbbbb"}}
	seq, deduped, err := AppendGroupMessageRouted(ctx, st, msg, &round)
	if err != nil || seq != 1 || deduped {
		t.Fatalf("append = %d/%v/%v", seq, deduped, err)
	}
	if round.TriggerSeq != 1 {
		t.Fatalf("round trigger = %d", round.TriggerSeq)
	}
	msgs, err := st.ReadMessages(ctx, g.ID, 0, 10)
	if err != nil || len(msgs) != 2 || msgs[1].Kind != KindRouting || msgs[1].RoundID != round.ID {
		t.Fatalf("log = %+v, %v", msgs, err)
	}
	acts, err := st.ReadActivity(ctx, g.ID, 0, 10)
	if err != nil || len(acts) != 1 || acts[0].Kind != "routing" ||
		acts[0].AfterSeq != msgs[1].Seq || acts[0].RunID != round.ID {
		t.Fatalf("activity = %+v, %v", acts, err)
	}
	// 回执不进快照（P3a-04 口径）。
	snap := BuildGroupSnapshot(g, "ag_bbbbbbbb", msgs, 0, Round{TriggerSeq: msgs[1].Seq}, SnapshotOptions{})
	if strings.Contains(snap.Text, "targets=") || anyKind(snap.Messages, KindRouting) {
		t.Fatalf("routing receipt leaked into snapshot: %+v", snap)
	}
	// 同 id 重试：原 seq、deduped=true，日志不增。
	retry := *msg
	retryRound := Round{ID: "rnd_other", GroupID: g.ID, Targets: []AgentID{"ag_bbbbbbbb"}}
	seq2, deduped, err := AppendGroupMessageRouted(ctx, st, &retry, &retryRound)
	if err != nil || seq2 != 1 || !deduped {
		t.Fatalf("retry = %d/%v/%v", seq2, deduped, err)
	}
	if msgs, _ = st.ReadMessages(ctx, g.ID, 0, 10); len(msgs) != 2 {
		t.Fatalf("dedupe wrote log: %+v", msgs)
	}
	// 并发同 id：锁内去重，只能有一条消息 + 一条回执。
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := &Message{GroupID: g.ID, Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: "race",
				Refs: map[string]any{"client_msg_id": "race-dup"}}
			r := Round{ID: "rnd_race", GroupID: g.ID, Targets: []AgentID{"ag_bbbbbbbb"}}
			if _, _, err := AppendGroupMessageRouted(ctx, st, m, &r); err != nil {
				t.Errorf("race append: %v", err)
			}
		}()
	}
	wg.Wait()
	msgs, _ = st.ReadMessages(ctx, g.ID, 0, 10)
	var dupText, dupReceipt int
	for _, m := range msgs {
		if messageClientMsgID(m) == "race-dup" {
			dupText++
		}
		if m.Kind == KindRouting && m.RoundID == "rnd_race" {
			dupReceipt++
		}
	}
	if dupText != 1 || dupReceipt != 1 {
		t.Fatalf("concurrent dedupe: texts=%d receipts=%d msgs=%+v", dupText, dupReceipt, msgs)
	}
	// 无 @：round=nil 只写消息，不写回执。
	beforePlain, _ := st.ReadMessages(ctx, g.ID, 0, 100)
	plain := &Message{GroupID: g.ID, Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: "只同步"}
	seq3, deduped, err := AppendGroupMessageRouted(ctx, st, plain, nil)
	if err != nil || seq3 != beforePlain[len(beforePlain)-1].Seq+1 || deduped {
		t.Fatalf("plain append = %d/%v/%v", seq3, deduped, err)
	}
	if msgs, _ = st.ReadMessages(ctx, g.ID, 0, 100); len(msgs) != len(beforePlain)+1 || msgs[len(msgs)-1].Kind != KindText {
		t.Fatalf("plain log = %+v", msgs)
	}
	if acts, _ = st.ReadActivity(ctx, g.ID, 0, 10); len(acts) != 2 {
		t.Fatalf("plain added activity: %+v", acts)
	}
}

// 上次写了消息但回执失败：同 client_msg_id 重试补回执，不重写消息。
func TestAppendMessageWithRoundHealsMissingReceipt(t *testing.T) {
	ctx := context.Background()
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g, _ := routerFixture(t)
	if err := st.SaveGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	msg := &Message{
		GroupID: g.ID, Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: "@Bob hi",
		Refs: map[string]any{"client_msg_id": "half-1"},
	}
	if _, err := st.AppendMessage(ctx, msg); err != nil {
		t.Fatal(err)
	}
	round := Round{ID: "rnd_heal", GroupID: g.ID, Targets: []AgentID{"ag_bbbbbbbb"}}
	seq, deduped, err := AppendGroupMessageRouted(ctx, st, msg, &round)
	if err != nil || seq != 1 || !deduped {
		t.Fatalf("heal = %d/%v/%v", seq, deduped, err)
	}
	msgs, _ := st.ReadMessages(ctx, g.ID, 0, 10)
	if len(msgs) != 2 || msgs[1].Kind != KindRouting {
		t.Fatalf("receipt not healed: %+v", msgs)
	}
}

// P3a-03 B-4：并发发送后 activity 的 AfterSeq 必须严格递增，且游标不跳条。
func TestAppendMessageWithRoundConcurrentOrdering(t *testing.T) {
	ctx := context.Background()
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g, _ := routerFixture(t)
	if err := st.SaveGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := Round{ID: NewID(PrefixRound), GroupID: g.ID, Targets: []AgentID{"ag_bbbbbbbb"}}
			m := &Message{GroupID: g.ID, Kind: KindText, Sender: Sender{Kind: SenderAgent, AgentID: "ag_cccccccc"}, Text: fmt.Sprintf("m%d", i)}
			if _, _, err := AppendGroupMessageRouted(ctx, st, m, &r); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	msgs, _ := st.ReadMessages(ctx, g.ID, 0, 100)
	if len(msgs) != 2*n {
		t.Fatalf("messages = %d, want %d", len(msgs), 2*n)
	}
	acts, _ := st.ReadActivity(ctx, g.ID, 0, 100)
	if len(acts) != n {
		t.Fatalf("activities = %d", len(acts))
	}
	for i := 1; i < len(acts); i++ {
		if acts[i].AfterSeq <= acts[i-1].AfterSeq {
			t.Fatalf("activity out of order at %d: %d <= %d", i, acts[i].AfterSeq, acts[i-1].AfterSeq)
		}
	}
	// 游标语义：用最后一条消息 seq 读 activity 应为空，但用倒数第二条应能拿到最后一条。
	last := msgs[len(msgs)-1].Seq
	if page, _ := st.ReadActivity(ctx, g.ID, last, 10); len(page) != 0 {
		t.Fatalf("cursor %d returned %+v", last, page)
	}
	if page, _ := st.ReadActivity(ctx, g.ID, last-3, 10); len(page) == 0 {
		t.Fatalf("cursor %d dropped tail activity", last-3)
	}
}

func anyKind(msgs []Message, kind MessageKind) bool {
	for _, m := range msgs {
		if m.Kind == kind {
			return true
		}
	}
	return false
}

// P3a-03 复审 NB-1：heal 分支二次失败不能留下没有 activity 的孤儿回执。
func TestAppendMessageWithRoundHealRollback(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := OpenFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	g, _ := routerFixture(t)
	if err := st.SaveGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	msg := &Message{
		GroupID: g.ID, Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: "@Bob hi",
		Refs: map[string]any{"client_msg_id": "heal-rollback"},
	}
	if _, err := st.AppendMessage(ctx, msg); err != nil {
		t.Fatal(err)
	}
	// 让 activity.jsonl 变成目录：回执消息能写、activity 必失败。
	activityPath := st.activityPath(g.ID)
	if err := os.MkdirAll(activityPath, 0o700); err != nil {
		t.Fatal(err)
	}
	round := Round{ID: "rnd_heal_fail", GroupID: g.ID, Targets: []AgentID{"ag_bbbbbbbb"}}
	if _, _, err := AppendGroupMessageRouted(ctx, st, msg, &round); err == nil {
		t.Fatal("heal must fail when activity is unwritable")
	}
	msgs, err := st.ReadMessages(ctx, g.ID, 0, 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("orphan receipt left behind: %+v, %v", msgs, err)
	}
	if err := os.Remove(activityPath); err != nil {
		t.Fatal(err)
	}
	seq, deduped, err := AppendGroupMessageRouted(ctx, st, msg, &round)
	if err != nil || seq != 1 || !deduped {
		t.Fatalf("retry heal = %d/%v/%v", seq, deduped, err)
	}
	msgs, _ = st.ReadMessages(ctx, g.ID, 0, 10)
	if len(msgs) != 2 || msgs[1].Kind != KindRouting {
		t.Fatalf("healed log = %+v", msgs)
	}
	if acts, _ := st.ReadActivity(ctx, g.ID, 0, 10); len(acts) != 1 {
		t.Fatalf("healed activity = %+v", acts)
	}
}
