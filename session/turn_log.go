package session

// turns 追加日志：record body 只保留尾部窗口，历史轮次落在这里（无损）。
//
// 为什么需要它（2026-11-08 实测）：turns 是「每次 LLM 调用一条」的追加型数组，
// 真实会话 88.7k 条 / 24.3MB，占 view body 的 94%。它带来的成本是
//   · 打开：完整解码 155ms，哪怕只用 RawMessage「跳过不解」也要 105ms 扫描；
//   · 提交：编码 37ms + 写盘 24ms（整个 record 才 26.7MB）。
// 而 UI 只用到「总条数 + 最后两条」。
//
// 于是：body 里只放尾部窗口 + turns_total，历史按批追加到本日志。日志只追加、
// 不重写，所以「历史越长、每次提交越慢」这条曲线被切断。
//
// 格式（小端）：
//
//	magic "HAITURN1\n"
//	重复：<uint32 chunkLen><uint32 count><chunkLen 字节的 JSON 数组>
//
// count 单独存，是为了「只数条数」时不必解析 JSON（扫头即可）。

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const turnLogMagic = "HAITURN1\n"

// TurnLogPath 返回某条 record 对应的 turns 日志路径。
func TurnLogPath(recordPath string) string { return recordPath + ".turns" }

// AppendTurnChunk 追加一批 turns（偶发场景：打开时 1 次，之后每提交 1 次）。
//
// 语义：调用方给的是「自上次成功追加以来的新轮次」，本函数不做去重。
// 一次写完 + fsync，半截写入由读取侧的 chunkLen 校验发现并截断。
func AppendTurnChunk(recordPath string, chunk []byte, count int) error {
	if count < 0 {
		return fmt.Errorf("turn log: negative count %d", count)
	}
	// 会话目录在「首次提交」时可能还不存在（record 本身也是由 CommitRecord
	// 创建的）—— 这里必须先建目录，否则新会话的第一次提交会因日志写失败而中止。
	if err := os.MkdirAll(filepath.Dir(TurnLogPath(recordPath)), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(TurnLogPath(recordPath), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() == 0 {
		if _, err := f.Write([]byte(turnLogMagic)); err != nil {
			return err
		}
	}
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(len(chunk)))
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(count))
	if _, err := f.Write(hdr[:]); err != nil {
		return err
	}
	if len(chunk) > 0 {
		if _, err := f.Write(chunk); err != nil {
			return err
		}
	}
	return f.Sync()
}

// TurnLogCount 扫长度头数出日志里的总条数（不解析 JSON）。
func TurnLogCount(recordPath string) (int, error) {
	f, err := os.Open(TurnLogPath(recordPath))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	magic := make([]byte, len(turnLogMagic))
	if _, err := io.ReadFull(f, magic); err != nil {
		return 0, fmt.Errorf("turn log: short magic: %w", err)
	}
	if string(magic) != turnLogMagic {
		return 0, fmt.Errorf("turn log: bad magic")
	}
	total := 0
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(f, hdr[:]); err == io.EOF {
			return total, nil
		} else if err != nil {
			return total, fmt.Errorf("turn log: truncated header: %w", err)
		}
		n := int64(binary.LittleEndian.Uint32(hdr[0:4]))
		total += int(binary.LittleEndian.Uint32(hdr[4:8]))
		if n == 0 {
			continue
		}
		if _, err := f.Seek(n, io.SeekCurrent); err != nil {
			return total, err
		}
	}
}

// ReadTurnChunks 返回日志里的全部 chunk（按写入顺序）。
//
// 目前只有测试与迁移校验会全量读；UI 翻旧轮次时应当按 turns_total 与
// 页游标取尾部若干 chunk（接口留在这里，避免以后再加一套文件格式）。
func ReadTurnChunks(recordPath string) ([][]byte, error) {
	f, err := os.Open(TurnLogPath(recordPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	magic := make([]byte, len(turnLogMagic))
	if _, err := io.ReadFull(f, magic); err != nil {
		return nil, fmt.Errorf("turn log: short magic: %w", err)
	}
	if string(magic) != turnLogMagic {
		return nil, fmt.Errorf("turn log: bad magic")
	}
	var out [][]byte
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(f, hdr[:]); err == io.EOF {
			return out, nil
		} else if err != nil {
			return out, fmt.Errorf("turn log: truncated header: %w", err)
		}
		n := int(binary.LittleEndian.Uint32(hdr[0:4]))
		count := int(binary.LittleEndian.Uint32(hdr[4:8]))
		if n == 0 {
			continue
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(f, buf); err != nil {
			return out, fmt.Errorf("turn log: truncated chunk: %w", err)
		}
		if count > 0 {
			out = append(out, buf)
		}
	}
}
