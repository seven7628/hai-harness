package main

// Dream 设置/调度的纯逻辑测试（写入路径用 dreamDirOverride 隔离到临时目录，
// 不碰真实 ~/.go-code：那里是用户数据）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withDreamDir 把 Dream 的配置与记录落点切到临时目录。
func withDreamDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := dreamDirOverride
	dreamDirOverride = dir
	t.Cleanup(func() { dreamDirOverride = prev })
	return dir
}

func TestDreamDue(t *testing.T) {
	// 固定"今天"：用本地时区构造，避免 UTC 与本地日期的分歧。
	day := time.Date(2026, 11, 20, 0, 0, 0, 0, time.Local)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }

	cases := []struct {
		name     string
		settings dreamSettings
		now      time.Time
		want     bool
	}{
		{
			name:     "未到点不跑",
			settings: dreamSettings{Enabled: true, At: "03:00"},
			now:      at(2, 59),
			want:     false,
		},
		{
			name:     "到点就跑",
			settings: dreamSettings{Enabled: true, At: "03:00"},
			now:      at(3, 0),
			want:     true,
		},
		{
			name:     "过点也跑（迟到的补跑：桥在 03:00 之后才启动）",
			settings: dreamSettings{Enabled: true, At: "03:00"},
			now:      at(9, 30),
			want:     true,
		},
		{
			name:     "今天跑过就不再跑",
			settings: dreamSettings{Enabled: true, At: "03:00", LastRunDate: "2026-11-20"},
			now:      at(23, 59),
			want:     false,
		},
		{
			name:     "昨天跑过不影响今天",
			settings: dreamSettings{Enabled: true, At: "03:00", LastRunDate: "2026-11-19"},
			now:      at(3, 0),
			want:     true,
		},
		{
			name:     "关闭时永远不跑",
			settings: dreamSettings{Enabled: false, At: "03:00"},
			now:      at(12, 0),
			want:     false,
		},
		{
			name:     "时刻非法 → 回落 03:00（02:59 不跑）",
			settings: dreamSettings{Enabled: true, At: "99:99"},
			now:      at(2, 59),
			want:     false,
		},
		{
			name:     "时刻非法 → 回落 03:00（03:00 跑）",
			settings: dreamSettings{Enabled: true, At: "99:99"},
			now:      at(3, 0),
			want:     true,
		},
		{
			name:     "时刻为空 → 回落 03:00",
			settings: dreamSettings{Enabled: true},
			now:      at(3, 5),
			want:     true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dreamDue(c.settings, c.now); got != c.want {
				t.Fatalf("dreamDue(%+v, %s) = %v, want %v", c.settings, c.now.Format(time.RFC3339), got, c.want)
			}
		})
	}
}

func TestParseClock(t *testing.T) {
	cases := []struct {
		in   string
		h, m int
		ok   bool
	}{
		{"03:00", 3, 0, true},
		{" 23:59 ", 23, 59, true},
		{"00:00", 0, 0, true},
		{"24:00", 0, 0, false}, // 小时越界
		{"12:60", 0, 0, false}, // 分钟越界
		{"12", 0, 0, false},    // 不是两段
		{"12:34:56", 0, 0, false},
		{"ab:cd", 0, 0, false}, // 非数字（atoiSafe 返回 -1）
		{"", 0, 0, false},
	}
	for _, c := range cases {
		h, m, ok := parseClock(c.in)
		if ok != c.ok || (ok && (h != c.h || m != c.m)) {
			t.Fatalf("parseClock(%q) = (%d,%d,%v), want (%d,%d,%v)", c.in, h, m, ok, c.h, c.m, c.ok)
		}
	}
}

func TestDreamClockValid(t *testing.T) {
	// 写路径比读路径严格：形状必须是 HH:MM（两位补零），parseClock 宽容的写法不认。
	for _, in := range []string{"03:00", "23:59", "00:00"} {
		if !dreamClockValid(in) {
			t.Fatalf("dreamClockValid(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"3:00", "03:0", "24:00", "03:60", "0300", "", "03:00:00"} {
		if dreamClockValid(in) {
			t.Fatalf("dreamClockValid(%q) = true, want false", in)
		}
	}
}

func TestDreamSettingsRoundTrip(t *testing.T) {
	dir := withDreamDir(t)

	want := dreamSettings{Enabled: true, Provider: "deepseek", Model: "deepseek-chat", At: "04:30", LastRunDate: "2026-11-19"}
	if err := saveDreamSettings(want); err != nil {
		t.Fatalf("saveDreamSettings: %v", err)
	}
	if got := loadDreamSettings(); got != want {
		t.Fatalf("读回不一致: got %+v, want %+v", got, want)
	}

	// 权限：里面有用户选定的 provider/model，按配置文件对待（0600）。
	p := filepath.Join(dir, "dream.json")
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Fatalf("%s 权限 = %o, want 600", p, mode)
	}

	// 文件不存在 / 内容损坏 → 默认 03:00，开关按关处理（不猜用户意图）。
	if err := os.Remove(p); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := loadDreamSettings(); got.Enabled || got.At != "03:00" {
		t.Fatalf("缺文件时应回落默认（关闭 + 03:00），got %+v", got)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write bad json: %v", err)
	}
	if got := loadDreamSettings(); got.Enabled || got.At != "03:00" {
		t.Fatalf("坏文件时应回落默认（关闭 + 03:00），got %+v", got)
	}

	// 缺 at 字段的老文件 → 补默认时刻，其余字段照读。
	if err := os.WriteFile(p, []byte(`{"enabled":true,"provider":"openai","model":"gpt-4o"}`), 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	if got := loadDreamSettings(); !got.Enabled || got.At != "03:00" || got.Provider != "openai" {
		t.Fatalf("老文件读错: %+v", got)
	}
}

func TestReadDreamRuns(t *testing.T) {
	dir := withDreamDir(t)
	p := filepath.Join(dir, "dream_runs.jsonl")

	base := time.Date(2026, 11, 1, 3, 0, 0, 0, time.Local)
	var buf []byte
	for i := 0; i < 5; i++ {
		rec, _ := json.Marshal(dreamRun{
			StartedAt: base.Add(time.Duration(i) * 24 * time.Hour),
			Provider:  "deepseek",
			Model:     "deepseek-chat",
			SessionID: "s" + string(rune('0'+i)),
		})
		buf = append(buf, rec...)
		buf = append(buf, '\n')
		// 坏行（半截 JSON / 空行）混在中间：不能因此丢掉其余记录。
		buf = append(buf, []byte("{\"started_at\":")...)
		buf = append(buf, '\n')
	}
	buf = append(buf, '\n')
	if err := os.WriteFile(p, buf, 0o600); err != nil {
		t.Fatalf("write runs: %v", err)
	}

	all := readDreamRuns(0)
	if len(all) != 5 {
		t.Fatalf("坏行应跳过，得到 %d 条，want 5", len(all))
	}
	last := all[len(all)-1]
	if last.SessionID != "s4" || !last.StartedAt.Equal(base.Add(4*24*time.Hour)) {
		t.Fatalf("顺序/内容不对: %+v", last)
	}

	recent := readDreamRuns(2)
	if len(recent) != 2 {
		t.Fatalf("limit=2 得到 %d 条", len(recent))
	}
	// limit 取**最近**的（文件尾），不是文件头。
	if recent[0].SessionID != "s3" || recent[1].SessionID != "s4" {
		t.Fatalf("limit 取错了范围: %s,%s", recent[0].SessionID, recent[1].SessionID)
	}

	// 文件不存在 → nil（设置页显示空态，不报错）。
	if err := os.Remove(p); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := readDreamRuns(5); got != nil {
		t.Fatalf("缺文件应返回 nil，got %+v", got)
	}
}

func TestCompleteDreamRun(t *testing.T) {
	dir := withDreamDir(t)
	p := filepath.Join(dir, "dream_runs.jsonl")
	start := time.Date(2026, 11, 20, 3, 0, 0, 0, time.Local)
	appendDreamRun(dreamRun{StartedAt: start, Provider: "deepseek", Model: "deepseek-chat", SessionID: "sA"})
	appendDreamRun(dreamRun{StartedAt: start.Add(time.Hour), Provider: "openai", Model: "gpt-4o", SessionID: "sB"})

	fin := start.Add(9 * time.Minute)
	completeDreamRun("sA", fin, "处理 3 个库，归档 2 条", "")

	runs := readDreamRuns(0)
	if len(runs) != 2 {
		t.Fatalf("收口应改写而不是追加，得到 %d 条", len(runs))
	}
	if runs[0].SessionID != "sA" || !runs[0].FinishedAt.Equal(fin) || runs[0].Summary == "" {
		t.Fatalf("sA 收口失败: %+v", runs[0])
	}
	// 别的会话不能被波及（它们各自有 StartedAt/FinishedAt）。
	if !runs[1].FinishedAt.IsZero() || runs[1].Summary != "" {
		t.Fatalf("sB 不该被改写: %+v", runs[1])
	}

	// 找不到会话 → 文件内容原样（不写空文件）。
	completeDreamRun("nope", fin, "x", "y")
	if got := readDreamRuns(0); len(got) != 2 || got[0].Summary == "" {
		t.Fatalf("未知会话不应改动记录: %+v", got)
	}

	// 错误路径：AgentEnd 带错误 → 记录里能看到，摘要仍可空。
	completeDreamRun("sB", fin, "", "provider 认证失败")
	got := readDreamRuns(0)
	if got[1].Error != "provider 认证失败" {
		t.Fatalf("错误没落进记录: %+v", got[1])
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Fatalf("运行记录权限 = %o, want 600", mode)
	}
}

func TestDreamSummaryOf(t *testing.T) {
	if got := dreamSummaryOf("  已完成巡检：3 个库，归档 2 条。\n第二行不该进列表 "); got != "已完成巡检：3 个库，归档 2 条。 第二行不该进列表" {
		t.Fatalf("摘要压行不对: %q", got)
	}
	if got := dreamSummaryOf(""); got != "" {
		t.Fatalf("空正文应得空摘要，got %q", got)
	}
	long := ""
	for i := 0; i < 500; i++ {
		long += "记"
	}
	if got := []rune(dreamSummaryOf(long)); len(got) != 201 || got[200] != '…' {
		t.Fatalf("超长摘要应截到 200 字 + 省略号，得到 %d 字", len(got))
	}
}

// —— 命令面与调度触发 ——

// dreamTestManager 构造隔离的 manager（HOME 与 APP 数据目录都落临时目录）。
// 与 cronTestManager 同形：不发网络，只验证"触发后有没有按预期建会话/记记录"。
func dreamTestManager(t *testing.T) (*manager, *syncBuf) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dataDir := t.TempDir()
	prev := appDataDirOverride
	appDataDirOverride = dataDir
	t.Cleanup(func() { appDataDirOverride = prev })

	buf := &syncBuf{}
	m := &manager{
		workspaces: map[string]*workspaceRuntime{},
		provCfg:    loadProviderConfig(),
		out:        newOut(buf),
	}
	// 派生会话装了运行态就要收尾（run goroutine 会在 TempDir 清理后继续写盘）。
	t.Cleanup(func() {
		m.mu.Lock()
		paths := make([]string, 0, len(m.workspaces))
		for p := range m.workspaces {
			paths = append(paths, p)
		}
		m.mu.Unlock()
		for _, p := range paths {
			ws := m.workspaces[p]
			if ws == nil {
				continue
			}
			ws.mu.Lock()
			items := make([]*bridgeSession, 0, len(ws.sessions))
			for _, bs := range ws.sessions {
				items = append(items, bs)
			}
			ws.mu.Unlock()
			for _, bs := range items {
				stopSession(bs)
			}
			ws.stopMCPWatch()
		}
	})
	return m, buf
}

func TestDreamSetGetCommands(t *testing.T) {
	m, buf := dreamTestManager(t)
	withDreamDir(t)

	// 空目录首次打开：设置回落默认（关闭 + 03:00），没有运行记录。
	m.dispatch(cmd(1, "dream_get", map[string]any{}))
	resps := commandResponses(readLines(buf))
	if len(resps) != 1 || resps[0]["ok"] != true {
		t.Fatalf("dream_get 失败: %+v", resps)
	}
	data := resps[0]["data"].(map[string]any)
	s := data["settings"].(map[string]any)
	if s["enabled"] != false || s["at"] != "03:00" {
		t.Fatalf("默认设置不对: %+v", s)
	}
	if runs, _ := data["runs"].([]any); len(runs) != 0 {
		t.Fatalf("首次打开不应有运行记录: %+v", runs)
	}

	// 写入：开关 + provider/model + 时刻
	m.dispatch(cmd(2, "dream_set", map[string]any{
		"enabled": true, "provider": "deepseek", "model": "deepseek-chat", "at": "04:30",
	}))
	// 写入成功会先推一条 dream_changed（设置页据此刷新），按 command_response 取结果。
	resps = commandResponses(readLines(buf))
	if len(resps) != 1 || resps[0]["ok"] != true {
		t.Fatalf("dream_set 失败: %+v", resps)
	}
	if got := loadDreamSettings(); !got.Enabled || got.At != "04:30" || got.Provider != "deepseek" || got.Model != "deepseek-chat" {
		t.Fatalf("落盘不一致: %+v", got)
	}

	// 再打开面板：读回同一份；LastRunDate 不被 dream_set 清掉（否则改设置=今天重跑）
	prev := loadDreamSettings()
	prev.LastRunDate = "2026-11-20"
	if err := saveDreamSettings(prev); err != nil {
		t.Fatalf("save: %v", err)
	}
	m.dispatch(cmd(3, "dream_set", map[string]any{"enabled": true, "at": "05:00"}))
	_ = readLines(buf)
	if got := loadDreamSettings(); got.LastRunDate != "2026-11-20" {
		t.Fatalf("dream_set 不该清 LastRunDate: %+v", got)
	}
}

func TestDreamSetValidation(t *testing.T) {
	m, buf := dreamTestManager(t)
	withDreamDir(t)

	cases := []struct {
		name    string
		payload map[string]any
		wantErr string
	}{
		{"缺 enabled", map[string]any{"at": "03:00"}, "enabled"},
		{"enabled 类型错", map[string]any{"enabled": "yes"}, "enabled"},
		{"时刻形状错", map[string]any{"enabled": true, "at": "3:0"}, "HH:MM"},
		{"时刻越界", map[string]any{"enabled": true, "at": "24:00"}, "HH:MM"},
		{"provider 无 model", map[string]any{"enabled": true, "provider": "deepseek"}, "provider 与 model"},
		{"未知 provider", map[string]any{"enabled": true, "provider": "nope", "model": "x"}, "未知的 provider"},
	}
	for i, c := range cases {
		m.dispatch(cmd(100+i, "dream_set", c.payload))
		lines := commandResponses(readLines(buf))
		if len(lines) != 1 || lines[0]["ok"] != false {
			t.Fatalf("%s: 应被拒绝，得到 %+v", c.name, lines)
		}
		if errStr, _ := lines[0]["error"].(string); !strings.Contains(errStr, c.wantErr) {
			t.Fatalf("%s: 错误文案 %q 不含 %q", c.name, errStr, c.wantErr)
		}
	}
	// 全被拒 → 磁盘上什么都没写（不覆盖用户已有设置）。
	if got := loadDreamSettings(); got.Enabled || got.At != "03:00" {
		t.Fatalf("校验失败不应落盘: %+v", got)
	}
}

func TestDreamFireCreatesDerivedSession(t *testing.T) {
	m, _ := dreamTestManager(t)
	withDreamDir(t)

	s := dreamSettings{Enabled: true, At: "03:00", Provider: "deepseek", Model: "deepseek-chat"}
	if err := saveDreamSettings(s); err != nil {
		t.Fatalf("save: %v", err)
	}

	now := time.Date(2026, 11, 20, 3, 0, 5, 0, time.Local)
	bs, err := m.fireDream(s, now)
	if err != nil {
		t.Fatalf("fireDream: %v", err)
	}
	if bs == nil || !bs.isDream {
		t.Fatalf("派生会话应标记 isDream: %+v", bs)
	}
	if !bs.derived() || bs.dreamSessionDone == nil {
		t.Fatal("派生会话应同时被视作 derived 且装上收口钩子")
	}
	// 先写后跑：触发那一刻就把今天记下来，ticker 不会在长运行期间重复触发。
	if got := loadDreamSettings(); got.LastRunDate != "2026-11-20" {
		t.Fatalf("LastRunDate 应为今天，得到 %q", got.LastRunDate)
	}
	// 记录：一条、已开始、未完成、带 session id。
	runs := readDreamRuns(0)
	if len(runs) != 1 {
		t.Fatalf("应有一条运行记录，得到 %d", len(runs))
	}
	if runs[0].SessionID != bs.id || runs[0].Provider != "deepseek" || !runs[0].StartedAt.Equal(now) {
		t.Fatalf("运行记录不对: %+v", runs[0])
	}
	if !runs[0].FinishedAt.IsZero() {
		t.Fatal("运行还没结束，FinishedAt 应为空")
	}
	// 会话/事件存储落在 dream_sessions/<sid>，不进工作区命名空间。
	if _, err := os.Stat(filepath.Join(dreamSessionsRoot(), bs.id)); err != nil {
		t.Fatalf("派生会话目录未创建: %v", err)
	}
	// 并发闩：第二次触发直接被拒，不会起第二个会话。
	if _, err := m.fireDream(s, now); err == nil || !strings.Contains(err.Error(), "正在运行") {
		t.Fatalf("并发触发应被拒绝，得到 %v", err)
	}

	// 收口：AgentEnd 钩子把同一条记录补成"已完成"（不追加第二条）。
	m.finishDreamRun(bs.id, "", "已巡检 3 个库，归档 2 条")
	runs = readDreamRuns(0)
	if len(runs) != 1 {
		t.Fatalf("收口应改写而非追加，得到 %d 条", len(runs))
	}
	if runs[0].FinishedAt.IsZero() || runs[0].Summary == "" {
		t.Fatalf("收口没写全: %+v", runs[0])
	}
	// 闩放开：下一轮可以再触发。
	if !m.dreamAcquire() {
		t.Fatal("收口后应能再次触发")
	}
	m.dreamRelease()
}

func TestDreamRunNowCommand(t *testing.T) {
	m, buf := dreamTestManager(t)
	withDreamDir(t)
	if err := saveDreamSettings(dreamSettings{Enabled: false, At: "03:00", Provider: "deepseek", Model: "deepseek-chat"}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// 「立即运行」不看开关与时刻；但会写 LastRunDate（今天别再自动跑一次）。
	m.dispatch(cmd(1, "dream_run_now", map[string]any{}))
	lines := readLines(buf)
	// 响应里会先出现 dream_changed 事件行，按 command_response 找。
	resps := commandResponses(lines)
	if len(resps) != 1 || resps[0]["ok"] != true {
		t.Fatalf("dream_run_now 失败: %+v", lines)
	}
	resp := resps[0]
	data := resp["data"].(map[string]any)
	if sid, _ := data["session_id"].(string); sid == "" {
		t.Fatalf("响应缺 session_id: %+v", data)
	}
	if got := loadDreamSettings(); got.LastRunDate != time.Now().Format("2006-01-02") {
		t.Fatalf("立即运行也应写 LastRunDate，得到 %q", got.LastRunDate)
	}
}
