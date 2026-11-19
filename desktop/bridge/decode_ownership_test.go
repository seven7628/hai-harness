package main

// 「解压缓冲只在回调期间有效」的所有权不变式（2026-11-18，bridge 内存爬山第 4 步）。
//
// 背景：读路径改成 transient 变体后，解压缓冲**用完就归池**、可能被下一个块覆写。
// ⇒ 解析出来的块必须**自己拥有**数据（不别名那块缓冲）。
//
// 这条测试钉住它：解一批块 → 再解一批（逼池复用同一块缓冲）→ 校验第一批没被改坏。
//
// 为什么这个前提成立（改动时确认过）：Block 的字段全是值类型
// （**没有 RawMessage / []byte / map**）⇒ encoding/json 会把数据拷出来。

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"testing"

	"github.com/seven7628/hai-harness/session"
)

// compressedBlock 造一个压缩块的 payload。
func compressedBlock(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestDecodeBlocksOwnsParsedData：已解析的块不因池复用而损坏。
func TestDecodeBlocksOwnsParsedData(t *testing.T) {
	const n = 8
	spans := make([]session.BlockSpan, n)
	want := make([]string, n)
	for i := 0; i < n; i++ {
		want[i] = fmt.Sprintf("第 %d 个块的内容-内容-内容", i)
		payload := compressedBlock(t, fmt.Sprintf(`{"kind":"assistant","id":%d,"text":%q}`, i+1, want[i]))
		spans[i] = session.BlockSpan{Raw: payload, Compressed: true}
	}

	out := make([]Block, n)
	if err := decodeBlocksParallel(spans, nil, out); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	for i := range out {
		if out[i].Text != want[i] {
			t.Fatalf("块 %d 内容 = %q，期望 %q", i, out[i].Text, want[i])
		}
	}

	// 再解一批**别的**块 ⇒ 池里那块缓冲会被复用并覆写。
	other := make([]session.BlockSpan, n)
	for i := 0; i < n; i++ {
		other[i] = session.BlockSpan{
			Raw:       compressedBlock(t, fmt.Sprintf(`{"kind":"tool","id":%d,"text":"别的内容"}`, 1000+i)),
			Compressed: true,
		}
	}
	otherOut := make([]Block, n)
	if err := decodeBlocksParallel(other, nil, otherOut); err != nil {
		t.Fatalf("第二批解码失败: %v", err)
	}

	// 第一批必须原封不动。
	for i := range out {
		if out[i].Text != want[i] {
			t.Fatalf("块 %d 被后来的解压改坏了：%q（期望 %q）⇒ 解析结果别名了池化缓冲",
				i, out[i].Text, want[i])
		}
		if out[i].Kind != "assistant" {
			t.Fatalf("块 %d 的 kind 被改坏了：%q", i, out[i].Kind)
		}
	}
}
