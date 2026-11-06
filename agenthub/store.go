package agenthub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNotFound / ErrInvalid / ErrConflict / ErrCorrupt / ErrUnsupportedSchema / ErrTooLarge
// 是 Store 层对调用方的稳定错误分类；上层用 errors.Is 判断，不解析字符串。
var (
	ErrNotFound          = errors.New("agenthub: not found")
	ErrInvalid           = errors.New("agenthub: invalid")
	ErrConflict          = errors.New("agenthub: conflict")
	ErrCorrupt           = errors.New("agenthub: corrupt")
	ErrUnsupportedSchema = errors.New("agenthub: unsupported schema")
	// ErrForbidden 信任边界拒绝（路径不在允许根内、命中敏感目录等）。
	// 与 ErrInvalid 分开：这不是「参数写错」，而是「这个参数合法但你没权限」。
	ErrForbidden = errors.New("agenthub: forbidden")
	ErrTooLarge  = errors.New("agenthub: too large")
)

// Store 是 Hub 的持久化接口。领域规则不依赖具体实现；FileStore 落盘，MemoryStore 供测试/评测。
//
// 契约：
//   - 实体缺失（LoadAgent/LoadTask/...）返回 ErrNotFound；
//   - 集合缺失（List*/ReadMessages/ReadActivity/ReadTaskEvents）返回空 slice，不返回错误；
//   - 游标缺失返回零值 *Cursor（GroupID/AgentID 已填），不返回 ErrNotFound；
//   - Store 是哑持久化层：名字/角色/状态迁移等领域不变量由 Service 校验；
//   - AppendMessage 只接收群日志（GroupID 非空）；DM 走 agent session 存储。
type Store interface {
	// ID 分配：6 位 base32 有碰撞概率，必须检查目标文件/索引后重试。
	AllocateID(ctx context.Context, prefix string) (string, error)

	LoadAgent(ctx context.Context, id AgentID) (*AgentProfile, error)
	SaveAgent(ctx context.Context, p *AgentProfile) error
	ListAgents(ctx context.Context) ([]*AgentProfile, error)

	LoadGroup(ctx context.Context, id GroupID) (*Group, error)
	SaveGroup(ctx context.Context, g *Group) error
	ListGroups(ctx context.Context) ([]*Group, error)

	AppendMessage(ctx context.Context, m *Message) (int64, error)
	// AppendMessageWithRound 在同一个临界区内追加触发消息、routing_receipt 消息与
	// routing activity；client_msg_id 命中时返回原 seq 并置 deduped（P3a-03 B-2/B-4）。
	AppendMessageWithRound(ctx context.Context, m *Message, round *Round) (seq int64, deduped bool, err error)
	ReadMessages(ctx context.Context, gid GroupID, beforeSeq int64, limit int) ([]Message, error)
	FindGroupMessageByClientID(ctx context.Context, gid GroupID, clientMsgID string) (*Message, error)
	SearchMessages(ctx context.Context, gid GroupID, query string, beforeSeq int64, limit int) ([]Message, error)

	AppendActivity(ctx context.Context, a *Activity) error
	ReadActivity(ctx context.Context, gid GroupID, afterSeq int64, limit int) ([]Activity, error)

	LoadTask(ctx context.Context, id TaskID) (*Task, error)
	SaveTask(ctx context.Context, t *Task) error
	AppendTaskEvent(ctx context.Context, id TaskID, ev TaskEvent) (int64, error)
	ReadTaskEvents(ctx context.Context, id TaskID) ([]TaskEvent, error)
	ListTasks(ctx context.Context, filter TaskFilter) ([]*Task, error)

	LoadNeed(ctx context.Context, id NeedID) (*Need, error)
	SaveNeed(ctx context.Context, n *Need) error
	ListNeeds(ctx context.Context, filter NeedFilter) ([]*Need, error)

	LoadHandoff(ctx context.Context, id string) (*Handoff, error)
	SaveHandoff(ctx context.Context, h *Handoff) error
	ListHandoffs(ctx context.Context, gid GroupID) ([]*Handoff, error)

	SaveArtifact(ctx context.Context, a *Artifact) error
	LoadArtifact(ctx context.Context, id ArtifactID) (*Artifact, error)

	SaveCursor(ctx context.Context, c *Cursor) error
	LoadCursor(ctx context.Context, groupID GroupID, agentID AgentID) (*Cursor, error)
}

const (
	defaultMessageLimit = 50
	maxMessageLimit     = 500
	maxJSONLLineBytes   = 8 << 20
)

type hubMeta struct {
	SchemaVersion int       `json:"schema_version"`
	CreatedAt     time.Time `json:"created_at"`
}

// FileStore 是 Store 的文件系统实现。
//
// 布局：
//
//	hub.json
//	agents/<id>/profile.json
//	groups/<gid>/group.json
//	groups/<gid>/log.jsonl
//	groups/<gid>/activity.jsonl
//	tasks/<id>/task.json
//	tasks/<id>/events.jsonl
//	needs/<id>.json
//	artifacts/<id>/artifact.json
//	cursors/<gid>/<agent>.json
//
// 实体用 tmp+rename 原子写；日志用 O_APPEND 单行追加并 fsync。
// FileStore 假定同一 root 只有一个写者进程（bridge）；不做跨进程 flock，多开会得到未定义结果。
type FileStore struct {
	root string

	mu       sync.Mutex // 保护 seq / events 索引 / ID 预留与日志读写的可见性
	seq      map[GroupID]int64
	dmSeq    map[AgentID]int64 // DM 索引的 per-agent 游标（§6.1.3）
	taskSeq  map[TaskID]int64
	reserved map[string]struct{}
	idGen    func(prefix string) string
}

// OpenFileStore 打开/创建 root，校验 schema，并从日志重建 seq。
func OpenFileStore(root string) (*FileStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("%w: empty root", ErrInvalid)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("%w: root: %v", ErrInvalid, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create store root: %w", err)
	}

	metaPath := filepath.Join(abs, "hub.json")
	var meta hubMeta
	err = readJSON(metaPath, &meta)
	switch {
	case errors.Is(err, ErrNotFound):
		meta = hubMeta{SchemaVersion: CurrentSchemaVersion, CreatedAt: time.Now().UTC()}
		if err := writeJSONAtomic(metaPath, &meta); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case meta.SchemaVersion > CurrentSchemaVersion:
		return nil, fmt.Errorf("%w: got %d want <= %d", ErrUnsupportedSchema, meta.SchemaVersion, CurrentSchemaVersion)
	case meta.SchemaVersion < CurrentSchemaVersion:
		if err := migrateSchema(abs, meta.SchemaVersion); err != nil {
			return nil, err
		}
		meta.SchemaVersion = CurrentSchemaVersion
		if meta.CreatedAt.IsZero() {
			meta.CreatedAt = time.Now().UTC()
		}
		if err := writeJSONAtomic(metaPath, &meta); err != nil {
			return nil, err
		}
	}

	fs := &FileStore{
		root:     abs,
		seq:      make(map[GroupID]int64),
		dmSeq:    make(map[AgentID]int64),
		taskSeq:  make(map[TaskID]int64),
		reserved: make(map[string]struct{}),
		idGen:    NewID,
	}
	if err := fs.rebuildIndex(); err != nil {
		return nil, err
	}
	return fs, nil
}

// Root 返回绝对路径（测试/诊断用）。
func (s *FileStore) Root() string { return s.root }

func migrateSchema(root string, from int) error {
	// v0 是“没有 meta/试验版”：目录结构已经是 v1 兼容，补齐 meta 即可。
	if from == 0 {
		return nil
	}
	return fmt.Errorf("%w: no migration from %d to %d", ErrUnsupportedSchema, from, CurrentSchemaVersion)
}

// rebuildIndex 从 append-only 日志恢复内存索引：
//   - 每个群的 log.jsonl -> 下一个消息 seq；
//   - 每个任务的 events.jsonl -> 下一个事件 seq。
//
// 快照 JSON（task.json/group.json）是实体权威值；日志只负责序号与审计，二者不互相覆盖。
func (s *FileStore) rebuildIndex() error {
	matches, err := filepath.Glob(filepath.Join(s.root, "groups", "*", "log.jsonl"))
	if err != nil {
		return err
	}
	for _, path := range matches {
		gid := GroupID(filepath.Base(filepath.Dir(path)))
		msgs, err := readJSONLTolerantTail[Message](path)
		if err != nil {
			return err
		}
		var max int64
		for _, m := range msgs {
			if m.Seq > max {
				max = m.Seq
			}
		}
		s.seq[gid] = max
	}

	// activity.jsonl 也要在启动时截断半行：否则重启后「未读先写」会把新记录
	// 粘在半行后面，读侧再 tolerant 也救不回（P3a-02 复审 N-1 残留）。
	activityMatches, err := filepath.Glob(filepath.Join(s.root, "groups", "*", "activity.jsonl"))
	if err != nil {
		return err
	}
	for _, path := range activityMatches {
		if _, err := readJSONLTolerantTail[Activity](path); err != nil {
			return err
		}
	}

	dmMatches, err := filepath.Glob(filepath.Join(s.root, "dms", "*", "log.jsonl"))
	if err != nil {
		return err
	}
	for _, path := range dmMatches {
		aid := AgentID(filepath.Base(filepath.Dir(path)))
		msgs, err := readJSONLTolerantTail[Message](path)
		if err != nil {
			return err
		}
		var max int64
		for _, m := range msgs {
			if m.Seq > max {
				max = m.Seq
			}
		}
		s.dmSeq[aid] = max
	}

	eventMatches, err := filepath.Glob(filepath.Join(s.root, "tasks", "*", "events.jsonl"))
	if err != nil {
		return err
	}
	for _, path := range eventMatches {
		tid := TaskID(filepath.Base(filepath.Dir(path)))
		events, err := readJSONLTolerantTail[TaskEvent](path)
		if err != nil {
			return err
		}
		var max int64
		for _, ev := range events {
			if ev.Seq > max {
				max = ev.Seq
			}
		}
		s.taskSeq[tid] = max
	}
	return nil
}

func (s *FileStore) AllocateID(ctx context.Context, prefix string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Round ID 由上层按轮次生成，不进 Store；reserved 只在进程生命周期内增长，不回收。
	if !knownPrefix(prefix) || prefix == PrefixRound {
		return "", fmt.Errorf("%w: prefix %q", ErrInvalid, prefix)
	}
	for i := 0; i < 16; i++ {
		id := s.idGen(prefix)
		s.mu.Lock()
		if _, used := s.reserved[id]; used {
			s.mu.Unlock()
			continue
		}
		exists, err := s.idExists(prefix, id)
		if err != nil {
			s.mu.Unlock()
			return "", err
		}
		if !exists {
			// 预留到进程退出：同一进程并发分配不会拿到同一个 ID。
			s.reserved[id] = struct{}{}
			s.mu.Unlock()
			return id, nil
		}
		s.mu.Unlock()
	}
	return "", fmt.Errorf("%w: id collision for prefix %q", ErrConflict, prefix)
}

func (s *FileStore) LoadAgent(ctx context.Context, id AgentID) (*AgentProfile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(id)) {
		return nil, fmt.Errorf("%w: agent id %q", ErrInvalid, id)
	}
	var p AgentProfile
	if err := readJSON(s.agentPath(id), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *FileStore) SaveAgent(ctx context.Context, p *AgentProfile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || !validID(string(p.ID)) {
		return fmt.Errorf("%w: agent", ErrInvalid)
	}
	return writeJSONAtomic(s.agentPath(p.ID), p)
}

func (s *FileStore) ListAgents(ctx context.Context) ([]*AgentProfile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(s.root, "agents", "*", "profile.json"))
	if err != nil {
		return nil, err
	}
	out := make([]*AgentProfile, 0, len(matches))
	for _, path := range matches {
		var p AgentProfile
		if err := readJSON(path, &p); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *FileStore) LoadGroup(ctx context.Context, id GroupID) (*Group, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(id)) {
		return nil, fmt.Errorf("%w: group id %q", ErrInvalid, id)
	}
	var g Group
	if err := readJSON(s.groupPath(id), &g); err != nil {
		return nil, err
	}
	return &g, nil
}

func (s *FileStore) SaveGroup(ctx context.Context, g *Group) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if g == nil || !validID(string(g.ID)) {
		return fmt.Errorf("%w: group", ErrInvalid)
	}
	return writeJSONAtomic(s.groupPath(g.ID), g)
}

func (s *FileStore) ListGroups(ctx context.Context) ([]*Group, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(s.root, "groups", "*", "group.json"))
	if err != nil {
		return nil, err
	}
	out := make([]*Group, 0, len(matches))
	for _, path := range matches {
		var g Group
		if err := readJSON(path, &g); err != nil {
			return nil, err
		}
		out = append(out, &g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *FileStore) AppendMessage(ctx context.Context, m *Message) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if m == nil || !validID(string(m.GroupID)) {
		return 0, fmt.Errorf("%w: message group", ErrInvalid)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	seq := s.seq[m.GroupID] + 1
	// 成功落盘后才改调用方对象；失败时不能让它带着一个没写进 log 的 seq。
	stored := *m
	stored.Seq = seq
	// 消息 ID 是 seq 的确定函数；忽略调用方传入值，避免写出不符合协议的 ID。
	stored.ID = MessageID(seq)
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = time.Now().UTC()
	}
	if err := appendJSONL(s.messagesPath(m.GroupID), &stored); err != nil {
		return 0, err
	}
	s.seq[m.GroupID] = seq
	*m = stored
	return seq, nil
}

// AppendMessageWithRound 原子追加一条群消息及其路由回执。
//
// 三件事必须在同一把 s.mu 下完成，否则并发 group_say 会让 activity 的 after_seq
// 乱序（P3a-03 B-4）；client_msg_id 去重也必须在锁内，否则同 id 重试双写（B-2）。
// 回执写失败时把 log.jsonl 截回写入前，避免留下没有回执的半条消息。
func (s *FileStore) AppendMessageWithRound(ctx context.Context, m *Message, round *Round) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if m == nil || !validID(string(m.GroupID)) {
		return 0, false, fmt.Errorf("%w: message group", ErrInvalid)
	}
	if round != nil && round.GroupID != "" && round.GroupID != m.GroupID {
		return 0, false, fmt.Errorf("%w: round group %q != message group %q", ErrInvalid, round.GroupID, m.GroupID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	logPath := s.messagesPath(m.GroupID)
	if cid := messageClientMsgID(*m); cid != "" {
		// 只有带幂等键才需要回看日志；普通发送不付全量重读的成本（P3a-03 复审 NB-4）。
		msgs, err := readJSONLTolerantTail[Message](logPath)
		if err != nil {
			return 0, false, err
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			if messageClientMsgID(msgs[i]) != cid {
				continue
			}
			existing := msgs[i]
			// 上次可能已写消息但回执失败；重试时补齐回执，仍返回原 seq。
			if round != nil && !hasRoundReceipt(msgs, existing.Seq) {
				beforeSize, err := regularFileSize(logPath)
				if err != nil {
					return 0, false, err
				}
				if err := s.appendRoundLocked(existing.GroupID, existing.Seq, round); err != nil {
					// 二次失败也不能留下没有 activity 的孤儿回执（复审 NB-1）。
					if truncErr := os.Truncate(logPath, beforeSize); truncErr != nil {
						return 0, false, fmt.Errorf("heal receipt: %v; rollback: %w", err, truncErr)
					}
					return 0, false, err
				}
			}
			*m = existing
			return existing.Seq, true, nil
		}
	}

	beforeSize, err := regularFileSize(logPath)
	if err != nil {
		return 0, false, err
	}
	seq := s.seq[m.GroupID] + 1
	stored := *m
	stored.Seq = seq
	stored.ID = MessageID(seq)
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = time.Now().UTC()
	}
	if err := appendJSONL(logPath, &stored); err != nil {
		return 0, false, err
	}
	s.seq[m.GroupID] = seq
	if round != nil {
		if err := s.appendRoundLocked(m.GroupID, seq, round); err != nil {
			// 回执失败：整条触发消息回滚，调用方重试不会留下半条。
			// 截断失败时保留 seq（消息仍在日志里），否则下一条会复用同一 seq（复审 NB-6）。
			if truncErr := os.Truncate(logPath, beforeSize); truncErr != nil {
				return 0, false, fmt.Errorf("round receipt: %v; rollback: %w", err, truncErr)
			}
			s.seq[m.GroupID] = seq - 1
			return 0, false, err
		}
	}
	*m = stored
	if round != nil {
		round.TriggerSeq = seq
	}
	return seq, false, nil
}

// appendRoundLocked 写入 KindRouting 消息 + routing activity（调用方持 s.mu）。
// activity.AfterSeq 用回执消息的 seq，保证读方用「最新消息 seq」当游标时不会漏。
func (s *FileStore) appendRoundLocked(gid GroupID, triggerSeq int64, round *Round) error {
	r := *round
	r.GroupID = gid
	if r.TriggerSeq == 0 {
		r.TriggerSeq = triggerSeq
	}
	if r.ID == "" {
		r.ID = NewID(PrefixRound)
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	receiptSeq := s.seq[gid] + 1
	if receiptSeq <= triggerSeq {
		receiptSeq = triggerSeq + 1
	}
	receipt := Message{
		Seq: receiptSeq, ID: MessageID(receiptSeq), GroupID: gid,
		Kind: KindRouting, Sender: Sender{Kind: SenderSystem},
		Text: r.Receipt().Summary, RoundID: r.ID, CreatedAt: r.CreatedAt,
		Refs: map[string]any{"trigger_seq": triggerSeq},
	}
	if err := appendJSONL(s.messagesPath(gid), &receipt); err != nil {
		return err
	}
	a := r.Receipt()
	a.AfterSeq = receiptSeq
	a.RunID = r.ID
	if err := s.appendActivityLocked(&a); err != nil {
		// 回执消息由调用方负责截断 log；activity 自己不会留下半行。
		return err
	}
	s.seq[gid] = receiptSeq
	*round = r
	return nil
}

func (s *FileStore) ReadMessages(ctx context.Context, gid GroupID, beforeSeq int64, limit int) ([]Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(gid)) {
		return nil, fmt.Errorf("%w: group id %q", ErrInvalid, gid)
	}
	limit = normalizeLimit(limit)

	s.mu.Lock()
	msgs, err := readJSONL[Message](s.messagesPath(gid))
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return sliceMessages(msgs, beforeSeq, limit), nil
}

func (s *FileStore) FindGroupMessageByClientID(ctx context.Context, gid GroupID, clientMsgID string) (*Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(gid)) {
		return nil, fmt.Errorf("%w: group id %q", ErrInvalid, gid)
	}
	clientMsgID = strings.TrimSpace(clientMsgID)
	if clientMsgID == "" {
		return nil, nil
	}
	s.mu.Lock()
	msgs, err := readJSONL[Message](s.messagesPath(gid))
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if id, ok := msgs[i].Refs["client_msg_id"].(string); ok && strings.TrimSpace(id) == clientMsgID {
			m := msgs[i]
			return &m, nil
		}
	}
	return nil, nil
}

func (s *FileStore) SearchMessages(ctx context.Context, gid GroupID, query string, beforeSeq int64, limit int) ([]Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(gid)) {
		return nil, fmt.Errorf("%w: group id %q", ErrInvalid, gid)
	}
	limit = normalizeLimit(limit)

	s.mu.Lock()
	msgs, err := readJSONL[Message](s.messagesPath(gid))
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	query = strings.TrimSpace(strings.ToLower(query))
	if query == "" {
		return sliceMessages(msgs, 0, limit), nil
	}
	matches := make([]Message, 0)
	for _, m := range msgs {
		if beforeSeq > 0 && m.Seq >= beforeSeq {
			continue
		}
		// 搜索也必须按可见性过滤：否则 agent 可用 query 布尔探测 owner-only 正文。
		if !m.VisibleTo(false) {
			continue
		}
		if strings.Contains(strings.ToLower(m.Text), query) {
			matches = append(matches, m)
		}
	}
	return tail(matches, limit), nil
}

func (s *FileStore) AppendActivity(ctx context.Context, a *Activity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a == nil || !validID(string(a.GroupID)) {
		return fmt.Errorf("%w: activity group", ErrInvalid)
	}
	cp := *a
	if cp.At.IsZero() {
		cp.At = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendActivityLocked(&cp); err != nil {
		return err
	}
	*a = cp
	return nil
}

// appendActivityLocked 追加活动行（调用方持 s.mu）。
func (s *FileStore) appendActivityLocked(a *Activity) error {
	return appendJSONL(s.activityPath(a.GroupID), a)
}

func (s *FileStore) ReadActivity(ctx context.Context, gid GroupID, afterSeq int64, limit int) ([]Activity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(gid)) {
		return nil, fmt.Errorf("%w: group id %q", ErrInvalid, gid)
	}
	limit = normalizeLimit(limit)
	s.mu.Lock()
	// 与 log.jsonl 同口径：崩溃留下的半行在读取时截断，否则后续追加会粘在
	// 半行后面，整条活动流永久 ErrCorrupt（P3a-02 复审 N-1）。
	items, err := readJSONLTolerantTail[Activity](s.activityPath(gid))
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
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

func (s *FileStore) LoadTask(ctx context.Context, id TaskID) (*Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(id)) {
		return nil, fmt.Errorf("%w: task id %q", ErrInvalid, id)
	}
	var t Task
	if err := readJSON(s.taskPath(id), &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *FileStore) SaveTask(ctx context.Context, t *Task) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t == nil || !validID(string(t.ID)) {
		return fmt.Errorf("%w: task", ErrInvalid)
	}
	return writeJSONAtomic(s.taskPath(t.ID), t)
}

func (s *FileStore) AppendTaskEvent(ctx context.Context, id TaskID, ev TaskEvent) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !validID(string(id)) {
		return 0, fmt.Errorf("%w: task id %q", ErrInvalid, id)
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ev.Seq = s.taskSeq[id] + 1
	ev.TaskID = id
	if err := appendJSONL(s.taskEventsPath(id), &ev); err != nil {
		return 0, err
	}
	s.taskSeq[id] = ev.Seq
	return ev.Seq, nil
}

func (s *FileStore) ReadTaskEvents(ctx context.Context, id TaskID) ([]TaskEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(id)) {
		return nil, fmt.Errorf("%w: task id %q", ErrInvalid, id)
	}
	s.mu.Lock()
	events, err := readJSONL[TaskEvent](s.taskEventsPath(id))
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return events, nil
}

func (s *FileStore) ListTasks(ctx context.Context, filter TaskFilter) ([]*Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(s.root, "tasks", "*", "task.json"))
	if err != nil {
		return nil, err
	}
	out := make([]*Task, 0, len(matches))
	for _, path := range matches {
		var t Task
		if err := readJSON(path, &t); err != nil {
			return nil, err
		}
		if !matchTask(&t, filter) {
			continue
		}
		out = append(out, &t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

func (s *FileStore) LoadNeed(ctx context.Context, id NeedID) (*Need, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(id)) {
		return nil, fmt.Errorf("%w: need id %q", ErrInvalid, id)
	}
	var n Need
	if err := readJSON(s.needPath(id), &n); err != nil {
		return nil, err
	}
	return &n, nil
}

func (s *FileStore) SaveNeed(ctx context.Context, n *Need) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if n == nil || !validID(string(n.ID)) {
		return fmt.Errorf("%w: need", ErrInvalid)
	}
	cp := *n
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	return writeJSONAtomic(s.needPath(cp.ID), &cp)
}

func (s *FileStore) ListNeeds(ctx context.Context, filter NeedFilter) ([]*Need, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(s.root, "needs", "*.json"))
	if err != nil {
		return nil, err
	}
	out := make([]*Need, 0, len(matches))
	for _, path := range matches {
		var n Need
		if err := readJSON(path, &n); err != nil {
			return nil, err
		}
		if filter.AgentID != "" && n.AgentID != filter.AgentID {
			continue
		}
		if filter.GroupID != "" && n.GroupID != filter.GroupID {
			continue
		}
		if filter.Status != "" && n.Status != filter.Status {
			continue
		}
		out = append(out, &n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (s *FileStore) LoadHandoff(ctx context.Context, id string) (*Handoff, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(id) || !strings.HasPrefix(id, PrefixHandoff+"_") {
		return nil, fmt.Errorf("%w: handoff id %q", ErrInvalid, id)
	}
	var h Handoff
	if err := readJSON(s.handoffPath(id), &h); err != nil {
		return nil, err
	}
	return &h, nil
}

func (s *FileStore) SaveHandoff(ctx context.Context, h *Handoff) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if h == nil || !validID(h.ID) || !strings.HasPrefix(h.ID, PrefixHandoff+"_") {
		return fmt.Errorf("%w: handoff", ErrInvalid)
	}
	cp := *h
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	return writeJSONAtomic(s.handoffPath(cp.ID), &cp)
}

func (s *FileStore) ListHandoffs(ctx context.Context, gid GroupID) ([]*Handoff, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(s.root, "handoffs", "*.json"))
	if err != nil {
		return nil, err
	}
	out := make([]*Handoff, 0, len(matches))
	for _, path := range matches {
		var h Handoff
		if err := readJSON(path, &h); err != nil {
			return nil, err
		}
		if gid != "" && h.GroupID != gid {
			continue
		}
		out = append(out, &h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *FileStore) SaveArtifact(ctx context.Context, a *Artifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a == nil || !validID(string(a.ID)) {
		return fmt.Errorf("%w: artifact", ErrInvalid)
	}
	cp := *a
	now := time.Now().UTC()
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	cp.UpdatedAt = now
	if cp.Version == 0 {
		cp.Version = 1
	}
	return writeJSONAtomic(s.artifactPath(cp.ID), &cp)
}

// ListArtifacts 扫描产物目录（FileStore 专用：Store 接口保持薄，版本计算在领域层）。
func (s *FileStore) ListArtifacts(ctx context.Context) ([]*Artifact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(s.root, "artifacts", "*", "artifact.json"))
	if err != nil {
		return nil, err
	}
	out := make([]*Artifact, 0, len(matches))
	for _, path := range matches {
		var a Artifact
		if err := readJSON(path, &a); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, nil
}

func (s *FileStore) LoadArtifact(ctx context.Context, id ArtifactID) (*Artifact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(id)) {
		return nil, fmt.Errorf("%w: artifact id %q", ErrInvalid, id)
	}
	var a Artifact
	if err := readJSON(s.artifactPath(id), &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *FileStore) SaveCursor(ctx context.Context, c *Cursor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c == nil || !validID(string(c.GroupID)) || !validID(string(c.AgentID)) {
		return fmt.Errorf("%w: cursor", ErrInvalid)
	}
	cp := *c
	cp.UpdatedAt = time.Now().UTC()
	return writeJSONAtomic(s.cursorPath(cp.GroupID, cp.AgentID), &cp)
}

func (s *FileStore) LoadCursor(ctx context.Context, groupID GroupID, agentID AgentID) (*Cursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(groupID)) || !validID(string(agentID)) {
		return nil, fmt.Errorf("%w: cursor key", ErrInvalid)
	}
	var c Cursor
	if err := readJSON(s.cursorPath(groupID, agentID), &c); err != nil {
		if errors.Is(err, ErrNotFound) {
			return &Cursor{GroupID: groupID, AgentID: agentID}, nil
		}
		return nil, err
	}
	return &c, nil
}

func (s *FileStore) idExists(prefix, id string) (bool, error) {
	var path string
	switch prefix {
	case PrefixAgent:
		path = filepath.Dir(s.agentPath(AgentID(id)))
	case PrefixGroup:
		path = filepath.Dir(s.groupPath(GroupID(id)))
	case PrefixTask:
		path = filepath.Dir(s.taskPath(TaskID(id)))
	case PrefixNeed:
		path = s.needPath(NeedID(id))
	case PrefixHandoff:
		path = s.handoffPath(id)
	case PrefixArtifact:
		path = filepath.Dir(s.artifactPath(ArtifactID(id)))
	default:
		return false, fmt.Errorf("%w: prefix %q", ErrInvalid, prefix)
	}
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (s *FileStore) agentPath(id AgentID) string {
	return filepath.Join(s.root, "agents", string(id), "profile.json")
}
func (s *FileStore) groupPath(id GroupID) string {
	return filepath.Join(s.root, "groups", string(id), "group.json")
}
func (s *FileStore) messagesPath(id GroupID) string {
	return filepath.Join(s.root, "groups", string(id), "log.jsonl")
}
func (s *FileStore) activityPath(id GroupID) string {
	return filepath.Join(s.root, "groups", string(id), "activity.jsonl")
}
func (s *FileStore) taskPath(id TaskID) string {
	return filepath.Join(s.root, "tasks", string(id), "task.json")
}
func (s *FileStore) taskEventsPath(id TaskID) string {
	return filepath.Join(s.root, "tasks", string(id), "events.jsonl")
}
func (s *FileStore) needPath(id NeedID) string {
	return filepath.Join(s.root, "needs", string(id)+".json")
}
func (s *FileStore) handoffPath(id string) string {
	return filepath.Join(s.root, "handoffs", id+".json")
}
func (s *FileStore) artifactPath(id ArtifactID) string {
	return filepath.Join(s.root, "artifacts", string(id), "artifact.json")
}
func (s *FileStore) cursorPath(gid GroupID, aid AgentID) string {
	return filepath.Join(s.root, "cursors", string(gid), string(aid)+".json")
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	// rename 的可见性已原子；再 fsync 父目录，降低掉电后目录项丢失的概率。
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrCorrupt, path, err)
	}
	return nil
}

func messageClientMsgID(m Message) string {
	if m.Refs == nil {
		return ""
	}
	id, _ := m.Refs["client_msg_id"].(string)
	return strings.TrimSpace(id)
}

func hasRoundReceipt(msgs []Message, triggerSeq int64) bool {
	for _, m := range msgs {
		if m.Kind != KindRouting || m.Refs == nil {
			continue
		}
		switch seq := m.Refs["trigger_seq"].(type) {
		case float64:
			if int64(seq) == triggerSeq {
				return true
			}
		case int64:
			if seq == triggerSeq {
				return true
			}
		case int:
			if int64(seq) == triggerSeq {
				return true
			}
		}
	}
	return false
}

func regularFileSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func appendJSONL(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	data = append(data, '\n')
	if len(data) > maxJSONLLineBytes {
		return fmt.Errorf("%w: %s line %d > %d", ErrTooLarge, path, len(data), maxJSONLLineBytes)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	before, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return err
	}
	rollback := func() {
		// 短写/fsync 失败时把文件截回写入前，避免残片与下一行粘连成永久坏行。
		_ = f.Truncate(before)
		_ = f.Sync()
		_ = f.Close()
	}
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		rollback()
		return err
	}
	// Sync 已成功；Close 的迟到错误不改变“数据已持久化”的事实，
	// 不能让调用方以为没写而复用同一个 seq。
	_ = f.Close()
	return nil
}

// readJSONL 读取 JSONL；不限制单行长度（写入侧由 appendJSONL 的 maxJSONLLineBytes 兜底）。
// 中间行损坏返回 ErrCorrupt；最后一行不完整由 readJSONLTolerantTail 在启动时截断。
func readJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	var out []T
	line := 0
	for {
		chunk, readErr := reader.ReadBytes('\n')
		if len(chunk) > 0 {
			line++
			raw := bytes.TrimSpace(chunk)
			if len(raw) > 0 {
				var v T
				if err := json.Unmarshal(raw, &v); err != nil {
					return nil, fmt.Errorf("%w: %s:%d: %v", ErrCorrupt, path, line, err)
				}
				out = append(out, v)
			}
		}
		if readErr == io.EOF {
			return out, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

// readJSONLTolerantTail 与 readJSONL 相同，但只容忍“最后一行没有换行且 JSON 不完整”——
// 崩溃/断电最可能留下的就是这种半行。它把文件截断到最后一个完整行并 fsync；
// 中间行损坏仍然返回 ErrCorrupt，不能静默跳过。
func readJSONLTolerantTail[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	var out []T
	var offset int64
	line := 0
	for {
		chunk, readErr := reader.ReadBytes('\n')
		if len(chunk) > 0 {
			line++
			// 正常写入一定以换行结尾；EOF 处缺换行的末行说明那次追加没有完整落盘
			// （哪怕 JSON 恰好能解析），保留它会让下一次 O_APPEND 直接粘在它后面。
			// 这条记录未被 ack，丢弃是安全的；截断到本行起点。
			missingNewline := readErr == io.EOF && !bytes.HasSuffix(chunk, []byte{'\n'})
			raw := bytes.TrimSpace(chunk)
			if len(raw) > 0 && !missingNewline {
				var v T
				if err := json.Unmarshal(raw, &v); err != nil {
					return nil, fmt.Errorf("%w: %s:%d: %v", ErrCorrupt, path, line, err)
				}
				out = append(out, v)
			}
			if missingNewline {
				if err := truncateFile(path, offset); err != nil {
					return nil, err
				}
				return out, nil
			}
			offset += int64(len(chunk))
		}
		if readErr == io.EOF {
			return out, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func truncateFile(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return defaultMessageLimit
	}
	if limit > maxMessageLimit {
		return maxMessageLimit
	}
	return limit
}

func sliceMessages(msgs []Message, beforeSeq int64, limit int) []Message {
	if len(msgs) == 0 {
		return nil
	}
	end := len(msgs)
	if beforeSeq > 0 {
		end = 0
		for i, m := range msgs {
			if m.Seq >= beforeSeq {
				break
			}
			end = i + 1
		}
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	if start >= end {
		return nil
	}
	return append([]Message(nil), msgs[start:end]...)
}

func tail[T any](items []T, limit int) []T {
	if len(items) == 0 {
		return nil
	}
	if limit > len(items) {
		limit = len(items)
	}
	return append([]T(nil), items[len(items)-limit:]...)
}

func matchTask(t *Task, f TaskFilter) bool {
	if f.Status != "" && t.Status != f.Status {
		return false
	}
	if f.RootTaskID != "" && t.RootTaskID != f.RootTaskID {
		return false
	}
	if f.GroupID != "" && t.Origin.GroupID != f.GroupID && t.Target.GroupID != f.GroupID {
		return false
	}
	if f.AgentID != "" && t.Origin.AgentID != f.AgentID && t.Target.AgentID != f.AgentID {
		return false
	}
	return true
}

func validID(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, `/\`) || strings.ContainsRune(s, 0) {
		return false
	}
	return true
}

func knownPrefix(prefix string) bool {
	switch prefix {
	case PrefixAgent, PrefixGroup, PrefixTask, PrefixRound, PrefixNeed, PrefixArtifact, PrefixHandoff:
		return true
	default:
		return false
	}
}
