package cron

import (
	"time"
)

// defaultExpiry recurring 任务的自动过期时长（7 天：过期后最后触发一次即删除）。
// 一次性任务不设 ExpireAt（触发后自删）。宿主触发时向用户告知此限制。
const defaultExpiry = 7 * 24 * time.Hour

// pollInterval 宿主调度轮询建议值：cron 精度到分钟，15s 轮询开销可忽略且不漏触发。
const pollInterval = 15 * time.Second

// PollInterval 宿主调度轮询周期（bridge cronLoop 使用）。
const PollInterval = pollInterval

// Due 返回 now 时刻「已到点」的任务（NextRun 已到或早于 now）。
// 宿主按次逐个处理：enabled → 触发 + Advance(fired=true)；disabled → Advance(fired=false) 跳过。
// paused 任务不在其中（暂停 = 到点不触发、不推进 next、不计数；恢复后从原 next 继续）。
// 纯函数，便于单测；不修改任务本身（Advance 另做）。
func Due(jobs []*Job, now time.Time) []*Job {
	var due []*Job
	for _, j := range jobs {
		if j.Paused {
			continue
		}
		if !j.NextRun.IsZero() && !j.NextRun.After(now) {
			due = append(due, j)
		}
	}
	return due
}

// Jitter 确定性抖动：为「approximate 请求」避开 :00/:30 整点打点。
// 该策略在创建侧的 prompt 引导层解决（见上层设计），此处保留钩子：
// 宿主可在触发时叠加不超过 10% 周期（上限 15min）的确定性延迟以削峰。
// v1 直接到点触发（靠 poll 保证），此函数保留供后续削峰启用。
func Jitter(job *Job, base time.Time) time.Time {
	return base
}
