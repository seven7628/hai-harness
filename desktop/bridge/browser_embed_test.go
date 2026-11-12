package main

import (
	"bufio"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/plugin/browser/cdp"
)

// TestWaitEmbedAttachedEventDriven 事件驱动验证：attach 完成信号到达后，
// waitEmbedAttached 立即返回（不等满 timeout）——替代 100ms 轮询 + 2s sleep。
func TestWaitEmbedAttachedEventDriven(t *testing.T) {
	m := &manager{embed: embedBridge{}}
	m.embed.attachedCh = make(chan struct{})

	// 后台 goroutine 模拟前端 attach：50ms 后 close 信号
	go func() {
		time.Sleep(50 * time.Millisecond)
		m.embed.mu.Lock()
		if !m.embed.attachedOnce {
			m.embed.attachedOnce = true
			close(m.embed.attachedCh)
		}
		m.embed.mu.Unlock()
	}()

	start := time.Now()
	m.waitEmbedAttached(5 * time.Second) // 超时给足，若事件驱动生效应 ~50ms 返回
	elapsed := time.Since(start)
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("waitEmbedAttached 应事件驱动返回（~50ms），实际 %v（仍轮询/超时？）", elapsed)
	}
	t.Logf("attach 信号到达后 %v 返回（事件驱动生效）", elapsed)
}

// TestWaitEmbedAttachedTimeout 超时放行：attach 信号不来，到 timeout 返回（不阻塞）。
func TestWaitEmbedAttachedTimeout(t *testing.T) {
	m := &manager{embed: embedBridge{}}
	m.embed.attachedCh = make(chan struct{}) // 永不 close

	start := time.Now()
	m.waitEmbedAttached(100 * time.Millisecond)
	elapsed := time.Since(start)
	if elapsed < 80*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("超时应 ~100ms 放行，实际 %v", elapsed)
	}
}

// targetIDsOf 读转发器页表里的 targetId 集合（Target.getTargets 只列绑定了真实视图的 target）。
func targetIDsOf(t *testing.T, f *cdp.Forwarder) []string {
	t.Helper()
	res, err := f.Handle("Target.getTargets", nil)
	if err != nil {
		t.Fatalf("Target.getTargets: %v", err)
	}
	infos, _ := res["targetInfos"].([]any)
	out := make([]string, 0, len(infos))
	for _, it := range infos {
		if m, ok := it.(map[string]any); ok {
			if id, _ := m["targetId"].(string); id != "" {
				out = append(out, id)
			}
		}
	}
	return out
}

func hasID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestEmbedAttachDestroyedRemovesTarget：阶段 3（P1）——Electron 侧推来
// {"type":"destroyed","targetId":...}（用户关标签 / AI close_page）必须把该 target 从
// 转发器页表移除。补这条链路之前 Electron 关视图**不通知** Go 侧，页表留着幽灵页
// （list_pages 仍列它、命令路由报 target 不存在），见 docs/RIGHT_PANEL_TABS_PLAN.md §11.1。
//
// 走**真实 embedAttach**（连一条假 Electron 调试代理的 TCP 行协议）而不是直接调
// MultiClient.OnDestroyed：被测对象是 browser_embed.go 里那行注入，直调会绕过它。
func TestEmbedAttachDestroyedRemovesTarget(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("端口绑定不可用（沙箱）: %v", err)
	}
	defer ln.Close()
	// 假 Electron 调试代理：enumerate 回两个视图；destroyed 推送必须走**同一条连接**
	//（MultiClient 的 readLoop 只读它自己那条 socket —— 换个连接推等于推给了空气）。
	pushCh := make(chan any, 8)
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer conn.Close()
		w := bufio.NewWriter(conn)
		// 两个协程共用这条连接写行（推送协程 + enumerate 应答）：bufio.Writer 非并发安全，
		// 无锁写会触发 -race（门禁 `go test -race desktop/bridge` 必现）。
		var wmu sync.Mutex
		writeLine := func(v any) {
			raw, _ := json.Marshal(v)
			wmu.Lock()
			defer wmu.Unlock()
			w.Write(append(raw, '\n'))
			w.Flush()
		}
		go func() { // 推送协程（同一条连接）
			for v := range pushCh {
				writeLine(v)
			}
		}()
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			if m["type"] == "enumerate" {
				writeLine(map[string]any{"type": "targets", "rid": m["rid"], "targets": []any{
					map[string]any{"id": "t1", "url": "file:///a.html", "title": "A"},
					map[string]any{"id": "t2", "url": "file:///b.html", "title": "B"},
				}})
			}
		}
	}()

	fwd := cdp.New("go-code-embed", "", "about:blank", nil)
	m := &manager{embed: embedBridge{fwd: fwd}}
	m.embed.attachedCh = make(chan struct{})
	if err := m.embedAttach(ln.Addr().String()); err != nil {
		t.Fatalf("embedAttach: %v", err)
	}
	push := func(v any) { pushCh <- v }

	if ids := targetIDsOf(t, fwd); !hasID(ids, "t1") || !hasID(ids, "t2") {
		t.Fatalf("attach 后页表应含 t1/t2，实际 %v", ids)
	}

	push(map[string]any{"type": "destroyed", "targetId": "t1"})
	deadline := time.Now().Add(3 * time.Second)
	for {
		ids := targetIDsOf(t, fwd)
		if !hasID(ids, "t1") {
			if !hasID(ids, "t2") {
				t.Fatalf("只应移除 t1，实际 %v", ids)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("destroyed 推送后 t1 仍在页表里（实际 %v）", ids)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 幂等：同一条 destroyed 再推一次（用户重复点关闭 / 与 close 应答路径叠加）不得 panic、
	// 不得把别的 target 带走（RemoveTarget 对未知 id 早退）。
	push(map[string]any{"type": "destroyed", "targetId": "t1"})
	push(map[string]any{"type": "destroyed", "targetId": ""})
	time.Sleep(200 * time.Millisecond)
	if ids := targetIDsOf(t, fwd); !hasID(ids, "t2") {
		t.Fatalf("重复/空 id 推送后 t2 应仍在，实际 %v", ids)
	}
}
