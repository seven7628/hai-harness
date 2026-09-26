package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// NewService 构造记忆服务。
//
// Root 留空 = DefaultRoot()（~/.go-code/memory），取不到 home 时**构造成功但 Open
// 会失败**（错误为 ErrNoHome）——让"环境缺 home"这种配置问题在使用点报错，
// 而不是让 NewService 变成一个无法处理的构造错误。
func NewService(cfg Config) *MemoryService {
	return &MemoryService{cfg: cfg, root: cfg.Root}
}

// Root 记忆根目录（未显式配置时为 ~/.go-code/memory；取不到 home 时为空串）。
func (s *MemoryService) Root() string {
	if s.root != "" {
		return s.root
	}
	return DefaultRoot()
}

// Open 打开（或准备打开）某个工作目录所属仓库的记忆。
//
// 步骤：解析 root（配置 → home 默认）→ Resolver 解析身份 → NewFileStore。
// **不创建任何目录、不读任何文件**：Open 是无副作用的准备动作，真正落盘发生在
// BootstrapIndex / WriteTopic。这让"打开一个还没写过记忆的仓库"不会产生垃圾目录。
func (s *MemoryService) Open(ctx context.Context, workdir string) (*Bound, error) {
	root := s.Root()
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("memory: cannot locate memory root (set Config.Root): %w", ErrNoHome)
	}
	resolver := s.cfg.Resolver
	if resolver == nil {
		resolver = &GitIdentityResolver{FallbackID: s.cfg.FallbackID, Timeout: s.cfg.Timeout}
	}
	ident, err := resolver.Resolve(ctx, workdir)
	if err != nil {
		return nil, err
	}
	store, err := NewFileStore(root, ident.ID)
	if err != nil {
		return nil, err
	}
	s.root, s.store, s.ident = root, store, &ident
	return &Bound{Identity: ident, Store: store}, nil
}

// Bound 取最近一次 Open 的结果；未 Open 返回 nil。
func (s *MemoryService) Bound() *Bound {
	if s.store == nil || s.ident == nil {
		return nil
	}
	return &Bound{Identity: *s.ident, Store: s.store}
}

// require 取已绑定的存储，未绑定时给出可操作错误。
func (s *MemoryService) require() (Store, error) {
	if s.store == nil {
		return nil, errors.New("memory: Open(...) must be called before using the memory service")
	}
	return s.store, nil
}

func (s *MemoryService) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now().UTC().Truncate(time.Second)
	}
	return time.Now().UTC().Truncate(time.Second)
}

// BootstrapIndex 读取索引；不存在则创建一份带骨架的 MEMORY.md。
//
// 幂等：已存在时**原样返回**（不重写、不动 generated、不刷新条目）——它是"确保有
// 一份可读索引"，不是"同步索引"。已存在但超过限额时也**不截断写回**：截断是读取
// 侧的行为，写回会把用户没预期的条目永久删掉；只在 Truncated 为真时由调用方决定
// 是否提示用户手工整理。
//
// 已存在但解析不出仓库身份字段时：补齐 RepoID/RemoteURL/RepoRoot（缺失的才补，
// 已有值一律以文件为准）并回写，让索引自描述当前仓库。
func (s *MemoryService) BootstrapIndex() (*Index, error) {
	store, err := s.require()
	if err != nil {
		return nil, err
	}
	chain := s.chainFor(store)
	chain.Lock()
	defer chain.Unlock()

	idx, err := store.ReadIndex()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		fresh := &Index{
			RepoID:    store.RepoID(),
			RepoRoot:  s.repoRoot(),
			RemoteURL: s.remoteURL(),
			Generated: s.now(),
			Bootstrap: true,
		}
		if err := store.WriteIndex(fresh); err != nil {
			return nil, err
		}
		// 回读：让返回值的 Lines/Bytes/Path/Entries 与磁盘真实内容一致。
		return store.ReadIndex()
	}
	// 已存在的索引原样返回。索引中的仓库元数据不由 Bootstrap 补写：
	// 读-改-写会和 WriteTopic 争用索引，且旧格式索引没有必要被自动重写。
	return idx, nil
}

// View 读取一份记忆：索引或主题。
//
// ref 接受 ""/"index"/"MEMORY.md"（索引）、"topic:foo"/"topics/foo"/"foo"（主题）
// 三种写法；主题不存在返回包裹 os.ErrNotExist 的错误（调用点用 errors.Is 判别，
// 以便"没有这条记忆"与"存储坏了"能被区分开）。
func (s *MemoryService) View(ref string) (string, error) {
	store, err := s.require()
	if err != nil {
		return "", err
	}
	r, err := ParseRef(ref)
	if err != nil {
		return "", err
	}
	if r.IsIndex {
		idx, err := store.ReadIndex()
		if err != nil {
			return "", err
		}
		return idx.Raw, nil
	}
	t, err := store.ReadTopic(r.Name)
	if err != nil {
		return "", err
	}
	// 返回**渲染**结果而非磁盘原文：Topic 的时间戳/标签已规范化，渲染保证
	// "从 API 读到的" 与 "从文件读到的" 结构一致（用户可能手改过 frontmatter）。
	return renderTopic(t), nil
}

// WriteTopic 写一个主题并把它的摘要同步进索引。
//
// 顺序：先写主题文件，再更新索引。索引是"地图"，主题是"正文"——先正文后地图，
// 中途失败最坏是"有正文没索引条目"（下轮自愈），反过来会留下指向空文件的条目。
//
// 幂等键是主题名：同名即覆盖（Created 保留、Updated 刷新），不做"追加历史"。
// 边界：Phase A **不**记录 outcome/replay（那属于后续阶段），所以一次 WriteTopic
// 就是全部语义——调用点想保留旧版本必须自己换名字。
func (s *MemoryService) WriteTopic(t *Topic) error {
	store, err := s.require()
	if err != nil {
		return err
	}
	valid, err := ValidateTopic(t)
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	now := s.now()

	// 已有主题：沿用其 Created 与既有标签（调用点没显式给标签时），Updated 刷成 now。
	if existing, err := store.ReadTopic(valid.Name); err == nil {
		if valid.Created.IsZero() {
			valid.Created = existing.Created
		}
		if len(valid.Tags) == 0 {
			valid.Tags = existing.Tags
		}
		if valid.Summary == "" {
			valid.Summary = existing.Summary
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if valid.Created.IsZero() {
		valid.Created = now
	}
	valid.Updated = now

	chain := s.chainFor(store)
	chain.Lock()
	defer chain.Unlock()

	if err := store.WriteTopic(valid); err != nil {
		return err
	}
	return s.syncIndexEntry(store, valid)
}

// chainFor 取该存储的写互斥。Phase A 索引更新是 read-modify-write，
// 同一进程内所有 MemoryService 对同一 Memory 根串行化；先保证不丢条目，
// 后续再按 repoID 细分锁粒度。
func (s *MemoryService) chainFor(store Store) *sync.Mutex {
	return globalChain
}

// 非 FileStore 实现（测试替身 / 未来别的后端）没有自己的 repo id，
// 用进程级兜底锁保证复合写仍串行。
var globalChain = &sync.Mutex{}

// syncIndexEntry 把主题摘要并入索引（读-改-写；同名条目覆盖、按 updated 倒序）。
//
// 排序口径：updated 倒序（同值按名字升序，保证确定性）——索引是"查询地图"，
// 近期记忆排前面才有用；因此**读到的索引被重排是有意的**，
// 顺序信息的权威是 generated/updated，而不是文件里的行序。
func (s *MemoryService) syncIndexEntry(store Store, t *Topic) error {
	idx, err := store.ReadIndex()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		idx = &Index{RepoID: store.RepoID(), RepoRoot: s.repoRoot(), RemoteURL: s.remoteURL()}
	}
	entry := IndexEntry{
		Name:    t.Name,
		Title:   t.Title,
		Summary: t.Summary,
		Scope:   t.Scope,
		Tags:    t.Tags,
		Updated: t.Updated,
	}
	out := make([]IndexEntry, 0, len(idx.Entries)+1)
	replaced := false
	for _, e := range idx.Entries {
		if e.Name == entry.Name {
			// 已有条目：保留文件里已有但调用点没给的字段。
			if entry.Title == "" {
				entry.Title = e.Title
			}
			if entry.Summary == "" {
				entry.Summary = e.Summary
			}
			if len(entry.Tags) == 0 {
				entry.Tags = e.Tags
			}
			out = append(out, entry)
			replaced = true
			continue
		}
		out = append(out, e)
	}
	if !replaced {
		out = append(out, entry)
	}
	idx.Entries = sortEntries(out)
	idx.TopicCount = len(idx.Entries)
	idx.Generated = s.now()
	return store.WriteIndex(idx)
}

// sortEntries 索引条目排序：updated 倒序 → 名字升序（完全确定性）。
func sortEntries(entries []IndexEntry) []IndexEntry {
	out := make([]IndexEntry, len(entries))
	copy(out, entries)
	// 插入排序：条目数上限约 200，O(n²) 完全够用且零依赖。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && entryLess(out[j], out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func entryLess(a, b IndexEntry) bool {
	if !a.Updated.Equal(b.Updated) {
		return a.Updated.After(b.Updated)
	}
	return a.Name < b.Name
}

// ParseRef 解析记忆引用。
//
// 接受：""/"index"/"MEMORY.md" → 索引；"foo"/"topic:foo"/"topics/foo" → 主题。
// 主题名走与写入同一套规范化，因此 View(t.Name) 一定能命中 WriteTopic(t.Name) 写的文件。
func ParseRef(ref string) (Ref, error) {
	r := strings.ToLower(strings.TrimSpace(ref))
	r = strings.TrimPrefix(r, "topic:")
	r = strings.TrimPrefix(r, "topics/")
	switch r {
	case "", "index", "memory.md", "memory":
		return Ref{IsIndex: true}, nil
	}
	name, err := validateTopicName(r)
	if err != nil {
		return Ref{}, fmt.Errorf("memory: bad memory ref %q: %w", ref, err)
	}
	return Ref{Name: name}, nil
}

func (s *MemoryService) repoRoot() string {
	if s.ident == nil {
		return ""
	}
	return s.ident.RepoRoot
}

func (s *MemoryService) remoteURL() string {
	if s.ident == nil {
		return ""
	}
	return s.ident.RemoteURL
}

// userHomeDir 取 home 目录（os.UserHomeDir 的薄包装，便于测试替身与错误归一）。
func userHomeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoHome, err)
	}
	if strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("%w: empty home path", ErrNoHome)
	}
	return home, nil
}

// joinPath 拼路径（薄包装，供 DefaultRoot 保持单行）。
func joinPath(elems ...string) string { return filepath.Join(elems...) }
