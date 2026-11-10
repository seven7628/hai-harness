package memory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Dream v2：从会话转录里统计"最后一次被引用是什么时候"。

// 造一个 events 目录：workspace 字段决定归属，tool_run_start 决定读过哪条。
func eventsFixture(t *testing.T, workspace string, lines []string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "Users_admin_x-0cd9bf5d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "s-1.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestScanTranscriptsFindsReads(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	ws := "/Users/admin/proj"
	d := memDir(t) // ID = test-id
	mem := "/Users/admin/.go-code/memory/repos/" + d.ID + "/"
	root := eventsFixture(t, ws, []string{
		`{"event_type":"user_inputs_consumed","workspace":"` + ws + `","user_texts":["hi"]}`,
		`{"event_type":"tool_run_start","name":"read_file","arguments":"{\"path\":\"` + mem + `x.md\"}","timestamp":"2026-09-01T00:00:00Z","workspace":"` + ws + `"}`,
		`{"event_type":"tool_run_start","name":"bash","arguments":"grep foo ` + mem + `y.md","timestamp":"2026-10-01T00:00:00Z"}`,
		`{"event_type":"tool_run_start","name":"write_file","arguments":"{\"path\":\"` + mem + `z.md\"}","timestamp":"2026-10-01T00:00:00Z"}`,
	})

	bindRepoForTest(t, d, "/Users/admin/proj")

	rep, err := (Config{EventsRoot: root, Now: func() time.Time { return now }}).ScanTranscripts(d, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unbound || rep.Files != 1 {
		t.Fatalf("应扫到 1 个事件文件：%+v", rep)
	}
	if _, ok := rep.Traces["x.md"]; !ok {
		t.Fatalf("应记到 x.md：%+v", rep.Traces)
	}
	if _, ok := rep.Traces["y.md"]; !ok {
		t.Fatalf("bash 里的路径也算读取：%+v", rep.Traces)
	}
	if _, ok := rep.Traces["z.md"]; ok {
		t.Fatal("write_file 不算读取")
	}
	if got := rep.SilencedFor("x.md", now); int(got.Hours()/24) != 69 {
		t.Fatalf("沉寂天数 = %v", got)
	}
	if got := rep.SilencedFor("never.md", now); got != -1 {
		t.Fatalf("从未读过应返回 -1，得到 %v", got)
	}
}

// 别的**记忆库**的读取不能被算进来（按路径里的 repo-id 过滤）。
func TestScanTranscriptsIgnoresOtherRepo(t *testing.T) {
	ws := "/Users/admin/proj"
	root := eventsFixture(t, ws, []string{
		`{"event_type":"tool_run_start","name":"read_file","arguments":"/Users/admin/.go-code/memory/repos/other-repo/x.md","timestamp":"2026-10-01T00:00:00Z"}`,
	})
	d := memDir(t)
	bindRepoForTest(t, d, "/Users/admin/proj")

	rep, _ := (Config{EventsRoot: root}).ScanTranscripts(d, 0)
	if len(rep.Traces) != 0 {
		t.Fatalf("不该把别的记忆库的读取算进来：%+v", rep.Traces)
	}
}

// 找不到 events 目录 → Unbound，且报告要说明，不许沉默。
func TestDreamTranscriptsUnboundIsExplicit(t *testing.T) {
	d := memDir(t)
	// 指向一个不存在的转录根：Unbound 走的就是"读不到 events"这条路。
	r, err := (Config{EventsRoot: filepath.Join(t.TempDir(), "missing")}).Dream(d)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Transcripts.Unbound {
		t.Fatal("没绑定 workspace 应报 Unbound")
	}
	if !containsStr(r.Markdown(timeZero()), "会话转录：未扫描") {
		t.Fatalf("报告必须写明未扫描：\n%s", r.Markdown(timeZero()))
	}
}

func containsStr(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
