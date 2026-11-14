package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"github.com/seven7628/hai-harness/desktop/bridge/jsonwire"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// maxEventLineBytes 事件日志单行读取上限（Scanner buffer）。
// 与 checkpoint 懒迁移路径同口径（必须一致）：两条路径读同一份 events.jsonl，
// 上限不一致会导致同一会话在「有 record」与「无 record」时恢复出不同结果
// （超限行被一条路径静默丢弃）。
// 取值需覆盖最大单行事件：含图片的 user_inputs_consumed / tool response
// （read_file 读图 / 截图）—— 图片 data URL 单张内嵌上限 3MiB（tools 引擎闸门），
// 一条消息多张图 + 转义开销 → 16MiB 留足余量。
const maxEventLineBytes = 16 * 1024 * 1024

// buildSnapshot 冷启动恢复：把事件日志聚合为单行 command_response 内的紧凑快照
// （替代逐行重放，修复大量事件行造成的 stdout 洪峰/响应延迟与前端逐行 dispatch 的 O(N×M) 拷贝）。
// 事件保持文件顺序的单个有序数组；每元素保留 event_type 与全部字段
// （仅去掉 session_id 省空间——前端已知 sid）。无日志/无有效事件 → (nil, false)。
//
// 性能：避免每行 Unmarshal→Marshal 两次序列化——直接在原始行字节上删除
// `"session_id":"..."` 字段（字符串扫描），仅坏行才回退 Unmarshal 校验。
func (m *manager) buildSnapshot(wsPath, id string) (map[string]any, bool) {
	return buildSnapshotFromPath(filepath.Join(m.eventsDirFor(wsPath), id+".jsonl"))
}

// eventsDirFor 事件日志目录：插件虚拟 home 走它自己的 events 目录（plugin-sessions/<id>/events），
// 其它 workspace 走既有的 wsEventsDir。插件已卸载（没有运行态）时按 pluginID 直接推目录 ——
// 「卸载后会话仍可读」正是靠这条回退成立的（09 §4.2）。
func (m *manager) eventsDirFor(wsPath string) string {
	if isPluginHomePath(wsPath) {
		if ws := m.pluginRuntimeByPath(wsPath); ws != nil {
			return ws.eventsDir
		}
		if _, ed, ok := pluginSessionDirsForHome(wsPath); ok {
			return ed
		}
	}
	return wsEventsDir(wsPath)
}

// buildSnapshotFromPath 从任意事件日志文件聚合 snapshot（buildSnapshot 的底层实现；
// cron 独立历史目录复用同一管线，保证详情页与主对话页恢复同构）。
func buildSnapshotFromPath(evPath string) (map[string]any, bool) {
	f, err := os.Open(evPath)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxEventLineBytes)
	var events []json.RawMessage
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		raw := []byte(line)
		// 快速路径：原始字节删 session_id（省 Unmarshal/Marshal）
		cleaned := jsonwire.RemoveField(raw, "session_id")
		if cleaned != nil && !bytes.Contains(cleaned, []byte(`"event_type"`)) {
			// 非事件行（缺 event_type）→ 跳过（与旧逻辑一致）
			continue
		}
		if cleaned == nil {
			// 慢路径：坏行/异常结构 → 校验 event_type，能恢复则保留
			var rec map[string]json.RawMessage
			if json.Unmarshal(raw, &rec) != nil || string(rec["event_type"]) == "" {
				continue
			}
			delete(rec, "session_id")
			cleaned, _ = json.Marshal(rec)
			if cleaned == nil {
				continue
			}
		}
		events = append(events, cleaned)
	}
	// 扫描错误（超长行 / IO 失败）：已读前缀不完整 —— 不能当作「会话就这一段」返回，
	// 否则前端会把截断后的历史当成全部（图片/工具结果整段消失且无提示）。
	// 与 migrateLegacyToCheckpoint 的 sc.Err() 检查同语义（两条路径同口径）。
	if err := sc.Err(); err != nil {
		slog.Warn("event snapshot truncated: scanner stopped early",
			"path", evPath, "events", len(events), "max_line_bytes", maxEventLineBytes, "error", err)
		return nil, false
	}
	if len(events) == 0 {
		return nil, false
	}
	return map[string]any{"events": events}, true
}
