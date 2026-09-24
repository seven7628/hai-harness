package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/seven7628/hai-harness/core"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// FileStore 基于 JSONL 追加的文件会话存储（每会话一个文件）。
//
// 文件结构（追加行，按 type 区分）：
//
//	{"type":"history","messages":[{...},{...}]}   // 消息日志（append-only，压缩永不删除）
//	{"type":"state","state":{...}}                // 状态行（有效上下文 + 成本 + 积压）
//
// 恢复 = 文件尾反扫最后一条 state 行（有效上下文续跑），不加载历史日志 ——
// 运行时上下文只需有效上下文（State.Messages = 压缩后形态）；History 为 append-only
// 审计日志，桌面层不使用（SDK append 去重基于 persistedCount，不受 History 为空影响）。
// 崩溃容忍：尾行半截/损坏行在反扫时跳过。
type FileStore struct {
	dir string

	// 自压缩（store_compact.go）：抑制 state 全量快照的 O(N²) 膨胀。
	// 0 值（无论哪个字段）不关闭——由 NewFileStore 填默认值；要关闭用 WithAutoCompact(0, 0)。
	compactKeep      int
	compactThreshold int64

	// writeMu 串行化同一 FileStore 上的「追加」与「自压缩」：
	// 自压缩是「重写 + rename」，若与并发追加交错，落在旧 inode 上的行会丢失
	// （跨进程已由会话 flock 保证互斥，这里挡进程内并发）。
	writeMu sync.Mutex
}

// FileStoreOption 可选配置（测试用：调阈值/关自压缩）。
type FileStoreOption func(*FileStore)

// WithAutoCompact 配置自压缩：keep = 保留最后 K 条 state 行；threshold = 触发体积（字节）。
// keep<=0 或 threshold<=0 → 关闭自压缩。
func WithAutoCompact(keep int, threshold int64) FileStoreOption {
	return func(s *FileStore) {
		s.compactKeep = keep
		s.compactThreshold = threshold
	}
}

// NewFileStore 创建文件存储，目录不存在时自动创建。
// 默认开启自压缩（保留最后 defaultCompactKeepStates 条 state 行，见 store_compact.go）。
func NewFileStore(dir string, opts ...FileStoreOption) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil { // 会话目录含源码/命令输出，0700
		return nil, fmt.Errorf("file store: %w", err)
	}
	s := &FileStore{
		dir:              dir,
		compactKeep:      defaultCompactKeepStates,
		compactThreshold: defaultCompactThreshold,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s, nil
}

// Load 恢复会话：从文件尾反扫找到最后一条合法 state 记录，只解析它（O(有效上下文)，
// 不读整个文件的历史日志）。History 返回 nil —— 桌面运行时不需要，且 append 去重由
// persistedCount 保证（磁盘已含旧消息，只追加新消息）。文件无 state 记录时返回空数据。
func (s *FileStore) Load(_ context.Context, sessionId string) (*SessionData, error) {
	p, err := s.path(sessionId)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	state, err := tailLastState(f)
	if err != nil {
		return nil, err
	}
	return &SessionData{State: state}, nil
}

// tailLastState 从文件尾反扫，逐块向后读取，找出最后一条合法 state 记录并返回其 State。
// 崩溃残留的半截尾行 / 追加在 state 之后的 history 行 / 损坏行一律跳过；找到第一条
// state 即停止（文件尾反扫遇到的第一条 state 就是最后一条）。
// 无 state 记录时返回 nil。
//
// 行长度不受限：跨块累积（rest 最多持有一条超大行 + 当前块，如巨大 tool result 行）。
func tailLastState(f *os.File) (*State, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	const chunk = 64 * 1024
	var (
		pos  = st.Size()
		rest []byte // 已累积的尾部字节（文件末尾的后缀）
	)
	for pos > 0 {
		n := int64(chunk)
		if n > pos {
			n = pos
		}
		start := pos - n
		b := make([]byte, n)
		if _, err := f.ReadAt(b, start); err != nil {
			return nil, err
		}
		rest = append(b, rest...)
		pos = start

		// 从 rest 末尾向前逐行处理
		for len(rest) > 0 {
			idx := bytes.LastIndexByte(rest, '\n')
			if idx < 0 {
				// 无前导换行：若已读到文件头，剩余即第一行（可能无尾换行），按行处理；
				// 否则是跨块超长行的中段，读更多拼齐后再试。
				if pos == 0 {
					line := bytes.TrimSpace(rest)
					rest = nil
					if len(line) > 0 {
						var rec fileRecord
						if json.Unmarshal(line, &rec) == nil && rec.Type == "state" {
							st := rec.State
							return &st, nil
						}
					}
				}
				break
			}
			line := bytes.TrimSpace(rest[idx+1:])
			rest = rest[:idx]
			if len(line) == 0 {
				continue
			}
			var rec fileRecord
			if json.Unmarshal(line, &rec) == nil && rec.Type == "state" {
				st := rec.State
				return &st, nil
			}
			// 非 state 行（history / 损坏）：继续向前
		}
	}
	return nil, nil
}

func (s *FileStore) AppendHistory(_ context.Context, sessionId string, msgs []core.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	p, err := s.path(sessionId)
	if err != nil {
		return err
	}
	line, err := json.Marshal(fileRecord{Type: "history", Messages: msgs})
	if err != nil {
		return err
	}
	return s.appendLine(p, line)
}

func (s *FileStore) SaveState(_ context.Context, sessionId string, state *State) error {
	p, err := s.path(sessionId)
	if err != nil {
		return err
	}
	line, err := json.Marshal(fileRecord{Type: "state", State: *state})
	if err != nil {
		return err
	}
	if err := s.appendLine(p, line); err != nil {
		return err
	}
	// 自压缩（只在 state 追加后检查：state 行是膨胀源）。
	// 失败不影响本次保存——文件已追加成功，压缩只是空间治理；
	// 原文件在重写失败时保持不变，下次 checkpoint 会再试。
	_, _ = s.maybeCompact(p)
	return nil
}

// AppendCompaction 实现 CompactionRecorder（O2/G7）：压缩产物 + analysis 留档
// 以独立记录追加进会话 jsonl（append-only；恢复路径忽略，不参与上下文重建）。
func (s *FileStore) AppendCompaction(_ context.Context, sessionId string, rec *CompactionRecord) error {
	if rec == nil {
		return nil
	}
	p, err := s.path(sessionId)
	if err != nil {
		return err
	}
	line, err := json.Marshal(fileRecord{Type: "compaction", Compaction: rec})
	if err != nil {
		return err
	}
	return s.appendLine(p, line)
}

// AppendMeta 实现 MetaRecorder：展示元数据（AI 标题）以独立记录追加进会话 jsonl
// （append-only；恢复路径尾扫读取，不参与上下文重建）。
func (s *FileStore) AppendMeta(_ context.Context, sessionId string, rec *MetaRecord) error {
	if rec == nil {
		return nil
	}
	p, err := s.path(sessionId)
	if err != nil {
		return err
	}
	line, err := json.Marshal(fileRecord{Type: "meta", Meta: rec})
	if err != nil {
		return err
	}
	return s.appendLine(p, line)
}

// ReadMeta 从会话 jsonl 尾扫最后一条 meta 记录（AI 标题；无 → nil）。
// 供桌面层冷启动恢复标题（listSessions / mesh 标题桥），不参与上下文重建。
func (s *FileStore) ReadMeta(ctx context.Context, sessionId string) (*MetaRecord, error) {
	p, err := s.path(sessionId)
	if err != nil {
		return nil, err
	}
	return readLastMeta(p)
}

// ReadMetaFromFile 从会话 jsonl 文件尾扫最后一条 meta 记录（AI 标题；无 → 空串）。
// 供桌面层冷会话标题恢复（进程无 Session 实例时；与 ReadMeta 同实现）。
func ReadMetaFromFile(path string) string {
	mr, _ := readLastMeta(path)
	if mr == nil {
		return ""
	}
	return strings.TrimSpace(mr.Title)
}

// readLastMeta 从文件尾反扫最后一条 meta 记录（复用 tailLastState 的块式尾扫思路，
// 简化：逐行向前扫整个文件尾区直到找到 meta 行）。文件通常不大（消息行远多于 meta，
// meta 恒在文件尾部附近），从尾部读最后 64KB 足够覆盖；找不到再全量扫（罕见）。
func readLastMeta(path string) (*MetaRecord, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	// 从尾部读最后 256KB（覆盖 meta + 最近若干 history/state 行）；不够再全量
	readSize := st.Size()
	const maxTail = 256 * 1024
	if readSize > maxTail {
		readSize = maxTail
	}
	buf := make([]byte, readSize)
	if _, err := f.ReadAt(buf, st.Size()-readSize); err != nil && err != io.EOF {
		return nil, err
	}
	// 按行切分，从后往前找第一条 meta
	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var rec fileRecord
		if err := json.Unmarshal(line, &rec); err == nil && rec.Type == "meta" && rec.Meta != nil {
			return rec.Meta, nil
		}
	}
	return nil, nil
}

// path 校验 sessionId 并返回文件路径（防路径穿越）。
func (s *FileStore) path(sessionId string) (string, error) {
	if sessionId == "" || strings.ContainsAny(sessionId, `/\`) || strings.Contains(sessionId, "..") {
		return "", fmt.Errorf("invalid session id %q", sessionId)
	}
	return filepath.Join(s.dir, sessionId+".jsonl"), nil
}

// AcquireLock 实现 Locker：对会话锁文件（<dir>/<session>.jsonl.lock）加
// 非阻塞排他 flock；失败 = 另一实例持有（双实例冲突，拒绝运行）。
// release 关闭 fd 即释放锁（进程崩溃时锁由内核自动释放）。
func (s *FileStore) AcquireLock(_ context.Context, sessionId string) (func(), error) {
	p, err := s.path(sessionId)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("session %q is locked by another instance", sessionId)
	}
	return func() { f.Close() }, nil
}

// appendLine 追加一行并 fsync（快照频率低，可靠性优先）。
// 持 writeMu：与自压缩的「重写 + rename」互斥（否则落在旧 inode 上的行会丢）。
func (s *FileStore) appendLine(p string, line []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // 会话历史含敏感内容，0600
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// fileRecord JSONL 行记录。
type fileRecord struct {
	Type     string         `json:"type"`
	Messages []core.Message `json:"messages,omitempty"`
	State    State          `json:"state,omitempty"`
	// Compaction 记录（O2/G7）：{"type":"compaction",...}——压缩产物与压缩器
	// analysis 留档（append-only 审计；恢复路径忽略该记录，不参与上下文重建）。
	Compaction *CompactionRecord `json:"compaction,omitempty"`
	// Meta 记录（AI 标题）：{"type":"meta","meta":{"title":"..."}}——会话展示元数据
	// 持久化（append-only；恢复路径尾扫读取，不参与上下文重建）。
	Meta *MetaRecord `json:"meta,omitempty"`
}

// MetaRecord 会话 jsonl 的展示元数据记录（当前：AI 生成的会话标题）。
type MetaRecord struct {
	// Title AI 生成的会话标题（首次用户输入后异步生成；≤15 字符，跟随输入语言）。
	Title string `json:"title,omitempty"`
}

// CompactionRecord 会话 jsonl 的压缩留档记录（观测材料：摘要产物 + 压缩器
// analysis 决策过程 + 用量 + 质量警告；离线评估摘要质量的重放输入）。
type CompactionRecord struct {
	Summary   string      `json:"summary,omitempty"`
	Analysis  string      `json:"analysis,omitempty"` // 仅观测，绝不进上下文
	Model     string      `json:"model,omitempty"`
	Usage     *core.Usage `json:"usage,omitempty"`
	Warnings  []string    `json:"warnings,omitempty"`
	Timestamp time.Time   `json:"timestamp"`
}
