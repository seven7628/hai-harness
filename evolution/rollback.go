package evolution

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Revision 主题的一个历史版本快照（<repo>/evolution/revisions/<topic>/<n>.json）。
//
// **为什么必须存内容而不是只存元数据**：回滚要恢复的是**当时的正文**。只记
// "v3 于某日被 v4 取代" 无法回滚 —— v3 的正文可能已被再次覆写。而 Phase A 的
// WriteTopic 是同名覆盖语义（没有版本历史），所以想回滚就必须在被取代/回滚的
// 那一刻把内容存下来。这是 Phase C 唯一新增的正文落盘物。
//
// 存储格式是 JSON 而不是裸 Markdown：快照需要带版本号、原因、时间，正文塞在
// 字段里。裸 Markdown 无法携带这些元数据，而把元数据写成注释行又会让恢复路径
// 依赖一段 Markdown 解析逻辑。
type Revision struct {
	// Topic 主题名。
	Topic string `json:"topic"`
	// Version 版本号（单调递增，从 1 开始）。
	Version int `json:"version"`
	// Reason 产生这个快照的原因。
	Reason string `json:"reason"`
	// At 快照时间。
	At time.Time `json:"at"`
	// Content 当时的主题 Markdown（完整，含 frontmatter）。
	Content string `json:"content"`
}

// revisionExt 快照文件扩展名。
const revisionExt = ".json"

// revisionStore 快照存储。
type revisionStore struct{ dir string }

// topicDir 某主题的快照目录。
func (r *revisionStore) topicDir(topic string) (string, error) {
	n := sanitizeSegment(topic)
	if n == "" {
		return "", errors.New("evolution: empty topic name")
	}
	return joinSegment(r.dir, "topic", n)
}

// list 列出某主题的全部快照（按版本号升序）。
func (r *revisionStore) list(topic string) ([]*Revision, error) {
	dir, err := r.topicDir(topic)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, errNotExist) {
			return nil, nil // 还没有快照
		}
		return nil, fmt.Errorf("evolution: list revisions of %s: %w", topic, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), revisionExt) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := make([]*Revision, 0, len(names))
	for _, n := range names {
		var rev Revision
		if err := readJSON(filepath.Join(dir, n), &rev); err != nil {
			return nil, fmt.Errorf("evolution: read revision %s/%s: %w", topic, n, err)
		}
		out = append(out, &rev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// latest 读最新一版快照；没有快照返回包裹 ErrNoRevision 的错误。
func (r *revisionStore) latest(topic string) (*Revision, error) {
	all, err := r.list(topic)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("%w: topic %q has no snapshots", ErrNoRevision, topic)
	}
	return all[len(all)-1], nil
}

// save 写一版快照。
func (r *revisionStore) save(rev *Revision) error {
	dir, err := r.topicDir(rev.Topic)
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, revisionFileName(rev.Version)), rev)
}

// revisionFileName 快照文件名（零填充，保证字典序 = 版本序）。
func revisionFileName(version int) string { return fmt.Sprintf("%06d%s", version, revisionExt) }

// snapshotLocked 为当前主题内容存一份快照。
//
// 版本号 = 已有快照数 + 1：用"数文件"而不是"读最大号再 +1"，是因为后者在快照
// 缺失或被清理后会复用旧版本号，导致覆盖掉仍可能被回滚的历史。Service 的写路径
// 已串行，读-写之间不会并发。
func (s *Service) snapshotLocked(topic, reason string) error {
	t, err := s.cfg.Store.ReadTopic(topic)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // 主题不存在（可能已归档）：没有内容可存
		}
		return err
	}
	all, err := s.revisions().list(topic)
	if err != nil {
		return err
	}
	return s.revisions().save(&Revision{
		Topic:   topic,
		Version: len(all) + 1,
		Reason:  reason,
		At:      s.now(),
		Content: s.hooks.Render(t),
	})
}

// Revisions 列出某主题的全部版本快照（审计 / UI 用）。
func (s *Service) Revisions(topic string) ([]*Revision, error) { return s.revisions().list(topic) }

// RollbackResult 回滚结果。
type RollbackResult struct {
	// Topic 主题名。
	Topic string `json:"topic"`
	// RestoredVersion 恢复到磁盘上的版本号。
	RestoredVersion int `json:"restored_version"`
	// RestoredBytes 恢复的正文字节数。
	RestoredBytes int `json:"restored_bytes"`
	// RemainingSnapshots 目录里剩余的快照数（回滚本身也留了一版）。
	RemainingSnapshots int `json:"remaining_snapshots"`
	// At 回滚时间。
	At time.Time `json:"at"`
}

// Rollback 把主题恢复为最近一版快照的内容。
//
// 这是 Phase C 才引入的能力（Phase B 明确不做自动 Rollback），而且是**必须**的：
// 任何"修改或淘汰既有记忆"的能力都需要一条确定的退路，否则一次坏的生命周期
// 判定就等于永久损失用户知识。
//
// 两条不变式：
//   - **真的恢复内容**，不只是改状态：状态写 rolled_back 而正文还是坏的，就是
//     本包最不想制造的"账面与现实脱节"；
//   - **回滚本身也可逆**：先给当前（坏）内容留一版快照，所以"回滚再回滚"可行。
//     回滚不是单向操作，这一点是它能被信任的前提。
//
// 恢复经 Promoter 走 Phase A 的 WriteTopic，索引同步、限额、原子写仍由 Phase A 负责。
func (s *Service) Rollback(topic, reason string) (*RollbackResult, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, ErrNoReason
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	prev, err := s.revisions().latest(topic)
	if err != nil {
		return nil, err
	}
	parsed := s.hooks.Parse(prev.Content)
	if parsed == nil {
		return nil, fmt.Errorf("evolution: rollback %s: %w", topic, ErrCorruptSnapshot)
	}
	// 先给当前内容留快照，让回滚可逆。
	if err := s.snapshotLocked(topic, "pre-rollback snapshot"); err != nil {
		return nil, err
	}
	if err := s.cfg.Promoter.Promote(parsed); err != nil {
		return nil, fmt.Errorf("evolution: rollback %s: %w", topic, err)
	}
	lc, err := s.life().Load(topic)
	if errors.Is(err, ErrNoLifecycle) {
		lc = &Lifecycle{Topic: topic, State: LifeActive, Version: 1, Created: s.now()}
	} else if err != nil {
		return nil, err
	}
	from := lc.State
	lc.State = LifeRolledBack
	lc.Reason = "rollback: " + reason
	lc.Updated = s.now()
	lc.Version++
	lc.History = append(lc.History, LifecycleStep{From: from, To: LifeRolledBack, Reason: reason, At: s.now()})
	if err := s.life().Save(lc); err != nil {
		return nil, err
	}
	if err := s.ledger().Append(topic, Outcome{Kind: OutcomeReverted, Note: reason, At: s.now()}); err != nil {
		return nil, err
	}
	after, _ := s.revisions().list(topic)
	return &RollbackResult{
		Topic:              topic,
		RestoredVersion:    prev.Version,
		RestoredBytes:      len(prev.Content),
		RemainingSnapshots: len(after),
		At:                 s.now(),
	}, nil
}

// RestoreFrom 恢复到指定版本（不是最新版本）。
//
// 存在的理由：最新版本可能就是坏的那个（"回滚再回滚"之外还有"跳过中间某版"）。
// 越界版本号报 ErrNoRevision 而不是回退到最接近的版本 —— 静默替换会让调用方
// 以为恢复到了它要的版本。
func (s *Service) RestoreFrom(topic string, version int, reason string) (*RollbackResult, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, ErrNoReason
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	all, err := s.revisions().list(topic)
	if err != nil {
		return nil, err
	}
	var want *Revision
	for _, r := range all {
		if r.Version == version {
			want = r
			break
		}
	}
	if want == nil {
		return nil, fmt.Errorf("%w: topic %q has no version %d", ErrNoRevision, topic, version)
	}
	parsed := s.hooks.Parse(want.Content)
	if parsed == nil {
		return nil, fmt.Errorf("evolution: restore %s v%d: %w", topic, version, ErrCorruptSnapshot)
	}
	if err := s.snapshotLocked(topic, "pre-restore snapshot"); err != nil {
		return nil, err
	}
	if err := s.cfg.Promoter.Promote(parsed); err != nil {
		return nil, fmt.Errorf("evolution: restore %s v%d: %w", topic, version, err)
	}
	after, _ := s.revisions().list(topic)
	return &RollbackResult{
		Topic:              topic,
		RestoredVersion:    want.Version,
		RestoredBytes:      len(want.Content),
		RemainingSnapshots: len(after),
		At:                 s.now(),
	}, nil
}

// 快照层错误哨兵（errors.Is 判别）。
var (
	// ErrNoRevision 该主题没有可回滚的版本快照。
	ErrNoRevision = errors.New("evolution: no revision snapshot to roll back to")
	// ErrCorruptSnapshot 快照无法被解析回主题（内容损坏）。
	ErrCorruptSnapshot = errors.New("evolution: revision snapshot could not be parsed back into a topic")
)
