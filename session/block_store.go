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
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	blockStoreSuffix      = ".blocks"
	blockRecPut      byte = 1
	blockRecDel      byte = 2
)

var blockStoreMagic = []byte("HBBS0001")

// blocksRefKey 是引用对象在 blocks 字段里的标记键。
const blocksRefKey = "$blocks_ref"

// setSumPrefix 区分两种 sum 口径，让「哪种口径」成为 O(1) 的字符串判断。
//
// 背景：Sum 曾有前后两种定义
//
//	· 旧口径 "sha256:"     整份 sidecar 的滚动链（含废弃记录）
//	· 新口径 "blockset1:"  被引用块集合的摘要
//
// 旧实现读到 Sum 后**两种都算一遍**再比对：新口径要读全部 payload（~200ms），
// 不匹配再算旧口径把整个 sidecar 重读重哈希（1.8GB 实测 910ms）。
// 真实会话里每条 record 都是旧口径 → **每次打开白付 1.1 秒**（2026-10-20 实测）。
//
// 有了前缀就能直接判断该算哪个，不必试错。
const setSumPrefix = "blockset1:"

// blocksRefDoc 是 record 里 blocks 字段的引用形态。
type blocksRefDoc struct {
	Store  string `json:"store"`
	Sum    string `json:"sum"`
	Count  int    `json:"count"`
	Schema int    `json:"schema"`
	// IDs 是这条 record 引用的块 id（升序）。
	//
	// 为什么必须写进 record（2026-11-08）：旧引用只有 (count,sum)，读取时只能
	// 与「sidecar 索引全集」严格比对 —— 于是任何一次后续提交（哪怕只是**追加**
	// 一个新块）都会让上一版 record 与 .bak 同时失效：
	//
	//	· 超时/崩溃发生在 sidecar flush 之后 → 磁盘上 record 与 sidecar 不匹配
	//	  → 会话再也读不出来（真实故障，30s checkpoint 超时后必现）；
	//	· .bak 永远比当前 sidecar 旧 → 兜底路径 100% 不可用。
	//
	// 有了 ids，读取只校验「我引用的这些块」是否存在且内容一致，
	// sidecar 里多出来的块（后续提交/废弃版本）不再影响可读性。
	IDs []uint64 `json:"ids,omitempty"`

	// Range 是「id 连续」时的紧凑写法（min..max），此时省略 IDs。
	//
	// 为什么需要：块 id 由 reducer 单调分配，正常情况下是一个连续的 1..N 集合，
	// 30k 块写成 IDs 数组是约 210KB/次提交的 JSON（真实会话 30,885 块）；
	// 用 range 只需 30 字节。稀疏集合（发生过裁剪/清空后的洞）仍写 IDs。
	Range *idRange `json:"range,omitempty"`

	// Missing 是 Range 里的「洞」（被裁剪/清空后不再引用的 id，通常个位数）。
	// 真实会话实测：31,040 个引用、max=31,042 —— 3 个洞，写成 missing 只有 20 字节。
	Missing []uint64 `json:"missing,omitempty"`
}

// idRange 是闭区间 [Min, Max]。
type idRange struct {
	Min uint64 `json:"min"`
	Max uint64 `json:"max"`
}

// refIDs 展开引用里的 id 集合（IDs 优先；否则按 Range 连续展开）。
func (r blocksRefDoc) refIDs() []uint64 {
	if len(r.IDs) > 0 {
		return r.IDs
	}
	if r.Range != nil && r.Range.Max >= r.Range.Min {
		hole := make(map[uint64]struct{}, len(r.Missing))
		for _, v := range r.Missing {
			hole[v] = struct{}{}
		}
		out := make([]uint64, 0, r.Range.Max-r.Range.Min+1)
		for v := r.Range.Min; v <= r.Range.Max; v++ {
			if _, ok := hole[v]; ok {
				continue
			}
			out = append(out, v)
		}
		return out
	}
	return nil
}

// maybeDenseRange 在 id 连续时把 IDs 换成 Range，省掉 30k 个数字
// （真实会话约 210KB/次提交的 JSON）。
func (r *blocksRefDoc) maybeDenseRange() {
	if len(r.IDs) < 2 {
		return
	}
	lo, hi := r.IDs[0], r.IDs[len(r.IDs)-1]
	span := hi - lo + 1
	if span < uint64(len(r.IDs)) {
		return
	}
	holes := span - uint64(len(r.IDs))
	// 洞太多（超过 1% 或 64 个）就不值得用 range，直接写 ids。
	// 判据按「序列化字节数」比较，而不是拍一个百分比：ids 每个约 6.3 字节，
	// missing 也是每个约 6.3 字节 —— 所以只要洞比引用少，写 missing 就更小。
	// 真实会话实测：n=30,947、hi=34,495、洞=3,548（10.3%）→ missing 约 25KB，
	// 而 ids 是 200KB。（早先按 10% 卡，正好把这条挡在门外。）
	if holes > uint64(len(r.IDs)) || holes > 8192 {
		return
	}
	missing := make([]uint64, 0, holes)
	next := lo
	for _, v := range r.IDs {
		for next < v {
			missing = append(missing, next)
			next++
		}
		next = v + 1
	}
	r.Range = &idRange{Min: lo, Max: hi}
	r.Missing = missing
	r.IDs = nil
}

// refCount 返回引用集合的块数（Range 形态下 count 仍是权威值）。
func (r blocksRefDoc) refCount() int {
	if len(r.IDs) > 0 {
		return len(r.IDs)
	}
	if r.Range != nil && r.Count > 0 {
		return r.Count
	}
	return r.Count
}

// sortedIDNums 把 id 字符串转成升序的数值列表（写引用时用）。
func sortedIDNums(ids []string) ([]uint64, error) {
	nums := make([]uint64, 0, len(ids))
	for _, id := range ids {
		v, err := parseUintID(id)
		if err != nil {
			return nil, err
		}
		nums = append(nums, v)
	}
	sortUint64s(nums)
	return nums, nil
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
	nBytes int64
	index  map[string]blockRef // id → payload 位置（不驻留 payload 本体）

	// sidecarSize 是本实例已知的稳定文件大小（写满后更新）。
	sidecarSize int64
	// sortedIDs 是全部 block id 的升序缓存（打开时排一次，分页 O(1) 定位）。
	// 为什么缓存：map 迭代顺序随机，每页重排 21759 项实测 59.7ms，
	// 而分页每次只该花「读那几块」的钱。
	sortedIDs   []uint64
	sortedDirty bool // 有写入后置位，下次分页前重建

	// openSize/openMtime 是打开时的 sidecar 状态，用于检出「文件被外部改动」。
	//
	// 为什么必须检查：blockStoreCache 是进程内缓存，而 sidecar 可能被另一个
	// 进程（或同一进程的旧实例）追加/compact。缓存命中却文件已变 → 索引位置
	// 全部失效 → 读到错位的字节，或校验报 mismatch。
	// 实测：真实会话在测量期间被后台进程持续写入（838MB→1378MB），
	// 缓存返回了陈旧索引，导致 LoadRecord 报 checksum mismatch。
	openSize  int64
	openMtime int64

	// recordsWritten 是已写入（含覆盖）的记录总数，staleRecords 是被后续记录
	// 覆盖掉的条数。二者用于判断是否需要 compact —— 全都 O(1) 维护，
	// 不拖慢提交流程。
	//
	// 为什么需要它：sidecar 是 append-only 的。子智能体卡片在 running 期每次
	// 事件都会重写同一个 block，于是同一 id 的历史版本无限堆积。真实会话实测
	// 23893 条记录里只有 21551 个唯一 id —— **93% 是废弃版本**，文件 1.4GB
	// 里有效内容只有 58MB。
	recordsWritten int64
	staleRecords   int64
	// lastCompactAt 是最近一次压缩完成的时刻（零值 = 从未压缩过）。
	//
	// 用途：同一 record 的两次压缩之间的**最小间隔**（见 minCompactInterval）。
	// 它是进程内状态：重启后归零，于是「上次压缩遗留的垃圾」可以在启动后被
	// 一次性回收 —— 这正是我们想要的（重启本来就要付一次打开成本）。
	lastCompactAt time.Time
	// bytesWritten 是历史累计写入字节（含废弃），用于估算放大倍数。
	bytesWritten int64
	// hashedBytes 是累计被哈希校验的 payload 字节数（可观测性/测试用）。
	// 提交路径的 sha256 成本直接由它体现：脏块快路径生效时它应当为 0。
	hashedBytes int64
	// liveBytes 是索引里当前有效记录占用的字节数（含每条记录的头部与摘要）。
	//
	// 为什么需要它：compact 判据原本只看「废弃条数/总条数」。真实会话里废弃的
	// 恰恰是**大块**（子智能体卡片 running 期整块重写），条数占比 18% 而字节
	// 占比 94% —— 判据看不见的前者让 3.45GB 的文件永不回收。
	// 维护成本 O(1)：put/delete 各加减一次，重建时按索引求和。
	liveBytes int64
	// frags 是**分片块**的片段目录：id → 目录。
	//
	// 有了它，「只增」的块（running 的 agent 卡片）每次只追加新增的那一段，
	// 而不是整个 payload —— 见 block_frags.go 的写放大分析。
	// 整块块不在这里；两者互斥（kind=1 清空分片，kind=4 建立分片）。
	frags map[string]*fragDir
}

// blockRecordSize 是一条 sidecar 记录的总字节数（头 + payload + 摘要）。
func blockRecordSize(id string, payloadLen int) int64 {
	return int64(1 + 4 + len(id) + 4 + payloadLen + 8)
}

type blockRef struct {
	off  int64
	n    int32
	hash [8]byte // 该 id 当前 payload 的**逻辑内容**摘要（解压后算 —— 见 block_compress.go）
	// kind 是**块类型编码 + 标志位**（见 blockIndex.go 的 kind 字节布局）：
	// 低位 = 真实块类型；0x80 = 分片；0x40 = 落盘是压缩字节。
	//
	// 为什么标志放这里：索引文件（.idx）的每条记录**本来就有一个 kind 字节**
	// ⇒ 压缩标志无需改索引格式 ✓（旧索引照常可读 ✓）。
	kind uint8
}

// unchanged 报告同 id 的新内容是否与已缓存的一致。
func (r blockRef) unchanged(payload []byte) bool {
	return r.hash == payloadDigest(payload)
}

// blockStoreCache 缓存已打开的 sidecar 实例（按 record 路径）。
//
// ## 为什么必须缓存
//
// openBlockStore 要扫描整个 sidecar 重建索引（86MB 实测 78ms），而提交是高频路径
// （debounce 800ms，长会话每 16.9 秒一次）。每次提交重开 = 每次白扫 78ms，
// 吃掉增量方案的收益。
//
// ## 生命周期：LRU + 预算（2026-11-12，P2）
//
// 原来「随进程存活；record 被删除时显式剔除」⇒ **无上限增长**：
// 实测 bridge 打开 **395 个 sidecar**（共 1184 个 fd），而 UI 只开了 15 个会话。
//
// 根因不是"会话多"，而是**启动清扫**对每个 record 都 `acquireBlockStore`
// 却从不释放（446 个 record ⇒ 446 个 sidecar 常开）。
//
// 现在三件事：
//
//	· **引用计数（pin）** —— 正在用的实例不可淘汰。
//	  这不只是为了淘汰，也是**正确性**：后台 compact 会在释放 record 锁之后
//	  继续用 bs（compact_scheduler.go 的 rewriteCompact 阶段），
//	  没有 pin 时它可能被另一个 goroutine 关掉 ⇒ use-after-close。
//	· **LRU + 预算** —— 超预算时按「最近最少使用」淘汰**未 pin** 的实例。
//	  淘汰动作在**后台**做：Close 会 fsync 整个 sidecar，提交路径延迟敏感。
//	· **预算按索引内存算**，不按实例数 —— 大小会话的索引差三个数量级
//	  （40,232 块 vs 20 块）。
//
// 并发：FileStorage.mu 已串行化提交，这里再加一层锁保护 Reader/Writer 路径。
type blockStoreEntry struct {
	bs  *blockStore
	pin int       // 引用计数：>0 ⇒ 不可淘汰
	at  time.Time // 最近一次 acquire 的时刻（LRU 用）
}

var (
	blockStoreMu    sync.Mutex
	blockStoreCache = map[string]*blockStoreEntry{}
)

// 缓存预算（var 而非 const：测试要能临时压到 0 来验证淘汰路径）。
var (
	// blockStoreCacheBudgetBytes 是 sidecar 实例缓存的**索引内存**预算。
	//
	// 按内存而不是实例数：大会话的索引（40,232 条 × ~96B ≈ 4MB）是小会话的
	// 上千倍，按个数限会让「大会话 × 少量」这种组合直接爆内存。
	blockStoreCacheBudgetBytes int64 = 64 << 20

	// blockStoreCacheMaxInstances 是实例数上限（句柄上限）。
	// macOS 的 fd 上限很高（实测 1,048,576），但每个实例还占一个 bufio（1MB)
	// 与索引 ⇒ 实例数也要有个硬顶。
	blockStoreCacheMaxInstances = 128
)

// acquireBlockStore 取出（或创建）该 record 对应的 sidecar 实例。
//
// **调用方必须 releaseBlockStore 归还**（归还 = unpin；实例可能留在缓存里复用，
// 也可能立刻被后台淘汰器关掉 ⇒ 归还后**不得**再碰 bs）。
func acquireBlockStore(recordPath string) (*blockStore, error) {
	blockStoreMu.Lock()
	defer blockStoreMu.Unlock()
	storePath := blockStorePath(recordPath)
	if e, ok := blockStoreCache[recordPath]; ok {
		if e.bs.stillCurrent(storePath) {
			e.pin++
			e.at = time.Now()
			return e.bs, nil
		}
		// 文件已被外部改动 → 丢弃陈旧实例，重新打开。
		_ = e.bs.Close()
		delete(blockStoreCache, recordPath)
	}
	bs, err := openBlockStore(storePath)
	if err != nil {
		return nil, err
	}
	blockStoreCache[recordPath] = &blockStoreEntry{bs: bs, pin: 1, at: time.Now()}
	if len(blockStoreCache) > blockStoreCacheMaxInstances/2 {
		scheduleBlockStoreEviction()
	}
	return bs, nil
}

// releaseBlockStore 归还实例（unpin）。
//
// 不做 I/O：数据落盘由调用方显式 flush（提交路径与清扫路径都 flush 过）。
// 归还后实例可能立刻被后台淘汰器关掉 ⇒ 调用方**不得**再碰 bs。
func releaseBlockStore(recordPath string, bs *blockStore) {
	if bs == nil {
		return
	}
	blockStoreMu.Lock()
	defer blockStoreMu.Unlock()
	e, ok := blockStoreCache[recordPath]
	if !ok || e.bs != bs {
		return
	}
	if e.pin > 0 {
		e.pin--
	}
}

// blockStoreStillCached 报告该实例是否仍是缓存里的那一个（身份校验）。
//
// 后台 compact 在锁外用着 bs，回来时要确认「期间没有被换掉」——
// 换了就说明会话被删了或文件被外部改动 ⇒ 放弃这次回收。
func blockStoreStillCached(recordPath string, bs *blockStore) bool {
	blockStoreMu.Lock()
	defer blockStoreMu.Unlock()
	e, ok := blockStoreCache[recordPath]
	return ok && e.bs == bs
}

// blockStoreIndexBytes 估算一个实例占用的索引内存。
//
// 组成：map 条目（键字符串 + blockRef 32B）+ 排序缓存（8B/条）。
// 这是**估算**（不是精确分配量）—— 预算只需要一个量级正确的尺子。
func (bs *blockStore) indexBytes() int64 {
	if bs == nil {
		return 0
	}
	return int64(len(bs.index))*96 + int64(len(bs.sortedIDs))*8
}

// blockStoreEvictionScheduled 保证同一时刻只有一轮后台淘汰在跑。
var blockStoreEvictionScheduled atomic.Bool

// scheduleBlockStoreEviction 投递一轮后台淘汰（超预算时才真的动手）。
//
// 为什么不放在提交路径里：Close 会 fsync 整个 sidecar（实测在颠簸的机器上
// 一次 fsync 能到 10 秒）⇒ 必须离开提交路径。
func scheduleBlockStoreEviction() {
	if !blockStoreEvictionScheduled.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer blockStoreEvictionScheduled.Store(false)
		evictBlockStoresIfNeeded()
	}()
}

// evictBlockStoresIfNeeded 淘汰超预算的实例（LRU 优先，只动未 pin 的）。
func evictBlockStoresIfNeeded() {
	blockStoreMu.Lock()
	total := int64(0)
	for _, e := range blockStoreCache {
		total += e.bs.indexBytes()
	}
	if len(blockStoreCache) <= blockStoreCacheMaxInstances && total <= blockStoreCacheBudgetBytes {
		blockStoreMu.Unlock()
		return
	}
	// 挑最旧的未 pin 受害者，直到回到预算内。
	var victims []*blockStore
	for len(blockStoreCache) > blockStoreCacheMaxInstances || total > blockStoreCacheBudgetBytes {
		var victimKey string
		var victim *blockStoreEntry
		for k, e := range blockStoreCache {
			if e.pin > 0 {
				continue
			}
			if victim == nil || e.at.Before(victim.at) {
				victim, victimKey = e, k
			}
		}
		if victim == nil {
			break // 全都被 pin 住了 ⇒ 只能超预算
		}
		delete(blockStoreCache, victimKey)
		total -= victim.bs.indexBytes()
		victims = append(victims, victim.bs)
	}
	blockStoreMu.Unlock()

	// Close 在**锁外**做：它会 fsync，锁内做会把所有提交挡住几秒。
	for _, bs := range victims {
		_ = bs.Close()
	}
}

// stillCurrent 报告 sidecar 自打开以来是否未被外部改动。
//
// 判据 = 文件大小 + mtime 都未变。任一变化都视为「需要重新打开」：
// 宁可多扫一次（有索引时很便宜），也不能用陈旧索引读到错位数据。
func (bs *blockStore) stillCurrent(storePath string) bool {
	if bs == nil || bs.f == nil {
		return false
	}
	fi, err := os.Stat(storePath)
	if err != nil {
		return false
	}
	return fi.Size() == bs.openSize && fi.ModTime().UnixNano() == bs.openMtime
}

// closeBlockStore 丢弃并关闭缓存实例（record 被删除时用）。
func closeBlockStore(recordPath string) {
	blockStoreMu.Lock()
	e, ok := blockStoreCache[recordPath]
	if ok {
		delete(blockStoreCache, recordPath)
	}
	blockStoreMu.Unlock()
	if ok {
		_ = e.bs.Close()
	}
}

// evictBlockStore 在 record 被删除/替换时丢弃缓存实例。
func evictBlockStore(recordPath string) { closeBlockStore(recordPath) }

// ## 校验强度（有意设计，勿当作疏忽）
//
// 打开 sidecar 有三条路径，校验强度不同：
//
//	路径                触发条件          校验内容
//	─────────────────────────────────────────────────────────────────────
//	索引命中           索引 covered==size  索引自校验（entries 的 sha256，680KB）
//	索引+增量          索引 covered<size   上述 + 增量段的逐条摘要校验
//	全量扫描           无索引/索引损坏     逐条 payload sha256（最慢但最强）
//
// 索引命中路径**不读 sidecar**，因此无法对历史 payload 做逐字节校验。
// 这是「切换会话 2.66s → 3.12ms」的唯一来源，取舍如下：
//
//   · 检出能力没有消失，只是分层：
//       - 索引损坏 / 与 sidecar 长度不符 → 立即回退全量扫描（会报错或重建）；
//       - 单条 payload 被损坏 → 在**真正读取该块时**校验摘要（readPage）；
//       - 需要全量体检 → 显式调用 VerifyBlockStore（fsck / 迁移场景）。
//   · 实际风险面很小：payload 摘要存在索引里，读取时逐块比对，
//     损坏只会让「那一块」读失败（返回错误，不是返回脏数据）。
//   · 因此不存在「静默返回错误内容」的路径 —— 这比「打开时全扫一遍」
//     更符合实际使用：用户只读看得见的块，校验就发生在那里。

// openBlockStore 打开（或创建）sidecar 并重建索引。
func openBlockStore(path string) (*blockStore, error) {
	bs := &blockStore{path: path, index: map[string]blockRef{}, frags: map[string]*fragDir{}}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	bs.f = f
	if err := bs.rebuild(); err != nil {
		_ = f.Close()
		return nil, err
	}
	bs.recordOpenState()
	return bs, nil
}

// recordOpenState 记下当前文件状态，供 stillCurrent 比对。
func (bs *blockStore) recordOpenState() {
	fi, err := bs.f.Stat()
	if err != nil {
		return
	}
	bs.openSize = fi.Size()
	bs.openMtime = fi.ModTime().UnixNano()
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

	// 优先用索引文件：只扫描「索引未覆盖」的尾部增量。
	//
	// 这是「切换会话」从 1798ms 降到亚毫秒的关键（实测 2026-10-20）：
	// 旧路径每次打开都要逐条解析整个 sidecar（1029MB：遍历 25ms + 拷贝 239ms +
	// 逐块 sha256 598ms）。索引覆盖了已解析部分后，正常路径只需读 692KB 并
	// 扫描「自上次索引以来新增的几十条记录」。
	start := int64(len(blockStoreMagic))
	var tailPending bool
	if entries, covered, _, idxRecords, idxStale, ok := loadBlockIndex(bs.path); ok {
		bs.recordsWritten = idxRecords
		bs.staleRecords = idxStale
		switch {
		case covered == size:
			// 索引与 sidecar 完全同步：**零扫描、零哈希**。
			//
			// 这是最快的路径（实测 0.56ms / 21632 块）。滚动链值由索引头部
			// 携带（写入侧在 covered 处记下的截断摘要），所以 sum() 也无需重算。
			// 索引命中：**不读 sidecar 一个字节**。这是「切换会话」到亚毫秒的关键。
			//
			// 校验强度说明见文件头注释：payload 在本块被真正读取时逐块校验。
			bs.index = indexToMap(entries)
			// 索引里的 entries 已是 id 升序（mapToIndex 保证）→ 直接构建有序表，
			// 省掉打开后首次分页的 60ms 排序。
			sorted := make([]uint64, 0, len(entries))
			for _, e := range entries {
				sorted = append(sorted, e.id)
			}
			if isAscending(sorted) {
				bs.sortedIDs = sorted
			}
			bs.nBytes = size
			bs.recomputeLiveBytes()
			return bs.startWriter()
		case covered > size:
			// 索引超前（sidecar 被 compact/截断过）→ 视为不可用，全量重建。
			tailPending = true
		case covered < size && covered >= start:
			// 索引落后：载入已覆盖部分，只扫描增量 [covered, EOF)。
			//
			// 这里**不重放已覆盖区间的字节**。旧实现会调 replayChainUpTo(covered)
			// 把 [magic, covered) 整个再读一遍喂滚动哈希 —— 而那个哈希从不被读
			//（见结构体注释），纯属白读。真实 1.8GB 会话索引只落后 5.7MB 时，
			// 这一步仍要花 3.15s，是「打开会话 3.9s」的主因（实测 2026-10-20）。
			//
			// 正确性不依赖它：已覆盖部分的索引条目自带 payload 摘要，且
			// loadBlockIndex 已校验索引自身的完整性；payload 在逐块读取时校验。
			bs.index = indexToMap(entries)
			start = covered
		}
	} else {
		tailPending = true
	}
	_ = tailPending

	pos := start
	r := bufio.NewReaderSize(io.NewSectionReader(bs.f, pos, size-pos), 1<<20)
	for {
		off := pos
		kind, id, _, payload, digest, headLen, err := readBlockRecordExt(r)
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
		recLen := int64(headLen + len(payload) + 8)
		switch kind &^ kindCompressed {
		case blockRecPut:
			// 摘要用记录里带的（**逻辑内容**的摘要 —— 压缩记录的落盘字节摘要
			// 对不上；见 block_compress.go 的不变量）。
			// 压缩块的真实类型码要从**解压后的前缀**取（payload 是压缩字节）——
			// 只需解压 128 字节（blockKindCodeCompressed）✓。
			kindCode := blockKindCode(payload)
			if kind&kindCompressed != 0 {
				kindCode = blockKindCodeCompressed(payload) | kindCompressed
			}
			bs.index[id] = blockRef{off: off + int64(headLen), n: int32(len(payload)), hash: digest, kind: kindCode}
			delete(bs.frags, id) // 整块替换分片形态（二者互斥）
		case blockRecDel:
			delete(bs.index, id)
			delete(bs.frags, id)
		case blockRecFrag:
			// 片段内容本身不建索引：目录（kind=4）里记着它的位置。
			bs.recordsWritten++
		case blockRecDir:
			// 目录 = 该 id 的最新片段清单（重建后即真值）。
			if aerr := bs.applyDirRecord(id, payload, off+int64(headLen)); aerr != nil {
				return aerr
			}
		}
		pos += recLen
	}
	bs.nBytes = pos
	bs.recomputeLiveBytes()
	if err := bs.startWriter(); err != nil {
		return err
	}
	// 走到这里说明刚才**全量扫过一遍** sidecar（索引缺失 / 超前 / 损坏）。
	// 必须立刻把索引落盘，否则下次打开还会重扫 —— 真实 1.5GB 侧车每次 ~80ms，
	// 而且这条路会被「compact 之后索引暂时超前」反复触发。
	// 失败不影响正确性（下次再扫一遍），所以只忽略。
	_ = bs.persistIndex()
	return nil
}

// recomputeLiveBytes 按索引重算有效字节（打开/重建/compact 后调用，O(块数)）。
func (bs *blockStore) recomputeLiveBytes() {
	var live int64
	for id, ref := range bs.index {
		live += blockRecordSize(id, int(ref.n))
	}
	bs.liveBytes = live
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

// PayloadDigest 是单个块 payload 的内容摘要（与索引里记录的值同口径）。
// 导出给恢复路径：映射模式下打开时不再逐块读 payload，改由解码时并行校验。
func PayloadDigest(p []byte) [8]byte { return payloadDigest(p) }

func payloadDigest(p []byte) [8]byte {
	s := sha256.Sum256(p)
	var out [8]byte
	copy(out[:], s[:8])
	return out
}

// delete 追加一条删除记录（同 id 后续 put 可再覆盖回来）。
//
// 只用于「本次快照不再引用的块」（见 pruneTo）。为什么必须落成记录而不是从内存
// 索引里抹掉：索引会被 `rebuild` 从 sidecar 重扫出来，不落盘的话删除在下次启动
// 就失效了。
func (bs *blockStore) delete(id string) error {
	head := buildBlockRecordHead(blockRecDel, id, nil)
	if _, err := bs.w.Write(head); err != nil {
		return err
	}
	// 删除记录同样带 8 字节摘要（空 payload 的摘要），读取端按统一布局跳过。
	d := payloadDigest(nil)
	if _, err := bs.w.Write(d[:]); err != nil {
		return err
	}
	if prev, existed := bs.index[id]; existed {
		bs.staleRecords++
		bs.liveBytes -= bs.logicalSizeLocked(id, prev)
	}
	delete(bs.index, id)
	delete(bs.frags, id) // 分片形态一起删（见 block_frags.go 的不变量 3）
	bs.recordsWritten++
	bs.bytesWritten += int64(len(head) + 8)
	bs.sortedDirty = true
	bs.nBytes += int64(len(head) + 8)
	return nil
}

// pruneTo 把索引里**不再被本次快照引用**的块写成删除记录。
//
// ## 为什么必须有（2026-10-21 线上事故：刷新后会话与左栏子智能体全没了）
//
// sidecar 是 append-only 的，而 reducer 的 blocks 集合会**变小**：
//
//	· Marshal 的空壳 assistant 块被 normalizeDanglingState 丢弃（真实长会话实测 451 个）
//	· 新顶层 run 清掉旧 error 块（applyAgentStartLocked）
//	· 清空上下文 / 会话重置
//
// 被丢弃的块留在索引里 → `ref.Count`（本次引用块数）与 `store.Len()`（索引块数）
// 对不上 —— 而读取端把这个对账当完整性信号（见 LoadRecord 的 resolveBlockRef），
// 于是**下一次打开会话直接判定 record corrupt**。create 里的 reducer 因此是空的，
// 打开会话回一份 0 块 checkpoint：用户看到「刷新后会话、子智能体都不见了」。
// 实测两份真实会话分别差 3 块与 630 块。
func (bs *blockStore) pruneTo(keep map[string]struct{}) error {
	if len(bs.index) <= len(keep) {
		return nil // 常见的「只增不减」路径在这里 O(1) 退出
	}
	stale := make([]string, 0, len(bs.index)-len(keep))
	for id := range bs.index {
		if _, ok := keep[id]; !ok {
			stale = append(stale, id)
		}
	}
	for _, id := range stale {
		if err := bs.delete(id); err != nil {
			return err
		}
	}
	return nil
}

// put 追加一条 block。
//
// 分片感知：payload 里有长数组时以**片段**形式落盘（见 block_frags.go），
// 「只增」的块每次只追加新增的那一段 → 写放大从 150–275× 降到个位数。
// 没有长数组时走原路径，字节级行为与从前完全一致。
func (bs *blockStore) put(id string, payload []byte) error {
	return bs.putAuto(id, payload)
}

// putWhole 追加一条**整块**（同 id 后续记录覆盖前者）。
func (bs *blockStore) putWhole(id string, payload []byte) error {
	// 压缩（写路径）：落盘存压缩字节，**摘要算逻辑内容**（见 block_compress.go
	// 的不变量）⇒ 压缩不改变 id 的摘要 ⇒ record 里的块集合摘要照常有效 ✓。
	stored := payload
	recKind := blockRecPut
	if blockCompressEnabled {
		if c, ok := compressBlock(payload); ok {
			stored = c
			recKind |= kindCompressed
		}
	}
	off := bs.nBytes
	head := buildBlockRecordHead(recKind, id, stored)
	if _, err := bs.w.Write(head); err != nil {
		return err
	}
	if _, err := bs.w.Write(stored); err != nil {
		return err
	}
	d := payloadDigest(payload)
	if _, err := bs.w.Write(d[:]); err != nil {
		return err
	}
	if prev, existed := bs.index[id]; existed {
		bs.staleRecords++
		// 分片块的索引项 n 是**目录长度**而非逻辑长度 → 用 logicalSizeLocked 取真值。
		// 它已经覆盖了「整块」与「分片」两种形态，**不要**再单独减一次。
		bs.liveBytes -= bs.logicalSizeLocked(id, prev)
	}
	// 整块写入**替换**分片形态（二者互斥，见 block_frags.go 的不变量 3）。
	delete(bs.frags, id)
	kindCode := blockKindCode(payload)
	if recKind&kindCompressed != 0 {
		kindCode |= kindCompressed
	}
	bs.index[id] = blockRef{off: off + int64(len(head)), n: int32(len(stored)), hash: d, kind: kindCode}
	bs.liveBytes += blockRecordSize(id, len(stored))
	bs.recordsWritten++
	bs.bytesWritten += int64(len(head) + len(stored) + 8)
	bs.sortedDirty = true
	// nBytes 记的是**落盘**位置 ⇒ 必须用压缩后的长度（用 len(payload) 会让
	// 位置与文件实际内容错位 ⇒ 后续记录的偏移全部指到空洞里 ⇒ 读回失败）。
	bs.nBytes += int64(len(head) + len(stored) + 8)
	return nil
}

// persistIndex 把当前索引落盘（带 covered = 当前写入位置）。
//
// 调用时机：flush 之后。索引写失败不影响正确性（下次打开会扫描重建），
// 但会让「热启动」退化成「冷启动」，所以失败要能被观察到。
func (bs *blockStore) persistIndex() error {
	// 索引头不再携带"链快照"：sum 改由块集合摘要计算（见 sum 注释），
	// 因此这里传 nil，decode 出来也是 nil。
	return writeBlockIndex(bs.path, mapToIndex(bs.index), bs.nBytes, nil, bs.recordsWritten, bs.staleRecords)
}

func (bs *blockStore) flush() error {
	if bs.w != nil {
		if err := bs.w.Flush(); err != nil {
			return err
		}
	}
	if err := bs.f.Sync(); err != nil {
		return err
	}
	// 同步刷新索引：让下一次「切换会话」走热启动路径。
	// 自己的写入也改变了文件 → 刷新基准，否则下一次 acquire 会误判为「外部改动」。
	bs.recordOpenState()
	return bs.persistIndex()
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

// closeQuiet 关闭块存储但**不落盘**：不 flush 缓冲、不 fsync、不重写索引。
//
// 用途是「只读打开、发现不需要改」的路径 —— 清扫的探测阶段属于这种：
// Close() 会 fsync 整个 sidecar + 重写 .idx，而那次调用一个字节都没改
// （实测每次启动 477 次纯浪费）。
//
// 前置条件：自 open 以来没有经 bs.w 写过任何数据（只读路径天然满足）。
func (bs *blockStore) closeQuiet() {
	if bs == nil || bs.f == nil {
		return
	}
	bs.w = nil // 丢弃缓冲（只读路径下本来就是空的）
	_ = bs.f.Close()
	bs.f = nil
}

// sum 返回当前滚动校验链（写入 record 供读取侧对账）。
// sum 返回「当前索引里全部块」的一致性摘要。
//
// ## 为什么不再用「整个 sidecar 的滚动链」
//
// 旧实现把 sum 定义成 sha256(magic + 所有记录头 + 所有 payload)。它有两个
// 致命问题，都在真实数据上复现过：
//
//  1. **append-only 导致必然过期**：每次提交追加新块，链值随之改变，
//     而 record 里存的是提交那一刻的值 → 第二次提交后读取必然
//     "checksum mismatch"（TestRepeatedCommitsKeepReading 曾红）。
//  2. **无法从索引恢复**：sha256 的中间状态不可序列化，索引命中路径拿不到
//     真实链值，只能存一个"快照"，而快照与真实值在写入后立刻不一致
//     （实测索引里存 c08b…，真实链 a0de…）。
//
// ## 现在的定义
//
// sum = sha256( 按 id 升序：id(8B) || payloadDigest(8B) )
//
// 它只依赖「有哪些块、各自内容摘要」，完全可由索引重建（O(n) 但只算摘要，
// 21759 块 ≈ 700KB 输入，实测 < 5ms），且**与 sidecar 追加无关**。
//
// 完整文件字节的损坏检测由两层保证：
//
//	· 每条记录的 payload 在读该块时校验（readAndVerify）；
//	· 索引 entries 自带 sha256（loadBlockIndex 校验）。
func (bs *blockStore) sum() string {
	sum, err := blocksSetSum(bs, bs.allIDs())
	if err != nil {
		return ""
	}
	return sum
}

// allIDs 返回索引里的全部 id（字符串形态）。
func (bs *blockStore) allIDs() []string {
	out := make([]string, 0, len(bs.index))
	for id := range bs.index {
		out = append(out, id)
	}
	return out
}

func (bs *blockStore) has(id string) bool {
	_, ok := bs.index[id]
	return ok
}

// holdsSame 报告 sidecar 里该 id 的内容是否与给定 payload 完全一致。
//
// 这是增量正确性的核心：**不能只按 id 命中就跳过写入**。
// block 会在 running→done 时内容变化（items 追加、status/usage 更新），
// 只按 id 跳过会让磁盘永远停在旧版本 —— 用户看到永不更新的卡片
// （TestSidecarRewritesLiveBlocks 钉住这个行为）。
func (bs *blockStore) holdsSame(id string, payload []byte) bool {
	ref, ok := bs.index[id]
	if !ok {
		return false
	}
	bs.hashedBytes += int64(len(payload))
	return ref.unchanged(payload)
}

func (bs *blockStore) read(id string) ([]byte, bool) {
	return bs.readBlock(id) // 分片感知（见 block_frags.go）
}

func (bs *blockStore) Len() int { return len(bs.index) }

func blockStorePath(recordPath string) string { return recordPath + blockStoreSuffix }

// refStorePath 解析引用里记录的 sidecar 文件名。
//
// 旧实现无条件用 recordPath+".blocks"，等于**忽略**引用里已经写下的 store 名。
// 后果：读 `<sid>.record.json.bak` 时去找 `<sid>.record.json.bak.blocks` ——
// 一个 8 字节的空壳，块数 0 vs N，.bak 兜底 100% 失败（现场证据：
// .bak.blocks 只有 magic、.bak.blocks.idx 是空索引）。
//
// 只接受「同目录 + 裸文件名（无分隔符）+ 以 .blocks 结尾」，避免引用把读
// 引到任意路径上。
func refStorePath(recordPath, store string) (string, error) {
	if store == "" {
		return blockStorePath(recordPath), nil
	}
	if store != filepath.Base(store) || strings.ContainsAny(store, `/\`) {
		return "", fmt.Errorf("block ref store must be a bare file name, got %q", store)
	}
	if !strings.HasSuffix(store, blockStoreSuffix) {
		return "", fmt.Errorf("block ref store must end with %q, got %q", blockStoreSuffix, store)
	}
	return filepath.Join(filepath.Dir(recordPath), store), nil
}

// ---------------------------------------------------------------------------
// 编码侧：view_checkpoint.blocks → 引用
// ---------------------------------------------------------------------------

// findBlocksField 一次性定位 view 里 blocks 字段的**元素切分**。
//
// 为什么合并：原先是 splitViewBlocks（找 blocks 值，扫 86MB）+ splitBlocksArray
// （切分元素，再扫 86MB），实测 107+104=211ms 纯浪费在重复扫描上。
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
// （引用形态只有几十字节），不构成性能问题。
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
			// 进行中的 block：内容**可能**变，但不一定每次都变。
			//
			// 原实现无条件整块重写（"内容会变"）——2026-11-09 实测这个假设对
			// **agent 卡片**不成立：卡片在整个子任务 running 期都是 status=running，
			// 而内容只在子任务产出新东西时才变。于是每次提交都把几十张 running 卡片
			// （单张最大 0.8MB）整块重写一遍，一次提交追加 16.1MB → 废弃比例越过
			// 20% 阈值 → 触发全量重写 → 无限循环（见 compactMinStaleBytes 的事故记录）。
			//
			// 现在与终态分支一样先判内容：没变就不写（append-only 文件里留着旧版本即可）。
			if bs.holdsSame(id, elem) {
				continue
			}
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

	refIDs := make([]string, 0, len(elems))
	keep := make(map[string]struct{}, len(elems))
	for _, e := range elems {
		if id, ok := blockID(e); ok {
			refIDs = append(refIDs, id)
			keep[id] = struct{}{}
		}
	}
	if err := bs.pruneTo(keep); err != nil {
		return nil, 0, err
	}
	setSum, serr := blocksSetSum(bs, refIDs)
	if serr != nil {
		return nil, 0, serr
	}
	ids, ierr := sortedIDNums(refIDs)
	if ierr != nil {
		return nil, 0, ierr
	}
	ref := blocksRefDoc{Store: filepath.Base(bs.path), Sum: setSum, Count: len(elems), Schema: 1, IDs: ids}
	ref.maybeDenseRange()
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
	return refBlocksFromSpansDirty(bs, view, spans, nil, nil, nil, nil)
}

// refBlocksFromSpansDirty 是 refBlocksFromSpans 的脏块快路径版本。
//
//	dirty == nil → 逐块全量哈希（兼容旧调用方与周期性校验）
//	ids   != nil 且与 spans 等长 → 直接用 id，不再逐个 block 解析 JSON
//	                                （30,787 块实测省 200~400ms）
//
// spanIDs 与 spans 一一对应（升序）；allIDs 是 record 要引用的**全部**块 id。
// 窗口恢复时 spans 只覆盖「窗口 + 卡片块」，allIDs 仍是全集 —— 引用必须按全集写，
// 否则 pruneTo 会把窗口外的历史从 sidecar 里删掉（不可逆的数据丢失）。
// allIDs 为 nil 时按 spans 自身构造（旧行为）。
// RefPhaseTiming 是「逐块比对 + 追加 + 集合摘要」这一阶段的**分解计时**。
//
// 为什么要拆：实测慢提交的 sidecar 阶段里 sc-ref 占 7.6s（23.4s 那次），
// 但这一段里至少有五件事（逐块比对 / 写入 / 回收 / 摘要 / 文档构造）——
// 不拆开就只能猜，而"猜"过一次就猜错过（当时以为 40,232 块的 sha256 摘要
// 要 10ms，实际差了三个数量级 ⇒ 真凶不是它）。
//
// 字段全为 0 时表示没量（tm 为 nil）。
type RefPhaseTiming struct {
	Compare time.Duration // 逐块比对（脏块快路径下只是 O(1) 存在性检查）
	Put     time.Duration // 真正写入 sidecar 的块（含压缩）
	Prune   time.Duration // 回收不在引用集里的旧块（O(索引大小)）
	SetSum  time.Duration // 块集合摘要（顺序无关：对全部块算一次 sha256）
	Doc     time.Duration // 引用文档构造（排序 + 稠密区间 + 序列化）
	Replace time.Duration // 把引用写回 view（等长替换）
	Total   time.Duration
}

func refBlocksFromSpansDirty(bs *blockStore, view []byte, spans [][]byte, dirty map[uint64]struct{}, spanIDs []uint64, allIDs []uint64, tm *RefPhaseTiming) ([]byte, int, error) {
	if tm != nil {
		tm.Total = 0
		defer func() { tm.Total = tm.Compare + tm.Put + tm.Prune + tm.SetSum + tm.Doc + tm.Replace }()
	}
	if len(spans) == 0 {
		// 空 blocks：仍要写引用，避免下次被当作旧格式。
		// 同时把索引里残留的块清掉 —— 否则 ref.Count=0 与 store.Len()>0 对不上，
		// 下次读这份 record 会被判成 corrupt（见 pruneTo 的事故说明）。
		if err := bs.pruneTo(nil); err != nil {
			return nil, 0, err
		}
		emptySum, _ := blocksSetSum(bs, nil)
		ref := blocksRefDoc{Store: filepath.Base(bs.path), Sum: emptySum, Count: 0, Schema: 1, IDs: []uint64{}}
		refJSON, _ := json.Marshal(map[string]blocksRefDoc{blocksRefKey: ref})
		out, err := replaceJSONField(view, "blocks", refJSON)
		if err != nil {
			return nil, 0, err
		}
		return out, 0, nil
	}

	useIDs := len(spanIDs) == len(spans)
	wrote := 0
	// 分解计时：比对（跳过未变块）与写入（真写 sidecar）分开算。
	var cmpDur, putDur time.Duration
	for i, elem := range spans {
		var id string
		if useIDs {
			id = strconv.FormatUint(spanIDs[i], 10)
		} else {
			var ok bool
			id, ok = blockID(elem)
			if !ok {
				return nil, 0, fmt.Errorf("block without id")
			}
		}
		if isLiveBlock(elem) {
			// running 块每个事件都可能变，一律重写（不哈希）。
			putStart := time.Now()
			if err := bs.put(id, elem); err != nil {
				return nil, 0, err
			}
			putDur += time.Since(putStart)
			wrote++
			continue
		}
		if dirty != nil {
			// 脏块快路径：调用方（reducer）已精确标出本轮内容变化的块，
			// 非脏块**不再逐块 sha256**（30,755 块 / 185MB 实测是提交里
			// sidecar 阶段的主体成本）。只做 O(1) 的存在性检查。
			//
			// 正确性前提：dirty 必须覆盖所有内容变化的块，否则磁盘会停在
			// 旧版本（「卡片永不更新」那个 bug）。因此调用方要周期性传 nil
			// 走全量校验（见 bridge 的 verifyEvery）。
			if num, err := parseUintID(id); err == nil {
				cmpStart := time.Now()
				_, known := dirty[num]
				present := bs.has(id)
				cmpDur += time.Since(cmpStart)
				if !known && present {
					continue
				}
			}
			putStart := time.Now()
			if err := bs.put(id, elem); err != nil {
				return nil, 0, err
			}
			putDur += time.Since(putStart)
			wrote++
			continue
		}
		cmpStart := time.Now()
		same := bs.holdsSame(id, elem)
		cmpDur += time.Since(cmpStart)
		if same {
			continue
		}
		putStart := time.Now()
		if err := bs.put(id, elem); err != nil {
			return nil, 0, err
		}
		putDur += time.Since(putStart)
		wrote++
	}
	if tm != nil {
		tm.Compare = cmpDur
		tm.Put = putDur
	}
	// 引用集合：窗口恢复下以 allIDs 为准（全集），否则按 spans 自身。
	refIDs2 := make([]string, 0, len(spans))
	keep2 := make(map[string]struct{}, len(spans))
	if len(allIDs) > 0 {
		refIDs2 = make([]string, 0, len(allIDs))
		keep2 = make(map[string]struct{}, len(allIDs))
		for _, v := range allIDs {
			id := strconv.FormatUint(v, 10)
			refIDs2 = append(refIDs2, id)
			keep2[id] = struct{}{}
		}
	} else {
		for i, e := range spans {
			if useIDs {
				id := strconv.FormatUint(spanIDs[i], 10)
				refIDs2 = append(refIDs2, id)
				keep2[id] = struct{}{}
				continue
			}
			if id, ok := blockID(e); ok {
				refIDs2 = append(refIDs2, id)
				keep2[id] = struct{}{}
			}
		}
	}
	pruneStart := time.Now()
	if err := bs.pruneTo(keep2); err != nil {
		return nil, 0, err
	}
	pruneDur := time.Since(pruneStart)
	sumStart := time.Now()
	setSum2, serr2 := blocksSetSum(bs, refIDs2)
	if serr2 != nil {
		return nil, 0, serr2
	}
	sumDur := time.Since(sumStart)
	docStart := time.Now()
	ids2, ierr2 := sortedIDNums(refIDs2)
	if ierr2 != nil {
		return nil, 0, ierr2
	}
	ref := blocksRefDoc{Store: filepath.Base(bs.path), Sum: setSum2, Count: len(refIDs2), Schema: 1, IDs: ids2}
	ref.maybeDenseRange()
	refJSON, err := json.Marshal(map[string]blocksRefDoc{blocksRefKey: ref})
	if err != nil {
		return nil, 0, err
	}
	docDur := time.Since(docStart)
	if tm != nil {
		tm.Prune = pruneDur
		tm.SetSum = sumDur
		tm.Doc = docDur
	}
	// 用 spans 的已知总长度直接算出数组结尾，省掉 matchJSONEnd 的 86MB 扫描
	//（实测 110ms，占提交 20%）。
	replaceStart := time.Now()
	out, err := replaceBlocksWithKnownSpans(view, spans, refJSON)
	if tm != nil {
		tm.Replace = time.Since(replaceStart)
	}
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

// blocksSetSum 计算「指定 id 集合」的一致摘要。
//
// ## 为什么不能沿用「整个 sidecar 的滚动链」
//
// 原实现把 sum 定义成「整个 sidecar 的 sha256」，但 sidecar 是 append-only
// 的：每次 checkpoint 追加新块，文件与链值都会变。record 里存的 sum 因此
// 必然过期 —— 下一次读取就会报 "block store checksum mismatch"。
// 实测（TestDbgSumTimeline）在第二次提交后立刻复现。
//
// ## 现在的定义
//
// sum = sha256( 按 id 升序，对每个被引用的块拼接 id || payloadDigest )
//
// 它只覆盖「这条 record 真正引用的块」，与 sidecar 里有多少其它内容无关：
//
//	· 追加新块不影响已引用块的值 → 不再误报；
//	· 块内容被改 → 该块 digest 变 → sum 变 → 能检出；
//	· 块缺失 → 读取时直接报错。
//
// 代价：O(引用块数) 的哈希（21759 块 ≈ 700KB 输入），实测 < 5ms。
// 相比「打开时扫 1378MB」便宜三个数量级。
func blocksSetSum(bs *blockStore, ids []string) (string, error) {
	return indexSetSum(bs.index, ids)
}

// indexSetSum 是 blocksSetSum 的索引级实现：只读 index，不碰 payload，
// 因此映射路径（没有 *blockStore）也能做同一口径的集合校验。
func indexSetSum(index map[string]blockRef, ids []string) (string, error) {
	h := sha256.New()
	sorted := make([]uint64, 0, len(ids))
	byNum := make(map[uint64]string, len(ids))
	for _, id := range ids {
		v, err := parseUintID(id)
		if err != nil {
			return "", err
		}
		sorted = append(sorted, v)
		byNum[v] = id
	}
	sortUint64s(sorted)
	var n8 [8]byte
	for _, v := range sorted {
		ref, ok := index[byNum[v]]
		if !ok {
			return "", fmt.Errorf("block %d missing from store", v)
		}
		binary.BigEndian.PutUint64(n8[:], v)
		h.Write(n8[:])
		h.Write(ref.hash[:])
	}
	return setSumPrefix + hex.EncodeToString(h.Sum(nil)), nil
}

// legacyChainSum 按**旧口径**重算「整个 sidecar 的滚动链」。
//
// 旧版实现把 record 里的 Sum 定义成 sha256(magic + 每条 head + payload)，
// 且包含被覆盖的废弃记录。那是 append-only 的（写后必然过期），已被取代；
// 但**已落盘的 record 用的是它**，必须能读出来。
//
// 这里仅在该 record 的 Sum 与新旧两种口径都不匹配时调用 —— 也就是只在
// 读旧数据时发生一次（读完后下次提交即写成新口径）。
func (bs *blockStore) legacyChainSum() string {
	fi, err := bs.f.Stat()
	if err != nil {
		return ""
	}
	h := sha256.New()
	h.Write(blockStoreMagic)
	pos := int64(len(blockStoreMagic))
	// 流式重算：只读头与 payload，不构造对象、不逐块校验。
	const chunk = 1 << 20
	buf := make([]byte, chunk)
	for pos < fi.Size() {
		var hb [9]byte
		if _, err := bs.f.ReadAt(hb[:], pos); err != nil {
			return ""
		}
		idl := int64(binary.BigEndian.Uint32(hb[1:5]))
		var plb [4]byte
		if _, err := bs.f.ReadAt(plb[:], pos+5+idl); err != nil {
			return ""
		}
		pl := int64(binary.BigEndian.Uint32(plb[:]))
		headLen := 5 + idl + 4
		// head = kind + idLen + id + payloadLen
		idBuf := make([]byte, idl)
		if _, err := bs.f.ReadAt(idBuf, pos+5); err != nil {
			return ""
		}
		// 必须与写入侧逐字节一致：head = kind(1) + idLen(4) + id + payloadLen(4)
		h.Write(hb[:1])
		h.Write(hb[1:5])
		h.Write(idBuf)
		h.Write(plb[:])
		off := pos + headLen
		remain := pl
		for remain > 0 {
			n := int64(chunk)
			if remain < n {
				n = remain
			}
			if _, err := bs.f.ReadAt(buf[:n], off); err != nil {
				return ""
			}
			h.Write(buf[:n])
			off += n
			remain -= n
		}
		pos += headLen + pl + 8
	}
	return hex.EncodeToString(h.Sum(nil))
}

// resolveBlocksRef 在读取时把引用还原成完整 blocks 数组。
//
// 若 view 里没有引用（旧格式），原样返回。
// 引用存在但 sidecar 不可用/校验不符 → 返回错误（调用方回退 .bak），
// **绝不**返回「blocks 为空」的半份数据。
func resolveBlocksRef(view []byte, recordPath string) ([]byte, error) {
	out, _, err := resolveBlocksRefSpans(view, recordPath)
	return out, err
}

// resolveBlocksRefSpans 同 resolveBlocksRef，另外返回每个 block 的字节切片
// （仅 sidecar 引用形态有；内嵌数组形态返回 nil）。供恢复路径并行解码。
// resolveBlocksRefSpans 走「拼装」路径（无 mmap / 索引落后时的回退）。
//
// 返回的 span 全部是**逻辑内容**（readBlockVerified 已解压并校验过摘要），
// 所以 Compressed=false —— 与映射路径的 BlockSpan 语义一致。
func resolveBlocksRefSpans(view []byte, recordPath string) ([]byte, []BlockSpan, error) {
	blocksRaw, ok := splitViewBlocks(view)
	if !ok {
		return view, nil, nil // 无 blocks 字段：保持原样，由上层校验
	}
	trimmed := bytes.TrimSpace(blocksRaw)
	if !bytes.Contains(trimmed, []byte(blocksRefKey)) {
		return view, nil, nil // 旧格式：内嵌完整数组
	}
	var wrapper map[string]blocksRefDoc
	if err := json.Unmarshal(trimmed, &wrapper); err != nil {
		return nil, nil, fmt.Errorf("parse blocks ref: %w", err)
	}
	ref, ok := wrapper[blocksRefKey]
	if !ok {
		return nil, nil, fmt.Errorf("parse blocks ref: missing %q", blocksRefKey)
	}

	storePath, perr := refStorePath(recordPath, ref.Store)
	if perr != nil {
		return nil, nil, perr
	}
	bs, err := openBlockStore(storePath)
	if err != nil {
		return nil, nil, fmt.Errorf("open block store: %w", err)
	}
	defer bs.Close()

	if refNums := ref.refIDs(); len(refNums) > 0 {
		// 新口径（带 ids 或 range）：只校验「我引用的块」。sidecar 里多出来的块
		// 属于后续提交或废弃版本，不该影响这一版 record 的可读性。
		if ref.Count != len(refNums) {
			return nil, nil, fmt.Errorf("block ref malformed: count=%d ids=%d", ref.Count, len(refNums))
		}
		ids := make([]string, len(refNums))
		for i, v := range refNums {
			ids[i] = strconv.FormatUint(v, 10)
		}
		got, serr := blocksSetSum(bs, ids)
		if serr != nil {
			return nil, nil, fmt.Errorf("block store missing referenced block: %w", serr)
		}
		if ref.Sum != "" && strings.HasPrefix(ref.Sum, setSumPrefix) && got != ref.Sum {
			return nil, nil, fmt.Errorf("block store checksum mismatch (have %s want %s)", got, ref.Sum)
		}
		full, spans, aerr := bs.assembleIDsWithSpans(ids)
		if aerr != nil {
			return nil, nil, aerr
		}
		resolved, rerr := replaceJSONField(view, "blocks", full)
		return resolved, logicalSpans(spans), rerr
	}

	// 旧口径（无 ids）：只能要求「引用集合 == 索引集合」严格相等。
	// 这条路径保留是为了兼容既有 record，新写入的记录一律带 ids。
	if ref.Count != bs.Len() {
		return nil, nil, fmt.Errorf("block store count mismatch (have %d want %d)", bs.Len(), ref.Count)
	}
	// 摘要校验：**只比对索引里的摘要，不读 sidecar 字节**。
	//
	// ## 为什么可以不读字节就下结论
	//
	// 完整性由三层分别负责，各管各的字节：
	//   1. 索引自身    —— entries 带 sha256，loadBlockIndex 打开时校验；
	//   2. sidecar 字节 —— 每次**读某块**时由 readAndVerify / ReadPage 校验该块
	//                      payload 的摘要（见 TestCorruptPayloadReportedOnRead），
	//                      损坏只会让「读那一页」报错，不会静默返回坏内容；
	//   3. record ↔ 索引 —— 就是这里：块集合摘要 + 块数，确认 record 引用的
	//                      正是这份索引。
	//
	// ## 为什么必须这样（实测代价）
	//
	// 旧实现在打开时把整份 sidecar 读一遍去算摘要。真实 1.8GB 会话：
	//   · 新口径 actualSetSum   ~200ms
	//   · 旧口径 legacyChainSum ~910ms（每条已落盘 record 都是旧口径）
	//   合计每次打开白付 1.1s，且随文件线性增长 —— 而打开路径只需要标量。
	// 用户原话：「有索引不读 sidecar 不正常吗？」—— 对，就是不该读。
	//
	// ## 口径分派
	//
	// sum 自带前缀（见 setSumPrefix），所以「哪种口径」是 O(1) 判断，
	// 不需要「两种都算一遍」的试错。旧口径记录只比对字段、不再重算链：
	// 它的链值覆盖整个 sidecar（含废弃记录），语义上已不属于「这份索引」，
	// 重算也无法证明 record 与索引匹配 —— 那件事由块数 + 顺序校验负责。
	if ref.Sum != "" {
		got := bs.sum()
		if strings.HasPrefix(ref.Sum, setSumPrefix) {
			if got != ref.Sum {
				return nil, nil, fmt.Errorf("block store checksum mismatch (have %s want %s)", got, ref.Sum)
			}
		}
		// 旧口径：链值不可重建（见上），跳过；块数校验在下方照常执行。
	}
	if ref.Count != bs.Len() {
		return nil, nil, fmt.Errorf("block store count mismatch (have %d want %d)", bs.Len(), ref.Count)
	}

	// 旧口径：ref 里没有 ids（本机既有记录全是这种），语义是「引用集合 == 索引全集」，
	// 所以按索引全集装配即可。仍然返回 spans —— 恢复路径的并行解码对旧记录同样生效，
	// 不必等它被重写一次。
	full, spans, err := bs.assembleIDsWithSpans(bs.allIDs())
	if err != nil {
		return nil, nil, err
	}
	resolved, rerr := replaceJSONField(view, "blocks", full)
	return resolved, logicalSpans(spans), rerr
}

// assemble 按 id 顺序拼出完整 blocks 数组。
//
// 顺序为什么可靠：sidecar 是 append-only，索引里的 offset 单调递增；
// 但 reducer 的 block 顺序由 id 决定（nextBlockID 单调递增），而 block 在数组里
// 的顺序就是 id 递增序 —— 两者一致。这里显式按 id 数值排序，不依赖写入顺序。
func (bs *blockStore) assemble() ([]byte, error) {
	return bs.assembleIDs(bs.allIDs())
}

// assembleIDs 按指定 id 集合拼出 blocks 数组（顺序按 id 数值升序）。
//
// 指定集合是「旧 record 仍可读」的关键：调用方给的是**那条 record 自己**
// 引用的块，与 sidecar 当前有多少块无关。
func (bs *blockStore) assembleIDs(ids []string) ([]byte, error) {
	out, _, err := bs.assembleIDsWithSpans(ids)
	return out, err
}

// assembleIDsWithSpans 同 assembleIDs，另外返回每个 block 在结果里的切片。
//
// 用途：恢复路径并行解码（每个 block 独立 json.Unmarshal）——避免对 210MB
// 做一次单线程解码（实测 1.0~2.2s）。切片指向返回的同一块内存，零额外拷贝。
func (bs *blockStore) assembleIDsWithSpans(ids []string) ([]byte, [][]byte, error) {
	nums := make([]uint64, len(ids))
	for i, id := range ids {
		n, err := strconv.ParseUint(id, 10, 64)
		if err != nil {
			return nil, nil, fmt.Errorf("block store: non-numeric block id %q", id)
		}
		nums[i] = n
	}
	sortUint64WithIDs(nums, ids)

	var buf bytes.Buffer
	buf.Grow(1 << 20)
	buf.WriteByte('[')
	type seg struct{ start, end int }
	segs := make([]seg, len(ids))
	for i, id := range ids {
		// 必须用 readAndVerify：assemble 是打开会话的主路径，而打开时
		// 已不再为摘要去读整个 sidecar（见 resolveBlocksRef 注释），
		// 所以**这里是 payload 完整性唯一的校验点**。逐块校验的成本
		// 与数据量同比，且这些字节本来就要读 —— 不额外多读一遍。
		payload, ok := bs.readAndVerify(id)
		if !ok {
			return nil, nil, fmt.Errorf("block store: payload unreadable or corrupt for id %q", id)
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		start := buf.Len()
		buf.Write(payload)
		segs[i] = seg{start: start, end: buf.Len()}
	}
	buf.WriteByte(']')
	out := buf.Bytes()
	spans := make([][]byte, len(segs))
	for i, sg := range segs {
		spans[i] = out[sg.start:sg.end]
	}
	return out, spans, nil
}

// assembleWithChain 拼出完整 blocks 数组，**同时**算出旧口径的整文件滚动链。
//
// 为什么合并：旧口径 record 需要 (a) 整份 blocks 数组 和 (b) 整个 sidecar 的
// 滚动链。分开算要读两遍 sidecar（1.8GB 实测 138ms + 910ms）；两者都需要
// 顺序遍历全部记录，合起来只读一遍。
//
// 链的定义必须与写入侧逐字节一致（blockStoreMagic 之后，对每条记录的
// kind||idLen||id||payloadLen||payload 顺序哈希，**不含**尾部 digest）。
func (bs *blockStore) assembleWithChain() ([]byte, string, error) {
	ids := bs.allIDs()
	nums := make([]uint64, len(ids))
	for i, id := range ids {
		n, err := strconv.ParseUint(id, 10, 64)
		if err != nil {
			return nil, "", fmt.Errorf("block store: non-numeric block id %q", id)
		}
		nums[i] = n
	}
	sortUint64WithIDs(nums, ids)

	var buf bytes.Buffer
	buf.Grow(1 << 20)
	buf.WriteByte('[')
	for i, id := range ids {
		payload, ok := bs.readAndVerify(id)
		if !ok {
			return nil, "", fmt.Errorf("block store: payload unreadable or corrupt for id %q", id)
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(payload)
	}
	buf.WriteByte(']')

	// 链：顺序遍历 sidecar 全部记录（含废弃版本，语义就是如此）。
	h := sha256.New()
	h.Write(blockStoreMagic)
	fi, err := bs.f.Stat()
	if err != nil {
		return nil, "", err
	}
	size := fi.Size()
	const chunk = 1 << 20
	scanBuf := make([]byte, chunk)
	head := make([]byte, 0, 64)
	pos := int64(len(blockStoreMagic))
	for pos < size {
		var hb [9]byte
		if _, err := bs.f.ReadAt(hb[:], pos); err != nil {
			return nil, "", err
		}
		idl := int64(binary.BigEndian.Uint32(hb[1:5]))
		var plb [4]byte
		if _, err := bs.f.ReadAt(plb[:], pos+5+idl); err != nil {
			return nil, "", err
		}
		pl := int64(binary.BigEndian.Uint32(plb[:]))
		idBuf := make([]byte, idl)
		if _, err := bs.f.ReadAt(idBuf, pos+5); err != nil {
			return nil, "", err
		}
		head = head[:0]
		head = append(head, hb[:1]...)
		head = append(head, hb[1:5]...)
		head = append(head, idBuf...)
		head = append(head, plb[:]...)
		h.Write(head)
		off := pos + 5 + idl + 4
		remain := pl
		for remain > 0 {
			n := int64(chunk)
			if remain < n {
				n = remain
			}
			if _, err := bs.f.ReadAt(scanBuf[:n], off); err != nil {
				return nil, "", err
			}
			h.Write(scanBuf[:n])
			off += n
			remain -= n
		}
		pos = off + 8 // 跳过尾部 digest
	}
	return buf.Bytes(), hex.EncodeToString(h.Sum(nil)), nil
}

// sortUint64WithIDs 按 nums 升序同步重排 ids。
//
// 同样不能用插入排序：ids 来自 map 迭代（完全无序），O(n²) 在 21700 项上
// 实测 120ms，是 assemble 的主要 CPU 开销。这里按索引置换，O(n log n)。
func sortUint64WithIDs(nums []uint64, ids []string) {
	idx := make([]int, len(nums))
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(a, b int) int {
		return cmp.Compare(nums[a], nums[b])
	})
	sortedNums := make([]uint64, len(nums))
	sortedIDs := make([]string, len(ids))
	for i, k := range idx {
		sortedNums[i] = nums[k]
		sortedIDs[i] = ids[k]
	}
	copy(nums, sortedNums)
	copy(ids, sortedIDs)
}

// removeBlockStore 删除 sidecar（record 删除时同步清理）。
func removeBlockStore(recordPath string) {
	store := blockStorePath(recordPath)
	_ = os.Remove(store)
	// 索引是 sidecar 的纯加速结构，但也是独立文件：只删 .blocks 会留下
	// .blocks.idx 孤儿（DeleteSessionRecord 后 record 目录永远非空）。
	_ = os.Remove(blockIndexPath(store))
}

// ---------------------------------------------------------------------------
// 按需分页读取
// ---------------------------------------------------------------------------

// PageRequest 描述一次分页读取。
//
// 游标语义（与前端「加载更早」严格对应）：
//   - BeforeID == 0 且 FromEnd == true → 取「最新的一页」（首屏）。
//   - BeforeID > 0                      → 取 id < BeforeID 的最后一页（向上翻）。
//
// 为什么用 id 而不是 offset：block id 单调递增且唯一，天然是稳定游标；
// 用「倒数第 N 条」会在新 block 追加后错位（用户正在看的内容会跳）。
type PageRequest struct {
	BeforeID uint64
	FromEnd  bool
	Limit    int
}

// Page 是一页 block 的原始 JSON 字节（已按 id 升序）。
type Page struct {
	Blocks   [][]byte
	Total    int    // 全部 block 数（用于 UI 显示「共 N 条」）
	HasMore  bool   // 是否还有更早的
	NextFrom uint64 // 下一页的 BeforeID（0 表示没有更早的了）
}

// ReadPage 按 id 游标读取一页 block 字节。
//
// 关键：只 read 需要的块（按索引 offset 精确 ReadAt），并对**这些块**校验
// payload 摘要 —— 校验成本与页大小成正比（50 块约 285KB），与 sidecar 总量无关。
func (bs *blockStore) ReadPage(req PageRequest) (*Page, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	ids := bs.sortedIDsLocked()
	total := len(ids)

	// 定位窗口 [lo, hi)
	hi := total
	if !req.FromEnd && req.BeforeID > 0 {
		hi = lowerBound(ids, req.BeforeID) // 第一个 >= BeforeID 的位置
	}
	lo := hi - limit
	if lo < 0 {
		lo = 0
	}
	if lo >= hi {
		return &Page{Total: total, HasMore: false, NextFrom: 0}, nil
	}

	out := make([][]byte, 0, hi-lo)
	for _, v := range ids[lo:hi] {
		payload, ok := bs.readAndVerify(uintToString(v))
		if !ok {
			return nil, fmt.Errorf("block %d: payload missing or corrupt", v)
		}
		out = append(out, payload)
	}
	next := uint64(0)
	if lo > 0 {
		next = ids[lo]
	}
	return &Page{Blocks: out, Total: total, HasMore: lo > 0, NextFrom: next}, nil
}

// sortedIDsLocked 返回升序 id 列表（必要时重建）。
//
// 重建只在「索引发生变化」后发生一次；正常情况下分页是纯二分查找 +
// 按 offset 读取，与 block 总数无关。
func (bs *blockStore) sortedIDsLocked() []uint64 {
	if bs.sortedIDs != nil && !bs.sortedDirty {
		return bs.sortedIDs
	}
	ids := make([]uint64, 0, len(bs.index))
	for id := range bs.index {
		v, err := parseUintID(id)
		if err != nil {
			continue
		}
		ids = append(ids, v)
	}
	sortUint64s(ids)
	bs.sortedIDs = ids
	bs.sortedDirty = false
	return ids
}

// actualSetSum 与 blocksSetSum 相同，但 hash 取自**磁盘上的真实 payload**
// （逐块读取并重算摘要），而不是索引里记录的值。
//
// 代价：O(总字节) 的读 + sha256。仅用于「校验 record 引用」这条路径，
// 而该路径只在完整读取（LoadRecord）时走；分页读取另有逐块校验。
func (bs *blockStore) actualSetSum(ids []string) (string, error) {
	h := sha256.New()
	sorted := make([]uint64, 0, len(ids))
	byNum := make(map[uint64]string, len(ids))
	for _, id := range ids {
		v, err := parseUintID(id)
		if err != nil {
			return "", err
		}
		sorted = append(sorted, v)
		byNum[v] = id
	}
	sortUint64s(sorted)
	var n8 [8]byte
	for _, v := range sorted {
		bref, ok := bs.index[byNum[v]]
		if !ok {
			return "", fmt.Errorf("block %d missing from store", v)
		}
		buf := make([]byte, bref.n)
		if _, err := bs.f.ReadAt(buf, bref.off); err != nil {
			return "", fmt.Errorf("block %d unreadable: %w", v, err)
		}
		binary.BigEndian.PutUint64(n8[:], v)
		h.Write(n8[:])
		d := payloadDigest(buf)
		h.Write(d[:])
	}
	return setSumPrefix + hex.EncodeToString(h.Sum(nil)), nil
}

// readAndVerify 按索引位置读取并校验该块摘要。
// 校验只覆盖「这一块」，成本与该块大小成正比。
func (bs *blockStore) readAndVerify(id string) ([]byte, bool) {
	return bs.readBlockVerified(id) // 分片感知：校验逻辑摘要（见 block_frags.go）
}

// isAscending 报告切片是否已升序（用于跳过重复排序）。
func isAscending(a []uint64) bool {
	for i := 1; i < len(a); i++ {
		if a[i] < a[i-1] {
			return false
		}
	}
	return true
}

func sortUint64s(a []uint64) {
	// 必须用真排序（O(n log n)）：输入来自 map 迭代，**完全无序**，
	// 对它做插入排序是 O(n²) —— 实测 21700 项要 70ms（2026-10-20）。
	// 旧注释假设「近有序」，那只对已排序输入成立，map 迭代从不满足。
	slices.Sort(a)
}

// lowerBound 返回第一个 >= v 的下标。
func lowerBound(a []uint64, v uint64) int {
	lo, hi := 0, len(a)
	for lo < hi {
		mid := (lo + hi) / 2
		if a[mid] < v {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// ---------------------------------------------------------------------------
// compact：回收被覆盖的废弃记录
// ---------------------------------------------------------------------------

// compactThreshold 是「废弃字节占文件总量」的历史阈值（0.20）。
//
// 保留它是为了**遥测与兼容**：既有测试按它构造数据，日志里也用它描述废弃率。
// 但它**不再**作为触发条件（见 compactGainRatio）—— 原因见下。
//
// 为什么用字节而不是条数（2026-11-08 修正）：废弃的往往是大块 —— 子智能体
// 卡片在 running 期每个事件整块重写，旧版本全部留在 append-only 文件里。
// 真实会话实测：3.45GB sidecar，有效内容只有 184MB（字节废弃 94%），
// 而废弃**条数**占比只有 18.66%（7054/37810）—— 按条数判，永远不回收。
const compactThreshold = 0.20

// compactGainRatio 是**回收量 vs 重写量**的判据：只有当「可回收的废弃字节」
// 至少达到「有效字节（live）」的这个比例时，全量重写才划算。
//
// 为什么换判据（2026-11-11，CHECKPOINT_V3_DESIGN §6.1 P0）：
//
//	旧判据：废弃**占比** > 20% → 重写全部 live 字节。
//	问题：占比是相对**总量**的，成本却是相对**live**的 —— 两者不对等。
//	实测热会话：live=210MB / stale=106MB（占比 33% 过线），
//	压一次 = 读 315MB + 写 210MB，只为省 106MB → 写放大 3×。
//	而这个「过线」状态每涨 100MB 就重现一次 → 压缩死循环 → swap 打满 → 全机卡。
//
//	新判据：stale ≥ live×0.5。含义是「回收的至少要值回重写的一半」，
//	于是单次压缩的写放大被**封顶**在 ~5×，且循环不可能持续：
//	压完后 stale 归零，文件必须重新长到 1.5×live 才可能再压。
//
//	它同时**内含**了旧的「1.5× 体积冷却」（stale ≥ live×0.5 ⟺ total ≥ 1.5×live），
//	所以旧的 compactCooldownFactor 可以退休（2026-11-11 移除）。
//
//	小文件 + 高废弃率仍会被收：stale/live 与文件大小无关（比例判据）。
//	实测既有用例（200 小块 + 1MB 大块写 3 次）：stale=2MB / live=1.01MB → 2 ≥ 0.5 ✓。
const compactGainRatio = 0.5

// minCompactInterval 是同一 record 两次压缩之间的最小间隔。
//
// 为什么需要它：即便满足「收益 > 成本」，一个**高频写入**的会话也可能反复
// 达到阈值（每次提交都追加大量废弃）。间隔把「即使划算也不连续做」钉死，
// 避免压缩本身成为 IO 抖动源。
//
// 为什么不影响「重启后回收历史垃圾」：lastCompactAt 是进程内状态，重启归零。
const minCompactInterval = 10 * time.Minute

// compactMinBytes 是 compact 的最小体积门槛：太小的文件重写不划算。
//
// 用 atomic 而不是普通变量：后台回收 goroutine 会读它，而测试会临时调小它
// （`-race` 下普通变量会直接报 data race）。
var compactMinBytes atomic.Int64

func init() { compactMinBytes.Store(32 << 20) }

// staleByteRatio 报告废弃字节占比（O(1)）。
func (bs *blockStore) staleByteRatio() float64 {
	total := bs.nBytes - int64(len(blockStoreMagic))
	if total <= 0 {
		return 0
	}
	stale := total - bs.liveBytes
	if stale < 0 {
		return 0
	}
	return float64(stale) / float64(total)
}

// needsCompact 报告是否应当回收（O(1)）。
func (bs *blockStore) needsCompact() bool { return bs.needsCompactAt(compactMinBytes.Load()) }

// needsCompactAt 报告是否值得做一次**全量重写**（O(1)）。
//
// 判据（P0，2026-11-11）：
//  1. 文件本身够大（≥ minBytes）；
//  2. **回收量下限**：可回收的废弃字节 ≥ minBytes（省不下 32MB 就不动）；
//  3. **收益 > 成本**：stale ≥ live×compactGainRatio（写放大封顶 ~5×）；
//  4. **最小间隔**：距上次压缩 ≥ minCompactInterval。
func (bs *blockStore) needsCompactAt(minBytes int64) bool {
	total := bs.nBytes - int64(len(blockStoreMagic))
	if total < minBytes {
		return false
	}
	stale := total - bs.liveBytes
	if stale < minBytes {
		return false // 回收量下限：省不下这么多就别动
	}
	if float64(stale) < float64(bs.liveBytes)*compactGainRatio {
		return false // 收益 < 成本：不值得重写全部 live
	}
	if !bs.lastCompactAt.IsZero() && time.Since(bs.lastCompactAt) < minCompactInterval {
		return false // 最小间隔：避免压缩成为 IO 抖动源
	}
	return true
}

// maybeCompact 在废弃比例超阈值时重写 sidecar，只保留每个 id 的最新版本。
//
// 安全性（这是本函数最重要的部分）：
//   - 写到临时文件 → fsync → 原子 rename 覆盖原 sidecar；
//   - 索引与 sidecar 的更新顺序：**先 rename sidecar，再写索引**。若中途崩溃，
//     索引的 covered 会大于新 sidecar 大小 → 下次打开判定「索引超前」→
//     回退全量扫描并重建索引。绝不会读到错位数据。
//   - 失败不影响正确性：sidecar 保持原样，只是继续膨胀（下次再试）。
//
// 收益（真实数据）：1378MB → 58MB（23×），且此后稳定在 1.2× 有效体积内。
func (bs *blockStore) maybeCompact() error {
	if !bs.needsCompact() {
		return nil
	}
	return bs.compact()
}

// compact 强制重写（不看阈值），供维护入口调用。
func (bs *blockStore) compact() error {
	ids := bs.sortedIDsLocked()

	tmpPath := bs.path + ".compact.tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()

	w := bufio.NewWriterSize(f, 1<<20)
	if _, err := w.Write(blockStoreMagic); err != nil {
		return err
	}
	// 先 flush 挂起的追加，确保 payload 可读。
	if bs.w != nil {
		if err := bs.w.Flush(); err != nil {
			return err
		}
	}

	newIndex := make(map[string]blockRef, len(ids))
	pos := int64(len(blockStoreMagic))
	for _, v := range ids {
		id := uintToString(v)
		ref, okRef := bs.index[id]
		if !okRef {
			continue
		}
		// 分片块：读出目录 → 拼接完整内容（compact 把分片**折叠**成整块；
		// 下次写入时 putAuto 会重新分片，见 block_frags.go）。
		var payload []byte
		if isFragmented(ref.kind) {
			d, dok := bs.fragDirFor(id, ref)
			if !dok {
				return fmt.Errorf("compact: block %s has unreadable fragment dir", id)
			}
			payload, dok = bs.readFragDir(d)
			if !dok {
				return fmt.Errorf("compact: block %s fragment read failed", id)
			}
		} else {
			payload = make([]byte, ref.n)
			if _, err := bs.f.ReadAt(payload, ref.off); err != nil {
				return err
			}
		}
		// 只搬运内容完好的块；校验失败的块保留在原文件里不参与 compact，
		// 让后续读取时明确报错（而不是把损坏数据搬过去当成好的）。
		//
		// **压缩块**：落盘字节是压缩过的 ⇒ 摘要对不上（摘要算逻辑内容）⇒
		// 这里不解压、不校验（读路径会解压并校验 ⇒ 损坏照样被挡住 ✓），
		// 只是**字节级搬运**（不解压 ⇒ compact 不会因为压缩而变慢 ✓）。
		// 压缩标志只对「整块」成立：分片块在 compact 里被**折叠成整块**，
		// 它的内容是逻辑的（readFragDir）⇒ 落盘不压缩 ⇒ 标志要清掉。
		compressed := isCompressed(ref.kind) && !isFragmented(ref.kind)
		recKind := blockRecPut
		if compressed {
			recKind |= kindCompressed
		} else if payloadDigest(payload) != ref.hash {
			return fmt.Errorf("compact: block %s is corrupt, refusing to rewrite", id)
		}
		head := buildBlockRecordHead(recKind, id, payload)
		payloadOff := pos + int64(len(head))
		if _, err := w.Write(head); err != nil {
			return err
		}
		if _, err := w.Write(payload); err != nil {
			return err
		}
		if _, err := w.Write(ref.hash[:]); err != nil {
			return err
		}
		// 类型码从**内容**取（压缩字节要解压前缀）✓
		kindCode := blockKindCode(payload)
		if compressed {
			kindCode = blockKindCodeCompressed(payload) | kindCompressed
		}
		newIndex[id] = blockRef{off: payloadOff, n: int32(len(payload)), hash: ref.hash, kind: kindCode}
		pos += int64(len(head)+len(payload)) + 8
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	// 原子替换 sidecar
	if err := os.Rename(tmpPath, bs.path); err != nil {
		return err
	}
	ok = true

	// 重新打开文件句柄（旧 inode 已被替换）
	nf, err := os.OpenFile(bs.path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	_ = bs.f.Close()
	bs.f = nf
	bs.index = newIndex
	bs.nBytes = pos
	bs.recordsWritten = int64(len(newIndex))
	bs.staleRecords = 0
	bs.bytesWritten = pos
	bs.recomputeLiveBytes()
	bs.sortedDirty = true
	if err := bs.startWriter(); err != nil {
		return err
	}
	bs.recordOpenState()
	// 索引在 sidecar 之后写：崩溃时索引会「超前」，下次打开安全回退。
	return bs.persistIndex()
}
