package session

// block 索引文件：让「打开会话」不必扫描整个 sidecar。
//
// ## 为什么需要它（实测 2026-10-20，真实 1029 MB sidecar）
//
// openBlockStore 原先要逐条解析 sidecar 才能知道「每个 id 的最新 payload 在哪」：
//   · 纯遍历 1029MB                                 25 ms
//   · 逐条 payload 分配拷贝                          239 ms
//   · 逐条 payload sha256 校验                       598 ms
//   ──────────────────────────────────────────────────────
//   合计 ≈ 862 ms，`LoadRecord` 实测 1798 ms。
//
// 而会话里 93% 的记录是废弃的旧版本，永远读不到。为它们付校验与拷贝成本，
// 是切换会话慢的直接原因。
//
// ## 设计
//
// 索引文件 `<sid>.record.json.blocks.idx` 记录「每个 id 最新版本的位置」：
//
//	magic(8) | count(4) | covered(8) | entries(count × 32B) | digest(8)
//	entry = id(8) | offset(8) | length(8) | payloadDigest(8)
//
// 头部另存 `chain(8)`：写入侧滚动校验链在 covered 处的**截断摘要**。
// 有了它，"索引完整覆盖"时连哈希都不用重算（O(1) 打开）。
//
// 关键点 `covered`：索引只覆盖 sidecar 的**前 covered 字节**。
// 打开时若 sidecar 比 covered 长（追加了新记录），只扫描 [covered, EOF) 这段
// 增量并合并进索引 —— O(增量) 而非 O(总量)。
//
// 实测（21632 块）：索引 692 KB，读取 + 解析 0.56 ms；
// 配合分页后「切换会话」只需读 索引 + 首屏，约 0.8 ms。
//
// ## 一致性
//
// - 索引自带 entries 的 sha256 → 检出损坏；
// - `covered` 与 sidecar 实际大小比对 → 检出「索引落后」（走增量）或
//   「索引超前」（说明被 compact/截断过 → 全量重建）；
// - 任何不符都只是**回退到扫描重建**，不会返回错误数据。
//
// 索引是纯加速结构：删掉它只会变慢，不会丢数据。

import (
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
)

const (
	indexSuffix = ".idx"
	indexMagic  = "HBIDX001"

	// indexEntrySize = id(8) + offset(8) + length(8) + payloadDigest(8)
	indexEntrySize = 32
	// indexHeadSize = magic(8) + count(4) + covered(8) + chainFull(32)
	//               + records(8) + stale(8)
	//
	// records/stale 让「索引命中」打开后仍能判断是否需要 compact：
	// 否则 bs.staleRecords 永远是 0，maybeCompact 永不触发，
	// 文件会像之前那样无限膨胀（实测 838MB→1378MB）。
	indexHeadSize = 68
	indexTailSize = 8
)

// blockIndexPath 由 sidecar 路径派生索引路径。
func blockIndexPath(storePath string) string { return storePath + indexSuffix }

// indexEntry 是索引里的一条（内存形态与磁盘形态一致）。
type indexEntry struct {
	id     uint64
	off    int64
	n      int64
	digest [8]byte
}

// encodeBlockIndex 把索引序列化成磁盘格式。
func encodeBlockIndex(entries []indexEntry, covered int64, chain []byte, records, stale int64) []byte {
	buf := make([]byte, 0, indexHeadSize+len(entries)*indexEntrySize+indexTailSize)
	buf = append(buf, indexMagic...)

	var n4 [4]byte
	binary.BigEndian.PutUint32(n4[:], uint32(len(entries)))
	buf = append(buf, n4[:]...)

	var n8 [8]byte
	binary.BigEndian.PutUint64(n8[:], uint64(covered))
	buf = append(buf, n8[:]...)
	// chain：该 covered 处的**完整 32 字节**滚动链摘要。
	//
	// 为什么存全量而不是截断：sum() 是 record 里校验 sidecar 完整性的唯一凭据，
	// 用截断值（8B=64bit）会弱化校验强度。存 32 字节只多 24 字节/文件。
	cp := make([]byte, 32)
	copy(cp, chain)
	buf = append(buf, cp...)
	binary.BigEndian.PutUint64(n8[:], uint64(records))
	buf = append(buf, n8[:]...)
	binary.BigEndian.PutUint64(n8[:], uint64(stale))
	buf = append(buf, n8[:]...)

	for _, e := range entries {
		binary.BigEndian.PutUint64(n8[:], e.id)
		buf = append(buf, n8[:]...)
		binary.BigEndian.PutUint64(n8[:], uint64(e.off))
		buf = append(buf, n8[:]...)
		binary.BigEndian.PutUint64(n8[:], uint64(e.n))
		buf = append(buf, n8[:]...)
		buf = append(buf, e.digest[:]...)
	}

	sum := sha256.Sum256(buf[indexHeadSize:])
	buf = append(buf, sum[:indexTailSize]...)
	return buf
}

// decodeBlockIndex 解析索引；任何异常都返回 ok=false（调用方回退扫描重建）。
func decodeBlockIndex(b []byte) (entries []indexEntry, covered int64, chain []byte, records, stale int64, ok bool) {
	if len(b) < indexHeadSize+indexTailSize {
		return nil, 0, nil, 0, 0, false
	}
	if string(b[:8]) != indexMagic {
		return nil, 0, nil, 0, 0, false
	}
	count := int(binary.BigEndian.Uint32(b[8:12]))
	covered = int64(binary.BigEndian.Uint64(b[12:20]))
	chain = append([]byte(nil), b[20:52]...)
	records = int64(binary.BigEndian.Uint64(b[52:60]))
	stale = int64(binary.BigEndian.Uint64(b[60:68]))
	if count < 0 || indexHeadSize+count*indexEntrySize+indexTailSize != len(b) {
		return nil, 0, nil, 0, 0, false
	}
	// 校验 entries 的摘要（一次哈希 692KB ≈ 0.1ms，远低于逐块校验）
	body := b[indexHeadSize : indexHeadSize+count*indexEntrySize]
	sum := sha256.Sum256(body)
	if !equalBytes(b[len(b)-indexTailSize:], sum[:indexTailSize]) {
		return nil, 0, nil, 0, 0, false
	}
	entries = make([]indexEntry, count)
	p := body
	for i := 0; i < count; i++ {
		entries[i].id = binary.BigEndian.Uint64(p[0:8])
		entries[i].off = int64(binary.BigEndian.Uint64(p[8:16]))
		entries[i].n = int64(binary.BigEndian.Uint64(p[16:24]))
		copy(entries[i].digest[:], p[24:32])
		p = p[32:]
	}
	return entries, covered, chain, records, stale, true
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// writeBlockIndex 原子写入索引文件（临时文件 + fsync + rename）。
//
// 索引是加速结构，写失败不影响正确性（下次打开会扫描重建），
// 所以这里的错误只做上报，不中断提交。
func writeBlockIndex(storePath string, entries []indexEntry, covered int64, chain []byte, records, stale int64) error {
	data := encodeBlockIndex(entries, covered, chain, records, stale)
	path := blockIndexPath(storePath)
	tmp := path + ".tmp"

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// loadBlockIndex 读取并校验索引文件。
// 不存在 / 损坏 / 版本不符 → ok=false（调用方走扫描重建）。
func loadBlockIndex(storePath string) (entries []indexEntry, covered int64, chain []byte, records, stale int64, ok bool) {
	b, err := os.ReadFile(blockIndexPath(storePath))
	if err != nil {
		return nil, 0, nil, 0, 0, false
	}
	return decodeBlockIndex(b)
}

// indexToMap 把索引条目转成运行时索引（id → 位置）。
func indexToMap(entries []indexEntry) map[string]blockRef {
	m := make(map[string]blockRef, len(entries))
	for _, e := range entries {
		m[uintToString(e.id)] = blockRef{off: e.off, n: int32(e.n), hash: e.digest}
	}
	return m
}

// mapToIndex 把运行时索引转成磁盘条目（按 id 升序，便于增量比对）。
func mapToIndex(m map[string]blockRef) []indexEntry {
	out := make([]indexEntry, 0, len(m))
	for id, ref := range m {
		v, err := parseUintID(id)
		if err != nil {
			continue
		}
		out = append(out, indexEntry{id: v, off: ref.off, n: int64(ref.n), digest: ref.hash})
	}
	sortIndexEntries(out)
	return out
}

// sortIndexEntries 按 id 升序排序（插入排序在生产中足够：id 本就近有序）。
func sortIndexEntries(e []indexEntry) {
	// 不能用插入排序：输入来自 map 迭代（完全无序），O(n²) 在 21700 项上
	// 实测 110ms —— 持久化索引时每次都付这个代价（2026-10-20）。
	slices.SortFunc(e, func(a, b indexEntry) int { return cmp.Compare(a.id, b.id) })
}

func uintToString(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

func parseUintID(s string) (uint64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty id")
	}
	var v uint64
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("non-numeric id %q", s)
		}
		v = v*10 + uint64(s[i]-'0')
	}
	return v, nil
}

// indexDigestHex 供诊断/日志使用。
func indexDigestHex(entries []indexEntry) string {
	h := sha256.New()
	var n8 [8]byte
	for _, e := range entries {
		binary.BigEndian.PutUint64(n8[:], e.id)
		h.Write(n8[:])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
