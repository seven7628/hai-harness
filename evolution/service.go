package evolution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/memory"
)

// Promoter 把一个已通过门禁的记忆写入**可召回**的 Phase A 存储。
//
// 抽成接口而不是直接调 Store.WriteTopic 的原因很实际：Phase A 里
// "写主题 + 同步 MEMORY.md 索引"是 MemoryService.WriteTopic 的一体动作
// （service.go 的 syncIndexEntry），Store 接口只暴露裸写入。直接用
// Store.WriteTopic 会写出"有主题没索引条目"的半状态——正文在、地图里没有，
// Agent 永远看不到它。索引同步是 Phase A 的职责，不能被 evolution 重做一遍，
// 所以这里走注入的 Promoter，由宿主接上 *memory.MemoryService.WriteTopic。
type Promoter interface {
	// Promote 把主题写入 topics/ 并同步 MEMORY.md 索引。
	Promote(topic *memory.Topic) error
}

// PromoterFunc 把普通函数适配成 Promoter。
type PromoterFunc struct {
	Fn func(topic *memory.Topic) error
}

func (f PromoterFunc) Promote(topic *memory.Topic) error {
	if f.Fn == nil {
		return errors.New("evolution: promoter has nil function")
	}
	return f.Fn(topic)
}

// Config 控制面配置。
//
// 零值可用的部分：Validators 为空用 DefaultValidatorChain，Replayer 为空用
// DefaultReplayer（确定性、零 LLM 调用）。**没有**任何字段能让本包自动做事：
// 所有生命周期推进都由宿主显式调用。
type Config struct {
	// Store 记忆存储（Phase A 的 memory.Store）。**必填**：候选的仓库身份、
	//	RepoRoot、既有关联主题都从它来。
	Store memory.Store
	// Promoter 晋级写入器（**必填**）。宿主应接上 *memory.MemoryService.WriteTopic，
	//	以保证"写主题 + 同步索引"由 Phase A 一体完成、限额不被绕过。
	//	为 nil 时 NewService 直接报错——静默退化成裸 Store.WriteTopic 会留下
	//	"有正文没索引"的半状态，比构造失败更糟。
	Promoter Promoter
	// Candidates 候选存储（空 = 在 Store.Dir() 下建默认的 FileCandidateStore）。
	Candidates CandidateStore
	// Validators 静态门禁链（空 = DefaultValidatorChain）。
	Validators []Validator
	// Replayer 单次 Replay 执行器（空 = DefaultReplayer，零 LLM 调用）。
	Replayer Replayer
	// RequireReplay 为真表示"没有 Replayer 就报错"（ErrNoReplayer），
	//	而不是退化成 replay_skipped。宿主想保证每条记忆都被真实验证时打开它。
	RequireReplay bool
	// Now 时钟（空 = time.Now）。测试注入固定时钟。
	Now func() time.Time
}

// Service Phase B/C/D 的统一入口。
//
// 无状态（除绑定的 store 之外），可跨会话复用；所有写操作都走 per-repo 互斥，
// 与 Phase A 的进程内链一致，避免"读-改-写"在候选账本上丢更新。
type Service struct {
	cfg Config
	// hooks Phase A 格式的渲染/解析钩子（结构 Replay 用；由 NewService 填好）。
	hooks StructuralHooks
	mu    sync.Mutex
}

// NewService 构造控制面。
//
// Store / Promoter 为 nil 是配置错误而非可降级项：没有 memory.Store 就无法保证
// Repository-scoped 不变式；没有 Promoter 就会出现"有正文没索引"的半状态。
// 两者都宁可构造失败也不要静默降级。
func NewService(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("evolution: Config.Store is required (repository-scoped memory store)")
	}
	if cfg.Promoter == nil {
		return nil, errors.New("evolution: Config.Promoter is required (must route through Phase A WriteTopic so the index stays authoritative)")
	}
	if cfg.Candidates == nil {
		cs, err := NewFileCandidateStore(cfg.Store.Dir(), cfg.Store.RepoID())
		if err != nil {
			return nil, err
		}
		cfg.Candidates = cs
	}
	if len(cfg.Validators) == 0 {
		cfg.Validators = DefaultValidatorChain()
	}
	if cfg.Replayer == nil {
		cfg.Replayer = DefaultReplayer(DefaultStructuralHooks())
	}
	return &Service{cfg: cfg, hooks: DefaultStructuralHooks()}, nil
}

// DefaultStructuralHooks 用 Phase A 的公开入口填出渲染/解析钩子。
//
// Phase A 没导出 renderTopic/parseTopic，因此这里走"经由 FileStore 的
// WriteTopic/ReadTopic 往返"这一条**公开**路径：写进临时目录再读回来。
// 代价是 Replay 会碰一次磁盘，收益是"结构 Replay 校验的就是真实写盘路径"，
// 而不是我们复制一份格式逻辑（复制 = 迟早与 Phase A 漂移）。
func DefaultStructuralHooks() StructuralHooks {
	return StructuralHooks{
		Render: renderViaPhaseA,
		Parse:  parseViaPhaseA,
	}
}

// renderViaPhaseA 用 Phase A 的写入路径渲染主题（保证与真实落盘字节一致）。
var renderViaPhaseA = func(t *memory.Topic) string {
	dir, err := os.MkdirTemp("", "evolution-render-")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(dir)
	fs, err := memory.NewFileStore(dir, "replay")
	if err != nil {
		return ""
	}
	if err := fs.WriteTopic(t); err != nil {
		return ""
	}
	raw, err := os.ReadFile(mustTopicPath(fs, t.Name))
	if err != nil {
		return ""
	}
	return string(raw)
}

// parseViaPhaseA 用 Phase A 的读取路径解析 Markdown。
var parseViaPhaseA = func(raw string) *memory.Topic {
	dir, err := os.MkdirTemp("", "evolution-parse-")
	if err != nil {
		return nil
	}
	defer os.RemoveAll(dir)
	fs, err := memory.NewFileStore(dir, "replay")
	if err != nil {
		return nil
	}
	// WriteTopic 会建 topics/，但这里直接写文件，所以目录要自己建。
	if err := os.MkdirAll(filepath.Dir(mustTopicPath(fs, "probe")), dirPerm); err != nil {
		return nil
	}
	if err := os.WriteFile(mustTopicPath(fs, "probe"), []byte(raw), filePerm); err != nil {
		return nil
	}
	t, err := fs.ReadTopic("probe")
	if err != nil {
		return nil
	}
	return t
}

// mustTopicPath 拼出主题文件路径（仅测试与内部渲染用；名称已在 Phase A 校验过）。
func mustTopicPath(fs *memory.FileStore, name string) string {
	p, err := fs.TopicPath(name)
	if err != nil {
		return ""
	}
	return p
}

// Store 返回绑定的记忆存储。
func (s *Service) Store() memory.Store { return s.cfg.Store }

// Candidates 返回候选存储。
func (s *Service) Candidates() CandidateStore { return s.cfg.Candidates }

func (s *Service) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now().UTC().Truncate(time.Second)
	}
	return time.Now().UTC().Truncate(time.Second)
}

// Submit 提交一条候选（落盘到 candidates/，状态 Pending）。
//
// 落盘即不可召回：Candidate 落在 evolution/candidates/，而 memory_view 只读
// topics/、BootstrapIndex 只读 MEMORY.md——**结构上**未晋级的候选对 Agent 不可见。
// 这条不变式在 promote_not_recallable_test.go 里有端到端断言。
func (s *Service) Submit(c *Candidate) (*Candidate, error) {
	if c == nil {
		return nil, errors.New("evolution: candidate is nil")
	}
	now := s.now()
	if c.Created.IsZero() {
		c.Created = now
	}
	c.Updated = now
	if c.ID == "" {
		if c.Kind == KindSkill && c.Skill != nil {
			c.ID = DeriveCandidateID(&memory.Topic{Name: c.Skill.Name, Title: c.Skill.Description, Body: c.Skill.Instructions}, c.Created)
		} else {
			c.ID = DeriveCandidateID(&c.Topic, c.Created)
		}
	}
	if c.Status == "" {
		c.Status = StatusPending
	}
	// 落盘前 RepoID 必须与 store 一致（normalize 会做 ErrScopeMismatch 检查）。
	if c.RepoID == "" {
		c.RepoID = s.cfg.Store.RepoID()
	}
	if err := c.normalize(s.cfg.Store.RepoID()); err != nil {
		return nil, err
	}
	if err := s.cfg.Candidates.Save(c); err != nil {
		return nil, err
	}
	return c, nil
}

// Verify 对一条候选跑完整门禁链（静态校验 + 单次 Replay）。
//
// 返回的报告总是**非 nil**（哪怕失败），这样调用方永远能拿到"为什么被拒"。
// 状态变更：全通过 → StatusVerified；任一静态门禁失败 → StatusRejected；
// Replay 判 fail → StatusFailed。**三个终态都不会进索引**。
func (s *Service) Verify(ctx context.Context, id string) (*VerificationReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.verifyLocked(ctx, id)
}

func (s *Service) verifyLocked(ctx context.Context, id string) (*VerificationReport, error) {
	c, err := s.cfg.Candidates.Get(id)
	if err != nil {
		return nil, err
	}
	report := &VerificationReport{CandidateID: c.ID, At: s.now()}

	// 静态门禁链：短路执行，第一个失败即终止。
	for _, v := range s.cfg.Validators {
		if v == nil {
			continue
		}
		if err := v.Check(c); err != nil {
			report.Verdicts = append(report.Verdicts, Verdict{Gate: v.Name(), Pass: false, Reason: err.Error()})
			report.Passed = false
			return s.finish(c, report, StatusRejected)
		}
		report.Verdicts = append(report.Verdicts, Verdict{Gate: v.Name(), Pass: true})
	}

	// 单次 Replay（可注入；默认零 LLM 调用）。
	replayer := s.cfg.Replayer
	if replayer == nil {
		if s.cfg.RequireReplay {
			return nil, ErrNoReplayer
		}
		report.ReplaySkipped = true
	} else {
		req := ReplayRequest{
			Candidate: c,
			RepoRoot:  repoRootOf(s.cfg.Store),
			Baseline:  s.baselineFor(c),
		}
		res, err := replayer.Replay(ctx, req)
		if err != nil {
			// 执行基础设施坏了 → inconclusive，不是"内容有问题"。
			// 记成 fail 状态会误伤好候选；记成 pass 会放进未验证的记忆。
			report.Verdicts = append(report.Verdicts, Verdict{Gate: "replay", Pass: false, Reason: "replay error: " + err.Error()})
			report.Passed = false
			return s.finish(c, report, StatusFailed)
		}
		if res == nil {
			report.Verdicts = append(report.Verdicts, Verdict{Gate: "replay", Pass: false, Reason: "replay returned no result"})
			report.Passed = false
			return s.finish(c, report, StatusFailed)
		}
		report.Replay = res
		switch res.Verdict {
		case ReplayPass:
			report.Verdicts = append(report.Verdicts, Verdict{Gate: "replay", Pass: true, Reason: res.Reason})
			report.Passed = true
		case ReplayInconclusive:
			// inconclusive **不放行**：未验证即不可召回。这与"验证失败不进索引"
			// 是同一条不变式——把它单列出来是为了让调用方能区分"内容失败"与
			// "没结论"，并决定是否重试。
			report.Verdicts = append(report.Verdicts, Verdict{Gate: "replay", Pass: false, Reason: "inconclusive: " + res.Reason})
			report.Passed = false
			return s.finish(c, report, StatusFailed)
		default: // ReplayFail
			report.Verdicts = append(report.Verdicts, Verdict{Gate: "replay", Pass: false, Reason: res.Reason})
			report.Passed = false
			return s.finish(c, report, StatusFailed)
		}
	}
	if report.ReplaySkipped {
		report.Passed = true // 静态门禁全过且按策略跳过 Replay
	}
	return s.finish(c, report, StatusVerified)
}

// finish 把报告写回候选并落盘，同时落终态。
func (s *Service) finish(c *Candidate, report *VerificationReport, status CandidateStatus) (*VerificationReport, error) {
	c.Report = report
	c.Status = status
	c.Updated = s.now()
	if err := s.cfg.Candidates.Save(c); err != nil {
		return nil, err
	}
	return report, nil
}

// baselineFor 取候选的对照基线（同主题名的既有内容；无则 nil）。
func (s *Service) baselineFor(c *Candidate) *TopicSnapshot {
	if c.Kind != KindTopic || c.Topic.Name == "" {
		return nil
	}
	existing, err := s.cfg.Store.ReadTopic(c.Topic.Name)
	if err != nil {
		return nil
	}
	return &TopicSnapshot{Name: existing.Name, Title: existing.Title, Body: existing.Body, Updated: existing.Updated}
}

// repoRootOf 尽力取仓库根。
//
// memory.Store 接口刻意不暴露 RepoRoot（Phase A 的最小接口原则），所以这里对
// FileStore 也拿不到；ReplayRequest.RepoRoot 因此默认为空，宿主 Replayer 若需要
// 应自己从 Identity 或工作目录取。**留空而不是猜一个**：猜错的路径会让宿主
// Replayer 在错误目录里跑验证，比不跑更危险。
func repoRootOf(store memory.Store) string { return "" }

// Promote 把通过验证的候选写入可召回记忆（经 Promoter → Phase A 的 WriteTopic）。
//
// 不变式（promote 是唯一进入索引的口子，且只能走 Phase A 的写入）：
//   - 只有 StatusVerified 能 Promote；其它状态一律 ErrNotVerified（失败/被拒/未验证）；
//   - 写回经 Promoter，宿主接的是 *memory.MemoryService.WriteTopic，因此
//     200 行 / 25KB 索引限额、路径边界、secret 扫描、原子写、**索引同步**
//     全部复用 Phase A，不会被本包绕过或重做；
//   - Promote 成功后候选状态置为 StatusPromoted（可审计"这条记忆是被门禁放进来的"）。
func (s *Service) Promote(c *Candidate) error {
	if c == nil {
		return errors.New("evolution: candidate is nil")
	}
	if c.Status != StatusVerified {
		return fmt.Errorf("%w: candidate %s is %q", ErrNotVerified, c.ID, c.Status)
	}
	if c.Kind != KindTopic {
		return fmt.Errorf("evolution: promote expects kind=topic, got %q", c.Kind)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 状态可能已被另一次 Verify 改掉（例如被拒），重读一次以状态文件为准。
	stored, err := s.cfg.Candidates.Get(c.ID)
	if err != nil {
		return err
	}
	if stored.Status != StatusVerified {
		return fmt.Errorf("%w: candidate %s is %q on disk", ErrNotVerified, c.ID, stored.Status)
	}
	topic := stored.Topic
	if err := s.cfg.Promoter.Promote(&topic); err != nil {
		return fmt.Errorf("evolution: promote %s: %w", c.ID, err)
	}
	stored.Status = StatusPromoted
	stored.Updated = s.now()
	return s.cfg.Candidates.Save(stored)
}

// ListPending 列出待验证候选（Pending 状态，ID 升序）。
func (s *Service) ListPending() ([]*Candidate, error) {
	all, err := s.cfg.Candidates.List()
	if err != nil {
		return nil, err
	}
	var out []*Candidate
	for _, c := range all {
		if c.Status == StatusPending {
			out = append(out, c)
		}
	}
	sortCandidates(out)
	return out, nil
}

// RejectAllPending 批量拒绝所有 Pending 候选（记录原因）。用于门禁整体不可用时的
// "拒绝一切"，返回被拒的候选数。
//
// 提供这个操作的原因：宿主拿到一个写坏掉的候选（如内容截断导致 JSON 解析失败）
// 时，直接 Get 会报错、无法走 Verify → 状态永远停在 Pending。批量拒绝给它一条
// 确定的出清路径。
func (s *Service) RejectAllPending(reason string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.cfg.Candidates.List()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range all {
		if c.Status != StatusPending {
			continue
		}
		c.Status = StatusRejected
		c.Updated = s.now()
		if reason != "" {
			if c.Report == nil {
				c.Report = &VerificationReport{CandidateID: c.ID, At: s.now()}
			}
			c.Report.Verdicts = append(c.Report.Verdicts, Verdict{Gate: "bulk-reject", Pass: false, Reason: reason})
			c.Report.Passed = false
		}
		if err := s.cfg.Candidates.Save(c); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func sortCandidates(cs []*Candidate) {
	sort.Slice(cs, func(i, j int) bool { return cs[i].ID < cs[j].ID })
}
