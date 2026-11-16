package main

// Dream 设置/调度的纯逻辑测试（写入路径用 dreamDirOverride 隔离到临时目录，
// 不碰真实 ~/.go-code：那里是用户数据）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/memory"
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
	completeDreamRun("sA", fin, "处理 3 个库，归档 2 条", "", []dreamRepoResult{{ID: "repo-a", Topics: 12, Archived: 2}}, dreamRunDetail{})

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
	completeDreamRun("nope", fin, "x", "y", nil, dreamRunDetail{})
	if got := readDreamRuns(0); len(got) != 2 || got[0].Summary == "" {
		t.Fatalf("未知会话不应改动记录: %+v", got)
	}

	// 错误路径：AgentEnd 带错误 → 记录里能看到，摘要仍可空。
	completeDreamRun("sB", fin, "", "provider 认证失败", nil, dreamRunDetail{})
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

// 摘要的四类真实输入：表格开头、纯表格、正常段落、空串。
//
// 真机观察到的原始正文（2026-11，opencode/deepseek-v4.1-flash）：
//
//	巡检完成。以下是每个记忆库的处理情况：
//	| 库 | 条目 | 证据要点 | 动作 |
//	|---|---|---|---|
//	| a | 80 | … | 保持 |
func TestDreamSummaryOf(t *testing.T) {
	tally4 := []dreamRepoResult{
		{ID: "a", Topics: 80}, {ID: "b", Topics: 12}, {ID: "c", Topics: 9}, {ID: "d", Topics: 3, Archived: 1},
	}
	cases := []struct {
		name  string
		final string
		repos []dreamRepoResult
		want  string
	}{
		{
			name: "表格开头",
			final: "巡检完成。以下是每个记忆库的处理情况：\n" +
				"| 库 | 条目 | 证据要点 | 动作 |\n|---|---|---|---|\n| a | 80 | 锚点还在 | 保持 |\n" +
				"整体可用：d 库有 1 条被继任，已提交归档。\n下一天继续。",
			repos: tally4,
			want:  "整体可用：d 库有 1 条被继任，已提交归档。 下一天继续。",
		},
		{
			name: "纯表格", // 去掉表格后没有任何正文 → 用宿主台账说话（不解析模型文字）
			final: "| 库 | 条目 | 证据要点 | 动作 |\n" +
				"|---|---|---|---|\n| a | 80 | 锚点还在 | 保持 |\n| d | 3 | 被继任 | 归档 1 |",
			repos: tally4,
			want:  "已处理 4 个记忆库，归档 1 条",
		},
		{
			name:  "纯表格且一条都没归档",
			final: "| 库 | 动作 |\n|---|---|\n| a | 保持 |",
			repos: []dreamRepoResult{{ID: "a", Topics: 80}},
			want:  "已处理 1 个记忆库，没有需要归档的条目",
		},
		{
			name:  "纯表格且没有台账", // 台账为空时不能编数字，宁可空
			final: "| 库 | 动作 |\n|---|---|\n| a | 保持 |",
			want:  "",
		},
		{
			name:  "正常段落", // 含数字的首行是结论，不是开场白
			final: "  已完成巡检：3 个库，归档 2 条。\n第二行不该进列表 ",
			want:  "已完成巡检：3 个库，归档 2 条。 第二行不该进列表",
		},
		{
			name:  "空串",
			final: "",
			want:  "",
		},
		{
			name:  "只有一句开场白、正文全是表格", // 开场白丢完没正文 → 台账顶上
			final: "巡检完毕。\n| 库 | 动作 |\n|---|---|\n| a | 保持 |",
			repos: []dreamRepoResult{{ID: "a", Topics: 80, Archived: 2}},
			want:  "已处理 1 个记忆库，归档 2 条",
		},
		{
			name:  "只有一句开场白、也没有台账", // 什么都凑不出来时，至少把原话还回去
			final: "巡检完毕。",
			want:  "巡检完毕。",
		},
		{
			name:  "中转义的分隔行也丢掉",
			final: "---\n:--:  \n有效结论：a 库的锚点全部失效，已归档。",
			want:  "有效结论：a 库的锚点全部失效，已归档。",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dreamSummaryOf(c.final, c.repos); got != c.want {
				t.Fatalf("dreamSummaryOf(%q) = %q, want %q", c.final, got, c.want)
			}
		})
	}

	long := ""
	for i := 0; i < 500; i++ {
		long += "记"
	}
	if got := []rune(dreamSummaryOf(long, nil)); len(got) != 201 || got[200] != '…' {
		t.Fatalf("超长摘要应截到 200 字 + 省略号，得到 %d 字", len(got))
	}
}

// 运行记录只保留最近 200 行：写失败不动历史、裁剪原子替换、权限不变、仍是时间序。
func TestDreamRunsRotationKeepsRecentTwoHundred(t *testing.T) {
	dir := withDreamDir(t)
	p := filepath.Join(dir, "dream_runs.jsonl")
	base := time.Date(2026, 1, 1, 3, 0, 0, 0, time.Local)

	total := dreamRunsKeep + 1
	for i := 0; i < total; i++ {
		appendDreamRun(dreamRun{
			StartedAt: base.Add(time.Duration(i) * time.Hour),
			Provider:  "opencode",
			Model:     "deepseek-v4.1-flash",
			SessionID: "s" + itoa(i),
		})
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读运行记录: %v", err)
	}
	var lines []string
	for _, ln := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(ln) != "" {
			lines = append(lines, ln)
		}
	}
	if len(lines) != dreamRunsKeep {
		t.Fatalf("文件应只剩 %d 行，实际 %d 行", dreamRunsKeep, len(lines))
	}
	// 顺序仍是时间序，且丢的是**最旧的**那条（s0），最新的那条（s200）还在。
	runs := readDreamRuns(0)
	if len(runs) != dreamRunsKeep {
		t.Fatalf("解析到 %d 条，want %d", len(runs), dreamRunsKeep)
	}
	if runs[0].SessionID != "s1" || runs[len(runs)-1].SessionID != "s"+itoa(total-1) {
		t.Fatalf("裁剪后的首尾不对: %s … %s", runs[0].SessionID, runs[len(runs)-1].SessionID)
	}
	for i := 1; i < len(runs); i++ {
		if runs[i].StartedAt.Before(runs[i-1].StartedAt) {
			t.Fatalf("第 %d 条时间倒序：%s < %s", i, runs[i].StartedAt, runs[i-1].StartedAt)
		}
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Fatalf("裁剪后权限 = %o, want 600", mode)
	}
	// 临时文件不许留在原地。
	if _, err := os.Stat(p + ".trim"); !os.IsNotExist(err) {
		t.Fatalf("裁剪用的临时文件应已被 rename 掉")
	}

	// 再追加一条：仍是 200 行（不是 201），头部的 s1 被挤掉，尾巴是最新的。
	appendDreamRun(dreamRun{StartedAt: base.Add(500 * time.Hour), SessionID: "tail"})
	runs = readDreamRuns(0)
	if len(runs) != dreamRunsKeep {
		t.Fatalf("再追加后应仍是 %d 行，实际 %d 行", dreamRunsKeep, len(runs))
	}
	if runs[0].SessionID != "s2" || runs[len(runs)-1].SessionID != "tail" {
		t.Fatalf("第二次裁剪后的首尾不对: %s … %s", runs[0].SessionID, runs[len(runs)-1].SessionID)
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
	m.finishDreamRun(bs.id, "", "已巡检 3 个库，归档 2 条", []dreamRepoResult{{ID: "repo-a", Topics: 12, Archived: 2}},
		[]dreamStep{{Tool: "memory_dream", Arg: "repos", OK: true, Note: "1 个库"}})
	runs = readDreamRuns(0)
	if len(runs) != 1 {
		t.Fatalf("收口应改写而非追加，得到 %d 条", len(runs))
	}
	if runs[0].FinishedAt.IsZero() || runs[0].Summary == "" {
		t.Fatalf("收口没写全: %+v", runs[0])
	}
	if len(runs[0].Repos) != 1 || runs[0].Repos[0].ID != "repo-a" || runs[0].Repos[0].Archived != 2 {
		t.Fatalf("repos 没进记录: %+v", runs[0].Repos)
	}
	// 收口同时落详情：正文全文（此处就是钩子给的 final）+ 时间线。
	if runs[0].Result != "已巡检 3 个库，归档 2 条" || len(runs[0].Steps) != 1 {
		t.Fatalf("result/steps 没进记录: %+v", runs[0])
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

// TestDreamWatchdogReleasesStuckLatch 看门狗兜底：一轮没有 AgentEnd 的巡检不能让
// 后续每一夜都撞在"正在运行中"上（真机验收指出的健壮性缺口）。
func TestDreamWatchdogReleasesStuckLatch(t *testing.T) {
	m, _ := dreamTestManager(t)
	withDreamDir(t)

	start := time.Date(2026, 11, 20, 3, 0, 0, 0, time.Local)
	appendDreamRun(dreamRun{StartedAt: start, Provider: "deepseek", Model: "deepseek-chat", SessionID: "sStuck"})

	if !m.dreamAcquire() {
		t.Fatal("首次取闩应成功")
	}
	m.dreamNoteSession("sStuck")
	m.dreamSince = start // 把起跑时刻定格，模拟"这一轮已经跑了很久"

	// 没到阈值：闩保持（正常的慢不是故障）。
	m.dreamWatchdog(start.Add(dreamMaxRunDuration - time.Minute))
	if !m.dreamRunning() {
		t.Fatal("未到 90 分钟不应放闩")
	}
	// 过阈值：强制放闩 + 记录收口成失败。
	m.dreamWatchdog(start.Add(dreamMaxRunDuration + time.Minute))
	if m.dreamRunning() {
		t.Fatal("超过 90 分钟应强制放闩")
	}
	runs := readDreamRuns(0)
	if len(runs) != 1 {
		t.Fatalf("看门狗不应追加第二条记录，得到 %d 条", len(runs))
	}
	if runs[0].FinishedAt.IsZero() {
		t.Fatal("卡住的那一轮应被收口（FinishedAt 非空）")
	}
	if !strings.Contains(runs[0].Error, "闩已强制释放") {
		t.Fatalf("记录里应写明强制释放，得到 %q", runs[0].Error)
	}
	// 真的放开了：下一轮能取到闩。
	if !m.dreamAcquire() {
		t.Fatal("看门狗之后应能再次触发")
	}
	m.dreamRelease()

	// 空闲时什么都不做（不写记录、不推事件）。
	m.dreamWatchdog(start.Add(24 * time.Hour))
	if got := readDreamRuns(0); len(got) != 1 {
		t.Fatalf("空闲时看门狗不应改动记录，得到 %d 条", len(got))
	}
}

// TestMemoryDreamReposAction repos 动作：宿主自己列记忆库，不依赖 bash/glob 的路径权限。
func TestMemoryDreamReposAction(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := filepath.Join(home, ".go-code", "memory", "repos")
	mk := func(repo string, files map[string]string) {
		dir := filepath.Join(root, repo)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		for name, body := range files {
			p := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
	}
	mk("repo-with-topics", map[string]string{
		"MEMORY.md":          "- [a](a.md) — 一\ntopics\n",
		"a.md":               "---\ntype: project\n---\n\nA\n",
		"b.md":               "---\ntype: user\n---\n\nB\n",
		"a.md.lock":          "",
		"MEMORY.md.bak-1105": "旧索引",
	})
	// 只有 .state：空库，不进清单（真机验收里它被模型当成了第 9 个记忆库）
	mk("empty-repo", map[string]string{".state/repo.json": "{}"})
	// 索引在、条目为零：算有内容（用户刚写了索引）
	mk("index-only", map[string]string{"MEMORY.md": "- [x](x.md) — x\n"})
	// 非法名（带分隔符/点）：不是记忆库目录，忽略
	if err := os.MkdirAll(filepath.Join(root, ".hidden"), 0o700); err != nil {
		t.Fatal(err)
	}

	h := newMemoryHost(memory.Config{})
	tool := newMemoryDreamTool(h, t.TempDir())
	out, err := tool.Call(context.Background(), "", `{"action":"repos"}`)
	if err != nil {
		t.Fatalf("repos: %v", err)
	}
	var payload struct {
		Root  string `json:"root"`
		Count int    `json:"count"`
		Repos []struct {
			ID        string `json:"id"`
			Topics    int    `json:"topics"`
			Bytes     int64  `json:"bytes"`
			HasIndex  bool   `json:"has_index"`
			LastWrite string `json:"last_write"`
		} `json:"repos"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("repos 返回不是 JSON: %v（%s）", err, out)
	}
	byID := map[string]int{}
	for _, r := range payload.Repos {
		byID[r.ID] = r.Topics
	}
	if payload.Count != 2 || len(payload.Repos) != 2 {
		t.Fatalf("应只列出 2 个有内容的库，得到 count=%d repos=%+v", payload.Count, byID)
	}
	if byID["repo-with-topics"] != 2 {
		t.Fatalf("条目数应为 2（MEMORY.md/.bak/.lock 不算），得到 %d", byID["repo-with-topics"])
	}
	if _, ok := byID["empty-repo"]; ok {
		t.Fatal("只有 .state 的空库不应出现在清单里")
	}
	if _, ok := byID["index-only"]; !ok {
		t.Fatal("只有索引（条目为零）的库应出现在清单里")
	}
	// 排序稳定（便于模型逐条对账）
	if payload.Repos[0].ID > payload.Repos[1].ID {
		t.Fatalf("清单应按 id 排序，得到 %s > %s", payload.Repos[0].ID, payload.Repos[1].ID)
	}
	if payload.Repos[0].LastWrite == "" && payload.Repos[1].LastWrite == "" {
		t.Fatal("有文件的库应带 last_write")
	}
}

// TestDreamReposActionRejectsUnknownAction 第三动作没有挤坏原有的动作校验。
func TestDreamReposActionRejectsUnknownAction(t *testing.T) {
	h := newMemoryHost(memory.Config{})
	tool := newMemoryDreamTool(h, t.TempDir())
	err := tool.ValidParams(context.Background(), "", `{"action":"delete"}`)
	if err == nil || !strings.Contains(err.Error(), "repos / evidence / apply") {
		t.Fatalf("非法动作应被拒且文案列出三种动作，得到 %v", err)
	}
	if err := tool.ValidParams(context.Background(), "", `{"action":"repos"}`); err != nil {
		t.Fatalf("repos 应被接受: %v", err)
	}
}

// TestDreamTallyFromToolResults 台账只认工具真的执行了什么（不是模型自述）。
func TestDreamTallyFromToolResults(t *testing.T) {
	tl := newDreamTally()
	tl.observe(dreamToolEvent("bash", `{"action":"repos"}`, "无关工具不记"))
	tl.observe(dreamToolEvent("memory_dream", `{"action":"repos"}`, `{"count":2,"repos":[{"id":"repo-a","topics":9},{"id":"repo-b","topics":0}]}`))
	tl.observe(dreamToolEvent("memory_dream", `{"action":"evidence","repo":"repo-b"}`, `{"repo":"repo-b","topics":3}`))
	tl.observe(dreamToolEvent("memory_dream", `{"action":"apply","repo":"repo-a"}`, "记忆归档：归档 2 条：x.md、y.md；快照 .state/dream/1"))
	tl.observe(dreamToolEvent("memory_dream", `{"action":"apply","repo":"repo-a"}`, "记忆归档：归档 0 条；跳过 2 条"))
	got := tl.results()
	if len(got) != 2 {
		t.Fatalf("应记 2 个库，得到 %+v", got)
	}
	if got[0].ID != "repo-a" || got[0].Topics != 9 || got[0].Archived != 2 {
		t.Fatalf("repo-a 台账不对: %+v", got[0])
	}
	if got[1].ID != "repo-b" || got[1].Topics != 3 || got[1].Archived != 0 {
		t.Fatalf("repo-b 台账不对: %+v", got[1])
	}
	// nil 台账安全（非 Dream 会话走不到，但守卫要挡住 nil 解引用）。
	var empty *dreamTally
	empty.observe(dreamToolEvent("memory_dream", `{"action":"repos"}`, "{}"))
	if len(empty.results()) != 0 {
		t.Fatal("nil 台账应返回空")
	}
}

// TestDreamWorkspaceOutsideConfigDir 工作目录必须在 ~/.go-code 之外。
//
// 回归：它曾经是 ~/.go-code/dream_workspace，而沙箱与文件工具都拒绝 .go-code
// （只放行 memory/plugins/runtime）——真机验收里每次 bash 都以
// "getcwd: cannot access parent directories: Operation not permitted" 开头，
// glob 直接回"是 go-code 的配置目录，受保护"。
func TestDreamWorkspaceOutsideConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	prev := dreamWorkspaceRootOverride
	dreamWorkspaceRootOverride = ""
	t.Cleanup(func() { dreamWorkspaceRootOverride = prev })

	cfg := filepath.Join(home, ".go-code")
	ws := dreamWorkspacePath()
	if ws == cfg || strings.HasPrefix(ws, cfg+string(filepath.Separator)) {
		t.Fatalf("工作目录不能落在 ~/.go-code 内：%s", ws)
	}
	if !isDreamWorkspacePath(ws) {
		t.Fatalf("isDreamWorkspacePath(%s) 应为 true", ws)
	}
	if isDreamWorkspacePath(filepath.Join(home, "some-project")) {
		t.Fatal("普通工作区不该被认成 Dream 工作目录")
	}
	// 固定路径（不随会话变化）：m.runtime 按路径缓存运行态。
	if dreamWorkspacePath() != dreamWorkspacePath() {
		t.Fatal("工作目录路径必须稳定")
	}

	// 空工作目录不产生自己的记忆库（真机验收里它变成了"第 9 个记忆库"）。
	dirOverride := t.TempDir()
	prevDir := dreamDirOverride
	dreamDirOverride = dirOverride
	t.Cleanup(func() { dreamDirOverride = prevDir })
	h := newMemoryHost(memory.Config{})
	got, err := h.resolve(context.Background(), ws)
	if err != nil || got != nil {
		t.Fatalf("Dream 工作目录不应解析出记忆库，得到 %+v / %v", got, err)
	}
}
