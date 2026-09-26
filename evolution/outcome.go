package evolution

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// Outcome 一次结果观测：某条记忆被读到之后，实际发生了什么。
//
// 字段口径刻意极简，因为**它只能来自真实任务**。任何"这个字段应该填什么"的
// 猜测都会把主观判断伪装成证据，进而污染 Confidence —— 而 Confidence 是
// 后续 Stale / Supersede / Expire 判定的唯一输入。所以：
//
//   - OutcomeKind：观测到的事实类型（见 OutcomeKind 常量）；
//   - Help：这条记忆有没有帮上忙（有用 / 中性 / 有害）；
//   - Context：任务指纹，Stale 判定用它发现"适用条件已经变了"。
//
// 写不出来的字段就留空，不要填默认值——空值意味着"没观测到"，而 0 值会被
// 当成"观测到是 0"。
type Outcome struct {
	// ID 观测 id（outcomes/<topic>.<id>.json 的文件名部分）。
	ID string `json:"id"`
	// Kind 观测类型。
	Kind OutcomeKind `json:"kind"`
	// Help 有害性判定。
	Help HelpVerdict `json:"help"`
	// TaskID 任务标识（同一任务的多次观测共享它，用于配对复现）。
	TaskID string `json:"task_id,omitempty"`
	// Context 任务指纹（可选）。Stale 判定用它发现"适用条件已变"。
	Context string `json:"context,omitempty"`
	// Note 人可读说明（为什么判定为有害、观测来自哪次 run）。
	Note string `json:"note,omitempty"`
	// At 观测时间。
	At time.Time `json:"at"`
}

// OutcomeKind 观测类型。
const (
	// OutcomeRead 记忆被读取（JIT 命中）。
	OutcomeRead = "read"
	// OutcomeHelped 读取后确实减少了探索 / 重复失败。
	OutcomeHelped = "helped"
	// OutcomeHarmful 读取后造成误导、重复失败或无关干扰。
	OutcomeHarmful = "harmful"
	// OutcomeContradicted 与当前仓库事实冲突。
	OutcomeContradicted = "contradicted"
	// OutcomeUnused 判定为不再需要（Stale）。
	OutcomeUnused = "unused"
	// OutcomeReverted 回滚（Rollback 产生）。
	OutcomeReverted = "reverted"
	// OutcomeArchived 归档（Archive 产生）。
	OutcomeArchived = "archived"
)

// HelpVerdict 有害性判定。
const (
	// HelpUseful 有用。
	HelpUseful HelpVerdict = "useful"
	// HelpNeutral 中性（读了但没影响）。
	HelpNeutral HelpVerdict = "neutral"
	// HelpHarmful 有害。
	HelpHarmful HelpVerdict = "harmful"
)

// OutcomeKind 类型。
type OutcomeKind = string

// HelpVerdict 类型。
type HelpVerdict = string

// negative 是"负证据"的判定口径：哪些观测算作对某条记忆不利的证据。
//
// 为什么 harmful 与 contradicted 算负证据而 read / neutral 不算：read 只说明
// "被找到了"，那是成本不是害处；neutral 说明无影响。把它们计成负证据会让
// 一条无害但少被读的记忆被判为低置信，从而被淘汰 —— 那是在惩罚"没被用到"，
// 而淘汰的理由应该是"有害"，不是"没用过"。Confidence 因此**只**由
// helped / harmful / contradicted / unused 驱动。
func isNegative(o Outcome) bool {
	switch o.Kind {
	case OutcomeHarmful, OutcomeContradicted, OutcomeUnused, OutcomeReverted, OutcomeArchived:
		return true
	}
	return false
}

// isPositive 是否是"支持这条记忆"的证据。
func isPositive(o Outcome) bool { return o.Kind == OutcomeHelped }

// stats 一条记忆的证据统计。
type stats struct {
	helped   int
	harmful  int
	stale    int // unused
	other    int
	first    time.Time
	last     time.Time
	lastKind string
}

func (s *stats) observe(o Outcome) {
	if isPositive(o) {
		s.helped++
	} else if isNegative(o) {
		switch o.Kind {
		case OutcomeUnused:
			s.stale++
		case OutcomeHarmful, OutcomeContradicted, OutcomeReverted, OutcomeArchived:
			s.harmful++
		}
	} else {
		s.other++
	}
	if s.first.IsZero() || o.At.Before(s.first) {
		s.first = o.At
	}
	if s.last.IsZero() || o.At.After(s.last) {
		s.last = o.At
		s.lastKind = o.Kind
	}
}

// Confidence 一条记忆的置信度。
type Confidence struct {
	// Value 置信值 [0,1]。
	//
	// 口径：Beta 后验均值，伪计数取 1（先验均匀，2 个虚拟观测：一个好一个坏）。
	// 选它而不是"命中率"的原因：命中率为 0 的新记忆与命中率为 0 的坏记忆
	// 数值相同，但前者应该被观察、后者应该被淘汰。伪计数让两者都落在 0.5 附近，
	// 真正的区分发生在样本累积之后，而"样本不足"由 Count 单独表达。
	Value float64 `json:"value"`
	// Count 参与计算的有效观测数（helped + harmful）。
	Count int `json:"count"`
	// HarmfulRatio 负证据占有效观测的比例。
	HarmfulRatio float64 `json:"harmful_ratio"`
	// LastKind 最近一次观测类型。
	LastKind string `json:"last_kind,omitempty"`
	// LastAt 最近观测时间。
	LastAt time.Time `json:"last_at"`
}

// DefaultConfidence 从观测序列计算置信度。
func DefaultConfidence(outcomes []Outcome) Confidence {
	var s stats
	for _, o := range outcomes {
		s.observe(o)
	}
	return confidenceFrom(&s)
}

func confidenceFrom(s *stats) Confidence {
	priorGood, priorBad := 1.0, 1.0
	good := priorGood + float64(s.helped)
	bad := priorBad + float64(s.harmful)
	c := Confidence{
		Value:        good / (good + bad),
		Count:        s.helped + s.harmful,
		HarmfulRatio: 0,
		LastKind:     s.lastKind,
		LastAt:       s.last,
	}
	if c.Count > 0 {
		c.HarmfulRatio = float64(s.harmful) / float64(c.Count)
	}
	// 数值夹紧：浮点误差不能产出 1.0000000002 这种会被误读为"绝对确定"的读数。
	if c.Value < 0 {
		c.Value = 0
	}
	if c.Value > 1 {
		c.Value = 1
	}
	if math.IsNaN(c.Value) {
		c.Value = priorGood / (priorGood + priorBad)
	}
	return c
}

// outcomeLedger 主题级结果账本（<repo>/evolution/outcomes/<topic>.json）。
//
// 为什么是"每主题一个文件"而不是全局追加日志：读 Confidence 需要的是**该主题的
// 全部观测**，而 per-topic 文件让读取是 O(1) 打开、且能原子重写；全局日志在
// 并发追加下要么丢记录要么需要复杂锁，而观测丢失会让置信度悄悄偏低 ——
// 一个会静默低估风险的账本比没有账本更危险。
type outcomeLedger struct {
	mu      sync.Mutex
	dir     string
	maxKeep int
}

// ErrTooManyOutcomes 观测条数超过上限（拒绝写入而不是静默丢弃最旧的）。
var ErrTooManyOutcomes = errors.New("evolution: outcome ledger is full")

// maxOutcomesPerTopic 单主题观测条数上限。
//
// 边界：账本是判定依据不是日志，超过上限说明观测口径有问题（同一个任务被重复
// 记录），此时**报错**而不是丢最旧的——丢记录会让置信度在无感知的情况下漂移。
const maxOutcomesPerTopic = 200

func newOutcomeLedger(dir string) *outcomeLedger {
	return &outcomeLedger{dir: dir, maxKeep: maxOutcomesPerTopic}
}

func (l *outcomeLedger) path(topic string) (string, error) {
	n := sanitizeSegment(topic)
	if n == "" {
		return "", errors.New("evolution: empty topic name")
	}
	return joinSegment(l.dir, "topic", n+".json")
}

// Load 读一个主题的全部观测（无记录返回 nil, nil）。
func (l *outcomeLedger) Load(topic string) ([]Outcome, error) {
	p, err := l.path(topic)
	if err != nil {
		return nil, err
	}
	var out []Outcome
	if err := readJSON(p, &out); err != nil {
		if errors.Is(err, errNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

// Append 追加一条观测（原子重写该主题的账本）。
func (l *outcomeLedger) Append(topic string, o Outcome) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, err := l.path(topic)
	if err != nil {
		return err
	}
	existing, err := l.Load(topic)
	if err != nil {
		return err
	}
	if len(existing) >= l.maxKeep {
		return fmt.Errorf("%w: topic %q already has %d outcomes", ErrTooManyOutcomes, topic, len(existing))
	}
	if o.ID == "" {
		o.ID = fmt.Sprintf("%d-%d", o.At.UTC().Unix(), len(existing)+1)
	}
	if o.At.IsZero() {
		o.At = time.Now().UTC()
	}
	existing = append(existing, o)
	return writeJSON(p, existing)
}

// Reset 清空一个主题的观测（实验重置用；必须显式调用）。
func (l *outcomeLedger) Reset(topic string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, err := l.path(topic)
	if err != nil {
		return err
	}
	if err := removeIfExists(p); err != nil {
		return err
	}
	return nil
}

// Ledger 暴露给 Phase D：读另一主题的观测用于联合评估。
type Ledger interface {
	// Load 读一个主题的全部观测（无记录返回 nil, nil）。
	Load(topic string) ([]Outcome, error)
	// Append 追加一条观测。
	Append(topic string, o Outcome) error
}

// ensure outcomeLedger 满足 Ledger 接口（编译期断言）。
var _ Ledger = (*outcomeLedger)(nil)

// sortOutcomes 按时间升序（同刻按 id 升序，保证确定性）。
func sortOutcomes(os []Outcome) {
	sort.SliceStable(os, func(i, j int) bool {
		if !os[i].At.Equal(os[j].At) {
			return os[i].At.Before(os[j].At)
		}
		return os[i].ID < os[j].ID
	})
}
