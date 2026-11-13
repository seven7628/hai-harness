package agenthub

// P6-09：对话状态持久化（§1.6）—— 快照/恢复、pair 窗口过期、指纹封顶、
// 坏文件不覆盖、兼容「没有文件 = 空」。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newPersistStore 开一个临时 FileStore（每个测试独立目录）。
func newPersistStore(t *testing.T) *FileStore {
	t.Helper()
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}

// newPersistController 建一个带 persister 的控制器。
func newPersistController(t *testing.T, st ConversationPersister, limits ConversationLimits) (*ConversationController, *convClock) {
	t.Helper()
	clk := newConvClock()
	return NewConversationController(ConversationOptions{Limits: limits, Now: clk.Now, Persister: st}), clk
}

// TestConversationStateRoundTrip：一代对话（计数/队列/账本/pair/指纹）落盘后，换一个
// 控制器恢复：队列还在、已投递未完成标 interrupted、计数与指纹原样回来。
func TestConversationStateRoundTrip(t *testing.T) {
	st := newPersistStore(t)
	c, clk := newPersistController(t, st, ConversationLimits{Cooldown: -1, PairLimit: 99, MaxDepth: 99})
	c.BeginHuman("group_1", 7, clk.Now())
	// 一条已投递（running）的 agent→agent 唤醒（会记 pair/指纹）。
	wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
		Text: "@agent_b 第一件事", Depth: 2, TriggerSeq: 11, RoundID: "rnd_1",
	})
	if err != nil || len(wakes) != 1 {
		t.Fatalf("offer: %v %v", wakes, err)
	}
	begun, fresh := c.BeginDelivery(wakes[0], clk.Now())
	if !fresh {
		t.Fatal("begin delivery")
	}
	c.CommitDelivery(begun, clk.Now())
	// 一条还在排队的人类唤醒（未投递）。
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Human: true, Targets: convIDs("agent_c"),
		Text: "@agent_c 第二件事", TriggerSeq: 12, RoundID: "rnd_2",
	}); err != nil {
		t.Fatalf("offer human: %v", err)
	}
	before := c.View("group_1")

	// 「重启」：同一个 store，新的控制器。
	c2, clk2 := newPersistController(t, st, ConversationLimits{Cooldown: -1, PairLimit: 99, MaxDepth: 99})
	state, err := st.LoadConversationState(context.Background(), "group_1")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	rep := c2.Restore(state, clk2.Now())
	if rep.Restored != 1 {
		t.Fatalf("恢复的队列条数 = %d, want 1", rep.Restored)
	}
	if rep.Interrupted != 1 {
		t.Fatalf("interrupted = %d, want 1（已投递未完成的那条）", rep.Interrupted)
	}
	after := c2.View("group_1")
	if after.ID != before.ID || after.HumanSeq != before.HumanSeq || after.Status != before.Status {
		t.Fatalf("恢复的对话 = %+v, want id/human_seq/status 同 %+v", after, before)
	}
	if after.TurnsUsed != before.TurnsUsed || after.QueueLen != 1 {
		t.Fatalf("恢复的 turns/queue = %d/%d, want %d/1", after.TurnsUsed, after.QueueLen, before.TurnsUsed)
	}
	if got := c2.PairCount("group_1", "agent_a", "agent_b"); got != 1 {
		t.Fatalf("pair 计数 = %d, want 1（无序 pair 要跟着恢复）", got)
	}
	// 已投递未完成：账本记 interrupted，且**不进队列**（不重放副作用）。
	if d, ok := c2.Delivery("group_1", wakes[0].ID); !ok || d.Status != DeliveryFailed || d.Reason != DeliveryReasonInterrupted {
		t.Fatalf("重启后 running 记录 = %+v ok=%v, want failed(interrupted)", d, ok)
	}
	due := c2.DueWakes("group_1", clk2.Now())
	if len(due) != 1 || due[0].Target != "agent_c" {
		t.Fatalf("恢复后队列 = %+v, want 只有 agent_c（不重放已投递的那条）", due)
	}
	// 指纹也恢复了：同一段正文再来一次仍然判循环。
	if _, err := c2.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
		Text: "@agent_b 第一件事", Depth: 2, TriggerSeq: 13,
	}); !errors.Is(err, ErrConversationExhausted) {
		t.Fatalf("恢复后的指纹没生效（同内容没判循环）：%v", err)
	}
}

// TestConversationStatePairWindowExpiresOnLoad：加载时先按 pair 窗口过期 —— 重启期间
// 过期的额度直接还回来，不能把「昨天的互踢」算到今天的对话里。
func TestConversationStatePairWindowExpiresOnLoad(t *testing.T) {
	st := newPersistStore(t)
	old := time.Now().Add(-30 * time.Minute)
	fresh := time.Now().Add(-1 * time.Minute)
	state := &ConversationState{
		SchemaVersion: ConversationStateSchema, GroupID: "group_1", ConversationID: "conv_1",
		Status: ConversationRunning, Turns: 3,
		Pairs: []ConversationPairState{{Key: ConversationPairKey("agent_a", "agent_b"), At: []time.Time{old, fresh}}},
	}
	if err := st.SaveConversationState(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	c := NewConversationController(ConversationOptions{
		Limits: ConversationLimits{PairWindow: 10 * time.Minute}, Persister: st,
	})
	loaded, err := st.LoadConversationState(context.Background(), "group_1")
	if err != nil {
		t.Fatal(err)
	}
	rep := c.Restore(loaded, time.Now())
	if rep.ExpiredPairs != 1 {
		t.Fatalf("过期的时间戳 = %d, want 1", rep.ExpiredPairs)
	}
	if got := c.PairCount("group_1", "agent_b", "agent_a"); got != 1 {
		t.Fatalf("窗口内的 pair 计数 = %d, want 1（只留窗口内那一条）", got)
	}
}

// TestConversationStateCorruptNotOverwritten：坏文件必须报 ErrCorrupt，且 BlockPersist
// 之后**不再写**这个文件（覆盖掉就丢了唯一一份证据）。
func TestConversationStateCorruptNotOverwritten(t *testing.T) {
	st := newPersistStore(t)
	path := filepath.Join(st.Root(), "conversations", "group_1.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	garbage := []byte("{ this is not json")
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LoadConversationState(context.Background(), "group_1"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("坏文件应返回 ErrCorrupt，得到 %v", err)
	}
	c := NewConversationController(ConversationOptions{
		Limits: ConversationLimits{Cooldown: -1}, Persister: st,
	})
	c.BlockPersist("group_1")
	c.BeginHuman("group_1", 1, time.Time{})
	if !c.PersistBlocked("group_1") {
		t.Fatal("BlockPersist 没生效")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(garbage) {
		t.Fatalf("坏文件被覆盖了：%q", string(got))
	}
}

// TestConversationStatePrintsCapped：指纹集合有上限（FIFO 丢最旧），落盘后也不膨胀。
func TestConversationStatePrintsCapped(t *testing.T) {
	st := newPersistStore(t)
	c, clk := newPersistController(t, st, ConversationLimits{Cooldown: -1, PairLimit: 9999, MaxDepth: 99})
	c.BeginHuman("group_1", 1, clk.Now())
	for i := 0; i < MaxConversationPrints+8; i++ {
		if _, err := c.Offer(ConversationOffer{
			GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
			Text: "第 " + itoa(i) + " 段不同的话", Depth: 2, TriggerSeq: int64(i + 1),
		}); err != nil {
			t.Fatalf("offer %d: %v", i, err)
		}
	}
	state, err := st.LoadConversationState(context.Background(), "group_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Prints) != 1 || len(state.Prints[0].Prints) != MaxConversationPrints {
		t.Fatalf("落盘指纹 = %+v, want 1 对 / %d 条", state.Prints, MaxConversationPrints)
	}
	// 最旧的被丢掉了：第 0 段不再判循环；最后一段仍然判循环。
	if _, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
		Text: "第 0 段不同的话", Depth: 2, TriggerSeq: 99,
	}); err != nil {
		t.Fatalf("最旧的指纹应该已经被挤出：%v", err)
	}
}

// TestConversationStateMissingFileIsEmpty：兼容旧数据 —— 没有文件 = 空对话，
// Restore(nil) 是 no-op，不能凭空造出一代对话。
func TestConversationStateMissingFileIsEmpty(t *testing.T) {
	st := newPersistStore(t)
	if _, err := st.LoadConversationState(context.Background(), "group_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("缺文件应返回 ErrNotFound，得到 %v", err)
	}
	groups, err := st.ListConversationGroups(context.Background())
	if err != nil || len(groups) != 0 {
		t.Fatalf("空存储的对话群 = %v err=%v, want 空", groups, err)
	}
	c := NewConversationController(ConversationOptions{Limits: ConversationLimits{Cooldown: -1}, Persister: st})
	if rep := c.Restore(nil, time.Time{}); rep.Restored != 0 || rep.Interrupted != 0 {
		t.Fatalf("Restore(nil) 报告 = %+v, want 空", rep)
	}
	if v := c.View("group_1"); v.Status != ConversationIdle {
		t.Fatalf("空恢复后视图 = %+v, want idle", v)
	}
}

// TestConversationStateDeleteClears：删除群时清状态文件。
func TestConversationStateDeleteClears(t *testing.T) {
	st := newPersistStore(t)
	c, clk := newPersistController(t, st, ConversationLimits{Cooldown: -1})
	c.BeginHuman("group_1", 1, clk.Now())
	if groups, _ := st.ListConversationGroups(context.Background()); len(groups) != 1 {
		t.Fatalf("写入后对话群 = %v, want 1 个", groups)
	}
	if err := c.DeleteState(context.Background(), "group_1"); err != nil {
		t.Fatal(err)
	}
	if groups, _ := st.ListConversationGroups(context.Background()); len(groups) != 0 {
		t.Fatalf("删除后对话群 = %v, want 空", groups)
	}
	if _, err := st.LoadConversationState(context.Background(), "group_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后加载应 ErrNotFound，得到 %v", err)
	}
}

// countingPersister 记录落盘次数与最后一份状态（断言「写时机」用）。
type countingPersister struct {
	mu    sync.Mutex
	saves int
	last  *ConversationState
}

func (p *countingPersister) SaveConversationState(_ context.Context, st *ConversationState) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saves++
	p.last = st
	return nil
}

func (p *countingPersister) LoadConversationState(context.Context, GroupID) (*ConversationState, error) {
	return nil, ErrNotFound
}

func (p *countingPersister) ListConversationGroups(context.Context) ([]GroupID, error) {
	return nil, nil
}

func (p *countingPersister) DeleteConversationState(context.Context, GroupID) error { return nil }

func (p *countingPersister) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.saves
}

// TestConversationStateWritesOnTransitionsOnly：落盘只发生在状态变化上 —— 读视图
// （View 在 emit 热路径上，每个 token 级事件都会调）一次都不写。
func TestConversationStateWritesOnTransitionsOnly(t *testing.T) {
	p := &countingPersister{}
	clk := newConvClock()
	c := NewConversationController(ConversationOptions{
		Limits: ConversationLimits{Cooldown: -1}, Now: clk.Now, Persister: p,
	})
	c.BeginHuman("group_1", 1, clk.Now())
	if got := p.count(); got != 1 {
		t.Fatalf("BeginHuman 后落盘 = %d 次, want 1", got)
	}
	// 读视图（每个流式事件都会走这条路）不写盘。
	for i := 0; i < 20; i++ {
		_ = c.View("group_1")
		_ = c.Deliveries("group_1")
	}
	if got := p.count(); got != 1 {
		t.Fatalf("读视图触发了 %d 次落盘（不能按 token 写）", got-1)
	}
	wakes, err := c.Offer(ConversationOffer{
		GroupID: "group_1", Sender: "agent_a", Targets: convIDs("agent_b"),
		Text: "一段话", Depth: 2, TriggerSeq: 2,
	})
	if err != nil || len(wakes) != 1 {
		t.Fatalf("offer: %v %v", wakes, err)
	}
	if got := p.count(); got != 2 {
		t.Fatalf("入队后落盘 = %d 次, want 2", got)
	}
	begun, fresh := c.BeginDelivery(wakes[0], clk.Now())
	if !fresh {
		t.Fatal("begin delivery")
	}
	if got := p.count(); got != 3 {
		t.Fatalf("出队（BeginDelivery）后落盘 = %d 次, want 3", got)
	}
	c.CommitDelivery(begun, clk.Now())
	if got := p.count(); got != 4 {
		t.Fatalf("投递成功（pair 变化）后落盘 = %d 次, want 4", got)
	}
	c.FinishAgentDeliveries("group_1", "agent_b", DeliveryDone, "", clk.Now())
	if got := p.count(); got != 5 {
		t.Fatalf("收尾（complete）后落盘 = %d 次, want 5", got)
	}
	c.Stop("group_1", "人类停止", clk.Now())
	if got := p.count(); got != 6 {
		t.Fatalf("停止后落盘 = %d 次, want 6", got)
	}
	// 最后一次快照里：running 记录已经收口成 done，队列为空。
	last := p.last
	if last == nil || last.Status != ConversationStopped {
		t.Fatalf("最后一份状态 = %+v, want stopped", last)
	}
	if len(last.Queue) != 0 {
		t.Fatalf("最后一份状态的队列 = %+v, want 空（终态记录不落盘）", last.Queue)
	}
}

// itoa 是 strconv.Itoa 的极小替代（测试里只用来拼不同正文）。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [8]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[n:])
}
