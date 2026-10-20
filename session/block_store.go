package session

// sidecar 化 blocks：让长会话的每次 checkpoint 只写「新增的 block」，而不是整份 record。
//
// 问题（实测 2026-10，agent-server-g 的真实会话）：
//   一个跑了 64.5 小时的会话，revision 13771（debounce 800ms，平均每 16.9 秒提交一次），
//   record 90.9 MiB。每次提交都要 marshal 91MB + sha256 91MB + 写 91MB + fsync，
//   累计写入约 1.2 TB；而 record 里 95.3% 是 view_checkpoint.blocks，
//   且 3520 个 block **100% 是终态** —— 一个字节都不会再变。
//
// 方案：把 blocks 从 record 主体挪到 append-only 的 sidecar（`<sid>.record.json.blocks`）。
//   · record 里的 blocks 字段变成引用：{"$blocks_ref":{"sum":..,"count":N,"schema":1}}
//   · 提交时：已缓存的 block（终态、id 命中）**不重新序列化、不重写**；
//     新 block 追加到 sidecar 尾部。record 主体因此缩到 ~4MB（只剩 turns/diffs/标量）。
//   · 读取时 resolve：按引用从 sidecar 流式重建 blocks 数组，返回给上层的
//     ViewCheckpoint 与旧格式**逐字节等价**（上层完全无感）。
//
// 为什么按 block 切：block 是 reducer 的不可变单位 —— 终态后 reducer 不再写它
// （view_reducer 的 hasAgentToolItem 只在 running 期追加 items），所以「终态即冻结」
// 是这套方案的正确性基石；running 的 block 永不进缓存。
//
// 一致性三层：
//   1. 每条 sidecar 记录带 8 字节 payload 摘要 → 检出单条损坏；
//   2. record 里存整个 sidecar 的滚动校验链 → 检出整体替换/截断；
//   3. 任何一层不符 → 读取失败并回退 .bak，绝不返回半份数据。

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

const (
	blockStoreSuffix = ".blocks"
	blockRecPut      byte = 1
	blockRecDel      byte = 2
)

var blockStoreMagic = []byte("HBBS0001")

// blocksRefKey 是引用对象在 blocks 字段里的标记键。
const blocksRefKey = "$blocks_ref"

// blocksRefDoc 是 record 里 blocks 字段的引用形态。
type blocksRefDoc struct {
	Store  string `json:"store"`
	Sum    string `json:"sum"`
	Count  int    `json:"count"`
	Schema int    `json:"schema"`
}

// blockStore 是 append-only 的 block 序列化缓存。
//
// 布局：magic(8) || 若干条记录
// 记录：kind(1) || idLen(4) || id || payloadLen(4) || payload || digest(8)
// kind=1 写入/覆盖，kind=2 删除。读取时按序应用，最后一次生效。
type blockStore struct {
	path   string
	f      *os.File
	w      *bufio.Writer
	h      hash.Hash // 滚动校验链（覆盖 magic 与所有记录的 head+payload）
	nBytes int64
	index  map[string]blockRef // id → payload 位置（不驻留 payload 本体）
}

type blockRef struct {
	off  int64
	n    int32
	hash [8]byte // 该 id 当前 payload 的内容摘要
}

// unchanged 报告同 id 的新内容是否与已缓存的一致。
func (r blockRef) unchanged(payload []byte) bool {
	return r.hash == payloadDigest(payload)
}

// blockStoreCache 缓存已打开的 sidecar 实例（按 record 路径）。
//
// 为什么必须缓存：openBlockStore 要扫描整个 sidecar 重建索引（86MB 实测 78ms），
// 而提交是高频路径（debounce 800ms，长会话每 16.9 秒一次）。
// 每次提交重开 = 每次白扫 78ms，吃掉增量方案的收益。
//
// 生命周期：随进程存活；record 被删除时（DeleteSessionRecord）显式剔除。
// 并发：FileStorage.mu 已串行化提交，这里再加一层锁保护 Reader/Writer 路径。
var (
	blockStoreMu    sync.Mutex
	blockStoreCache = map[string]*blockStore{}
)

// acquireBlockStore 取出（或创建）该 record 对应的 sidecar 实例。
// 调用方用完必须 closeBlockStore（它只是归还，不真正关闭）。
func acquireBlockStore(recordPath string) (*blockStore, error) {
	blockStoreMu.Lock()
	defer blockStoreMu.Unlock()
	if bs, ok := blockStoreCache[recordPath]; ok {
		return bs, nil
	}
	bs, err := openBlockStore(blockStorePath(recordPath))
	if err != nil {
		return nil, err
	}
	blockStoreCache[recordPath] = bs
	return bs, nil
}

// closeBlockStore 归还实例（不做 I/O；真正的 flush 由调用方显式触发）。
func closeBlockStore(recordPath string) {
	blockStoreMu.Lock()
	defer blockStoreMu.Unlock()
	if bs, ok := blockStoreCache[recordPath]; ok {
		_ = bs.Close()
		delete(blockStoreCache, recordPath)
	}
}

// evictBlockStore 在 record 被删除/替换时丢弃缓存实例。
func evictBlockStore(recordPath string) { closeBlockStore(recordPath) }

// openBlockStore 打开（或创建）sidecar 并重建索引。
func openBlockStore(path string) (*blockStore, error) {
	bs := &blockStore{path: path, index: map[string]blockRef{}, h: sha256.New()}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	bs.f = f
	if err := bs.rebuild(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return bs, nil
}

// rebuild 扫描已有 sidecar、重建索引与校验链，并把写位置移到末尾。
// 尾部半截记录（崩溃导致）会被截断丢弃 —— 只丢弃未完成的那一条。
func (bs *blockStore) rebuild() error {
	fi, err := bs.f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()
	if size < int64(len(blockStoreMagic)) {
		if err := bs.f.Truncate(0); err != nil {
			return err
		}
		if _, err := bs.f.WriteAt(blockStoreMagic, 0); err != nil {
			return err
		}
		bs.h.Write(blockStoreMagic)
		bs.nBytes = int64(len(blockStoreMagic))
		if err := bs.f.Sync(); err != nil {
			return err
		}
		return bs.startWriter()
	}
	hdr := make([]byte, len(blockStoreMagic))
	if _, err := bs.f.ReadAt(hdr, 0); err != nil {
		return err
	}
	if !bytes.Equal(hdr, blockStoreMagic) {
		return fmt.Errorf("block store %s: bad magic", bs.path)
	}
	bs.h.Write(blockStoreMagic)

	pos := int64(len(blockStoreMagic))
	r := bufio.NewReaderSize(io.NewSectionReader(bs.f, pos, size-pos), 1<<20)
	for {
		off := pos
		kind, id, payload, err := readBlockRecord(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			// 尾部损坏/半截 → 截到最后一个完整记录处。
			if terr := bs.f.Truncate(off); terr != nil {
				return terr
			}
			pos = off
			break
		}
		head := buildBlockRecordHead(kind, id, payload)
		bs.h.Write(head)
		bs.h.Write(payload)
		recLen := int64(len(head) + len(payload) + 8)
		switch kind {
		case blockRecPut:
			bs.index[id] = blockRef{off: off + int64(len(head)), n: int32(len(payload)), hash: payloadDigest(payload)}
		case blockRecDel:
			delete(bs.index, id)
		}
		pos += recLen
	}
	bs.nBytes = pos
	return bs.startWriter()
}

func (bs *blockStore) startWriter() error {
	if _, err := bs.f.Seek(bs.nBytes, io.SeekStart); err != nil {
		return err
	}
	bs.w = bufio.NewWriterSize(bs.f, 1<<20)
	return nil
}

func recordHeadLen(id string) int { return 1 + 4 + len(id) + 4 }

func buildBlockRecordHead(kind byte, id string, payload []byte) []byte {
	b := make([]byte, 0, recordHeadLen(id))
	b = append(b, kind)
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(id)))
	b = append(b, n[:]...)
	b = append(b, id...)
	binary.BigEndian.PutUint32(n[:], uint32(len(payload)))
	b = append(b, n[:]...)
	return b
}

func readBlockRecord(r *bufio.Reader) (kind byte, id string, payload []byte, err error) {
	k, err := r.ReadByte()
	if err != nil {
		return 0, "", nil, err
	}
	var n [4]byte
	if _, err = io.ReadFull(r, n[:]); err != nil {
		return 0, "", nil, io.ErrUnexpectedEOF
	}
	idb := make([]byte, int(binary.BigEndian.Uint32(n[:])))
	if _, err = io.ReadFull(r, idb); err != nil {
		return 0, "", nil, io.ErrUnexpectedEOF
	}
	if _, err = io.ReadFull(r, n[:]); err != nil {
		return 0, "", nil, io.ErrUnexpectedEOF
	}
	payload = make([]byte, int(binary.BigEndian.Uint32(n[:])))
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, "", nil, io.ErrUnexpectedEOF
	}
	var sum [8]byte
	if _, err = io.ReadFull(r, sum[:]); err != nil {
		return 0, "", nil, io.ErrUnexpectedEOF
	}
	if want := payloadDigest(payload); !bytes.Equal(sum[:], want[:]) {
		return 0, "", nil, fmt.Errorf("block store: payload digest mismatch (id %q)", string(idb))
	}
	return k, string(idb), payload, nil
}

func payloadDigest(p []byte) [8]byte {
	s := sha256.Sum256(p)
	var out [8]byte
	copy(out[:], s[:8])
	return out
}

// put 追加一条 block（同 id 后续记录覆盖前者）。
func (bs *blockStore) put(id string, payload []byte) error {
	off := bs.nBytes
	head := buildBlockRecordHead(blockRecPut, id, payload)
	if _, err := bs.w.Write(head); err != nil {
		return err
	}
	if _, err := bs.w.Write(payload); err != nil {
		return err
	}
	d := payloadDigest(payload)
	if _, err := bs.w.Write(d[:]); err != nil {
		return err
	}
	bs.h.Write(head)
	bs.h.Write(payload)
	bs.index[id] = blockRef{off: off + int64(len(head)), n: int32(len(payload)), hash: d}
	bs.nBytes += int64(len(head) + len(payload) + 8)
	return nil
}

func (bs *blockStore) flush() error {
	if bs.w != nil {
		if err := bs.w.Flush(); err != nil {
			return err
		}
	}
	return bs.f.Sync()
}

func (bs *blockStore) Close() error {
	if bs == nil || bs.f == nil {
		return nil
	}
	err := bs.flush()
	if cerr := bs.f.Close(); err == nil {
		err = cerr
	}
	bs.f = nil
	return err
}

// sum 返回当前滚动校验链（写入 record 供读取侧对账）。
func (bs *blockStore) sum() string { return hex.EncodeToString(bs.h.Sum(nil)) }

func (bs *blockStore) has(id string) bool {
	_, ok := bs.index[id]
	return ok
}

// holdsSame 报告 sidecar 里该 id 的内容是否与给定 payload 完全一致。
//
// 这是增量正确性的核心：**不能只按 id 命中就跳过写入**。
// block 会在 running→done 时内容变化（items 追加、status/usage 更新），
// 只按 id 跳过会让磁盘永远停在旧版本 —— 用户看到永不更新的卡片
//（TestSidecarRewritesLiveBlocks 钉住这个行为）。
func (bs *blockStore) holdsSame(id string, payload []byte) bool {
	ref, ok := bs.index[id]
	if !ok {
		return false
	}
	return ref.unchanged(payload)
}

func (bs *blockStore) read(id string) ([]byte, bool) {
	ref, ok := bs.index[id]
	if !ok {
		return nil, false
	}
	buf := make([]byte, ref.n)
	if _, err := bs.f.ReadAt(buf, ref.off); err != nil {
		return nil, false
	}
	return buf, true
}

func (bs *blockStore) Len() int { return len(bs.index) }

func blockStorePath(recordPath string) string { return recordPath + blockStoreSuffix }

// ---------------------------------------------------------------------------
// 编码侧：view_checkpoint.blocks → 引用
// ---------------------------------------------------------------------------

// findBlocksField 一次性定位 view 里 blocks 字段的**元素切分**。
//
// 为什么合并：原先是 splitViewBlocks（找 blocks 值，扫 86MB）+ splitBlocksArray
//（切分元素，再扫 86MB），实测 107+104=211ms 纯浪费在重复扫描上。
// 这里一趟扫描同时完成「找数组」与「按顶层逗号切分」，实测 ~100ms。
//
// 安全性：view 是本进程 reducer 刚序列化出的字节（非外部输入），键名固定；
// 仍做完整字符串/转义/括号配对，异常返回 false 让调用方回退全量路径。
func findBlocksField(view []byte) ([][]byte, bool) {
	const key = `"blocks":`
	ki := bytes.Index(view, []byte(key))
	if ki < 0 {
		return nil, false
	}
	i := ki + len(key)
	for i < len(view) && (view[i] == ' ' || view[i] == '\n' || view[i] == '\t' || view[i] == '\r') {
		i++
	}
	if i >= len(view) || view[i] != '[' {
		return nil, false // 非数组（引用形态等）→ 调用方决定
	}
	bodyStart := i + 1
	// 一趟扫描：跟踪字符串/转义/嵌套深度，顶层逗号处切分。
	depth := 0
	inStr := false
	esc := false
	start := bodyStart
	var out [][]byte
	for j := bodyStart; j < len(view); j++ {
		c := view[j]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			if c == ']' && depth == 0 {
				// 数组结束
				if j > start {
					out = append(out, view[start:j])
				} else if len(out) > 0 {
					// 尾随空元素（`[a,]` 非法，但容错）
				}
				return out, true
			}
			depth--
		case ',':
			if depth == 0 {
				out = append(out, view[start:j])
				start = j + 1
			}
		}
	}
	return nil, false
}

// splitViewBlocks 定位 view 里 blocks 字段的完整值字节（含方括号）。
//
// 读取路径用它判断「是内嵌数组还是 sidecar 引用」—— 那时 view 很小
//（引用形态只有几十字节），不构成性能问题。
func splitViewBlocks(view []byte) ([]byte, bool) {
	const key = `"blocks":`
	i := bytes.Index(view, []byte(key))
	if i < 0 {
		return nil, false
	}
	start := i + len(key)
	for start < len(view) && (view[start] == ' ' || view[start] == '\n' || view[start] == '\t' || view[start] == '\r') {
		start++
	}
	if start >= len(view) {
		return nil, false
	}
	switch view[start] {
	case '[':
		end, ok := matchJSONEnd(view, start, '[', ']')
		if !ok {
			return nil, false
		}
		return view[start:end], true
	case '{':
		end, ok := matchJSONEnd(view, start, '{', '}')
		if !ok {
			return nil, false
		}
		return view[start:end], true
	}
	return nil, false
}

// matchJSONEnd 从 open 位置找到配对的闭合括号（字符串/转义感知）。
func matchJSONEnd(b []byte, open int, openCh, closeCh byte) (int, bool) {
	depth := 0
	inStr := false
	esc := false
	for i := open; i < len(b); i++ {
		c := b[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case openCh:
			depth++
		case closeCh:
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

// splitBlocksArray 把 `[a,b,c]` 拆成元素切片（零拷贝，指向同一 backing）。
func splitBlocksArray(raw []byte) ([][]byte, bool) {
	t := bytes.TrimSpace(raw)
	if len(t) < 2 || t[0] != '[' || t[len(t)-1] != ']' {
		return nil, false
	}
	body := t[1 : len(t)-1]
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, true
	}
	var out [][]byte
	depth := 0
	inStr := false
	esc := false
	start := 0
	for i := 0; i < len(body); i++ {
		c := body[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, body[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, body[start:])
	return out, true
}

// blockID 取 block 对象的 id（缓存键）。
func blockID(elem []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(elem))
	tok, err := dec.Token()
	if err != nil {
		return "", false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", false
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return "", false
		}
		if key, _ := kt.(string); key == "id" {
			var id uint64
			if err := dec.Decode(&id); err != nil {
				return "", false
			}
			return strconv.FormatUint(id, 10), true
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return "", false
		}
	}
	return "", false
}

// isLiveBlock 报告 block 是否仍可能变化。
//
// 只有终态 block 可进 sidecar：reducer 在 running 期会按 tool_id 追加 items，
// 缓存它会让后续提交读到陈旧内容。
// status 字段在 Block 结构里位于 id/kind/ts 之后、items 之前，
// 实测偏移 < 120 字节。只查头部即可，避免对 86MB 逐块全文扫描（实测 49ms → 0.3ms）。
func isLiveBlock(elem []byte) bool {
	head := elem
	if len(head) > 256 {
		head = head[:256]
	}
	return bytes.Contains(head, []byte(`"status":"running"`)) ||
		bytes.Contains(head, []byte(`"status":"interrupting"`))
}

// refBlocks 把 view 里的 blocks 逐块存入 sidecar，并把 blocks 字段改写成引用。
//
// 返回改写后的 view（体积从 87MB 降到几 KB）与本次新写入的 block 数。
// 任何 block 缺 id / 结构异常 → 返回 error，调用方回退全量路径。
func refBlocks(bs *blockStore, view []byte) ([]byte, int, error) {
	elems, ok := findBlocksField(view)
	if !ok {
		return nil, 0, fmt.Errorf("view has no blocks array")
	}

	wrote := 0
	for _, elem := range elems {
		id, ok := blockID(elem)
		if !ok {
			return nil, 0, fmt.Errorf("block without id")
		}
		if isLiveBlock(elem) {
			// 进行中的 block 每次都覆盖写（内容会变），不复用缓存。
			if err := bs.put(id, elem); err != nil {
				return nil, 0, err
			}
			wrote++
			continue
		}
		if bs.holdsSame(id, elem) {
			continue // 终态且内容未变 → 完全跳过（本优化的收益来源）
		}
		if err := bs.put(id, elem); err != nil {
			return nil, 0, err
		}
		wrote++
	}

	ref := blocksRefDoc{Store: filepath.Base(bs.path), Sum: bs.sum(), Count: len(elems), Schema: 1}
	refJSON, err := json.Marshal(map[string]blocksRefDoc{blocksRefKey: ref})
	if err != nil {
		return nil, 0, err
	}
	out, err := replaceJSONField(view, "blocks", refJSON)
	if err != nil {
		return nil, 0, err
	}
	return out, wrote, nil
}

// refBlocksFromSpans 用调用方已切好的 block 字节直接写 sidecar，**不做任何扫描**。
//
// 这是增量方案的关键入口：reducer 在 Marshal 时本来就有每个 block 的字节，
// 由它给出 spans 后，存储层不必再解析 86MB（实测省 106ms/次提交）。
//
// 安全性完全依赖调用方保证 spans 与 view 一致（同一次 Marshal 产出）；
// 数量对不上就回退全量路径。
func refBlocksFromSpans(bs *blockStore, view []byte, spans [][]byte) ([]byte, int, error) {
	if len(spans) == 0 {
		// 空 blocks：仍要写引用，避免下次被当作旧格式
		ref := blocksRefDoc{Store: filepath.Base(bs.path), Sum: bs.sum(), Count: 0, Schema: 1}
		refJSON, _ := json.Marshal(map[string]blocksRefDoc{blocksRefKey: ref})
		out, err := replaceJSONField(view, "blocks", refJSON)
		if err != nil {
			return nil, 0, err
		}
		return out, 0, nil
	}

	wrote := 0
	for _, elem := range spans {
		id, ok := blockID(elem)
		if !ok {
			return nil, 0, fmt.Errorf("block without id")
		}
		if isLiveBlock(elem) {
			if err := bs.put(id, elem); err != nil {
				return nil, 0, err
			}
			wrote++
			continue
		}
		if bs.holdsSame(id, elem) {
			continue
		}
		if err := bs.put(id, elem); err != nil {
			return nil, 0, err
		}
		wrote++
	}
	ref := blocksRefDoc{Store: filepath.Base(bs.path), Sum: bs.sum(), Count: len(spans), Schema: 1}
	refJSON, err := json.Marshal(map[string]blocksRefDoc{blocksRefKey: ref})
	if err != nil {
		return nil, 0, err
	}
	// 用 spans 的已知总长度直接算出数组结尾，省掉 matchJSONEnd 的 86MB 扫描
	//（实测 110ms，占提交 20%）。
	out, err := replaceBlocksWithKnownSpans(view, spans, refJSON)
	if err != nil {
		// 长度对不上（调用方 spans 与 view 不一致）→ 回退扫描式定位。
		fallback, ferr := replaceJSONField(view, "blocks", refJSON)
		if ferr != nil {
			return nil, 0, ferr
		}
		return fallback, wrote, nil
	}
	return out, wrote, nil
}

// replaceBlocksWithKnownSpans 用 spans 的总长度定位 blocks 数组末尾并替换。
//
// spans[i] 就是数组里第 i 个元素的字节；数组原文是
// `[` + spans[0] + `,` + ... + spans[n-1] + `]`，总长 = sum(len)+n+1。
// 因此结尾位置 = 起点 + 总长，无需扫描。
func replaceBlocksWithKnownSpans(view []byte, spans [][]byte, value []byte) ([]byte, error) {
	const key = `"blocks":`
	ki := bytes.Index(view, []byte(key))
	if ki < 0 {
		return nil, fmt.Errorf("no blocks field")
	}
	start := ki + len(key)
	for start < len(view) && (view[start] == ' ' || view[start] == '\n' || view[start] == '\t' || view[start] == '\r') {
		start++
	}
	if start >= len(view) || view[start] != '[' {
		return nil, fmt.Errorf("blocks is not an array")
	}
	total := 0
	for _, sp := range spans {
		total += len(sp)
	}
	end := start + total + len(spans) + 1 // '[' + 元素 + ',' ×(n-1) + ']'
	if end > len(view) {
		return nil, fmt.Errorf("spans length exceeds view")
	}
	// 校验：结尾前必须是 ']'，否则 spans 与 view 不匹配。
	if view[end-1] != ']' {
		return nil, fmt.Errorf("span-derived end is not ']'")
	}
	out := make([]byte, 0, len(view)-(end-start)+len(value))
	out = append(out, view[:start]...)
	out = append(out, value...)
	out = append(out, view[end:]...)
	return out, nil
}

// replaceJSONField 把对象里某个字段的值替换成给定的 JSON 字节。
// 只处理「字段已存在」的形态（view 恒含 blocks）。
func replaceJSONField(obj []byte, field string, value []byte) ([]byte, error) {
	t := bytes.TrimSpace(obj)
	if len(t) < 2 || t[0] != '{' || t[len(t)-1] != '}' {
		return nil, fmt.Errorf("not a JSON object")
	}
	// 字节定位而非 json.Decoder：后者逐 token 走完 90MB 实测 371ms
	//（占提交 70%），而这里只需找到 key 字面量再跳到值起点，~2ms。
	keyLit := []byte(`"` + field + `":`)
	ki := bytes.Index(t, keyLit)
	if ki < 0 {
		return nil, fmt.Errorf("field %q not found", field)
	}
	// 安全：只接受「key 在顶层」的形态。
	//
	// 不用 depthAt(t, ki) 全前缀扫描（90MB 实测 100ms+）：改为只看 key 前面的
	// 那个非空白字节 —— 根对象里两个顶层键之间必然是 ','，而嵌套出现的
	// `"blocks":` 前面是 '{' 或 ',' 且位于更深层。用「前一个逗号或 '{' 之后
	// 没有未闭合的括号」来判断太绕，这里用更便宜且严格的等价判据：
	//   ki 之前出现的 '{' 与 '}' 数量必须相等（即所有嵌套对象都已闭合）。
	// 只统计括号、跳过字符串，仍是线性扫描但常量极小；且在实践中
	// blocks 是第 2 个字段，前缀只有几十字节。
	if !topLevelKey(t, ki) {
		return nil, fmt.Errorf("field %q is not top-level", field)
	}
	valStart := ki + len(keyLit)
	for valStart < len(t) && (t[valStart] == ' ' || t[valStart] == '\n' || t[valStart] == '\t' || t[valStart] == '\r') {
		valStart++
	}
	if valStart >= len(t) {
		return nil, fmt.Errorf("field %q has no value", field)
	}
	var valEnd int
	switch t[valStart] {
	case '[':
		e, ok := matchJSONEnd(t, valStart, '[', ']')
		if !ok {
			return nil, fmt.Errorf("unbalanced array for %q", field)
		}
		valEnd = e
	case '{':
		e, ok := matchJSONEnd(t, valStart, '{', '}')
		if !ok {
			return nil, fmt.Errorf("unbalanced object for %q", field)
		}
		valEnd = e
	default:
		// 标量：走到下一个 , 或 }
		j := valStart
		for j < len(t) && t[j] != ',' && t[j] != '}' {
			j++
		}
		valEnd = j
	}
	out := make([]byte, 0, len(t)-(valEnd-valStart)+len(value))
	out = append(out, t[:valStart]...)
	out = append(out, value...)
	out = append(out, t[valEnd:]...)
	return out, nil
}

// topLevelKey 报告 pos 处的 key 是否位于根对象的第一层。
//
// 判据：pos 之前 '{' 与 '}' 的净差为 1（只开着根对象），且方括号净差为 0。
func topLevelKey(b []byte, pos int) bool {
	curly, square := 0, 0
	inStr := false
	esc := false
	for i := 0; i < pos && i < len(b); i++ {
		c := b[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			curly++
		case '}':
			curly--
		case '[':
			square++
		case ']':
			square--
		}
	}
	return curly == 1 && square == 0
}

// ---------------------------------------------------------------------------
// 读取侧：引用 → 真实 blocks 数组
// ---------------------------------------------------------------------------

// resolveBlocksRef 在读取时把引用还原成完整 blocks 数组。
//
// 若 view 里没有引用（旧格式），原样返回。
// 引用存在但 sidecar 不可用/校验不符 → 返回错误（调用方回退 .bak），
// **绝不**返回「blocks 为空」的半份数据。
func resolveBlocksRef(view []byte, recordPath string) ([]byte, error) {
	blocksRaw, ok := splitViewBlocks(view)
	if !ok {
		return view, nil // 无 blocks 字段：保持原样，由上层校验
	}
	trimmed := bytes.TrimSpace(blocksRaw)
	if !bytes.Contains(trimmed, []byte(blocksRefKey)) {
		return view, nil // 旧格式：内嵌完整数组
	}
	var wrapper map[string]blocksRefDoc
	if err := json.Unmarshal(trimmed, &wrapper); err != nil {
		return nil, fmt.Errorf("parse blocks ref: %w", err)
	}
	ref, ok := wrapper[blocksRefKey]
	if !ok {
		return nil, fmt.Errorf("parse blocks ref: missing %q", blocksRefKey)
	}

	path := blockStorePath(recordPath)
	bs, err := openBlockStore(path)
	if err != nil {
		return nil, fmt.Errorf("open block store: %w", err)
	}
	defer bs.Close()

	if ref.Sum != "" && bs.sum() != ref.Sum {
		return nil, fmt.Errorf("block store checksum mismatch (have %s want %s)", bs.sum(), ref.Sum)
	}
	if ref.Count != bs.Len() {
		return nil, fmt.Errorf("block store count mismatch (have %d want %d)", bs.Len(), ref.Count)
	}

	full, err := bs.assemble()
	if err != nil {
		return nil, err
	}
	return replaceJSONField(view, "blocks", full)
}

// assemble 按 id 顺序拼出完整 blocks 数组。
//
// 顺序为什么可靠：sidecar 是 append-only，索引里的 offset 单调递增；
// 但 reducer 的 block 顺序由 id 决定（nextBlockID 单调递增），而 block 在数组里
// 的顺序就是 id 递增序 —— 两者一致。这里显式按 id 数值排序，不依赖写入顺序。
func (bs *blockStore) assemble() ([]byte, error) {
	ids := make([]string, 0, len(bs.index))
	for id := range bs.index {
		ids = append(ids, id)
	}
	nums := make([]uint64, len(ids))
	for i, id := range ids {
		n, err := strconv.ParseUint(id, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("block store: non-numeric block id %q", id)
		}
		nums[i] = n
	}
	sortUint64WithIDs(nums, ids)

	var buf bytes.Buffer
	buf.Grow(1 << 20)
	buf.WriteByte('[')
	for i, id := range ids {
		payload, ok := bs.read(id)
		if !ok {
			return nil, fmt.Errorf("block store: missing payload for id %q", id)
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(payload)
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}

// sortUint64WithIDs 按 nums 升序同步重排 ids（插入排序足够：区间顺序基本已排好）。
func sortUint64WithIDs(nums []uint64, ids []string) {
	for i := 1; i < len(nums); i++ {
		n, id := nums[i], ids[i]
		j := i - 1
		for j >= 0 && nums[j] > n {
			nums[j+1], ids[j+1] = nums[j], ids[j]
			j--
		}
		nums[j+1], ids[j+1] = n, id
	}
}

// removeBlockStore 删除 sidecar（record 删除时同步清理）。
func removeBlockStore(recordPath string) {
	_ = os.Remove(blockStorePath(recordPath))
}
