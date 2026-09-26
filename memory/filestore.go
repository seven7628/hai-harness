package memory

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// 权限位：目录 0700、文件 0600。
//
// 为什么这么紧：记忆内容是"从用户代码库里提炼的项目知识"，同机其他用户不该读；
// 与 runtime 受管解释器目录同口径（见 tools/builtin/run_python.go 的同款理由）。
const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// FileStore 基于目录布局的记忆存储（Phase A 唯一实现）。
//
// 布局：<root>/repos/<repoID>/MEMORY.md
//
//	<root>/repos/<repoID>/topics/<name>.md
//
// 并发边界：
//   - 单进程内按仓库 ID 串行化（chain 懒创建），覆盖"读索引 → 改 → 写回"的读-改-写；
//   - **跨进程不提供锁**：原子写保证读者不会看到半截文件，但不保证两个进程
//     同时写不互相覆盖。要跨进程安全得靠文件系统锁（flock），Phase A 不做。
//   - 同一目录下不同仓库 ID 之间无共享状态，可并发。
type FileStore struct {
	repoID string
	dir    string
	chains *chainTable
}

// chainTable 懒创建的 per-repo 写互斥表。
type chainTable struct {
	mu    sync.Mutex
	entry map[string]*sync.Mutex
}

func newChainTable() *chainTable { return &chainTable{entry: make(map[string]*sync.Mutex)} }

func (c *chainTable) chain(id string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.entry[id]
	if !ok {
		m = &sync.Mutex{}
		c.entry[id] = m
	}
	return m
}

// NewFileStore 在 root 下为 repoID 建立（或复用）一个存储。
//
// 只做路径构造与校验，**不创建目录**——BootstrapIndex/WriteTopic 首次写入时才建，
// 这样"打开一个还没写过记忆的仓库"没有任何副作用。
func NewFileStore(root, repoID string) (*FileStore, error) {
	if err := validateRepoID(repoID); err != nil {
		return nil, fmt.Errorf("memory: %w", err)
	}
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("%w: store root is empty (set Config.Root)", ErrNoHome)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("memory: resolve store root %q: %w", root, err)
	}
	dir, err := safeJoin(filepath.Join(abs, "repos"), repoID)
	if err != nil {
		return nil, fmt.Errorf("memory: %w", err)
	}
	return &FileStore{repoID: repoID, dir: dir, chains: newChainTable()}, nil
}

// RepoID 实现 Store。
func (s *FileStore) RepoID() string { return s.repoID }

// Dir 实现 Store（记忆仓库根目录绝对路径）。
func (s *FileStore) Dir() string { return s.dir }

// IndexPath 索引文件绝对路径。
func (s *FileStore) IndexPath() string { return filepath.Join(s.dir, IndexFileName) }

// TopicsDir 主题目录绝对路径。
func (s *FileStore) TopicsDir() string { return filepath.Join(s.dir, TopicsDirName) }

// TopicPath 主题文件绝对路径（名已规范化校验）。
func (s *FileStore) TopicPath(name string) (string, error) {
	n, err := validateTopicName(name)
	if err != nil {
		return "", err
	}
	return safeJoin(s.TopicsDir(), n+".md")
}

// ReadIndex 实现 Store：有界读取 + 解析。
//
// 限额口径（与 CC 对齐）：先按 25KB 截字节，再按 200 行截行，**同时**标记 Truncated。
// 读上限而非报错：索引是被限额约束的文档，超限是正常状态（如人工塞了一个大 Context），
// 应该"少读一点并如实告知"而不是让记忆功能整体失败。
func (s *FileStore) ReadIndex() (*Index, error) {
	raw, byteCut, lineCut, err := readBounded(s.IndexPath(), IndexMaxBytes, IndexMaxLines)
	if err != nil {
		return nil, err
	}
	return parseIndex(s.repoID, s.IndexPath(), raw, byteCut || lineCut), nil
}

// WriteIndex 实现 Store：原子写。
func (s *FileStore) WriteIndex(idx *Index) error {
	if idx == nil {
		return fmt.Errorf("memory: index is nil")
	}
	if idx.RepoID == "" {
		idx.RepoID = s.repoID
	}
	if idx.RepoID != s.repoID {
		return fmt.Errorf("memory: index repo id %q does not match store %q", idx.RepoID, s.repoID)
	}
	out, _ := renderIndex(idx)
	if countLines(out) > IndexMaxLines || len(out) > IndexMaxBytes {
		return fmt.Errorf("memory: index limit exceeded (%d lines / %d bytes); remove or merge topics before writing", countLines(out), len(out))
	}
	return writeFileAtomic(s.IndexPath(), out)
}

// ReadTopic 实现 Store。
func (s *FileStore) ReadTopic(name string) (*Topic, error) {
	p, err := s.TopicPath(name)
	if err != nil {
		return nil, err
	}
	raw, err := readAllBounded(p, maxBodyBytes*4)
	if err != nil {
		return nil, err
	}
	return parseTopic(strings.TrimSuffix(filepath.Base(p), ".md"), p, raw), nil
}

// WriteTopic 实现 Store：校验 → 规范化 → 原子写。
func (s *FileStore) WriteTopic(t *Topic) error {
	valid, err := ValidateTopic(t)
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	p, err := s.TopicPath(valid.Name)
	if err != nil {
		return err
	}
	return writeFileAtomic(p, renderTopic(valid))
}

// ListTopics 实现 Store：字典序列出主题名（不含 .md 后缀）。
//
// 跳过项：非 .md 文件、非法主题名（不能当路径用）、以及**目录**（含符号链接指向目录）。
// 不静默改写非法名——ListTopics 的输出要能直接喂回 ReadTopic/WriteTopic。
func (s *FileStore) ListTopics() ([]string, error) {
	entries, err := os.ReadDir(s.TopicsDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // 还没写过主题：空列表而非错误
		}
		return nil, fmt.Errorf("memory: list topics in %s: %w", s.TopicsDir(), err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		if _, err := validateTopicName(name); err != nil {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// writeFileAtomic 通用原子写：临时文件 + rename（同目录 ⇒ 同文件系统 ⇒ 原子）。
//
// 为什么不用 "直接 WriteFile"：读者（用户 cat、编辑器、我们自己下一次读取）可能
// 正好在写入过程中打开文件，会看到半截内容。rename 只暴露旧版本或完整新版本。
// 临时文件权限 0600，避免 rename 之前存在一个宽权限窗口。
func writeFileAtomic(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("memory: create dir for %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("memory: create temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	fail := func(err error, verb string) error {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("memory: %s %s: %w", verb, path, err)
	}
	if _, err := tmp.WriteString(content); err != nil {
		return fail(err, "write")
	}
	// 显式 Chmod：CreateTemp 本就建 0600，但 umask/平台差异下不假设，显式钉死。
	if err := tmp.Chmod(filePerm); err != nil {
		return fail(err, "chmod")
	}
	if err := tmp.Sync(); err != nil {
		return fail(err, "sync")
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("memory: close %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("memory: rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}

// readBounded 有界读取（字节 + 行双限额），并如实报告触发了哪一层限额。
//
// 用 `io.LimitReader(limit+1)` 而不是按 size 判断后 ReadFile：文件可能正被另一个
// 进程写（无跨进程锁），size 读到的值与实际内容可能不一致，多读 1 字节才能确证
// "超限"。返回文本已按行数截断并**保证以换行结尾**（截在行中间会让解析器把半行
// 当成条目，产生一个坏条目）。
func readBounded(path string, maxBytes, maxLines int) (text string, byteCut, lineCut bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, false, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return "", false, false, fmt.Errorf("memory: read %s: %w", path, err)
	}
	if len(raw) > maxBytes {
		raw, byteCut = raw[:maxBytes], true
	}
	s := string(raw)
	if countLines(s) > maxLines {
		s = truncateLines(s, maxLines)
		lineCut = true
	}
	return s, byteCut, lineCut, nil
}

// readAllBounded 全量但有上限的读取（主题文件；超限报错——内容不该这么大）。
func readAllBounded(path string, maxBytes int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return "", fmt.Errorf("memory: read %s: %w", path, err)
	}
	if len(raw) > maxBytes {
		return "", fmt.Errorf("memory: %s exceeds %d bytes", path, maxBytes)
	}
	return string(raw), nil
}

// truncateLines 截到最多 maxLines 行（保留每行换行符，结果以 \n 结尾）。
func truncateLines(s string, maxLines int) string {
	if s == "" || countLines(s) <= maxLines {
		return s
	}
	cut := 0
	for i := 0; i < maxLines; i++ {
		j := strings.IndexByte(s[cut:], '\n')
		if j < 0 {
			return s
		}
		cut += j + 1
	}
	return s[:cut]
}
