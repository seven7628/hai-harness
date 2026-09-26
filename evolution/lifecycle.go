package evolution

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/memory"
)

// Lifecycle 主题的生命周期状态（Phase C）。
//
// 状态是**显式**的，不是自动推进的：宿主在评估出"该淘汰了"之后调用
// Stale / Supersede / Expire / Archive。自动淘汰需要长期 outcome 数据支撑，
// 而 Phase C 的职责是建立记录与判定口径 —— 谁在什么依据下判、判完做了什么，
// 全部留痕。判定依据存在 Reason 里，回滚时能回答"我为什么退回去"。
type Lifecycle struct {
	// Topic 主题名。
	Topic string `json:"topic"`
	// State 当前状态。
	State LifeState `json:"state"`
	// Confidence 最近一次刷新的置信度快照（回滚时回答"当时有多确信"）。
	Confidence Confidence `json:"confidence"`
	// Reason 最近一次状态变更的依据（**必填**：无依据的状态变更不可审计）。
	Reason string `json:"reason"`
	// SupersededBy 被谁取代（State=superseded 时必填）。
	SupersededBy string `json:"superseded_by,omitempty"`
	// Version 内容版本号（每次 supersede / rollback 递增）。
	Version int `json:"version"`
	// Created 生命周期记录创建时间。
	Created time.Time `json:"created"`
	// Updated 最近一次状态变更时间。
	Updated time.Time `json:"updated"`
	// History 状态变更历史（只增不改）。
	History []LifecycleStep `json:"history,omitempty"`
}

// LifeState 生命周期状态。
type LifeState = string

const (
	// LifeActive 活跃（默认；未经任何生命周期操作）。
	LifeActive LifeState = "active"
	// LifeStale 长期未用（软标记：仍可召回，只在状态里体现）。
	//
	// 边界：Stale **不删除内容、不移出索引**。把它做成软标记而不是淘汰，是因为
	// "多久没用"与"还有没有用"不等价 —— 一条只在季度任务里用到的记忆半年没用
	// 是正常的。直接删会误伤低频高价值记忆（评估协议说的"只在单个偶然任务中
	// 有用"指的是*无效*，不是*低频*）。
	LifeStale LifeState = "stale"
	// LifeSuperseded 被新主题取代。
	LifeSuperseded LifeState = "superseded"
	// LifeExpired 按明确 TTL 到期（仍是软标记）。
	LifeExpired LifeState = "expired"
	// LifeArchived 已归档（文件移出 topics/，不可召回，内容保留）。
	LifeArchived LifeState = "archived"
	// LifeRolledBack 已回滚（内容已从 revisions/ 恢复）。
	LifeRolledBack LifeState = "rolled_back"
)

// LifecycleStep 一次状态变更。
type LifecycleStep struct {
	From   LifeState `json:"from"`
	To     LifeState `json:"to"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// 生命周期错误哨兵（调用点用 errors.Is 判别）。
var (
	// ErrNoLifecycle 该主题还没有生命周期记录（首次变更时会自动建立）。
	ErrNoLifecycle = errors.New("evolution: no lifecycle record for this topic")
	// ErrNoReason 状态变更缺少依据说明。
	ErrNoReason = errors.New("evolution: a lifecycle change requires a reason")
	// ErrArchiveStore 存储未实现 TopicPath，无法归档真实文件。
	ErrArchiveStore = errors.New("evolution: store does not expose TopicPath; cannot move topic files")
	// ErrIllegalTransition 非法状态迁移。
	ErrIllegalTransition = errors.New("evolution: illegal lifecycle transition")
)

// legalTransitions 允许的状态迁移。
//
// 显式白名单而不是"随便改"：状态一旦乱了（superseded 又 active 又 archived），
// 回滚就找不到一致的父版本，账本也没法回答"这条记忆现在算不算有效"。白名单把
// 非法迁移挡在写入之前 —— 这类状态机的 bug 几乎都是"漏了一条边"造成的。
var legalTransitions = map[LifeState][]LifeState{
	LifeActive:     {LifeStale, LifeSuperseded, LifeExpired, LifeArchived, LifeRolledBack},
	LifeStale:      {LifeActive, LifeSuperseded, LifeExpired, LifeArchived, LifeRolledBack},
	LifeSuperseded: {LifeRolledBack, LifeArchived, LifeActive},
	LifeExpired:    {LifeActive, LifeArchived, LifeRolledBack, LifeStale},
	LifeArchived:   {LifeActive, LifeRolledBack},
	LifeRolledBack: {LifeActive, LifeStale, LifeSuperseded, LifeExpired, LifeArchived},
}

func transitionAllowed(from, to LifeState) bool {
	// 自迁移（stale -> stale）是**幂等重标注**，不是非法迁移。宿主重复执行
	// 同一条标记时应该成功并刷新依据，而不是报"非法迁移"——后者会让重试脚本
	// 在第二次运行时意外失败。
	if from == to {
		return true
	}
	for _, s := range legalTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// lifecycleStore 生命周期状态存储（<repo>/evolution/lifecycle/<topic>.json）。
type lifecycleStore struct {
	dir string
}

func (l *lifecycleStore) path(topic string) (string, error) {
	n := sanitizeSegment(topic)
	if n == "" {
		return "", errors.New("evolution: empty topic name")
	}
	return joinSegment(l.dir, "topic", n+".json")
}

// Load 读生命周期记录；没有记录返回包裹 ErrNoLifecycle 的错误。
func (l *lifecycleStore) Load(topic string) (*Lifecycle, error) {
	p, err := l.path(topic)
	if err != nil {
		return nil, err
	}
	var lc Lifecycle
	if err := readJSON(p, &lc); err != nil {
		if errors.Is(err, errNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNoLifecycle, topic)
		}
		return nil, err
	}
	return &lc, nil
}

func (l *lifecycleStore) Save(lc *Lifecycle) error {
	p, err := l.path(lc.Topic)
	if err != nil {
		return err
	}
	return writeJSON(p, lc)
}

// Mark 对一个主题施加一次生命周期状态变更（带依据、置信度快照与历史）。
//
// 四个软标记操作（Stale / Supersede / Expire / Unmark）都收敛到这一个函数：
// 它们共享"记录依据 + 刷新置信度快照 + 追加历史"的结构，只有目标状态和额外
// 语义不同。收敛到一处的收益是审计口径统一 —— 不存在某条路径忘了写依据。
func (s *Service) Mark(topic string, to LifeState, reason, supersededBy string) (*Lifecycle, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, ErrNoReason
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.markLocked(topic, to, reason, supersededBy)
}

func (s *Service) markLocked(topic string, to LifeState, reason, supersededBy string) (*Lifecycle, error) {
	lc, err := s.life().Load(topic)
	if errors.Is(err, ErrNoLifecycle) {
		lc = &Lifecycle{Topic: topic, State: LifeActive, Version: 1, Created: s.now()}
	} else if err != nil {
		return nil, err
	}
	from := lc.State
	if !transitionAllowed(from, to) {
		return nil, fmt.Errorf("%w: %s -> %s for topic %q", ErrIllegalTransition, from, to, topic)
	}
	if to == LifeSuperseded && strings.TrimSpace(supersededBy) == "" {
		return nil, errors.New("evolution: supersede requires the superseding topic name")
	}

	// 变更前给当前内容留一份快照，这样 Rollback 有真实的父版本可恢复 ——
	// 而不是靠"把 superseded_by 的内容拷回来"（依赖对方仍然正确，superseded_by
	// 自己也可能已被回滚或改写）。
	if err := s.snapshotLocked(topic, "pre-change snapshot ("+to+")"); err != nil {
		return nil, err
	}

	outcomes, err := s.ledger().Load(topic)
	if err != nil {
		return nil, err
	}
	lc.State = to
	lc.Reason = reason
	lc.Updated = s.now()
	lc.Confidence = DefaultConfidence(outcomes)
	if to == LifeSuperseded {
		lc.SupersededBy = supersededBy
	}
	lc.History = append(lc.History, LifecycleStep{From: from, To: to, Reason: reason, At: s.now()})
	if err := s.life().Save(lc); err != nil {
		return nil, err
	}
	return lc, nil
}

// Stale 标记主题为长期未用（软标记，仍可召回）。
func (s *Service) Stale(topic, reason string) (*Lifecycle, error) {
	return s.Mark(topic, LifeStale, reason, "")
}

// Supersede 用另一个主题取代当前主题。
//
// 取代**不删除**旧主题：旧主题进入 superseded，回滚依赖它的历史版本快照。
// 真正删除由宿主在确认新主题稳定后显式 Archive。
func (s *Service) Supersede(topic, supersededBy, reason string) (*Lifecycle, error) {
	return s.Mark(topic, LifeSuperseded, reason, supersededBy)
}

// Expire 按明确 TTL 标记主题到期（软标记；删除由宿主显式 Archive）。
func (s *Service) Expire(topic, reason string) (*Lifecycle, error) {
	return s.Mark(topic, LifeExpired, reason, "")
}

// Unmark 把 stale / expired / superseded 主题恢复为 active（误判后的纠正入口）。
func (s *Service) Unmark(topic, reason string) (*Lifecycle, error) {
	return s.Mark(topic, LifeActive, reason, "")
}

// LifecycleOf 读一个主题的生命周期记录；没有记录返回 ErrNoLifecycle。
func (s *Service) LifecycleOf(topic string) (*Lifecycle, error) { return s.life().Load(topic) }

// Revalidate 只刷新置信度快照，不改状态。
//
// 为什么允许状态不动而快照更新：置信度随新观测变化，但状态由宿主显式推进。
// 宿主因此能"随时看到当前置信度"，又不会因为刷新一次读数就自动淘汰。
func (s *Service) Revalidate(topic string) (*Lifecycle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lc, err := s.life().Load(topic)
	if err != nil {
		return nil, err
	}
	outcomes, err := s.ledger().Load(topic)
	if err != nil {
		return nil, err
	}
	lc.Confidence = DefaultConfidence(outcomes)
	if err := s.life().Save(lc); err != nil {
		return nil, err
	}
	return lc, nil
}

// ExpireOptions 过期建议的查询选项。
type ExpireOptions struct {
	// StaleAfter 超过这么久没有观测就算"长期未用"（零值 = 不启用该建议）。
	StaleAfter time.Duration
}

// ExpireSuggestion 一条过期/归档建议（**建议**，不是动作）。
type ExpireSuggestion struct {
	Topic         string
	Target        LifeState
	Reason        string
	Confidence    Confidence
	LastOutcomeAt time.Time
}

// 建议阈值写死在这里，好让评估口径稳定（宿主可覆盖 EvictPolicy）。
const (
	// harmfulRatioThreshold 建议归档所需的有害观测占比。
	harmfulRatioThreshold = 0.5
	// minEvidence 建议归档所需的最少有效观测数。
	//
	// 为什么需要它：一次 harmful 就归档太激进。记忆偶尔误导一次是正常的
	// （它本来就只是"非显然的经验"，不是真理），但**反复**有害才是信号。
	// 没有这个下限，系统会变成"任何一次失败就删掉这条记忆"。
	minEvidence = 2
)

// ExpireCandidates 列出"建议宿主考虑降级/归档"的主题（只读，不改任何状态）。
//
// 口径：
//   - 有害观测占比 >= harmfulRatioThreshold 且有效观测数 >= minEvidence → 建议归档；
//   - 或最后一次观测距今超过 opts.StaleAfter → 建议标记 stale。
//
// 明确不做的事：**不删任何东西**。淘汰决策是宿主的，本包只提供判定口径。
// 理由：把自动淘汰做进库里，就等于在没有长期数据支撑的情况下让系统自己删除
// 用户知识，而误删的代价（丢掉花很久总结的经验）远高于收益（省几 KB 索引）。
func (s *Service) ExpireCandidates(opts ExpireOptions) ([]ExpireSuggestion, error) {
	names, err := s.cfg.Store.ListTopics()
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out []ExpireSuggestion
	for _, name := range names {
		outcomes, err := s.ledger().Load(name)
		if err != nil {
			return nil, err
		}
		if len(outcomes) == 0 {
			continue // 没有观测的主题不给建议：不能因为"没数据"就淘汰
		}
		conf := DefaultConfidence(outcomes)
		var last time.Time
		for _, o := range outcomes {
			if last.IsZero() || o.At.After(last) {
				last = o.At
			}
		}
		sw := ExpireSuggestion{Topic: name, Confidence: conf, LastOutcomeAt: last}
		switch {
		case conf.Count >= minEvidence && conf.HarmfulRatio >= harmfulRatioThreshold:
			sw.Target = LifeArchived
			sw.Reason = fmt.Sprintf("harmful ratio %.2f over %d observations", conf.HarmfulRatio, conf.Count)
		case opts.StaleAfter > 0 && now.Sub(last) >= opts.StaleAfter:
			sw.Target = LifeStale
			sw.Reason = fmt.Sprintf("no observation since %s", last.UTC().Format(time.RFC3339))
		default:
			continue
		}
		out = append(out, sw)
	}
	return out, nil
}

// topicFile 主题在 topics/ 下的路径。
func (s *Service) topicFile(topic string) (string, error) {
	fs, ok := s.cfg.Store.(interface {
		TopicPath(string) (string, error)
	})
	if !ok {
		return "", ErrArchiveStore
	}
	return fs.TopicPath(topic)
}

// archiveTarget 归档副本路径（<repo>/evolution/archive/<topic>.md）。
func (s *Service) archiveTarget(topic string) (string, error) {
	n := sanitizeSegment(topic)
	if n == "" {
		return "", errors.New("evolution: empty topic name")
	}
	return joinSegment(s.dirs().archive, "topic", n+".md")
}

// Archive 归档一个主题：文件移出 topics/，索引条目移除，状态置 archived。
//
// 两件事都必须做，缺一件就产生自欺：
//   - 文件移出 topics/ —— 只改状态不移文件，memory_view 仍会读到它，Agent 继续
//     被一条"已归档"的记忆影响；
//   - 索引条目移除 —— 索引留着一个点开是空的链接，比没有链接更糟。
//
// 归档副本完整保留在 archive/，并可经 Restore 取回。
func (s *Service) Archive(topic, reason string) (*Lifecycle, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, ErrNoReason
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	src, err := s.topicFile(topic)
	if err != nil {
		return nil, err
	}
	t, err := s.cfg.Store.ReadTopic(topic)
	if err != nil {
		return nil, err
	}
	target, err := s.archiveTarget(topic)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(target, s.hooks.Render(t)); err != nil {
		return nil, err
	}
	if err := removeIfExists(src); err != nil {
		return nil, err
	}
	if err := s.dropIndexEntryLocked(topic); err != nil {
		return nil, err
	}
	// 记一条观测：归档改变了该主题的可用性，将来要能回答"它什么时候消失的"。
	if err := s.record().Append(topic, Outcome{Kind: OutcomeArchived, Note: reason, At: s.now()}); err != nil {
		return nil, err
	}
	return s.markLocked(topic, LifeArchived, reason, "")
}

// Restore 从 archive/ 取回一个被归档的主题。
func (s *Service) Restore(topic, reason string) (*Lifecycle, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, ErrNoReason
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	target, err := s.archiveTarget(topic)
	if err != nil {
		return nil, err
	}
	parsed := s.hooks.Parse(readFileString(target))
	if parsed == nil {
		return nil, fmt.Errorf("evolution: archive copy of %q is unreadable", topic)
	}
	if err := s.cfg.Promoter.Promote(parsed); err != nil {
		return nil, fmt.Errorf("evolution: restore %s: %w", topic, err)
	}
	if err := removeIfExists(target); err != nil {
		return nil, err
	}
	return s.markLocked(topic, LifeActive, "restored: "+reason, "")
}

// dropIndexEntryLocked 从 MEMORY.md 移除一个条目（复用 Phase A 的索引写入）。
func (s *Service) dropIndexEntryLocked(topic string) error {
	idx, err := s.cfg.Store.ReadIndex()
	if err != nil {
		if errors.Is(err, errNotExist) {
			return nil // 索引不存在：没有条目可删
		}
		return err
	}
	out := make([]memory.IndexEntry, 0, len(idx.Entries))
	for _, e := range idx.Entries {
		if e.Name == topic {
			continue
		}
		out = append(out, e)
	}
	if len(out) == len(idx.Entries) {
		return nil // 本来就没有该条目
	}
	idx.Entries = out
	idx.TopicCount = len(out)
	idx.Generated = s.now()
	idx.Truncated = false
	return s.cfg.Store.WriteIndex(idx)
}
