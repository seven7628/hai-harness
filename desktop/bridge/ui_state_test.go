// ui_state_test.go —— `ui` 通道的桥侧缓存（PR-12）钉子。
//
// 全部从**生产入口**进（00-conventions §4.7）：快照由 `m.dispatch(command{Type:"ui"})`
// 写入（渲染层真正走的那条），事件面用现成的 syncBuf/readLines 收集。
// 不另建一份「等价缓存」来测 —— 那样测的是测试自己的实现。
package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// uiSnapshotPayload 造一条最小合法快照（形状与渲染层 lib/uiSnapshot.ts 一致）。
func uiSnapshotPayload() map[string]any {
	return map[string]any{
		"layout":   map[string]any{"leftExpanded": true, "right": map[string]any{"open": true, "bias": "even"}},
		"surfaces": []any{map[string]any{"kind": "git", "group": "workflow", "order": 10, "disabled": false}},
		"tabs": map[string]any{
			"scope":    "S1",
			"activeId": "t1",
			"tabs": []any{
				map[string]any{"id": "t1", "kind": "git", "title": "Git", "active": true},
				map[string]any{"id": "t2", "kind": "file", "title": "main.go", "active": false},
			},
		},
		"editors":  map[string]any{"open": 1, "dirty": 0},
		"theme":    "dark",
		"viewport": map[string]any{"w": 1440, "h": 900},
		"at":       1731600000000,
	}
}

func dispatchUISnapshot(t *testing.T, m *manager, data map[string]any) {
	t.Helper()
	m.dispatch(cmd(1, "ui", map[string]any{"op": "snapshot", "data": data}))
}

// ---- 存 / 取（走 dispatch）----

func TestUIDispatchStoresSnapshotWithoutAnyOutput(t *testing.T) {
	m, buf := testManager(t)
	dispatchUISnapshot(t, m, uiSnapshotPayload())

	// 「不进事件」= stdout 一条都不要有：既没有 event_type 行，也没有 command_response
	// （ui 信封没有回执价值 —— 有回执就会在事件流里留下一条假的同步点）。
	if lines := readLines(buf); len(lines) != 0 {
		t.Fatalf("ui 消息产生了 %d 行输出，want 0：%v", len(lines), lines)
	}

	raw, age, ok := m.ui.Snapshot()
	if !ok {
		t.Fatal("快照没存进去（Snapshot ok=false）")
	}
	if age < 0 || age > 2*time.Second {
		t.Fatalf("age = %v，不像刚写入的", age)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("存的不是合法 JSON: %v", err)
	}
	tabs := got["tabs"].(map[string]any)
	if tabs["activeId"] != "t1" {
		t.Fatalf("tabs.activeId = %v，want t1（快照内容被改写了？）", tabs["activeId"])
	}
	if got["theme"] != "dark" {
		t.Fatalf("theme = %v，want dark", got["theme"])
	}
}

// ---- ageMs 增长 + stale 判定 ----

func TestUISnapshotAgeGrows(t *testing.T) {
	m, _ := testManager(t)
	dispatchUISnapshot(t, m, uiSnapshotPayload())

	_, a1, ok := m.ui.Snapshot()
	if !ok {
		t.Fatal("快照没存进去")
	}
	time.Sleep(15 * time.Millisecond)
	_, a2, ok := m.ui.Snapshot()
	if !ok {
		t.Fatal("第二次 Snapshot ok=false")
	}
	if a2 <= a1 {
		t.Fatalf("age 没增长：a1=%v a2=%v", a1, a2)
	}
}

func TestUIInspectMarksStaleAfterFiveSeconds(t *testing.T) {
	m, _ := testManager(t)
	dispatchUISnapshot(t, m, uiSnapshotPayload())

	// 直接回拨存储时刻（不 sleep 5s）：age 是「现在 - at」的函数，
	// 回拨与等待对被测代码完全等价，而等待会让这条用例变成门禁里最慢的一条。
	m.ui.mu.Lock()
	m.ui.at = time.Now().Add(-6 * time.Second)
	m.ui.mu.Unlock()

	out := callUIInspect(t, m, `{"what":"tabs"}`)
	if out["stale"] != true {
		t.Fatalf("ageMs = %v 时 stale = %v，want true", out["ageMs"], out["stale"])
	}
	if ageMs, _ := out["ageMs"].(float64); ageMs < 5000 {
		t.Fatalf("ageMs = %v，want ≥5000", ageMs)
	}
}

// ---- 大小上限 / 非法 JSON / data 非对象：整条拒绝且旧值仍在 ----

func TestUIStateRejectsOversizeAndKeepsOldSnapshot(t *testing.T) {
	m, _ := testManager(t)
	dispatchUISnapshot(t, m, uiSnapshotPayload())
	before, _, _ := m.ui.Snapshot()

	huge := `{"blob":"` + strings.Repeat("x", maxUISnapshotBytes) + `"}`
	if len(huge) <= maxUISnapshotBytes {
		t.Fatalf("夹具没超过上限：%d", len(huge))
	}
	if m.ui.put([]byte(huge)) {
		t.Fatal("超过 256 KiB 的快照被接受了")
	}
	after, _, ok := m.ui.Snapshot()
	if !ok || string(after) != string(before) {
		t.Fatal("拒绝之后旧快照没了/被改写了")
	}
}

func TestUIStateAcceptsSnapshotJustUnderLimit(t *testing.T) {
	m, _ := testManager(t)
	// 上限内必须收：否则「拒绝」这条判据可能只是「什么都拒绝」。
	body := `{"blob":"` + strings.Repeat("y", maxUISnapshotBytes-32) + `"}`
	raw := []byte(body)
	if len(raw) > maxUISnapshotBytes {
		t.Fatalf("夹具超限：%d", len(raw))
	}
	if !m.ui.put(raw) {
		t.Fatalf("上限内（%d 字节）的快照被拒了", len(raw))
	}
}

func TestUIStateRejectsInvalidJSONAndNonObject(t *testing.T) {
	m, _ := testManager(t)

	// 对照组先立：合法对象必须被接受 —— 否则后面的「都拒绝了」可能只是 put 恒 false。
	if !m.ui.put([]byte(`{"ok":true}`)) {
		t.Fatal("对照组（合法对象）被拒")
	}
	for _, bad := range []string{`{"a":`, `[1,2,3]`, `"just a string"`, ``, `   `} {
		if m.ui.put([]byte(bad)) {
			t.Fatalf("非法快照 %q 被接受了", bad)
		}
	}

	dispatchUISnapshot(t, m, uiSnapshotPayload())
	before, _, _ := m.ui.Snapshot()

	// data 不是对象：dispatch 层就该整条拒（数组/字符串都不能进缓存）。
	m.dispatch(cmd(2, "ui", map[string]any{"op": "snapshot", "data": []any{1, 2, 3}}))
	m.dispatch(cmd(3, "ui", map[string]any{"op": "snapshot", "data": "文件内容不该进快照"}))
	m.dispatch(cmd(4, "ui", map[string]any{"op": "snapshot"})) // 连 data 都没有

	after, _, _ := m.ui.Snapshot()
	if string(after) != string(before) {
		t.Fatal("data 非对象时被写进缓存了（应整条拒绝并保留旧值）")
	}
}

// ---- 未知 op：静默忽略（不报错、不写日志、不动缓存）----

func TestUIUnknownOpIsSilentlyIgnored(t *testing.T) {
	m, buf := testManager(t)
	dispatchUISnapshot(t, m, uiSnapshotPayload())
	before, _, _ := m.ui.Snapshot()

	m.dispatch(cmd(2, "ui", map[string]any{"op": "nope", "data": map[string]any{"x": 1}}))
	m.dispatch(cmd(3, "ui", map[string]any{"data": map[string]any{"x": 1}})) // 连 op 都没有

	if lines := readLines(buf); len(lines) != 0 {
		t.Fatalf("未知 op 产生了输出（应为静默）：%v", lines)
	}
	after, _, _ := m.ui.Snapshot()
	if string(after) != string(before) {
		t.Fatal("未知 op 改动了缓存")
	}
}

// ---- surface_status：单个面板状态（PR-13 的安装器读它）----

func TestUISurfaceStatusIsStoredWithoutEvents(t *testing.T) {
	m, buf := testManager(t)
	m.dispatch(cmd(1, "ui", map[string]any{
		"op": "surface_status",
		"data": map[string]any{
			"kind": "plugin:demo", "mounted": true, "disabled": false, "error": "",
		},
	}))

	if lines := readLines(buf); len(lines) != 0 {
		t.Fatalf("surface_status 产生了输出，want 0：%v", lines)
	}
	st, ok := m.ui.status("plugin:demo")
	if !ok || !st.Mounted || st.Disabled {
		t.Fatalf("面板状态 = %+v ok=%v，want mounted=true disabled=false", st, ok)
	}
}

// ---- bridge → 渲染层：surfaces_changed ----

func TestPushSurfacesChangedEmitsUIEnvelopeNotEvent(t *testing.T) {
	m, buf := testManager(t)
	m.PushSurfacesChanged()

	lines := readLines(buf)
	if len(lines) != 1 {
		t.Fatalf("输出 %d 行，want 1：%v", len(lines), lines)
	}
	line := lines[0]
	if line["type"] != "ui" || line["op"] != "surfaces_changed" {
		t.Fatalf("信封 = %v，want {type:ui, op:surfaces_changed}", line)
	}
	if _, isEvent := line["event_type"]; isEvent {
		t.Fatal("surfaces_changed 带了 event_type —— 它绝不能进 28 类事件流")
	}
	// 不进会话 transcript 的前提是「根本不进事件记录」：事件记录只会记 event_type 行。
	// 这条断言与上面等价但更直白：信封的键集只有三个。
	if len(line) != 3 {
		t.Fatalf("信封字段 = %v（%d 个），want 恰好 type/op/data", line, len(line))
	}
}

// ---- 不落盘：ui 消息不产生任何应用数据文件 ----

func TestUIDispatchWritesNothingToDisk(t *testing.T) {
	m, _ := testManager(t)
	dir := appDataDirOverride
	dispatchUISnapshot(t, m, uiSnapshotPayload())
	m.dispatch(cmd(2, "ui", map[string]any{"op": "surface_status", "data": map[string]any{"kind": "git"}}))

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读应用数据目录: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("ui 消息落盘了：%v", names)
	}
}
