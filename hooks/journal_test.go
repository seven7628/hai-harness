package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 日志的意义是回答"这个 hook 到底跑过没有"。所以第一条要钉住的是：**正常结束的
// 执行也必须留一行**——它正是此前唯一不留痕的那一类。
func TestJournalRecordsSuccessfulRuns(t *testing.T) {
	p := filepath.Join(t.TempDir(), "runs.jsonl")
	j := NewJournal(p)
	j.Append(HookRun{Event: EventPostToolUse, Name: "gofmt", Status: "ok",
		At: time.Unix(1700000000, 0).UTC(), Session: "s1"})
	j.Append(HookRun{Event: EventPreToolUse, Name: "guard", Status: "blocked", ExitCode: 2})

	runs, err := ReadJournal(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("应有 2 条记录，实际 %d", len(runs))
	}
	if runs[0].Name != "gofmt" || runs[0].Status != "ok" {
		t.Fatalf("第一条不对：%+v", runs[0])
	}
	if runs[0].Session != "s1" || runs[0].At.IsZero() {
		t.Fatalf("会话/时间没有落盘：%+v", runs[0])
	}
}

// limit 取的是**最近** N 条，不是最早 N 条：排查时人要的是最近发生了什么。
func TestReadJournalLimitKeepsMostRecent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "runs.jsonl")
	j := NewJournal(p)
	for _, n := range []string{"a", "b", "c"} {
		j.Append(HookRun{Name: n, Status: "ok"})
	}
	runs, err := ReadJournal(p, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].Name != "b" || runs[1].Name != "c" {
		t.Fatalf("取最近 2 条失败：%+v", runs)
	}
}

// 无日志文件不是错误：第一次运行前它就不存在。
func TestReadJournalMissingFileIsEmpty(t *testing.T) {
	runs, err := ReadJournal(filepath.Join(t.TempDir(), "nope.jsonl"), 10)
	if err != nil || len(runs) != 0 {
		t.Fatalf("缺文件应返回空且无错，得到 %v / %d", err, len(runs))
	}
}

// 损坏行不能让整份日志读不出来：这是诊断日志，不是账本。
func TestReadJournalSkipsCorruptLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := os.WriteFile(p, []byte("{\"name\":\"a\",\"status\":\"ok\"}\n{ 半行\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runs, err := ReadJournal(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Name != "a" {
		t.Fatalf("应只读出完整那一行：%+v", runs)
	}
}

// 压缩：超过上限后保留尾部一半，且文件不再无限增长。
func TestJournalCompactsWhenOversized(t *testing.T) {
	p := filepath.Join(t.TempDir(), "runs.jsonl")
	j := NewJournal(p)
	j.maxBytes = 2048 // 压小以便几条就触发

	for i := 0; i < 40; i++ {
		j.Append(HookRun{Name: strings.Repeat("x", 50), Status: "ok"})
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 4096 {
		t.Fatalf("日志没有压缩，仍有 %d 字节", fi.Size())
	}
	runs, _ := ReadJournal(p, 0)
	if len(runs) == 0 {
		t.Fatal("压缩后不该一条不剩")
	}
}

// NewJournal("") 返回 nil，且 nil 上调用 Append 不 panic——装配处可以无脑设置。
func TestNewJournalEmptyPathIsNilAndSafe(t *testing.T) {
	var j *Journal = NewJournal("")
	if j != nil {
		t.Fatal("空路径应返回 nil")
	}
	j.Append(HookRun{Name: "x"}) // 不应 panic
}
