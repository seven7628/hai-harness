package session

// sidecar 的**片段存储**：让「只增」的块以片段形式落盘，消灭写放大。
//
// ## 问题（2026-11-11 实测）
//
// sidecar 是 append-only 的「一个 id → 一个 payload（最后写的赢）」。块内容一变，
// **整个 payload** 被重新追加一遍：
//
//	追加字节 = Σ(每个版本的大小)     ← 不是增量
//	放大倍数 = 块大小 ÷ 每次增量
//
// 真实会话 `s-1792132279027217000-3`：37,430 个块 / live 208MB / sidecar 245MB；
// `s-1792132279027217000-21`：30,564 个块 / live 84MB / sidecar **1,196MB**
// —— 1,111MB 是垃圾，**99.95% 来自 242 个 agent 卡片块**（每个被写 100–290 次，
// 单次最大版本 0.6–1.2MB）。
//
// ## 解法
//
// 块 payload 里最长的**顶层数组**（agent 卡片的 items）被切成片段：
//
//	前缀（含 `"items":[`）│ 元素0 │ 元素1 │ … │ 后缀（含 `],"items_trimmed":N`）
//
// 拼接结果与原字节**逐字节相同** → 上层（reducer / bridge / 前端）零改动，
// 摘要校验也照常成立。
//
// 只增的块（running 卡片）每次只写**新元素那一片** + 变化的前缀 →
// 追加量 ≈ 增量 → 写放大从 150–275× 降到个位数。
//
// ## 磁盘格式（附加式，老文件原样可读）
//
//	kind=1 (put)  kind(1)│idLen(4)│id│payloadLen(4)│payload│digest(8)   ← 整块（旧）
//	kind=2 (del)  kind(1)│idLen(4)│id│digest(8)                          ← 删除（旧）
//	kind=3 (frag) kind(1)│idLen(4)│id│key(4)│payloadLen(4)│payload│digest(8)
//	kind=4 (dir)  kind(1)│idLen(4)│id│dirLen(4)│dir│digest(8)
//
// **索引格式完全不变**：分片块的索引项指向**目录记录**（kind=4），
// 目录里列出各片段的 (key, off, len, digest) 与逻辑摘要/总长。
// 索引项的 kind 字节最高位（0x80）标记「这是分片块」，低 7 位是真实块类型。
//
// ## 关键不变量
//
//  1. 拼接顺序 = key 升序；拼接结果与写入时的 payload 逐字节相同；
//  2. 索引项的 hash = **逻辑摘要**（拼接内容的摘要）→ `holdsSame` /
//     `indexSetSum` 的口径不变；
//  3. 一个 id 要么整块、要么分片，二者互斥：kind=1 清空分片，kind=4 建立分片。

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// 片段相关的记录类型。
const (
	blockRecFrag byte = 3 // 片段内容
	blockRecDir  byte = 4 // 片段目录
)

// kindFragmented 是 kind 字节的高位标志：置位表示「该 id 以片段存储」。
// 低 7 位是真实块类型（现有取值 0..11，不会撞）。
const kindFragmented uint8 = 0x80

// fragSplitMinBytes 是「数组内容多小才值得切」的下限。
//
// 太小的数组切了反而碎片化：每个片段都要占一条目录项 + 一次读。
// 实测 agent 卡片：items 数组通常 50KB–1.5MB；tool / assistant 卡片没有长数组
// （或数组很短）→ 不切，行为与今天完全一致。
const fragSplitMinBytes = 8 << 10

// fragDir 是一个 id 的片段目录。
type fragDir struct {
	kind    uint8      // 真实块类型（未带 kindFragmented 标志）
	total   int64      // 逻辑总长（拼接后的字节数）
	logical [8]byte    // 逻辑摘要（拼接内容的摘要）
	items   []fragItem // 片段（按 key 升序）
}

// fragItemSize 是目录里**一条片段项**的字节数：key(4)+off(8)+len(4)+digest(8)。
//
// （2026-11-11 修：编码按 24 字节写，校验却按 28 算 → parseFragDir 永远失败，
//
//	只在「索引命中后重新读目录」这条路径上暴露 —— 直读路径命中内存缓存，测不出来。）
const fragItemSize = 24

// fragItem 是一个片段。
type fragItem struct {
	key  uint32
	off  int64 // payload 在 sidecar 里的位置
	n    int32
	hash [8]byte
}

// isFragmented 报告 kind 字节是否带分片标志。
func isFragmented(k uint8) bool { return k&kindFragmented != 0 }

// realKind 取掉标志位后的真实块类型（分片 0x80 / 压缩 0x40）。
func realKind(k uint8) uint8 { return k &^ (kindFragmented | kindCompressed) }

// isCompressed 报告索引项的 kind 字节是否带「压缩」标志。
func isCompressed(k uint8) bool { return k&kindCompressed != 0 }

// —— 目录记录的编解码 ——

// encodeFragDir 编码目录 payload。
//
// 布局：kind(1) │ count(4) │ total(8) │ logical(8) │ 项×count
// 项：key(4) │ off(8) │ len(8) │ digest(8)
func encodeFragDir(d *fragDir) []byte {
	buf := make([]byte, 0, 21+len(d.items)*fragItemSize)
	buf = append(buf, d.kind)
	var n4 [4]byte
	binary.BigEndian.PutUint32(n4[:], uint32(len(d.items)))
	buf = append(buf, n4[:]...)
	var n8 [8]byte
	binary.BigEndian.PutUint64(n8[:], uint64(d.total))
	buf = append(buf, n8[:]...)
	buf = append(buf, d.logical[:]...)
	for _, it := range d.items {
		binary.BigEndian.PutUint32(n4[:], it.key)
		buf = append(buf, n4[:]...)
		binary.BigEndian.PutUint64(n8[:], uint64(it.off))
		buf = append(buf, n8[:]...)
		binary.BigEndian.PutUint32(n4[:], uint32(it.n))
		buf = append(buf, n4[:]...)
		buf = append(buf, it.hash[:]...)
	}
	return buf
}

// parseFragDir 解析目录 payload。
func parseFragDir(b []byte) (*fragDir, bool) {
	if len(b) < 21 {
		return nil, false
	}
	d := &fragDir{kind: b[0]}
	count := int(binary.BigEndian.Uint32(b[1:5]))
	d.total = int64(binary.BigEndian.Uint64(b[5:13]))
	copy(d.logical[:], b[13:21])
	if count < 0 || 21+count*fragItemSize != len(b) {
		return nil, false
	}
	d.items = make([]fragItem, count)
	for i := 0; i < count; i++ {
		o := 21 + i*fragItemSize
		d.items[i].key = binary.BigEndian.Uint32(b[o : o+4])
		d.items[i].off = int64(binary.BigEndian.Uint64(b[o+4 : o+12]))
		d.items[i].n = int32(binary.BigEndian.Uint32(b[o+12 : o+16]))
		copy(d.items[i].hash[:], b[o+16:o+24])
	}
	return d, true
}

// buildFragRecordHead 构造 kind=3（片段内容）的记录头。
func buildFragRecordHead(id string, key uint32, payload []byte) []byte {
	b := make([]byte, 0, 1+4+len(id)+4+4)
	b = append(b, blockRecFrag)
	var n4 [4]byte
	binary.BigEndian.PutUint32(n4[:], uint32(len(id)))
	b = append(b, n4[:]...)
	b = append(b, id...)
	binary.BigEndian.PutUint32(n4[:], key)
	b = append(b, n4[:]...)
	binary.BigEndian.PutUint32(n4[:], uint32(len(payload)))
	b = append(b, n4[:]...)
	return b
}

// buildDirRecordHead 构造 kind=4（目录）的记录头。
func buildDirRecordHead(id string, dir []byte) []byte {
	b := make([]byte, 0, 1+4+len(id)+4)
	b = append(b, blockRecDir)
	var n4 [4]byte
	binary.BigEndian.PutUint32(n4[:], uint32(len(id)))
	b = append(b, n4[:]...)
	b = append(b, id...)
	binary.BigEndian.PutUint32(n4[:], uint32(len(dir)))
	b = append(b, n4[:]...)
	return b
}

// —— 写入 ——

// putAuto 是 put 的**分片感知**版本：payload 里有长数组就分片存，
// 否则走原来的整块路径（字节级完全一致）。
//
// 分片时：逐段与已存目录 diff → 只追加**变化的段** → 再写一份新目录。
// 于是「只增」的块（running 卡片）每次只写新增的那一段。
func (bs *blockStore) putAuto(id string, payload []byte) error {
	frags := splitTopLevelArray(payload, fragSplitMinBytes)
	if len(frags) < 2 {
		return bs.putWhole(id, payload)
	}
	return bs.putFragmented(id, payload, frags)
}

// putFragmented 以片段形式写入一个块。
func (bs *blockStore) putFragmented(id string, payload []byte, frags [][]byte) error {
	digests := make([][8]byte, len(frags))
	for i, f := range frags {
		digests[i] = payloadDigest(f)
	}
	logical := payloadDigest(payload)

	// 已存目录（真值在 sidecar 的目录记录里；bs.frags 只是缓存）。
	// 索引命中的打开路径不会重建缓存 → 这里按需读出。
	var prev *fragDir
	if ref, ok := bs.index[id]; ok && isFragmented(ref.kind) {
		prev, _ = bs.fragDirFor(id, ref)
	}
	items := make([]fragItem, len(frags))
	for i, f := range frags {
		key := uint32(i)
		// 未变的片段直接复用旧位置（不写盘）—— 这是写放大归零的关键。
		if prev != nil && i < len(prev.items) && prev.items[i].hash == digests[i] {
			items[i] = prev.items[i]
			continue
		}
		head := buildFragRecordHead(id, key, f)
		off := bs.nBytes
		if _, err := bs.w.Write(head); err != nil {
			return err
		}
		if _, err := bs.w.Write(f); err != nil {
			return err
		}
		if _, err := bs.w.Write(digests[i][:]); err != nil {
			return err
		}
		items[i] = fragItem{key: key, off: off + int64(len(head)), n: int32(len(f)), hash: digests[i]}
		bs.recordsWritten++
		bs.bytesWritten += int64(len(head) + len(f) + 8)
		bs.nBytes += int64(len(head) + len(f) + 8)
	}
	// 目录（放在片段之后写：目录里记的是片段的实际位置）
	dir := &fragDir{
		kind:    blockKindCode(payload),
		total:   int64(len(payload)),
		logical: logical,
		items:   items,
	}
	dirBytes := encodeFragDir(dir)
	dirHead := buildDirRecordHead(id, dirBytes)
	dirOff := bs.nBytes
	if _, err := bs.w.Write(dirHead); err != nil {
		return err
	}
	if _, err := bs.w.Write(dirBytes); err != nil {
		return err
	}
	dd := payloadDigest(dirBytes)
	if _, err := bs.w.Write(dd[:]); err != nil {
		return err
	}
	bs.recordsWritten++
	bs.bytesWritten += int64(len(dirHead) + len(dirBytes) + 8)
	bs.nBytes += int64(len(dirHead) + len(dirBytes) + 8)

	// 索引：分片块的条目指向目录，hash 是**逻辑摘要**（口径与整块一致）。
	if prevRef, existed := bs.index[id]; existed {
		bs.staleRecords++
		bs.liveBytes -= bs.logicalSizeLocked(id, prevRef)
	}
	bs.index[id] = blockRef{
		off:  dirOff + int64(len(dirHead)),
		n:    int32(len(dirBytes)),
		hash: logical,
		kind: kindFragmented | dir.kind,
	}
	bs.frags[id] = dir
	bs.liveBytes += int64(len(payload))
	bs.sortedDirty = true
	return nil
}

// logicalSizeLocked 返回该 id 之前占用的有效字节（整块=记录大小，分片=逻辑长度）。
func (bs *blockStore) logicalSizeLocked(id string, ref blockRef) int64 {
	if d, ok := bs.frags[id]; ok {
		return d.total
	}
	return blockRecordSize(id, int(ref.n))
}

// —— 读取 ——

// fragDirFor 读出并缓存某 id 的片段目录。
//
// 真值在 sidecar 文件的 kind=4 记录里：索引项指向它。
// 为什么不能只靠内存缓存：索引命中的打开路径（rebuild 的 covered==size 分支）
// 只恢复 bs.index，不重建片段信息 —— 片段必须能按需从文件读出。
func (bs *blockStore) fragDirFor(id string, ref blockRef) (*fragDir, bool) {
	if d, ok := bs.frags[id]; ok {
		return d, true
	}
	if !isFragmented(ref.kind) {
		return nil, false
	}
	buf := make([]byte, ref.n)
	if _, err := bs.f.ReadAt(buf, ref.off); err != nil {
		return nil, false
	}
	d, ok := parseFragDir(buf)
	if !ok {
		return nil, false
	}
	bs.frags[id] = d
	return d, true
}

// readFragDir 读出分片块的完整内容（按 key 升序拼接）。
func (bs *blockStore) readFragDir(d *fragDir) ([]byte, bool) {
	buf := make([]byte, 0, d.total)
	for _, it := range d.items {
		p := make([]byte, it.n)
		if _, err := bs.f.ReadAt(p, it.off); err != nil {
			return nil, false
		}
		buf = append(buf, p...)
	}
	return buf, true
}

// readBlock 读一个块的完整内容（整块或分片）。
func (bs *blockStore) readBlock(id string) ([]byte, bool) {
	ref, ok := bs.index[id]
	if !ok {
		return nil, false
	}
	if isFragmented(ref.kind) {
		d, ok := bs.fragDirFor(id, ref)
		if !ok {
			return nil, false
		}
		return bs.readFragDir(d)
	}
	buf := make([]byte, ref.n)
	if _, err := bs.f.ReadAt(buf, ref.off); err != nil {
		return nil, false
	}
	if isCompressed(ref.kind) {
		// 落盘的是压缩字节 → 解压回逻辑内容（见 block_compress.go）。
		out, derr := decompressBlock(buf)
		if derr != nil {
			return nil, false
		}
		buf = out
	}
	return buf, true
}

// readBlockVerified 读一个块并校验（分片走逻辑摘要，整块走 payload 摘要）。
func (bs *blockStore) readBlockVerified(id string) ([]byte, bool) {
	ref, ok := bs.index[id]
	if !ok {
		return nil, false
	}
	if isFragmented(ref.kind) {
		d, ok := bs.fragDirFor(id, ref)
		if !ok {
			return nil, false
		}
		buf, ok := bs.readFragDir(d)
		if !ok {
			return nil, false
		}
		if payloadDigest(buf) != d.logical {
			return nil, false
		}
		return buf, true
	}
	buf := make([]byte, ref.n)
	if _, err := bs.f.ReadAt(buf, ref.off); err != nil {
		return nil, false
	}
	if isCompressed(ref.kind) {
		// 摘要算的是**逻辑内容** ⇒ 先解压再比（解压失败 = 块损坏 ✓）。
		out, derr := decompressBlock(buf)
		if derr != nil {
			return nil, false
		}
		buf = out
	}
	if d := payloadDigest(buf); d != ref.hash {
		return nil, false
	}
	return buf, true
}

// —— 目录记录的处理 ——

// applyDirRecord 处理一条 kind=4（目录）记录：建立分片索引。
func (bs *blockStore) applyDirRecord(id string, dirBytes []byte, off int64) error {
	d, ok := parseFragDir(dirBytes)
	if !ok {
		return fmt.Errorf("block store: corrupt fragment dir for %s", id)
	}
	if prev, existed := bs.index[id]; existed {
		bs.staleRecords++
		bs.liveBytes -= bs.logicalSizeLocked(id, prev)
	}
	bs.index[id] = blockRef{
		off:  off,
		n:    int32(len(dirBytes)),
		hash: d.logical,
		kind: kindFragmented | d.kind,
	}
	bs.frags[id] = d
	bs.liveBytes += d.total
	return nil
}

// —— 记录读取（支持四种 kind）——

// readBlockRecordExt 读取一条记录，支持 kind=1/2/3/4。
//
// 返回 headLen 是该 kind 的**记录头长度**（用于记账 pos 推进）。
// kind=3（片段）与 kind=4（目录）的头比整块多/少一个字段，
// 所以不能用 buildBlockRecordHead 反推长度。
//
// 返回 digest 是记录里带的**逻辑内容**摘要（压缩记录落盘的是压缩字节，
// 但摘要算的是解压后的内容 —— 见 block_compress.go）。
func readBlockRecordExt(r *bufio.Reader) (kind byte, id string, key uint32, payload []byte, digest [8]byte, headLen int, err error) {
	k, err := r.ReadByte()
	if err != nil {
		return 0, "", 0, nil, digest, 0, err
	}
	kind = k
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return 0, "", 0, nil, digest, 0, err
	}
	idLen := int(binary.BigEndian.Uint32(n[:]))
	idb := make([]byte, idLen)
	if _, err := io.ReadFull(r, idb); err != nil {
		return 0, "", 0, nil, digest, 0, err
	}
	id = string(idb)
	headLen = 1 + 4 + idLen
	if kind&^kindCompressed == blockRecFrag {
		// 片段：key(4) 跟在 id 后面
		if _, err := io.ReadFull(r, n[:]); err != nil {
			return 0, "", 0, nil, digest, 0, err
		}
		key = binary.BigEndian.Uint32(n[:])
		headLen += 4
	}
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return 0, "", 0, nil, digest, 0, err
	}
	plen := int(binary.BigEndian.Uint32(n[:]))
	headLen += 4
	payload = make([]byte, plen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, "", 0, nil, digest, 0, err
	}
	// 摘要（8 字节）必须**消费掉**：reader 是顺序读的，不吃它就会错位。
	// （调用方记账时用 headLen+len(payload)+8 跳过它。）
	var sum [8]byte
	if _, err := io.ReadFull(r, sum[:]); err != nil {
		return 0, "", 0, nil, digest, 0, io.ErrUnexpectedEOF
	}
	digest = sum
	// 摘要算的是**逻辑内容**（解压后）—— 压缩记录的落盘字节摘要对不上，
	// 它的校验挪到读路径（那里本来就要解压，见 readBlockVerified）。
	if kind&kindCompressed == 0 {
		if want := payloadDigest(payload); !bytes.Equal(sum[:], want[:]) {
			return 0, "", 0, nil, digest, 0, fmt.Errorf("block store: payload digest mismatch (id %q)", id)
		}
	}
	return kind, id, key, payload, digest, headLen, nil
}
