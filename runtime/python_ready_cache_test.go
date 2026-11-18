package runtime

// 就绪快路径的回归网（第 4 轮爬山：Ensure 每次调用都起一次进程自检 → 20.5 ms）。
//
// 契约：
//  1. venv 指纹一致 + 自检时间还新鲜 ⇒ **不起任何进程**（Status 不探、Ensure 不跑）；
//  2. 指纹不一致（venv 被动过）或标记是老格式（没指纹）⇒ 探一次（行为同改造前），
//     并把新指纹写回标记 ⇒ 下一次回到快路径；
//  3. 探不过 ⇒ 判定未就绪（与改造前一致：宁可说不可用，也不进坏环境）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// probeCalls 数「探了一次解释器」的次数（`-c import sys`）。
func probeCalls(f *fakeCmd) int { return f.count("-c import sys") }

// TestEnsureFastPathSkipsProbe：就绪且指纹一致 ⇒ Ensure **不起进程**。
func TestEnsureFastPathSkipsProbe(t *testing.T) {
	f := &fakeCmd{}
	rt := newTestRuntime(t, f)
	if _, err := rt.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 清空调用记录
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()

	if _, err := rt.Ensure(context.Background()); err != nil {
		t.Fatalf("已就绪环境再 Ensure 应成功: %v", err)
	}
	if n := probeCalls(f); n != 0 {
		t.Errorf("快路径不该起进程自检，却探了 %d 次", n)
	}
	if n := f.count("pip install"); n != 0 {
		t.Errorf("快路径不该重跑 pip，got %d", n)
	}
}

// TestStatusFastPathNoProbe：Status 本身（不经 Ensure）也不该起进程。
func TestStatusFastPathNoProbe(t *testing.T) {
	f := &fakeCmd{}
	rt := newTestRuntime(t, f)
	if _, err := rt.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()

	st, err := rt.Status(context.Background())
	if err != nil {
		t.Fatalf("Status 不应报错: %v", err)
	}
	if !st.Ready {
		t.Fatalf("已就绪应判为就绪: %+v", st)
	}
	if n := probeCalls(f); n != 0 {
		t.Errorf("指纹一致时 Status 不该起进程，却探了 %d 次", n)
	}
	if st.RefreshMarker {
		t.Error("指纹一致且自检时间新鲜 ⇒ 不需要刷新标记")
	}
}

// TestVenvChangedTriggersProbeOnce：venv 被动过（改了解释器文件）⇒ 探一次，
// 并把新指纹写回标记 ⇒ 再来一次又回到快路径。
func TestVenvChangedTriggersProbeOnce(t *testing.T) {
	f := &fakeCmd{}
	rt := newTestRuntime(t, f)
	if _, err := rt.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 动一下解释器（改内容 ⇒ size/mtime 变 ⇒ 指纹失配）
	py := filepath.Join(rt.root(), "venv", "bin", "python")
	if err := os.WriteFile(py, []byte("#!/bin/sh\n# touched\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()

	if _, err := rt.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure 应成功: %v", err)
	}
	if n := probeCalls(f); n != 1 {
		t.Errorf("venv 被动过后应探 1 次，却探了 %d 次", n)
	}
	// 标记应被刷新 ⇒ 下一次不再探
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	if _, err := rt.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := probeCalls(f); n != 0 {
		t.Errorf("刷新标记后应回到快路径，却探了 %d 次", n)
	}
}

// TestOldMarkerWithoutStampProbesOnce：老标记（没有 venv 指纹）⇒ 探一次并升级标记。
func TestOldMarkerWithoutStampProbesOnce(t *testing.T) {
	f := &fakeCmd{}
	rt := newTestRuntime(t, f)
	if _, err := rt.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 把标记降级成老格式（抹掉 venv_stamp / probed_at）
	path := filepath.Join(rt.root(), readyMarker)
	old := `{"version":1,"requirements":"` + fingerprint(requirementsLock) + `","base":"","venv_python":"` +
		filepath.Join(rt.root(), "venv", "bin", "python") + `","created_at":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	if _, err := rt.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure 应成功: %v", err)
	}
	if n := probeCalls(f); n != 1 {
		t.Errorf("老标记应探 1 次，却探了 %d 次", n)
	}
	// 升级后的标记应带指纹 ⇒ 下一次走快路径
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	if _, err := rt.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := probeCalls(f); n != 0 {
		t.Errorf("老标记升级后应回到快路径，却探了 %d 次", n)
	}
}

// TestStaleProbeIntervalTriggersReprobe：自检时间过旧（超过 probeInterval）⇒ 再探一次。
func TestStaleProbeIntervalTriggersReprobe(t *testing.T) {
	f := &fakeCmd{}
	rt := newTestRuntime(t, f)
	if _, err := rt.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 把标记里的自检时间改成「很久以前」
	path := filepath.Join(rt.root(), readyMarker)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m readyInfo
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m.ProbedAt = time.Now().Add(-2 * probeInterval).Unix()
	if err := writeMarker(path, m); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	if _, err := rt.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := probeCalls(f); n != 1 {
		t.Errorf("自检时间过旧应探 1 次，却探了 %d 次", n)
	}
}
