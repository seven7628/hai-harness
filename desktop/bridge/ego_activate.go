package main

// ego lite 窗口激活钩子（2026-09，方案 1）：
// 引擎 = ego 时，把 ego lite 窗口带到前台 + 提示「切到 agent 的 Space 查看」。
//
// 背景：ego lite 的 agent Space 默认后台运行不打扰用户；窗口 space 切换是纯 GUI
// 操作（ego 无 API 可切）。宿主能做的是「窗口带前」——用户看到窗口后点一下
// space 标签即可看到 agent 实时操作。
//
// 触发（两条路径，覆盖用户中途关 app 的场景）：
//  1. load_skill(ego-browser)：模型决定用 ego 的语义起点（引擎=ego + 首次）。
//  2. bash 执行 ego-browser CLI 且 app 刚被 CLI 自拉起（上次探测不在运行）：
//     ego-browser 在 app 关闭时会自动拉起 app 但窗口在后台——用户需要被提示。
//
// 去重：不按进程一次——按 app 生命周期。app 从「不在运行」变「在运行」时重置
// 激活标记；app 持续运行期间只激活一次（不反复抢焦点）。
//
// 执行位置（重要）：bridge 不直接执行激活——统一由 Electron 主进程跑 `open -a`
// （LaunchServices，无需 TCC 自动化授权；bridge 子进程跑 osascript 会因无授权失败，
// open 则无此问题且 app 未运行时可拉起）。bridge 只发 ego_activate_request 事件，
// 激活结果经 ego_activate_status 回 renderer 展示。

import (
	"os/exec"
	"strings"
	"sync"
	"time"
)

// egoActivateMu 保护激活状态（egoActivated + 上次 app 运行探测）。
var (
	egoActivateMu  sync.Mutex
	egoActivated   bool  // 当前 app 运行周期内已请求过激活
	egoLastAppSeen bool  // 上次探测 ego app 是否在运行（探测节流用）
	egoProbeAt     int64 // 上次探测时间戳（纳秒；节流：≥2s 才重探）
)

// egoSkillName ego 浏览器技能名（load_skill 触发判定）。
const egoSkillName = "ego-browser"

// loadSkillOnLoadHook 返回注入 skills.LoadSkillTool.OnLoad 的钩子：
// 加载 ego-browser 技能且引擎 = ego → 请求激活 ego lite 窗口（当前运行周期首次）。
func (m *manager) loadSkillOnLoadHook() func(name string) {
	return func(name string) {
		if name != egoSkillName {
			return
		}
		m.maybeActivateEgo()
	}
}

// bashEgoBeforeExec 返回注入 BashTool.BeforeExec 的钩子：
// bash 命令含 ego-browser 且引擎 = ego 时，若 app 当前不在运行（命令会自拉起它，
// 窗口在后台）→ 重置激活标记并请求激活（用户关闭 app 后 agent 继续用 CLI 的场景）。
// 若 app 在运行 → 不动作（load_skill 路径已覆盖首次；避免每条 heredoc 抢焦点）。
func (m *manager) bashEgoBeforeExec() func(cmd string) {
	return func(cmd string) {
		if !strings.Contains(cmd, "ego-browser") {
			return
		}
		if m.getBrowserEngine() != EngineEgo {
			return
		}
		if egoAppRunningCached() {
			return // app 在跑：不打扰（load_skill 已激活过；或用户正用着）
		}
		// app 不在运行：ego-browser CLI 将自拉起它（窗口后台）→ 请求激活。
		// 重置激活标记（新运行周期），并立即请求。
		egoActivateMu.Lock()
		egoActivated = false
		egoActivateMu.Unlock()
		m.maybeActivateEgo()
	}
}

// maybeActivateEgo 引擎 = ego 且当前运行周期内首次 → 发 ego_activate_request
// （Electron 主进程执行激活）。不阻塞 load_skill/bash 工具结果返回（事件异步送达）。
func (m *manager) maybeActivateEgo() {
	if m.getBrowserEngine() != EngineEgo {
		return
	}
	egoActivateMu.Lock()
	if egoActivated {
		egoActivateMu.Unlock()
		return
	}
	egoActivated = true
	egoActivateMu.Unlock()

	m.out.writeLine(map[string]any{
		"event_type": "ego_activate_request",
		"ok":         true,
		"message":    egoActivateHint,
	})
}

// egoActivateHint 激活提示文案（前端 toast 用；与 Electron 侧一致）。
const egoActivateHint = "ego lite 已带到前台——请在其窗口切到 agent 正在操作的 Space 查看实时进展（agent 的 Space 默认后台运行，不会自动抢占你的窗口）。"

// egoAppRunning 探测 ego lite 主进程是否在运行（pgrep；秒级）。
func egoAppRunning() bool {
	out, err := exec.Command("pgrep", "-f", egoAppPattern).Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// egoAppRunningCached 带节流的 app 运行探测（≥2s 缓存，避免每条 bash 命令都 pgrep）。
func egoAppRunningCached() bool {
	now := time.Now().UnixNano()
	egoActivateMu.Lock()
	defer egoActivateMu.Unlock()
	if now-egoProbeAt < 2e9 { // 2s 内用缓存
		return egoLastAppSeen
	}
	egoProbeAt = now
	egoLastAppSeen = egoAppRunning()
	return egoLastAppSeen
}
