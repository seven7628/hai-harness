package evolution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// --- MemoryRefs：记忆 → 技能的引用关联 ------------------------------------

// MemoryRef 技能与记忆的引用关联。
//
// **它没有加载语义，也没有权限语义。** 这条边界必须保持：Phase A 的契约是
// "Agent 按索引自主判断并 memory_view 读取"。如果 MemoryRef 变成自动加载钩子，
// 就会 (1) 让技能在 Agent 不知情时把记忆内容塞进上下文，(2) 绕过 Subagent
// 默认隔离（技能对 subagent 可见），(3) 让 Prompt Cache 里多出一层不可预期的
// 上下文。MemoryRef 只是"这个技能的结论来自哪条记忆"的溯源信息，
// 谁读、什么时候读，仍由 Agent 自己决定。
type MemoryRef struct {
	// Ref 规范化后的记忆引用（"topic:<name>"）。
	Ref string `json:"ref"`
	// Relation 关联性质（见 RefRelation）。
	Relation RefRelation `json:"relation"`
	// Note 关联理由（人可读）。
	Note string `json:"note,omitempty"`
}

// RefRelation 引用性质。
type RefRelation = string

const (
	// RefDerived 技能的结论由这条记忆推出。
	RefDerived RefRelation = "derived"
	// RefSupports 记忆支持技能的某个做法。
	RefSupports RefRelation = "supports"
	// RefContradicts 记忆与技能冲突（**待解决**，不是授权覆盖）。
	//
	// 为什么把冲突显式建模：一条"与技能矛盾"的记忆被静默忽略时，Agent 会同时
	// 持有两个矛盾说法并随机选一个。显式记成 contradicts，让宿主评估阶段能
	// 把它识别为需要人处理的问题，而不是一个已解决的项。
	RefContradicts RefRelation = "contradicts"
)

func validRelation(r RefRelation) bool {
	switch r {
	case RefDerived, RefSupports, RefContradicts:
		return true
	}
	return false
}

// skillRecord 一条技能候选/已晋级技能的完整记录。
//
// 落盘：<repo>/evolution/skills/<name>/candidate.json 或 active.json。
//
// 与 Phase B 的记忆候选**共用** Candidate 结构与门禁链（见 SkillCandidate），
// 但**不共用落盘目录**：记忆候选在 candidates/，技能候选在 skills/。理由是
// 二者的晋升路径完全不同 —— 记忆晋级走 Phase A 的 WriteTopic（写 topics/ +
// MEMORY.md），技能晋级写的是 SKILL.md 目录。混在一个目录里会让"晋级"
// 这个词的含义变得不确定。
type skillRecord struct {
	// Name 技能名。
	Name string `json:"name"`
	// Status 技能状态（见 SkillStatus）。
	Status SkillStatus `json:"status"`
	// Candidate 技能候选内容（含 MemoryRefs）。
	Candidate SkillCandidate `json:"candidate"`
	// Report 最近一次门禁报告。
	Report *VerificationReport `json:"report,omitempty"`
	// Version 技能版本（每次晋级/修订递增）。
	Version int `json:"version"`
	// Created 创建时间。
	Created time.Time `json:"created"`
	// Updated 最近更新时间。
	Updated time.Time `json:"updated"`
	// History 状态/版本变更历史。
	History []SkillStep `json:"history,omitempty"`
}

// SkillStatus 技能状态。
type SkillStatus = string

const (
	// SkillCandidatePending 待验证。
	SkillCandidatePending SkillStatus = "candidate"
	// SkillCandidateVerified 通过门禁，尚未激活。
	SkillCandidateVerified SkillStatus = "verified"
	// SkillCandidateRejected 门禁失败。
	SkillCandidateRejected SkillStatus = "rejected"
	// SkillCandidateFailed Replay 未通过。
	SkillCandidateFailed SkillStatus = "failed"
	// SkillActive 已激活（宿主已把它落成 SKILL.md）。
	SkillActive SkillStatus = "active"
	// SkillRevoked 已撤销（负迁移后宿主回退）。
	SkillRevoked SkillStatus = "revoked"
)

// SkillStep 一次状态或版本变更。
type SkillStep struct {
	From   SkillStatus `json:"from"`
	To     SkillStatus `json:"to"`
	Reason string      `json:"reason"`
	At     time.Time   `json:"at"`
}

// Skill 阶段错误哨兵。
var (
	// ErrSkillNotFound 该技能记录不存在。
	ErrSkillNotFound = errors.New("evolution: skill record not found")
	// ErrSkillNotVerified 技能未通过门禁，不能激活。
	ErrSkillNotVerified = errors.New("evolution: skill candidate did not pass validation")
	// ErrNoSkillInstaller 未配置 SkillInstaller，无法真正落成 SKILL.md。
	ErrNoSkillInstaller = errors.New("evolution: no skill installer configured; the host must inject one")
	// ErrBrokenMemoryRef 技能引用了一条不存在的记忆。
	ErrBrokenMemoryRef = errors.New("evolution: skill references a memory topic that does not exist")
	// ErrNegativeMigration 联合评估判定为负迁移。
	ErrNegativeMigration = errors.New("evolution: joint evaluation detected negative migration")
)

// SkillInstaller 把已验证的技能候选落成真正的技能目录（SKILL.md）。
//
// 为什么是接口而不是直接写文件：技能目录的发现路径、命名空间与覆盖优先级由
// skills.Registry 决定（全局 ~/.agents/skills、~/.go-code/skills，工作区同名覆盖），
// 那是宿主装配的事。本包**不依赖 skills/ 包**（避免 skills/ 反向依赖 evolution/），
// 也不自己猜该写进哪个命名空间 —— 猜错的后果是技能加载不到或者覆盖了用户的。
//
// 注入的实现应当复用 skills.Skill 的字段语义（Name/Description/Instructions/
// Version/MemoryRefs 落在 metadata 里）。
type SkillInstaller interface {
	// Install 落成一个可被 skills.Registry 发现的技能目录；返回目录绝对路径。
	Install(c *SkillCandidate) (dir string, err error)
	// Remove 撤销一个已安装的技能（负迁移回退用）。
	Remove(name string) error
}

// SkillInstallerFunc 把普通函数适配成 SkillInstaller。
type SkillInstallerFunc struct {
	InstallFn func(c *SkillCandidate) (string, error)
	RemoveFn  func(name string) error
}

func (f SkillInstallerFunc) Install(c *SkillCandidate) (string, error) {
	if f.InstallFn == nil {
		return "", ErrNoSkillInstaller
	}
	return f.InstallFn(c)
}

func (f SkillInstallerFunc) Remove(name string) error {
	if f.RemoveFn == nil {
		return ErrNoSkillInstaller
	}
	return f.RemoveFn(name)
}

// --- 技能候选的提交与验证 -------------------------------------------------

// SubmitSkill 提交一条技能候选（复用 Phase B 的门禁链与 Replayer）。
//
// 技能候选走的是**同一个** Verify 路径：同一批 Validator、同一个 Replayer、
// 同一份报告格式。理由写在 Phase B 的设计里：两套门禁语义只会有一个被修。
func (s *Service) SubmitSkill(c *SkillCandidate, rationale string) (*Candidate, error) {
	if c == nil {
		return nil, errors.New("evolution: skill candidate is nil")
	}
	// 引用完整性：技能引用的每条记忆都必须真实存在。
	//
	// 这条检查必须在**提交时**做而不是激活时：引用一条不存在的记忆的技能，
	// 即使自身内容完全正确，它的溯源链也是断的，而断链在事后无法追溯修复。
	if err := s.checkMemoryRefs(c.MemoryRefs); err != nil {
		return nil, err
	}
	now := s.now()
	cand := &Candidate{
		Kind:      KindSkill,
		Source:    "agent",
		Rationale: rationale,
		Skill:     c,
		Status:    StatusPending,
		Created:   now,
		Updated:   now,
		RepoID:    s.cfg.Store.RepoID(),
	}
	submitted, err := s.Submit(cand)
	if err != nil {
		return nil, err
	}
	// 同步写技能侧记录：SubmitSkill 是技能的入口，调用方不应该为了"看看这个
	// 技能什么状态"还得记得自己留一份。candidate.json 记的是**技能状态**，
	// 而 Candidate 记的是**门禁状态**，两者不是一回事（见 skillRecord 注释）。
	if err := s.writeSkillRecord(submitted.Skill, SkillCandidatePending, nil, "submitted"); err != nil {
		return nil, err
	}
	return submitted, nil
}

// checkMemoryRefs 校验技能引用的记忆主题都存在。
func (s *Service) checkMemoryRefs(refs []string) error {
	for _, ref := range refs {
		name, err := normalizeMemoryRef(ref)
		if err != nil {
			return err
		}
		topic := strings.TrimPrefix(name, "topic:")
		if _, err := s.cfg.Store.ReadTopic(topic); err != nil {
			return fmt.Errorf("%w: skill references %s", ErrBrokenMemoryRef, ref)
		}
	}
	return nil
}

// ActivateSkill 把已验证的技能候选激活（经 SkillInstaller 落成 SKILL.md）。
//
// 只有 SkillCandidateVerified 能激活 —— 与记忆晋级同一条不变式：
// 未通过门禁的内容不进入运行时可见集合。
func (s *Service) ActivateSkill(c *Candidate) (string, error) {
	if c == nil || c.Skill == nil {
		return "", errors.New("evolution: skill candidate is nil")
	}
	if c.Status != StatusVerified {
		return "", fmt.Errorf("%w: %s is %q", ErrSkillNotVerified, c.Name(), c.Status)
	}
	if s.cfg.SkillInstaller == nil {
		return "", ErrNoSkillInstaller
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, err := s.cfg.Candidates.Get(c.ID)
	if err != nil {
		return "", err
	}
	if stored.Status != StatusVerified {
		return "", fmt.Errorf("%w: %s is %q on disk", ErrSkillNotVerified, c.Name(), stored.Status)
	}
	// 激活前再验一次引用完整性：记忆可能在上次 Verify 之后被归档了。
	if err := s.checkMemoryRefs(stored.Skill.MemoryRefs); err != nil {
		return "", err
	}
	dir, err := s.cfg.SkillInstaller.Install(stored.Skill)
	if err != nil {
		// 安装失败要**留下记录**（状态退回 failed），而不是什么都没发生。
		// 否则候选停在 verified，宿主重试时会误以为"还没试过"，而技能其实
		// 可能已经被装了一半。
		_ = s.writeSkillRecord(stored.Skill, SkillCandidateFailed, stored.Report, "install failed: "+err.Error())
		return "", err
	}
	if err := s.writeSkillRecord(stored.Skill, SkillActive, stored.Report, "activated"); err != nil {
		return "", err
	}
	stored.Status = StatusPromoted // 候选记录本身也标记为已晋级
	stored.Updated = s.now()
	if err := s.cfg.Candidates.Save(stored); err != nil {
		return "", err
	}
	return dir, nil
}

// RevokeSkill 撤销一个已激活技能（负迁移后的回退入口）。
func (s *Service) RevokeSkill(name, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return ErrNoReason
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.loadSkillRecord(name)
	if err != nil {
		return err
	}
	if s.cfg.SkillInstaller != nil {
		if err := s.cfg.SkillInstaller.Remove(name); err != nil {
			return err
		}
	}
	return s.writeSkillRecord(&rec.Candidate, SkillRevoked, rec.Report, reason)
}

// SkillRecord 读一个技能的记录。
func (s *Service) SkillRecord(name string) (*skillRecord, error) { return s.loadSkillRecord(name) }

// --- 技能记录存储 --------------------------------------------------------

func (s *Service) skillsDir() string { return s.dirsCache.skills }

func (s *Service) skillRecordPath(name, kind string) (string, error) {
	n := sanitizeSegment(name)
	if n == "" {
		return "", errors.New("evolution: empty skill name")
	}
	if !validUTF8(kind) || strings.ContainsAny(kind, `/\.`) {
		return "", fmt.Errorf("evolution: bad skill record kind %q", kind)
	}
	dir, err := joinSegment(s.skillsDir(), "skill", n)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, kind+".json"), nil
}

// loadSkillRecord 读一个技能的记录。
//
// 优先级：**candidate.json 覆盖 active.json**。反过来会让 revoke 之后
// loadSkillRecord 仍从 active.json 读回旧状态（撤销"没生效"），因为
// active.json 是激活时刻的快照，只有 candidate.json 才是当前真相。
func (s *Service) loadSkillRecord(name string) (*skillRecord, error) {
	for _, kind := range []string{"candidate", "active"} {
		p, err := s.skillRecordPath(name, kind)
		if err != nil {
			return nil, err
		}
		var rec skillRecord
		if err := readJSON(p, &rec); err == nil {
			return &rec, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrSkillNotFound, name)
}

// writeSkillRecord 写技能的当前记录（candidate.json 是权威副本）。
//
// 状态为 active 时**额外**写一份 active.json 快照，作为"这个技能确实被装上了"
// 的审计证据：它回答的是"装过没有"，不是"现在是什么状态"（那是 candidate.json）。
func (s *Service) writeSkillRecord(c *SkillCandidate, status SkillStatus, report *VerificationReport, reason string) error {
	if c == nil {
		return errors.New("evolution: skill candidate is nil")
	}
	p, err := s.skillRecordPath(c.Name, "candidate")
	if err != nil {
		return err
	}
	var rec skillRecord
	if existing, err := s.loadSkillRecord(c.Name); err == nil {
		rec = *existing
	} else if !errors.Is(err, ErrSkillNotFound) {
		return err
	}
	from := rec.Status
	if from == "" {
		from = SkillCandidatePending
		rec.Version = 1
	}
	rec.Name, rec.Candidate, rec.Status, rec.Report = c.Name, *c, status, report
	if rec.Created.IsZero() {
		rec.Created = s.now()
	}
	rec.Updated = s.now()
	if status != from {
		rec.History = append(rec.History, SkillStep{From: from, To: status, Reason: reason, At: s.now()})
	}
	if err := writeJSON(p, rec); err != nil {
		return err
	}
	if status == SkillActive {
		ap, err := s.skillRecordPath(c.Name, "active")
		if err != nil {
			return err
		}
		return writeJSON(ap, rec)
	}
	return nil
}

// ListSkills 列出全部技能记录（按名字升序）。
func (s *Service) ListSkills() ([]*skillRecord, error) {
	entries, err := os.ReadDir(filepath.Join(s.skillsDir(), "skill"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("evolution: list skills: %w", err)
	}
	var out []*skillRecord
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(s.skillsDir(), "skill", e.Name(), "candidate.json")
		var rec skillRecord
		if err := readJSON(p, &rec); err != nil {
			continue // 损坏记录跳过，不阻塞列举
		}
		out = append(out, &rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// --- 联合 Replay ---------------------------------------------------------

// JointArm 联合评估的实验臂（2×2 对照）。
type JointArm string

const (
	// ArmNone 都不开（基线）。
	ArmNone JointArm = "none"
	// ArmMemory 只开记忆。
	ArmMemory JointArm = "memory"
	// ArmSkill 只开技能。
	ArmSkill JointArm = "skill"
	// ArmBoth 记忆 + 技能（联合）。
	ArmBoth JointArm = "both"
)

// JointArms 2×2 对照的四个臂（顺序固定，便于报告复现）。
func JointArms() []JointArm { return []JointArm{ArmNone, ArmMemory, ArmSkill, ArmBoth} }

// Valid 判断臂名是否已知。
func (a JointArm) Valid() bool {
	switch a {
	case ArmNone, ArmMemory, ArmSkill, ArmBoth:
		return true
	}
	return false
}

// JointConfig 一条臂的装配配置（宿主 Replay 执行器按它决定"这一臂能看到什么"）。
//
// 边界（重要）：本包**只描述**一臂该有什么，不去实施。是否真的把记忆/技能喂给
// 模型、喂多少、怎么配平预算，全部是宿主执行器的职责。理由：2×2 对照的效度
// 取决于执行时是否严格配平（同任务、同模型、同预算、同 fixture），而这些
// 只有宿主知道；本包若参与实施，就有了"悄悄配平失败"的可能 —— 而配平失败
// 的 2×2 会让"联合有效"的结论完全无效。
type JointConfig struct {
	// Arm 实验臂。
	Arm JointArm `json:"arm"`
	// MemoryRefs 该臂可见的记忆引用（ArmNone 为空；ArmMemory/ArmBoth 非空）。
	MemoryRefs []string `json:"memory_refs,omitempty"`
	// SkillNames 该臂可见的技能（ArmSkill/ArmBoth 非空）。
	SkillNames []string `json:"skill_names,omitempty"`
	// Budget 该臂的预算（与其它臂必须相等，否则对照无效）。
	Budget time.Duration `json:"budget"`
}

// JointRequest 一次联合 Replay 的输入。
type JointRequest struct {
	// TaskID 任务标识（配对复现的配对键）。
	TaskID string `json:"task_id"`
	// Configs 四个臂的配置。
	Configs []JointConfig `json:"configs"`
	// Budget 统一预算（四臂必须相同）。
	Budget time.Duration `json:"budget"`
}

// JointArmResult 单臂结果。
type JointArmResult struct {
	// Arm 实验臂。
	Arm JointArm `json:"arm"`
	// Success 是否达成任务目标。
	Success bool `json:"success"`
	// Score 任务得分（宿主给的连续指标；0 = 宿主没给）。
	Score float64 `json:"score"`
	// Cost 成本读数（token / 时延，宿主定义）。
	Cost float64 `json:"cost"`
	// Note 备注。
	Note string `json:"note,omitempty"`
}

// JointVerdict 联合评估结论。
type JointVerdict struct {
	// TaskID 任务标识。
	TaskID string `json:"task_id"`
	// Results 各臂结果。
	Results map[JointArm]JointArmResult `json:"results"`
	// GainOverNone 联合臂相对无记忆无技能基线的收益。
	GainOverNone float64 `json:"gain_over_none"`
	// GainMemoryOnly 仅记忆臂的收益。
	GainMemoryOnly float64 `json:"gain_memory_only"`
	// GainSkillOnly 仅技能臂的收益。
	GainSkillOnly float64 `json:"gain_skill_only"`
	// Residual 联合臂不能被单侧解释的残差收益。
	//
	// 口径：残差 = (联合收益) - max(仅记忆收益, 仅技能收益)。
	// 交互项的常见写法是 both - memory - skill，但那会**恒为负**（两臂各自
	// 有效时必然重叠），无法用来宣称联合有效。这里取"超出最好单侧的部分"，
	// 因为要回答的问题是"联合是否比最好的单侧更好"，而不是"两个收益是否可以
	// 简单相加"。
	Residual float64 `json:"residual"`
	// NegativeMigration 联合臂是否造成负迁移。
	//
	// 判据：联合臂得分低于**任一**单侧臂。低于基线（ArmNone）是"没收益"，
	// 低于单侧臂才是"负迁移"——联合把已经有效的单侧能力弄坏了。
	NegativeMigration bool `json:"negative_migration"`
	// JointEffective 能否宣称联合有效。
	//
	// 三个条件**全部**满足才可以宣称（评估协议第 9 节的硬要求）：
	//  1. 残差 > MinResidual（收益不能被单侧完全解释）；
	//  2. 无负迁移；
	//  3. 提升超过 opts.MinGain（噪声地板由宿主给，本包不猜）。
	JointEffective bool `json:"joint_effective"`
	// Reasons 判定理由（人可读，报告直接用）。
	Reasons []string `json:"reasons"`
}

// JointOptions 联合评估的判定阈值。
type JointOptions struct {
	// MinResidual 宣称联合有效所需的最小残差收益（零值 = 不额外要求残差为正）。
	MinResidual float64
	// MinGain 相对基线的最小收益（**噪声地板**；宿主按评估协议填）。
	MinGain float64
}

// JointEvaluate 汇总四臂结果并给出是否"联合有效"的判定。
//
// 这是 Phase D 最关键的一个函数，因为它守着方案里最容易被绕过的一条：
// 只有当联合臂的收益**不能被 Memory 或 Skill 单独解释**、且**没有负迁移**时，
// 才允许宣称 Memory + Skill 联合有效。三个条件里任何一个不满足，结论都是
// "不可宣称"，而不是"部分有效"。
//
// 注意它**不做**任何实验编排：调用方必须先按 JointRequest 跑完四臂再调它。
// 本包不发起任务、不调模型，因为配平的可信度只能来自宿主。
func JointEvaluate(req JointRequest, results map[JointArm]JointArmResult, opts JointOptions) JointVerdict {
	v := JointVerdict{TaskID: req.TaskID, Results: results}
	none, hasNone := results[ArmNone]
	mem, hasMem := results[ArmMemory]
	skill, hasSkill := results[ArmSkill]
	both, hasBoth := results[ArmBoth]
	if !hasNone || !hasMem || !hasSkill || !hasBoth {
		v.Reasons = append(v.Reasons, "incomplete: the 2x2 comparison needs all four arms (none/memory/skill/both)")
		return v
	}
	if req.Budget <= 0 {
		v.Reasons = append(v.Reasons, fmt.Sprintf("no budget recorded: arms are not comparable (%s)", req.Budget))
		return v
	}

	v.GainOverNone = both.Score - none.Score
	v.GainMemoryOnly = mem.Score - none.Score
	v.GainSkillOnly = skill.Score - none.Score
	best := v.GainMemoryOnly
	if v.GainSkillOnly > best {
		best = v.GainSkillOnly
	}
	v.Residual = v.GainOverNone - best

	// 负迁移：联合臂比任一单侧臂都差。
	if both.Score < mem.Score || both.Score < skill.Score {
		v.NegativeMigration = true
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"negative migration: joint score %.3f is below memory-only %.3f and/or skill-only %.3f",
			both.Score, mem.Score, skill.Score))
	}
	if v.GainOverNone < opts.MinGain {
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"gain over baseline %.3f is below the noise floor %.3f: inconclusive", v.GainOverNone, opts.MinGain))
	}
	if v.Residual <= opts.MinResidual {
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"residual %.3f is not above the threshold %.3f: the joint gain is explained by a single side",
			v.Residual, opts.MinResidual))
	}
	if v.NegativeMigration {
		v.Reasons = append(v.Reasons, "joint effectiveness cannot be claimed while negative migration is present")
	}
	if v.GainOverNone >= opts.MinGain && v.Residual > opts.MinResidual && !v.NegativeMigration {
		v.JointEffective = true
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"joint effective: gain %.3f over baseline, residual %.3f above threshold %.3f, no negative migration",
			v.GainOverNone, v.Residual, opts.MinResidual))
	}
	return v
}

// JointReplayer 联合 Replay 执行器（可注入，宿主实现）。
//
// 与 Phase B 的 Replayer 一样：默认不执行任何真实 LLM 调用。宿主注入后才产生
// 模型调用。本包只提供请求描述与结果汇总。
type JointReplayer interface {
	// Name 执行者名。
	Name() string
	// RunJoint 按四个臂的配平配置各跑一次，返回各臂结果。
	RunJoint(ctx context.Context, req JointRequest) (map[JointArm]JointArmResult, error)
}

// JointReplayerFunc 把普通函数适配成 JointReplayer。
type JointReplayerFunc struct {
	ReplayerName string
	Fn           func(ctx context.Context, req JointRequest) (map[JointArm]JointArmResult, error)
}

func (f JointReplayerFunc) Name() string { return f.ReplayerName }

func (f JointReplayerFunc) RunJoint(ctx context.Context, req JointRequest) (map[JointArm]JointArmResult, error) {
	if f.Fn == nil {
		return nil, errors.New("evolution: joint replayer has nil function")
	}
	return f.Fn(ctx, req)
}

// BuildJointRequest 组装一次 2×2 联合 Replay 的请求。
//
// 它**不**检查配平（预算、任务、fixture 是否一致），只保证四臂的"可见集合"
// 符合 2×2 定义：none 什么都没有；memory 只有记忆；skill 只有技能；both 两者都有。
// 把这个"可见集合"显式写出来，是因为它就是对照实验的自变量 —— 任何一条臂
// 多看了不该看的东西，整个结论就作废了。
func (s *Service) BuildJointRequest(taskID string, memoryRefs, skillNames []string, budget time.Duration) (JointRequest, error) {
	if strings.TrimSpace(taskID) == "" {
		return JointRequest{}, errors.New("evolution: task id is required")
	}
	if budget <= 0 {
		return JointRequest{}, errors.New("evolution: a positive budget is required; unpaired arms are not comparable")
	}
	// 记忆引用必须真实存在，否则"only memory"臂实际看到的是空记忆。
	if err := s.checkMemoryRefs(memoryRefs); err != nil {
		return JointRequest{}, err
	}
	// 技能必须已激活，否则"only skill"臂看到的是一个 Agent 根本加载不了的东西。
	active := make([]string, 0, len(skillNames))
	for _, n := range skillNames {
		rec, err := s.loadSkillRecord(n)
		if err != nil {
			return JointRequest{}, err
		}
		if rec.Status != SkillActive {
			return JointRequest{}, fmt.Errorf("%w: skill %q is %q, only active skills can be used in an arm", ErrSkillNotVerified, n, rec.Status)
		}
		active = append(active, rec.Name)
	}
	sort.Strings(active)
	refs := append([]string(nil), memoryRefs...)
	sort.Strings(refs)

	return JointRequest{
		TaskID: taskID,
		Budget: budget,
		Configs: []JointConfig{
			{Arm: ArmNone, Budget: budget},
			{Arm: ArmMemory, MemoryRefs: refs, Budget: budget},
			{Arm: ArmSkill, SkillNames: active, Budget: budget},
			{Arm: ArmBoth, MemoryRefs: refs, SkillNames: active, Budget: budget},
		},
	}, nil
}

// RunJoint 做一次联合 Replay 并给出判定。
//
// 这是"默认不做真实 LLM 调用"这条约束的落点：没有注入 JointReplayer 时，
// RunJoint **直接返回判定为不可宣称**的结论，而不是编一个空结果让调用方
// 误以为实验跑过了。
func (s *Service) RunJoint(ctx context.Context, req JointRequest, opts JointOptions) (JointVerdict, error) {
	v := JointVerdict{TaskID: req.TaskID, Reasons: []string{}}
	if s.cfg.JointReplayer == nil {
		v.Reasons = append(v.Reasons, "no joint replayer configured: the experiment did not run, so nothing can be claimed")
		return v, ErrNoReplayer
	}
	if len(req.Configs) != len(JointArms()) {
		v.Reasons = append(v.Reasons, "the request must describe all four arms")
		return v, fmt.Errorf("evolution: joint replay needs %d arms, got %d", len(JointArms()), len(req.Configs))
	}
	// 配平自检：四臂预算必须相同。这是本包能做的唯一一条"实验效度"检查 ——
	// 预算不一致的 2×2 在统计上无法比较，宿主跑完也白跑。
	budget := req.Configs[0].Budget
	for i, c := range req.Configs {
		if c.Budget != budget {
			v.Reasons = append(v.Reasons, fmt.Sprintf("arm %d has budget %s but the first arm has %s: the arms are unpaired", i, c.Budget, budget))
			return v, errors.New("evolution: unpaired joint replay (arm budgets differ)")
		}
		if !c.Arm.Valid() {
			v.Reasons = append(v.Reasons, fmt.Sprintf("unknown arm %q", c.Arm))
			return v, fmt.Errorf("evolution: unknown joint arm %q", c.Arm)
		}
	}
	results, err := s.cfg.JointReplayer.RunJoint(ctx, req)
	if err != nil {
		v.Reasons = append(v.Reasons, "joint replay failed: "+err.Error())
		return v, err
	}
	if len(results) != len(JointArms()) {
		v.Reasons = append(v.Reasons, "the joint replayer did not return all four arms")
		return v, fmt.Errorf("evolution: joint replay returned %d arms, want %d", len(results), len(JointArms()))
	}
	return JointEvaluate(req, results, opts), nil
}

// SkillJointVerdict 某个技能在一次联合评估里的表现。
type SkillJointVerdict struct {
	// SkillName 技能名。
	SkillName string `json:"skill_name"`
	// BaseScore 该技能单独使用时的得分（ArmSkill）。
	BaseScore float64 `json:"base_score"`
	// WithMemoryScore 该技能与记忆同时使用时的得分（ArmBoth）。
	WithMemoryScore float64 `json:"with_memory_score"`
	// Delta 联合相对单独的差值（负 = 与记忆搭配变差）。
	Delta float64 `json:"delta"`
	// NegativeTransfer 是否判定为负迁移。
	//
	// 判据：Delta 低于阈值 **或** 存在该技能的 harmful 级负证据。两个条件是
	// "或"而不是"且"：一次实测的显著变差足够判负；反复记录的负证据也足够。
	// 两个独立信号各自都不足以确信，但任意一个出现就值得停下来看。
	NegativeTransfer bool `json:"negative_transfer"`
	// Reason 人可读理由。
	Reason string `json:"reason"`
}

// SkillTransferOptions 技能负迁移判定阈值。
type SkillTransferOptions struct {
	// MinDelta 可接受的最小联合增益（负值容忍度；零值 = 联合不允许比单独差）。
	MinDelta float64
	// RequireNoHarmfulEvidence 为真时，存在 harmful 级负证据即判负迁移。
	RequireNoHarmfulEvidence bool
}

// EvaluateSkillTransfer 判定一个技能在与记忆搭配时是否造成负迁移。
//
// 为什么必须单独评估技能：技能是**常驻**的（一旦 load_skill 就进上下文并影响
// 后续所有步骤），而记忆是**按需**的。所以一个"自己单用挺好、但会把 Agent 带偏"
// 的技能，比一个完全没用的技能更糟 —— 它污染的是整段推理。
//
// 负迁移不是"技能没用"：技能单独用仍可能显著优于基线。只有"技能与记忆搭配
// 后比技能单独用更差"（或存在有害负证据）才是负迁移。
func (s *Service) EvaluateSkillTransfer(v JointVerdict, skillName string, opts SkillTransferOptions) SkillJointVerdict {
	out := SkillJointVerdict{SkillName: skillName}
	skill, hasSkill := v.Results[ArmSkill]
	both, hasBoth := v.Results[ArmBoth]
	if !hasSkill || !hasBoth {
		out.Reason = "incomplete: joint evaluation has no skill-only and joint arms"
		return out
	}
	out.BaseScore = skill.Score
	out.WithMemoryScore = both.Score
	out.Delta = both.Score - skill.Score

	evidence, err := s.SummarizeEvidence("skill:" + skillName)
	if err != nil {
		out.Reason = "evidence lookup failed: " + err.Error()
		return out
	}
	harmful := evidence.ByKind[NegHarmful] + evidence.ByKind[NegMigration]

	switch {
	case out.Delta < opts.MinDelta:
		out.NegativeTransfer = true
		out.Reason = fmt.Sprintf("joint use scores %.3f below skill-only %.3f (delta %.3f < %.3f)",
			both.Score, skill.Score, out.Delta, opts.MinDelta)
	case opts.RequireNoHarmfulEvidence && harmful > 0:
		out.NegativeTransfer = true
		out.Reason = fmt.Sprintf("%d harmful/migration evidence entr(y/ies) recorded for skill %q", harmful, skillName)
	case out.Delta == 0 && harmful == 0:
		out.Reason = "no measurable interaction between this skill and memory"
	default:
		out.Reason = fmt.Sprintf("no negative transfer: delta %.3f >= %.3f, %d harmful evidence entries", out.Delta, opts.MinDelta, harmful)
	}
	return out
}
