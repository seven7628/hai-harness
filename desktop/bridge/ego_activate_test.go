package main

// ego lite 激活钩子测试（2026-09，方案 1）：
//   - load_skill(ego-browser) 且引擎=ego → 发 ego_activate_request（当前运行周期首次）
//   - 非 ego-browser 技能 → 不触发
//   - 引擎=mcp → 不触发
//   - 去重：同运行周期二次加载不重复发
//   - bash 钩子：app 不在运行时执行 ego-browser 命令 → 重置并请求激活
//   - bash 钩子：app 在运行时 → 不打扰

import (
	"strings"
	"testing"
	"time"
)

// resetEgoActivateState 重置进程级激活状态（测试隔离；顺序执行即可）。
func resetEgoActivateState() {
	egoActivateMu.Lock()
	egoActivated = false
	egoLastAppSeen = false
	egoProbeAt = 0
	egoActivateMu.Unlock()
}

// requestCount 统计 out 里 ego_activate_request 事件数。
func requestCount(buf *syncBuf) int {
	n := 0
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, `"event_type":"ego_activate_request"`) {
			n++
		}
	}
	return n
}

// TestLoadSkillHookSendsRequest 引擎=ego + load_skill(ego-browser) → 发激活请求。
func TestLoadSkillHookSendsRequest(t *testing.T) {
	m, buf := testManager(t)
	m.setBrowserEngine(EngineEgo)
	resetEgoActivateState()

	hook := m.loadSkillOnLoadHook()
	hook("ego-browser")
	// 事件同步写 out（writeLine 直接写 buffer）——立即断言
	if n := requestCount(buf); n != 1 {
		t.Fatalf("应发 1 次激活请求, got %d\n%s", n, buf.String())
	}
}

// TestLoadSkillHookSkipsOtherSkills 非 ego-browser 技能不发。
func TestLoadSkillHookSkipsOtherSkills(t *testing.T) {
	m, buf := testManager(t)
	m.setBrowserEngine(EngineEgo)
	resetEgoActivateState()

	hook := m.loadSkillOnLoadHook()
	hook("playwright-cli")
	if n := requestCount(buf); n != 0 {
		t.Fatalf("其他技能不应触发, got %d", n)
	}
}

// TestLoadSkillHookSkipsMCPEngine 引擎=mcp 不发。
func TestLoadSkillHookSkipsMCPEngine(t *testing.T) {
	m, buf := testManager(t)
	m.setBrowserEngine(EngineMCP)
	resetEgoActivateState()

	hook := m.loadSkillOnLoadHook()
	hook("ego-browser")
	if n := requestCount(buf); n != 0 {
		t.Fatalf("引擎=mcp 不应触发, got %d", n)
	}
}

// TestLoadSkillHookDedupWithinRun 同运行周期二次触发只发一次。
func TestLoadSkillHookDedupWithinRun(t *testing.T) {
	m, buf := testManager(t)
	m.setBrowserEngine(EngineEgo)
	resetEgoActivateState()

	hook := m.loadSkillOnLoadHook()
	hook("ego-browser")
	hook("ego-browser")
	if n := requestCount(buf); n != 1 {
		t.Fatalf("去重失败：应只发 1 次, got %d", n)
	}
}

// TestBashHookWhenAppDown 引擎=ego + app 不在运行 + bash 含 ego-browser → 重置并请求激活。
func TestBashHookWhenAppDown(t *testing.T) {
	m, buf := testManager(t)
	m.setBrowserEngine(EngineEgo)
	resetEgoActivateState()

	// app 不在运行（直接置缓存为 false；避免真 pgrep 依赖环境）
	egoActivateMu.Lock()
	egoLastAppSeen = false
	egoProbeAt = time.Now().UnixNano() // 使缓存生效（2s 内不重探）
	egoActivated = false
	egoActivateMu.Unlock()

	hook := m.bashEgoBeforeExec()
	hook("ego-browser nodejs <<EOF ... EOF")
	if n := requestCount(buf); n != 1 {
		t.Fatalf("app 不在运行时应请求激活, got %d\n%s", n, buf.String())
	}
}

// TestBashHookWhenAppRunning 引擎=ego + app 在运行 + bash 含 ego-browser → 不请求（不打扰）。
func TestBashHookWhenAppRunning(t *testing.T) {
	m, buf := testManager(t)
	m.setBrowserEngine(EngineEgo)
	resetEgoActivateState()

	egoActivateMu.Lock()
	egoLastAppSeen = true // app 在跑
	egoProbeAt = time.Now().UnixNano()
	egoActivated = true // 已激活过（load_skill 路径）
	egoActivateMu.Unlock()

	hook := m.bashEgoBeforeExec()
	hook("ego-browser nodejs <<EOF ... EOF")
	if n := requestCount(buf); n != 0 {
		t.Fatalf("app 在运行且已激活过不应再请求, got %d", n)
	}
}

// TestBashHookSkipsNonEgoCmd bash 命令不含 ego-browser 不触发。
func TestBashHookSkipsNonEgoCmd(t *testing.T) {
	m, buf := testManager(t)
	m.setBrowserEngine(EngineEgo)
	resetEgoActivateState()

	egoActivateMu.Lock()
	egoLastAppSeen = false
	egoProbeAt = time.Now().UnixNano()
	egoActivated = false
	egoActivateMu.Unlock()

	hook := m.bashEgoBeforeExec()
	hook("ls -la")
	if n := requestCount(buf); n != 0 {
		t.Fatalf("非 ego-browser 命令不应触发, got %d", n)
	}
}
