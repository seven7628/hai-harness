package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/seven7628/hai-harness/im"
	implugin "github.com/seven7628/hai-harness/plugin/im"
)

// imPluginOf 取全局 IM 插件实例（manager.imPlugin；不存在时回退 workspace registry）。
func (m *manager) imPluginOf(ws string) *implugin.Plugin {
	if m.imPlugin != nil {
		return m.imPlugin
	}
	// 回退：从 workspace registry 取（测试注入场景）
	plg := m.runtime(ws).plugins
	if cap, ok := plg.Get(implugin.ID); ok {
		if p, ok := cap.(*implugin.Plugin); ok {
			return p
		}
	}
	return nil
}

// imConfigPath IM 配置文件路径（与插件默认一致）。
func (m *manager) imConfigPath() string {
	return implugin.DefaultConfigPath()
}

// handleIMCommand im_* 桥接命令（IM 集成管理面，docs/IM_INTEGRATION.md §9.2）。
// 命令：im_status / im_schema_list / im_config_get / im_config_set /
//
//	im_route_list / im_route_set / im_route_del / im_mirror_list / im_mirror_set / im_mirror_del /
//	im_bind_confirm
func (m *manager) handleIMCommand(c command) {
	ws := str(c.Payload, "workspace")
	if ws == "" {
		m.resp(c, false, "workspace 为空", nil)
		return
	}
	p := m.imPluginOf(ws)
	if p == nil {
		m.resp(c, false, "IM 插件不可用", nil)
		return
	}
	ctx := context.Background()
	// 归属：payload.owner → session_id → workspace（与 hooks 同一条派生，见 owner_wire.go）。
	// 没有归属时按「未绑定」返回（ownerEnabled=false），**不**回落到全局状态。
	ownerID, ownerErr := ownerFromPayload(c)
	if ownerErr != nil {
		m.resp(c, false, ownerErr.Error(), nil)
		return
	}

	switch c.Type {
	case "im_status": // 各 Gateway 连接状态 + 主开关 + **当前归属**的启用状态
		cfg, _ := im.LoadConfig(m.imConfigPath())
		rt := p.Runtime()
		gateways := []map[string]any{}
		for _, gc := range cfg.Gateways {
			st := "not_started"
			if rt != nil {
				if g, ok := rt.Gateways[gc.ID]; ok {
					st = string(g.Status())
				}
			}
			gateways = append(gateways, map[string]any{
				"id": gc.ID, "type": gc.Type, "enabled": gc.Enabled, "status": st,
			})
		}
		// 连接是共享的（同一份 gateways），启用状态按归属给 —— A 群关了不会显示 B 也关。
		view := p.OwnerView(ownerID)
		m.resp(c, true, "", map[string]any{
			"enabled":      cfg.Enabled,
			"gateways":     gateways,
			"owner":        ownerID,
			"ownerLabel":   ownerScopeLabel(ownerID),
			"ownerEnabled": view.Enabled,
			"ownerRoutes":  view.Routes,
			"owners":       cfg.OwnersAll(),
		})

	case "im_chat_list": // 群列表（路由绑定下拉数据源；按 gateway_id 查）
		gatewayID := str(c.Payload, "gateway_id")
		if gatewayID == "" {
			m.resp(c, false, "缺少 gateway_id", nil)
			return
		}
		rt := p.Runtime()
		if rt == nil {
			m.resp(c, false, "IM 未启用", nil)
			return
		}
		gw, ok := rt.Gateways[gatewayID]
		if !ok {
			m.resp(c, false, "gateway 未启动: "+gatewayID, nil)
			return
		}
		cl, ok := gw.(im.ChatLister)
		if !ok {
			m.resp(c, false, "该渠道不支持群列表", nil)
			return
		}
		chats, err := cl.ListChats(ctx)
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.resp(c, true, "", map[string]any{"chats": chats})

	case "im_schema_list": // 全部 Gateway 配置 Schema（UI 动态表单数据源）
		m.resp(c, true, "", map[string]any{"schemas": im.Schemas()})

	case "im_config_get": // 当前完整配置
		cfg, err := im.LoadConfig(m.imConfigPath())
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.resp(c, true, "", map[string]any{"config": cfg})

	case "im_config_set": // 保存配置（整体替换；校验后落盘 + 热应用）
		var cfg im.Config
		if err := json.Unmarshal(mustJSON(c.Payload["config"]), &cfg); err != nil {
			m.resp(c, false, "配置解析失败: "+err.Error(), nil)
			return
		}
		// owners（归属账本）不属于编辑器表单：payload 里没有就沿用磁盘上的，
		// 否则「保存一次配置」会把所有人刚设的归属启用状态悄悄重置。
		if cfg.Owners == nil {
			if prev, perr := im.LoadConfig(m.imConfigPath()); perr == nil {
				cfg.Owners = prev.Owners
			}
		}
		// 迁移：为「有路由但没有显式状态」的归属补 enabled=true（升级不静默丢消息）。
		cfg.MigrateOwners()
		cfg, err := cfg.Validate()
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		if err := im.SaveConfig(m.imConfigPath(), cfg); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.applyIMConfig(ctx, ws, cfg)
		m.resp(c, true, "", map[string]any{"owners": cfg.OwnersAll()})

	case "im_route_list": // 路由表（渠道维度；给了 owner 就只返回该归属的路由）
		cfg, _ := im.LoadConfig(m.imConfigPath())
		// 聚合所有 gateway 的路由（带 gateway_id 标注 + 归属）。
		// owner 过滤是本条隔离的读半边：A 群的管理面看不到 B 智能体的路由。
		routes := []map[string]any{}
		for _, g := range cfg.Gateways {
			for _, r := range g.Routes {
				ro := r.EffectiveOwner()
				if ownerID != "" && ro != ownerID {
					continue
				}
				routes = append(routes, map[string]any{
					"gateway_id":   g.ID,
					"chat":         r.Chat,
					"workspace":    r.Workspace,
					"agent":        r.Agent,
					"group":        r.Group,
					"owner":        ro,
					"ownerLabel":   ownerScopeLabel(ro),
					"ownerEnabled": cfg.OwnerEnabled(ro),
					"target":       r.TargetKind(),
				})
			}
		}
		m.resp(c, true, "", map[string]any{
			"routes": routes,
			"owner":  ownerID,
			"owners": cfg.OwnersAll(),
		})

	case "im_route_set": // 增/改路由（Chat → workspace/agent/group，挂在指定 gateway 下）
		var rc im.RouteConfig
		if err := json.Unmarshal(mustJSON(c.Payload["route"]), &rc); err != nil {
			m.resp(c, false, "路由解析失败: "+err.Error(), nil)
			return
		}
		gatewayID := str(c.Payload, "gateway_id")
		if gatewayID == "" {
			m.resp(c, false, "缺少 gateway_id（路由属于渠道）", nil)
			return
		}
		if _, ok := rc.Chat.Normalize(); !ok {
			m.resp(c, false, "路由缺少 chat", nil)
			return
		}
		// 归属**由投递目标派生**（workspace/agent/group），不取 payload.owner：
		// 设置页可能在「A 智能体」会话里给 B 项目加路由 —— 那是合法操作，路由的归属
		// 只能是它的目标本身（否则「关 A」会停掉 B 的投递）。payload.owner 在
		// im_route_list 里用于过滤，在这里不做授权边界。
		// 显式 route.owner 若给出，必须与目标一致（见 im.Config.Validate）。
		if strings.TrimSpace(rc.Owner) != "" {
			derived := (im.RouteConfig{Chat: rc.Chat, Workspace: rc.Workspace, Agent: rc.Agent, Group: rc.Group}).EffectiveOwner()
			if rc.Owner != derived {
				m.resp(c, false, fmt.Sprintf("route.owner=%s 与投递目标（%s）不一致", rc.Owner, derived), nil)
				return
			}
		}
		if rc.TargetKind() == "" {
			m.resp(c, false, "路由缺少投递目标（workspace / agent / group+agent）", nil)
			return
		}
		cfg, err := im.LoadConfig(m.imConfigPath())
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		// 找 gateway 实例
		gi := -1
		for i := range cfg.Gateways {
			if cfg.Gateways[i].ID == gatewayID {
				gi = i
				break
			}
		}
		if gi < 0 {
			m.resp(c, false, "gateway 不存在: "+gatewayID, nil)
			return
		}
		// 同 Chat 已存在 → 替换；否则追加
		replaced := false
		for i := range cfg.Gateways[gi].Routes {
			if cfg.Gateways[gi].Routes[i].Chat.Gateway == rc.Chat.Gateway && cfg.Gateways[gi].Routes[i].Chat.ChatID == rc.Chat.ChatID && cfg.Gateways[gi].Routes[i].Chat.ThreadID == rc.Chat.ThreadID {
				cfg.Gateways[gi].Routes[i] = rc
				replaced = true
				break
			}
		}
		if !replaced {
			cfg.Gateways[gi].Routes = append(cfg.Gateways[gi].Routes, rc)
		}
		// 显式记下该归属的启用状态（新配的路由默认启用；升级后也看得见来源）。
		if o := rc.EffectiveOwner(); o != "" {
			if st, ok := cfg.Owners[o]; !ok || st.Enabled == nil {
				on := true
				_ = cfg.SetOwnerEnabled(o, &on)
			}
		}
		cfg.MigrateOwners()
		cfg, err = cfg.Validate()
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		if err := im.SaveConfig(m.imConfigPath(), cfg); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.applyIMConfig(ctx, ws, cfg)
		m.resp(c, true, "", map[string]any{
			"owner":        rc.EffectiveOwner(),
			"ownerLabel":   ownerScopeLabel(rc.EffectiveOwner()),
			"ownerEnabled": cfg.OwnerEnabled(rc.EffectiveOwner()),
		})

	case "im_route_del": // 删路由（按 Chat + gateway_id；给了 owner 再按归属收窄）
		var chat im.Chat
		if err := json.Unmarshal(mustJSON(c.Payload["chat"]), &chat); err != nil {
			m.resp(c, false, "chat 解析失败: "+err.Error(), nil)
			return
		}
		gatewayID := str(c.Payload, "gateway_id")
		cfg, err := im.LoadConfig(m.imConfigPath())
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		for i := range cfg.Gateways {
			g := &cfg.Gateways[i]
			if gatewayID != "" && g.ID != gatewayID {
				continue
			}
			out := g.Routes[:0]
			for _, rc := range g.Routes {
				if rc.Chat.Gateway == chat.Gateway && rc.Chat.ChatID == chat.ChatID && rc.Chat.ThreadID == chat.ThreadID {
					// owner 过滤：管理面在 A 归属下不该删掉 B 归属的路由
					//（Chat 唯一性由 im_route_set 保证，这里只是防御 + 语义清晰）。
					if ownerID != "" && rc.EffectiveOwner() != ownerID {
						out = append(out, rc)
						continue
					}
					continue
				}
				out = append(out, rc)
			}
			g.Routes = out
		}
		if err := im.SaveConfig(m.imConfigPath(), cfg); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.applyIMConfig(ctx, ws, cfg)
		m.resp(c, true, "", nil)

	case "im_mirror_list": // 镜像表
		cfg, _ := im.LoadConfig(m.imConfigPath())
		m.resp(c, true, "", map[string]any{"mirrors": cfg.Mirrors})

	case "im_mirror_set": // 增/改镜像（session ↔ Chat）
		var mc im.MirrorConfig
		if err := json.Unmarshal(mustJSON(c.Payload["mirror"]), &mc); err != nil {
			m.resp(c, false, "镜像解析失败: "+err.Error(), nil)
			return
		}
		if _, ok := mc.Chat.Normalize(); !ok {
			m.resp(c, false, "镜像缺少 chat", nil)
			return
		}
		if mc.SessionID == "" {
			m.resp(c, false, "镜像缺少 session_id", nil)
			return
		}
		if mc.Mode != im.MirrorPush && mc.Mode != im.MirrorBidirectional {
			m.resp(c, false, "镜像 mode 必须是 push 或 bidirectional", nil)
			return
		}
		cfg, err := im.LoadConfig(m.imConfigPath())
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		replaced := false
		for i := range cfg.Mirrors {
			if cfg.Mirrors[i].SessionID == mc.SessionID && cfg.Mirrors[i].Chat.Gateway == mc.Chat.Gateway && cfg.Mirrors[i].Chat.ChatID == mc.Chat.ChatID {
				cfg.Mirrors[i] = mc
				replaced = true
				break
			}
		}
		if !replaced {
			cfg.Mirrors = append(cfg.Mirrors, mc)
		}
		if err := im.SaveConfig(m.imConfigPath(), cfg); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.applyIMConfig(ctx, ws, cfg)
		m.resp(c, true, "", nil)

	case "im_mirror_del": // 删镜像（按 session + chat）
		var mc im.MirrorConfig
		if err := json.Unmarshal(mustJSON(c.Payload["mirror"]), &mc); err != nil {
			m.resp(c, false, "镜像解析失败: "+err.Error(), nil)
			return
		}
		cfg, err := im.LoadConfig(m.imConfigPath())
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		out := cfg.Mirrors[:0]
		for _, m2 := range cfg.Mirrors {
			if m2.SessionID == mc.SessionID && m2.Chat.Gateway == mc.Chat.Gateway && m2.Chat.ChatID == mc.Chat.ChatID {
				continue
			}
			out = append(out, m2)
		}
		cfg.Mirrors = out
		if err := im.SaveConfig(m.imConfigPath(), cfg); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.applyIMConfig(ctx, ws, cfg)
		m.resp(c, true, "", nil)

	case "im_bind_confirm": // 绑定确认（陌生 Chat 首次消息 → 桌面端弹窗 → 用户决策）
		var chat im.Chat
		if err := json.Unmarshal(mustJSON(c.Payload["chat"]), &chat); err != nil {
			m.resp(c, false, "chat 解析失败: "+err.Error(), nil)
			return
		}
		approve := boolVal(c.Payload["approve"])
		workspace := str(c.Payload, "workspace") // 确认绑定的目标 workspace
		cfg, err := im.LoadConfig(m.imConfigPath())
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		if !approve {
			// 拒绝：记录审计，不写路由
			if rt := p.Runtime(); rt != nil {
				rt.Security.Audit(im.AuditEntry{
					Gateway: chat.Gateway, Chat: chat.String(), Action: "bind_rejected",
					Detail: "用户拒绝绑定",
				})
			}
			m.resp(c, true, "", nil)
			return
		}
		// 批准：写路由（Chat → workspace，挂到对应 gateway 实例）+ 标记已确认
		if strings.TrimSpace(workspace) == "" {
			m.resp(c, false, "绑定确认需要 workspace", nil)
			return
		}
		// 定位 gateway 实例（payload.gateway_id；缺省按 chat.gateway 类型找第一个）
		gatewayID := str(c.Payload, "gateway_id")
		gi := -1
		for i := range cfg.Gateways {
			if gatewayID != "" && cfg.Gateways[i].ID == gatewayID {
				gi = i
				break
			}
			if gatewayID == "" && cfg.Gateways[i].Type == chat.Gateway {
				gi = i
				break
			}
		}
		if gi < 0 {
			m.resp(c, false, "找不到 gateway 实例（类型 "+chat.Gateway+"）", nil)
			return
		}
		cfg.Gateways[gi].Routes = append(cfg.Gateways[gi].Routes, im.RouteConfig{Chat: chat, Workspace: workspace})
		if err := im.SaveConfig(m.imConfigPath(), cfg); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		if rt := p.Runtime(); rt != nil {
			rt.Security.ConfirmChat(chat)
			rt.Security.Audit(im.AuditEntry{
				Gateway: chat.Gateway, Chat: chat.String(), Action: "bind_confirmed",
				Detail: "绑定到 " + workspace,
			})
		}
		m.applyIMConfig(ctx, ws, cfg)
		m.resp(c, true, "", nil)
	}
}

// applyIMConfig 热应用 IM 配置：
//   - Router/Security：直接 Update（路由/镜像/授权即时生效）
//   - Gateway 连接：差异重建（新增的启动、删除的停止、配置变化的停止后按新配置重启、
//     未变化的保留连接——避免无谓断连）
func (m *manager) applyIMConfig(ctx context.Context, ws string, cfg im.Config) {
	p := m.imPluginOf(ws)
	if p == nil {
		return
	}
	rt := p.Runtime()
	if rt == nil {
		return
	}
	// 主开关关闭：停止全部 Gateway 连接（IM 远程能力立即失效；配置保留）
	if !cfg.Enabled {
		for id, g := range rt.Gateways {
			_ = g.Stop(ctx)
			delete(rt.Gateways, id)
		}
		rt.Router.Update(cfg)
		rt.Security.Update(cfg)
		rt.Config = cfg
		return
	}
	// ① 路由/镜像/安全即时更新
	rt.Router.Update(cfg)
	rt.Security.Update(cfg)

	// ② Gateway 连接差异重建（用旧 rt.Config 对比——必须先于 rt.Config 更新，
	//    否则 oldCfg 已是新配置，恒等 → 永不重建）
	old := rt.Gateways // id → Gateway（旧实例）
	oldCfg := map[string]im.GatewayConfig{}
	for _, gc := range rt.Config.Gateways {
		oldCfg[gc.ID] = gc
	}
	newMap := map[string]im.GatewayConfig{}
	for _, gc := range cfg.Gateways {
		if gc.Enabled {
			newMap[gc.ID] = gc
		}
	}
	// 删除的：停止
	for id, g := range old {
		if _, keep := newMap[id]; !keep {
			_ = g.Stop(ctx)
			delete(rt.Gateways, id)
		}
	}
	// 新增/配置变化的：重建（先停旧的再启新的）
	for id, gc := range newMap {
		oldG, existed := old[id]
		oc, hadCfg := oldCfg[id]
		if existed && hadCfg && gatewayConfigEqual(gc, oc) {
			continue // 未变化，保留连接
		}
		if existed {
			_ = oldG.Stop(ctx)
		}
		g, err := im.New(gc.Type, gc.Config)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ im 重建 %s: %v\n", id, err)
			continue
		}
		if err := g.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "✗ im 启动 %s: %v\n", id, err)
			continue
		}
		rt.Gateways[id] = g
		// 新连接重新 Attach 回调
		g.OnMessage(m.imBridgeMessageSink())
		g.OnAction(m.imBridgeActionSink())
	}

	// ③ 最后更新 rt.Config（差异对比已完成）
	rt.Config = cfg
}

// gatewayConfigEqual 判断 Gateway 配置是否等价（type + config 字节比较）。
func gatewayConfigEqual(a, b im.GatewayConfig) bool {
	if a.Type != b.Type {
		return false
	}
	return bytes.Equal(a.Config, b.Config)
}

// mustJSON 把 payload 值序列化为 JSON 字节（map/string 都支持）。
func mustJSON(v any) []byte {
	if s, ok := v.(string); ok {
		return []byte(s)
	}
	b, _ := json.Marshal(v)
	return b
}

// imAuditf 记录 IM 审计（快捷）。
func (m *manager) imAuditf(ws, action, detail string) {
	if p := m.imPluginOf(ws); p != nil {
		if rt := p.Runtime(); rt != nil {
			rt.Security.Audit(im.AuditEntry{Action: action, Detail: detail})
		}
	}
}

// 确保 fmt 被使用（错误构造）。
var _ = fmt.Sprintf
