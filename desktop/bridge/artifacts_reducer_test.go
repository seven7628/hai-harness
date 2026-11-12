package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/desktop/bridge/jsonwire"
	"github.com/seven7628/hai-harness/events"
)

// artifactsEvent 构造一个携带产出清单的 AgentEnd。
func artifactsEvent(runID string, trunc bool, arts ...core.Artifact) *events.AgentEnd {
	return &events.AgentEnd{
		RunId: runID, Content: "done", FinishReason: core.FinishReasonStop,
		Artifacts: arts, ArtifactsTruncated: trunc,
		Timestamp: time.Now(), EventType: events.AgentEndType,
	}
}

// artifactsOf 取快照中 artifacts 块（无则 nil）。
func artifactsOf(t *testing.T, r *ViewReducer) *Block {
	t.Helper()
	cp := r.Snapshot()
	for i := range cp.Blocks {
		if cp.Blocks[i].Kind == blockKindArtifacts {
			return &cp.Blocks[i]
		}
	}
	return nil
}

// TestReducerCarriesArtifacts AgentEnd.Artifacts 必须投影为独立块并进入 checkpoint：
// 这是「刷新/重启后产出卡片仍在」的唯一数据源（checkpoint 是渲染主路径）。
func TestReducerCarriesArtifacts(t *testing.T) {
	r := NewViewReducer()
	r.Apply(artifactsEvent("run-1", false,
		core.Artifact{Name: "a.html", Path: "out/a.html", Kind: core.ArtifactKindCreated, Added: 12, Size: 200, Lines: 12, SHA256: "h1"},
		core.Artifact{Name: "b.go", Path: "b.go", Kind: core.ArtifactKindModified, Added: 3, Removed: 2, Size: 80, Lines: 9, SHA256: "h2"},
	), EventMeta{})

	blk := artifactsOf(t, r)
	if blk == nil {
		t.Fatal("artifacts block must be projected into the view")
	}
	if len(blk.Artifacts) != 2 {
		t.Fatalf("want 2 artifact refs, got %d", len(blk.Artifacts))
	}
	if blk.RunID != "run-1" {
		t.Fatalf("artifacts block must carry run_id: %q", blk.RunID)
	}
	a := blk.Artifacts[0]
	if a.Name != "a.html" || a.Path != "out/a.html" || a.Kind != core.ArtifactKindCreated {
		t.Fatalf("artifact ref mismatch: %+v", a)
	}
	if a.Size != 200 || a.Lines != 12 || a.SHA256 != "h1" {
		t.Fatalf("artifact facts must survive projection: %+v", a)
	}
	if b := blk.Artifacts[1]; b.Added != 3 || b.Removed != 2 || b.Kind != core.ArtifactKindModified {
		t.Fatalf("modified ref mismatch: %+v", b)
	}
}

// TestReducerArtifactsJSONShape checkpoint 序列化形状：字段名稳定（宿主按 JSON 消费）。
func TestReducerArtifactsJSONShape(t *testing.T) {
	r := NewViewReducer()
	r.Apply(artifactsEvent("run-1", true,
		core.Artifact{Name: "x.csv", Path: "/abs/x.csv", Kind: core.ArtifactKindCreated, Size: 10, Lines: 2, SHA256: "hh"},
	), EventMeta{})

	raw, err := json.Marshal(r.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Blocks []struct {
			Kind               string `json:"kind"`
			ArtifactsTruncated bool   `json:"artifacts_truncated"`
			Artifacts          []struct {
				Name   string `json:"name"`
				Path   string `json:"path"`
				Kind   string `json:"kind"`
				Size   int64  `json:"size"`
				Lines  int64  `json:"lines"`
				SHA256 string `json:"sha256"`
			} `json:"artifacts"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, b := range parsed.Blocks {
		if b.Kind != "artifacts" {
			continue
		}
		found = true
		if !b.ArtifactsTruncated {
			t.Fatal("artifacts_truncated must serialize")
		}
		if len(b.Artifacts) != 1 || b.Artifacts[0].Name != "x.csv" || b.Artifacts[0].Path != "/abs/x.csv" {
			t.Fatalf("json artifact shape mismatch: %+v", b.Artifacts)
		}
		if b.Artifacts[0].SHA256 != "hh" || b.Artifacts[0].Size != 10 || b.Artifacts[0].Lines != 2 {
			t.Fatalf("json facts mismatch: %+v", b.Artifacts[0])
		}
	}
	if !found {
		t.Fatal(`checkpoint JSON must contain a block with "kind":"artifacts"`)
	}
}

// TestReducerArtifactsEmptyNoBlock 无产出 → 不产生空块（叙述区不被空卡片污染）。
func TestReducerArtifactsEmptyNoBlock(t *testing.T) {
	r := NewViewReducer()
	r.Apply(artifactsEvent("run-1", false), EventMeta{})
	if blk := artifactsOf(t, r); blk != nil {
		t.Fatalf("no artifacts → no block, got %+v", blk)
	}
}

// TestReducerArtifactsBeforeDividerOnAbort 中断时产出块必须在 divider **之前**：
// 产出属于本轮成果，横线分隔的是下一轮上下文（顺序错会让产出看起来属于下一轮）。
func TestReducerArtifactsBeforeDividerOnAbort(t *testing.T) {
	r := NewViewReducer()
	ev := artifactsEvent("run-1", false,
		core.Artifact{Name: "wip.txt", Path: "wip.txt", Kind: core.ArtifactKindCreated, Added: 4},
	)
	ev.FinishReason = core.FinishReasonAbort
	r.Apply(ev, EventMeta{})

	cp := r.Snapshot()
	ai, di := -1, -1
	for i := range cp.Blocks {
		switch cp.Blocks[i].Kind {
		case blockKindArtifacts:
			ai = i
		case blockKindDivider:
			di = i
		}
	}
	if ai < 0 || di < 0 {
		t.Fatalf("abort must produce both artifacts(%d) and divider(%d) blocks", ai, di)
	}
	if ai > di {
		t.Fatalf("artifacts block must come BEFORE the abort divider: artifacts@%d divider@%d", ai, di)
	}
}

// TestReducerArtifactsIdempotent 同 run 重复 AgentEnd（重连补发/重放）不得追加第二个块。
func TestReducerArtifactsIdempotent(t *testing.T) {
	r := NewViewReducer()
	ev := func() *events.AgentEnd {
		return artifactsEvent("run-1", false,
			core.Artifact{Name: "a.txt", Path: "a.txt", Kind: core.ArtifactKindCreated, Added: 1},
		)
	}
	r.Apply(ev(), EventMeta{})
	r.Apply(ev(), EventMeta{})

	count := 0
	for _, b := range r.Snapshot().Blocks {
		if b.Kind == blockKindArtifacts {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("duplicate AgentEnd must not append a second artifacts block: got %d", count)
	}
}

// TestReducerArtifactsCloneDeepCopy Snapshot 深拷贝：修改快照里的 Artifacts 不得影响
// 后续 Snapshot（值拷贝共享底层数组是经典陷阱）。
func TestReducerArtifactsCloneDeepCopy(t *testing.T) {
	r := NewViewReducer()
	r.Apply(artifactsEvent("run-1", false,
		core.Artifact{Name: "a.txt", Path: "a.txt", Kind: core.ArtifactKindCreated, Added: 1},
	), EventMeta{})

	cp1 := r.Snapshot()
	for i := range cp1.Blocks {
		if cp1.Blocks[i].Kind == blockKindArtifacts {
			cp1.Blocks[i].Artifacts[0].Name = "MUTATED"
		}
	}
	cp2 := r.Snapshot()
	for i := range cp2.Blocks {
		if cp2.Blocks[i].Kind == blockKindArtifacts {
			if cp2.Blocks[i].Artifacts[0].Name != "a.txt" {
				t.Fatalf("Snapshot must deep-copy Artifacts (mutation leaked): %q", cp2.Blocks[i].Artifacts[0].Name)
			}
			return
		}
	}
	t.Fatal("artifacts block missing in second snapshot")
}

// TestReducerArtifactsCheckpointRoundTrip checkpoint 往返（Restore → Snapshot）：
// 恢复后产出清单必须完整保留（刷新/重启的渲染依据）。
func TestReducerArtifactsCheckpointRoundTrip(t *testing.T) {
	r1 := NewViewReducer()
	r1.Apply(artifactsEvent("run-1", true,
		core.Artifact{Name: "out.xlsx", Path: "out/out.xlsx", Kind: core.ArtifactKindCreated, Size: 500, Lines: 3, SHA256: "zz", Added: 7},
	), EventMeta{})
	cp := r1.Snapshot()

	r2 := NewViewReducer()
	if err := r2.Restore(cp); err != nil {
		t.Fatalf("restore: %v", err)
	}
	blk := artifactsOf(t, r2)
	if blk == nil {
		t.Fatal("artifacts block lost across checkpoint round-trip")
	}
	if !blk.ArtifactsTruncated {
		t.Fatal("artifacts_truncated lost across round-trip")
	}
	if len(blk.Artifacts) != 1 {
		t.Fatalf("want 1 artifact after restore, got %d", len(blk.Artifacts))
	}
	a := blk.Artifacts[0]
	if a.Name != "out.xlsx" || a.Path != "out/out.xlsx" || a.Size != 500 || a.Lines != 3 || a.SHA256 != "zz" || a.Added != 7 {
		t.Fatalf("artifact facts lost across round-trip: %+v", a)
	}
}

// TestParseLegacyEventCarriesArtifacts 旧会话懒迁移路径：events.jsonl 里的
// agent_end 行（含 artifacts）经 parseLegacyEvent 解析后必须保留产出清单
// （否则老会话恢复后产出卡片全空）。
func TestParseLegacyEventCarriesArtifacts(t *testing.T) {
	line := `{"event_type":"agent_end","run_id":"run-1","content":"done","finish_reason":"stop",` +
		`"artifacts":[{"name":"a.html","path":"out/a.html","kind":"created","added":5,"size":120,"lines":5,"sha256":"hh"}],` +
		`"artifacts_truncated":true}`

	ev, err := parseLegacyEvent(line)
	if err != nil {
		t.Fatalf("parseLegacyEvent: %v", err)
	}
	end, ok := ev.(*events.AgentEnd)
	if !ok {
		t.Fatalf("want *events.AgentEnd, got %T", ev)
	}
	if len(end.Artifacts) != 1 {
		t.Fatalf("legacy replay must carry artifacts, got %d", len(end.Artifacts))
	}
	a := end.Artifacts[0]
	if a.Name != "a.html" || a.Path != "out/a.html" || a.Kind != "created" || a.Added != 5 || a.Size != 120 || a.Lines != 5 || a.SHA256 != "hh" {
		t.Fatalf("legacy artifact fields mismatch: %+v", a)
	}
	if !end.ArtifactsTruncated {
		t.Fatal("legacy artifacts_truncated must survive parse")
	}
}

// TestParseLegacyArtifactsFeedsReducer 端到端：legacy 行 → 解析 → reducer → 块。
func TestParseLegacyArtifactsFeedsReducer(t *testing.T) {
	line := `{"event_type":"agent_end","run_id":"run-9","content":"x","finish_reason":"stop",` +
		`"artifacts":[{"name":"r.txt","path":"r.txt","kind":"modified","added":2,"removed":1}]}`
	ev, err := parseLegacyEvent(line)
	if err != nil || ev == nil {
		t.Fatalf("parse: %v (%v)", ev, err)
	}
	r := NewViewReducer()
	r.Apply(ev, EventMeta{})
	blk := artifactsOf(t, r)
	if blk == nil || len(blk.Artifacts) != 1 {
		t.Fatalf("legacy replay must produce an artifacts block: %+v", blk)
	}
	if blk.Artifacts[0].Kind != core.ArtifactKindModified || blk.Artifacts[0].Added != 2 {
		t.Fatalf("legacy artifact mismatch: %+v", blk.Artifacts[0])
	}
}

// TestReducerSubAgentArtifactsIntoCard 子 agent 的产出归入其 agent 卡片
// （不产生独立块——避免主对话被每个子 agent 的产出卡片刷屏）。
func TestReducerSubAgentArtifactsIntoCard(t *testing.T) {
	r := NewViewReducer()
	now := time.Now()
	mustApply(t, r, evAgentStart(t, "child-1", "root-1", "重构支付", "task-1", now))
	end := artifactsEvent("child-1", false,
		core.Artifact{Name: "pay.go", Path: "internal/pay.go", Kind: core.ArtifactKindModified, Added: 8, Removed: 4, Size: 300, Lines: 20, SHA256: "pp"},
	)
	end.ParentRunId = "root-1"
	mustApply(t, r, end)

	cp := r.Snapshot()
	// 不得产生独立 artifacts 块（子 agent 产出归卡片）
	if blocks := blockByKind(t, cp, blockKindArtifacts); len(blocks) != 0 {
		t.Fatalf("sub-agent output must NOT create a standalone artifacts block: %+v", blocks)
	}
	// 必须挂到 agent 卡片上
	agents := blockByKind(t, cp, blockKindAgent)
	if len(agents) != 1 {
		t.Fatalf("want 1 agent card, got %d", len(agents))
	}
	if len(agents[0].Artifacts) != 1 {
		t.Fatalf("agent card must carry artifacts: %+v", agents[0].Artifacts)
	}
	a := agents[0].Artifacts[0]
	if a.Name != "pay.go" || a.Kind != core.ArtifactKindModified || a.Added != 8 || a.Removed != 4 || a.Size != 300 || a.Lines != 20 || a.SHA256 != "pp" {
		t.Fatalf("sub-agent artifact facts mismatch: %+v", a)
	}
}

// TestReducerMainRunStillUsesStandaloneBlock 主 run 仍走独立块（与子 agent 区分）。
func TestReducerMainRunStillUsesStandaloneBlock(t *testing.T) {
	r := NewViewReducer()
	mustApply(t, r, artifactsEvent("root-1", false,
		core.Artifact{Name: "main.txt", Path: "main.txt", Kind: core.ArtifactKindCreated, Added: 1},
	))
	cp := r.Snapshot()
	if blocks := blockByKind(t, cp, blockKindArtifacts); len(blocks) != 1 {
		t.Fatalf("main run must produce a standalone artifacts block, got %d", len(blocks))
	}
}

func TestAgentEndArtifactsEmitPathJSON(t *testing.T) {
	ev := &events.AgentEnd{
		Index: 3, Content: "Flappy Bird 已生成！", FinishReason: core.FinishReasonStop,
		RunId: "run-1", Model: "m", Timestamp: time.Now(),
		Artifacts: []core.Artifact{
			{Name: "flappy-bird.html", Path: "flappy-bird.html", Kind: core.ArtifactKindCreated, Added: 120, Size: 15249, Lines: 412, SHA256: "abc"},
			{Name: "r.md", Path: "docs/r.md", Kind: core.ArtifactKindModified, Added: 5, Removed: 2, Size: 900, Lines: 40, SHA256: "def"},
		},
		EventType: events.AgentEndType,
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	// 复刻 emit 的注入：session_id/workspace 前插
	injected, err := jsonwire.InjectKeys(b, []jsonwire.KV{
		{Key: "session_id", Value: []byte(`"s-1"`)},
		{Key: "workspace", Value: []byte(`"/ws"`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 必须是单行合法 JSON（前端逐行 parse）
	if bytes.Contains(injected, []byte("\n")) {
		t.Fatal("event line must not contain raw newline")
	}
	var back map[string]any
	if err := json.Unmarshal(injected, &back); err != nil {
		t.Fatalf("injected line must be valid JSON: %v", err)
	}
	if back["session_id"] != "s-1" || back["event_type"] != "agent_end" {
		t.Fatalf("routing keys missing: %+v", back)
	}
	arts, ok := back["artifacts"].([]any)
	if !ok || len(arts) != 2 {
		t.Fatalf("artifacts must survive emit path: %+v", back["artifacts"])
	}
	first := arts[0].(map[string]any)
	if first["name"] != "flappy-bird.html" || first["kind"] != "created" {
		t.Fatalf("artifact fields mismatch: %+v", first)
	}
	if _, present := back["artifacts_truncated"]; present {
		t.Fatal("artifacts_truncated must be omitted when false")
	}
	t.Logf("✅ emit 路径输出合法单行 JSON，artifacts 完整：%d 项", len(arts))
}
