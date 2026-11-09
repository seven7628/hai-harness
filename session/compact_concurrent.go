package session

// 并发回收（3.3）：把 compact 拆成「快照 → 无锁重写 → 合流增量 + 原子切换」。
//
// 为什么需要：即使回收挪到了后台（3.2），旧做法仍然整段持有该 record 的锁，
// 同会话的下一次提交要等整份 sidecar 重写完（真实 3.45GB 约 7s）。
//
// 现在锁只被持有两小段：
//
//	① 快照（µs）：flush 写缓冲、拷一份索引、记下 nBytes；
//	② 合流（O(增量)）：把重写期间新追加的记录重放进新文件、平移 offset，
//	   然后 rename + 装上新索引。
//
// 中间的重写阶段**不持锁**：提交可以继续往旧文件追加（append-only → 已写下
// 的 offset 永远可读），回收结束后这些尾部记录被合流进新文件，一条都不丢。
//
// 失败语义：任何一步失败都只删临时文件，正式 sidecar 保持原样（下次再试）。
// 崩溃语义：与既有 compact 一致 —— 先 rename sidecar，再写索引；索引「超前」
// 时下次打开会全量重建。

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
)

var errCompactAborted = errors.New("compact aborted")

// compactSnapshot 是回收开始时的稳定视图。
type compactSnapshot struct {
	pos   int64               // 尾部起点：此位置之前的字节在重写期只读
	index map[string]blockRef // 快照索引（重写阶段只用它，不碰 bs.index）
	// file 是快照时的 sidecar 身份（os.SameFile 比对 dev+ino）：
	// 重写期间若实例被换掉（会话被删/别的回收已经切过文件），合流必须放弃。
	file os.FileInfo
}

type compactWriter struct {
	tmpPath  string
	f        *os.File
	w        *bufio.Writer
	newIndex map[string]blockRef
	pos      int64
}

// beginCompact 在持锁状态下拍快照。
func (bs *blockStore) beginCompact() (*compactSnapshot, error) {
	if bs == nil || bs.f == nil {
		return nil, errCompactAborted
	}
	// 写缓冲必须先落地：nBytes 已经把缓冲中的字节算进去了，
	// 而重写阶段要按 offset 从文件里读 payload。
	if bs.w != nil {
		if err := bs.w.Flush(); err != nil {
			return nil, err
		}
	}
	idx := make(map[string]blockRef, len(bs.index))
	for id, ref := range bs.index {
		idx[id] = ref
	}
	fi, err := bs.f.Stat()
	if err != nil {
		return nil, err
	}
	return &compactSnapshot{pos: bs.nBytes, index: idx, file: fi}, nil
}

// rewriteCompact 在**不持锁**的状态下把快照里的有效 payload 写进临时文件。
func (bs *blockStore) rewriteCompact(snap *compactSnapshot) (*compactWriter, error) {
	tmpPath := bs.path + ".compact.tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	cw := &compactWriter{
		tmpPath:  tmpPath,
		f:        f,
		w:        bufio.NewWriterSize(f, 1<<20),
		newIndex: make(map[string]blockRef, len(snap.index)),
		pos:      int64(len(blockStoreMagic)),
	}
	if _, err := cw.w.Write(blockStoreMagic); err != nil {
		cw.abort()
		return nil, err
	}

	ids := make([]string, 0, len(snap.index))
	for id := range snap.index {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return lessID(ids[i], ids[j]) })

	for _, id := range ids {
		ref := snap.index[id]
		payload := make([]byte, ref.n)
		if _, err := bs.f.ReadAt(payload, ref.off); err != nil {
			cw.abort()
			return nil, err
		}
		if payloadDigest(payload) != ref.hash {
			cw.abort()
			return nil, fmt.Errorf("compact: block %s is corrupt, refusing to rewrite", id)
		}
		head := buildBlockRecordHead(blockRecPut, id, payload)
		payloadOff := cw.pos + int64(len(head))
		if _, err := cw.w.Write(head); err != nil {
			cw.abort()
			return nil, err
		}
		if _, err := cw.w.Write(payload); err != nil {
			cw.abort()
			return nil, err
		}
		if _, err := cw.w.Write(ref.hash[:]); err != nil {
			cw.abort()
			return nil, err
		}
		cw.newIndex[id] = blockRef{off: payloadOff, n: ref.n, hash: ref.hash}
		cw.pos += int64(len(head) + int(ref.n) + 8)
	}
	return cw, nil
}

// finishCompact 在持锁状态下合流尾部增量并原子切换。
func (bs *blockStore) finishCompact(snap *compactSnapshot, cw *compactWriter) error {
	if bs == nil || bs.f == nil {
		cw.abort()
		return errCompactAborted
	}
	if fi, err := bs.f.Stat(); err != nil || !os.SameFile(fi, snap.file) {
		cw.abort()
		return errCompactAborted
	}
	// 重写期间可能有新提交：它们写进旧文件 [snap.pos, nBytes)。先把写缓冲落地，
	// 再把这段记录按序重放进新文件（offset 由 cw.pos 重算）。
	if bs.w != nil {
		if err := bs.w.Flush(); err != nil {
			cw.abort()
			return err
		}
	}
	applied := 0
	if end := bs.nBytes; end > snap.pos {
		r := bufio.NewReaderSize(io.NewSectionReader(bs.f, snap.pos, end-snap.pos), 1<<20)
		pos := snap.pos
		for pos < end {
			kind, id, payload, err := readBlockRecord(r)
			if err != nil {
				// 尾部半截/损坏：放弃本次回收，正式文件保持原样。
				cw.abort()
				return fmt.Errorf("%w: tail replay at %d: %v", errCompactAborted, pos, err)
			}
			head := buildBlockRecordHead(kind, id, payload)
			if kind == blockRecPut {
				payloadOff := cw.pos + int64(len(head))
				if _, err := cw.w.Write(head); err != nil {
					cw.abort()
					return err
				}
				if _, err := cw.w.Write(payload); err != nil {
					cw.abort()
					return err
				}
				digest := payloadDigest(payload)
				if _, err := cw.w.Write(digest[:]); err != nil {
					cw.abort()
					return err
				}
				cw.newIndex[id] = blockRef{off: payloadOff, n: int32(len(payload)), hash: digest}
				cw.pos += int64(len(head) + len(payload) + 8)
			} else { // blockRecDel：写删除记录，索引里去掉
				if _, err := cw.w.Write(head); err != nil {
					cw.abort()
					return err
				}
				digest := payloadDigest(nil)
				if _, err := cw.w.Write(digest[:]); err != nil {
					cw.abort()
					return err
				}
				delete(cw.newIndex, id)
				cw.pos += int64(len(head) + 8)
			}
			pos += int64(len(head) + len(payload) + 8)
			applied++
		}
	}

	if err := cw.w.Flush(); err != nil {
		cw.abort()
		return err
	}
	if err := cw.f.Sync(); err != nil {
		cw.abort()
		return err
	}
	if err := cw.f.Close(); err != nil {
		os.Remove(cw.tmpPath)
		return err
	}
	if err := os.Rename(cw.tmpPath, bs.path); err != nil {
		os.Remove(cw.tmpPath)
		return err
	}

	nf, err := os.OpenFile(bs.path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	_ = bs.f.Close()
	bs.f = nf
	bs.index = cw.newIndex
	bs.nBytes = cw.pos
	bs.recordsWritten = int64(len(cw.newIndex))
	bs.staleRecords = 0
	bs.bytesWritten = cw.pos
	bs.sortedDirty = true
	bs.recomputeLiveBytes()
	// 记下压缩后的体积：冷却判据用（见 block_store.go 的 compactCooldownFactor）。
	bs.lastCompactBytes = cw.pos
	if err := bs.startWriter(); err != nil {
		return err
	}
	bs.recordOpenState()
	return bs.persistIndex()
}

func (cw *compactWriter) abort() {
	if cw == nil {
		return
	}
	if cw.f != nil {
		_ = cw.f.Close()
	}
	_ = os.Remove(cw.tmpPath)
}

// lessID 按数值比较两个 id（非数值 id 退化为字典序，保证确定性）。
func lessID(a, b string) bool {
	na, ea := strconv.ParseUint(a, 10, 64)
	nb, eb := strconv.ParseUint(b, 10, 64)
	if ea == nil && eb == nil {
		return na < nb
	}
	return a < b
}
