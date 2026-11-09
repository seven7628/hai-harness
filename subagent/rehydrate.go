package subagent

// 按 task id 从 journal 还原已结束任务。
//
// 为什么需要（2026-11-09）：注册表不再保留已结束任务的**对象**（见
// Registry.reclaimTask —— 只留终态摘要，完整内容按 id 从 journal 还原），
// 于是「按 id 查历史任务」这条路径（TaskOutput / agent_resume / 跨进程恢复）
// 必须能从 journal 把任务读回来。
//
// journal 路径只是 task id 的函数（宿主注入 spec.JournalPath / journalPathFor），
// 所以跨进程同样成立 —— 这正是「agent_resume 完全靠 journal 工作」的基础。

import (
	"errors"
	"strings"

	"github.com/seven7628/hai-harness/core"
)

// RehydrateTaskFromJournal 从 journal 还原一个**终态**任务对象。
//
// 返回 (nil, false) = journal 不可读 / 没有终态记录（调用方按「查不到」处理）。
//
// 还原出来的字段：状态、最终回复、错误、用量、名字、恢复链、journal 路径。
// **不含** parentTaskID：journal 的 start 记录带的是发起方的 **run id**
// （ParentRunID），不是父任务的 task id —— 跨进程场景下父归属无从还原，
// 进程内则由终态摘要（Registry.finished）提供。
func RehydrateTaskFromJournal(taskID, path string) (*Task, bool) {
	if taskID == "" || path == "" {
		return nil, false
	}
	recs, err := ReadAll(path)
	if err != nil {
		return nil, false
	}
	t := &Task{
		ID:                taskID,
		outputFile:        path,
		acceptingDelivery: false,
		done:              make(chan struct{}),
		inbox:             make(chan core.Message, 16),
	}
	sawStart := false
	for _, r := range recs {
		switch r.Type {
		case "start":
			sawStart = true
			if t.Name == "" {
				t.Name = r.Name
			}
			t.resumedFrom = r.ResumedFrom
		case "result":
			if r.Status != "" {
				t.Status = TaskStatus(r.Status)
			}
			t.Result = r.Text
			if r.Error != "" {
				t.Err = errors.New(r.Error)
			}
			if r.Usage != nil {
				t.Usage = *r.Usage
			}
		}
	}
	if t.Status == "" {
		// 没有终态记录 → 不是可还原的终态任务（还在跑 / 异常退出没写终态）。
		return nil, false
	}
	_ = sawStart // 没有 start 记录（名字缺失）也允许还原：终态信息本身是全的。
	// 终态任务的 inbox 无人消费；这里明确关掉（与 closeDelivery 同义）。
	t.acceptingDelivery = false
	return t, true
}

// RehydratedTaskNameFromJournal 从 journal 取任务名（宿主给「按 id 查历史」的轻量路径用）。
func RehydratedTaskNameFromJournal(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	recs, err := ReadAll(path)
	if err != nil {
		return "", false
	}
	for _, r := range recs {
		if r.Type == "start" && strings.TrimSpace(r.Name) != "" {
			return r.Name, true
		}
	}
	return "", false
}
