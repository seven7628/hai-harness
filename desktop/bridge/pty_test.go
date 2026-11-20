// [implemented] 依据 docs/TERMINAL_BRIDGE_SPEC.md §1.2/§1.3 与已还原的
// components/terminal/frame-test.ts 契约，为 pty.go 的控制帧编解码与 ticket
// 生命周期补回归测试（前端已用同一组用例验证，这里在 Go 侧对拍）。
package main

import (
	"bytes"
	"testing"
	"time"
)

func TestPtyCtrlFrameRoundTrip(t *testing.T) {
	frame := encodeCtrl(map[string]any{"t": "resize", "cols": float64(120), "rows": float64(40)})
	if len(frame) < 2 || frame[0] != ptyCtrlByte {
		t.Fatalf("控制帧必须以 0x00 开头: %v", frame)
	}
	ctrl, data := decodeFrame(frame)
	if data != nil {
		t.Fatalf("控制帧不应回落成数据: %v", data)
	}
	if ctrl == nil || asString(ctrl["t"]) != "resize" {
		t.Fatalf("控制帧解码失败: %#v", ctrl)
	}
	if asInt(ctrl["cols"]) != 120 || asInt(ctrl["rows"]) != 40 {
		t.Fatalf("载荷丢失: %#v", ctrl)
	}
}

func TestPtyDataFramePassthroughKeepsMiddleNUL(t *testing.T) {
	raw := []byte{104, 0, 105} // "h\x00i"
	ctrl, data := decodeFrame(raw)
	if ctrl != nil {
		t.Fatalf("非 0x00 开头不应是控制帧: %#v", ctrl)
	}
	if !bytes.Equal(data, raw) {
		t.Fatalf("数据帧必须原样返回（中间的 NUL 也是数据）: %v", data)
	}
}

func TestPtyMalformedCtrlFallsBackToData(t *testing.T) {
	cases := [][]byte{
		{0},                        // 只有首字节
		{0, '{'},                   // 半个 JSON
		{0, 'x', 'y'},              // 非 JSON
		[]byte("\x00{\"t\":\"\"}"), // t 为空
	}
	for _, c := range cases {
		ctrl, data := decodeFrame(c)
		if ctrl != nil {
			t.Fatalf("畸形控制帧不应判为控制: %v", c)
		}
		if !bytes.Equal(data, c) {
			t.Fatalf("畸形控制帧必须回落成数据: got %v want %v", data, c)
		}
	}
}

func TestPtyOversizedFrameNotJSONParsed(t *testing.T) {
	big := bytes.Repeat([]byte{'a'}, 256<<10) // 首字节 'a' != 0x00
	ctrl, data := decodeFrame(big)
	if ctrl != nil {
		t.Fatalf("大帧不应被当控制帧")
	}
	if len(data) != len(big) {
		t.Fatalf("大帧长度应原样保留: got %d want %d", len(data), len(big))
	}
}

func TestPtyUTF8Passthrough(t *testing.T) {
	raw := []byte("中文日志")
	ctrl, data := decodeFrame(raw)
	if ctrl != nil {
		t.Fatalf("UTF-8 输出不应是控制帧")
	}
	if string(data) != "中文日志" {
		t.Fatalf("UTF-8 字节必须原样透传: %q", string(data))
	}
}

func TestPtyTicketOneTimeAndExpiry(t *testing.T) {
	pm := &ptyManager{sessions: map[string]*ptySession{}, tickets: map[string]ptyTicket{}}
	pm.sessions["p1"] = &ptySession{id: "p1", done: make(chan struct{})}

	tk, err := pm.issueTicket("p1")
	if err != nil {
		t.Fatalf("issueTicket: %v", err)
	}
	if id, ok := pm.consumeTicket(tk); !ok || id != "p1" {
		t.Fatalf("首次消费应成功: id=%q ok=%v", id, ok)
	}
	// 一次性：第二次必须失败（防重放）
	if _, ok := pm.consumeTicket(tk); ok {
		t.Fatalf("ticket 必须一次性，第二次消费应失败")
	}

	// 过期：手工塞一个已过期票据。
	pm.mu.Lock()
	pm.tickets["old"] = ptyTicket{ptyID: "p1", expires: time.Now().Add(-time.Second)}
	pm.mu.Unlock()
	if _, ok := pm.consumeTicket("old"); ok {
		t.Fatalf("过期 ticket 必须被拒")
	}
}

func TestPtyTicketUnknownSession(t *testing.T) {
	pm := &ptyManager{sessions: map[string]*ptySession{}, tickets: map[string]ptyTicket{}}
	if _, err := pm.issueTicket("nope"); err == nil {
		t.Fatalf("未知 pty_id 应报错")
	}
}

func TestPtyLiveEcho(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过真实 PTY 冒烟测试（-short）")
	}
	pm := &ptyManager{sessions: map[string]*ptySession{}, tickets: map[string]ptyTicket{}}
	dir := t.TempDir()
	sess, err := pm.create(dir, "", 80, 24)
	if err != nil {
		t.Skipf("本机无法分配 PTY，跳过: %v", err)
	}
	defer pm.closeSession(sess.id, "close")

	if _, err := sess.mf.Write([]byte("echo __PTY_MARK__\n")); err != nil {
		t.Fatalf("写 master: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	buf := make([]byte, 4096)
	var seen []byte
	for time.Now().Before(deadline) {
		n, rerr := sess.mf.Read(buf)
		if n > 0 {
			seen = append(seen, buf[:n]...)
			if bytes.Contains(seen, []byte("__PTY_MARK__")) {
				return
			}
		}
		if rerr != nil {
			break
		}
	}
	t.Fatalf("未在 PTY 输出中看到回显标记，已收 %d 字节: %q", len(seen), string(seen))
}
