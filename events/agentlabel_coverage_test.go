package events

// 盖章覆盖面的**不变式测试**（2026-11-19）。
//
// 规则：**凡是带 `run_id` 的事件类型，都必须带 `agent_task_id` 字段，
// 且都必须被 StampAgentTaskID 覆盖。**
//
// 为什么用 AST 扫源码而不是手写清单：新增事件类型时若忘了加字段 / 忘了进盖章
// 函数，手写清单不会自己提醒，而这条测试会红 —— 也就是「漏了当场知道」，
// 而不是等到某个会话的归属统计悄悄少了一半。
//
// 判据拆成两条：
//  1. **字段覆盖面**：包内所有带 `json:"run_id"` 的结构体，都必须有
//     `json:"agent_task_id"` 字段（例外：已带 `json:"task_id"` 的 —— 它们
//     本来就有子智能体身份，不重复加）。
//  2. **盖章覆盖面**：所有带 `json:"agent_task_id"` 字段的结构体，都必须出现在
//     StampAgentTaskID 的 type switch 里。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// parsedEventTypes 扫本包源码，返回结构体名 → 该结构体的 json tag 集合。
func parsedEventTypes(t *testing.T) map[string]map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("解析 events 包失败: %v", err)
	}
	out := map[string]map[string]bool{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.TYPE {
					continue
				}
				for _, spec := range gd.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					tags := map[string]bool{}
					for _, fld := range st.Fields.List {
						if fld.Tag == nil {
							continue
						}
						tag, err := strconv.Unquote(fld.Tag.Value)
						if err != nil {
							continue
						}
						// "json:\"run_id,omitempty\"" → run_id
						if i := strings.Index(tag, `json:"`); i >= 0 {
							rest := tag[i+len(`json:"`):]
							if j := strings.IndexAny(rest, `",`); j >= 0 {
								tags[rest[:j]] = true
							}
						}
					}
					out[ts.Name.Name] = tags
				}
			}
		}
	}
	return out
}

// stampCoveredTypes 从 StampAgentTaskID 的源码里取出 type switch 覆盖的类型名。
func stampCoveredTypes(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(".", "agentlabel.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "agentlabel.go", raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSwitchStmt)
		if !ok {
			return true
		}
		for _, c := range ts.Body.List {
			cc := c.(*ast.CaseClause)
			for _, expr := range cc.List {
				if star, ok := expr.(*ast.StarExpr); ok {
					if id, ok := star.X.(*ast.Ident); ok {
						covered[id.Name] = true
					}
				}
			}
		}
		return true
	})
	return covered
}

// TestAgentLabelFieldCoverage：凡是带 run_id 的事件类型，都必须带 agent_task_id
// （已带 task_id 的除外 —— 它们本来就有子智能体身份）。
func TestAgentLabelFieldCoverage(t *testing.T) {
	types := parsedEventTypes(t)
	var missing []string
	for name, tags := range types {
		if !tags["run_id"] {
			continue
		}
		if tags["agent_task_id"] {
			continue
		}
		if tags["task_id"] {
			continue // 已有子智能体身份字段，不重复
		}
		missing = append(missing, name)
	}
	if len(missing) > 0 {
		t.Fatalf("这些事件类型带 run_id 却没有 agent_task_id（新增事件类型请补字段）：%v", missing)
	}
}

// TestAgentLabelStampCoverage：凡是带 agent_task_id 字段的事件类型，
// 都必须被 StampAgentTaskID 覆盖（否则该类型的事件会静默漏盖章）。
func TestAgentLabelStampCoverage(t *testing.T) {
	types := parsedEventTypes(t)
	covered := stampCoveredTypes(t)
	var missing []string
	for name, tags := range types {
		if !tags["agent_task_id"] {
			continue
		}
		if !covered[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("这些事件类型有 agent_task_id 字段但没进 StampAgentTaskID 的盖章分支：%v", missing)
	}
}

// TestStampAgentTaskIDBasic：盖章行为本身（支持的类型被盖；空 id 不动；
// 不支持的类型原样）。
func TestStampAgentTaskIDBasic(t *testing.T) {
	// 支持的类型
	le := &LLMEnd{RunId: "r1"}
	StampAgentTaskID(le, "task-abc")
	if le.AgentTaskId != "task-abc" {
		t.Fatalf("LLMEnd 未盖章: %+v", le)
	}
	ts := &ToolStart{Id: "c1"}
	StampAgentTaskID(ts, "task-abc")
	if ts.AgentTaskId != "task-abc" {
		t.Fatalf("ToolStart 未盖章: %+v", ts)
	}

	// 空 id ⇒ 不动（主会话事件必须逐字节不变）
	le2 := &LLMEnd{RunId: "r1"}
	StampAgentTaskID(le2, "")
	if le2.AgentTaskId != "" {
		t.Fatalf("空 id 不该盖章: %+v", le2)
	}

	// 已有 task_id 的类型（Agent*）不被 agent_task_id 覆盖（不重复加）
	as := &AgentStart{TaskId: "task-xyz"}
	StampAgentTaskID(as, "task-abc")
	if as.TaskId != "task-xyz" {
		t.Fatalf("AgentStart.TaskId 不该被改: %+v", as)
	}
}
