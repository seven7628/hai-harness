package main

// P2.5-05 复审 B2：hub_alert 单一出口的 envelope 完整性。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHubAlertEnvelopeUnified(t *testing.T) {
	m, buf := testManager(t)
	m.emitHubAlert(hubAlertEvent{Code: "blocked_budget", Message: "预算到顶"})
	m.emitHubAlert(hubAlertEvent{
		Code: "settle_failed", TaskID: "task_alert", AgentID: "ag_alert",
		GroupID: "grp_alert", Message: "结算失败",
	})
	m.emitHubAlert(hubAlertEvent{Code: "some_new_code", Message: "未知告警"})
	lines := hubEvents(t, buf, "hub_alert")
	if len(lines) != 3 {
		t.Fatalf("hub_alert lines = %d, want 3", len(lines))
	}
	required := []string{"event_type", "scope", "code", "kind", "severity", "message", "at"}
	for i, ev := range lines {
		for _, k := range required {
			if _, ok := ev[k]; !ok {
				t.Fatalf("alert %d missing %q: %+v", i, k, ev)
			}
		}
		if ev["code"] != ev["kind"] {
			t.Fatalf("alert %d code/kind diverged: %+v", i, ev)
		}
	}
	if lines[0]["scope"] != "hub" || lines[0]["severity"] != "error" {
		t.Fatalf("global blocked_budget = %+v; want hub/error", lines[0])
	}
	if _, ok := lines[0]["task_id"]; ok {
		t.Fatalf("global alert must omit task_id, got %+v", lines[0])
	}
	if lines[1]["scope"] != "task" || lines[1]["task_id"] != "task_alert" ||
		lines[1]["agent_id"] != "ag_alert" || lines[1]["group_id"] != "grp_alert" {
		t.Fatalf("task alert envelope = %+v", lines[1])
	}
	if lines[2]["severity"] != "warning" {
		t.Fatalf("unknown code severity = %v, want warning", lines[2]["severity"])
	}
}

func TestHubAlertSeverityMapping(t *testing.T) {
	cases := map[string]string{
		"recovery_failed": "error", "blocked_budget": "error", "settle_failed": "error",
		"batch_conflict": "error", "bad_header": "error",
		"usage_unpriced": "warning", "recovered_interrupted": "warning",
		"hub_background_timeout": "warning", "never_seen": "warning",
	}
	for code, want := range cases {
		if got := hubAlertSeverity(code); got != want {
			t.Errorf("severity(%s) = %s, want %s", code, got, want)
		}
	}
}

// P2.5-05 B2 静态守卫：hub_alert 字面量只允许出现在 emitHubAlert 一个出口。
// 否则新增生产点很容易裸写 writeLine，丢掉 scope/severity（变异 M2 存活）。
func TestHubAlertLiteralHasSingleWriter(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"event_type": "hub_alert"`) && name != "hub_task_finish.go" {
			t.Fatalf("hub_alert envelope written outside emitHubAlert: %s", name)
		}
	}
}
