package agenthub

// archive.go：群日志 / DM 索引的冷归档（P4-04b-2a）。
//
// 边界：只归档 FileStore 的 groups/<gid>/log.jsonl 与 dms/<aid>/log.jsonl。
// activity、session checkpoint、artifact 不在归档范围内；本文件**没有任何删除动作**，
// 归档只是把热日志里较早的消息搬进不可变段。
//
// 布局：
//
//	archive/groups/<gid>/manifest.json
//	archive/groups/<gid>/<startSeq>-<endSeq>.jsonl
//	archive/dms/<aid>/manifest.json
//	archive/dms/<aid>/<startSeq>-<endSeq>.jsonl
//
// 失败语义：先写段 → 再提交 manifest → 最后裁剪热日志。任何一步失败都只会
// 留下「孤儿段」或「热日志里重复的已归档消息」，读取侧按 seq 去重（热优先），
// 下一次归档先修复；不会丢消息。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// ArchiveSchemaVersion 是 manifest 的 schema 版本。只接受 1；更高版本视为损坏
	// （fail-fast，避免旧进程按旧语义改写新格式）。
	ArchiveSchemaVersion = 1
	// ArchiveScopeGroups / ArchiveScopeDMs 是 archive/<scope>/<id> 的两级作用域。
	ArchiveScopeGroups = "groups"
	ArchiveScopeDMs    = "dms"
)

// ArchivePolicy 是冷归档阈值。零值字段回落到 DefaultArchivePolicy()；
// Percent 是「本次归档热日志最旧的比例」。
type ArchivePolicy struct {
	MaxMessages int
	MaxBytes    int64
	KeepRecent  int
	Percent     int
}

// DefaultArchivePolicy 返回默认策略（消息默认永久保留，只有热日志过大才归档）。
func DefaultArchivePolicy() ArchivePolicy {
	return ArchivePolicy{
		MaxMessages: 20000,
		MaxBytes:    50 << 20,
		KeepRecent:  2000,
		Percent:     70,
	}
}

// withDefaults 把零值字段补成默认值；调用方只需关心自己覆盖的字段。
func (p ArchivePolicy) withDefaults() ArchivePolicy {
	def := DefaultArchivePolicy()
	if p.MaxMessages <= 0 {
		p.MaxMessages = def.MaxMessages
	}
	if p.MaxBytes <= 0 {
		p.MaxBytes = def.MaxBytes
	}
	if p.KeepRecent <= 0 {
		p.KeepRecent = def.KeepRecent
	}
	if p.Percent <= 0 || p.Percent > 100 {
		p.Percent = def.Percent
	}
	return p
}

// ArchiveSegment 是一个不可变归档段的 manifest 行。Bytes/sha256 用于读取时校验。
type ArchiveSegment struct {
	StartSeq   int64     `json:"start_seq"`
	EndSeq     int64     `json:"end_seq"`
	Count      int       `json:"count"`
	Bytes      int64     `json:"bytes"`
	SHA256     string    `json:"sha256"`
	File       string    `json:"file"`
	ArchivedAt time.Time `json:"archived_at"`
}

// ArchiveManifest 是每个会话一份的归档索引；段按 start_seq 升序、互不重叠。
type ArchiveManifest struct {
	SchemaVersion int              `json:"schema_version"`
	Scope         string           `json:"scope"`
	ID            string           `json:"id"`
	Segments      []ArchiveSegment `json:"segments"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

// ArchiveResult 汇报一次归档的结果；Skipped 表示没有写新段（未到阈值或触到保留下限）。
type ArchiveResult struct {
	Scope        string          `json:"scope"`
	ID           string          `json:"id"`
	Archived     int             `json:"archived"`
	Segment      *ArchiveSegment `json:"segment,omitempty"`
	HotRemaining int             `json:"hot_remaining"`
	HotBytes     int64           `json:"hot_bytes"`
	Skipped      bool            `json:"skipped"`
	Repaired     bool            `json:"repaired,omitempty"`
	Reason       string          `json:"reason,omitempty"`
}

// archiveSegmentFile 是段的规范文件名；manifest 校验会拒绝任何别的命名。
func archiveSegmentFile(start, end int64) string {
	return fmt.Sprintf("%d-%d.jsonl", start, end)
}

func (s *FileStore) archiveDir(scope, id string) string {
	return filepath.Join(s.root, "archive", scope, id)
}

func (s *FileStore) archiveManifestPath(scope, id string) string {
	return filepath.Join(s.archiveDir(scope, id), "manifest.json")
}

// archiveHotLogPath 返回归档作用域对应的热日志路径。
func (s *FileStore) archiveHotLogPath(scope, id string) string {
	if scope == ArchiveScopeDMs {
		return s.dmLogPath(AgentID(id))
	}
	return s.messagesPath(GroupID(id))
}

func validateArchiveTarget(scope, id string) error {
	switch scope {
	case ArchiveScopeGroups, ArchiveScopeDMs:
	default:
		return fmt.Errorf("%w: archive scope %q", ErrInvalid, scope)
	}
	if !validID(id) {
		return fmt.Errorf("%w: archive id %q", ErrInvalid, id)
	}
	return nil
}

// LoadArchiveManifest 读取并校验 manifest（测试 / 诊断用）。没有归档返回 ErrNotFound。
func (s *FileStore) LoadArchiveManifest(ctx context.Context, scope, id string) (*ArchiveManifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateArchiveTarget(scope, id); err != nil {
		return nil, err
	}
	var m ArchiveManifest
	if err := readJSON(s.archiveManifestPath(scope, id), &m); err != nil {
		return nil, err
	}
	if err := validateArchiveManifest(&m, scope, id); err != nil {
		return nil, err
	}
	return &m, nil
}

// readArchiveManifest 是内部读取：缺失时返回空 manifest，便于归档/重建索引走同一条路。
func (s *FileStore) readArchiveManifest(scope, id string) (*ArchiveManifest, error) {
	var m ArchiveManifest
	err := readJSON(s.archiveManifestPath(scope, id), &m)
	if errors.Is(err, ErrNotFound) {
		return &ArchiveManifest{SchemaVersion: ArchiveSchemaVersion, Scope: scope, ID: id}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := validateArchiveManifest(&m, scope, id); err != nil {
		return nil, err
	}
	return &m, nil
}

func validateArchiveManifest(m *ArchiveManifest, scope, id string) error {
	if m == nil {
		return fmt.Errorf("%w: nil archive manifest", ErrCorrupt)
	}
	if m.SchemaVersion != ArchiveSchemaVersion {
		return fmt.Errorf("%w: archive schema %d want %d", ErrCorrupt, m.SchemaVersion, ArchiveSchemaVersion)
	}
	if m.Scope != scope || m.ID != id {
		return fmt.Errorf("%w: manifest scope/id %q/%q want %q/%q", ErrCorrupt, m.Scope, m.ID, scope, id)
	}
	if !validID(id) {
		return fmt.Errorf("%w: manifest id %q", ErrCorrupt, id)
	}
	var prevEnd int64
	for i, seg := range m.Segments {
		if seg.StartSeq <= 0 || seg.EndSeq < seg.StartSeq {
			return fmt.Errorf("%w: segment %d range %d-%d", ErrCorrupt, i, seg.StartSeq, seg.EndSeq)
		}
		if seg.Count <= 0 || int64(seg.Count) > seg.EndSeq-seg.StartSeq+1 {
			return fmt.Errorf("%w: segment %d count %d for range %d-%d", ErrCorrupt, i, seg.Count, seg.StartSeq, seg.EndSeq)
		}
		if seg.Bytes < 0 {
			return fmt.Errorf("%w: segment %d bytes %d", ErrCorrupt, i, seg.Bytes)
		}
		if len(seg.SHA256) != sha256.Size*2 || !isHex(seg.SHA256) {
			return fmt.Errorf("%w: segment %d sha256 %q", ErrCorrupt, i, seg.SHA256)
		}
		if seg.File != archiveSegmentFile(seg.StartSeq, seg.EndSeq) || filepath.Base(seg.File) != seg.File {
			return fmt.Errorf("%w: segment %d file %q", ErrCorrupt, i, seg.File)
		}
		if seg.ArchivedAt.IsZero() {
			return fmt.Errorf("%w: segment %d archived_at", ErrCorrupt, i)
		}
		if i > 0 && seg.StartSeq <= prevEnd {
			return fmt.Errorf("%w: segment %d overlaps previous end %d", ErrCorrupt, i, prevEnd)
		}
		prevEnd = seg.EndSeq
	}
	return nil
}

func isHex(s string) bool {
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// readArchiveSegment 读取一个段并校验字节数 / sha256 / count；任何不一致都返回 ErrCorrupt。
func (s *FileStore) readArchiveSegment(scope, id string, seg ArchiveSegment) ([]Message, error) {
	path := filepath.Join(s.archiveDir(scope, id), seg.File)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: missing segment %s", ErrCorrupt, path)
	}
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != seg.Bytes {
		return nil, fmt.Errorf("%w: segment %s bytes %d want %d", ErrCorrupt, path, len(data), seg.Bytes)
	}
	if sha256Hex(data) != seg.SHA256 {
		return nil, fmt.Errorf("%w: segment %s sha256 mismatch", ErrCorrupt, path)
	}
	msgs, err := decodeJSONL[Message](path, data)
	if err != nil {
		return nil, err
	}
	if len(msgs) != seg.Count {
		return nil, fmt.Errorf("%w: segment %s count %d want %d", ErrCorrupt, path, len(msgs), seg.Count)
	}
	for i, m := range msgs {
		if m.Seq < seg.StartSeq || m.Seq > seg.EndSeq {
			return nil, fmt.Errorf("%w: segment %s seq %d outside %d-%d", ErrCorrupt, path, m.Seq, seg.StartSeq, seg.EndSeq)
		}
		if i > 0 && m.Seq <= msgs[i-1].Seq {
			return nil, fmt.Errorf("%w: segment %s seq not increasing at line %d", ErrCorrupt, path, i+1)
		}
	}
	return msgs, nil
}

// readAllArchived 按段升序读取全部消息；用于搜索/去重/未读（这些路径本来就要全量扫）。
func (s *FileStore) readAllArchived(scope, id string) ([]Message, error) {
	m, err := s.readArchiveManifest(scope, id)
	if err != nil {
		return nil, err
	}
	var out []Message
	for _, seg := range m.Segments {
		msgs, err := s.readArchiveSegment(scope, id, seg)
		if err != nil {
			return nil, err
		}
		out = append(out, msgs...)
	}
	return out, nil
}

// mergedMessages 合并热+冷：seq 升序、同 seq 去重（热优先）。
func mergedMessages(hot, cold []Message) []Message {
	if len(hot) == 0 && len(cold) == 0 {
		return nil
	}
	bySeq := make(map[int64]Message, len(hot)+len(cold))
	for _, m := range cold {
		bySeq[m.Seq] = m
	}
	for _, m := range hot {
		bySeq[m.Seq] = m
	}
	seqs := make([]int64, 0, len(bySeq))
	for seq := range bySeq {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	out := make([]Message, 0, len(seqs))
	for _, seq := range seqs {
		out = append(out, bySeq[seq])
	}
	return out
}

// mergedLogMessages 读取热（调用方已读）+ 全部冷段并合并。
func (s *FileStore) mergedLogMessages(scope, id string, hot []Message) ([]Message, error) {
	cold, err := s.readAllArchived(scope, id)
	if err != nil {
		return nil, err
	}
	return mergedMessages(hot, cold), nil
}

// pageMessages 从热+冷取「seq < beforeSeq 的最新 limit 条」，返回旧→新。
//
// 不加载全部段：热日志先进入候选（最新），再按 manifest 从新到旧读；一旦已有
// limit 条候选比下一个（更旧）段的最大 seq 还新，就可以停止。热日志里的重复
// seq（崩溃窗口）覆盖冷段同 seq 的副本（热优先）。
func (s *FileStore) pageMessages(scope, id string, hot []Message, beforeSeq int64, limit int) ([]Message, error) {
	if limit <= 0 {
		return nil, nil
	}
	m, err := s.readArchiveManifest(scope, id)
	if err != nil {
		return nil, err
	}
	candidates := make(map[int64]Message, len(hot))
	for _, msg := range hot {
		if beforeSeq > 0 && msg.Seq >= beforeSeq {
			continue
		}
		candidates[msg.Seq] = msg
	}
	enough := func(bound int64) bool {
		if len(candidates) < limit {
			return false
		}
		n := 0
		for seq := range candidates {
			if seq > bound {
				n++
				if n >= limit {
					return true
				}
			}
		}
		return false
	}
	for i := len(m.Segments) - 1; i >= 0; i-- {
		seg := m.Segments[i]
		if enough(seg.EndSeq) {
			break // 剩余（更旧）段不可能进入最新 limit
		}
		if beforeSeq > 0 && seg.StartSeq >= beforeSeq {
			continue // 整段都 >= beforeSeq
		}
		msgs, err := s.readArchiveSegment(scope, id, seg)
		if err != nil {
			return nil, err
		}
		for _, msg := range msgs {
			if beforeSeq > 0 && msg.Seq >= beforeSeq {
				continue
			}
			if _, ok := candidates[msg.Seq]; ok {
				continue // 热日志已有同 seq，热优先
			}
			candidates[msg.Seq] = msg
		}
	}
	seqs := make([]int64, 0, len(candidates))
	for seq := range candidates {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] > seqs[j] })
	if len(seqs) > limit {
		seqs = seqs[:limit]
	}
	out := make([]Message, 0, len(seqs))
	for i := len(seqs) - 1; i >= 0; i-- {
		out = append(out, candidates[seqs[i]])
	}
	return out, nil
}

// findArchivedMessageByClientID 在冷段里从新到旧找 client_msg_id（幂等重发用）。
// trim 与调用方热日志的匹配口径保持一致（群 trim，DM 精确匹配）。
func (s *FileStore) findArchivedMessageByClientID(scope, id, clientMsgID string, trim bool) (*Message, error) {
	m, err := s.readArchiveManifest(scope, id)
	if err != nil {
		return nil, err
	}
	match := func(m Message) bool {
		got := messageClientMsgID(m)
		if trim {
			got = strings.TrimSpace(got)
		}
		return got == clientMsgID
	}
	for i := len(m.Segments) - 1; i >= 0; i-- {
		msgs, err := s.readArchiveSegment(scope, id, m.Segments[i])
		if err != nil {
			return nil, err
		}
		for j := len(msgs) - 1; j >= 0; j-- {
			if match(msgs[j]) {
				msg := msgs[j]
				return &msg, nil
			}
		}
	}
	return nil, nil
}

// hasArchivedRoundReceipt 在冷段里查触发消息是否已有回执（幂等重发补齐回执前先查，
// 避免原回执也随触发消息一起归档时重复写）。
func (s *FileStore) hasArchivedRoundReceipt(gid GroupID, triggerSeq int64) (bool, error) {
	m, err := s.readArchiveManifest(ArchiveScopeGroups, string(gid))
	if err != nil {
		return false, err
	}
	for i := len(m.Segments) - 1; i >= 0; i-- {
		seg := m.Segments[i]
		if seg.EndSeq < triggerSeq {
			continue // 回执 seq > triggerSeq，不可能在这个更旧的段
		}
		msgs, err := s.readArchiveSegment(ArchiveScopeGroups, string(gid), seg)
		if err != nil {
			return false, err
		}
		if hasRoundReceipt(msgs, triggerSeq) {
			return true, nil
		}
	}
	return false, nil
}

func encodeMessages(msgs []Message) ([]byte, error) {
	var buf bytes.Buffer
	for i := range msgs {
		data, err := json.Marshal(&msgs[i])
		if err != nil {
			return nil, fmt.Errorf("marshal archive message: %w", err)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

func decodeJSONL[T any](path string, data []byte) ([]T, error) {
	var out []T
	for i, line := range bytes.Split(data, []byte{'\n'}) {
		raw := bytes.TrimSpace(line)
		if len(raw) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("%w: %s:%d: %v", ErrCorrupt, path, i+1, err)
		}
		out = append(out, v)
	}
	return out, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeArchiveSegment 写不可变段：临时文件 → fsync → rename → fsync 目录 → chmod 0400。
// 目标已存在且逐字节相同则复用（崩溃留下的孤儿段）；不同则 ErrConflict，绝不覆盖。
func (s *FileStore) writeArchiveSegment(scope, id string, seg ArchiveSegment, data []byte) (bool, error) {
	dir := s.archiveDir(scope, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	target := filepath.Join(dir, seg.File)
	existing, err := os.ReadFile(target)
	switch {
	case err == nil:
		if !bytes.Equal(existing, data) {
			return false, fmt.Errorf("%w: archive segment %s exists with different content", ErrConflict, target)
		}
		if err := os.Chmod(target, 0o400); err != nil {
			return false, err
		}
		return true, nil
	case errors.Is(err, os.ErrNotExist):
	default:
		return false, err
	}

	tmp, err := os.CreateTemp(dir, ".seg-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return false, err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return false, err
	}
	if err := os.Rename(tmpName, target); err != nil {
		os.Remove(tmpName)
		return false, err
	}
	syncDir(dir)
	if err := os.Chmod(target, 0o400); err != nil {
		return false, err
	}
	return false, nil
}

// rewriteHotLog 原子重写热日志（保留未归档消息，包括归档期间新 append 的）。
func rewriteHotLog(path string, msgs []Message) (int64, error) {
	data, err := encodeMessages(msgs)
	if err != nil {
		return 0, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(dir, ".hot-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return 0, err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return 0, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return 0, err
	}
	syncDir(dir)
	return int64(len(data)), nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// archivedSeqSet 只读取与 [minSeq,maxSeq] 相交的段，返回已归档 seq 集合。
// 段允许有空洞，所以不能用 range 直接判断「已归档」。
func (s *FileStore) archivedSeqSet(scope, id string, m *ArchiveManifest, minSeq, maxSeq int64) (map[int64]struct{}, error) {
	set := make(map[int64]struct{})
	if len(m.Segments) == 0 {
		return set, nil
	}
	for _, seg := range m.Segments {
		if seg.EndSeq < minSeq || seg.StartSeq > maxSeq {
			continue
		}
		msgs, err := s.readArchiveSegment(scope, id, seg)
		if err != nil {
			return nil, err
		}
		for _, msg := range msgs {
			set[msg.Seq] = struct{}{}
		}
	}
	return set, nil
}

// pruneArchived 去掉热日志里已经进入 manifest 段的 seq（修复 manifest 已提交、
// 热日志尚未裁剪的崩溃窗口）。保留原顺序。
func (s *FileStore) pruneArchived(scope, id string, hot []Message, m *ArchiveManifest) ([]Message, int, error) {
	if len(hot) == 0 || len(m.Segments) == 0 {
		return hot, 0, nil
	}
	minSeq, maxSeq := hot[0].Seq, hot[0].Seq
	for _, msg := range hot[1:] {
		if msg.Seq < minSeq {
			minSeq = msg.Seq
		}
		if msg.Seq > maxSeq {
			maxSeq = msg.Seq
		}
	}
	set, err := s.archivedSeqSet(scope, id, m, minSeq, maxSeq)
	if err != nil {
		return nil, 0, err
	}
	if len(set) == 0 {
		return hot, 0, nil
	}
	kept := make([]Message, 0, len(hot))
	pruned := 0
	for _, msg := range hot {
		if _, ok := set[msg.Seq]; ok {
			pruned++
			continue
		}
		kept = append(kept, msg)
	}
	return kept, pruned, nil
}

func shouldArchive(count int, bytes int64, p ArchivePolicy) (bool, string) {
	if count > p.MaxMessages {
		return true, "max_messages"
	}
	if bytes > p.MaxBytes {
		// 体积触发不能为降体积破坏 KeepRecent 保留承诺。
		if count <= p.KeepRecent {
			return false, "keep_recent"
		}
		return true, "max_bytes"
	}
	return false, "below_threshold"
}

func archiveCount(count int, p ArchivePolicy) int {
	n := count * p.Percent / 100
	if max := count - p.KeepRecent; n > max {
		n = max
	}
	if n < 0 {
		n = 0
	}
	return n
}

// sortMessagesBySeq 把快照按 seq 稳定排序：热日志正常是递增的，但崩溃修复/
// 手工导入可能让旧的重复 seq 排在后面；归档段必须按 seq 升序才能通过读侧校验。
func sortMessagesBySeq(msgs []Message) {
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].Seq < msgs[j].Seq })
}

// ArchiveGroup 归档一个群的热日志。policy 零值 = DefaultArchivePolicy()。
func (s *FileStore) ArchiveGroup(ctx context.Context, gid GroupID, policy ArchivePolicy) (ArchiveResult, error) {
	if err := ctx.Err(); err != nil {
		return ArchiveResult{}, err
	}
	if !validID(string(gid)) {
		return ArchiveResult{}, fmt.Errorf("%w: group id %q", ErrInvalid, gid)
	}
	return s.archiveScope(ctx, ArchiveScopeGroups, string(gid), policy)
}

// ArchiveDM 归档一个智能体 DM 索引的热日志。
func (s *FileStore) ArchiveDM(ctx context.Context, agentID AgentID, policy ArchivePolicy) (ArchiveResult, error) {
	if err := ctx.Err(); err != nil {
		return ArchiveResult{}, err
	}
	if !validID(string(agentID)) {
		return ArchiveResult{}, fmt.Errorf("%w: agent id %q", ErrInvalid, agentID)
	}
	return s.archiveScope(ctx, ArchiveScopeDMs, string(agentID), policy)
}

// ArchiveIfNeeded 用默认策略扫未删除群 + 智能体 DM；单会话失败不阻塞其它会话。
func (s *FileStore) ArchiveIfNeeded(ctx context.Context) ([]ArchiveResult, error) {
	return s.archiveIfNeeded(ctx, ArchivePolicy{})
}

func (s *FileStore) archiveIfNeeded(ctx context.Context, policy ArchivePolicy) ([]ArchiveResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []ArchiveResult
	var errs []error
	groups, err := s.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if g == nil || g.Deleted {
			continue
		}
		res, err := s.ArchiveGroup(ctx, g.ID, policy)
		if err != nil {
			errs = append(errs, fmt.Errorf("archive group %s: %w", g.ID, err))
			continue
		}
		out = append(out, res)
	}
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return out, errors.Join(append(errs, err)...)
	}
	for _, p := range agents {
		if p == nil || p.Deleted {
			continue
		}
		res, err := s.ArchiveDM(ctx, p.ID, policy)
		if err != nil {
			errs = append(errs, fmt.Errorf("archive dm %s: %w", p.ID, err))
			continue
		}
		out = append(out, res)
	}
	return out, errors.Join(errs...)
}

func (s *FileStore) archiveScope(ctx context.Context, scope, id string, policy ArchivePolicy) (ArchiveResult, error) {
	res := ArchiveResult{Scope: scope, ID: id}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if err := validateArchiveTarget(scope, id); err != nil {
		return res, err
	}
	pol := policy.withDefaults()
	hotPath := s.archiveHotLogPath(scope, id)

	// 归档串行化：同一时刻只有一个归档在决定段边界/写 manifest。
	s.archiveMu.Lock()
	defer s.archiveMu.Unlock()

	// 阶段 1：在 s.mu 下快照热日志，先剔除已经归档的 seq，再判断阈值。
	s.mu.Lock()
	hot, err := readJSONLTolerantTail[Message](hotPath)
	if err != nil {
		s.mu.Unlock()
		return res, err
	}
	manifest, err := s.readArchiveManifest(scope, id)
	if err != nil {
		s.mu.Unlock()
		return res, err
	}
	fileBytes, err := regularFileSize(hotPath)
	if err != nil {
		s.mu.Unlock()
		return res, err
	}
	live, pruned, err := s.pruneArchived(scope, id, hot, manifest)
	if err != nil {
		s.mu.Unlock()
		return res, err
	}
	sortMessagesBySeq(live)
	hotBytes := fileBytes
	if pruned > 0 {
		data, err := encodeMessages(live)
		if err != nil {
			s.mu.Unlock()
			return res, err
		}
		hotBytes = int64(len(data))
	}
	trigger, reason := shouldArchive(len(live), hotBytes, pol)
	count := 0
	if trigger {
		count = archiveCount(len(live), pol)
		if count <= 0 {
			trigger = false
			reason = "keep_recent"
		}
	}
	var batch []Message
	var seg ArchiveSegment
	var segData []byte
	if trigger {
		batch = append([]Message(nil), live[:count]...)
		segData, err = encodeMessages(batch)
		if err != nil {
			s.mu.Unlock()
			return res, err
		}
		seg = ArchiveSegment{
			StartSeq:   batch[0].Seq,
			EndSeq:     batch[len(batch)-1].Seq,
			Count:      len(batch),
			Bytes:      int64(len(segData)),
			SHA256:     sha256Hex(segData),
			File:       archiveSegmentFile(batch[0].Seq, batch[len(batch)-1].Seq),
			ArchivedAt: time.Now().UTC(),
		}
	}
	s.mu.Unlock()

	if !trigger {
		res.Skipped = true
		res.Reason = reason
		res.HotRemaining = len(live)
		res.HotBytes = hotBytes
		if pruned > 0 {
			// 已归档但热未裁剪：不写新段也要把热日志修好，否则重复记录会一直留着。
			repaired, remaining, bytes, err := s.repairHotLog(scope, id, hotPath)
			if err != nil {
				return res, err
			}
			res.Repaired = repaired
			res.HotRemaining = remaining
			res.HotBytes = bytes
		}
		return res, nil
	}

	// 阶段 2：不持 s.mu 写段（慢 I/O 不阻塞 append）。
	_, err = s.writeArchiveSegment(scope, id, seg, segData)
	if err != nil {
		return res, err
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	// 阶段 3：重新持锁提交 manifest，再裁剪热日志。
	// 这之间新 append 的消息在 hot2 里，按 manifest 过滤后原样保留。
	s.mu.Lock()
	defer s.mu.Unlock()

	manifest, err = s.readArchiveManifest(scope, id)
	if err != nil {
		return res, err
	}
	already, err := segmentAlreadyCommitted(manifest, seg)
	if err != nil {
		return res, err
	}
	if !already {
		manifest.Segments = append(manifest.Segments, seg)
		sort.Slice(manifest.Segments, func(i, j int) bool {
			return manifest.Segments[i].StartSeq < manifest.Segments[j].StartSeq
		})
		manifest.UpdatedAt = time.Now().UTC()
		if err := validateArchiveManifest(manifest, scope, id); err != nil {
			return res, err
		}
		if err := writeJSONAtomic(s.archiveManifestPath(scope, id), manifest); err != nil {
			return res, err
		}
	}

	hot2, err := readJSONLTolerantTail[Message](hotPath)
	if err != nil {
		return res, err
	}
	remaining, _, err := s.pruneArchived(scope, id, hot2, manifest)
	if err != nil {
		return res, err
	}
	sortMessagesBySeq(remaining)
	bytes, err := rewriteHotLog(hotPath, remaining)
	if err != nil {
		// manifest 已提交：热日志原文仍在，只是重复；下一次归档会修复。
		return res, fmt.Errorf("archive manifest committed but hot prune failed: %w", err)
	}
	s.advanceSeqLocked(scope, id, manifest, remaining)
	res.Archived = len(batch)
	res.Segment = &seg
	res.Repaired = pruned > 0
	res.HotRemaining = len(remaining)
	res.HotBytes = bytes
	return res, nil
}

// segmentAlreadyCommitted 校验新段与已有段不重叠；完全相同视为幂等（崩溃后重放）。
func segmentAlreadyCommitted(m *ArchiveManifest, seg ArchiveSegment) (bool, error) {
	for _, old := range m.Segments {
		if old.StartSeq == seg.StartSeq && old.EndSeq == seg.EndSeq {
			if old.File != seg.File || old.SHA256 != seg.SHA256 || old.Bytes != seg.Bytes || old.Count != seg.Count {
				return false, fmt.Errorf("%w: segment %s already committed with different metadata", ErrConflict, seg.File)
			}
			return true, nil
		}
		if seg.StartSeq <= old.EndSeq && old.StartSeq <= seg.EndSeq {
			return false, fmt.Errorf("%w: segment %d-%d overlaps %d-%d", ErrConflict, seg.StartSeq, seg.EndSeq, old.StartSeq, old.EndSeq)
		}
	}
	return false, nil
}

// repairHotLog 在不写新段的情况下，把热日志里已有的归档消息去掉（崩溃窗口修复）。
func (s *FileStore) repairHotLog(scope, id, hotPath string) (bool, int, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hot, err := readJSONLTolerantTail[Message](hotPath)
	if err != nil {
		return false, 0, 0, err
	}
	manifest, err := s.readArchiveManifest(scope, id)
	if err != nil {
		return false, 0, 0, err
	}
	remaining, pruned, err := s.pruneArchived(scope, id, hot, manifest)
	if err != nil {
		return false, 0, 0, err
	}
	if pruned == 0 {
		bytes, err := regularFileSize(hotPath)
		return false, len(remaining), bytes, err
	}
	sortMessagesBySeq(remaining)
	bytes, err := rewriteHotLog(hotPath, remaining)
	if err != nil {
		return false, 0, 0, err
	}
	s.advanceSeqLocked(scope, id, manifest, remaining)
	return true, len(remaining), bytes, nil
}

// advanceSeqLocked 把内存 seq 前移到 max(热最大 seq, manifest 最大 end_seq)。
// 只前进不后退：全量归档后重启也不能从 1 重号。
func (s *FileStore) advanceSeqLocked(scope, id string, m *ArchiveManifest, hot []Message) {
	var max int64
	for _, seg := range m.Segments {
		if seg.EndSeq > max {
			max = seg.EndSeq
		}
	}
	for _, msg := range hot {
		if msg.Seq > max {
			max = msg.Seq
		}
	}
	switch scope {
	case ArchiveScopeGroups:
		gid := GroupID(id)
		if max > s.seq[gid] {
			s.seq[gid] = max
		}
	case ArchiveScopeDMs:
		aid := AgentID(id)
		if max > s.dmSeq[aid] {
			s.dmSeq[aid] = max
		}
	}
}

// rebuildArchiveIndex 在 OpenFileStore 时校验所有 manifest，并把冷段最大 end_seq
// 并入 seq 索引（热日志为空/完全归档后仍从 max+1 分配）。
func (s *FileStore) rebuildArchiveIndex() error {
	for _, scope := range []string{ArchiveScopeGroups, ArchiveScopeDMs} {
		matches, err := filepath.Glob(filepath.Join(s.root, "archive", scope, "*", "manifest.json"))
		if err != nil {
			return err
		}
		for _, path := range matches {
			id := filepath.Base(filepath.Dir(path))
			if !validID(id) {
				return fmt.Errorf("%w: archive id %q", ErrCorrupt, id)
			}
			m, err := s.readArchiveManifest(scope, id)
			if err != nil {
				return err
			}
			for _, seg := range m.Segments {
				segPath := filepath.Join(s.archiveDir(scope, id), seg.File)
				fi, err := os.Stat(segPath)
				if errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("%w: missing segment %s", ErrCorrupt, segPath)
				}
				if err != nil {
					return err
				}
				if fi.Size() != seg.Bytes {
					return fmt.Errorf("%w: segment %s size %d want %d", ErrCorrupt, segPath, fi.Size(), seg.Bytes)
				}
			}
			s.advanceSeqLocked(scope, id, m, nil)
		}
	}
	return nil
}
