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
	mu        sync.Mutex
	agents    map[AgentID]*AgentProfile
	groups    map[GroupID]*Group
	messages  map[GroupID][]Message
	activity  map[GroupID][]Activity
	tasks     map[TaskID]*Task
	taskEvent map[TaskID][]TaskEvent
	taskSeq   map[TaskID]int64
	needs     map[NeedID]*Need
	artifacts map[ArtifactID]*Artifact
	cursors   map[string]Cursor
	reserved  map[string]struct{}
	idGen     func(prefix string) string
}

var (
	_ Store = (*FileStore)(nil)
	_ Store = (*MemoryStore)(nil)
)

// NewMemoryStore 创建空的内存 Store。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		agents:    make(map[AgentID]*AgentProfile),
		groups:    make(map[GroupID]*Group),
		messages:  make(map[GroupID][]Message),
		activity:  make(map[GroupID][]Activity),
		tasks:     make(map[TaskID]*Task),
		taskEvent: make(map[TaskID][]TaskEvent),
		taskSeq:   make(map[TaskID]int64),
		needs:     make(map[NeedID]*Need),
		artifacts: make(map[ArtifactID]*Artifact),
		cursors:   make(map[string]Cursor),
		reserved:  make(map[string]struct{}),
		idGen:     NewID,
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
	return cloneGroup(g), nil
}

func (s *MemoryStore) SaveGroup(ctx context.Context, g *Group) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if g == nil || !validID(string(g.ID)) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groups[g.ID] = cloneGroup(g)
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
		out = append(out, cloneGroup(g))
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
	m.ID = MessageID(seq)
	if m.CreatedAt.IsZero() {
		m.CreatedAt = nowUTC()
	}
	s.messages[m.GroupID] = append(s.messages[m.GroupID], cloneMessage(*m))
	return seq, nil
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

func (s *MemoryStore) SearchMessages(ctx context.Context, gid GroupID, query string, limit int) ([]Message, error) {
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
	cp.NotifiedRuns = cloneStrings(t.NotifiedRuns)
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
