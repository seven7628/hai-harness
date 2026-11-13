// surfaces_command_test.go —— `surface_command` 的白名单 + 审计（PR-14 D）。
//
// 全部走生产入口：`m.dispatch(cmd(...))`（不是直接调 handleSurfaceCommand），
// 磁盘走生产路径（HOME → appDataDir → surfaces/，见 manager.surfaces.Dir()），
// 响应读的是 stdout 上真正写出去的那一行 JSON。原因与 PR-12/13 的测试同一条
// （00-conventions §4.7）：这些函数是 bridge 的私有实现，绕过 dispatch 就等于绕过
// 「命令名 → 判定顺序」这一整层，而那层才是插件实际撞上的东西。
//
// 四条判据各有钉子：
//  1. 声明 + 宿主允许 → ok:true，**且** audit 有一行 ok:true（只有拒绝记录时对不了账）；
//  2. 未声明 → denied，audit 一行 ok:false；
//  3. 未知 id / 禁用 → 拒绝 + audit（禁用 = 能力一起收走）；
//  4. 宿主表外的命令（哪怕插件声明了）→ 拒绝（声明不等于授权）。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/tools"
)

// fakeSurface 直接在 store 根目录里铺一份装好的插件（index + <id>/surface.json）。
//
// 为什么不用 surfaceStore.Install（生产安装器）：它要一个**源目录**、会算 hash、会把
// 目录改名 —— 这些是 PR-13 的证据，与本轮的白名单判定无关；而用它就得在每条用例里
// 先造源目录，噪声盖过断言。这里铺的是**同一个磁盘协议**（index.json 的字段名与
// surface.json 的形状），读路径仍走生产代码（Enabled / Manifest）。
func fakeSurface(t *testing.T, m *manager, id string, enabled bool, commands []string) {
	t.Helper()
	root := m.surfaces.Dir()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("建 %s 失败: %v", dir, err)
	}
	mf := map[string]any{
		"id":       id,
		"name":     map[string]string{"zh": id},
		"version":  "1.0.0",
		"api":      1,
		"entry":    "panel.js",
		"commands": commands,
		"data":     []string{"surfaces_changed"},
	}
	raw, err := json.Marshal(mf)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "surface.json"), raw, 0o600); err != nil {
		t.Fatalf("写 surface.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "panel.js"), []byte("export const surface = {}\n"), 0o600); err != nil {
		t.Fatalf("写 panel.js: %v", err)
	}
	idx := map[string]any{
		"version": 1,
		"items": []map[string]any{{
			"id": id, "version": "1.0.0", "hash": strings.Repeat("a", 64),
			"source": "/tmp/src-" + id, "enabled": enabled,
			"installedAt": "2026-11-12T00:00:00Z",
		}},
	}
	raw, err = json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.json"), raw, 0o600); err != nil {
		t.Fatalf("写 index.json: %v", err)
	}
}

// auditLines 读 audit.jsonl（每行一个 JSON 对象）。
func auditLines(t *testing.T, m *manager) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(m.surfaces.Dir(), "audit.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("读 audit.jsonl: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("audit 行不是合法 JSON：%q", line)
		}
		out = append(out, rec)
	}
	return out
}

// commandResponse 取 id 对应的那条 command_response（dispatch 只回一条）。
func commandResponse(t *testing.T, m *manager, buf *syncBuf, id int) map[string]any {
	t.Helper()
	for _, line := range readLines(buf) {
		if line["event_type"] == "command_response" && intNumOf(line["id"]) == id {
			return line
		}
	}
	t.Fatalf("没有 id=%d 的 command_response", id)
	return nil
}

func intNumOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return -1
	}
}

func surfaceCommandPayload(id, name string) map[string]any {
	return map[string]any{"id": id, "name": name}
}

// ---- ① 声明 + 宿主允许 → ok:true，且审计也记一行 ----

func TestSurfaceCommandDeclaredAndAllowed(t *testing.T) {
	m, buf := testManager(t)
	fakeSurface(t, m, "demo", true, []string{"workspace_list"})

	m.dispatch(cmd(1, "surface_command", surfaceCommandPayload("demo", "workspace_list")))
	line := commandResponse(t, m, buf, 1)

	if line["ok"] != true {
		t.Fatalf("ok = %v，want true（声明且宿主允许）：%v", line["ok"], line)
	}
	data, _ := line["data"].(map[string]any)
	if data == nil {
		t.Fatalf("data 缺失：%v", line)
	}
	res, ok := data["result"].(map[string]any)
	if !ok {
		t.Fatalf("data.result 不是对象：%v", data)
	}
	if _, ok := res["workspaces"]; !ok {
		t.Fatalf("workspace_list 结果里没有 workspaces：%v", res)
	}

	lines := auditLines(t, m)
	if len(lines) != 1 {
		t.Fatalf("audit 行数 = %d，want 1：%v", len(lines), lines)
	}
	rec := lines[0]
	if rec["id"] != "demo" || rec["action"] != "command" || rec["cmd"] != "workspace_list" || rec["ok"] != true {
		t.Fatalf("audit 行 = %v，want {id:demo, action:command, cmd:workspace_list, ok:true}", rec)
	}
	if _, has := rec["reason"]; has {
		t.Fatalf("允许的调用不该有 reason：%v", rec)
	}
}

// ---- ② 未声明 → denied + 审计 ----

func TestSurfaceCommandUndeclaredDenied(t *testing.T) {
	m, buf := testManager(t)
	fakeSurface(t, m, "demo", true, []string{"git_snapshot"}) // 只声明了 git_snapshot

	m.dispatch(cmd(1, "surface_command", surfaceCommandPayload("demo", "workspace_list")))
	line := commandResponse(t, m, buf, 1)

	if line["ok"] != false {
		t.Fatalf("ok = %v，want false（未声明）：%v", line["ok"], line)
	}
	if line["code"] != "denied" {
		t.Fatalf("code = %v，want denied：%v", line["code"], line)
	}
	data, _ := line["data"].(map[string]any)
	if data["reason"] != "denied" {
		t.Fatalf("data.reason = %v，want denied", data["reason"])
	}

	lines := auditLines(t, m)
	if len(lines) != 1 || lines[0]["ok"] != false || lines[0]["cmd"] != "workspace_list" {
		t.Fatalf("audit = %v，want 一行 ok:false 的 workspace_list", lines)
	}
	if lines[0]["reason"] == nil {
		t.Fatal("拒绝的审计行必须带 reason（否则对不上「为什么被拒」）")
	}
}

// ---- ③ 未知 id / 禁用 → 拒绝 + 审计 ----

func TestSurfaceCommandUnknownIDDenied(t *testing.T) {
	m, buf := testManager(t)
	m.dispatch(cmd(1, "surface_command", surfaceCommandPayload("ghost", "workspace_list")))
	line := commandResponse(t, m, buf, 1)

	if line["ok"] != false || line["code"] != "unknown_id" {
		t.Fatalf("未知 id 的响应 = %v，want ok:false code:unknown_id", line)
	}
	lines := auditLines(t, m)
	if len(lines) != 1 || lines[0]["id"] != "ghost" || lines[0]["ok"] != false {
		t.Fatalf("audit = %v，want 一行针对 ghost 的拒绝", lines)
	}
}

func TestSurfaceCommandDisabledPluginDenied(t *testing.T) {
	m, buf := testManager(t)
	fakeSurface(t, m, "demo", false, []string{"workspace_list"}) // 装着但 enabled=false

	m.dispatch(cmd(1, "surface_command", surfaceCommandPayload("demo", "workspace_list")))
	line := commandResponse(t, m, buf, 1)

	if line["ok"] != false {
		t.Fatalf("禁用插件仍被放行：%v", line)
	}
	lines := auditLines(t, m)
	if len(lines) != 1 || lines[0]["ok"] != false {
		t.Fatalf("audit = %v，want 一行拒绝", lines)
	}
}

// 非法 id：连查表都不该发生（它是路径段），且不能把未校验的字符串写进审计行。
func TestSurfaceCommandInvalidIDDenied(t *testing.T) {
	m, buf := testManager(t)
	for i, bad := range []string{"", "../etc", "Demo", "a"} {
		m.dispatch(cmd(i+1, "surface_command", surfaceCommandPayload(bad, "workspace_list")))
		line := commandResponse(t, m, buf, i+1)
		if line["ok"] != false {
			t.Fatalf("id %q 被放行：%v", bad, line)
		}
	}
	lines := auditLines(t, m)
	if len(lines) != 4 {
		t.Fatalf("audit 行数 = %d，want 4（每条拒绝都要留下痕迹）", len(lines))
	}
	for _, rec := range lines {
		if s, _ := rec["id"].(string); s == "" {
			t.Fatalf("审计行 id 为空：%v", rec)
		}
	}
}

// ---- ④ 声明了但宿主表里没有 → 拒绝（声明不等于授权）----

func TestSurfaceCommandNotInHostWhitelistDenied(t *testing.T) {
	m, buf := testManager(t)
	// 插件把自己想调的都写进 manifest —— 这正是攻击面：manifest 是插件自己写的。
	fakeSurface(t, m, "demo", true, []string{"file_save", "workspace_add", "session_delete", "workspace_list"})
	for i, name := range []string{"file_save", "workspace_add", "session_delete"} {
		m.dispatch(cmd(i+1, "surface_command", surfaceCommandPayload("demo", name)))
		line := commandResponse(t, m, buf, i+1)
		if line["ok"] != false || line["code"] != "denied" {
			t.Fatalf("%s 被放行（写类命令绝不能开）：%v", name, line)
		}
	}
	lines := auditLines(t, m)
	if len(lines) != 3 {
		t.Fatalf("audit 行数 = %d，want 3", len(lines))
	}
}

// 宿主的只读白名单**恰好**是这四条：多一条都要在这里显式登记（先想清楚后果）。
func TestSurfaceHostCommandWhitelistIsClosed(t *testing.T) {
	got := surfaceHostCommandNames()
	want := []string{"git_snapshot", "session_list", "ui_inspect", "workspace_list"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("宿主白名单 = %v，want %v\n"+
			"加一条之前先回答：恶意插件连续调用 1000 次，用户会损失什么？", got, want)
	}
}

// ---- ⑤ 内部错误不原样回给插件 ----

func TestSurfaceCommandHandlerErrorIsOpaque(t *testing.T) {
	m, buf := testManager(t)
	fakeSurface(t, m, "demo", true, []string{"session_list"})
	// workspace 为空 → handler 报错。给插件看的只能是固定文案 `error`。
	m.dispatch(cmd(1, "surface_command", map[string]any{"id": "demo", "name": "session_list", "args": map[string]any{}}))
	line := commandResponse(t, m, buf, 1)
	if line["ok"] != false || line["code"] != "error" {
		t.Fatalf("handler 失败 = %v，want ok:false code:error", line)
	}
	data, _ := line["data"].(map[string]any)
	if data["reason"] != "error" {
		t.Fatalf("data.reason = %v，want 固定文案 error（内部 error 只进 audit）", data["reason"])
	}
	if msg, _ := line["error"].(string); strings.Contains(msg, "workspace 为空") {
		t.Fatalf("内部错误原文泄漏给插件了：%q", msg)
	}
}

// ---- ⑥ ui_inspect：插件能读渲染层快照（缓存为空时是状态，不是失败）----

func TestSurfaceCommandUIInspectReadsSnapshot(t *testing.T) {
	m, buf := testManager(t)
	fakeSurface(t, m, "demo", true, []string{"ui_inspect"})

	// 还没有快照 → ok:true + result.ok=false reason=no_snapshot（可编程状态）。
	m.dispatch(cmd(1, "surface_command", surfaceCommandPayload("demo", "ui_inspect")))
	line := commandResponse(t, m, buf, 1)
	data, _ := line["data"].(map[string]any)
	res, _ := data["result"].(map[string]any)
	if line["ok"] != true || res["ok"] != false || res["reason"] != "no_snapshot" {
		t.Fatalf("无快照时的响应 = %v，want ok:true + result{ok:false,reason:no_snapshot}", line)
	}

	// 推一份快照后再问 → 拿到 data + ageMs。
	dispatchUISnapshot(t, m, uiSnapshotPayload())
	m.dispatch(cmd(2, "surface_command", surfaceCommandPayload("demo", "ui_inspect")))
	line = commandResponse(t, m, buf, 2)
	data, _ = line["data"].(map[string]any)
	res, _ = data["result"].(map[string]any)
	if res["ok"] != true {
		t.Fatalf("有快照时的响应 = %v，want result.ok=true", res)
	}
	if _, has := res["ageMs"]; !has {
		t.Fatalf("ui_inspect 结果缺 ageMs：%v", res)
	}
}

// ---- ⑦ 开关：false 时工具不在引擎里，true 时在（走真实 settings.json 读路径）----

func TestSurfaceToolsFollowFeatureSwitch(t *testing.T) {
	writeFeatures := func(t *testing.T, on bool) {
		t.Helper()
		home := os.Getenv("HOME")
		dir := filepath.Join(home, ".go-code")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("建 %s: %v", dir, err)
		}
		state := "false"
		if on {
			state = "true"
		}
		body := `{"theme":"dark","features":{"surfaceTools":` + state + `}}`
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600); err != nil {
			t.Fatalf("写 settings.json: %v", err)
		}
	}
	toolNames := func(m *manager) map[string]bool {
		eng := tools.NewToolEngine()
		m.registerSurfaceTools(eng)
		got := map[string]bool{}
		for _, tl := range eng.Tools() {
			got[tl.Name()] = true
		}
		return got
	}

	t.Run("缺省（没有 settings.json）→ 不注册", func(t *testing.T) {
		m, _ := testManager(t)
		got := toolNames(m)
		for _, name := range []string{"surface_install", "surface_remove", "surface_list"} {
			if got[name] {
				t.Fatalf("%s 缺省就注册了 —— 常驻 schema 的成本纪律要求缺省 false", name)
			}
		}
	})

	t.Run("features.surfaceTools=false → 不注册", func(t *testing.T) {
		m, _ := testManager(t)
		writeFeatures(t, false)
		got := toolNames(m)
		for _, name := range []string{"surface_install", "surface_remove", "surface_list"} {
			if got[name] {
				t.Fatalf("%s 在开关 false 时仍被注册", name)
			}
		}
	})

	t.Run("features.surfaceTools=true → 三个都注册", func(t *testing.T) {
		m, _ := testManager(t)
		writeFeatures(t, true)
		got := toolNames(m)
		for _, name := range []string{"surface_install", "surface_remove", "surface_list"} {
			if !got[name] {
				t.Fatalf("%s 在开关 true 时没注册", name)
			}
		}
	})

	t.Run("ui_inspect 与开关无关（永远在）", func(t *testing.T) {
		m, _ := testManager(t)
		eng := tools.NewToolEngine()
		m.registerUITools(eng)
		m.registerSurfaceTools(eng) // 开关 false
		var hasUI bool
		for _, tl := range eng.Tools() {
			if tl.Name() == "ui_inspect" {
				hasUI = true
			}
		}
		if !hasUI {
			t.Fatal("ui_inspect 被开关连坐 —— 它是只读观测面，必须无条件注册")
		}
	})

	t.Run("settings.json 写坏 → 当作关（fail-closed）", func(t *testing.T) {
		m, _ := testManager(t)
		dir := filepath.Join(os.Getenv("HOME"), ".go-code")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("建目录: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"features":`), 0o600); err != nil {
			t.Fatalf("写坏 JSON: %v", err)
		}
		if got := toolNames(m); got["surface_install"] {
			t.Fatal("settings.json 写坏时放开了工具 —— 必须是 fail-closed")
		}
	})
}
