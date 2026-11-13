// surface_settings.go —— `settings.features.surfaceTools` 开关（PR-14 E）。
//
// 口径（02-pr-plan PR-14、03-code-plan §3.7）：
//
//	settings.json  { "features": { "surfaceTools": true } }   // 缺省 false
//
// 为什么默认 **false**（与 03 §3.7 的「默认 true」不同，这是本轮的裁决）：
// 三个 surface_* 工具是**常驻 schema**，每个轮次都要付 token；而插件能力在 V0.1 是
// 给愿意动手的人用的（先读 skill，再打开开关）。默认关 = 绝大多数用户不为一小撮人
// 的常驻成本买单；开了之后行为与「无条件注册」逐字相同。
//
// 与 browser_engine 同一套读法（bridge 只读 settings.json，客户端落盘）：不新建配置
// 系统、不改 mode.go、不引入第五个常驻工具。生效时机 = 下一次 loop 重建
// （与 browser_engine 的 switch 一致：设置页改完 → reload_settings → 下一次 new_session
// 带上新的工具面）。
package main

import (
	"encoding/json"
	"os"
	"strings"
)

// surfaceToolsFeature 开关在 settings.json 里的位置（唯一字符串常量：测试与文档读它）。
const surfaceToolsFeature = "features.surfaceTools"

// loadSurfaceToolsEnabled 读 `features.surfaceTools`。
//
// 所有「读不到」都归为 false：文件不存在 / JSON 坏 / 字段类型不是布尔。这与
// browser_engine 的 fail-closed 同一条：**默认关**的开关在读不出来时绝不能变成开
// （一个写坏的 settings.json 会让常驻 schema 重新出现，且没有任何地方会提示）。
func loadSurfaceToolsEnabled() bool {
	raw, err := os.ReadFile(homeCfgPath("settings.json"))
	if err != nil {
		return false
	}
	var outer struct {
		Features struct {
			// 指针是为了区分「没写」与「写了 false」—— 行为一样，但读代码的人要一眼看出
			// 「没写 = 关」是刻意的，不是 json 零值的副作用。
			SurfaceTools *bool `json:"surfaceTools"`
		} `json:"features"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		return false
	}
	return outer.Features.SurfaceTools != nil && *outer.Features.SurfaceTools
}

// surfaceToolsEnabled 当前进程看到的开关值。
//
// **不缓存**（每次读盘）：一次工具注册在 loop 重建时才发生（秒级频率），一份 200 字节
// 的 JSON 比「缓存与磁盘分叉」便宜得多 —— 而分叉的症状是「设置里明明关了，
// 新会话的工具面里还有它」，那是最难解释的一类 bug（browser_engine 也留着 reload 热读，
// 原因相同）。
func (m *manager) surfaceToolsEnabled() bool {
	if m != nil && m.surfaceToolsForced != nil {
		return *m.surfaceToolsForced // 仅测试注入（生产恒 nil）
	}
	return loadSurfaceToolsEnabled()
}

// describeSurfaceToolsFeature 给回执/日志用的一句话（`features.surfaceTools=false`）。
// 空字符串不是合法返回值 —— 有名字才能让人在 settings.json 里 grep 到它。
func describeSurfaceToolsFeature(on bool) string {
	state := "false"
	if on {
		state = "true"
	}
	return strings.TrimSpace(surfaceToolsFeature) + "=" + state
}
