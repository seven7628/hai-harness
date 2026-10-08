package builtin

// bash_structured_test.go：bash 的 OutputSchemaProvider 钉子（设计文档 §15.4 ——「codemode
// 收益最大的一处单点改造」）。四件事各有一条回归：
//  1. 小输出：文本与结构化 output 同源（同一次调用同一份内容），不落盘；
//  2. 超 1 MiB：output 截断到上限 + 全文落盘（0600、工作区之外、可逐字节读回）；
//  3. exit_code / wall_time_seconds 取真实结局（含超时 -1 与「未执行则无结构化结果」）；
//  4. 模型路径零变化：文本仍是被引擎 20 KB 截断的同一份内容，且 bash 不实现 SkipTruncateProvider。
//
// 本文件**自包含**（不引用其它 _test.go 里的夹具/辅助函数）：本 worktree 的既有测试文件
// 都在 .git/info/exclude 里（只存在于工作区，不进本分支的提交），靠它们会让本文件在干净
// 检出上编译不过。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/tools"
)

// bashStructured 结构化结果的测试侧视图（字段名与 OutputSchema 一一对应）。
type bashStructured struct {
	Output         string  `json:"output"`
	Truncated      bool    `json:"truncated"`
	FullOutputPath string  `json:"full_output_path"`
	ExitCode       int     `json:"exit_code"`
	WallTimeSec    float64 `json:"wall_time_seconds"`
}

// callBashStructured 直接调工具（不经引擎）：返回文本路径与结构化原文。
// 引擎的读取时机（Call 成功返回之后）与此等价，故这里就是编排路径看到的同一份数据。
// ctx 必须带 tools.WithScriptCall 标记 —— 结构化结果**只在脚本路径产出**（引擎在
// ExecuteOne 里注入该标记），不带标记走的就是模型路径（见
// TestBashModelPathProducesNoStructuredContent）。
func callBashStructured(t *testing.T, tool *BashTool, command string) (string, bashStructured) {
	t.Helper()
	args, err := json.Marshal(map[string]any{"command": command})
	if err != nil {
		t.Fatal(err)
	}
	// 结构化结果写在**本次调用**的 ctx 槽里（引擎在真实链路中注入；这里自备一个同形态的槽）。
	sink := &tools.StructuredSink{}
	ctx := tools.WithScriptCall(tools.WithStructuredSink(context.Background(), sink))
	text, err := tool.Call(ctx, "bash", string(args))
	if err != nil {
		t.Fatalf("bash.Call(%q): %v", command, err)
	}
	return text, decodeBashStructured(t, sink.Content())
}

// decodeBashStructured 解码结构化原文（nil / 非法 JSON 直接失败）。
func decodeBashStructured(t *testing.T, raw []byte) bashStructured {
	t.Helper()
	if raw == nil {
		t.Fatal("StructuredContent = nil，want 结构化结果（本次调用已真实执行）")
	}
	if !json.Valid(raw) {
		t.Fatalf("StructuredContent 不是合法 JSON: %q", raw)
	}
	var s bashStructured
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("解码结构化结果: %v", err)
	}
	return s
}

// runBashEngine 经引擎执行一条 bash 命令（真实链路：bash → sandbox → ExecSink /
// OutputSchemaProvider 旁路），返回引擎结果（模型路径的文本 + 结构化结果都在里面）。
func runBashEngine(t *testing.T, toolTimeout time.Duration, ctx context.Context, command string) core.ToolResult {
	t.Helper()
	e := tools.NewToolEngine()
	e.RegisterTool(ctx, NewBashTool(t.TempDir(), toolTimeout, nil, nil))
	results, err := e.Sequence(ctx, []core.ToolCall{{
		Id: "c1", Name: "bash", Arguments: `{"command":` + strconv.Quote(command) + `}`,
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	return results[0]
}

// repeatBytes 生成 n 个字节 b 的期望输出（命令用 head -c + tr 产出同样的内容，
// 无尾换行 → 可逐字节比对）。
func repeatBytes(n int, b byte) string { return strings.Repeat(string([]byte{b}), n) }

// TestBashStructuredSmallOutputHasNoSpill 小输出：不截断、不落盘，文本即 output。
func TestBashStructuredSmallOutputHasNoSpill(t *testing.T) {
	tool := NewBashTool(t.TempDir(), 0, nil, nil)
	text, got := callBashStructured(t, tool, "echo hello; echo oops 1>&2")

	if got.Output != text {
		t.Fatalf("结构化 output 与文本不同源:\n文本 = %q\n结构化 = %q", text, got.Output)
	}
	if !strings.Contains(got.Output, "hello") || !strings.Contains(got.Output, "oops") {
		t.Fatalf("output 应含 stdout+stderr 合并结果: %q", got.Output)
	}
	if got.Truncated {
		t.Error("小输出不得标 truncated")
	}
	if got.FullOutputPath != "" {
		t.Errorf("小输出不该落盘: full_output_path = %q", got.FullOutputPath)
	}
	if got.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", got.ExitCode)
	}
	if got.WallTimeSec <= 0 {
		t.Errorf("wall_time_seconds = %v, want > 0（真实墙钟）", got.WallTimeSec)
	}
}

// TestBashStructuredOverLimitTruncatesAndSpillsPrivateFile 超 1 MiB：output 恰为上限、
// 全文落盘且文件是 0600 的工作区之外文件，内容可逐字节读回（脚本据此做过滤/聚合）。
func TestBashStructuredOverLimitTruncatesAndSpillsPrivateFile(t *testing.T) {
	const total = maxBashStructuredOutput + 200*1024
	ws := t.TempDir()
	tool := NewBashTool(ws, 0, nil, nil)
	_, got := callBashStructured(t, tool, fmt.Sprintf("head -c %d /dev/zero | tr '\\0' a", total))

	if !got.Truncated {
		t.Fatalf("超 1 MiB 必须标 truncated（output len=%d）", len(got.Output))
	}
	if len(got.Output) != maxBashStructuredOutput {
		t.Fatalf("output 应恰为 1 MiB 上限: len=%d, want %d", len(got.Output), maxBashStructuredOutput)
	}
	if got.Output != repeatBytes(maxBashStructuredOutput, 'a') {
		t.Fatal("截断应保留**前缀**（脚本按顺序读），内容不得被改写/插提示行")
	}
	if got.FullOutputPath == "" {
		t.Fatal("超 1 MiB 必须落盘并给出 full_output_path")
	}

	// 位置：工作区之外（落盘目录在系统临时目录下）。
	if strings.HasPrefix(got.FullOutputPath, ws+string(filepath.Separator)) {
		t.Fatalf("落盘文件不得在工作区内: %q（workspace = %q）", got.FullOutputPath, ws)
	}
	if !strings.HasPrefix(got.FullOutputPath, filepath.Clean(os.TempDir())) {
		t.Fatalf("落盘文件应在系统临时目录（%q）之下: %q", os.TempDir(), got.FullOutputPath)
	}

	// 权限：文件 0600（命令输出常含凭据/私有数据），目录不得给组/其他人任何权限。
	info, err := os.Stat(got.FullOutputPath)
	if err != nil {
		t.Fatalf("落盘文件不存在: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("落盘文件权限 = %04o, want 0600", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(got.FullOutputPath))
	if err != nil {
		t.Fatalf("落盘目录不存在: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("落盘目录权限 = %04o, want 仅属主可访问（无组/其他人位）", perm)
	}

	// 内容可读回 = 未截断全文。
	full, err := os.ReadFile(got.FullOutputPath)
	if err != nil {
		t.Fatalf("读回落盘文件: %v", err)
	}
	if len(full) != total {
		t.Fatalf("落盘文件长度 = %d, want %d（必须是未截断全文）", len(full), total)
	}
	if string(full) != repeatBytes(total, 'a') {
		t.Fatal("落盘内容与命令真实输出不一致")
	}
}

// TestBashStructuredReportsRealExitCode 失败命令：exit_code 如实（非零不伪造 0）。
func TestBashStructuredReportsRealExitCode(t *testing.T) {
	tool := NewBashTool(t.TempDir(), 0, nil, nil)
	_, got := callBashStructured(t, tool, "echo before-fail; exit 7")

	if got.ExitCode != 7 {
		t.Fatalf("exit_code = %d, want 7（真实退出码）", got.ExitCode)
	}
	if !strings.Contains(got.Output, "before-fail") {
		t.Fatalf("失败命令的部分输出仍应在 output 里: %q", got.Output)
	}
	if got.Truncated || got.FullOutputPath != "" {
		t.Fatalf("小输出不该截断/落盘: %+v", got)
	}
}

// TestBashStructuredNilWhenCommandNotExecuted 命令未执行（高危拦截）→ 它**自己的**槽
// 必须为空。槽每次调用独立（引擎在 Call 前注入），所以这里钉的是「失败路径不写槽」；
// 旧形态（实例字段）还需要额外防「上一次残留」，那种残留已随槽的引入结构性消失
// （并发下的清空/串味同理，见 tools 包的 TestStructuredSinkIsPerCallNotPerToolInstance）。
func TestBashStructuredNilWhenCommandNotExecuted(t *testing.T) {
	tool := NewBashTool(t.TempDir(), 0, nil, nil)
	// 先成功跑一次（历史上这是「残留」的来源），再跑被拦截的命令。
	if _, got := callBashStructured(t, tool, "echo first-call-ok"); got.ExitCode != 0 {
		t.Fatalf("前置调用应成功: %+v", got)
	}

	sink := &tools.StructuredSink{}
	ctx := tools.WithScriptCall(tools.WithStructuredSink(context.Background(), sink))
	if _, err := tool.Call(ctx, "bash", `{"command":"sudo rm -rf /"}`); err == nil {
		t.Fatal("高危命令必须被拒绝（未执行）")
	}
	if raw := sink.Content(); raw != nil {
		t.Fatalf("未执行的命令不得有结构化结果: %q", raw)
	}
}

// TestBashModelPathIsProjectionOfStructuredOutput 同一次调用的文本是结构化的投影：
// 脚本路径不砍（60 KB 全给），模型路径仍被引擎砍到 20 KB（头尾都在、中段省略）。
// TestBashModelPathProducesNoStructuredContent 模型路径（无脚本标记）**不产出**结构化
// 结果、也不落盘：设计文档 §15.4 要求完整输出只服务编排路径，模型看到的是引擎 20 KB
// 截断后的文本；在这里多产一份 1 MiB 的暂存 + 一个没人引用、永不删除的文件是纯成本。
func TestBashModelPathProducesNoStructuredContent(t *testing.T) {
	const total = 60 * 1024 // > 引擎 20 KB 上限、< 1 MiB 脚本上限
	cmd := fmt.Sprintf("head -c %d /dev/zero | tr '\\0' b", total)
	r := runBashEngine(t, 0, context.Background(), cmd)

	if r.Structured != nil {
		t.Fatalf("模型路径不该产出结构化结果: %s", r.Structured)
	}
	if !strings.Contains(r.Result, "bytes omitted from the middle") {
		t.Fatalf("模型路径必须仍被引擎 20 KB 截断（行为不变）：len=%d", len(r.Result))
	}
}

// TestBashStructuredScriptCallKeepsFullOutput 脚本路径（带脚本标记）：结构化 output 是
// 未截断的全文（1 MiB 以内），而**模型/文本路径仍被引擎截断** —— 两条路径的差异只体现在
// 结构化字段上，文本口径零变化。
func TestBashStructuredScriptCallKeepsFullOutput(t *testing.T) {
	const total = 60 * 1024
	cmd := fmt.Sprintf("head -c %d /dev/zero | tr '\\0' b", total)
	r := runBashEngine(t, 0, tools.WithScriptCall(context.Background()), cmd)
	got := decodeBashStructured(t, r.Structured)

	if len(got.Output) != total {
		t.Fatalf("脚本路径不该砍 60 KB 输出: len=%d, want %d", len(got.Output), total)
	}
	if got.Truncated || got.FullOutputPath != "" {
		t.Fatalf("未触达 1 MiB 上限时不该截断/落盘: %+v", got)
	}
	if got.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0", got.ExitCode)
	}
	if !strings.Contains(r.Result, "bytes omitted from the middle") {
		t.Fatalf("文本路径必须仍被引擎 20 KB 截断（模型看到的是同一份内容）：len=%d", len(r.Result))
	}
	if !strings.HasPrefix(r.Result, got.Output[:512]) || !strings.HasSuffix(r.Result, got.Output[len(got.Output)-512:]) {
		t.Fatal("文本头尾必须来自结构化 output（两者同源）")
	}
}

// TestBashStructuredOnTimeoutStatusMinusOne 超时：结构化 output 与文本逐字节相同
// （含 [TIMEOUT …] 首行声明，schema 没有 timed_out 字段，声明不能丢），exit_code = -1。
func TestBashStructuredOnTimeoutStatusMinusOne(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	r := runBashEngine(t, 5*time.Second, tools.WithScriptCall(ctx), "echo partial-out; sleep 5")
	got := decodeBashStructured(t, r.Structured)

	if got.ExitCode != -1 {
		t.Fatalf("超时的 exit_code = %d, want -1（被信号杀死）", got.ExitCode)
	}
	if !strings.HasPrefix(got.Output, "[TIMEOUT") {
		t.Fatalf("超时的结构化 output 必须带与文本一致的首行声明: %q", got.Output)
	}
	if !strings.Contains(got.Output, "partial-out") {
		t.Fatalf("超时输出必须是部分输出（已产生的部分不丢）: %q", got.Output)
	}
	if r.Result != got.Output {
		t.Fatalf("未触达 20 KB 时文本应逐字节等于结构化 output:\n文本 = %q\n结构化 = %q", r.Result, got.Output)
	}
}

// TestBashOutputSchemaDeclaresContract schema 必须精确声明这五个字段（脚本侧据此取值），
// 且 bash 不豁免引擎截断（SkipTruncateProvider 只给编排型工具 —— 模型路径的 20 KB 是刻意设计）。
func TestBashOutputSchemaDeclaresContract(t *testing.T) {
	tool := NewBashTool(t.TempDir(), 0, nil, nil)
	if _, ok := any(tool).(tools.SkipTruncateProvider); ok {
		t.Fatal("bash 不得实现 SkipTruncateProvider：模型路径的 20 KB 上限是刻意设计")
	}
	if _, ok := any(tool).(tools.OutputSchemaProvider); !ok {
		t.Fatal("bash 必须实现 tools.OutputSchemaProvider（codemode 脚本路径的原料）")
	}
	if _, err := json.Marshal(tool.OutputSchema()); err != nil {
		t.Fatalf("OutputSchema 不可序列化: %v", err)
	}
	schema, ok := tool.OutputSchema().(map[string]any)
	if !ok {
		t.Fatalf("OutputSchema 应是 JSON Schema object, got %T", tool.OutputSchema())
	}
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v, want object", schema["type"])
	}
	props, _ := schema["properties"].(map[string]any)
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	want := "exit_code,full_output_path,output,truncated,wall_time_seconds"
	if strings.Join(names, ",") != want {
		t.Fatalf("schema 属性 = %v, want %s", names, want)
	}
	wantTypes := map[string]string{
		"output": "string", "truncated": "boolean", "full_output_path": "string",
		"exit_code": "integer", "wall_time_seconds": "number",
	}
	for name, wantType := range wantTypes {
		p, _ := props[name].(map[string]any)
		if p["type"] != wantType {
			t.Errorf("%s.type = %v, want %v", name, p["type"], wantType)
		}
	}
	// required：恒存在的四个（full_output_path 仅在截断时有值，故不在其中）。
	req, _ := schema["required"].([]string)
	if strings.Join(req, ",") != "output,truncated,exit_code,wall_time_seconds" {
		t.Fatalf("schema required = %v", req)
	}
}
