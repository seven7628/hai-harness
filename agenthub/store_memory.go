package agenthub

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryStore 是 Store 的内存实现，供单元测试/评测使用；不落盘、不跨进程。
type MemoryStore struct {
	mu       sync.Mutex
	agents   map[AgentID]*AgentProfile
	groups   map[GroupID]*Group
	messages map[GroupID][]Message
	// dmMessages 是 DM 索引（FileStore 的 dms/<agent>/log.jsonl 对应物）。
	// DM 不在 Store 接口里（bridge 用 *FileStore 的 DM 方法），这里只为搜索测试/
	// 评测保留同一份能力。
	dmMessages map[AgentID][]Message
	activity   map[GroupID][]Activity
	tasks      map[TaskID]*Task
	taskEvent  map[TaskID][]TaskEvent
	taskSeq    map[TaskID]int64
	needs      map[NeedID]*Need
	handoffs   map[string]*Handoff
	artifacts  map[ArtifactID]*Artifact
	cursors    map[string]Cursor
	reserved   map[string]struct{}
	idGen      func(prefix string) string
	taskIdx    *taskIndex
}

var (
	_ Store     = (*FileStore)(nil)
	_ Store     = (*MemoryStore)(nil)
	_ TaskIndex = (*FileStore)(nil)
	_ TaskIndex = (*MemoryStore)(nil)
)

// NewMemoryStore 创建空的内存 Store。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		agents:     make(map[AgentID]*AgentProfile),
		groups:     make(map[GroupID]*Group),
		messages:   make(map[GroupID][]Message),
		dmMessages: make(map[AgentID][]Message),
		activity:   make(map[GroupID][]Activity),
		tasks:      make(map[TaskID]*Task),
		taskEvent:  make(map[TaskID][]TaskEvent),
		taskSeq:    make(map[TaskID]int64),
		needs:      make(map[NeedID]*Need),
		handoffs:   make(map[string]*Handoff),
		artifacts:  make(map[ArtifactID]*Artifact),
		cursors:    make(map[string]Cursor),
		reserved:   make(map[string]struct{}),
		idGen:      NewID,
		taskIdx:    newTaskIndex(),
	}
}

func (s *MemoryStore) AllocateID(ctx context.Context, prefix string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !knownPrefix(prefix) || prefix == PrefixRound {
		return "", ErrInvalid
	}
	for i := 0; i < 16; i++ {
		id := s.idGen(prefix)
		s.mu.Lock()
		_, used := s.reserved[id]
		exists := used || s.idExistsLocked(prefix, id)
		if !exists {
			s.reserved[id] = struct{}{}
		}
		s.mu.Unlock()
		if !exists {
			return id, nil
		}
	}
	return "", ErrConflict
}

func (s *MemoryStore) idExistsLocked(prefix, id string) bool {
	switch prefix {
	case PrefixAgent:
		_, ok := s.agents[AgentID(id)]
		return ok
	case PrefixGroup:
		_, ok := s.groups[GroupID(id)]
		return ok
	case PrefixTask:
		_, ok := s.tasks[TaskID(id)]
		return ok
	case PrefixNeed:
		_, ok := s.needs[NeedID(id)]
		return ok
	case PrefixArtifact:
		_, ok := s.artifacts[ArtifactID(id)]
		return ok
	default:
		return false
	}
}

func (s *MemoryStore) LoadAgent(ctx context.Context, id AgentID) (*AgentProfile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(id)) {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.agents[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneAgentProfile(p), nil
}

func (s *MemoryStore) SaveAgent(ctx context.Context, p *AgentProfile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || !validID(string(p.ID)) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agents[p.ID] = cloneAgentProfile(p)
	return nil
}

func (s *MemoryStore) ListAgents(ctx context.Context) ([]*AgentProfile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*AgentProfile, 0, len(s.agents))
	for _, p := range s.agents {
		out = append(out, cloneAgentProfile(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *MemoryStore) LoadGroup(ctx context.Context, id GroupID) (*Group, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(id)) {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[id]
	if !ok {
		return nil, ErrNotFound
	}
	out := cloneGroup(g)
	out.MigrateLegacyMemberRoles() // 与 FileStore 同口径（P6-12）
	return out, nil
}

func (s *MemoryStore) SaveGroup(ctx context.Context, g *Group) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if g == nil || !validID(string(g.ID)) {
		return ErrInvalid
	}
	if err := rejectRemovedMemberRoles(g); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 与 FileStore 同语义：群整份写回时保留更大的 human_read_seq（独立写者只前进）。
	saved := cloneGroup(g)
	if cur, ok := s.groups[g.ID]; ok && cur != nil && cur.HumanReadSeq > saved.HumanReadSeq {
		saved.HumanReadSeq = cur.HumanReadSeq
	}
	s.groups[g.ID] = saved
	return nil
}

func (s *MemoryStore) ListGroups(ctx context.Context) ([]*Group, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Group, 0, len(s.groups))
	for _, g := range s.groups {
		cp := cloneGroup(g)
		cp.MigrateLegacyMemberRoles() // 与 FileStore 同口径（P6-12）
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *MemoryStore) AppendMessage(ctx context.Context, m *Message) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if m == nil || !validID(string(m.GroupID)) {
		return 0, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := int64(len(s.messages[m.GroupID]) + 1)
	m.Seq = seq
	m.ID = messageIDFor(m, seq)
	if m.CreatedAt.IsZero() {
		m.CreatedAt = nowUTC()
	}
	s.messages[m.GroupID] = append(s.messages[m.GroupID], cloneMessage(*m))
	return seq, nil
}

// AppendMessageWithRound 与 FileStore 同契约：消息、回执消息、routing activity
// 在同一把锁内完成；client_msg_id 命中返回原 seq（P3a-03 B-2/B-4）。
func (s *MemoryStore) AppendMessageWithRound(ctx context.Context, m *Message, round *Round) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if m == nil || !validID(string(m.GroupID)) {
		return 0, false, ErrInvalid
	}
	if round != nil && round.GroupID != "" && round.GroupID != m.GroupID {
		return 0, false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cid := messageClientMsgID(*m); cid != "" {
		for i := len(s.messages[m.GroupID]) - 1; i >= 0; i-- {
			existing := s.messages[m.GroupID][i]
			if messageClientMsgID(existing) != cid {
				continue
			}
			if round != nil && !hasRoundReceipt(s.messages[m.GroupID], existing.Seq) {
				if err := s.appendRoundLocked(existing.GroupID, existing.Seq, round); err != nil {
					return 0, false, err
				}
			}
			*m = cloneMessage(existing)
			return existing.Seq, true, nil
		}
	}
	seq := int64(len(s.messages[m.GroupID]) + 1)
	stored := cloneMessage(*m)
	stored.Seq = seq
	stored.ID = messageIDFor(&stored, seq)
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = nowUTC()
	}
	s.messages[m.GroupID] = append(s.messages[m.GroupID], stored)
	if round != nil {
		if err := s.appendRoundLocked(m.GroupID, seq, round); err != nil {
			s.messages[m.GroupID] = s.messages[m.GroupID][:len(s.messages[m.GroupID])-1]
			return 0, false, err
		}
	}
	*m = cloneMessage(stored)
	if round != nil {
		round.TriggerSeq = seq
	}
	return seq, false, nil
}

func (s *MemoryStore) appendRoundLocked(gid GroupID, triggerSeq int64, round *Round) error {
	r := *round
	r.GroupID = gid
	if r.TriggerSeq == 0 {
		r.TriggerSeq = triggerSeq
	}
	if r.ID == "" {
		r.ID = NewID(PrefixRound)
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = nowUTC()
	}
	receiptSeq := int64(len(s.messages[gid]) + 1)
	if receiptSeq <= triggerSeq {
		receiptSeq = triggerSeq + 1
	}
	receipt := Message{
		Seq: receiptSeq, ID: MessageID(receiptSeq), GroupID: gid,
		Kind: KindRouting, Sender: Sender{Kind: SenderSystem},
		Text: r.Receipt().Summary, RoundID: r.ID, CreatedAt: r.CreatedAt,
		Refs: map[string]any{"trigger_seq": triggerSeq},
	}
	s.messages[gid] = append(s.messages[gid], receipt)
	a := r.Receipt()
	a.AfterSeq = receiptSeq
	a.RunID = r.ID
	s.activity[gid] = append(s.activity[gid], a)
	*round = r
	return nil
}

func (s *MemoryStore) ReadMessages(ctx context.Context, gid GroupID, beforeSeq int64, limit int) ([]Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(gid)) {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	msgs := cloneMessages(s.messages[gid])
	s.mu.Unlock()
	return sliceMessages(msgs, beforeSeq, normalizeLimit(limit)), nil
}

func (s *MemoryStore) FindGroupMessageByClientID(ctx context.Context, gid GroupID, clientMsgID string) (*Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(gid)) {
		return nil, ErrInvalid
	}
	clientMsgID = strings.TrimSpace(clientMsgID)
	if clientMsgID == "" {
		return nil, nil
	}
	s.mu.Lock()
	msgs := cloneMessages(s.messages[gid])
	s.mu.Unlock()
	for i := len(msgs) - 1; i >= 0; i-- {
		if id, ok := msgs[i].Refs["client_msg_id"].(string); ok && strings.TrimSpace(id) == clientMsgID {
			m := msgs[i]
			return &m, nil
		}
	}
	return nil, nil
}

func (s *MemoryStore) SearchMessages(ctx context.Context, gid GroupID, query string, beforeSeq int64, limit int) ([]Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(gid)) {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	msgs := cloneMessages(s.messages[gid])
	s.mu.Unlock()
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return sliceMessages(msgs, 0, normalizeLimit(limit)), nil
	}
	var out []Message
	for _, m := range msgs {
		if beforeSeq > 0 && m.Seq >= beforeSeq {
			continue
		}
		if !m.VisibleTo(false) || !m.AgentContextKind() {
			continue
		}
		if strings.Contains(strings.ToLower(m.Text), query) {
			out = append(out, m)
		}
	}
	return tail(out, normalizeLimit(limit)), nil
}

func (s *MemoryStore) AppendActivity(ctx context.Context, a *Activity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a == nil || !validID(string(a.GroupID)) {
		return ErrInvalid
	}
	if a.At.IsZero() {
		a.At = nowUTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activity[a.GroupID] = append(s.activity[a.GroupID], *a)
	return nil
}

func (s *MemoryStore) ReadActivity(ctx context.Context, gid GroupID, afterSeq int64, limit int) ([]Activity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(gid)) {
		return nil, ErrInvalid
	}
	limit = normalizeLimit(limit)
	s.mu.Lock()
	items := append([]Activity(nil), s.activity[gid]...)
	s.mu.Unlock()
	out := make([]Activity, 0, limit)
	for _, a := range items {
		if afterSeq > 0 && a.AfterSeq <= afterSeq {
			continue
		}
		out = append(out, a)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *MemoryStore) LoadTask(ctx context.Context, id TaskID) (*Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(id)) {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneTask(t), nil
}

func (s *MemoryStore) SaveTask(ctx context.Context, t *Task) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t == nil || !validID(string(t.ID)) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[t.ID] = cloneTask(t)
	if s.taskIdx != nil {
		s.taskIdx.put(t)
	}
	return nil
}

func (s *MemoryStore) AppendTaskEvent(ctx context.Context, id TaskID, ev TaskEvent) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !validID(string(id)) {
		return 0, ErrInvalid
	}
	if ev.At.IsZero() {
		ev.At = nowUTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ev.Seq = s.taskSeq[id] + 1
	ev.TaskID = id
	s.taskEvent[id] = append(s.taskEvent[id], cloneTaskEvent(ev))
	s.taskSeq[id] = ev.Seq
	return ev.Seq, nil
}

func (s *MemoryStore) ReadTaskEvents(ctx context.Context, id TaskID) ([]TaskEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(id)) {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TaskEvent, 0, len(s.taskEvent[id]))
	for _, ev := range s.taskEvent[id] {
		out = append(out, cloneTaskEvent(ev))
	}
	return out, nil
}

func (s *MemoryStore) ListTasks(ctx context.Context, filter TaskFilter) ([]*Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		if !matchTask(t, filter) {
			continue
		}
		out = append(out, cloneTask(t))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

// TaskIndex 接口实现：MemoryStore 与 FileStore 同语义（P2.5-02）。
func (s *MemoryStore) TaskIndexReady() bool { return s != nil && s.taskIdx != nil }

func (s *MemoryStore) FindByClientMsgID(target TaskTarget, clientMsgID string) (*Task, bool) {
	if s == nil || s.taskIdx == nil {
		return nil, false
	}
	return s.taskIdx.FindByClientMsgID(target, clientMsgID)
}

func (s *MemoryStore) ActiveCount(agent AgentID) int {
	if s == nil || s.taskIdx == nil {
		return 0
	}
	return s.taskIdx.ActiveCount(agent)
}

// PairActiveCount 实现 PairActiveIndex（与 FileStore 同语义，P3b-03 N1）。
func (s *MemoryStore) PairActiveCount(group GroupID, from, to AgentID) int {
	if s == nil || s.taskIdx == nil {
		return 0
	}
	return s.taskIdx.PairActiveCount(group, from, to)
}

func (s *MemoryStore) DailyCost(agent AgentID, group GroupID, day time.Time) (agentCost, groupCost, globalCost float64) {
	if s == nil || s.taskIdx == nil {
		return 0, 0, 0
	}
	return s.taskIdx.DailyCost(agent, group, day)
}

func (s *MemoryStore) FindDuplicate(t *Task, since time.Time) (TaskID, bool) {
	if s == nil || s.taskIdx == nil {
		return "", false
	}
	return s.taskIdx.FindDuplicate(t, since)
}

func (s *MemoryStore) LoadNeed(ctx context.Context, id NeedID) (*Need, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(id)) {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.needs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneNeed(n), nil
}

func (s *MemoryStore) SaveNeed(ctx context.Context, n *Need) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if n == nil || !validID(string(n.ID)) {
		return ErrInvalid
	}
	n = cloneNeed(n)
	if n.CreatedAt.IsZero() {
		n.CreatedAt = nowUTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.needs[n.ID] = n
	return nil
}

func (s *MemoryStore) ListNeeds(ctx context.Context, filter NeedFilter) ([]*Need, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Need, 0, len(s.needs))
	for _, n := range s.needs {
		if filter.AgentID != "" && n.AgentID != filter.AgentID {
			continue
		}
		if filter.GroupID != "" && n.GroupID != filter.GroupID {
			continue
		}
		if filter.Status != "" && n.Status != filter.Status {
			continue
		}
		out = append(out, cloneNeed(n))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (s *MemoryStore) LoadHandoff(ctx context.Context, id string) (*Handoff, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(id) || !strings.HasPrefix(id, PrefixHandoff+"_") {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.handoffs[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *h
	if h.AckedAt != nil {
		t := *h.AckedAt
		cp.AckedAt = &t
	}
	return &cp, nil
}

func (s *MemoryStore) SaveHandoff(ctx context.Context, h *Handoff) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if h == nil || !validID(h.ID) || !strings.HasPrefix(h.ID, PrefixHandoff+"_") {
		return ErrInvalid
	}
	cp := *h
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = nowUTC()
	}
	if h.AckedAt != nil {
		t := *h.AckedAt
		cp.AckedAt = &t
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handoffs[cp.ID] = &cp
	return nil
}

func (s *MemoryStore) ListHandoffs(ctx context.Context, gid GroupID) ([]*Handoff, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Handoff, 0, len(s.handoffs))
	for _, h := range s.handoffs {
		if gid != "" && h.GroupID != gid {
			continue
		}
		cp := *h
		if h.AckedAt != nil {
			t := *h.AckedAt
			cp.AckedAt = &t
		}
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *MemoryStore) SaveArtifact(ctx context.Context, a *Artifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a == nil || !validID(string(a.ID)) {
		return ErrInvalid
	}
	cp := *a
	now := nowUTC()
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	cp.UpdatedAt = now
	if cp.Version == 0 {
		cp.Version = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.artifacts[cp.ID] = &cp
	return nil
}

func (s *MemoryStore) LoadArtifact(ctx context.Context, id ArtifactID) (*Artifact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(id)) {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.artifacts[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *a
	return &cp, nil
}

func (s *MemoryStore) ListArtifacts(ctx context.Context) ([]*Artifact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Artifact, 0, len(s.artifacts))
	for _, a := range s.artifacts {
		cp := *a
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *MemoryStore) SaveCursor(ctx context.Context, c *Cursor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c == nil || !validID(string(c.GroupID)) || !validID(string(c.AgentID)) {
		return ErrInvalid
	}
	cp := *c
	cp.UpdatedAt = nowUTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cursors[cursorKey(cp.GroupID, cp.AgentID)] = cp
	return nil
}

func (s *MemoryStore) LoadCursor(ctx context.Context, groupID GroupID, agentID AgentID) (*Cursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(groupID)) || !validID(string(agentID)) {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cursors[cursorKey(groupID, agentID)]
	if !ok {
		return &Cursor{GroupID: groupID, AgentID: agentID}, nil
	}
	return &c, nil
}

// AdvanceAgentContextSeq 在 store 锁内读-改-写，只前移 agent 游标并保留人类已读。
func (s *MemoryStore) AdvanceAgentContextSeq(ctx context.Context, groupID GroupID, agentID AgentID, seq int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validID(string(groupID)) || !validID(string(agentID)) || seq <= 0 {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := cursorKey(groupID, agentID)
	c, ok := s.cursors[key]
	if !ok {
		c = Cursor{GroupID: groupID, AgentID: agentID}
	}
	if seq <= c.AgentContextSeq {
		return nil
	}
	c.GroupID = groupID
	c.AgentID = agentID
	c.AgentContextSeq = seq
	c.UpdatedAt = nowUTC()
	s.cursors[key] = c
	return nil
}

func cursorKey(g GroupID, a AgentID) string {
	return string(g) + "\x00" + string(a)
}

func nowUTC() time.Time { return time.Now().UTC() }

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string(nil), in...)
}

// CloneAgentProfile 深拷贝一个 profile（切片字段独立）。
//
// 为什么必须深拷贝：`next := *cur` 只复制 struct，切片的底层数组仍共享；
// 之后 json.Unmarshal 进 next 会**复用**已有底层数组写元素，把 cur（要归档的旧版本）
// 一起改掉 —— 「回滚」的事实源在改工具面/白名单时就是坏的（P1-03 复核实测）。
// 未知/新增切片字段请同步加进来（cloneStrings 保证 nil 与空切片语义一致）。
func CloneAgentProfile(p *AgentProfile) *AgentProfile {
	if p == nil {
		return nil
	}
	return cloneAgentProfile(p)
}

func cloneAgentProfile(p *AgentProfile) *AgentProfile {
	cp := *p
	cp.Tools.Disable = cloneStrings(p.Tools.Disable)
	cp.Tools.Allow = cloneStrings(p.Tools.Allow)
	cp.Tools.BashRoots = cloneStrings(p.Tools.BashRoots)
	cp.Tools.MCP.DisableServers = cloneStrings(p.Tools.MCP.DisableServers)
	cp.Tools.MCP.DisableTools = cloneStrings(p.Tools.MCP.DisableTools)
	cp.Tools.MCP.AllowTools = cloneStrings(p.Tools.MCP.AllowTools)
	cp.Policy.WorkspaceRoots = cloneStrings(p.Policy.WorkspaceRoots)
	return &cp
}

func cloneGroup(g *Group) *Group {
	cp := *g
	cp.Members = append([]Member(nil), g.Members...)
	return &cp
}

func cloneMessage(m Message) Message {
	if m.Context != nil {
		ctx := *m.Context
		ctx.TriggerSeqs = append([]int64(nil), m.Context.TriggerSeqs...)
		ctx.Senders = append([]AgentRef(nil), m.Context.Senders...)
		m.Context = &ctx
	}
	m.Mentions = append([]AgentID(nil), m.Mentions...)
	if m.Refs != nil {
		refs := make(map[string]any, len(m.Refs))
		for k, v := range m.Refs {
			refs[k] = cloneAnyValue(v)
		}
		m.Refs = refs
	}
	return m
}

func cloneMessages(in []Message) []Message {
	if in == nil {
		return nil
	}
	out := make([]Message, len(in))
	for i, m := range in {
		out[i] = cloneMessage(m)
	}
	return out
}

func cloneTask(t *Task) *Task {
	cp := *t
	cp.Chain = append([]TaskID(nil), t.Chain...)
	cp.ArtifactIDs = append([]string(nil), t.ArtifactIDs...)
	cp.NotifiedRuns = cloneStrings(t.NotifiedRuns)
	cp.SupersededRuns = cloneStrings(t.SupersededRuns)
	cp.Dispatch = cloneDispatch(t.Dispatch)
	if t.Result != nil {
		result := *t.Result
		result.Artifacts = append([]TaskArtifact(nil), t.Result.Artifacts...)
		cp.Result = &result
	}
	return &cp
}

func cloneDispatch(d Dispatch) Dispatch {
	d.DispatchedAt = cloneTimePtr(d.DispatchedAt)
	d.AcceptedAt = cloneTimePtr(d.AcceptedAt)
	d.AgentStartAt = cloneTimePtr(d.AgentStartAt)
	d.AgentEndAt = cloneTimePtr(d.AgentEndAt)
	return d
}

func cloneTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

func cloneNeed(n *Need) *Need {
	cp := *n
	cp.ResolvedAt = cloneTimePtr(n.ResolvedAt)
	return &cp
}

func cloneTaskEvent(ev TaskEvent) TaskEvent {
	if ev.Data != nil {
		data := make(map[string]any, len(ev.Data))
		for k, v := range ev.Data {
			data[k] = cloneAnyValue(v)
		}
		ev.Data = data
	}
	return ev
}

// cloneAnyValue 递归复制 JSON 形状的 map/切片；标量不可变，直接返回。
func cloneAnyValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = cloneAnyValue(v)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = cloneAnyValue(v)
		}
		return out
	case []string:
		return append([]string(nil), x...)
	case map[string]string:
		out := make(map[string]string, len(x))
		for k, v := range x {
			out[k] = v
		}
		return out
	default:
		return v
	}
}
