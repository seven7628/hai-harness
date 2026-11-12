package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/seven7628/hai-harness/plugin/browser/cdp"
)

// TestEmbedClientDisconnectNoPanic 回归测试（2026-08-29 崩溃根因）：
// ws 客户端断连（读循环 close(cl.send)）与 runRelay 广播（向 cl.send 发送）并发时，
// 不得再出现 "send on closed channel" panic。
//
// 崩溃现场：旧代码 handleWS 读循环断开时无锁 close(cl.send)，而 runRelay 持 s.mu
// 遍历 s.clients 发送 —— client 仍在 map 但 send 已关 → 发送即 panic，整个 bridge
// 进程崩溃（刷新页面触发：Electron 内嵌视图销毁 → debugger proxy 关闭 → ws 断连）。
// 修复后：close 统一在持 s.mu 的摘除路径执行，与 runRelay 发送串行化。
func TestEmbedClientDisconnectNoPanic(t *testing.T) {
	fwd := cdp.New("", "", "about:blank", nil)
	srv := newEmbedServer(fwd)
	srv.upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	ts := httptest.NewServer(http.HandlerFunc(srv.handleWS))
	defer ts.Close()
	defer srv.Stop()

	// 启动 runRelay（真实路径）：srv.Start 会拉起它；此处手动起以保证受控
	relayStop := make(chan struct{})
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		// 等价 srv.Start 里的 runRelay 主循环，但事件由测试注入
		events := make(chan map[string]any, 256)
		go fwd.Relay(events)
		for {
			select {
			case <-relayStop:
				return
			default:
			}
			select {
			case ev := <-events:
				raw, _ := json.Marshal(ev)
				srv.mu.Lock()
				for cl := range srv.clients {
					select {
					case cl.out <- raw:
					case <-time.After(2 * time.Second):
					}
				}
				srv.mu.Unlock()
			default:
				time.Sleep(1 * time.Millisecond)
			}
		}
	}()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
			if err != nil {
				return
			}
			// 让广播 goroutine 有机会往 send 塞事件（增加命中竞态窗口的概率）。
			// 用 Level=browser 事件：Relay 原样回推，不依赖 target/session 解析。
			for j := 0; j < 5; j++ {
				fwd.IngestTagged(cdp.TaggedEvent{TargetID: "t1", Level: "browser", Event: cdp.Event{Method: "Target.targetInfoChanged"}})
				time.Sleep(1 * time.Millisecond)
			}
			_ = conn.Close() // 对端断开 → 读循环 ReadMessage 返回 error → 断连路径
		}()
	}
	wg.Wait()
	close(relayStop)
	<-relayDone

	// 等所有 handleWS 收尾（摘除 + 写协程退出）
	time.Sleep(150 * time.Millisecond)
	t.Log("30 次连/断 + 并发广播完成，无 send on closed channel panic")
}
