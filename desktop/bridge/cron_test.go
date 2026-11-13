package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/cron"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/skills"
	"github.com/seven7628/hai-harness/subagent"
	"github.com/seven7628/hai-harness/tools"
)

// cronTestManager：带 cron store 的 manager（HOME 隔离；cron.json/账本写临时目录）。
func cronTestManager(t *testing.T) (*manager, *syncBuf) {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // 隔离 ~/.go-code（settings/cron 落盘）
	buf := &syncBuf{}
	m := &manager{
		workspaces: map[string]*workspaceRuntime{},
		provCfg:    loadProviderConfig(),
		out:        newOut(buf),
	}
	m.cron = cron.New(cron.WithFileStore(homeCfgPath("cron.json"), homeCfgPath("cron_runs.jsonl")))
	return m, buf
}

func TestLoadCronSettingsDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cs := loadCronSettings()
	if !cs.enabledOn() || !cs.autoCleanOn() {
		t.Fatalf("缺省应全开，得到 %+v", cs)
	}
	// 写设置：关闭定时任务
	home := t.TempDir()
	dir := filepath.Join(home, ".go-code")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"cron":{"enabled":false,"auto_clean":true}}`), 0o644)
	t.Setenv("HOME", home)
	cs2 := loadCronSettings()
	if cs2.enabledOn() {
		t.Fatal("enabled=false 应被读取")
	}
	if !cs2.autoCleanOn() {
		t.Fatal("auto_clean=true 应保留")
	}
}

func TestCronCommandsCRUD(t *testing.T) {
	m, buf := cronTestManager(t)
	ws := t.TempDir()
	m.dispatch(cmd(1, "cron_create", map[string]any{"workspace": ws, "cron": "3 9 * * *", "prompt": "提醒打卡", "recurring": true}))
	var id string
	for _, l := range readLines(buf) {
		if l["ok"] == true {
			if d, ok := l["data"].(map[string]any); ok {
				id, _ = d["id"].(string)
			}
		}
	}
	if id == "" {
		t.Fatal("cron_create 应返回 id")
	}
	if m.cron.Get(id) == nil {
		t.Fatal("create 后应能取到任务")
	}
	// list 输出应含任务 id
	m.dispatch(cmd(2, "cron_list", nil))
	raw := buf.String()
	if !strings.Contains(raw, id) {
		t.Fatalf("cron_list 应含任务 id %s，输出: %s", id, raw)
	}
	buf.Reset()
	// update
	m.dispatch(cmd(3, "cron_update", map[string]any{"id": id, "prompt": "新提醒", "cron": "7 9 * * *"}))
	j := m.cron.Get(id)
	if j.Prompt != "新提醒" || j.Cron != "7 9 * * *" {
		t.Fatalf("update 后应为新值，得到 %+v", j)
	}
	// delete
	m.dispatch(cmd(4, "cron_delete", map[string]any{"id": id}))
	if m.cron.Get(id) != nil {
		t.Fatal("delete 后不应存在")
	}
	// 非法表达式 create 应报错（先清缓冲，避免读到上一条响应）
	buf.Reset()
	m.dispatch(cmd(5, "cron_create", map[string]any{"workspace": ws, "cron": "bad", "prompt": "x"}))
	if lines := readLines(buf); len(lines) == 0 || lines[0]["ok"] != false {
		t.Fatal("非法 cron 应报错")
	}
}

func TestCronToolsGatedBySettings(t *testing.T) {
	assertNoUserPrices(t)
	home := t.TempDir()
	dir := filepath.Join(home, ".go-code")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"cron":{"enabled":false}}`), 0o644)
	t.Setenv("HOME", home)

	reg := subagent.NewRegistry()
	bg := func() context.Context { return context.Background() }
	sk, _ := skills.NewRegistry(t.TempDir())
	cs := cron.New() // 非 nil store（触发注册判断）
	cfg := loadProviderConfig()

	build := func() *tools.Engine {
		eng, _ := newSessionEngine(t.TempDir(), "s1", cs, cfg, "deepseek", cfg.defaultModel(),
			string(provider.ReasoningEffortLevelLow), bg, reg, events.NewQuestionWaiter(0), sk, buildSubagentRegistry(t.TempDir()), nil, nil, profiles["code"], nil, nil, nil, nil, nil)
		return eng
	}
	// enabled=false → 不注册
	eng := build()
	for _, name := range []string{"CronCreate", "CronList", "CronUpdate", "CronDelete"} {
		if _, err := eng.GetTool(context.Background(), name); err == nil {
			t.Fatalf("enabled=false 时 %s 不应注册", name)
		}
	}
	// enabled=true → 注册
	_ = os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"cron":{"enabled":true}}`), 0o644)
	eng2 := build()
	for _, name := range []string{"CronCreate", "CronList", "CronUpdate", "CronDelete"} {
		if _, err := eng2.GetTool(context.Background(), name); err != nil {
			t.Fatalf("enabled=true 时 %s 应注册: %v", name, err)
		}
	}
}

func TestCronTickDisabledSkips(t *testing.T) {
	m, _ := cronTestManager(t)
	home := t.TempDir()
	dir := filepath.Join(home, ".go-code")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"cron":{"enabled":false}}`), 0o644)
	t.Setenv("HOME", home)

	now := time.Now()
	ws := t.TempDir()
	// 造已到点任务（next 过去 → Due 命中）
	job, _ := m.cron.Add("3 9 * * *", "提醒", true, ws, "s1", now.Add(-time.Hour))
	_ = m.cron.SetNextRun(job.ID, now.Add(-time.Minute))

	before := len(m.cron.List())
	m.cronTick(now)
	if got := len(m.cron.List()); got != before {
		t.Fatalf("disabled 不应删除任务：before=%d after=%d", before, got)
	}
	led := m.cron.Ledger()
	runs := led.ReadAll()
	if len(runs) != 1 || runs[0].Status != cron.StatusSkipped {
		t.Fatalf("期望一条 skipped 记录，得到 %+v", runs)
	}
	if j := m.cron.Get(job.ID); j.RunCount != 0 {
		t.Fatalf("disabled 不应计数，得到 %d", j.RunCount)
	}
}

func TestCronTickFireCreatesSession(t *testing.T) {
	m, _ := cronTestManager(t)
	now := time.Now()
	ws := t.TempDir()
	m.cron.Add("3 9 * * *", "行情刷新", true, ws, "s1", now.Add(-time.Hour))
	_ = m.cron.SetNextRun("c1", now.Add(-time.Minute))

	m.cronTick(now)
	j := m.cron.Get("c1")
	if j == nil {
		t.Fatal("recurring 触发后任务应仍在")
	}
	if j.RunCount != 1 {
		t.Fatalf("run_count 应为 1，得到 %d", j.RunCount)
	}
	wr := m.runtime(ws)
	wr.mu.Lock()
	n := len(wr.sessions)
	wr.mu.Unlock()
	if n == 0 {
		t.Fatal("触发后应新建一个派生会话")
	}
}

func TestCronDeleteRemovesHistoryDir(t *testing.T) {
	m, buf := cronTestManager(t)
	ws := t.TempDir()
	// 手动注册 OnDelete（cronTestManager 不走 startCron，模拟宿主接线）
	m.cron.OnDelete(func(job *cron.Job) {
		if job == nil {
			return
		}
		dir := filepath.Join(cronSnapshotsRoot(), workspaceKey(job.Workspace), job.ID)
		_ = os.RemoveAll(dir)
	})
	m.cron.Add("3 9 * * *", "提醒", true, ws, "s1", time.Now())
	// 模拟已产生的独立历史文件
	jobDir := filepath.Join(cronSnapshotsRoot(), workspaceKey(ws), "c1")
	_ = os.MkdirAll(jobDir, 0o755)
	_ = os.WriteFile(filepath.Join(jobDir, "s-1.jsonl"), []byte("{}"), 0o644)
	_ = os.WriteFile(filepath.Join(jobDir, "r1.jsonl"), []byte("{}"), 0o644)

	m.dispatch(cmd(1, "cron_delete", map[string]any{"id": "c1"}))
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Fatalf("删除任务后历史目录应被清理，仍存在: %v", err)
	}
	if m.cron.Get("c1") != nil {
		t.Fatal("任务应从 store 移除")
	}
	_ = buf
}

func TestCronRunDetailReadsEventLog(t *testing.T) {
	m, _ := cronTestManager(t)
	ws := t.TempDir()
	led := m.cron.Ledger()
	m.cron.Add("3 9 * * *", "提醒", true, ws, "s1", time.Now())
	// 入账一条已完成运行
	now := time.Now().Add(-time.Hour)
	r := led.AppendStart(m.cron.Get("c1"), workspaceKey(ws), now)
	led.Start(r.RunID, "s-cron-1", now)
	led.Finish(r.RunID, cron.StatusSuccess, "", "完成", now, now.Add(time.Minute))
	// 写派生会话事件日志（独立目录）
	jobDir := filepath.Join(cronSnapshotsRoot(), workspaceKey(ws), "c1")
	_ = os.MkdirAll(jobDir, 0o755)
	ev := `{"event_type":"user_inputs_consumed","session_id":"s-cron-1","user_texts":["提醒"],"timestamp":1}` + "\n" +
		`{"event_type":"content_chunk","run_id":"r1","content":"好的","timestamp":2}` + "\n" +
		`{"event_type":"agent_end","run_id":"r1","content":"完成","timestamp":3}` + "\n"
	_ = os.WriteFile(filepath.Join(jobDir, "s-cron-1.jsonl"), []byte(ev), 0o644)

	d := m.cronRunDetailData("c1", r.RunID)
	// 新结构：返回 snapshot（事件日志聚合，与主对话页恢复同构）
	snap, ok := d["snapshot"].(map[string]any)
	if !ok || snap == nil {
		t.Fatalf("详情应返回 snapshot（事件日志聚合），得到 %v", d["snapshot"])
	}
	events, ok := snap["events"].([]json.RawMessage)
	if !ok || len(events) != 3 {
		t.Fatalf("snapshot.events 应为 3 条事件（consumed/chunk/end），得到 %T %v", snap["events"], snap["events"])
	}
	if d["found"] != true {
		t.Fatal("应 found=true（事件日志存在）")
	}
	if d["status"] != cron.StatusSuccess || d["session_id"] != "s-cron-1" {
		t.Errorf("元数据错误: %v", d)
	}
}
