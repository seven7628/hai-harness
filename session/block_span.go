package session

// 块内容的惰性来源（P5：惰性解压）。
//
// ## 为什么不是裸 []byte
//
// **压缩块**在磁盘上是 zlib 字节，而块摘要（payloadDigest）算的是**逻辑内容**。
// 于是「磁盘字节」与「逻辑字节」必须分开记，否则只有两个坏结局：
//
//   - 提前解压：每开一个会话把整份 sidecar 的块逐个解压进堆 —— 实测
//     **259MB/会话**（20 会话 5.06GB，占堆的 64%），而首屏只要 500 块；
//   - 不解压当逻辑内容用：摘要校验必错（摘要对不上压缩字节）。
//
// Raw 指向 sidecar 的只读 mmap（零拷贝，**不占堆**）；Compressed 为真时
// Raw 是压缩字节，要 Logical() 解压才是逻辑内容。
//
// ## 为什么这样能省
//
// mmap 是虚拟地址：没被读过的页不占物理内存、不占堆。改成惰性之后，
// 打开会话只触碰「窗口 + 卡片」那几千块，其余 3.8 万块**一个字节都不读** ——
// 堆占用从 422MB/会话 降到 ~160MB/会话，翻历史时才按页解压那一页。
//
// 对齐的开源做法：Redis 的 lazy-free-lazy-userdata（用到才解码）、
// Chromium 的 tab discarding（丢弃后按需重取）、Postgres 的 TOAST
// （大字段按需解压出 chunk）。

import "fmt"

// BlockSpan 是一块内容的来源。
type BlockSpan struct {
	// Raw 是磁盘上的字节：未压缩块 = 逻辑内容；压缩块 = zlib 压缩字节。
	// 指向 sidecar 的只读映射，不占堆（惰性解压路径下）。
	Raw []byte
	// Compressed 为真 ⇒ Raw 是压缩字节，需要 Logical() 解压。
	Compressed bool
}

// Logical 返回块的逻辑内容（必要时解压）。
//
// 解压失败 = 块损坏（与 readBlockVerified 的判定一致）：返回错误，
// 不返回半截内容。
func (s BlockSpan) Logical() ([]byte, error) {
	if !s.Compressed {
		return s.Raw, nil
	}
	out, err := decompressBlock(s.Raw)
	if err != nil {
		return nil, fmt.Errorf("decompress block: %w", err)
	}
	return out, nil
}

// logicalSpans 把裸字节切片包成 BlockSpan（全部视为「已是逻辑内容」）。
//
// 用于回退路径（无 mmap / 索引落后）：那条路径按块读出并**已解压**
// （readBlockVerified 见 block_frags.go）。
func logicalSpans(raw [][]byte) []BlockSpan {
	if len(raw) == 0 {
		return nil
	}
	out := make([]BlockSpan, len(raw))
	for i, b := range raw {
		out[i] = BlockSpan{Raw: b}
	}
	return out
}
