package main

// hub_search_bench_test.go：hub_search 在 1 万条消息 fixture 上的延迟实测（P4-04b-1）。
//
// 设计目标 p95 < 150ms（docs/agent-hub/P4-04B-DESIGN.md §1）。v1 没有 FTS/索引，
// 成本 = 「未删除会话数 × 单会话日志长度」的 JSONL 解析。这里直接写 fixture
//（逐条 AppendMessage 会 fsync 1 万次，太慢），再走完整的 dispatchHub 命令路径，
// 把 command_response 的 JSON 编码也算进去。
//
// 断言只设一个很宽松的上限（1.5s）防「数量级回归」；实测值用 t.Log 输出并写进 PR 说明。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

const (
	hubSearchFixtureGroups  = 10
	hubSearchFixturePerGrp  = 1000 // 10 × 1000 = 1 万条
	hubSearchFixtureQueries = 30
)

func TestHubSearchFanOut10kLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	root := t.TempDir()
	hubRoot := filepath.Join(root, "hub")
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	groupIDs := make([]agenthub.GroupID, 0, hubSearchFixtureGroups)
	for gi := 0; gi < hubSearchFixtureGroups; gi++ {
		gid := agenthub.GroupID(fmt.Sprintf("grp_fix%03d", gi))
		groupIDs = append(groupIDs, gid)
		dir := filepath.Join(hubRoot, "groups", string(gid))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		g := &agenthub.Group{
			ID: gid, Name: fmt.Sprintf("fixture 群 %d", gi),
			OwnerAgentID: "ag_fixture", Members: []agenthub.Member{{AgentID: "ag_fixture", Role: agenthub.RoleOwner}},
			Policy:    agenthub.GroupPolicy{Mode: agenthub.GroupModeAsk, MaxParallelRuns: 3},
			CreatedAt: base, UpdatedAt: base,
		}
		raw, _ := json.Marshal(g)
		if err := os.WriteFile(filepath.Join(dir, "group.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		for i := 1; i <= hubSearchFixturePerGrp; i++ {
			text := fmt.Sprintf("普通消息 %d-%d：今天的工作记录", gi, i)
			if i%100 == 0 {
				text = fmt.Sprintf("关键词 needle 出现在 %d-%d 的记录里", gi, i)
			}
			seq := int64(i)
			m := agenthub.Message{
				ID: fmt.Sprintf("msg_%d", seq), Seq: seq, GroupID: gid,
				Kind: agenthub.KindText, Text: text, Visibility: agenthub.VisibilityGroup,
				Sender:    agenthub.Sender{Kind: agenthub.SenderHuman},
				CreatedAt: base.Add(time.Duration(gi*hubSearchFixturePerGrp+i) * time.Second),
			}
			line, _ := json.Marshal(m)
			buf.Write(line)
			buf.WriteByte('\n')
		}
		if err := os.WriteFile(filepath.Join(dir, "log.jsonl"), buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	appDataDirOverride = root
	t.Cleanup(func() { appDataDirOverride = "" })
	bufOut := &syncBuf{}
	m := &manager{workspaces: map[string]*workspaceRuntime{}, out: newOut(bufOut)}
	t.Cleanup(m.shutdownHubRuntimes)

	// 预热：打开 store（rebuildIndex 读 1 万行）。
	if _, err := m.hubStore(); err != nil {
		t.Fatal(err)
	}

	latencies := make([]time.Duration, 0, hubSearchFixtureQueries)
	for i := 0; i < hubSearchFixtureQueries; i++ {
		started := time.Now()
		if !m.dispatchHub(command{Id: 100 + i, Type: CmdHubSearch, Payload: map[string]any{
			"query": "needle", "limit": 50,
		}}) {
			t.Fatal("hub_search 未被 dispatchHub 处理")
		}
		latencies = append(latencies, time.Since(started))
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p95 := latencies[(len(latencies)*95)/100]
	var total time.Duration
	for _, d := range latencies {
		total += d
	}
	t.Logf("hub_search fan-out：%d 群 × %d 条 = %d 条消息；n=%d p50=%v p95=%v mean=%v",
		hubSearchFixtureGroups, hubSearchFixturePerGrp, hubSearchFixtureGroups*hubSearchFixturePerGrp,
		len(latencies), latencies[len(latencies)/2], p95, total/time.Duration(len(latencies)))
	if p95 > 1500*time.Millisecond {
		t.Fatalf("p95 = %v，超过 1.5s 的灾难线（设计目标 <150ms）", p95)
	}
}
