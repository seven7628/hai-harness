package evolution

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// NegativeEvidence 一条负证据：某个观测被判定为"对某条记忆不利"的记录。
//
// 为什么要单独的账本，而不是直接在 Outcome 上加个 flag：负证据的**查询方向**
// 与 Outcome 相反。Outcome 是"某主题的历史"，负证据是"哪些观测会让我不该再用
// 这条记忆"—— 后者要跨主题、跨技能聚合（Phase D 的技能负迁移评估就靠它）。
// 两类查询的形状不同，混在一份结构里会让两边都写得不自然。
//
// 负证据只**记录**与**检索**，不驱动任何自动动作：是否因此淘汰、归档或拒绝晋级，
// 由宿主依据 Confidence 与评估结论决定。
type NegativeEvidence struct {
	// ID 负证据 id。
	ID string `json:"id"`
	// Target 被指控的对象（"topic:<name>" 或 "skill:<name>"）。
	Target string `json:"target"`
	// Kind 负证据类型（见 NegativeKind）。
	Kind NegativeKind `json:"kind"`
	// Source 证据来源（宿主标注：哪次评估 / 哪条 trace）。
	Source string `json:"source,omitempty"`
	// Detail 人可读细节。
	Detail string `json:"detail"`
	// Weight 权重（默认 1.0）。
	//
	// 口径：负证据**不**默认等于"有害"。conflicting（与仓库事实冲突）比
	// unhelpful（没帮上忙）更重，权重体现这个差别 —— 让宿主能直接累加，
	// 而不必自己发明一套打分。
	Weight float64 `json:"weight"`
	// At 记录时间。
	At time.Time `json:"at"`
}

// NegativeKind 负证据类型。
type NegativeKind = string

const (
	// NegHarmful 读取后造成实际危害（误导、重复失败、额外成本）。
	NegHarmful NegativeKind = "harmful"
	// NegContradicted 与当前仓库事实冲突。
	NegContradicted NegativeKind = "conflicting"
	// NegUnhelpful 读了但没帮上忙（纯成本）。
	NegUnhelpful NegativeKind = "unhelpful"
	// NegStale 内容已与当前适用条件脱节。
	NegStale NegativeKind = "stale"
	// NegMigration 技能负迁移（Phase D：技能让结果变差）。
	NegMigration NegativeKind = "negative_migration"
)

// 各类负证据的默认权重。
const (
	// weightHarmful 造成实际危害 —— 最强证据。
	weightHarmful = 3.0
	// weightContradicted 与事实冲突 —— 次强。
	weightContradicted = 2.0
	// weightMigration 技能负迁移 —— 与 harmful 同级（它就是一种有害）。
	weightMigration = 3.0
	// weightStale 脱节 —— 弱证据（可能只是场景不同）。
	weightStale = 1.0
	// weightUnhelpful 没用上 —— 最弱（可能只是任务不相关）。
	weightUnhelpful = 0.5
)

func defaultWeight(k NegativeKind) float64 {
	switch k {
	case NegHarmful, NegMigration:
		return weightHarmful
	case NegContradicted:
		return weightContradicted
	case NegStale:
		return weightStale
	case NegUnhelpful:
		return weightUnhelpful
	}
	return 1.0
}

// evidenceStore 负证据账本（<repo>/evolution/evidence/<target>.json）。
type evidenceStore struct{ dir string }

// normalizeTarget 规范化被指控对象（"topic:foo" / "skill:bar" / "foo" → "topic:foo"）。
func normalizeTarget(target string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(target))
	if t == "" {
		return "", errors.New("evolution: empty evidence target")
	}
	switch {
	case strings.HasPrefix(t, "skill:"):
		name := strings.TrimPrefix(t, "skill:")
		if err := safeSegment("skill", name); err != nil {
			return "", err
		}
		return "skill:" + name, nil
	case strings.HasPrefix(t, "topic:"):
		ref, err := normalizeMemoryRef(t)
		if err != nil {
			return "", err
		}
		return ref, nil
	default:
		ref, err := normalizeMemoryRef(t)
		if err != nil {
			return "", err
		}
		return ref, nil
	}
}

func (e *evidenceStore) path(target string) (string, error) {
	n := sanitizeSegment(strings.ReplaceAll(target, ":", "-"))
	if n == "" {
		return "", errors.New("evolution: empty evidence target")
	}
	return joinSegment(e.dir, "target", n+".json")
}

// Load 读某对象的全部负证据（无记录返回 nil, nil）。
func (e *evidenceStore) Load(target string) ([]NegativeEvidence, error) {
	t, err := normalizeTarget(target)
	if err != nil {
		return nil, err
	}
	p, err := e.path(t)
	if err != nil {
		return nil, err
	}
	var out []NegativeEvidence
	if err := readJSON(p, &out); err != nil {
		if errors.Is(err, errNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

// Append 追加一条负证据。
func (e *evidenceStore) Append(ev NegativeEvidence) error {
	t, err := normalizeTarget(ev.Target)
	if err != nil {
		return err
	}
	ev.Target = t
	p, err := e.path(t)
	if err != nil {
		return err
	}
	existing, err := e.Load(t)
	if err != nil {
		return err
	}
	if len(existing) >= maxOutcomesPerTopic {
		return fmt.Errorf("%w: evidence for %s already has %d entries", ErrTooManyOutcomes, t, len(existing))
	}
	if ev.Weight == 0 {
		ev.Weight = defaultWeight(ev.Kind)
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	if ev.ID == "" {
		ev.ID = fmt.Sprintf("%d-%d", ev.At.UTC().Unix(), len(existing)+1)
	}
	existing = append(existing, ev)
	return writeJSON(p, existing)
}

// EvidenceVerdict 一个对象的负证据汇总结论。
type EvidenceVerdict struct {
	// Target 被指控对象。
	Target string `json:"target"`
	// Count 负证据条数。
	Count int `json:"count"`
	// TotalWeight 权重总和。
	TotalWeight float64 `json:"total_weight"`
	// ByKind 按类型计数。
	ByKind map[string]int `json:"by_kind"`
	// Blocking 为真表示证据强度足以阻止晋级/淘汰判定。
	//
	// 阈值口径：至少 1 条 harmful 级证据（权重 3）**或** 至少 3 条 stale 级证据
	// （权重 1×3）。前者对应"一次就足以证明有害"，后者对应"反复脱节"。
	// 为什么不用纯权重和：那会让"三条 barely 提到它没用"的证据压过"一次误导
	// 了整次任务"的证据，而这两种严重程度显然不同。
	Blocking bool `json:"blocking"`
	// Reason 人可读理由。
	Reason string `json:"reason"`
}

// evidenceBlockingThreshold 阻止判定所需的权重和（配合 blockingMinCount 使用）。
const evidenceBlockingThreshold = 3.0

// blockingMinCount 阻止判定所需的最少条数。
const blockingMinCount = 1

// Summarize 汇总某对象的负证据并给出是否阻止晋级的判定。
func (e *evidenceStore) Summarize(target string) (EvidenceVerdict, error) {
	t, err := normalizeTarget(target)
	if err != nil {
		return EvidenceVerdict{}, err
	}
	all, err := e.Load(t)
	if err != nil {
		return EvidenceVerdict{}, err
	}
	v := EvidenceVerdict{Target: t, Count: len(all), ByKind: map[string]int{}}
	var harmful int
	for _, ev := range all {
		v.TotalWeight += ev.Weight
		v.ByKind[ev.Kind]++
		if ev.Kind == NegHarmful || ev.Kind == NegMigration {
			harmful++
		}
	}
	switch {
	case harmful >= blockingMinCount:
		v.Blocking = true
		v.Reason = fmt.Sprintf("%d harmful/migration evidence entr(y/ies) recorded", harmful)
	case v.TotalWeight >= evidenceBlockingThreshold:
		v.Blocking = true
		v.Reason = fmt.Sprintf("cumulative evidence weight %.1f reached the blocking threshold %.1f", v.TotalWeight, evidenceBlockingThreshold)
	case v.Count > 0:
		v.Reason = fmt.Sprintf("%d evidence entr(y/ies), weight %.1f below threshold", v.Count, v.TotalWeight)
	default:
		v.Reason = "no negative evidence"
	}
	return v, nil
}

// NegativeEvidenceOf 读某对象的负证据（Service 公开入口）。
func (s *Service) NegativeEvidenceOf(target string) ([]NegativeEvidence, error) {
	return s.evidence().Load(target)
}

// SummarizeEvidence 汇总负证据并判断是否阻止晋级。
func (s *Service) SummarizeEvidence(target string) (EvidenceVerdict, error) {
	return s.evidence().Summarize(target)
}

// RecordNegative 记录一条负证据（宿主在评估出负面结果时调用）。
func (s *Service) RecordNegative(ev NegativeEvidence) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evidence().Append(ev)
}
