// surfaces_manage_test.go —— 插件页（PR-14b）的写路径：启用开关 / 卸载 / 审计尾部。
//
// 全部从**生产入口**进（00-conventions §4.7）：`m.dispatch(cmd(...))` → Go 的
// surfaceStore → 临时 HOME/AppData 里的真文件；响应读的是 stdout 上真正写出去的那一行。
// 绕开 dispatch 直接调 store 就等于绕过「命令名 → 形状校验 → 归类」这一层，
// 而那层才是页面真正撞上的东西（与 surfaces_command_test.go 同一条纪律）。
//
// 四条判据各有钉子：
//  1. 翻转**只动 enabled**：hash / installedAt / source 逐字不变，插件文件一个都没少；
//  2. `.bak` 是**变更前**的内容（写成变更后 = 回滚点指向坏状态）；
//  3. 未知 id → `unknown_id`，**index 一个字节不改**、不推 surfaces_changed；
//  4. ReadAudit 的界（缺省 50 / 上限 200）与「坏行跳过、不整段失败」。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// surfaceLine 从一次 dispatch 的全部输出里挑出「这条命令的响应」与「推没推 surfaces_changed」。
//
// 必须一次取完：readLines 是 Take（取走即清空），先找响应再找 UI 事件会把后者丢了 ——
// 那种写法会一直绿，直到有人真的删掉 push（假绿形态，00-conventions §4.7）。
func surfaceLine(t *testing.T, buf *syncBuf, id int) (resp map[string]any, pushed int) {
	t.Helper()
	for _, line := range readLines(buf) {
		if line["event_type"] == "command_response" && intNumOf(line["id"]) == id {
			resp = line
		}
		if line["type"] == "ui" && line["op"] == "surfaces_changed" {
			pushed++
		}
	}
	if resp == nil {
		t.Fatalf("没有 id=%d 的 command_response", id)
	}
	return resp, pushed
}

// indexBytes 读一份 index.json 的**原始字节**（「一个字节都没改」只能这样断言）。
func indexBytes(t *testing.T, m *manager) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(m.surfaces.Dir(), "index.json"))
	if err != nil {
		t.Fatalf("读 index.json: %v", err)
	}
	return raw
}

func entryOf(t *testing.T, raw []byte, id string) surfaceEntry {
	t.Helper()
	var idx surfaceIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatalf("index.json 不是合法 JSON: %v", err)
	}
	pos := idx.pos(id)
	if pos < 0 {
		t.Fatalf("index 里没有 %s：%s", id, raw)
	}
	return idx.Items[pos]
}

// ---- ① 翻转 + .bak + audit + 推送 ----

func TestSurfaceSetEnabledFlipsIndexAndAudits(t *testing.T) {
	m, buf := testManager(t)
	fakeSurface(t, m, "demo", true, []string{"workspace_list"})
	before := indexBytes(t, m)
	prevEntry := entryOf(t, before, "demo")

	m.dispatch(cmd(1, "surface_set_enabled", map[string]any{"id": "demo", "enabled": false}))
	resp, pushed := surfaceLine(t, buf, 1)

	if resp["ok"] != true {
		t.Fatalf("ok = %v，want true：%v", resp["ok"], resp)
	}
	data, _ := resp["data"].(map[string]any)
	if data["id"] != "demo" || data["enabled"] != false || data["status"] != surfaceApplied {
		t.Fatalf("data = %v，want {id:demo, enabled:false, status:applied}", data)
	}

	// index：enabled 翻了，其余字段逐字不变。
	after := entryOf(t, indexBytes(t, m), "demo")
	if after.Enabled {
		t.Fatal("index 里 enabled 仍是 true（开关没落盘）")
	}
	if after.Hash != prevEntry.Hash || after.InstalledAt != prevEntry.InstalledAt ||
		after.Version != prevEntry.Version || after.Source != prevEntry.Source {
		t.Fatalf("翻转动了别的字段：before=%+v after=%+v", prevEntry, after)
	}

	// .bak = **变更前**的内容（逐字节）。
	bak, err := os.ReadFile(filepath.Join(m.surfaces.Dir(), "index.json.bak"))
	if err != nil {
		t.Fatalf("没有 .bak（回滚点丢了）: %v", err)
	}
	if string(bak) != string(before) {
		t.Fatalf(".bak 不是变更前的内容：\n got=%s\nwant=%s", bak, before)
	}

	// 插件文件一个都没少：禁用 ≠ 卸载。
	dir := filepath.Join(m.surfaces.Dir(), "demo")
	for _, f := range []string{"surface.json", "panel.js"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("禁用删了 %s（禁用不该动文件）: %v", f, err)
		}
	}

	// audit：一行 {id, action:disable, ok:true}。
	lines := auditLines(t, m)
	if len(lines) != 1 {
		t.Fatalf("audit 行数 = %d，want 1：%v", len(lines), lines)
	}
	if rec := lines[0]; rec["id"] != "demo" || rec["action"] != "disable" || rec["ok"] != true {
		t.Fatalf("audit 行 = %v，want {id:demo, action:disable, ok:true}", rec)
	}
	if _, has := lines[0]["reason"]; has {
		t.Fatalf("成功的翻转不该有 reason：%v", lines[0])
	}

	if pushed != 1 {
		t.Fatalf("surfaces_changed 推了 %d 次，want 1（loader 靠它重算 provider）", pushed)
	}

	// 再翻回来：action=enable，index 回到原样（顺序与字段都不变）。
	m.dispatch(cmd(2, "surface_set_enabled", map[string]any{"id": "demo", "enabled": true}))
	resp2, _ := surfaceLine(t, buf, 2)
	if resp2["ok"] != true {
		t.Fatalf("enable 回执 = %v，want ok:true", resp2)
	}
	if got := entryOf(t, indexBytes(t, m), "demo"); !got.Enabled {
		t.Fatal("enable 没落盘")
	}
	lines = auditLines(t, m)
	if last := lines[len(lines)-1]; last["action"] != "enable" || last["ok"] != true {
		t.Fatalf("最后一条 audit = %v，want {action:enable, ok:true}", last)
	}
}

// ---- ② 未知 id → unknown_id，index 一个字节不改 ----

func TestSurfaceSetEnabledUnknownIDRejected(t *testing.T) {
	m, buf := testManager(t)
	fakeSurface(t, m, "demo", true, nil)
	before := indexBytes(t, m)

	m.dispatch(cmd(1, "surface_set_enabled", map[string]any{"id": "ghost", "enabled": false}))
	resp, pushed := surfaceLine(t, buf, 1)

	if resp["ok"] != false {
		t.Fatalf("ok = %v，want false（index 里没有 ghost）：%v", resp["ok"], resp)
	}
	// 断言的是**线协议字面量**而不是 Go 常量：常量改名了这条也该红（它是页面与主进程
	// 共同依赖的封闭集合，不是实现细节）。
	if resp["code"] != "unknown_id" {
		t.Fatalf("code = %v，want unknown_id", resp["code"])
	}
	data, _ := resp["data"].(map[string]any)
	if data["reason"] != "unknown_id" {
		t.Fatalf("data.reason = %v，want unknown_id（页面直接拿它当文案键）", data["reason"])
	}
	if got := indexBytes(t, m); string(got) != string(before) {
		t.Fatalf("未知 id 改了 index：\n got=%s\nwant=%s", got, before)
	}
	if pushed != 0 {
		t.Fatalf("失败也推了 surfaces_changed（%d 次）—— 一次没改的事件会让所有面板重挂", pushed)
	}
	// 拒绝同样留痕（「谁在什么 id 上失败过」是排查的第一条线索）。
	lines := auditLines(t, m)
	if len(lines) != 1 || lines[0]["ok"] != false || lines[0]["action"] != "disable" {
		t.Fatalf("audit = %v，want 一行 {action:disable, ok:false}", lines)
	}
}

// ---- ③ 形状不对 → bad_request（且不碰磁盘）----

func TestSurfaceSetEnabledBadRequest(t *testing.T) {
	m, buf := testManager(t)
	fakeSurface(t, m, "demo", true, nil)
	before := indexBytes(t, m)

	for _, payload := range []map[string]any{
		{"id": "demo"},                           // 少 enabled：缺省**不**当 false（会变成静默禁用）
		{"id": "demo", "enabled": "false"},       // 字符串不是布尔
		{"id": "../etc/passwd", "enabled": true}, // id 不是路径段之外的东西
		{"enabled": true},                        // 少 id
	} {
		m.dispatch(cmd(1, "surface_set_enabled", payload))
		resp, pushed := surfaceLine(t, buf, 1)
		if resp["ok"] != false || resp["code"] != "bad_request" {
			t.Fatalf("payload=%v 的回执 = %v，want ok:false code:bad_request", payload, resp)
		}
		if pushed != 0 {
			t.Fatalf("payload=%v 也推了 surfaces_changed", payload)
		}
	}
	if got := indexBytes(t, m); string(got) != string(before) {
		t.Fatalf("形状不对却改了 index：%s", got)
	}
}

// ---- ④ 卸载：走 Remove（进 .trash），不是删文件 ----

func TestSurfaceRemoveCommandMovesToTrash(t *testing.T) {
	m, buf := testManager(t)
	fakeSurface(t, m, "demo", true, nil)

	m.dispatch(cmd(1, "surface_remove", map[string]any{"id": "demo"}))
	resp, pushed := surfaceLine(t, buf, 1)
	if resp["ok"] != true {
		t.Fatalf("卸载回执 = %v，want ok:true", resp)
	}
	data, _ := resp["data"].(map[string]any)
	if data["status"] != surfaceApplied {
		t.Fatalf("status = %v，want applied", data["status"])
	}
	if _, err := os.Stat(filepath.Join(m.surfaces.Dir(), "demo")); !os.IsNotExist(err) {
		t.Fatalf("目录还在原处（应当挪进 .trash）: %v", err)
	}
	trash, err := filepath.Glob(filepath.Join(m.surfaces.Dir(), surfaceTrashDir, "demo-*"))
	if err != nil || len(trash) != 1 {
		t.Fatalf(".trash 里的目录 = %v（err %v），want 恰好 1 个（卸载 ≠ 直接删）", trash, err)
	}
	if _, err := os.Stat(filepath.Join(trash[0], "panel.js")); err != nil {
		t.Fatalf(".trash 里没有 panel.js（恢复就靠它）: %v", err)
	}
	if pushed != 1 {
		t.Fatalf("卸载推了 %d 次 surfaces_changed，want 1", pushed)
	}
	// 幂等：再删一次 = cleaned（index 已无条目、目录也不在），仍 ok（工具契约里就是这么写的）。
	m.dispatch(cmd(2, "surface_remove", map[string]any{"id": "demo"}))
	resp2, _ := surfaceLine(t, buf, 2)
	if resp2["ok"] != true {
		t.Fatalf("重复卸载 = %v，want ok:true（幂等）", resp2)
	}
	if d, _ := resp2["data"].(map[string]any); d["status"] != surfaceCleaned {
		t.Fatalf("重复卸载的 status = %v，want cleaned", d["status"])
	}
}

// ---- ⑤ ReadAudit：尾部 / 界 / 坏行跳过 ----

func TestSurfaceReadAuditTailAndLimits(t *testing.T) {
	m, buf := testManager(t)
	fakeSurface(t, m, "demo", true, nil)

	// 直接写一份「手搓」的审计文件：250 条好行 + 两行垃圾（断电写坏的半行 / 手工编辑）。
	// 用 store 反复写 250 行会让用例依赖 install 的次数，而这里要钉的只是**读侧**。
	auditPath := filepath.Join(m.surfaces.Dir(), surfaceAuditName)
	var sb strings.Builder
	for i := 0; i < 250; i++ {
		rec, _ := json.Marshal(map[string]any{
			"id": "demo", "action": "command", "cmd": "workspace_list", "ok": true,
			"ts": "2026-11-12T00:00:00Z",
		})
		sb.WriteString(string(rec) + "\n")
	}
	sb.WriteString("{\"id\":\"demo\",\"action\":\"comm\n") // 半行（非法 JSON）
	sb.WriteString("\n")                                   // 空行
	sb.WriteString("not json at all\n")
	// 末尾再来一条好的：它必须在「尾部 N 行」里出现（证明坏行不会截断窗口）。
	good, _ := json.Marshal(map[string]any{"id": "demo", "action": "enable", "ok": true, "ts": "2026-11-12T01:00:00Z"})
	sb.WriteString(string(good) + "\n")
	if err := os.WriteFile(auditPath, []byte(sb.String()), surfaceFileMode); err != nil {
		t.Fatalf("写 audit.jsonl: %v", err)
	}

	// 缺省 50：坏行跳过，好行照数；返回的是文件顺序（旧 → 新）。
	got, err := m.surfaces.ReadAudit(0)
	if err != nil {
		t.Fatalf("ReadAudit(0): %v", err)
	}
	if len(got) != surfaceAuditDefaultLimit {
		t.Fatalf("缺省行数 = %d，want %d", len(got), surfaceAuditDefaultLimit)
	}
	if got[len(got)-1].Action != "enable" {
		t.Fatalf("最后一行 = %+v，want 末尾那条 enable（尾部窗口要包含它）", got[len(got)-1])
	}

	// 上限 200：超了压到 200，而不是把整份文件吐出来。
	got, err = m.surfaces.ReadAudit(5000)
	if err != nil {
		t.Fatalf("ReadAudit(5000): %v", err)
	}
	if len(got) != surfaceAuditMaxLimit {
		t.Fatalf("上限行数 = %d，want %d", len(got), surfaceAuditMaxLimit)
	}

	// 小窗口：正好是尾部 3 条好行（两条垃圾行在这之前，不影响）。
	got, err = m.surfaces.ReadAudit(3)
	if err != nil {
		t.Fatalf("ReadAudit(3): %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ReadAudit(3) = %d 行，want 3：%+v", len(got), got)
	}
	if got[2].Action != "enable" || got[0].TS != "2026-11-12T00:00:00Z" {
		t.Fatalf("尾部 3 行 = %+v，want 最后一条是末尾的 enable", got)
	}

	// 命令层：同样的数据经 dispatch 出去，坏行不许把整段变成错误。
	m.dispatch(cmd(1, "surface_audit", map[string]any{"limit": 2}))
	resp, pushed := surfaceLine(t, buf, 1)
	if resp["ok"] != true {
		t.Fatalf("surface_audit = %v，want ok:true（坏行只跳过，不整段失败）", resp)
	}
	data, _ := resp["data"].(map[string]any)
	rows, _ := data["lines"].([]any)
	if len(rows) != 2 {
		t.Fatalf("limit=2 回了 %d 行，want 2：%v", len(rows), data)
	}
	if pushed != 0 {
		t.Fatalf("读审计推了 surfaces_changed（%d 次）", pushed)
	}

	// 空态：文件不存在 = 空列表 + nil error（一次都没装过的新用户走这条路）。
	if err := os.Remove(auditPath); err != nil {
		t.Fatalf("删 audit.jsonl: %v", err)
	}
	got, err = m.surfaces.ReadAudit(0)
	if err != nil || len(got) != 0 {
		t.Fatalf("空态 = (%v, %v)，want (空列表, nil)", got, err)
	}
}
