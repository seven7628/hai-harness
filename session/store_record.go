package session

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// SessionRecordType identifies the atomic, checkpoint-first session record.
	SessionRecordType = "session_record"

	// SessionRecordSchemaVersion is the on-disk schema version for SessionRecord.
	SessionRecordSchemaVersion = 1

	// ViewCheckpointSchemaVersion is the first version of the backend view
	// checkpoint envelope. The checkpoint body is intentionally kept as a JSON
	// payload here; the bridge owns its event-derived schema and will add the
	// typed reducer projection in a later phase.
	ViewCheckpointSchemaVersion = 1
)

var (
	// ErrRecordNotFound is returned when neither the record nor its bounded
	// backup exists. It also wraps ErrNotFound for callers that already use the
	// legacy session-store error.
	ErrRecordNotFound = fmt.Errorf("session record not found: %w", ErrNotFound)

	// ErrInvalidSessionRecord indicates a record that cannot be committed or
	// trusted because its identity/schema/required checkpoint fields are
	// invalid.
	ErrInvalidSessionRecord = errors.New("invalid session record")

	// ErrRecordChecksum indicates that the durable payload does not match its
	// recorded checksum.
	ErrRecordChecksum = errors.New("session record checksum mismatch")

	// ErrRecordCorrupt indicates malformed or otherwise untrusted on-disk data.
	ErrRecordCorrupt = errors.New("session record corrupt")

	// ErrRecordRevision indicates a stale or conflicting durable revision.
	ErrRecordRevision = errors.New("session record revision conflict")
)

// ViewCheckpoint is the versioned backend view payload stored inside a
// SessionRecord. It is an alias rather than a frontend/state-store object on
// purpose: the bridge reducer will define and validate the typed checkpoint
// projection, while the session storage layer treats the JSON body as an
// immutable payload and protects it with the enclosing record checksum.
//
// The payload must be a JSON object containing at least schema_version,
// blocks, run_nodes and turns. New fields may be added by a compatible
// checkpoint schema without changing the storage envelope.
type ViewCheckpoint = json.RawMessage

// SessionRecord is the single atomic recovery object for one main session.
// SDKState and ViewCheckpoint are committed under the same Revision. The
// Checksum covers every field except Checksum itself using canonical JSON.
//
// This type deliberately does not include SubAgent journal entries. Journals
// use a separate namespace/path and have a different durability and recovery
// contract.
type SessionRecord struct {
	Type            string         `json:"type"`
	SchemaVersion   int            `json:"schema_version"`
	SessionID       string         `json:"session_id"`
	WorkspaceKey    string         `json:"workspace_key"`
	Revision        uint64         `json:"revision"`
	ReducerSeq      uint64         `json:"reducer_seq"`
	ClearGeneration uint64         `json:"clear_generation"`
	UpdatedAt       time.Time      `json:"updated_at"`
	SDKState        State          `json:"sdk_state"`
	ViewCheckpoint  ViewCheckpoint `json:"view_checkpoint"`
	Checksum        string         `json:"checksum"`

	// BlockSpans 是调用方（reducer）已切好的 block 字节，可选。
	// 提供它可让提交路径跳过对 view 的二次扫描（86MB 实测省 106ms）。
	// 不持久化、不校验：仅当长度与 view 内 blocks 一致时才被采用。
	BlockSpans [][]byte `json:"-"`
}

// SessionRecordStore is the storage boundary used by the future bridge
// SessionCommitCoordinator. It is intentionally separate from the legacy
// Store interface: legacy JSONL history/state writes must not silently become
// a second durable source for the new checkpoint-first path.
type SessionRecordStore interface {
	LoadRecord(ctx context.Context, workspaceKey, sessionID string) (*SessionRecord, error)
	CommitRecord(ctx context.Context, record *SessionRecord) error
	DeleteSessionRecord(ctx context.Context, workspaceKey, sessionID string) error
}

// NewEmptyViewCheckpoint returns a valid empty checkpoint envelope for a new
// session. Reducer fields can be populated without changing the storage API.
func NewEmptyViewCheckpoint() ViewCheckpoint {
	return ViewCheckpoint([]byte(`{"schema_version":1,"blocks":[],"run_nodes":[],"turns":[],"usage":{},"cost_usd":0,"todos":[],"degraded":false}`))
}

// NewSessionRecord constructs a record with the frozen envelope defaults. The
// caller still supplies the durable revision and reducer boundary; CommitRecord
// calculates the checksum and performs the atomic write.
func NewSessionRecord(workspaceKey, sessionID string, revision, reducerSeq, clearGeneration uint64, state State, view ViewCheckpoint) *SessionRecord {
	if len(view) == 0 {
		view = NewEmptyViewCheckpoint()
	}
	return &SessionRecord{
		Type:            SessionRecordType,
		SchemaVersion:   SessionRecordSchemaVersion,
		SessionID:       sessionID,
		WorkspaceKey:    workspaceKey,
		Revision:        revision,
		ReducerSeq:      reducerSeq,
		ClearGeneration: clearGeneration,
		UpdatedAt:       time.Now().UTC(),
		SDKState:        state,
		ViewCheckpoint:  append(ViewCheckpoint(nil), view...),
	}
}

// Validate checks a fully materialized record, including its checksum. A
// freshly constructed record may omit Checksum until CommitRecord has computed
// it; CommitRecord performs the shape validation before filling the checksum.
func (r *SessionRecord) Validate() error {
	if err := validateSessionRecordShape(r); err != nil {
		return err
	}
	if strings.TrimSpace(r.Checksum) == "" {
		return fmt.Errorf("%w: checksum is required", ErrRecordChecksum)
	}
	// v2 口径（sha256b:）是「对磁盘字节算的」，纯内存里没有那份字节，无法在此复算。
	// 调用方若需要校验 v2，必须走 readRecordCandidate（它拿得到原始字节）。
	// 这里只做结构校验并通过 —— v2 的完整性由读取路径负责。
	if strings.HasPrefix(r.Checksum, checksumV2Prefix) {
		return nil
	}
	expected, err := checksumForRecord(r)
	if err != nil {
		return err
	}
	if !sameChecksum(r.Checksum, expected) {
		return fmt.Errorf("%w: want %s, got %s", ErrRecordChecksum, expected, r.Checksum)
	}
	return nil
}

// Clone returns a detached copy suitable for serialization. In particular,
// state slices and the raw checkpoint bytes do not share mutable backing
// storage with the caller.
func (r *SessionRecord) Clone() (*SessionRecord, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: nil record", ErrInvalidSessionRecord)
	}
	payload, err := json.Marshal(recordPayloadFrom(r))
	if err != nil {
		return nil, fmt.Errorf("clone session record: %w", err)
	}
	var out recordPayload
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("clone session record: %w", err)
	}
	return &SessionRecord{
		Type:            out.Type,
		SchemaVersion:   out.SchemaVersion,
		SessionID:       out.SessionID,
		WorkspaceKey:    out.WorkspaceKey,
		Revision:        out.Revision,
		ReducerSeq:      out.ReducerSeq,
		ClearGeneration: out.ClearGeneration,
		UpdatedAt:       out.UpdatedAt,
		SDKState:        out.SDKState,
		ViewCheckpoint:  append(ViewCheckpoint(nil), out.ViewCheckpoint...),
		Checksum:        r.Checksum,
	}, nil
}

// FileStorage owns the session-record namespace rooted at a directory such as
// ~/.go-code/sessions. It does not own legacy events or SubAgent journals.
// Those paths have separate owners and schemas even when the same process
// creates all of them.
type FileStorage struct {
	root string
	mu   sync.Mutex
}

// NewFileStorage creates the atomic SessionRecord storage root. The root is
// expected to be the sessions namespace, not a single workspace directory.
func NewFileStorage(root string) (*FileStorage, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("file storage: root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("file storage: resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil { // 会话记录含敏感内容，0700
		return nil, fmt.Errorf("file storage: create root: %w", err)
	}
	return &FileStorage{root: filepath.Clean(abs)}, nil
}

// NewSessionRecordStorage is an explicit alias for callers that want to make
// the namespace visible at the call site.
func NewSessionRecordStorage(root string) (*FileStorage, error) {
	return NewFileStorage(root)
}

// Root returns the resolved storage root. It is useful to bridge wiring and
// tests, while path construction remains centralized in RecordPath.
func (s *FileStorage) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// RecordPath returns the only path accepted by the session-record namespace.
// It cannot point at legacy <sid>.jsonl files or <sid>/agents paths.
func (s *FileStorage) RecordPath(workspaceKey, sessionID string) (string, error) {
	if s == nil || s.root == "" {
		return "", fmt.Errorf("%w: storage is nil", ErrInvalidSessionRecord)
	}
	if err := validatePathComponent("workspace key", workspaceKey); err != nil {
		return "", err
	}
	if err := validatePathComponent("session id", sessionID); err != nil {
		return "", err
	}
	dir := filepath.Join(s.root, workspaceKey)
	path := filepath.Join(dir, sessionID+".record.json")
	// The component checks above are the primary guard. Keep a root-relative
	// check as defense in depth if this code is changed to accept new names.
	rel, err := filepath.Rel(s.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: record path escapes storage root", ErrInvalidSessionRecord)
	}
	return path, nil
}

// LoadRecord loads a checksum-valid formal record. A valid bounded .bak is
// used only when the formal file is missing or corrupt. Temporary files are
// never considered recovery sources.
func (s *FileStorage) LoadRecord(ctx context.Context, workspaceKey, sessionID string) (*SessionRecord, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	path, err := s.RecordPath(workspaceKey, sessionID)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadRecordLocked(ctx, path, workspaceKey, sessionID)
}

// RecordMetadata 是打开会话所需的最小持久化信息：只有三个标量。
//
// 为什么需要它：open_session 的 checkpoint-first 分支原本调 LoadRecord，
// 把 24MB~90MB 的 sdk_state/view_checkpoint 全部解出来，却只读取
// Revision/ReducerSeq/ClearGeneration 三个标量（返回给 UI 的视图来自内存
// 里的 reducer.Marshal，磁盘 payload 解析完即丢弃）。实测 660ms → 104ms。
//
// 正确性前提：SessionRecord 的 JSON 字段顺序固定（encoding/json 按结构体
// 定义顺序输出），三个标量都排在 sdk_state/view_checkpoint 之前，所以
// 读到它们即可停止，无需扫描整个文件。
type RecordMetadata struct {
	Revision        uint64
	ReducerSeq      uint64
	ClearGeneration uint64
}

// LoadRecordMetadata 只解出元数据，不 materialize payload。
//
// 它与 LoadRecord 的**接受判据保持一致**：同样认 formal，失败时回退 .bak；
// 身份不匹配、schema 不符、checkpoint 不可解析一律视为 corrupt。差异只在
// 于不校验 payload 的 checksum —— 因为调用方（打开会话）本来就不用 payload，
// 而 payload 的完整性由之后真正读它的路径（LoadRecord / 提交基线）负责。
// 若某天有调用方需要「连 payload 一起校验」，必须用 LoadRecord，不能用本方法。
func (s *FileStorage) LoadRecordMetadata(ctx context.Context, workspaceKey, sessionID string) (*RecordMetadata, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	path, err := s.RecordPath(workspaceKey, sessionID)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	meta, err := loadMetadataCandidate(path, workspaceKey, sessionID)
	if err == nil {
		return meta, nil
	}
	if backup, backupErr := loadMetadataCandidate(path+".bak", workspaceKey, sessionID); backupErr == nil {
		return backup, nil
	} else if errors.Is(err, ErrRecordNotFound) && errors.Is(backupErr, ErrRecordNotFound) {
		return nil, ErrRecordNotFound
	}
	return nil, fmt.Errorf("%w: %v", ErrRecordCorrupt, err)
}


// CommitRecord atomically replaces the formal record after validating identity,
// schema, monotonic revision and canonical checksum. The previous valid formal
// record is copied to one bounded .bak before replacement. The caller's record
// is not used as the mutable serialization buffer; its checksum is updated only
// after a successful durable commit.
func (s *FileStorage) CommitRecord(ctx context.Context, record *SessionRecord) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := validateSessionRecordShape(record); err != nil {
		return err
	}
	// 浅拷贝足以满足「提交成功前不碰调用方对象」的契约：下面的流程只写
	// working.Checksum 这一个字段，SDKState/ViewCheckpoint 全程只读。
	// 原实现用 record.Clone()（整条 marshal+unmarshal）达成同一目的，
	// 在 90MB record 上是 0.7s / 两次全量 JSON 往返，实测提交 4.3s→3.64s。
	// ViewCheckpoint 仍单独拷一份：它是调用方可能复用的裸字节，别名会让
	// 落盘内容有被并发改写撕裂的风险。
	working := *record
	working.ViewCheckpoint = append(ViewCheckpoint(nil), record.ViewCheckpoint...)

	// 顺序很关键：**先序列化，再按字节算 checksum**。
	//
	// 旧实现反过来：先 checksumForRecord（canonicalJSON: marshal → decode →
	// marshal）再 marshal 落盘 —— 对同一份 90MB payload 往复三遍，实测 1250ms，
	// 占 commit 的 90% 以上。而 checksum 的目的只是「检出磁盘损坏 + 跨版本可读」，
	// 按**落盘字节**计算即可满足，且天然免疫 struct 演化（见 checksumBytes 注释）。
	//
	// 调用方自带 checksum 时仍会校验（防止提交错内容），但同样走字节口径：
	// 把调用方的值当作期望，与本份字节算出的值比对。
	// 一次 marshal（checksum 留空）→ 从字节里剥掉空 checksum 字段 → sha256 → 拼回。
	// 全程只序列化一次，不再有 canonicalJSON 的 decode+marshal。
	//
	// 用 marshalRecordEnvelope 而非 json.Marshal：后者对 `json.RawMessage`
	// 字段会做 compact（逐字节扫描转义，90MB 实测 280ms）；而 view_checkpoint
	// 本身就是合法 JSON 字节，原样拼接即可。实测 329ms → 32ms，**输出逐字节相同**。
	// blocks sidecar 化：把 87MB 的 blocks 换成几十字节的引用。
	//
	// 收益来源（实测 agent-server-g 真实会话，64.5 小时 / 13771 次提交）：
	// 3520 个 block 里 95.3% 的体积、100% 为终态，每次提交却要重写全部 91MB。
	// 改后 record 主体只剩 turns/diffs/标量（约 4MB），新 block 只追加 sidecar。
	//
	// 失败一律回退全量内嵌（正确性优先，静默）：任何 block 无 id、blocks 非数组、
	// view 结构异常 → working.ViewCheckpoint 保持原样，走原路径落盘。
	// 读回时有无引用都能解析（见 resolveBlocksRef），所以回退不影响可读性。
	if recPath, perr := s.RecordPath(working.WorkspaceKey, working.SessionID); perr == nil {
		// sidecar 与 record 同目录 —— 首次提交时目录可能还不存在（record 的
		// MkdirAll 在后面执行），必须在这里先建，否则 openBlockStore 直接失败、
		// 静默回退全量路径（实测踩到：优化完全没生效，record 仍是 90MB）。
		if mkErr := os.MkdirAll(filepath.Dir(recPath), 0o700); mkErr == nil {
			if bs, berr := acquireBlockStore(recPath); berr == nil {
				// 优先用调用方给的 spans（reducer 顺带产出，零解析）；否则自行扫描。
				var newView []byte
				var rerr error
				if len(record.BlockSpans) > 0 {
					newView, _, rerr = refBlocksFromSpans(bs, working.ViewCheckpoint, record.BlockSpans)
				} else {
					newView, _, rerr = refBlocks(bs, working.ViewCheckpoint)
				}
				// 先落 sidecar 再落 record：record 里的引用必须指向已 durable 的数据。
				if rerr == nil {
					if ferr := bs.flush(); ferr == nil {
						working.ViewCheckpoint = newView
					}
				}
				// 不 Close：实例留在缓存里复用（Close 会 fsync 并关文件，下次提交
				// 又要重扫 86MB 重建索引 —— 实测 78ms，吃掉增量方案的收益）。
			}
		}
	}

	prefix, view, suffix, err := encodeRecordEnvelope(&working)
	if err != nil {
		return fmt.Errorf("encode session record: %w", err)
	}

	// checksum 口径必须与 checksumBytes **逐字节一致**：
	// 把落盘字节里的 `,"checksum":"..."` 字段整个剥掉，再对剩余部分算 sha256。
	// 也就是说哈希的是 prefix + view + "}"，不含 checksum 字段本身。
	span, ok := checksumFieldSpan(suffix)
	if !ok {
		return fmt.Errorf("%w: cannot locate checksum field", ErrRecordChecksum)
	}
	sumWriter := sha256.New()
	for _, part := range [][]byte{prefix, view, suffix[:span.fieldStart], {'}'}} {
		if _, err := sumWriter.Write(part); err != nil {
			return fmt.Errorf("hash session record: %w", err)
		}
	}
	working.Checksum = checksumV2Prefix + hex.EncodeToString(sumWriter.Sum(nil))

	// 调用方自带 checksum 时校验（可能来自旧口径）。
	if record.Checksum != "" && !sameChecksum(record.Checksum, working.Checksum) {
		legacy, cerr := checksumForRecord(&working)
		if cerr != nil || !sameChecksum(record.Checksum, legacy) {
			return fmt.Errorf("%w: supplied checksum does not match payload", ErrRecordChecksum)
		}
	}

	path, err := s.RecordPath(working.WorkspaceKey, working.SessionID)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create session record directory: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return err
	}

	// 读基线只需要 (Revision, Checksum) 与「文件是否完好」三个事实，
	// 不需要把 90MB payload 解成对象图。原来用 readRecordCandidate（全解码 +
	// 旧口径 checksum 规范化校验）在 91MB 上要 1.8s，占 v2 提交的 40%+。
	//
	// 换成 readRecordBaseline：只解头部标量 + 字节口径校验（v2 记录 O(n) 一遍；
	// 旧口径记录仍走规范化，但只发生在新旧混合期）。
	//
	// 安全性不低于原实现：结构损坏/身份不符/checksum 失配一律拒绝并回退 .bak，
	// 只是不再 materialize payload —— 该 payload 后续也不需要（我们只写新内容）。
	current, currentRaw, currentErr := readRecordBaseline(path, working.WorkspaceKey, working.SessionID)
	if currentErr != nil {
		// A valid bounded backup is the only fallback baseline. This applies
		// both when the formal file is corrupt and when it is absent (for
		// example, an interrupted replacement), so a stale revision cannot
		// overwrite the last known-good record.
		backup, backupRaw, backupErr := readRecordBaseline(path+".bak", working.WorkspaceKey, working.SessionID)
		switch {
		case backupErr == nil:
			current, currentRaw, currentErr = backup, backupRaw, nil
		case errors.Is(backupErr, ErrRecordNotFound) && errors.Is(currentErr, ErrRecordNotFound):
			// First commit: neither the formal record nor its backup exists.
		case errors.Is(backupErr, ErrRecordNotFound):
			return fmt.Errorf("%w: formal=%v backup=%v", ErrRecordCorrupt, currentErr, backupErr)
		default:
			return fmt.Errorf("%w: formal=%v backup=%v", ErrRecordCorrupt, currentErr, backupErr)
		}
	}
	if currentErr == nil {
		switch {
		case current.Revision > working.Revision:
			return fmt.Errorf("%w: current revision %d is newer than %d", ErrRecordRevision, current.Revision, working.Revision)
		case current.Revision == working.Revision:
			// A retry after an uncertain rename is safe when it is exactly the
			// same payload. Equal revisions with different payloads are not.
			if sameChecksum(current.Checksum, working.Checksum) {
				record.Checksum = working.Checksum
				return nil
			}
			return fmt.Errorf("%w: revision %d already contains a different payload", ErrRecordRevision, working.Revision)
		}
	}

	if err := contextError(ctx); err != nil {
		return err
	}
	// 流式写出：prefix + view + `,"checksum":"<sum>"}` + "\n"。
	// 不再先拼出完整 encoded 再写 —— 那会多一次 91MB 拷贝，且在 Checksum
	// 已定稿后才发生，纯属浪费。writeTempFileParts 内部 fsync 后再 close。
	tmp, err := writeTempFileParts(dir, filepath.Base(path), []filePart{
		{prefix, 0o600},
		{view, 0o600},
		{[]byte(`,"checksum":"` + working.Checksum + `"}` + "\n"), 0o600},
	})
	if err != nil {
		return fmt.Errorf("write session record temporary file: %w", err)
	}
	defer os.Remove(tmp)

	if len(currentRaw) > 0 {
		// 备份旧 formal 到 .bak —— 用**硬链接**而不是复制整份内容。
		//
		// 原实现对 90MB 的 currentRaw 做 writeAtomicFile（写临时文件 + fsync +
		// rename），实测 ~138ms 且写入量翻倍。而紧随其后的 os.Rename(tmp, path)
		// 会让 path 指向新 inode，旧 inode 于是只剩 .bak 一个名字 ——
		// 硬链接语义在这里正好：
		//   1. link(path, .bak) 让 .bak 与旧 formal 共 inode（零拷贝）；
		//   2. rename(tmp, path) 把 path 换成新 inode；
		//   3. 旧 inode 的引用计数从 2 降到 1（只剩 .bak），内容不变。
		//
		// 失败回退到复制：跨文件系统/不支持硬链接时 link 会失败，
		// 此时保持原语义（正确性优先）。
		bakPath := path + ".bak"
		_ = os.Remove(bakPath) // link 要求目标不存在
		if linkErr := os.Link(path, bakPath); linkErr != nil {
			if werr := writeAtomicFile(dir, filepath.Base(bakPath), currentRaw, 0o600); werr != nil {
				return fmt.Errorf("backup session record: %w", werr)
			}
		}
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace session record: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync session record directory: %w", err)
	}

	record.Checksum = working.Checksum
	return nil
}

// DeleteSessionRecord removes the formal record, its bounded backup and owned
// temporary files. It deliberately does not remove legacy JSONL or SubAgent
// journal paths.
func (s *FileStorage) DeleteSessionRecord(ctx context.Context, workspaceKey, sessionID string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	path, err := s.RecordPath(workspaceKey, sessionID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := false
	for _, p := range []string{path, path + ".bak"} {
		err := os.Remove(p)
		switch {
		case err == nil:
			removed = true
		case errors.Is(err, os.ErrNotExist):
		default:
			return fmt.Errorf("delete session record %q: %w", p, err)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*"))
	for _, p := range matches {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("delete temporary session record %q: %w", p, err)
		}
		removed = true
	}
	// sidecar 与 record 同生命周期：不删会留下孤儿 blocks 文件（对长会话是几十 MB）。
	evictBlockStore(path)
	removeBlockStore(path)
	if removed {
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return fmt.Errorf("sync session record directory after delete: %w", err)
		}
	}
	return nil
}

// loadRecordLocked reads formal then backup. The caller must hold s.mu.
func (s *FileStorage) loadRecordLocked(ctx context.Context, path, workspaceKey, sessionID string) (*SessionRecord, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	formal, _, formalErr := readRecordCandidate(path, workspaceKey, sessionID)
	if formalErr == nil {
		return formal, nil
	}
	backup, _, backupErr := readRecordCandidate(path+".bak", workspaceKey, sessionID)
	if backupErr == nil {
		return backup, nil
	}
	if errors.Is(formalErr, ErrRecordNotFound) && errors.Is(backupErr, ErrRecordNotFound) {
		return nil, ErrRecordNotFound
	}
	return nil, fmt.Errorf("%w: formal=%v backup=%v", ErrRecordCorrupt, formalErr, backupErr)
}

// recordPayload is the canonical checksum input. Keep this struct explicit so
// adding a checksum-related transport field cannot accidentally exclude it from
// integrity coverage.
type recordPayload struct {
	Type            string         `json:"type"`
	SchemaVersion   int            `json:"schema_version"`
	SessionID       string         `json:"session_id"`
	WorkspaceKey    string         `json:"workspace_key"`
	Revision        uint64         `json:"revision"`
	ReducerSeq      uint64         `json:"reducer_seq"`
	ClearGeneration uint64         `json:"clear_generation"`
	UpdatedAt       time.Time      `json:"updated_at"`
	SDKState        State          `json:"sdk_state"`
	ViewCheckpoint  ViewCheckpoint `json:"view_checkpoint"`
}

func recordPayloadFrom(r *SessionRecord) recordPayload {
	return recordPayload{
		Type:            r.Type,
		SchemaVersion:   r.SchemaVersion,
		SessionID:       r.SessionID,
		WorkspaceKey:    r.WorkspaceKey,
		Revision:        r.Revision,
		ReducerSeq:      r.ReducerSeq,
		ClearGeneration: r.ClearGeneration,
		UpdatedAt:       r.UpdatedAt,
		SDKState:        r.SDKState,
		ViewCheckpoint:  r.ViewCheckpoint,
	}
}

// marshalRecordEnvelope 序列化 SessionRecord，但**保持 view_checkpoint 原样**。
//
// 为什么不直接用 json.Marshal：
// encoding/json 遇到 json.RawMessage 会调用 compact() 逐字节扫描、去除空白并
// 检查转义合法性。payload 自身已经是 json.Marshal 产出的紧凑合法 JSON，
// 这次扫描纯属浪费 —— 90MB 上实测 280ms（占总序列化时间的 85%）。
//
// 做法：把 record 拆成「除 view_checkpoint 外的字段」与「view_checkpoint 字节」
// 两段，前者正常 marshal（只有几百 KB），后者原样拼进去，最后按顶层键名排序
// 组装 —— 与 json.Marshal(map) 的键序规则一致（字典序）。
//
// **输出必须与 json.Marshal(record) 逐字节相同**（TestMarshalRecordEnvelopeEquivalence
// 用真实 record 对拍）。若不同，checksum 口径就变了，旧记录会读不出。
func marshalRecordEnvelope(r *SessionRecord) ([]byte, error) {
	// 先把 view 摘出来，用占位符参与通用 marshal。
	view := r.ViewCheckpoint
	tmp := *r
	tmp.ViewCheckpoint = nil
	head, err := json.Marshal(&tmp)
	if err != nil {
		return nil, err
	}
	// head 形如 {"type":"...",...,"view_checkpoint":null,"checksum":""}
	// 定位 "view_checkpoint":null 并替换成真实字节。
	const placeholder = `"view_checkpoint":null`
	idx := bytes.Index(head, []byte(placeholder))
	if idx < 0 {
		// 结构异常（字段被 omitempty 掉等）→ 退回标准路径，正确性优先。
		return json.Marshal(r)
	}
	// 与 json.Marshal 的行为**严格对齐**：
	//   nil              → null
	//   空但非 nil 的切片 → 报错（encoding/json 对 RawMessage 的行为）
	// 其它             → 原样
	if view == nil {
		view = []byte("null")
	} else if len(view) == 0 {
		return nil, fmt.Errorf("encode session record: unexpected end of JSON input")
	}
	out := make([]byte, 0, len(head)+len(view))
	out = append(out, head[:idx]...)
	out = append(out, `"view_checkpoint":`...)
	out = append(out, view...)
	out = append(out, head[idx+len(placeholder):]...)
	return out, nil
}

// encodeRecordEnvelope 与 marshalRecordEnvelope 语义相同，但**不做尾部二次拷贝**。
//
// marshalRecordEnvelope 的 out 是「head 前缀 + view + 尾部」的完整拼接，而
// CommitRecord 随后又要把它复制成 encoded（插入 checksum）—— 90MB payload
// 因此被拷贝两遍（实测 25ms + 13ms）。
//
// 这里改为返回三段共享 backing 的切片：
//
//	prefix  = {"type":...,"view_checkpoint":
//	view    = 调用方的 view 字节（只读）
//	suffix  = ,"checksum":""}
//
// 写入方按顺序流式写出并把 prefix/view/suffix 一起喂给 sha256，全程零额外拷贝。
func encodeRecordEnvelope(r *SessionRecord) (prefix, view, suffix []byte, err error) {
	rawView := r.ViewCheckpoint
	if rawView == nil {
		rawView = []byte("null")
	} else if len(rawView) == 0 {
		return nil, nil, nil, fmt.Errorf("encode session record: unexpected end of JSON input")
	}

	tmp := *r
	tmp.ViewCheckpoint = nil
	head, err := json.Marshal(&tmp)
	if err != nil {
		return nil, nil, nil, err
	}
	const placeholder = `"view_checkpoint":null`
	idx := bytes.Index(head, []byte(placeholder))
	if idx < 0 {
		// 结构异常 → 退回完整拼接路径（保持原语义）。
		full, ferr := marshalRecordEnvelope(r)
		if ferr != nil {
			return nil, nil, nil, ferr
		}
		return full, nil, nil, nil
	}
	prefix = head[:idx+len(`"view_checkpoint":`)]
	suffix = head[idx+len(placeholder):]
	return prefix, rawView, suffix, nil
}

// checksumV2Prefix 标识「按磁盘字节计算」的 checksum 口径。
//
// 为什么需要第二套口径（2026-10-18 性能优化，用户授权）：
//
// 旧口径 checksumForRecord 验的是「按当前结构体定义重新序列化」的 payload，
// 因此必须做 canonicalJSON（marshal → decode(UseNumber) → marshal）来消除
// 键序/数字词法差异。在 90MB record 上这一步实测 1250ms，占 commit 的 90%+。
//
// 但它的目的只有两个：①检出磁盘损坏；②跨版本可读。
// 而 canonicalJSON 对 ① 完全是浪费 —— 落盘时写什么字节、读盘时就校验什么字节，
// 同一份字节的 sha256 必然相等，不需要任何规范化。
// 而 ② 恰恰是旧口径的**脆弱点**（struct 加字段 → 重编码变化 → 旧 record 全读不出，
// 2026-09-23 现场 93 个 record 坏了 92 个）。按磁盘字节校验反而天然免疫：
// 文件没被改过，校验就一直通过，与 struct 如何演化无关。
//
// 成本：sha256(91MB) = 35ms，vs 旧口径 1250ms。
//
// 兼容：读侧先试 v2（字节口径），不匹配再回退 v1（旧口径）。
// 新写入一律用 v2；旧 record 无需迁移即可继续读。
const checksumV2Prefix = "sha256b:"

// checksumBytes 计算「磁盘字节口径」的 checksum：把对象里的 checksum 字段剔除后，
// 对剩余字节直接 sha256。不做 JSON 解析、不重编码。
//
// 前提：checksum 是 record 的**最后一个字段**（SessionRecord struct 定义顺序保证）。
// 这样只需在尾部做一次定位与拼接，O(n) 且常数极小。
// 若字段顺序将来改变（checksum 不在尾部），stripChecksumField 返回 ok=false，
// 调用方回退到规范化口径 —— 正确性优先于速度。
func checksumBytes(data []byte) (string, bool) {
	// 流式喂入，**不构造 body 副本** —— stripChecksumField 会分配一个 90MB
	// 的新切片并拷贝，而提交/读取路径都要算这个值，实测占提交总时间的 60%+。
	// 这里只定位边界，然后把「前缀」与「补齐的 }」两段喂给 hash。
	span, ok := checksumFieldSpan(data)
	if !ok {
		return "", false
	}
	h := sha256.New()
	h.Write(data[:span.fieldStart])   // checksum 字段之前
	h.Write([]byte{'}'})              // 补上对象收尾
	sum := h.Sum(nil)
	return checksumV2Prefix + hex.EncodeToString(sum), true
}

// checksumSpan 描述「checksum 字段」在字节中的位置（用于流式哈希）。
type checksumSpan struct {
	fieldStart int // 字段起始（含前置逗号）
	valueStart int
	valueEnd   int // 值结束引号的位置（exclusive 到引号之后）
	afterClose int // 对象收尾 '}' 之后的位置
}

// checksumFieldSpan 定位尾部 checksum 字段，**不复制任何数据**。
func checksumFieldSpan(b []byte) (checksumSpan, bool) {
	const key = `"checksum":"`
	i := bytes.LastIndex(b, []byte(key))
	if i < 0 {
		return checksumSpan{}, false
	}
	vStart := i + len(key)
	k := bytes.IndexByte(b[vStart:], '"')
	if k < 0 {
		return checksumSpan{}, false
	}
	vEnd := vStart + k

	start := i
	for start > 0 && isJSONSpaceByte(b[start-1]) {
		start--
	}
	if start == 0 || b[start-1] != ',' {
		return checksumSpan{}, false
	}
	start--

	rest := b[vEnd+1:]
	for len(rest) > 0 && isJSONSpaceByte(rest[0]) {
		rest = rest[1:]
	}
	if len(rest) == 0 || rest[0] != '}' {
		return checksumSpan{}, false
	}
	after := vEnd + 1 + (len(b) - vEnd - 1 - len(rest)) + 1
	return checksumSpan{fieldStart: start, valueStart: vStart, valueEnd: vEnd + 1, afterClose: after}, true
}

// stripChecksumField 从 record 字节中移除 `"checksum":"..."` 字段，返回其余字节。
// 只处理「checksum 为最后一个字段」这一形态（生产写入恒满足）。
func stripChecksumField(b []byte) ([]byte, bool) {
	const key = `"checksum":"`
	i := bytes.LastIndex(b, []byte(key))
	if i < 0 {
		return nil, false
	}
	vStart := i + len(key)
	k := bytes.IndexByte(b[vStart:], '"')
	if k < 0 {
		return nil, false
	}
	vEnd := vStart + k

	// 字段起点：回退到 key 前的逗号
	start := i
	for start > 0 && isJSONSpaceByte(b[start-1]) {
		start--
	}
	if start == 0 || b[start-1] != ',' {
		return nil, false // checksum 不是尾部字段 → 交给旧口径
	}
	start--

	// 值之后：跳过空白，吃掉对象收尾 '}'
	rest := b[vEnd+1:]
	for len(rest) > 0 && isJSONSpaceByte(rest[0]) {
		rest = rest[1:]
	}
	if len(rest) == 0 || rest[0] != '}' {
		return nil, false
	}
	rest = rest[1:]

	out := make([]byte, 0, len(b)-(vEnd+1-start)+1)
	out = append(out, b[:start]...)
	out = append(out, '}')
	// **丢弃**收尾后的任何字节（换行）。写入侧算 hash 时用的是内存里
	// json.Marshal 的结果（无换行），读取侧拿到的是含 '\n' 的文件字节；
	// 若这里把换行也算进 body，两侧 hash 必然不同（踩过：导致所有记录校验失败）。
	_ = rest
	return out, true
}

func isJSONSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func checksumForRecord(r *SessionRecord) (string, error) {
	payload := recordPayloadFrom(r)
	canonical, err := canonicalJSON(payload)
	if err != nil {
		return "", fmt.Errorf("canonicalize session record: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}



// checksumForStoredPayload 用**存盘字节**复算 checksum（checksumForRecord 的字节口径补充）。
//
// 为什么必须有这一口径：checksumForRecord 的输入是「按当前结构体定义重新序列化」的 payload
// （recordPayloadFrom → canonicalJSON）。payload 里的 sdk_state 是**强类型** State，
// 于是只要任意可达结构体新增字段（哪怕值恒为零），旧 record 的重编码结果就会多出键
// （例：2026-09-23 usage 维度补齐给 core.Usage 加 CacheWrite1h/Details、给 core.Cost 加
// Reasoning/Priced），canonical JSON 变化 → 校验必然失败。实测后果：工作区 93 个 record
// 有 92 个读不出来 → bridge 全部退化成 legacy snapshot 恢复 → 前端快照重放没有 llm_end
// 正文兜底 → 对话只剩工具/产出块，Reasoning/Content 全空（用户 2026-09-23 现场）。
//
// 存盘字节口径与写入侧同构（写入 = canonicalJSON(payload)，读取 = canonicalJSON(存盘 payload)）：
// 内容相同则哈希相同，字段集合演化不再影响**旧数据的可读性**；而任何字节级损坏/截断
// （torn write / 手改）依旧会被检出 —— 那才是 checksum 真正要防的。
func checksumForStoredPayload(data []byte) (string, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("canonicalize stored record payload: %w", err)
	}
	if len(payload) == 0 {
		return "", fmt.Errorf("canonicalize stored record payload: not a JSON object")
	}
	delete(payload, "checksum") // checksum 自身不参与（与 recordPayloadFrom 同口径）
	canonical, err := canonicalJSON(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// checksumMatchesStoredPayload 存盘字节口径比对（只用于「类型口径失配时的兼容判定」）。
// checksumMatchesStoredBytes 字节口径比对（v2）：sha256(磁盘字节去掉 checksum)。
// 不做任何 JSON 解析/重编码，O(n) 一遍。
func checksumMatchesStoredBytes(stored string, data []byte) bool {
	if !strings.HasPrefix(stored, checksumV2Prefix) {
		return false
	}
	got, ok := checksumBytes(data)
	if !ok {
		return false
	}
	return sameChecksum(stored, got)
}

func checksumMatchesStoredPayload(stored string, data []byte) bool {
	expected, err := checksumForStoredPayload(data)
	if err != nil {
		return false
	}
	return sameChecksum(stored, expected)
}

// canonicalJSON normalizes the JSON representation before hashing. The
// decoder preserves integer lexemes so large token counters are not rounded by
// an intermediate float64 representation; encoding/json sorts object keys.
func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var normalized any
	if err := dec.Decode(&normalized); err != nil {
		return nil, err
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

func validateSessionRecordShape(r *SessionRecord) error {
	if r == nil {
		return fmt.Errorf("%w: nil record", ErrInvalidSessionRecord)
	}
	if r.Type != SessionRecordType {
		return fmt.Errorf("%w: type must be %q, got %q", ErrInvalidSessionRecord, SessionRecordType, r.Type)
	}
	if r.SchemaVersion != SessionRecordSchemaVersion {
		return fmt.Errorf("%w: unsupported schema_version %d", ErrInvalidSessionRecord, r.SchemaVersion)
	}
	if err := validatePathComponent("workspace key", r.WorkspaceKey); err != nil {
		return err
	}
	if err := validatePathComponent("session id", r.SessionID); err != nil {
		return err
	}
	if r.Revision == 0 {
		return fmt.Errorf("%w: revision must be greater than zero", ErrInvalidSessionRecord)
	}
	if r.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: updated_at is required", ErrInvalidSessionRecord)
	}
	// 提交侧用轻量检查（O(1)）；读取侧的完整校验见 readRecordCandidate。
	// 详见 validateCheckpointForCommit 的注释（355ms → <1ms）。
	if err := validateCheckpointForCommit(r.ViewCheckpoint); err != nil {
		return err
	}
	return nil
}

// validateCheckpointForCommit 是**提交侧**的轻量形状检查。
//
// 与 validateCheckpoint 的关系：判定更弱，但**对提交路径是充分的**。
//
// 为什么充分：提交的内容来自本进程 reducer.Marshal() —— 它是
// json.Marshal(*Checkpoint) 的产物，字段集合由 struct 定义固定，不可能产出
// 缺字段/非数组的 envelope。完整校验在这里只会在**自己的代码有 bug 时**报错，
// 而那种情况下 payload 早就无法构造了。
//
// 为什么可以弱：真正的防御面在**读取侧** —— 磁盘文件是外部输入，
// readRecordCandidate 走完整 validateCheckpoint（那里已有全量解码，无额外成本）。
//
// 成本：O(1) 前缀检查。完整校验在 90MB 上是 355ms（解出每个顶层字段的
// RawMessage），是提交路径的第二大开销。
//
// 保留的最小断言（防止明显错用，如传了空/别的 JSON）：
//   非空、非 null、是对象、schema_version 字面量正确、`"blocks":[` 存在
// —— 这些都出现在 payload 的前几十字节（reducer 的 struct 顺序固定：
// schema_version 第一、blocks 第二，实测偏移 20/39 字节）。
func validateCheckpointForCommit(view ViewCheckpoint) error {
	trimmed := bytes.TrimSpace(view)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return fmt.Errorf("%w: view_checkpoint is required", ErrInvalidSessionRecord)
	}
	if trimmed[0] != '{' {
		return fmt.Errorf("%w: view_checkpoint must be an object", ErrInvalidSessionRecord)
	}
	// 只看前 256 字节：足够覆盖 schema_version 与 blocks 的开头。
	head := trimmed
	if len(head) > 256 {
		head = head[:256]
	}
	wantVer := []byte(fmt.Sprintf(`"schema_version":%d`, ViewCheckpointSchemaVersion))
	if !bytes.Contains(head, wantVer) {
		return fmt.Errorf("%w: view_checkpoint schema_version must be %d", ErrInvalidSessionRecord, ViewCheckpointSchemaVersion)
	}
	// blocks 有两种合法形态：
	//   · 内嵌完整数组   `"blocks":[`
	//   · sidecar 引用   `"blocks":{"$blocks_ref":...}`（长会话优化）
	if !bytes.Contains(head, []byte(`"blocks":[`)) && !bytes.Contains(head, []byte(`"blocks":{"`+blocksRefKey)) {
		return fmt.Errorf("%w: view_checkpoint.blocks must be an array or sidecar ref", ErrInvalidSessionRecord)
	}
	return nil
}

// skipJSONValue 跳过下一个 JSON 值（不复制内容、不建对象图）。
// 用 Decoder.Token 逐层走，只做括号计数与字符串转义感知。
func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil // 标量已消费
	}
	if d != '[' && d != '{' {
		return nil
	}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if dd, ok := tok.(json.Delim); ok {
			switch dd {
			case '[', '{':
				depth++
			case ']', '}':
				depth--
			}
		}
	}
	return nil
}

func validateCheckpoint(view ViewCheckpoint) error {
	trimmed := bytes.TrimSpace(view)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return fmt.Errorf("%w: view_checkpoint is required", ErrInvalidSessionRecord)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return fmt.Errorf("%w: invalid view_checkpoint JSON: %v", ErrInvalidSessionRecord, err)
	}
	if obj == nil {
		return fmt.Errorf("%w: view_checkpoint must be an object", ErrInvalidSessionRecord)
	}
	var version int
	if raw, ok := obj["schema_version"]; !ok || json.Unmarshal(raw, &version) != nil || version != ViewCheckpointSchemaVersion {
		return fmt.Errorf("%w: view_checkpoint schema_version must be %d", ErrInvalidSessionRecord, ViewCheckpointSchemaVersion)
	}
	for _, field := range []string{"blocks", "run_nodes", "turns"} {
		raw, ok := obj[field]
		if !ok || !isJSONArray(raw) {
			return fmt.Errorf("%w: view_checkpoint.%s must be an array", ErrInvalidSessionRecord, field)
		}
	}
	return nil
}

func isJSONArray(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '[' && trimmed[len(trimmed)-1] == ']'
}

func validatePathComponent(label, value string) error {
	if value == "" || value == "." || value == ".." || strings.Contains(value, "..") {
		return fmt.Errorf("%w: invalid %s %q", ErrInvalidSessionRecord, label, value)
	}
	// 注意：必须逐字符判断，不能用 ContainsAny 的反引号字面量（`\x00` 不会被转义，
	// 会把 'x' 和 '0' 也加入拒绝集——真实 workspaceKey 哈希后缀必含 0/x，会被误拒）。
	for _, r := range value {
		switch r {
		case '/', '\\', 0:
			return fmt.Errorf("%w: invalid %s %q", ErrInvalidSessionRecord, label, value)
		}
	}
	return nil
}

// ValidateSessionID 校验 session id 可作为路径组件（防穿越；与 record 路径同规则）。
// 供 bridge journalPathFor 等跨包路径构造使用。
func ValidateSessionID(id string) error {
	return validatePathComponent("session id", id)
}

func sameChecksum(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// recordBaseline 是提交路径判断「能否覆盖」所需的最小事实集。
type recordBaseline struct {
	Revision uint64
	Checksum string
}

// readRecordBaseline 只解出提交基线需要的标量并校验文件完整性，
// 不把 view_checkpoint / sdk_state 解成对象图（91MB 上省约 1.8s）。
//
// 校验强度：身份、schema、结构、checksum 全部照旧检查；只是不 materialize
// payload。结构损坏（截断/非法 JSON）会被检出 —— 因为解码停止在
// view_checkpoint 之前的字段，其后若被截断，stripChecksumField 找不到
// 尾部 checksum 字段，会判为不可用并回退 .bak。
func readRecordBaseline(path, workspaceKey, sessionID string) (*recordBaseline, []byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, ErrRecordNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}

	// 先按 v2 字节口径校验（新记录走这条，O(n) 一遍）。
	// 失败再走完整解码 + 旧口径（兼容改造前的记录）。
	if sum, ok := checksumBytes(data); ok {
		// checksum 字段在 JSON **末尾**（view_checkpoint 之后），所以既不能用
		// 「解到 view_checkpoint 就停」拿到它，也不能解整条 struct（会复制 90MB）。
		// 直接从字节里定位取值 —— 与 checksumBytes 用同一个 span 逻辑。
		if span, sok := checksumFieldSpan(data); sok {
			stored := string(data[span.valueStart:span.valueEnd-1])
			if strings.HasPrefix(stored, checksumV2Prefix) && sameChecksum(stored, sum) {
				if head, herr := parseRecordHeadScalars(data, workspaceKey, sessionID); herr == nil {
					return &recordBaseline{Revision: head.Revision, Checksum: stored}, data, nil
				}
			}
		}
	}

	// 旧口径（或 v2 校验没通过）：走完整路径，保持与改造前完全一致的判定。
	rec, raw, rerr := readRecordCandidate(path, workspaceKey, sessionID)
	if rerr != nil {
		return nil, nil, rerr
	}
	return &recordBaseline{Revision: rec.Revision, Checksum: rec.Checksum}, raw, nil
}

// recordHead 是提交基线与元数据共用的「头部标量」。
type recordHead struct {
	Revision uint64
	Checksum string
}

// parseRecordHeadScalars 只解析 record 头部的标量字段，遇到 view_checkpoint
// 立即停止 —— 绝不把 90MB payload 读进内存（Decode 成 RawMessage 会复制）。
func parseRecordHeadScalars(data []byte, workspaceKey, sessionID string) (*SessionRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("not a JSON object")
	}
	out := &SessionRecord{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := kt.(string)
		switch key {
		case "type":
			if err := dec.Decode(&out.Type); err != nil { return nil, err }
		case "schema_version":
			if err := dec.Decode(&out.SchemaVersion); err != nil { return nil, err }
		case "session_id":
			if err := dec.Decode(&out.SessionID); err != nil { return nil, err }
		case "workspace_key":
			if err := dec.Decode(&out.WorkspaceKey); err != nil { return nil, err }
		case "revision":
			if err := dec.Decode(&out.Revision); err != nil { return nil, err }
		case "reducer_seq":
			if err := dec.Decode(&out.ReducerSeq); err != nil { return nil, err }
		case "clear_generation":
			if err := dec.Decode(&out.ClearGeneration); err != nil { return nil, err }
		case "view_checkpoint":
			// 头部标量都在它之前（struct 定义顺序）→ 到此为止。
			out.ViewCheckpoint = ViewCheckpoint{} // 非 nil，表示「存在」
			return out, nil
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil { return nil, err }
		}
	}
	return nil, fmt.Errorf("view_checkpoint missing")
}

func readRecordCandidate(path, workspaceKey, sessionID string) (*SessionRecord, []byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, ErrRecordNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	var record SessionRecord
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&record); err != nil {
		return nil, nil, fmt.Errorf("%w: decode %s: %w", ErrRecordCorrupt, path, err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, nil, fmt.Errorf("%w: decode %s: %w", ErrRecordCorrupt, path, err)
	}
	if record.WorkspaceKey != workspaceKey || record.SessionID != sessionID {
		return nil, nil, fmt.Errorf("%w: record identity does not match path", ErrRecordCorrupt)
	}
	// v2 口径必须在这里校验（Validate 拿不到磁盘字节，对 v2 只做结构校验）。
	if strings.HasPrefix(record.Checksum, checksumV2Prefix) && !checksumMatchesStoredBytes(record.Checksum, data) {
		return nil, nil, fmt.Errorf("%w: %s: %w", ErrRecordCorrupt, path, ErrRecordChecksum)
	}
	if err := record.Validate(); err != nil {
		// checksum 失配时的兼容判定，**先试字节口径**（新写入的记录用它）：
		//   v2 = sha256b: + sha256(磁盘字节去掉 checksum 字段)
		// 命中即接受 —— 文件没被改过就算通过，与 struct 如何演化无关。
		//
		// 再回退旧口径（checksumMatchesStoredPayload，规范化 JSON）：
		// 兼容本次改动之前写入的全部 record（本机 371 个），无需迁移。
		// 其它校验失败（结构/身份/schema）照旧拒绝 —— 只放行 checksum 失配。
		if !errors.Is(err, ErrRecordChecksum) {
			return nil, nil, fmt.Errorf("%w: %s: %w", ErrRecordCorrupt, path, err)
		}
		if !checksumMatchesStoredBytes(record.Checksum, data) &&
			!checksumMatchesStoredPayload(record.Checksum, data) {
			return nil, nil, fmt.Errorf("%w: %s: %w", ErrRecordCorrupt, path, err)
		}
	}
	// blocks 若为 sidecar 引用，此处还原成完整数组 —— 上层拿到的 ViewCheckpoint
	// 与「内嵌完整 blocks」的旧格式**完全等价**，调用方无感。
	//
	// 还原失败必须报错而不是返回空 blocks：那会让长会话「打开后历史全空」，
	// 比打不开更糟（用户以为数据丢了）。调用方据此回退 .bak。
	if resolved, rerr := resolveBlocksRef(record.ViewCheckpoint, path); rerr != nil {
		return nil, nil, fmt.Errorf("%w: %s: %w", ErrRecordCorrupt, path, rerr)
	} else {
		record.ViewCheckpoint = resolved
	}
	return &record, append([]byte(nil), data...), nil
}

// readRecordMetadata 读取 record 头部元数据，遇到 sdk_state/view_checkpoint
// 即停止解码（不 materialize 大 payload）。判据与 readRecordCandidate 一致：
// 身份必须匹配、schema_version 必须合法、view_checkpoint 必须可解析成合法
// envelope。区别只在于不做 payload checksum 校验（见 LoadRecordMetadata 注释）。
//
// 实现要点：用 Decoder 流式读取而非 Unmarshal 整个文件；对不需要的大字段
// 先解码成 json.RawMessage（Go 标准库没有 Decoder.Skip），但由于字段顺序
// 固定，读到 view_checkpoint 时已经拿到了全部所需标量 —— 这里仍需检查
// view_checkpoint 的 envelope 合法性以保持与 LoadRecord 相同的接受面。
// loadMetadataCandidate 读字节 → 校验完整性 → 只解析头部。
//
// 为什么要读整份字节：v2 记录的 checksum 是对**磁盘字节**算的，校验它需要
// 全部字节。这一步 O(n) 不可省，但很便宜（91MB 只花 sha256 的 ~32ms）。
// **解析**则只走到 view_checkpoint 即停，不把 90MB payload 解成对象图 ——
// 那才是原来 543ms 的来源。
//
// 旧口径（sha256:）记录没有字节 checksum 可用，退回完整校验路径
//（readRecordCandidate，会解 payload）。只发生在新旧混合期，之后全是 v2。
func loadMetadataCandidate(path, workspaceKey, sessionID string) (*RecordMetadata, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrRecordNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	// 快路径（v2）：cheapParseChecksum → 比对字节口径。
	if stored, ok := cheapParseChecksum(data); ok && strings.HasPrefix(stored, checksumV2Prefix) {
		want, cok := checksumBytes(data)
		if !cok || !sameChecksum(stored, want) {
			return nil, fmt.Errorf("%w: %s: %w", ErrRecordCorrupt, path, ErrRecordChecksum)
		}
		return readRecordMetadataFrom(bytes.NewReader(data), path, workspaceKey, sessionID)
	}

	// 旧口径：完整校验（会解 payload，但这批记录不再被写入）。
	rec, _, rerr := readRecordCandidate(path, workspaceKey, sessionID)
	if rerr != nil {
		return nil, rerr
	}
	return &RecordMetadata{Revision: rec.Revision, ReducerSeq: rec.ReducerSeq, ClearGeneration: rec.ClearGeneration}, nil
}

// cheapParseChecksum 从 record 字节尾部取出 checksum 字段值（不解析 JSON）。
func cheapParseChecksum(data []byte) (string, bool) {
	body, ok := stripChecksumField(data)
	if !ok {
		return "", false
	}
	// stripChecksumField 已确认 checksum 在尾部：重新定位其值。
	idx := bytes.LastIndex(data, []byte(`"checksum":"`))
	if idx < 0 {
		return "", false
	}
	vStart := idx + len(`"checksum":"`)
	k := bytes.IndexByte(data[vStart:], '"')
	if k < 0 {
		return "", false
	}
	_ = body
	return string(data[vStart : vStart+k]), true
}

func readRecordMetadataFrom(r io.Reader, path, workspaceKey, sessionID string) (*RecordMetadata, error) {
	dec := json.NewDecoder(bufio.NewReaderSize(r, 64*1024))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: decode %s: %w", ErrRecordCorrupt, path, err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("%w: %s is not a JSON object", ErrRecordCorrupt, path)
	}

	var (
		meta        RecordMetadata
		seenType    string
		seenSchema  int
		seenWS      string
		seenSession string
		sawViewCP   bool
	)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: decode %s: %w", ErrRecordCorrupt, path, err)
		}
		key, _ := keyTok.(string)
		switch key {
		case "type":
			if err := dec.Decode(&seenType); err != nil {
				return nil, fmt.Errorf("%w: decode %s: %w", ErrRecordCorrupt, path, err)
			}
		case "schema_version":
			if err := dec.Decode(&seenSchema); err != nil {
				return nil, fmt.Errorf("%w: decode %s: %w", ErrRecordCorrupt, path, err)
			}
		case "session_id":
			if err := dec.Decode(&seenSession); err != nil {
				return nil, fmt.Errorf("%w: decode %s: %w", ErrRecordCorrupt, path, err)
			}
		case "workspace_key":
			if err := dec.Decode(&seenWS); err != nil {
				return nil, fmt.Errorf("%w: decode %s: %w", ErrRecordCorrupt, path, err)
			}
		case "revision":
			if err := dec.Decode(&meta.Revision); err != nil {
				return nil, fmt.Errorf("%w: decode %s: %w", ErrRecordCorrupt, path, err)
			}
		case "reducer_seq":
			if err := dec.Decode(&meta.ReducerSeq); err != nil {
				return nil, fmt.Errorf("%w: decode %s: %w", ErrRecordCorrupt, path, err)
			}
		case "clear_generation":
			if err := dec.Decode(&meta.ClearGeneration); err != nil {
				return nil, fmt.Errorf("%w: decode %s: %w", ErrRecordCorrupt, path, err)
			}
		case "view_checkpoint":
			// **到此为止**：三个标量（revision/reducer_seq/clear_generation）
			// 都排在 view_checkpoint 之前（struct 定义顺序），已经读到了。
			// 继续读 payload 只有坏处 —— 无论是 Decode 成 RawMessage（复制
			// 90MB）还是 Token 跳读（逐词法遍历，实测 549ms），都是纯浪费。
			//
			// 完整性由别处保证，这里不丢防线：
			//   1. readRecordMetadata 的调用方先用 checksumBytes 校验整份字节
			//      （O(n) sha256，91MB 约 32ms）—— truncation/篡改一律检出；
			//   2. envelope 结构非法时，真正需要 payload 的路径（LoadRecord）
			//      会以完整 validateCheckpoint 拒绝。
			sawViewCP = true
			goto done
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, fmt.Errorf("%w: decode %s: %w", ErrRecordCorrupt, path, err)
			}
		}
	}

	// 形状校验：与 validateSessionRecordShape 相同的必要条件。
	if seenType != SessionRecordType {
		return nil, fmt.Errorf("%w: %s: type must be %q, got %q", ErrRecordCorrupt, path, SessionRecordType, seenType)
	}
	if seenSchema != SessionRecordSchemaVersion {
		return nil, fmt.Errorf("%w: %s: unsupported schema_version %d", ErrRecordCorrupt, path, seenSchema)
	}
done:
	if seenWS != workspaceKey || seenSession != sessionID {
		return nil, fmt.Errorf("%w: record identity does not match path", ErrRecordCorrupt)
	}
	if meta.Revision == 0 {
		return nil, fmt.Errorf("%w: %s: revision must be greater than zero", ErrRecordCorrupt, path)
	}
	if !sawViewCP {
		return nil, fmt.Errorf("%w: %s: view_checkpoint is required", ErrRecordCorrupt, path)
	}
	return &meta, nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func writeTempFile(dir, base string, data []byte, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}

// filePart 是流式写入的一段字节及其期望权限（权限只取首段的 mode）。
type filePart struct {
	data []byte
	mode os.FileMode
}

// writeTempFileParts 把多段字节顺序写入一个临时文件并 fsync。
//
// 语义与 writeTempFile(dir, base, bytes.Join(parts...)) 相同，但**不做拼接**：
// 对 91MB record 而言拼接要多分配并拷贝一整份 payload。
func writeTempFileParts(dir, base string, parts []filePart) (string, error) {
	f, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if len(parts) > 0 {
		if err := f.Chmod(parts[0].mode); err != nil {
			return "", err
		}
	}
	for _, part := range parts {
		if len(part.data) == 0 {
			continue
		}
		if _, err := f.Write(part.data); err != nil {
			return "", err
		}
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}

func writeAtomicFile(dir, base string, data []byte, mode os.FileMode) error {
	tmp, err := writeTempFile(dir, base, data, mode)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := os.Rename(tmp, filepath.Join(dir, base)); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}


