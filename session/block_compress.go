package session

// sidecar 的**逐块压缩**（2026-11-11，CHECKPOINT_V3_DESIGN §6 P4）。
//
// ## 为什么是压缩而不是「内容轻量快照 + 事件投影」
//
// 实测 116,913 个真实块（6 个最大的 sidecar / 620MB）的构成：
//
//	items 31.5%（子智能体流水）  images 19.6%（base64）  result 14.6%
//	args 9.1%   thinking 7.5%   contents 3.4%   todos 3.0%   text 0.8%
//
// 两条路能省的空间：
//
//	投影（正文改成事件日志的引用）：4.1GB → ~1.4GB（省 2.7GB）
//	逐块压缩：                 4.1GB → ~1.9GB（省 2.2GB）
//
// 收益只差 20%，但代价差一个数量级：
//
//   · 投影要动 reducer 块模型 + 事件索引 + 读路径 + 分页 + 前端 id 语义，
//     且实测有 10+ 条风险（事件日志没有 seq/索引、chunk 派生的正文与计时
//     不在落盘行里、todos 不是纯事件源……见设计文档 §6 P4'）；
//   · 压缩**全部在存储层内部**，零 UI 语义变化。
//
// ⇒ 先做压缩；投影留作独立决策（它的上限与风险都已量化在案）。
//
// ## 格式
//
// 记录：kind(1) || idLen(4) || id || payloadLen(4) || payload || digest(8)
//
// kind 的 **0x40 位** = 「payload 是 zlib 压缩的」。
// （0x01–0x04 是记录类型；0x80 是**索引项** kind 上的分片标志，不是记录 kind。）
//
// ## 关键不变量：摘要算「逻辑内容」，不算落盘字节
//
// digest（以及由它派生的 blocksRefDoc.Sum）必须算**解压后**的内容。
// 理由：record 里存着 sidecar 的块集合摘要 ⇒ 若摘要算落盘字节，一压缩
// 摘要就变了 ⇒ record 失效 ⇒ 存量清扫重写 sidecar 就必须**同时**重写
// record（两个文件 ⇒ 无法原子 ⇒ 中途崩溃 = 会话不可读）。
//
// 算逻辑内容则：压缩与否摘要不变 ⇒ **清扫只动 sidecar，不用碰 record** ✓
// （未压缩块的落盘字节 == 逻辑字节 ⇒ 既有摘要天然兼容 ✓）。
//
// ## 兼容性
//
//   · 旧记录（无该标志）照常读 ✓；
//   · 旧的 record 摘要照常有效 ✓（存量 sidecar 不必重写）；
//   · 旧二进制读新 sidecar：kind 的 0x40 位它不认识 ⇒ 会把压缩字节当正文
//     ⇒ 视图乱码。**降级风险**：由 sidecar 的 magic 版本挡住（见下方说明）。
//
// ## 压缩级别
//
// 实测 116,913 个真实块：level 1 = 2.16× / 75 MB/s，level 6 = 2.30× / 50 MB/s。
// 压缩只发生在**写路径的脏块**上（每次提交几块），读路径是解压（很快），
// 而 compact 是**字节级搬运**（不解压）⇒ 压缩速度比压缩率更重要 ⇒ **level 1**。

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// kindCompressed 是记录 kind 字节的标志位：置位表示「payload 是 zlib 压缩的」。
const kindCompressed uint8 = 0x40

// blockCompressEnabled 控制 sidecar 是否逐块压缩（默认开）。
//
// 测试可以关掉它来量「不压缩时的追加语义」—— 压缩本身另有专门的测试
// （block_compress_test.go）。关掉它时 sidecar 的行为与压缩上线前完全一致。
var blockCompressEnabled = true

// compressMinBytes 是「值得压缩」的最小 payload 字节数。
// 小于它的块压不动（zlib 头 + 树就几十字节），还白费 CPU。
const compressMinBytes = 512

// compressBlock 压缩一个块的 payload；压不小（或太小）就返回 ok=false（调用方写原文）。
func compressBlock(payload []byte) ([]byte, bool) {
	if len(payload) < compressMinBytes {
		return nil, false
	}
	var buf bytes.Buffer
	buf.Grow(len(payload) / 2)
	w, err := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
	if err != nil {
		return nil, false
	}
	if _, err := w.Write(payload); err != nil {
		return nil, false
	}
	if err := w.Close(); err != nil {
		return nil, false
	}
	out := buf.Bytes()
	// 压不小就别压（省掉解压的 CPU，也省掉标志位）。
	if len(out) >= len(payload) {
		return nil, false
	}
	return out, true
}

// —— 解压上下文池 ——
//
// 一次「打开大会话」要解压上千个块（实测 2026-11-18：1971 块 / 输出 100.3MB）。
// 每块各造一个 zlib reader + 一次 io.ReadAll，代价有两处：
//
//  1. **zlib.NewReader ⇒ 新的 flate 解码器** ⇒ 每次重新分配 LZ77 的 32KB 滑窗
//     （实测 compress/flate 累计 219MB）。
//  2. **io.ReadAll 从 512B 起倍增扩容**，每一步都抛弃旧数组 ⇒ 实测总分配
//     **897MB**（真实输出的 8.9 倍），而最终只留下 58MB。
//
// 改法（2026-11-18，bridge 内存爬山第 1 步）：
//
//   - zlib reader 池化 —— `zlib.Resetter.Reset` **复用** flate 的 hist 窗口
//     （Go 源码 compress/flate/dict_decoder.go 的 init：`cap(hist) < size` 才重分配）
//     ⇒ 解码器状态跨块复用；
//   - 输出缓冲池化 —— 稳态**零分配**（缓冲跨块复用，不再倍增）；
//   - 交付时拷一份**精确大小**的 ⇒ 调用方独占，倍增留下的余量一并丢掉。
//
// 语义不变：返回的仍是「解压后的完整字节」，调用方照旧独占。
type decompCtx struct {
	// zr 是 zlib 的 reader（具体类型未导出，但它同时实现了 zlib.Resetter ⇒ 可复位复用）。
	zr  io.ReadCloser
	buf []byte
}

// decompPool 解压上下文池（zlib reader + 输出缓冲，整体一个）。
var decompPool = sync.Pool{
	New: func() any {
		c := &decompCtx{buf: make([]byte, 0, 256<<10)}
		if zr, err := newResettableZlib(); err == nil {
			c.zr = zr
		}
		return c
	},
}

// emptyZlibStream 是一个**空的 zlib 流**（用标准库自己生成 ⇒ 不依赖任何魔数）。
//
// 用途：newResettableZlib 用它造出「可 Reset」的 reader 实例。
// 为什么全局生成一次：zlib.NewWriter 一次要 ~1.1MB（flate 的 hash 表 + 窗口），
// 而 sync.Pool 的条目会被 GC 清掉 ⇒ 每次新建都付 1.1MB 纯属浪费。
var emptyZlibStream = func() []byte {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, _ = zw.Write(nil)
	_ = zw.Close()
	return buf.Bytes()
}()

// newResettableZlib 造一个**可 Reset** 的 zlib reader。
func newResettableZlib() (io.ReadCloser, error) {
	zr, err := zlib.NewReader(bytes.NewReader(emptyZlibStream))
	if err != nil {
		return nil, err
	}
	if _, ok := zr.(zlib.Resetter); !ok {
		return nil, fmt.Errorf("zlib: reader 不支持 Reset")
	}
	return zr, nil
}

// decompMaxKeep 归还池的输出缓冲上限：超大块（>4MB，典型是 base64 图片块）
// 不留在池里 —— 否则一个巨无霸会长期占着池。
const decompMaxKeep = 4 << 20

// decompressBlock 解开一个块的 payload。
//
// 输入不是合法 zlib 流时返回错误 —— 调用方据此把该块判为损坏
// （与「摘要不符」同等处理）。
//
// 返回的字节由调用方**独占**（拷贝出来的精确大小，不引用池化缓冲）。
func decompressBlock(payload []byte) ([]byte, error) {
	c := decompPool.Get().(*decompCtx)
	out, err := c.decompress(payload)
	if err != nil {
		decompPool.Put(c)
		return nil, err
	}
	// 交出所有权：拷一份精确大小的（倍增扩容留下的余量也不要了）。
	res := bytes.Clone(out)
	c.release()
	decompPool.Put(c)
	return res, nil
}

// decompressBlockTransient 解压到**池化缓冲**并在回调期间交它使用。
//
// 回调返回后缓冲归池 ⇒ 调用方**不得**保留它（要留就用 decompressBlock）。
// 给「只读一遍就不留」的路径用（后台清扫：逐块解压只为了算摘要）。
func decompressBlockTransient(payload []byte, fn func([]byte) error) error {
	c := decompPool.Get().(*decompCtx)
	out, err := c.decompress(payload)
	if err != nil {
		decompPool.Put(c)
		return err
	}
	err = fn(out)
	c.release()
	decompPool.Put(c)
	return err
}

// decompress 把 payload 解到池化缓冲（结果在 release 之前有效）。
func (c *decompCtx) decompress(payload []byte) ([]byte, error) {
	if c.zr == nil {
		zr, err := newResettableZlib()
		if err != nil {
			return nil, err
		}
		c.zr = zr
	}
	rs, ok := c.zr.(zlib.Resetter)
	if !ok {
		return nil, fmt.Errorf("zlib: reader 不支持 Reset")
	}
	if err := rs.Reset(bytes.NewReader(payload), nil); err != nil {
		return nil, err
	}
	buf := c.buf[:0]
	for {
		if len(buf) == cap(buf) {
			// 倍增扩容（同 io.ReadAll）：稳态下缓冲已足够大 ⇒ 不会走到这里。
			buf = append(buf, 0)[:len(buf)]
		}
		n, rerr := c.zr.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return nil, rerr
		}
	}
	c.buf = buf
	return buf, nil
}

// release 决定缓冲要不要留在池里（超大的不留）。
func (c *decompCtx) release() {
	if cap(c.buf) > decompMaxKeep {
		c.buf = nil
	}
}


// blockKindCodeCompressed 取压缩块的**真实块类型**。
//
// 块类型码在 JSON 的前 64 字节里（见 block_index.go 的 blockKindCode），
// 所以**只需解压前缀**（128 字节）—— 不必把整块解开（冷启动全量重建时
// 每个块都这样 ⇒ 整份解压要 20s；只解压前缀则是 O(1)）。
func blockKindCodeCompressed(payload []byte) uint8 {
	r, err := zlib.NewReader(bytes.NewReader(payload))
	if err != nil {
		return blockKindUnknown
	}
	defer r.Close()
	head := make([]byte, 128)
	n, _ := io.ReadFull(r, head)
	return blockKindCode(head[:n])
}

// —— 存量清扫 ——

// CompressSweepResult 是 sidecar 压缩清扫的结果。
type CompressSweepResult struct {
	Files       int   // 扫到的 sidecar 数（进入处理流程的）
	Rewritten   int   // 实际重写的（有可压的块）
	Skipped     int   // 索引判定「清过且没动过」而跳过的 sidecar 数
	Before      int64 // 重写前总字节
	After       int64 // 重写后总字节
	Blocks      int   // 压缩的块数
	BlocksTotal int   // 扫描的块数
	Elapsed     time.Duration
}

// Saved 返回省下的字节数。
func (r CompressSweepResult) Saved() int64 { return r.Before - r.After }

// compressBlockStore 把一个 sidecar 里**未压缩**的块原地压一遍。
//
// 返回 (是否重写, 压缩块数, 扫描块数, 错误)。
//
// ## 为什么可以只动 sidecar、不碰 record
//
// 摘要算的是**逻辑内容**（见本文件头部的不变量）⇒ 压缩前后
// `blocksRefDoc.Sum` 完全相同 ⇒ record records 无需重写 ✓
// （这是整套方案能安全落地的关键：只重写一个文件 ⇒ 原子替换 ⇒ 无撕裂态）。
//
// ## 复用 compact 的重写路径
//
// 走 `beginCompact` / `rewriteCompact` / `finishCompact` —— 它已经是
// 「持锁 + 临时文件 + 原子 rename + 尾部增量合流」的成熟实现。
// 区别只在重写循环里：这里的块**要压缩**（compact 是字节级搬运，不解压也不压缩）。
func compressBlockStore(bs *blockStore, compressed, total *int) (bool, int, int, error) {
	ids := make([]string, 0, len(bs.index))
	for id := range bs.index {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return false, 0, 0, nil
	}
	sort.Slice(ids, func(i, j int) bool { return lessID(ids[i], ids[j]) })

	snap, err := bs.beginCompact()
	if err != nil {
		return false, 0, 0, err
	}
	cw, err := bs.rewriteCompact(snap)
	if err != nil {
		return false, 0, 0, err
	}
	// rewriteCompact 已经写完全部块（它是「字节级搬运」）—— 但存量块是**未压缩**的，
	// 所以我们**丢弃它的产物**，自己按压缩口径重写一遍。
	// 说明：这样会多写一遍临时文件（存量清扫是一次性操作，且只发生在启动后台，
	// 为了复用「持锁 + 合流」那套逻辑，这个代价可接受）。
	cw.abort()

	// 自己写（与 rewriteCompact 同构，但**压缩未压缩的块**）。
	before := bs.nBytes
	if err := bs.rewriteWithCompression(snap, compressed, total); err != nil {
		return false, *compressed, *total, err
	}
	after := bs.nBytes
	return *compressed > 0 && after < before, *compressed, *total, nil
}

// rewriteWithCompression 把 sidecar 重写成「尽量压缩」的形态（原子替换）。
func (bs *blockStore) rewriteWithCompression(snap *compactSnapshot, compressed, total *int) error {
	tmpPath := bs.path + ".compress.tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := w.Write(blockStoreMagic); err != nil {
		return err
	}
	pos := int64(len(blockStoreMagic))
	newIndex := make(map[string]blockRef, len(snap.index))
	ids := make([]string, 0, len(snap.index))
	for id := range snap.index {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return lessID(ids[i], ids[j]) })

	for _, id := range ids {
		ref := snap.index[id]
		*total++
		var payload []byte
		// 分片块：折叠成整块（与 compact 同语义），内容来自片段拼接。
		if isFragmented(ref.kind) {
			d, dok := bs.fragDirFor(id, ref)
			if !dok {
				return fmt.Errorf("compress: block %s has unreadable fragment dir", id)
			}
			payload, dok = bs.readFragDir(d)
			if !dok {
				return fmt.Errorf("compress: block %s fragment read failed", id)
			}
			if payloadDigest(payload) != ref.hash {
				return fmt.Errorf("compress: block %s is corrupt", id)
			}
		} else {
			payload = make([]byte, ref.n)
			if _, err := bs.f.ReadAt(payload, ref.off); err != nil {
				return err
			}
			if isCompressed(ref.kind) {
				// 已压缩：解压出逻辑内容（摘要按逻辑内容校验）。
				dec, derr := decompressBlock(payload)
				if derr != nil {
					return fmt.Errorf("compress: block %s decompress: %w", id, derr)
				}
				payload = dec
			}
			if payloadDigest(payload) != ref.hash {
				return fmt.Errorf("compress: block %s is corrupt, refusing to rewrite", id)
			}
		}
		stored := payload
		recKind := blockRecPut
		if c, cok := compressBlock(payload); cok {
			stored = c
			recKind |= kindCompressed
			// 只数「**这次新压上的**」块：已经压过的块重压一次不算数 ——
			// 否则这个计数永远等于「全部块数」，看不出到底有没有进展
			// （而且 rewritten 的判据 `compressed > 0` 就永远为真）。
			if !isCompressed(ref.kind) {
				*compressed++
			}
		}
		head := buildBlockRecordHead(recKind, id, stored)
		payloadOff := pos + int64(len(head))
		if _, err := w.Write(head); err != nil {
			return err
		}
		if _, err := w.Write(stored); err != nil {
			return err
		}
		if _, err := w.Write(ref.hash[:]); err != nil {
			return err
		}
		kindCode := blockKindCode(payload)
		if recKind&kindCompressed != 0 {
			kindCode |= kindCompressed
		}
		newIndex[id] = blockRef{off: payloadOff, n: int32(len(stored)), hash: ref.hash, kind: kindCode}
		pos += int64(len(head) + len(stored) + 8)
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
	// 原子替换（与 compact 的收尾同一套：rename + 目录 fsync + 索引重建）。
	if err := os.Rename(tmpPath, bs.path); err != nil {
		return fmt.Errorf("replace block store: %w", err)
	}
	if err := syncDir(filepath.Dir(bs.path)); err != nil {
		return err
	}
	ok = true
	bs.index = newIndex
	bs.frags = map[string]*fragDir{}
	bs.nBytes = pos
	bs.sortedDirty = true
	bs.recordsWritten = int64(len(newIndex))
	bs.staleRecords = 0
	bs.liveBytes = 0
	for _, r := range newIndex {
		bs.liveBytes += blockRecordSize("0", int(r.n))
	}
	// 重新打开文件（rename 换了 inode）。
	_ = bs.f.Close()
	nf, oerr := os.OpenFile(bs.path, os.O_RDWR, 0o600)
	if oerr != nil {
		return oerr
	}
	bs.f = nf
	if err := bs.startWriter(); err != nil {
		return err
	}
	bs.recordOpenState()
	return bs.persistIndex()
}

// SweepBlockStores 扫描 root 下所有 sidecar（`*.record.json.blocks`），
// 把其中**未压缩**的块就地压一遍（存量清扫）。
//
// 参数：
//   - root：会话根目录（~/.go-code/sessions）
//   - pause：每个文件之间的间隔（限速，避免与正常使用抢 IO）
//   - ix：清扫索引（可为 nil）。非 nil 时，「上次清过且此后没动过」的
//     sidecar 直接跳过（见 sweep_index.go）。
//
// 安全性：
//
//	· 单文件失败不影响整轮；
//	· 只重写 sidecar，**不碰 record**（摘要算逻辑内容 ⇒ Sum 不变 ✓）；
//	· 原子替换（临时文件 + fsync + rename）；
//	· 与提交/compact 共用同一把**进程级按路径锁**（`lockRecord`）——
//	  否则重写期间到达的追加会写进被 rename 掉的旧 inode（丢数据）。
func SweepBlockStores(root string, pause time.Duration, ix *SweepIndex) (CompressSweepResult, error) {
	res := CompressSweepResult{}
	start := time.Now()
	if root == "" {
		return res, fmt.Errorf("sweep blocks: empty root")
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".record.json.blocks") {
			return nil
		}
		fi, serr := os.Stat(path)
		if serr != nil {
			return nil
		}
		// 索引：清过且没动过 → 跳过（连文件都不打开）。
		if ix.Skip(path, fi.Size(), fi.ModTime().UnixNano()) {
			res.Skipped++
			return nil
		}
		res.Files++
		res.Before += fi.Size()

		rewritten, nComp, nTotal, cerr := sweepOneBlockStore(path)
		res.BlocksTotal += nTotal
		res.Blocks += nComp
		if cerr == nil && rewritten {
			res.Rewritten++
		}
		if nfi, nerr := os.Stat(path); nerr == nil {
			res.After += nfi.Size()
			// **失败不标记**：压缩失败的文件必须留到下一轮再试 ——
			// 标了就会在下次清扫被跳过 ⇒ 未压缩的块永远补不上。
			if cerr == nil {
				ix.Mark(path, nfi.Size(), nfi.ModTime().UnixNano())
			}
		} else {
			res.After += fi.Size()
		}
		if pause > 0 {
			time.Sleep(pause)
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	res.Elapsed = time.Since(start)
	return res, nil
}

// hasCompressibleUncompressed 报告「有没有**未压缩且压得动**的块」。
//
// ## 为什么不是「有没有未压缩块」
//
// 实测（2026-11-13）：每个 sidecar 里剩下的未压缩块**100% 压不动**
// （内容本身是随机/二进制，压了反而更大）。zlib 压不动时 compressBlock 返回
// ok=false ⇒ 块保持未压缩 ⇒ 老判据「有未压缩块 ⇒ 需要重写」永远为真 ⇒
// **每次启动都把整份 sidecar 解压一遍、压一遍、重写一遍**（实测 10.5s +
// 96MB 重写），而文件内容一个字节都没变。
//
// 后果有两层：白烧 CPU/IO；重写改了 mtime ⇒ 清扫索引的指纹失效 ⇒ 下次
// 启动照样重扫。**索引本来能把这轮整个跳过的**。
//
// 所以判据必须是「压得动」：压不动的内容，试一万次也压不动 ⇒ 不该重写。
func (bs *blockStore) hasCompressibleUncompressed() bool {
	for _, ref := range bs.index {
		if isCompressed(ref.kind) {
			continue
		}
		if ref.n <= 0 {
			continue
		}
		payload := make([]byte, ref.n)
		if _, err := bs.f.ReadAt(payload, ref.off); err != nil {
			continue // 读不出来 = 坏块，交给别的路径报错，不在这里重写
		}
		if _, ok := compressBlock(payload); ok {
			return true // 找到一个就够了
		}
	}
	return false
}

// sweepOneBlockStore 压一个 sidecar（与提交/compact 共用按路径锁）。
func sweepOneBlockStore(path string) (bool, int, int, error) {
	unlock := lockRecord(path)
	defer unlock()

	bs, err := openBlockStore(path) // 会走索引热启动
	if err != nil {
		return false, 0, 0, err
	}
	// 没有「未压缩且压得动」的块 → 跳过（幂等：二次清扫不重写）。
	// 见 hasCompressibleUncompressed 的说明：老判据「有未压缩块」会让
	// **每次启动都重写整份文件**（内容还是压不动那批）。
	if !bs.hasCompressibleUncompressed() {
		// 什么都没改 → **不落盘**地关闭。Close() 会 fsync 整个 sidecar +
		// 重写 .idx，而这次调用一个字节都没改（实测每次启动 477 次）。
		bs.closeQuiet()
		return false, 0, len(bs.index), nil
	}
	defer bs.Close()
	compressed, total := 0, 0
	rewritten, nComp, nTotal, rerr := compressBlockStore(bs, &compressed, &total)
	if rerr != nil {
		return false, nComp, nTotal, rerr
	}
	return rewritten, nComp, nTotal, nil
}
