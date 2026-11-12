package hooks

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/seven7628/hai-harness/events"
)

// 这条测试钉的是"跑过没有"这条链路的两端：
//
//	① 执行日志里必须有这一行（含正常结束的）；
//	② 有副作用的正常跑法必须发给用户（SideEffect != ""），没副作用的必须保持安静
//	   （PostToolUse 每个工具一次，报了就是刷屏）。
func TestDispatchJournalsAndReportsOnlySideEffects(t *testing.T) {
	cases := []struct {
		name       string
		command    string
		wantStatus string
		wantSide   string
		wantWarn   bool
	}{
		{
			name:       "有注入",
			command:    `printf '{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"看这里"}}'`,
			wantStatus: "ok",
			wantSide:   "context",
			wantWarn:   true,
		},
		{
			name:       "无输出",
			command:    `true`,
			wantStatus: "ok_no_output",
			wantWarn:   false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			journalPath := filepath.Join(t.TempDir(), "runs.jsonl")
			var warned []string
			cfg := &Config{Events: map[Event][]Group{
				EventPostToolUse: {{
					Hooks: []Handler{{Type: "command", Name: "probe", Command: c.command}},
					scope: scopeUser, // 用户级配置默认可信
				}},
			}}
			d := NewDispatcher(cfg, nil, func(name, msg string) {
				warned = append(warned, name+": "+msg)
			})
			d.Runner().SetJournal(NewJournal(journalPath))

			_ = d.ToolHooks(t.TempDir(), "sess-1").PostToolUse(context.Background(),
				events.PostToolUseContext{})

			runs, err := ReadJournal(journalPath, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 1 {
				t.Fatalf("执行日志应有 1 条，实际 %d", len(runs))
			}
			got := runs[0]
			if got.Name != "probe" || got.Status != c.wantStatus {
				t.Fatalf("日志内容不对：%+v", got)
			}
			if got.Session != "sess-1" {
				t.Fatalf("会话 id 没记下来：%+v", got)
			}
			if got.SideEffect != c.wantSide {
				t.Fatalf("SideEffect = %q，期望 %q", got.SideEffect, c.wantSide)
			}
			if (len(warned) > 0) != c.wantWarn {
				t.Fatalf("上报行为不对：warned=%v，期望上报=%v", warned, c.wantWarn)
			}
		})
	}
}
