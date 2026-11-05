package agenthub

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNotFound / ErrInvalid / ErrConflict / ErrCorrupt / ErrUnsupportedSchema
// 是 Store 层对调用方的稳定错误分类；上层用 errors.Is 判断，不解析字符串。
var (
	ErrNotFound          = errors.New("agenthub: not found")
	ErrInvalid           = errors.New("agenthub: invalid")
	ErrConflict          = errors.New("agenthub: conflict")
	ErrCorrupt           = errors.New("agenthub: corrupt")
	ErrUnsupportedSchema = errors.New("agenthub: unsupported schema")
)

// Store 是 Hub 的持久化接口。领域规则不依赖具体实现；FileStore 落盘，MemoryStore 供测试/评测。
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
	ReadMessages(ctx context.Context, gid GroupID, beforeSeq int64, limit int) ([]Message, error)
	SearchMessages(ctx context.Context, gid GroupID, query string, limit int) ([]Message, error)

	AppendActivity(ctx context.Context, a *Activity) error
	ReadActivity(ctx context.Context, gid GroupID, afterSeq int64, limit int) ([]Activity, error)

	LoadTask(ctx context.Context, id TaskID) (*Task, error)
	SaveTask(ctx context.Context, t *Task) error
	AppendTaskEvent(ctx context.Context, id TaskID, ev TaskEvent) error
	ReadTaskEvents(ctx context.Context, id TaskID) ([]TaskEvent, error)
	ListTasks(ctx context.Context, filter TaskFilter) ([]*Task, error)

	LoadNeed(ctx context.Context, id NeedID) (*Need, error)
	SaveNeed(ctx context.Context, n *Need) error
	ListNeeds(ctx context.Context, filter NeedFilter) ([]*Need, error)

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
type FileStore struct {
	root string

	mu       sync.Mutex // 保护 seq / events 索引 / ID 预留与日志读写的可见性
	seq      map[GroupID]int64
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
		msgs, err := readJSONL[Message](path)
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

	eventMatches, err := filepath.Glob(filepath.Join(s.root, "tasks", "*", "events.jsonl"))
	if err != nil {
		return err
	}
	for _, path := range eventMatches {
		tid := TaskID(filepath.Base(filepath.Dir(path)))
		events, err := readJSONL[TaskEvent](path)
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
	m.Seq = seq
	// 消息 ID 是 seq 的确定函数；忽略调用方传入值，避免写出不符合协议的 ID。
	m.ID = MessageID(seq)
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	if err := appendJSONL(s.messagesPath(m.GroupID), m); err != nil {
		return 0, err
	}
	s.seq[m.GroupID] = seq
	return seq, nil
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

func (s *FileStore) SearchMessages(ctx context.Context, gid GroupID, query string, limit int) ([]Message, error) {
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
	if a.At.IsZero() {
		a.At = time.Now().UTC()
	}
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
	items, err := readJSONL[Activity](s.activityPath(gid))
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

func (s *FileStore) AppendTaskEvent(ctx context.Context, id TaskID, ev TaskEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validID(string(id)) {
		return fmt.Errorf("%w: task id %q", ErrInvalid, id)
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ev.Seq = s.taskSeq[id] + 1
	ev.TaskID = id
	if err := appendJSONL(s.taskEventsPath(id), &ev); err != nil {
		return err
	}
	s.taskSeq[id] = ev.Seq
	return nil
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
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
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
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = cp.CreatedAt
	}
	if cp.Version == 0 {
		cp.Version = 1
	}
	return writeJSONAtomic(s.artifactPath(cp.ID), &cp)
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
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = time.Now().UTC()
	}
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

func appendJSONL(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func readJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxJSONLLineBytes)
	var out []T
	line := 0
	for scanner.Scan() {
		line++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		var v T
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, fmt.Errorf("%w: %s:%d: %v", ErrCorrupt, path, line, err)
		}
		out = append(out, v)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
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
	case PrefixAgent, PrefixGroup, PrefixTask, PrefixRound, PrefixNeed, PrefixArtifact:
		return true
	default:
		return false
	}
}
