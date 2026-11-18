package builtin

// http_get 的**连接复用**测试（2026-11-17，第 6 步）。
//
// 改造前：每次调用 new 一个 http.Client ⇒ 每次都是**新连接池** ⇒
// 连续调用每次都要重新 TCP + TLS 握手；且响应体只读了上限那点、**没 drain**
// ⇒ 连接连归还都做不到。
//
// 现在：客户端按工具实例缓存（连接池跨调用复用）+ 响应体读完后 drain（≤64 KB）
// ⇒ 第二次调用直接复用连接。

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestHTTPGetReusesConnection 连续两次请求 ⇒ 只建立**一条**连接。
func TestHTTPGetReusesConnection(t *testing.T) {
	var conns int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok-body")
	}))
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateNew {
			atomic.AddInt32(&conns, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	ht := NewHTTPTool(5 * time.Second)
	ht.AllowPrivate = true // httptest 是 127.0.0.1
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		out, err := ht.Call(ctx, "", `{"url":"`+srv.URL+`"}`)
		if err != nil {
			t.Fatalf("第 %d 次调用失败: %v", i+1, err)
		}
		if !strings.Contains(out, "ok-body") {
			t.Fatalf("第 %d 次调用结果不对: %q", i+1, out)
		}
	}
	if n := atomic.LoadInt32(&conns); n != 1 {
		t.Errorf("3 次调用应复用 1 条连接，实际建立了 %d 条（连接池没复用）", n)
	}
}

// TestHTTPGetRespectsContextCancel 已取消的 ctx ⇒ 立刻返回（不挂着等）。
func TestHTTPGetRespectsContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		fmt.Fprint(w, "late")
	}))
	defer srv.Close()

	ht := NewHTTPTool(5 * time.Second)
	ht.AllowPrivate = true
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := ht.Call(ctx, "", `{"url":"`+srv.URL+`"}`)
	if err == nil {
		t.Fatalf("取消后应当报错")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("取消后返回太慢: %v", d)
	}
}
