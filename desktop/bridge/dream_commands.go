package main

// Dream 的设置面命令（dream_get / dream_set / dream_run_now）。
//
// 与 dream_scheduler.go 的分工：那边管"什么时候跑"，这里管"用户怎么改设置、
// 怎么手动跑一次、怎么看到跑过什么"。三条命令都不碰记忆文件本身——归档只走
// memory_dream 工具（宿主护栏在那里），命令面不开第二条写路径。

import (
	"fmt"
	"strings"
	"time"
)

// dreamRunsShown 设置页展示的运行记录条数上限。
//
// 20 条 = 三周左右，足够看出"最近还正常吗"；再多用户也不会翻，
// 而 dream_runs.jsonl 是追加文件，全量读进来只是白花内存。
const dreamRunsShown = 20

func (m *manager) handleDreamGet(c command) {
	// runs 必须是 []，不能是 null：readDreamRuns 在没有记录/没有文件时返回 nil，
	// 而 nil 切片会被 encoding/json 序列化成 null —— 前端已经从"非数组"兜住了，
	// 但契约上说好是数组就给数组（真机验收时 dream_get 的 runs 是 null）。
	runs := readDreamRuns(dreamRunsShown)
	if runs == nil {
		runs = []dreamRun{}
	}
	// 倒序：最新一轮在最上面。readDreamRuns 保持**存储顺序**（按时间正序，取"最近
	// N 条"从尾部取），只在出口翻一次——记录文件是追加写的，读时再翻不产生第二份
	// 真相。看运行记录想知道的是"昨晚跑得怎么样"，不是"第一夜跑得怎么样"。
	for i, j := 0, len(runs)-1; i < j; i, j = i+1, j-1 {
		runs[i], runs[j] = runs[j], runs[i]
	}
	m.resp(c, true, "", map[string]any{
		"settings": loadDreamSettings(),
		"runs":     runs,
		// running 由持有闩的一方给：前端据此把「立即运行」显示成进行中，
		// 而不是拿"有没有未完成的记录"去猜（进程被杀留下的半条记录会一直像在跑）。
		"running": m.dreamRunning(),
	})
}

func (m *manager) handleDreamSet(c command) {
	raw, present := c.Payload["enabled"]
	if !present {
		m.resp(c, false, "缺少 enabled 字段（Dream 开关）", nil)
		return
	}
	enabled, ok := raw.(bool)
	if !ok {
		m.resp(c, false, "enabled 必须是布尔值", nil)
		return
	}
	prov := strings.TrimSpace(str(c.Payload, "provider"))
	model := strings.TrimSpace(str(c.Payload, "model"))
	// provider 与 model 必须成对：只给一半时，派生会话会拿"这个 provider + 别的
	// provider 的模型"去装配 loop，错误要等到夜里跑起来才暴露。
	if (prov == "") != (model == "") {
		m.resp(c, false, "provider 与 model 必须一起设置（或一起留空 = 沿用当前默认模型）", nil)
		return
	}
	if prov != "" {
		if !m.provCfg.providerKnown(prov) {
			m.resp(c, false, fmt.Sprintf("未知的 provider：%s（请先在 Provider 页配置）", prov), nil)
			return
		}
		// 模型表非空时装错模型名一定跑不起来；空表（本地 vLLM 等）允许任意模型名，
		// 与 switch_model 的 fail-open 口径一致（见 providerConfig.allowUnlistedModel）。
		if m.provCfg.providerHasModels(prov) && !m.provCfg.hasModelIn(prov, model) {
			m.resp(c, false, fmt.Sprintf("模型 %s 不在 provider %s 的模型表里", model, prov), nil)
			return
		}
	}
	at := strings.TrimSpace(str(c.Payload, "at"))
	if at == "" {
		at = "03:00" // 缺省时刻：产品默认凌晨三点（与 dreamDue 的回退一致）
	}
	if !dreamClockValid(at) {
		m.resp(c, false, "时间格式必须是 HH:MM（例如 03:00）", nil)
		return
	}

	// LastRunDate 由调度器独占维护：这里必须保留磁盘上的值，否则"保存一次设置"
	// 就等于"今天还没跑过"，到点会再跑一遍（甚至会因为改设置而立刻补跑）。
	prev := loadDreamSettings()
	next := dreamSettings{
		Enabled:     enabled,
		Provider:    prov,
		Model:       model,
		At:          at,
		LastRunDate: prev.LastRunDate,
	}
	if err := saveDreamSettings(next); err != nil {
		m.resp(c, false, "保存 Dream 设置失败: "+err.Error(), nil)
		return
	}
	m.emitDreamChanged()
	m.resp(c, true, "", map[string]any{"settings": next})
}

func (m *manager) handleDreamRunNow(c command) {
	s := loadDreamSettings()
	bs, err := m.fireDream(s, time.Now())
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	m.resp(c, true, "", map[string]any{"session_id": bs.id})
}

// dreamClockValid 严格的 "HH:MM" 形状校验。
//
// parseClock 宽容（"3:0" 也算 3:00），那是给读旧配置用的；命令面是**写**路径，
// 放宽容会让用户输入的 "3:0" 落盘成非法形状，设置页回填时看着就像个 bug。
func dreamClockValid(s string) bool {
	if len(s) != 5 || s[2] != ':' {
		return false
	}
	h, mnt, ok := parseClock(s)
	if !ok {
		return false
	}
	return h >= 0 && h <= 23 && mnt >= 0 && mnt <= 59
}
