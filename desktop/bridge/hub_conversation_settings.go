package main

// hub_conversation_settings.go：全局对话设置（P6-04 / D8）。
//
// settings.json 顶层 `hub.conversation`：
//
//	{
//	  "hub": { "conversation": {
//	    "max_depth": 1000,          // 每条链的深度上限（P6-05）
//	    "pair_limit": 10,           // 无序 pair 在窗口内的成功投递上限
//	    "pair_window_ms": 600000,   // pair 计数的滚动窗口（默认 10 分钟）
//	    "idle_reset_ms": 1800000,   // 群空闲多久回 idle 并清零（默认 30 分钟）
//	    "cooldown_ms": 0,           // 两次自动唤醒之间的最小间隔（默认 0）
//	    "run_timeout_ms": 600000,   // 单轮 run 上限（默认 10 分钟）
//	    "max_pending_wakes": 200    // 待投递队列上限
//	  } }
//	}
//
// 为什么是全局而不是群策略：这几个旋钮调的是「对话引擎的护栏」，不是某个群的业务
// 规则。放在群策略里会让 12 个群各调一套，出问题时无法用一句话解释（D8 决策）。
//
// 缺省/越界一律回落默认值：坏配置不能让群聊停摆（宁可宽松，也不要「设成 0 就永不
// 唤醒」）。0 在这些字段里都不是合法业务值，故走「<=0 → 默认」而不是「0 = 关闭」。

import (
	"encoding/json"
	"os"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

// hubConversationSettings 是 settings.json 的 hub.conversation 段。
// 指针字段承载「没写」语义（0 与缺省在 JSON 里不可区分）。
type hubConversationSettings struct {
	MaxDepth        *int   `json:"max_depth,omitempty"`
	PairLimit       *int   `json:"pair_limit,omitempty"`
	PairWindowMS    *int64 `json:"pair_window_ms,omitempty"`
	IdleResetMS     *int64 `json:"idle_reset_ms,omitempty"`
	CooldownMS      *int64 `json:"cooldown_ms,omitempty"`
	RunTimeoutMS    *int64 `json:"run_timeout_ms,omitempty"`
	MaxPendingWakes *int   `json:"max_pending_wakes,omitempty"`
}

// 生效上下界：上界防误配成天文数字（等于关掉护栏），下界防「0 = 永不唤醒」。
const (
	hubConvMaxDepthLimit   = 100000
	hubConvPairLimitMax    = 1000
	hubConvWindowMSMax     = int64(24 * 60 * 60 * 1000)
	hubConvMaxPendingLimit = 10000
)

// loadHubConversationLimits 读 settings.json → 对话限额。
// TurnBudget 生产恒为 UnlimitedTurnBudget（用户口径：群策略限制取消）。
func loadHubConversationLimits() agenthub.ConversationLimits {
	limits := agenthub.ConversationLimits{TurnBudget: agenthub.UnlimitedTurnBudget}
	s, ok := readHubConversationSettings()
	if !ok {
		return limits.Effective()
	}
	if s.MaxDepth != nil && *s.MaxDepth > 0 {
		limits.MaxDepth = clampInt(*s.MaxDepth, 1, hubConvMaxDepthLimit)
	}
	if s.PairLimit != nil && *s.PairLimit > 0 {
		limits.PairLimit = clampInt(*s.PairLimit, 1, hubConvPairLimitMax)
	}
	if s.PairWindowMS != nil && *s.PairWindowMS > 0 {
		limits.PairWindow = msDur(clampMS(*s.PairWindowMS, hubConvWindowMSMax))
	}
	if s.IdleResetMS != nil && *s.IdleResetMS > 0 {
		limits.IdleReset = msDur(clampMS(*s.IdleResetMS, hubConvWindowMSMax))
	}
	if s.CooldownMS != nil && *s.CooldownMS >= 0 {
		// 0 是合法值（默认无冷却），显式写 0 与缺省同义。
		limits.Cooldown = msDur(clampMS(*s.CooldownMS, hubConvWindowMSMax))
	}
	if s.RunTimeoutMS != nil && *s.RunTimeoutMS > 0 {
		limits.RunTimeout = msDur(clampMS(*s.RunTimeoutMS, hubConvWindowMSMax))
	}
	if s.MaxPendingWakes != nil && *s.MaxPendingWakes > 0 {
		limits.MaxPendingWakes = clampInt(*s.MaxPendingWakes, 1, hubConvMaxPendingLimit)
	}
	return limits.Effective()
}

// readHubConversationSettings 读用户层 settings.json 的 hub.conversation（坏文件/缺段
// 都返回 ok=false，由调用方回落默认值）。
func readHubConversationSettings() (hubConversationSettings, bool) {
	raw, err := os.ReadFile(homeCfgPath("settings.json"))
	if err != nil {
		return hubConversationSettings{}, false
	}
	var root struct {
		Hub struct {
			Conversation *hubConversationSettings `json:"conversation"`
		} `json:"hub"`
	}
	if json.Unmarshal(raw, &root) != nil || root.Hub.Conversation == nil {
		return hubConversationSettings{}, false
	}
	return *root.Hub.Conversation, true
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// msDur 毫秒 → Duration（显式单位，避免 time.Duration(ms) 被当成纳秒）。
func msDur(ms int64) time.Duration { return time.Duration(ms) * time.Millisecond }

func clampMS(v, hi int64) int64 {
	if v < 0 {
		return 0
	}
	if v > hi {
		return hi
	}
	return v
}
