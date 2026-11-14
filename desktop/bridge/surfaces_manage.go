// surfaces_manage.go —— 插件页（PR-14b）的三个写/读命令：启用开关、卸载、审计。
//
// 为什么这三条要新增**命令**而不是让主进程直接改盘：`~/.go-code/surfaces/` 的写入
// 纪律（flock + .bak + 原子替换）全部住在 Go 的 surfaceStore 里（PR-13）。主进程再写一份
// 就等于把「谁有权改 index」变成两处判定，而那正是「禁用只是 UI 上灰了」的起点
// （surfaces_command.go 开头同一条纪律）。主进程只做形状校验 + 转发（electron/main.ts）。
//
// 与 `ui` / `surface_command` 同层级（dispatch 最前，不受 workspace 为空的分支影响）：
//
//	· 不进 events.Event 的 28 类（没有 run/session 归属）；
//	· 不落盘会话、不重放；
//	· 回执形状固定：成功 `{ok:true, ...}`，失败 `{ok:false, reason}`，reason 是封闭集合。
//
// 封闭集合只有三类，理由是页面会把它们映射成一句人话（多一类就多一处没人翻译的文案）：
//
//	unknown_id  index 里没有这个 id（装过又删了，或 id 打错）
//	bad_request 参数形状不对（id 不合法 / enabled 不是布尔）
//	error       磁盘或 IO 失败（重试可能有用）
package main

import (
	"strings"
)

// surfaceManage 回执的封闭 reason 集合（与 surface_command 的 denied/unknown_id/… 分开：
// 那一条是「插件调宿主」的语义，这一组是「用户点插件页」的语义，混用会让页面把
// 「你越权了」显示给一个正在点按钮的用户）。
const (
	surfaceManageUnknownID  = "unknown_id"
	surfaceManageBadRequest = "bad_request"
	surfaceManageError      = "error"
)

// surfaceManageDeny 写一条拒绝回执（code 与 data.reason 同值：主进程读 code、
// 脚本读 data.reason，两处都是固定集合）。
func (m *manager) surfaceManageDeny(c command, reason string) {
	m.respCode(c, false, reason, reason, map[string]any{"ok": false, "reason": reason})
}

// handleSurfaceSetEnabled 处理 `{type:"surface_set_enabled", payload:{id, enabled}}`。
//
// 成功之后推 `surfaces_changed`：渲染层 loader 收到就重算 provider（禁用 → 卸载；
// 启用 → 注册），标签变墓碑或复活是它的既有行为，这条命令不碰标签。
func (m *manager) handleSurfaceSetEnabled(c command) {
	id := strings.TrimSpace(str(c.Payload, "id"))
	enabled, ok := c.Payload["enabled"].(bool)
	if !ok || !surfaceIDPattern.MatchString(id) {
		// enabled 缺省**不**当 false：那会让一次少传字段的调用变成静默禁用。
		m.surfaceManageDeny(c, surfaceManageBadRequest)
		return
	}
	rec := m.surfaces.SetEnabled(id, enabled)
	m.replySurfaceReceipt(c, rec, map[string]any{"id": id, "enabled": enabled})
}

// handleSurfaceRemove 处理 `{type:"surface_remove", payload:{id}}`。
//
// 走 PR-13 的 Remove（目录进 `.trash/<id>-<ts>`，**不直接删**）—— 插件页的卸载按钮与
// `surface_remove` 工具是同一条磁盘路径，只有入口不同（一个用户点、一个模型调）。
func (m *manager) handleSurfaceRemove(c command) {
	id := strings.TrimSpace(str(c.Payload, "id"))
	if !surfaceIDPattern.MatchString(id) {
		m.surfaceManageDeny(c, surfaceManageBadRequest)
		return
	}
	rec := m.surfaces.Remove(id)
	m.replySurfaceReceipt(c, rec, map[string]any{"id": id})
}

// replySurfaceReceipt 把 store 的回执翻成命令回执（三种 status → 三个分支，不猜）。
//
// applied / cleaned 都改了磁盘（cleaned = index 有记录但目录早就没了，这次清掉），
// 都要推 surfaces_changed。rejected 只映射成 unknown_id：两个 handler 在调 store 之前
// 已经把 id 的形状校验过了（bad_request），所以走到这里只剩「index 里没这一条」这一种
// 解释 —— 不解析 store 的中文 reason，文案改了也不会让归类跟着漂。
func (m *manager) replySurfaceReceipt(c command, rec surfaceReceipt, data map[string]any) {
	switch rec.Status {
	case surfaceApplied, surfaceCleaned:
		data["status"] = rec.Status
		m.PushSurfacesChanged()
		m.resp(c, true, "", data)
	case surfaceRejected:
		m.surfaceManageDeny(c, surfaceManageUnknownID)
	default: // surfaceStatusError
		m.surfaceManageDeny(c, surfaceManageError)
	}
}

// handleSurfaceAudit 处理 `{type:"surface_audit", payload:{limit?}}`。
//
// limit 缺省 50、上限 200（界在 ReadAudit 里）。**不推 surfaces_changed**：读不改状态，
// 推一下会让每个刷新按钮都触发一次全量 loader 同步。
func (m *manager) handleSurfaceAudit(c command) {
	limit := 0 // 0 = 交给 ReadAudit 的缺省（50）
	if v, has := c.Payload["limit"]; has && v != nil {
		n, ok := surfaceAuditLimit(v)
		if !ok {
			m.surfaceManageDeny(c, surfaceManageBadRequest)
			return
		}
		limit = n
	}
	lines, err := m.surfaces.ReadAudit(limit)
	if err != nil {
		m.surfaceManageDeny(c, surfaceManageError)
		return
	}
	m.resp(c, true, "", map[string]any{"lines": lines})
}

// surfaceAuditLimit 把 payload.limit 收敛成一个整数。
//
// JSON 解出来是 float64（真的从 stdin 来的那一份），但进程内直接 dispatch 的调用方
// （测试、将来的内部入口）会传 int —— 两种都收，别的类型一律 bad_request。
// 负数/0 不在这里拦：ReadAudit 把「≤0」当缺省，那是它自己的契约（一个界只有一处）。
func surfaceAuditLimit(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	default:
		return 0, false
	}
}
