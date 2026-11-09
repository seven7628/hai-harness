package subagent

// 按 task id 从 journal 还原已结束任务（2026-11-09 的能力补齐）。
//
// 背景：注册表不再保留已结束任务的**对象**（Registry.reclaimTask：只留终态摘要），
// 于是「按 id 查历史任务」（TaskOutput / agent_resume / 跨进程恢复）必须能从
// journal 把任务读回来 —— 这也是「agent_resume 完全靠 journal 工作」的基础。

import (
	"path/filepath"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// TestRehydrateTaskFromJournal 还原出来的任务带齐状态/结果/用量/名字。
func TestRehydrateTaskFromJournal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-abcdefgh.jsonl")

	j, err := OpenTaskJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(JournalRecord{Type: "start", TaskId: "task-abcdefgh", Name: "做点事", Task: "原始指令"}); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(JournalRecord{Type: "turn", TaskId: "task-abcdefgh", Round: 1, Text: "第一轮"}); err != nil {
		t.Fatal(err)
	}
	usage := core.Usage{Input: 100, Output: 200, TotalTokens: 300}
	if err := j.Append(JournalRecord{Type: "result", TaskId: "task-abcdefgh", Status: string(TaskCompleted), Text: "最终回复", Usage: &usage}); err != nil {
		t.Fatal(err)
	}
	j.Close()

	task, ok := RehydrateTaskFromJournal("task-abcdefgh", path)
	if !ok || task == nil {
		t.Fatal("应能从 journal 还原出任务")
	}
	if task.Status != TaskCompleted {
		t.Fatalf("状态应为 completed，得 %q", task.Status)
	}
	if task.Result != "最终回复" {
		t.Fatalf("结果应为「最终回复」，得 %q", task.Result)
	}
	if task.Usage.Input != 100 || task.Usage.Output != 200 {
		t.Fatalf("用量应还原：%+v", task.Usage)
	}
	if task.Name != "做点事" {
		t.Fatalf("名字应还原：%q", task.Name)
	}
	if task.OutputFile() != path {
		t.Fatalf("journal 路径应保留：%q", task.OutputFile())
	}
}

// TestRehydrateWithoutTerminalRecord 还原不出（还在跑 / 没有终态行）→ 明确 false。
func TestRehydrateWithoutTerminalRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-abcdefgh.jsonl")
	j, err := OpenTaskJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(JournalRecord{Type: "start", TaskId: "task-abcdefgh", Name: "还在跑"}); err != nil {
		t.Fatal(err)
	}
	j.Close()

	if _, ok := RehydrateTaskFromJournal("task-abcdefgh", path); ok {
		t.Fatal("没有终态记录的任务不应被还原为终态")
	}
	if _, ok := RehydrateTaskFromJournal("task-abcdefgh", filepath.Join(dir, "missing.jsonl")); ok {
		t.Fatal("journal 不存在时不应还原")
	}
}

// TestRegistryGetFallsBackToJournal 验证注册表的第三级回落：表里没有 → 宿主注入的
// 回落器按 id 从 journal 还原。
func TestRegistryGetFallsBackToJournal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-abcdefgh.jsonl")
	j, err := OpenTaskJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	usage := core.Usage{Input: 7, Output: 9}
	if err := j.Append(JournalRecord{Type: "result", TaskId: "task-abcdefgh", Status: string(TaskFailed), Text: "炸了", Usage: &usage}); err != nil {
		t.Fatal(err)
	}
	j.Close()

	r := NewRegistry()
	r.SetTaskRehydrator(func(taskID string) (*Task, bool) {
		// 真实宿主按 task id 解析 journal 路径（sessions/<wsKey>/<sid>/agents/<id>.jsonl），
		// 未知 id 自然没有对应文件 → 还原不出。
		if taskID != "task-abcdefgh" {
			return nil, false
		}
		return RehydrateTaskFromJournal(taskID, path)
	})

	task, err := r.get("task-abcdefgh")
	if err != nil {
		t.Fatalf("get 应能经回落器还原：%v", err)
	}
	if task.Status != TaskFailed || task.Result != "炸了" {
		t.Fatalf("还原内容不对：status=%q result=%q", task.Status, task.Result)
	}
	// 未知 id 仍然是「查不到」。
	if _, err := r.get("task-zzzzzzzz"); err == nil {
		t.Fatal("未知 id 应报 ErrTaskNotFound")
	}
}
