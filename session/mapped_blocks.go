package session

// sidecar 的只读映射视图（S2：零拷贝取块）。
//
// 背景（2026-11-08 实测）：LoadRecord 498ms 里 347ms 是 assemble+replace ——
// 逐块 ReadAt(209MB) → sha256 → 写进一个大 buffer → 再用 replaceJSONField
// 把 buffer 复制进 view，合计 4 次 209MB 的搬运。而恢复路径真正需要的只是
// 「每个块的一段字节」，于是改成：mmap 整个 sidecar，span 直接切映射，
// 拼装与复制全部消失（209MB 由内核按页映射，只在解码时触碰）。
//
// 正确性边界：
//   - 只在索引覆盖整个 sidecar 时启用（否则尾部新块不可见，宁可走慢路径）；
//   - 映射是只读的，调用方必须在解码完成后立即 release —— 提交/回收会
//     重写文件（rename 换 inode 安全，truncate 不安全）；
//   - payload 完整性不在打开时校验，改为解码时逐块比对索引里的摘要
//     （见 LoadedBlockHashes），与旧路径的校验强度一致。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// errMappedUnavailable 表示这条 record 无法用映射路径（无索引 / 索引落后 /
// 平台不支持），调用方应回退到完整的拼装路径。
var errMappedUnavailable = fmt.Errorf("mapped blocks unavailable")

// mappedBlocks 是 sidecar 的只读 mmap 视图。
type mappedBlocks struct {
	path  string
	f     *os.File
	data  []byte
	index map[string]blockRef
}

// openMappedBlocks 打开 sidecar 的只读映射。
func openMappedBlocks(path string) (*mappedBlocks, error) {
	entries, covered, _, _, _, ok := loadBlockIndex(path)
	if !ok {
		return nil, fmt.Errorf("%w: no index", errMappedUnavailable)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if fi.Size() != covered || fi.Size() == 0 {
		// 索引落后/超前：映射会漏掉尾部记录，交给拼装路径（它只扫增量）。
		_ = f.Close()
		return nil, fmt.Errorf("%w: index covered=%d size=%d", errMappedUnavailable, covered, fi.Size())
	}
	data, err := mapFileReadOnly(f, fi.Size())
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: mmap: %v", errMappedUnavailable, err)
	}
	return &mappedBlocks{path: path, f: f, data: data, index: indexToMap(entries)}, nil
}

// span 返回块 id 的只读窗口与期望摘要。
func (m *mappedBlocks) span(id string) ([]byte, [8]byte, bool) {
	ref, ok := m.index[id]
	if !ok {
		return nil, [8]byte{}, false
	}
	end := ref.off + int64(ref.n)
	if ref.off < 0 || end > int64(len(m.data)) {
		return nil, [8]byte{}, false
	}
	return m.data[ref.off:end], ref.hash, true
}

func (m *mappedBlocks) close() error {
	var err error
	if m.data != nil {
		err = unmapFile(m.data)
		m.data = nil
	}
	if m.f != nil {
		if cerr := m.f.Close(); err == nil {
			err = cerr
		}
		m.f = nil
	}
	return err
}

// blocksRefTarget 解析 view 里的 blocks 引用，返回引用与 sidecar 路径。
// isRef=false 表示不是引用形态（旧格式内嵌数组，或根本没有 blocks 字段）。
func blocksRefTarget(view []byte, recordPath string) (ref blocksRefDoc, storePath string, isRef bool, err error) {
	blocksRaw, ok := splitViewBlocks(view)
	if !ok {
		return blocksRefDoc{}, "", false, nil
	}
	trimmed := bytes.TrimSpace(blocksRaw)
	if !bytes.Contains(trimmed, []byte(blocksRefKey)) {
		return blocksRefDoc{}, "", false, nil
	}
	var wrapper map[string]blocksRefDoc
	if uerr := json.Unmarshal(trimmed, &wrapper); uerr != nil {
		return blocksRefDoc{}, "", false, fmt.Errorf("parse blocks ref: %w", uerr)
	}
	ref, ok = wrapper[blocksRefKey]
	if !ok {
		return blocksRefDoc{}, "", false, fmt.Errorf("parse blocks ref: missing %q", blocksRefKey)
	}
	storePath, perr := refStorePath(recordPath, ref.Store)
	if perr != nil {
		return blocksRefDoc{}, "", false, perr
	}
	return ref, storePath, true, nil
}

// mappedView 是「整份 sidecar 的只读映射 + 按 id 升序的块窗口」。
type mappedView struct {
	ids     []uint64
	spans   [][]byte
	hashes  [][8]byte
	kinds   []uint8
	release func()
}

// resolveBlocksRefMapped 用只读映射解析 blocks 引用：返回按 id 升序的
// 窗口（零拷贝指向 mmap）与每块摘要/kind。校验口径与 resolveBlocksRefSpans 一致。
func resolveBlocksRefMapped(view []byte, recordPath string) (*mappedView, error) {
	ref, storePath, isRef, err := blocksRefTarget(view, recordPath)
	if err != nil || !isRef {
		return nil, err
	}
	mapped, err := openMappedBlocks(storePath)
	if err != nil {
		return nil, err
	}
	fail := func(e error) (*mappedView, error) {
		_ = mapped.close()
		return nil, e
	}

	ids := make([]string, 0, len(mapped.index))
	if refNums := ref.refIDs(); len(refNums) > 0 {
		if ref.Count != len(refNums) {
			return fail(fmt.Errorf("block ref malformed: count=%d ids=%d", ref.Count, len(refNums)))
		}
		for _, v := range refNums {
			ids = append(ids, uintToString(v))
		}
	} else {
		// 旧口径（无 ids）：引用集合 == 索引全集。
		if ref.Count != len(mapped.index) {
			return fail(fmt.Errorf("block store count mismatch (have %d want %d)", len(mapped.index), ref.Count))
		}
		for id := range mapped.index {
			ids = append(ids, id)
		}
	}
	// 摘要校验只触碰索引（不读 payload）：200ms → 亚毫秒，且足以证明
	// 「record 引用的正是这份索引」；payload 逐块校验下沉到解码阶段。
	got, serr := indexSetSum(mapped.index, ids)
	if serr != nil {
		return fail(fmt.Errorf("block store missing referenced block: %w", serr))
	}
	// 只有「集合摘要」口径可重算（旧口径是整文件滚动链，见 resolveBlocksRef 注释）。
	if strings.HasPrefix(ref.Sum, setSumPrefix) && got != ref.Sum {
		return fail(fmt.Errorf("block store checksum mismatch (have %s want %s)", got, ref.Sum))
	}

	// 统一按 id 数值升序：与 record 里的块顺序一致，窗口恢复才能「取尾部 N 个」。
	nums := make([]uint64, len(ids))
	for i, id := range ids {
		n, perr := parseUintID(id)
		if perr != nil {
			return fail(fmt.Errorf("block store: non-numeric block id %q", id))
		}
		nums[i] = n
	}
	sortUint64WithIDs(nums, ids)

	spans := make([][]byte, len(ids))
	hashes := make([][8]byte, len(ids))
	kinds := make([]uint8, len(ids))
	for i, id := range ids {
		if r, ok := mapped.index[id]; ok {
			kinds[i] = r.kind
		}
		span, h, ok := mapped.span(id)
		if !ok {
			return fail(fmt.Errorf("block store missing referenced block: %s", id))
		}
		spans[i] = span
		hashes[i] = h
	}
	release := func() { _ = mapped.close() }
	return &mappedView{ids: nums, spans: spans, hashes: hashes, kinds: kinds, release: release}, nil
}

// attachMappedSpans 为 record 挂上映射视图；映射不可用（无索引/索引落后/
// 平台不支持/旧格式内嵌数组）时回退到完整拼装路径，不影响正确性。
func attachMappedSpans(record *SessionRecord, body ViewCheckpoint, path string) error {
	mv, merr := resolveBlocksRefMapped(body, path)
	switch {
	case merr == nil && mv != nil && len(mv.spans) > 0:
		record.BodyView = body
		record.ViewCheckpoint = nil // 不再展开：调用方用骨架 + spans
		record.LoadedBlockSpans = mv.spans
		record.LoadedBlockHashes = mv.hashes
		record.LoadedBlockIDs = mv.ids
		record.LoadedBlockKinds = mv.kinds
		record.releaseView = mv.release
		return nil
	case merr == nil:
		// 不是引用形态（旧格式内嵌数组）→ 保持整份 view。
		record.BodyView = body
		return nil
	case errors.Is(merr, errMappedUnavailable), errors.Is(merr, os.ErrNotExist):
		resolved, spans2, rerr := resolveBlocksRefSpans(body, path)
		if rerr != nil {
			return rerr
		}
		record.ViewCheckpoint = resolved
		record.LoadedBlockSpans = spans2
		record.BodyView = body
		return nil
	default:
		return merr
	}
}
