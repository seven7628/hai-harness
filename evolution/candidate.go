package evolution

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/memory"
)

// Candidate 一条待验证的记忆候选（Phase B 的核心对象）。
//
// 落盘格式（candidates/<id>.json）由本文件定义，Phase C 的 Lifecycle 操作与
// Phase D 的 Skill Candidate **复用同一套读取/校验路径**——不另起格式，
// 免得后面阶段出现"两套候选语义、只有一套门禁"的不一致。
//
// 不变式：
//   - Candidate 只存在于 <repo>/evolution/candidates/，**不写** topics/、**不写** MEMORY.md；
//     因此在 Promote 之前，它对 memory_view 与 BootstrapIndex 不可见（结构保证，
//     不是过滤逻辑）；
//   - Topic 内容复用 memory.Topic，因此 Phase A 的规范化（slug、secret 扫描、
//     正文大小、标签）在这里是**同一套代码**，不会两套校验漂移；
//   - Skill 非 nil 表示这是 Phase D 的技能候选；Topic 此时可能为零值。
type Candidate struct {
	// ID 候选标识（candidates/<id>.json 的文件名，不含扩展名）。
	ID string `json:"id"`
	// RepoID 所属仓库身份（必须等于当前 store 的 RepoID，见 Verify 的 scope 校验）。
	RepoID string `json:"repo_id"`
	// Kind 候选类型：KindTopic（Phase B/C）或 KindSkill（Phase D）。
	Kind string `json:"kind"`
	// Topic 记忆候选内容（Kind=topic 时必填）。
	Topic memory.Topic `json:"topic"`
	// Skill 技能候选内容（Kind=skill 时必填，Phase D）。
	Skill *SkillCandidate `json:"skill,omitempty"`
	// Source 产生来源（人工 / Agent / 门禁晋级），用于审计。
	Source string `json:"source"`
	// Rationale 为什么值得写（人类可读理由，审计与负证据定位用）。
	Rationale string `json:"rationale,omitempty"`
	// Status 当前状态（见 CandidateStatus）。
	Status CandidateStatus `json:"status"`
	// Report 最近一次验证报告（Phase B 门禁链的产物）。
	Report *VerificationReport `json:"report,omitempty"`
	// Created 创建时间（UTC 秒精度，固定精度保证记录字节稳定）。
	Created time.Time `json:"created"`
	// Updated 最近更新时间。
	Updated time.Time `json:"updated"`
}

// 候选类型。
const (
	// KindTopic 记忆主题候选（Phase B/C）。
	KindTopic = "topic"
	// KindSkill 技能候选（Phase D）。
	KindSkill = "skill"
)

// CandidateStatus 候选状态机。
//
// 状态机是**单向且显式**的：任何 Verify / Reject 之外的操作都不能把候选从
// Rejected/Failed 改回 Verified（那等于"验证失败也能进索引"）。回退的唯一
// 合法路径是重新写入一条新候选（新的 Created/Rationale，可追溯）。
//
//	Pending → Verified → Promoted
//	Pending → Rejected（静态门禁失败）
//	Pending → Failed（Replay 执行失败/判定不通过）
type CandidateStatus string

const (
	// StatusPending 待验证（刚落盘）。
	StatusPending CandidateStatus = "pending"
	// StatusVerified 门禁链全通过，尚未写入可召回索引。
	StatusVerified CandidateStatus = "verified"
	// StatusRejected 静态门禁失败，**永久**不进索引。
	StatusRejected CandidateStatus = "rejected"
	// StatusFailed Replay 未通过，**永久**不进索引。
	StatusFailed CandidateStatus = "failed"
	// StatusPromoted 已写入 topics/ 与 MEMORY.md（Phase A 的 WriteTopic 路径）。
	StatusPromoted CandidateStatus = "promoted"
)

// terminalBadStatus 判断状态是否为"已被拒绝"（不能进索引）。
func terminalBadStatus(s CandidateStatus) bool {
	return s == StatusRejected || s == StatusFailed
}

// Sentinel 错误（调用点用 errors.Is 判别，不解析错误字符串）。
var (
	// ErrCandidateNotFound 候选不存在。
	ErrCandidateNotFound = errors.New("evolution: candidate not found")
	// ErrNotVerified 候选未通过门禁；调用方不得把它写进可召回索引。
	ErrNotVerified = errors.New("evolution: candidate did not pass validation")
	// ErrScopeMismatch 候选的 RepoID 与当前仓库不一致（跨仓库污染，拒绝）。
	ErrScopeMismatch = errors.New("evolution: candidate repo id does not match this repository")
	// ErrNoReplayer 未注入 Replayer，且策略要求必须 Replay。
	ErrNoReplayer = errors.New("evolution: no replayer configured")
	// ErrForbiddenTarget 候选试图自改 Phase C 禁止的对象（评估器/权限/Sandbox/信任…）。
	ErrForbiddenTarget = errors.New("evolution: candidate targets a forbidden self-modification surface")
)

// DeriveCandidateID 由内容派生候选 ID。
//
// 口径：<主题名 slug>-<内容+创建时间的 8 位哈希>。确定性是刻意的——同一份内容
// 在同一时刻重复提交得到同一个 ID，落盘是覆盖而不是堆积副本；内容变了哈希就变，
// 于是"改过内容的候选"与"原候选"在文件层面天然可区分。
func DeriveCandidateID(t *memory.Topic, created time.Time) string {
	sum := sha256.Sum256([]byte(t.Name + "\x00" + t.Body + "\x00" + t.Title + "\x00" + created.UTC().Format(time.RFC3339)))
	return sanitizeSegment(t.Name) + "-" + hex.EncodeToString(sum[:])[:8]
}

// sanitizeSegment 把任意主题名压成可作单级路径名的小写连字符串。
// 非 slug 字符一律替换（ID 是我们自己生成的，不是模型给的，替换是安全的）。
func sanitizeSegment(s string) string {
	var b strings.Builder
	prevDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "candidate"
	}
	if len(out) > 64 {
		out = strings.Trim(out[:64], "-")
	}
	return out
}

// CandidateStore 候选存储接口。
//
// 抽成接口的两个理由：(1) 测试可以用内存实现，不触碰真实仓库记忆目录；
// (2) 宿主将来若要把候选放到别处（例如集中式实验目录），不需要改门禁逻辑。
//
// 所有实现必须满足：原子写、目录 0700 / 文件 0600、**不触碰** topics/ 与 MEMORY.md。
type CandidateStore interface {
	// RepoID 所属仓库身份 id。
	RepoID() string
	// Save 落盘（覆盖同 ID）。
	Save(c *Candidate) error
	// Get 读一个候选；不存在返回包裹 ErrCandidateNotFound 的错误。
	Get(id string) (*Candidate, error)
	// List 列出全部候选（按 ID 升序，确定性）。
	List() ([]*Candidate, error)
	// Delete 删除一个候选（用于 Promote 成功后的清理；不删历史记录）。
	Delete(id string) error
	// Dir 候选目录绝对路径（审计用）。
	Dir() string
}

// FileCandidateStore 落盘实现：<repo>/evolution/candidates/<id>.json。
type FileCandidateStore struct {
	repoID string
	dirs   evolutionDirs
	mu     chainLock
}

// NewFileCandidateStore 在仓库记忆目录下建立候选存储。
//
// 注意它接收的是 **memory.Store.Dir()**（= <root>/repos/<id>/），而不是
// repo id 字符串：候选因此天然继承 Phase A 的 Repository-scoped 身份，
// 跨仓库共用同一个根目录名是不可能的。
func NewFileCandidateStore(repoDir, repoID string) (*FileCandidateStore, error) {
	if err := safeSegment("repo", repoID); err != nil {
		return nil, err
	}
	return &FileCandidateStore{repoID: repoID, dirs: newEvolutionDirs(repoDir)}, nil
}

func (s *FileCandidateStore) RepoID() string { return s.repoID }
func (s *FileCandidateStore) Dir() string    { return s.dirs.candidates }

func (s *FileCandidateStore) path(id string) (string, error) {
	if err := safeSegment("candidate", id); err != nil {
		return "", err
	}
	p, err := joinSegment(s.dirs.candidates, "candidate", id+".json")
	if err != nil {
		return "", err
	}
	return p, nil
}

func (s *FileCandidateStore) Save(c *Candidate) error {
	if c == nil {
		return errors.New("evolution: candidate is nil")
	}
	if err := c.normalize(s.repoID); err != nil {
		return err
	}
	p, err := s.path(c.ID)
	if err != nil {
		return err
	}
	unlock := s.mu.lock()
	defer unlock()
	return writeJSON(p, c)
}

func (s *FileCandidateStore) Get(id string) (*Candidate, error) {
	p, err := s.path(id)
	if err != nil {
		return nil, err
	}
	var c Candidate
	if err := readJSON(p, &c); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrCandidateNotFound, id)
		}
		return nil, err
	}
	if c.ID == "" {
		c.ID = strings.TrimSuffix(filepath.Base(p), ".json")
	}
	return &c, nil
}

func (s *FileCandidateStore) List() ([]*Candidate, error) {
	entries, err := os.ReadDir(s.dirs.candidates)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 还没有候选：空列表而非错误
		}
		return nil, fmt.Errorf("evolution: list candidates in %s: %w", s.dirs.candidates, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	out := make([]*Candidate, 0, len(names))
	for _, n := range names {
		c, err := s.Get(n)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *FileCandidateStore) Delete(id string) error {
	p, err := s.path(id)
	if err != nil {
		return err
	}
	unlock := s.mu.lock()
	defer unlock()
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("evolution: delete candidate %s: %w", id, err)
	}
	return nil
}

// normalize 规范化候选并做**落盘前**的最小自检。
//
// 自检项只有"不会破坏文件系统和跨仓库隔离"的那些：id 合法、kind 已知、
// topic 名合法、内容是合法 UTF-8。**内容门禁不在这里**——那是 Validator 链
// 的职责，且必须可注入（Phase B 的设计要求）。这里多做一点校验会让
// "门禁链是唯一的失败原因"这个不变量失真。
func (c *Candidate) normalize(repoID string) error {
	if c.ID == "" {
		return errors.New("evolution: candidate id is empty")
	}
	if err := safeSegment("candidate", c.ID); err != nil {
		return err
	}
	if c.RepoID == "" {
		c.RepoID = repoID
	}
	if c.RepoID != repoID {
		return fmt.Errorf("%w: candidate %s belongs to %s, store is %s", ErrScopeMismatch, c.ID, c.RepoID, repoID)
	}
	if c.Kind == "" {
		c.Kind = KindTopic
	}
	switch c.Kind {
	case KindTopic:
		valid, err := memory.ValidateTopic(&c.Topic)
		if err != nil {
			return fmt.Errorf("evolution: candidate %s topic: %w", c.ID, err)
		}
		c.Topic = *valid
	case KindSkill:
		if c.Skill == nil {
			return fmt.Errorf("evolution: candidate %s: kind=skill but skill payload is nil", c.ID)
		}
		if err := c.Skill.normalize(); err != nil {
			return fmt.Errorf("evolution: candidate %s skill: %w", c.ID, err)
		}
	default:
		return fmt.Errorf("evolution: candidate %s has unknown kind %q", c.ID, c.Kind)
	}
	if !validUTF8(c.Rationale) {
		return fmt.Errorf("evolution: candidate %s rationale is not valid UTF-8", c.ID)
	}
	c.Created = c.Created.UTC().Truncate(time.Second)
	c.Updated = c.Updated.UTC().Truncate(time.Second)
	return nil
}

// Name 返回候选的可读名字（主题名或技能名），用于日志与报告。
func (c *Candidate) Name() string {
	if c.Kind == KindSkill && c.Skill != nil {
		return c.Skill.Name
	}
	return c.Topic.Name
}

// TopicRef 返回候选若晋级后的记忆引用（"topic:<name>"）。
func (c *Candidate) TopicRef() string { return "topic:" + c.Topic.Name }
