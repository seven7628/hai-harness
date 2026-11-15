package main

// surface_agent_real_test.go —— 真 Provider 的 surface_agent 端到端冒烟（门控）。
//
// 默认 **skip**：没有真实凭据/网络时不让 `go test ./desktop/bridge` 变慢或红。
// 用法（provider/model 默认取真实 settings 的 opencode2 / deepseek-v4.1-flash）：
// 先把 key 用 `read -rs HAI_E2E_SURFACE_API_KEY` 静默放进环境变量（不要写进命令行、
// 文档或 history），再设置 HAI_E2E_SURFACE_REAL=1 / PROVIDER / MODEL 并运行：
//
//	go test ./desktop/bridge -run TestSurfaceAgentRealProviderSmoke -count=1 -v
//
// 这条测试证明的是「插件的 agent.run → 真 Provider 鉴权/HTTP/SSE → 事件与 jsonl」
// 这一段真的通；工具轮/审批/门禁仍由不依赖网络的 verify_agent_run_electron.mjs 钉住。
// key 只在内存里（hubRealInstallSettings 不写盘），报告/日志不打印 key。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSurfaceAgentRealProviderSmoke(t *testing.T) {
	if os.Getenv("HAI_E2E_SURFACE_REAL") != "1" {
		t.Skip("真 Provider 冒烟默认跳过：设 HAI_E2E_SURFACE_REAL=1 并提供 provider/model/key")
	}
	if key := strings.TrimSpace(os.Getenv("HAI_E2E_SURFACE_API_KEY")); key != "" {
		// hubRealResolveProvider 读这个变量；只在测试进程内覆盖，不写任何文件。
		t.Setenv("HAI_EVAL_HUB_API_KEY", key)
	}
	providerName := hubRealEnvOr("HAI_E2E_SURFACE_PROVIDER",
		hubRealEnvOr("HAI_EVAL_HUB_PROVIDER", hubRealDefaultProvider))
	model := hubRealEnvOr("HAI_E2E_SURFACE_MODEL",
		hubRealEnvOr("HAI_EVAL_HUB_MODEL", hubRealDefaultModel))
	cfg, skipReason := hubRealResolveProvider(t, providerName, model)
	if skipReason != "" {
		t.Skip(skipReason)
	}
	isolateUserPrices(t)

	m, buf := testManager(t)
	hubRealInstallSettings(t, cfg)
	m.provCfg.loadSettings()
	m.provCfg.setKey(providerName, cfg.key)
	if err := m.provCfg.setActive(providerName); err != nil {
		t.Fatalf("设 active provider 失败: %v", err)
	}
	m.mu.Lock()
	m.lastModel = model
	m.mu.Unlock()
	fakeSurface(t, m, "demo", true, agentCommands)

	resp := pluginResp(t, m, buf, 1, "surface_agent",
		agentPayload("demo", surfaceAgentSessionName, nil))
	sid, _ := pluginRespData(t, resp)["session_id"].(string)
	if sid == "" {
		t.Fatalf("agent.session 没回 session_id: %v", resp)
	}

	started := time.Now()
	resp = pluginResp(t, m, buf, 2, "surface_agent", agentPayload("demo", surfaceAgentRunName,
		map[string]any{"input": "Reply with exactly: pong"}))
	data := pluginRespData(t, resp)
	runID, _ := data["run_id"].(string)
	if runID == "" {
		t.Fatalf("agent.run 没回 run_id: %v", resp)
	}
	evs := waitSurfaceTerminal(t, buf, runID, 90*time.Second)
	var doneText string
	var sawUsage bool
	for _, item := range evs {
		switch surfaceEventType(item) {
		case surfaceAgentEvUsage:
			sawUsage = true
		case surfaceAgentEvDone:
			doneText, _ = surfaceEventData(item)["text"].(string)
		case surfaceAgentEvError:
			t.Fatalf("真 Provider 返回 error 终态: %v", surfaceEventData(item))
		}
	}
	if strings.TrimSpace(doneText) == "" {
		t.Fatalf("done 终态没有文本（真实模型可能拒答/端点错误）：%v", evs)
	}
	if !sawUsage {
		t.Fatalf("终态前没有 agent.usage（真实 usage 字段没走通）：%v", evs)
	}
	jsonl := filepath.Join(pluginSessionsDir("demo"), sid+".jsonl")
	if st, err := os.Stat(jsonl); err != nil || st.Size() == 0 {
		t.Fatalf("真 Provider run 后 jsonl 不存在/为空: %s (%v)", jsonl, err)
	}
	t.Logf("真 Provider smoke 通过：provider=%s model=%s run=%s elapsed=%s text=%q",
		providerName, model, runID, time.Since(started).Round(time.Millisecond), doneText)
}
