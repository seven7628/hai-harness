package main

// owner 隔离测试共用的命令/文件小工具（hooks 与 IM 两侧都用）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// runCmd 走真实 dispatch 入口跑一条命令，返回 (ok, error, data)。
// 直接调用 m.dispatch 而不是 handleHooksTrust：测的是「命令面」的归属派生与拒绝。
func runCmd(t *testing.T, m *manager, buf *syncBuf, typ Command, payload map[string]any) (bool, string, map[string]any) {
	t.Helper()
	buf.Take()
	m.dispatch(command{Id: 1, Type: typ, Payload: payload})
	for _, line := range readLines(buf) {
		if line["event_type"] != "command_response" {
			continue
		}
		ok, _ := line["ok"].(bool)
		errMsg, _ := line["error"].(string)
		data, _ := line["data"].(map[string]any)
		return ok, errMsg, data
	}
	t.Fatalf("命令 %s 没有响应行", typ)
	return false, "", nil
}

// hooksTrustCmd 走真实命令入口（hooks_trust），断言服务端归属派生/拒绝。
func hooksTrustCmd(t *testing.T, m *manager, buf *syncBuf, payload map[string]any) (bool, string) {
	t.Helper()
	ok, errMsg, _ := runCmd(t, m, buf, CmdHooksTrust, payload)
	return ok, errMsg
}

// writeFileJSON 写 JSON 文件（hooks 来源 fixture）。
func writeFileJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
