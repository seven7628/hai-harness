package cron

import (
	"time"
)

// RetentionConfig 派生会话文件自动清理配置（桌面端设置：只需「开启/关闭」，
// 默认开启；保留天数用默认值，不由用户改）。
type RetentionConfig struct {
	Enabled  bool `json:"enabled"`
	KeepDays int  `json:"keep_days,omitempty"`
}

// DefaultRetention 默认：开启自动清理，保留 30 天。
func DefaultRetention() RetentionConfig {
	return RetentionConfig{Enabled: true, KeepDays: 30}
}

// finished 判断一趟运行是否为「已派生会话且已结束」（可安全清理其会话文件）。
// pending 还没派生；skipped 未派生（无会话文件）。running 早已结束（按 FiredAt 判龄）。
func finished(status RunStatus) bool {
	switch status {
	case StatusRunning, StatusSuccess, StatusError, StatusTimeout:
		return true
	default:
		return false
	}
}

// CleanupCandidates 从账本运行记录中，选出「派生会话且距今超过 keepDays 天」的
// 会话 id，供宿主删除对应 {ws}/.go-code/sessions|events/{sid}.jsonl 文件。
// 只删派生会话文件；账本（历史）与任务定义（cron.json）不动。
// 纯函数，便于单测；返回列表去重。
func CleanupCandidates(runs []Run, now time.Time, keepDays int) []string {
	if keepDays <= 0 {
		keepDays = DefaultRetention().KeepDays
	}
	cutoff := now.Add(-time.Duration(keepDays) * 24 * time.Hour)
	seen := map[string]bool{}
	var ids []string
	for _, r := range runs {
		if r.SessionID == "" || !finished(r.Status) {
			continue
		}
		if r.FiredAt.Before(cutoff) {
			if !seen[r.SessionID] {
				seen[r.SessionID] = true
				ids = append(ids, r.SessionID)
			}
		}
	}
	return ids
}

// CleanupCandidatesByRun 与 CleanupCandidates 同语义，但返回完整的 Run 记录
// （宿主按 run_id/job_id/workspace_key 定位独立历史目录 cron_sessions/<wsKey>/<jobID>/）。
// 独立历史目录按 job 维度组织，因此同一 job 的多个超龄 run 可能命中同一目录；
// 宿主删除文件幂等（按 sid/run_id 精确删），重复目录无害。
func CleanupCandidatesByRun(runs []Run, now time.Time, keepDays int) []Run {
	if keepDays <= 0 {
		keepDays = DefaultRetention().KeepDays
	}
	cutoff := now.Add(-time.Duration(keepDays) * 24 * time.Hour)
	var out []Run
	for _, r := range runs {
		if r.SessionID == "" || !finished(r.Status) {
			continue
		}
		if r.FiredAt.Before(cutoff) {
			out = append(out, r)
		}
	}
	return out
}
