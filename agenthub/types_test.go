package agenthub

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"io/fs"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testTime() time.Time {
	return time.Date(2026, 11, 4, 10, 6, 0, 0, time.UTC)
}

func testTimePtr(t time.Time) *time.Time { return &t }

// assertJSONStable 序列化 → 反序列化回同一类型 → 再序列化，逐字节比较。
// 这样既验证字段可往返，也验证 JSON tag/omitempty 不漂移。
func assertJSONStable(t *testing.T, v any) {
	t.Helper()

	first, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	target := reflect.New(reflect.TypeOf(v).Elem())
	if err := json.Unmarshal(first, target.Interface()); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	second, err := json.Marshal(target.Interface())
	if err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("JSON round-trip changed bytes:\nfirst:  %s\nsecond: %s", first, second)
	}
}

func fullProfile() *AgentProfile {
	return &AgentProfile{
		ID:                "ag_7f3k2m9q",
		Name:              "Researcher",
		Avatar:            Avatar{Kind: "emoji", Value: "🟢", BG: "#2f6f5e"},
		Accent:            "#2f6f5e",
		Role:              "竞品研究员",
		Description:       "负责收集竞品价格、功能与公开情报。",
		Instructions:      "只使用公开来源；结论附链接。",
		Persona:           "先结论后证据。",
		Provider:          "deepseek",
		Model:             "deepseek-v4-flash",
		Effort:            "low",
		CompressThreshold: 0.8,
		Memory:            MemoryPrefs{Enabled: true, MaxIndexLines: 200},
		Tools: ToolPolicy{
			Preset:         ToolPresetCustom,
			Disable:        []string{"browser_*"},
			Allow:          []string{"bash", "run_python"},
			BashRoots:      []string{"/Users/me/shared"},
			DisableNetwork: true,
			MCP: MCPPolicy{
				DisableServers: []string{"github"},
				DisableTools:   []string{"linear.delete_*"},
				AllowTools:     []string{"linear.search"},
			},
		},
		Policy: RunPolicy{
			CanCreateWorkspace: true,
			CanDispatch:        true,
			CanHandoff:         true,
			CanShareArtifact:   true,
			WorkspaceRoots:     []string{"~/hai-workspaces"},
			MaxTaskCostUSD:     2,
			DailyBudgetUSD:     20,
			MaxAutoTurns:       6,
		},
		Version:   3,
		CreatedAt: testTime(),
		UpdatedAt: testTime().Add(time.Minute),
	}
}

func TestAgentProfileJSONStable(t *testing.T) {
	assertJSONStable(t, fullProfile())
}

func TestGroupJSONStable(t *testing.T) {
	g := &Group{
		ID:           "grp_9m2x",
		Name:         "竞品研究",
		OwnerAgentID: "ag_7f3k2m9q",
		Members: []Member{
			{AgentID: "ag_7f3k2m9q", Role: RoleOwner, JoinedAt: testTime()},
			{AgentID: "ag_2p8v", Role: RoleObserver, JoinedAt: testTime()},
		},
		Policy: GroupPolicy{
			Mode:             GroupModeAsk,
			MaxParallelRuns:  3,
			MaxHandoffDepth:  3,
			MaxTurnsPerHuman: 8,
			MaxMentions:      3,
			DailyBudgetUSD:   20,
		},
		Pinned:    true,
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}
	assertJSONStable(t, g)
}

func TestMessageJSONStable(t *testing.T) {
	m := &Message{
		Seq:     42,
		ID:      "msg_42",
		GroupID: "grp_9m2x",
		RoundID: "rnd_17",
		Context: &TurnContext{
			TriggerSeqs: []int64{40, 41},
			Senders: []AgentRef{
				{
					AgentID: "ag_2p8v",
					Name:    "Account Manager",
					Avatar:  Avatar{Kind: "emoji", Value: "🟣", BG: "#8b5cf6"},
					Accent:  "#8b5cf6",
				},
			},
			Reason: "multi_agent",
		},
		Sender:     Sender{Kind: SenderAgent, AgentID: "ag_7f3k2m9q"},
		Kind:       KindText,
		Text:       "三家价格表出来了。",
		Mentions:   []AgentID{"ag_5d1c"},
		MentionAll: true,
		ReplyToSeq: 41,
		Intent:     IntentReport,
		TaskID:     "task_31",
		Turn:       2,
		Visibility: VisibilityGroup,
		Refs:       map[string]any{"artifact_ids": []string{"art_88a"}},
		CreatedAt:  testTime(),
	}
	assertJSONStable(t, m)

	// 关键字段名是协议的一部分，改名必须让这里失败。
	raw, _ := json.Marshal(m)
	for _, key := range []string{`"seq"`, `"group_id"`, `"round_id"`, `"context"`, `"trigger_seqs"`, `"reply_to_seq"`, `"mention_all"`} {
		if !bytes.Contains(raw, []byte(key)) {
			t.Fatalf("message JSON missing %s: %s", key, raw)
		}
	}
}

func TestTaskJSONStable(t *testing.T) {
	task := &Task{
		ID:           "task_31",
		RootTaskID:   "task_31",
		ParentTaskID: "task_30",
		Chain:        []TaskID{"task_30", "task_31"},
		Origin:       TaskOrigin{Kind: OriginGroup, GroupID: "grp_9m2x", AgentID: "ag_7f3k2m9q", AgentName: "Researcher", MessageSeq: 42},
		Target:       TaskTarget{Kind: TargetWorkspace, Workspace: "/Users/me/hai-workspaces/pricing", SessionID: "8f2c1a"},
		Title:        "把三家价格表写成 2 页简报",
		Instruction:  "……",
		Callback:     CallbackAgentEnd,
		Status:       TaskDelivered,
		Attempt:      1,
		Dispatch: Dispatch{
			DispatchedAt: testTimePtr(testTime()),
			AcceptedAt:   testTimePtr(testTime().Add(time.Second)),
			RunID:        "run_abc",
			AgentStartAt: testTimePtr(testTime().Add(2 * time.Second)),
			AgentEndAt:   testTimePtr(testTime().Add(2 * time.Minute)),
		},
		Result: &TaskResult{
			FinishReason: "stop",
			Summary:      "2 页简报已产出",
			Artifacts:    []TaskArtifact{{Name: "pricing-v3.md", Path: "out/pricing-v3.md", Kind: "created", Size: 2140, Lines: 88, SHA256: "abc123"}},
			Usage:        Usage{Input: 12000, Output: 3000, CacheRead: 8000, CostPriced: true, CostUSD: 0.03},
		},
		NotifiedRuns:    []string{"run_abc"},
		HumanIntervened: false,
		CreatedAt:       testTime(),
		UpdatedAt:       testTime().Add(2 * time.Minute),
	}
	assertJSONStable(t, task)

	raw, _ := json.Marshal(task)
	for _, key := range []string{`"root_task_id"`, `"parent_task_id"`, `"notified_run_ids"`, `"human_intervened"`, `"cost_priced"`, `"cost_usd"`, `"sha256"`, `"finish_reason"`} {
		if !bytes.Contains(raw, []byte(key)) {
			t.Fatalf("task JSON missing %s: %s", key, raw)
		}
	}
}

// TestNoDesktopImports 锁住领域层边界：agenthub 不能被 bridge/desktop/agents/session 反向依赖。
// core 是允许的（P0-08 会用到 core.Message），所以不做“仅 stdlib”的过强断言。
func TestNoDesktopImports(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return strings.HasSuffix(fi.Name(), ".go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	files := 0
	for _, pkg := range pkgs {
		files += len(pkg.Files)
		for name, file := range pkg.Files {
			for _, imp := range file.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatalf("%s: bad import %q: %v", name, imp.Path.Value, err)
				}
				if forbiddenImport(path) {
					t.Errorf("%s imports %q; agenthub must stay independent of desktop/bridge/agents/session", name, path)
				}
			}
		}
	}
	if files == 0 {
		t.Fatal("parsed no Go files; boundary check would pass vacuously")
	}
}

func forbiddenImport(path string) bool {
	for _, seg := range []string{"desktop", "bridge", "agents", "session"} {
		if path == seg || strings.HasSuffix(path, "/"+seg) || strings.Contains(path, "/"+seg+"/") {
			return true
		}
	}
	return false
}

func TestDispatchOmitsUnsetTimes(t *testing.T) {
	raw, err := json.Marshal(Dispatch{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"dispatched_at"`, `"accepted_at"`, `"agent_start_at"`, `"agent_end_at"`} {
		if bytes.Contains(raw, []byte(key)) {
			t.Fatalf("empty Dispatch leaked %s: %s", key, raw)
		}
	}
	if string(raw) != `{}` {
		t.Fatalf("empty Dispatch = %s, want {}", raw)
	}
}

func TestScopeSessionID(t *testing.T) {
	if got := (Scope{Kind: ScopeDM}).SessionID("ag_1"); got != "hub.ag_1.dm" {
		t.Fatalf("DM session id = %q", got)
	}
	if got := (Scope{Kind: ScopeGroup, GroupID: "grp_1"}).SessionID("ag_1"); got != "hub.ag_1.g.grp_1" {
		t.Fatalf("group session id = %q", got)
	}
	if got := (Scope{Kind: ScopeGroup}).SessionID("ag_1"); got != "" {
		t.Fatalf("empty group must return empty id, got %q", got)
	}
	if got := (Scope{}).SessionID("ag_1"); got != "" {
		t.Fatalf("empty scope must return empty id, got %q", got)
	}
}

func TestGroupPolicyEffectiveMode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", GroupModeAsk},
		{GroupModeAuto, GroupModeAuto},
		{GroupModeAsk, GroupModeAsk},
		{GroupModeReadonly, GroupModeReadonly},
		{"bogus", GroupModeAsk},
	}
	for _, c := range cases {
		if got := (GroupPolicy{Mode: c.in}).EffectiveMode(); got != c.want {
			t.Errorf("EffectiveMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMinimalDMMessageJSONShape(t *testing.T) {
	m := &Message{
		Seq:        1,
		ID:         MessageID(1),
		Sender:     Sender{Kind: SenderHuman},
		Kind:       KindText,
		Text:       "hello",
		Visibility: VisibilityGroup,
		CreatedAt:  testTime(),
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"group_id"`, `"round_id"`, `"context"`, `"mentions"`, `"mention_all"`, `"reply_to_seq"`, `"intent"`, `"task_id"`, `"turn"`, `"refs"`} {
		if bytes.Contains(raw, []byte(key)) {
			t.Fatalf("minimal DM message leaked %s: %s", key, raw)
		}
	}
	for _, key := range []string{`"seq"`, `"id"`, `"sender"`, `"kind"`, `"text"`, `"visibility"`, `"created_at"`} {
		if !bytes.Contains(raw, []byte(key)) {
			t.Fatalf("minimal DM message missing %s: %s", key, raw)
		}
	}
}

func TestToolPolicyZeroValueIsAllOn(t *testing.T) {
	raw, err := json.Marshal(ToolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"mcp":{}}` {
		t.Fatalf("ToolPolicy{} = %s, want {\"mcp\":{}}", raw)
	}
}
