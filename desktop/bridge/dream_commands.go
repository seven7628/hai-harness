package main

// Dream 的设置面命令（dream_get / dream_set / dream_run_now / dream_detail）。
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

// dreamDetailGone 详情取不到时的**唯一**文案（前端直接展示，不再加工）。
//
// 说清楚"为什么没有"：记录文件只保留最近 200 行（dreamRunsKeep 轮转），
// 用户点到一条老记录时不是出错，是那一轮的详情压根没留在本地。含糊的
// "未找到"会让人以为记录丢了/程序坏了，去翻日志。
const dreamDetailGone = "该轮详情已不在本地记录里（记录只保留最近 200 行）"

// handleDreamDetail 某一轮运行记录的详情：整条记录（含 result 全文与 steps 时间线）。
//
// 为什么整条记录原样返回、而不是挑字段：详情视图要显示 起止/耗时/provider/model/
// 状态/逐库/时间线/错误——就是记录的全部内容。逐字段挑选会让前端与 Go 结构体
// 两处各维护一份字段表，加一个字段就要改两边（且漏改时表现为"详情里永远没有它"）。
// 记录里没有密钥（provider 只是名字），可以整条给。
func (m *manager) handleDreamDetail(c command) {
	sid := strings.TrimSpace(str(c.Payload, "session_id"))
	if sid == "" {
		// 空 session_id 不可能是"记录过期"：这是调用方 bug，文案要能区分。
		m.resp(c, false, "缺少 session_id（要查哪一轮的运行记录）", nil)
		return
	}
	run, found := findDreamRun(sid)
	if !found {
		m.resp(c, false, dreamDetailGone, nil)
		return
	}
	m.resp(c, true, "", map[string]any{"run": run, "found": true})
}

// findDreamRun 按 session_id 精确找一条运行记录（不受 dreamRunsShown=20 限制）。
//
// 从文件**尾部**往前找：同一个 session 正常只有一条记录，但重试/看门狗可能留下
// 重复（后者已按 FinishedAt 幂等收口），取最新的那条最符合用户点的那一行。
func findDreamRun(sessionID string) (dreamRun, bool) {
	p := dreamRunsPath()
	if p == "" {
		return dreamRun{}, false
	}
	// 读盘与 completeDreamRun 的"整份重写"互斥：否则可能在 rename 前后读到半份。
	dreamRunsMu.Lock()
	defer dreamRunsMu.Unlock()
	records := readDreamRuns(0)
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].SessionID == sessionID {
			return records[i], true
		}
	}
	return dreamRun{}, false
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
