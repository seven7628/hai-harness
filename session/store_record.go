package session

import (
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
	working, err := record.Clone()
	if err != nil {
		return err
	}
	if working.Checksum != "" {
		expected, checksumErr := checksumForRecord(working)
		if checksumErr != nil {
			return checksumErr
		}
		if !sameChecksum(working.Checksum, expected) {
			return fmt.Errorf("%w: supplied checksum does not match payload", ErrRecordChecksum)
		}
	}
	working.Checksum, err = checksumForRecord(working)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(working)
	if err != nil {
		return fmt.Errorf("encode session record: %w", err)
	}
	encoded = append(encoded, '\n')

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

	current, currentRaw, currentErr := readRecordCandidate(path, working.WorkspaceKey, working.SessionID)
	if currentErr != nil {
		// A valid bounded backup is the only fallback baseline. This applies
		// both when the formal file is corrupt and when it is absent (for
		// example, an interrupted replacement), so a stale revision cannot
		// overwrite the last known-good record.
		backup, backupRaw, backupErr := readRecordCandidate(path+".bak", working.WorkspaceKey, working.SessionID)
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
	tmp, err := writeTempFile(dir, filepath.Base(path), encoded, 0o600)
	if err != nil {
		return fmt.Errorf("write session record temporary file: %w", err)
	}
	defer os.Remove(tmp)

	if len(currentRaw) > 0 {
		// Write the backup before touching the formal path. If this fails, the
		// old formal record remains untouched and the commit is safely aborted.
		if err := writeAtomicFile(dir, filepath.Base(path)+".bak", currentRaw, 0o600); err != nil {
			return fmt.Errorf("backup session record: %w", err)
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

func checksumForRecord(r *SessionRecord) (string, error) {
	canonical, err := canonicalJSON(recordPayloadFrom(r))
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
	if err := validateCheckpoint(r.ViewCheckpoint); err != nil {
		return err
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
	if err := record.Validate(); err != nil {
		// 类型口径失配时的兼容判定（见 checksumForStoredPayload）：旧 record 的 checksum 是对
		// 「写入当时的编码」算的，结构体后续新增字段会让重编码结果变化 —— 存盘字节口径命中即接受。
		// 只放行 checksum 失配；结构/身份/schema 等其它校验失败照旧拒绝。
		if !errors.Is(err, ErrRecordChecksum) || !checksumMatchesStoredPayload(record.Checksum, data) {
			return nil, nil, fmt.Errorf("%w: %s: %w", ErrRecordCorrupt, path, err)
		}
	}
	return &record, append([]byte(nil), data...), nil
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
