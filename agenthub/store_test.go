package agenthub

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testAgent() *AgentProfile {
	p := fullProfile()
	p.ID = "ag_test"
	p.CreatedAt = testTime()
	p.UpdatedAt = testTime().Add(time.Minute)
	return p
}

func testGroup() *Group {
	return &Group{
		ID:           "grp_test",
		Name:         "测试群",
		OwnerAgentID: "ag_test",
		Members:      []Member{{AgentID: "ag_test", Role: RoleOwner, JoinedAt: testTime()}},
		Policy:       GroupPolicy{Mode: GroupModeAsk, MaxParallelRuns: 3},
		Pinned:       true,
		CreatedAt:    testTime(),
		UpdatedAt:    testTime(),
	}
}

func testStoreMessage(text string) *Message {
	return &Message{
		ID:         "",
		GroupID:    "grp_test",
		Sender:     Sender{Kind: SenderHuman},
		Kind:       KindText,
		Text:       text,
		Visibility: VisibilityGroup,
		CreatedAt:  testTime(),
	}
}

func testTask() *Task {
	return &Task{
		ID:           "task_test",
		RootTaskID:   "task_test",
		Chain:        []TaskID{"task_test"},
		Origin:       TaskOrigin{Kind: OriginGroup, GroupID: "grp_test", AgentID: "ag_test"},
		Target:       TaskTarget{Kind: TargetAgent, AgentID: "ag_test", GroupID: "grp_test"},
		Title:        "测试任务",
		Status:       TaskCompleted,
		CreatedAt:    testTime(),
		UpdatedAt:    testTime().Add(time.Minute),
		NotifiedRuns: []string{"run_1"},
	}
}

func testNeed() *Need {
	return &Need{
		ID:        "need_test",
		Kind:      "approval",
		Status:    "pending",
		AgentID:   "ag_test",
		GroupID:   "grp_test",
		Title:     "需要审批",
		CreatedAt: testTime(),
	}
}

func testArtifact() *Artifact {
	return &Artifact{
		ID:           "art_test",
		Name:         "pricing.md",
		Path:         "out/pricing.md",
		Kind:         "created",
		OwnerAgentID: "ag_test",
		GroupID:      "grp_test",
		Visibility:   VisibilityGroup,
		CreatedAt:    testTime(),
	}
}

func TestStoreContract(t *testing.T) {
	stores := []struct {
		name string
		new  func(t *testing.T) Store
	}{
		{name: "file", new: func(t *testing.T) Store {
			fs, err := OpenFileStore(t.TempDir())
			if err != nil {
				t.Fatalf("open file store: %v", err)
			}
			return fs
		}},
		{name: "memory", new: func(t *testing.T) Store { return NewMemoryStore() }},
	}
	for _, tc := range stores {
		t.Run(tc.name, func(t *testing.T) {
			testStoreContract(t, tc.new(t))
		})
	}
}

func testStoreContract(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()

	// Agent
	agent := testAgent()
	if err := s.SaveAgent(ctx, agent); err != nil {
		t.Fatalf("SaveAgent: %v", err)
	}
	gotAgent, err := s.LoadAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("LoadAgent: %v", err)
	}
	if !reflect.DeepEqual(gotAgent, agent) {
		t.Fatalf("agent round-trip mismatch:\n got: %#v\nwant: %#v", gotAgent, agent)
	}
	agents, err := s.ListAgents(ctx)
	if err != nil || len(agents) != 1 || agents[0].ID != agent.ID {
		t.Fatalf("ListAgents = %v, %v", agents, err)
	}

	// Group
	group := testGroup()
	if err := s.SaveGroup(ctx, group); err != nil {
		t.Fatalf("SaveGroup: %v", err)
	}
	gotGroup, err := s.LoadGroup(ctx, group.ID)
	if err != nil || !reflect.DeepEqual(gotGroup, group) {
		t.Fatalf("group round-trip: %#v, %v", gotGroup, err)
	}

	// Messages / seq / pagination / search
	first := testStoreMessage("alpha")
	seq1, err := s.AppendMessage(ctx, first)
	if err != nil || seq1 != 1 || first.ID != "msg_1" {
		t.Fatalf("AppendMessage#1 = %d, %q, %v", seq1, first.ID, err)
	}
	second := testStoreMessage("beta")
	second.CreatedAt = testTime().Add(time.Second)
	seq2, err := s.AppendMessage(ctx, second)
	if err != nil || seq2 != 2 || second.ID != "msg_2" {
		t.Fatalf("AppendMessage#2 = %d, %q, %v", seq2, second.ID, err)
	}
	third := testStoreMessage("gamma")
	third.CreatedAt = testTime().Add(2 * time.Second)
	seq3, err := s.AppendMessage(ctx, third)
	if err != nil || seq3 != 3 || third.ID != "msg_3" {
		t.Fatalf("AppendMessage#3 = %d, %q, %v", seq3, third.ID, err)
	}
	if got, err := s.ReadMessages(ctx, "grp_test", 3, 1); err != nil || len(got) != 1 || got[0].Text != "beta" {
		t.Fatalf("ReadMessages(before 3) = %v, %v", got, err)
	}
	if got, err := s.ReadMessages(ctx, "grp_test", 0, 2); err != nil || len(got) != 2 || got[0].Text != "beta" || got[1].Text != "gamma" {
		t.Fatalf("ReadMessages(latest 2) = %v, %v", got, err)
	}
	if got, err := s.SearchMessages(ctx, "grp_test", "BETA", 10); err != nil || len(got) != 1 || got[0].Text != "beta" {
		t.Fatalf("SearchMessages = %v, %v", got, err)
	}

	// Activity / cursor
	if err := s.AppendActivity(ctx, &Activity{GroupID: "grp_test", AfterSeq: 1, Kind: "tool", Name: "read_file", Status: "ok"}); err != nil {
		t.Fatalf("AppendActivity: %v", err)
	}
	if err := s.AppendActivity(ctx, &Activity{GroupID: "grp_test", AfterSeq: 2, Kind: "tool", Name: "write_file", Status: "ok"}); err != nil {
		t.Fatalf("AppendActivity: %v", err)
	}
	if got, err := s.ReadActivity(ctx, "grp_test", 1, 10); err != nil || len(got) != 1 || got[0].AfterSeq != 2 {
		t.Fatalf("ReadActivity = %v, %v", got, err)
	}
	if err := s.SaveCursor(ctx, &Cursor{GroupID: "grp_test", AgentID: "ag_test", HumanReadSeq: 3, AgentContextSeq: 2}); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}
	if got, err := s.LoadCursor(ctx, "grp_test", "ag_test"); err != nil || got.HumanReadSeq != 3 || got.AgentContextSeq != 2 {
		t.Fatalf("LoadCursor = %#v, %v", got, err)
	}
	if got, err := s.LoadCursor(ctx, "grp_test", "ag_missing"); err != nil || got.HumanReadSeq != 0 || got.AgentContextSeq != 0 {
		t.Fatalf("LoadCursor(missing) = %#v, %v", got, err)
	}

	// Task / events / filter
	task := testTask()
	if err := s.SaveTask(ctx, task); err != nil {
		t.Fatalf("SaveTask: %v", err)
	}
	if err := s.AppendTaskEvent(ctx, task.ID, TaskEvent{Type: "completed", Data: map[string]any{"status": "completed"}}); err != nil {
		t.Fatalf("AppendTaskEvent: %v", err)
	}
	events, err := s.ReadTaskEvents(ctx, task.ID)
	if err != nil || len(events) != 1 || events[0].Seq != 1 || events[0].Type != "completed" {
		t.Fatalf("ReadTaskEvents = %#v, %v", events, err)
	}
	tasks, err := s.ListTasks(ctx, TaskFilter{GroupID: "grp_test", Status: TaskCompleted})
	if err != nil || len(tasks) != 1 || tasks[0].ID != task.ID {
		t.Fatalf("ListTasks = %v, %v", tasks, err)
	}
	if tasks, err := s.ListTasks(ctx, TaskFilter{Status: TaskFailed}); err != nil || len(tasks) != 0 {
		t.Fatalf("ListTasks(failed) = %v, %v", tasks, err)
	}

	// Need / artifact
	need := testNeed()
	if err := s.SaveNeed(ctx, need); err != nil {
		t.Fatalf("SaveNeed: %v", err)
	}
	if got, err := s.LoadNeed(ctx, need.ID); err != nil || got.Title != need.Title || got.Status != "pending" {
		t.Fatalf("LoadNeed = %#v, %v", got, err)
	}
	if _, err := s.LoadNeed(ctx, "need_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LoadNeed(missing) err = %v, want ErrNotFound", err)
	}
	needs, err := s.ListNeeds(ctx, NeedFilter{Status: "pending", GroupID: "grp_test"})
	if err != nil || len(needs) != 1 || needs[0].ID != need.ID {
		t.Fatalf("ListNeeds = %v, %v", needs, err)
	}
	artifact := testArtifact()
	if err := s.SaveArtifact(ctx, artifact); err != nil {
		t.Fatalf("SaveArtifact: %v", err)
	}
	gotArtifact, err := s.LoadArtifact(ctx, artifact.ID)
	if err != nil || gotArtifact.Name != artifact.Name || gotArtifact.Version != 1 {
		t.Fatalf("LoadArtifact = %#v, %v", gotArtifact, err)
	}

	// ID allocation / shape
	agentID, err := s.AllocateID(ctx, PrefixAgent)
	if err != nil || len(strings.TrimPrefix(agentID, PrefixAgent+"_")) != 8 {
		t.Fatalf("AllocateID(agent) = %q, %v", agentID, err)
	}
	groupID, err := s.AllocateID(ctx, PrefixGroup)
	if err != nil || len(strings.TrimPrefix(groupID, PrefixGroup+"_")) != 6 {
		t.Fatalf("AllocateID(group) = %q, %v", groupID, err)
	}
	if _, err := s.AllocateID(ctx, PrefixRound); !errors.Is(err, ErrInvalid) {
		t.Fatalf("AllocateID(round) err = %v, want ErrInvalid", err)
	}
}

func TestFileStoreAtomicWriteAndPermissions(t *testing.T) {
	root := t.TempDir()
	fs, err := OpenFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.SaveAgent(context.Background(), testAgent()); err != nil {
		t.Fatal(err)
	}

	profilePath := filepath.Join(root, "agents", "ag_test", "profile.json")
	fi, err := os.Stat(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("profile mode = %v, want no group/other bits", fi.Mode().Perm())
	}
	if tmp, _ := filepath.Glob(filepath.Join(filepath.Dir(profilePath), ".tmp-*")); len(tmp) != 0 {
		t.Fatalf("leftover temp files: %v", tmp)
	}
	var p AgentProfile
	if err := readJSON(profilePath, &p); err != nil {
		t.Fatalf("read persisted profile: %v", err)
	}
	if p.ID != "ag_test" {
		t.Fatalf("persisted agent = %#v", p)
	}
}

func TestFileStoreSeqRecovery(t *testing.T) {
	root := t.TempDir()
	fs, err := OpenFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := fs.AppendMessage(ctx, testStoreMessage("x")); err != nil {
			t.Fatal(err)
		}
	}
	// 重新打开 = 进程重启：seq 必须从日志重建，而不是从内存/0 开始。
	fs2, err := OpenFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	seq, err := fs2.AppendMessage(ctx, testStoreMessage("after restart"))
	if err != nil || seq != 3 {
		t.Fatalf("seq after restart = %d, %v; want 3", seq, err)
	}
}

func TestFileStoreSchemaMigrationAndErrors(t *testing.T) {
	t.Run("v0 migrates to current", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "hub.json"), []byte(`{"schema_version":0}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenFileStore(root); err != nil {
			t.Fatalf("OpenFileStore(v0): %v", err)
		}
		var meta hubMeta
		if err := readJSON(filepath.Join(root, "hub.json"), &meta); err != nil {
			t.Fatal(err)
		}
		if meta.SchemaVersion != CurrentSchemaVersion {
			t.Fatalf("schema = %d, want %d", meta.SchemaVersion, CurrentSchemaVersion)
		}
	})

	t.Run("future schema rejected", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "hub.json"), []byte(`{"schema_version":999}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenFileStore(root); !errors.Is(err, ErrUnsupportedSchema) {
			t.Fatalf("OpenFileStore(future) err = %v, want ErrUnsupportedSchema", err)
		}
	})

	t.Run("corrupt json rejected", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "hub.json"), []byte(`{`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenFileStore(root); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("OpenFileStore(corrupt) err = %v, want ErrCorrupt", err)
		}
	})
}

func TestFileStorePathSafety(t *testing.T) {
	fs, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := fs.SaveAgent(ctx, &AgentProfile{ID: "../escape"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("SaveAgent traversal err = %v, want ErrInvalid", err)
	}
	if _, err := fs.LoadAgent(ctx, ".."); !errors.Is(err, ErrInvalid) {
		t.Fatalf("LoadAgent traversal err = %v, want ErrInvalid", err)
	}
	if _, err := fs.ReadMessages(ctx, "../g", 0, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ReadMessages traversal err = %v, want ErrInvalid", err)
	}
}

func TestFileStoreTaskEventsAppend(t *testing.T) {
	root := t.TempDir()
	fs, err := OpenFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := fs.SaveTask(ctx, testTask()); err != nil {
		t.Fatal(err)
	}
	if err := fs.AppendTaskEvent(ctx, "task_test", TaskEvent{Type: "running"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "tasks", "task_test", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var ev TaskEvent
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("events lines = %d, want 1", len(lines))
	}
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.TaskID != "task_test" || ev.Type != "running" || ev.At.IsZero() {
		t.Fatalf("event = %#v", ev)
	}
}

func TestStoreAllocateIDCollision(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		fs, err := OpenFileStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := fs.SaveAgent(context.Background(), testAgent()); err != nil {
			t.Fatal(err)
		}
		calls := []string{"ag_test", "ag_test", "ag_other"}
		i := 0
		fs.idGen = func(string) string {
			id := calls[i]
			i++
			return id
		}
		got, err := fs.AllocateID(context.Background(), PrefixAgent)
		if err != nil || got != "ag_other" {
			t.Fatalf("AllocateID collision = %q, %v; want ag_other", got, err)
		}
	})
	t.Run("memory", func(t *testing.T) {
		ms := NewMemoryStore()
		if err := ms.SaveAgent(context.Background(), testAgent()); err != nil {
			t.Fatal(err)
		}
		calls := []string{"ag_test", "ag_test", "ag_other"}
		i := 0
		ms.idGen = func(string) string {
			id := calls[i]
			i++
			return id
		}
		got, err := ms.AllocateID(context.Background(), PrefixAgent)
		if err != nil || got != "ag_other" {
			t.Fatalf("AllocateID collision = %q, %v; want ag_other", got, err)
		}
	})
}

func TestStoreAllocateIDReserves(t *testing.T) {
	ctx := context.Background()
	fs, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fs.idGen = func(string) string { return "ag_reused" }
	first, err := fs.AllocateID(ctx, PrefixAgent)
	if err != nil || first != "ag_reused" {
		t.Fatalf("first allocate = %q, %v", first, err)
	}
	// 已被本进程预留但还没 Save：第二次不能再次发出同一个 ID。
	if _, err := fs.AllocateID(ctx, PrefixAgent); !errors.Is(err, ErrConflict) {
		t.Fatalf("second allocate err = %v, want ErrConflict", err)
	}

	ms := NewMemoryStore()
	ms.idGen = func(string) string { return "ag_reused" }
	if _, err := ms.AllocateID(ctx, PrefixAgent); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.AllocateID(ctx, PrefixAgent); !errors.Is(err, ErrConflict) {
		t.Fatalf("memory second allocate err = %v, want ErrConflict", err)
	}
}

func TestFileStoreTaskEventSeqRecovery(t *testing.T) {
	root := t.TempDir()
	fs, err := OpenFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := fs.SaveTask(ctx, testTask()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := fs.AppendTaskEvent(ctx, "task_test", TaskEvent{Type: "running"}); err != nil {
			t.Fatal(err)
		}
	}

	fs2, err := OpenFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs2.AppendTaskEvent(ctx, "task_test", TaskEvent{Type: "completed"}); err != nil {
		t.Fatal(err)
	}
	events, err := fs2.ReadTaskEvents(ctx, "task_test")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[2].Seq != 3 || events[2].Type != "completed" {
		t.Fatalf("events after restart = %#v", events)
	}
}

func TestMemoryStoreDeepCopy(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	agent := testAgent()
	agent.Tools.Disable = []string{"bash"}
	if err := s.SaveAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	agent.Tools.Disable[0] = "mutated"
	gotAgent, err := s.LoadAgent(ctx, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotAgent.Tools.Disable) != 1 || gotAgent.Tools.Disable[0] != "bash" {
		t.Fatalf("agent mutated through saved slice: %#v", gotAgent.Tools.Disable)
	}
	gotAgent.Tools.Disable[0] = "mutated-again"
	gotAgent, _ = s.LoadAgent(ctx, agent.ID)
	if gotAgent.Tools.Disable[0] != "bash" {
		t.Fatalf("agent mutated through loaded slice: %#v", gotAgent.Tools.Disable)
	}

	group := testGroup()
	if err := s.SaveGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	group.Members[0].Role = RoleObserver
	gotGroup, _ := s.LoadGroup(ctx, group.ID)
	if gotGroup.Members[0].Role != RoleOwner {
		t.Fatalf("group mutated through members slice: %#v", gotGroup.Members)
	}

	task := testTask()
	task.Result = &TaskResult{Artifacts: []TaskArtifact{{Name: "a.md"}}}
	if err := s.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	task.Result.Artifacts[0].Name = "mutated.md"
	gotTask, _ := s.LoadTask(ctx, task.ID)
	if gotTask.Result.Artifacts[0].Name != "a.md" {
		t.Fatalf("task mutated through result artifacts: %#v", gotTask.Result)
	}
}
