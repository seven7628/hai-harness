package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	goruntime "github.com/seven7628/hai-harness/runtime"
	"github.com/seven7628/hai-harness/sandbox"
	"github.com/seven7628/hai-harness/tools"
)

// 本文件测试分两档（对齐 runtime/python_test.go 的约定）：
//
//   - 快测试（默认跑，含 CI）：注入假受管运行时 + 假/真后端（/usr/bin/python3 做替身解释器，
//     不联网、不装依赖），覆盖参数校验、脚本传递、静态检查、超时解析、引擎闸门、
//     runtime 不可用降级、Seatbelt 文件边界（沙箱可用时）。
//   - 慢测试（-short 跳过）：真受管运行时（~/.go-code/runtime/python，需已 bootstrap）
//     + 真 Seatbelt —— 覆盖「run_python 在真实生产链路下跑通办公依赖」这一前提。

// rpFakeRuntime 假受管运行时：只实现 Ensure（run_python 依赖的唯一能力面）。
//
// calls 有锁：这个假实现会被**并发**的预览测试共用（多个 Convert 同时 Ensure）
// ⇒ 裸自增是数据竞争（-race 会报）。2026-11-18 由第 5 轮爬山的 -race 全包跑发现
// （**既有缺陷，不是本轮引入的** —— 基线 974a9ca 上同样复现）。
type rpFakeRuntime struct {
	py  goruntime.Python
	err error
	mu  sync.Mutex
	// calls 受 mu 保护（读走 callCount()）。
	calls  int
	before func()
}

func (f *rpFakeRuntime) Ensure(context.Context) (goruntime.Python, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.before != nil {
		f.before()
	}
	return f.py, f.err
}

// callCount 读调用次数（并发安全）。
func (f *rpFakeRuntime) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// rpRecordingSandbox 记录 ExecSpec 且不真执行命令的沙箱（检查工具产出的命令行/时限）。
type rpRecordingSandbox struct {
	spec    sandbox.ExecSpec
	out     string
	err     error
	onRun   func(spec sandbox.ExecSpec)
	scripts []string // 每次 Run 时读到的脚本文件内容（验证传递给解释器的确实是原文）
}

func (r *rpRecordingSandbox) Run(_ context.Context, spec sandbox.ExecSpec) (string, error) {
	r.spec = spec
	if r.onRun != nil {
		r.onRun(spec)
	}
	if p, ok := rpScriptPathOf(spec.Command); ok {
		if b, err := os.ReadFile(p); err == nil {
			r.scripts = append(r.scripts, string(b))
		}
	}
	return r.out, r.err
}

// rpScriptPathOf 从命令行里取脚本路径（`<解释器> '<脚本>'`；解释器固定无引号时直接剥离）。
func rpScriptPathOf(cmd string) (string, bool) {
	i := strings.LastIndex(cmd, " '")
	if i < 0 {
		return "", false
	}
	p := strings.TrimPrefix(cmd[i+1:], "'")
	p = strings.TrimSuffix(p, "'")
	if p == "" {
		return "", false
	}
	return p, true
}

// rpFakePython 假运行时句柄：解释器 = 本机 /usr/bin/python3（真执行、不联网、不建 venv）。
// 无该系统解释器 → 跳过（测试替身，非产品依赖）。
func rpFakePython(t *testing.T) goruntime.Python {
	t.Helper()
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skipf("/usr/bin/python3 不可用，跳过解释器替身测试: %v", err)
	}
	return goruntime.Python{Root: t.TempDir(), VenvPython: "/usr/bin/python3", Base: "/usr/bin/python3"}
}

func rpCall(t *testing.T, tool *RunPythonTool, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Call(context.Background(), "run_python", string(raw))
}

// ---------- 参数：script / script_path 二选一 ----------

func TestRunPythonScriptArgExactlyOne(t *testing.T) {
	ws := t.TempDir()
	tool := NewRunPythonTool(ws, &rpFakeRuntime{py: rpFakePython(t)}, nil)

	cases := []struct {
		name string
		args string
		want string
	}{
		{"都不给", `{}`, "one of script or script_path is required"},
		{"都给", `{"script":"print(1)","script_path":"a.py"}`, "mutually exclusive"},
		{"全空白", `{"script":"   ","script_path":"\n"}`, "one of script or script_path is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := tool.ValidParams(context.Background(), "run_python", c.args); err == nil {
				t.Fatal("ValidParams 应拒绝")
			} else if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ValidParams 错误信息应含 %q, got %v", c.want, err)
			}
			_, err := tool.Call(context.Background(), "run_python", c.args)
			if err == nil {
				t.Fatal("Call 应拒绝（不执行任何脚本）")
			}
			if !strings.HasPrefix(err.Error(), "TOOL_ERROR\n") || !strings.Contains(err.Error(), "invalid_arguments") {
				t.Fatalf("应为可恢复的结构化错误, got %v", err)
			}
		})
	}
}

// TestRunPythonTimeoutSchema 时限口径对齐 bash（schema 1-3600，默认 120）。
func TestRunPythonTimeoutSchema(t *testing.T) {
	tool := NewRunPythonTool(t.TempDir(), nil, nil)
	props := tool.Parameters().(map[string]any)["properties"].(map[string]any)
	to := props["timeout"].(map[string]any)
	if to["minimum"] != 1 || to["maximum"] != 3600 || to["default"] != 120 {
		t.Fatalf("timeout schema = %#v", to)
	}
	// 无 required（script/script_path 二选一，任一都不是恒必填）
	if _, has := tool.Parameters().(map[string]any)["required"]; has {
		t.Fatal("run_python 不应声明 required（二选一语义）")
	}
	if tool.CanParallel() || tool.ReadOnly() {
		t.Fatal("执行类工具：应 CanParallel=false 且 ReadOnly=false")
	}
	if !tool.RequiresApproval(context.Background(), core.ToolCall{}) {
		t.Fatal("run_python 应声明需要审批（能写文件/执行）")
	}
}

// TestRunPythonTimeoutClamp 模型 timeout 参数钳制到 [1s,1h]；引擎可见时限额外含 bootstrap 余量。
func TestRunPythonTimeoutClamp(t *testing.T) {
	tool := NewRunPythonTool(t.TempDir(), nil, nil)
	cases := []struct {
		args string
		want time.Duration // PerCallTimeout（含余量）
	}{
		{`{}`, 0},             // 缺省 → 工具级默认
		{`{"timeout":0}`, 0},  // 非法 → 默认
		{`{"timeout":-5}`, 0}, // 非法 → 默认
		{`{"timeout":1}`, time.Second + runPythonBootstrapHeadroom},       // 下限
		{`{"timeout":120}`, 120*time.Second + runPythonBootstrapHeadroom}, // 默认值显式传
		{`{"timeout":99999}`, time.Hour + runPythonBootstrapHeadroom},     // 上限钳制 1h
	}
	for _, c := range cases {
		if got := tool.TimeoutForArgs(c.args); got != c.want {
			t.Errorf("TimeoutForArgs(%s) = %v, want %v", c.args, got, c.want)
		}
	}
	// 工具级声明 = 脚本默认 120s + 余量（首次调用要先 Ensure/bootstrap）
	if got := tool.ToolTimeout(); got != 120*time.Second+runPythonBootstrapHeadroom {
		t.Fatalf("ToolTimeout = %v", got)
	}
	// 显式配置的工具默认值生效
	custom := NewRunPythonTool(t.TempDir(), nil, nil)
	custom.Timeout = 5 * time.Second
	if got := custom.ToolTimeout(); got != 5*time.Second+runPythonBootstrapHeadroom {
		t.Fatalf("自定义默认值未生效: %v", got)
	}
	if got := custom.effectiveTimeout(runPythonArgs{}); got != 5*time.Second {
		t.Fatalf("effectiveTimeout 应用自定义默认值: %v", got)
	}
}

// ---------- 脚本传递方式（回归守卫：内容绝不进命令行） ----------

// TestRunPythonScriptTransportViaTempFile script 落临时文件执行：命令行只有解释器 + 脚本路径，
// 源码（引号/换行/$/中文）不经 shell 转义；执行期间文件在、执行后清理。
func TestRunPythonScriptTransportViaTempFile(t *testing.T) {
	ws := t.TempDir()
	src := "s = \"中文 'single' \\\"double\\\" $HOME `whoami` $(id)\"\n" +
		"print(s)\n" +
		"print('second line; with && and | and > redirection')\n" +
		"print(f\"{1+1}\")\n"
	sbx := &rpRecordingSandbox{out: "stub-output"}
	rt := &rpFakeRuntime{py: rpFakePython(t)}
	var existed bool
	var content string
	sbx.onRun = func(spec sandbox.ExecSpec) {
		p, ok := rpScriptPathOf(spec.Command)
		if !ok {
			t.Fatalf("命令行应含脚本路径: %q", spec.Command)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("执行期间脚本文件应存在（%s）: %v", p, err)
		}
		existed, content = true, string(b)
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(p)) }) // 断言失败时的清理兜底
	}
	tool := NewRunPythonTool(ws, rt, sbx)
	out, err := rpCall(t, tool, map[string]any{"script": src})
	if err != nil {
		t.Fatalf("Call 不应报错: %v", err)
	}
	if out != "stub-output" {
		t.Fatalf("输出应原样透传: %q", out)
	}
	if !existed {
		t.Fatal("未被要求执行")
	}
	if content != src {
		t.Fatalf("脚本内容被改写:\nwant %q\ngot  %q", src, content)
	}
	// 关键：源码不得出现在命令行里（否则 shell/Python 双重转义会破坏脚本）
	if strings.Contains(sbx.spec.Command, "中文") || strings.ContainsAny(sbx.spec.Command, "\n$`") {
		t.Fatalf("脚本内容泄漏进命令行: %q", sbx.spec.Command)
	}
	// 命令行 = 单引号转义的解释器 + 单引号转义的脚本路径
	if sbx.spec.Command != shQuote(rt.py.VenvPython)+" "+shQuote(rpMustScriptPath(t, sbx.spec.Command)) {
		t.Fatalf("命令行形态不符: %q", sbx.spec.Command)
	}
	if sbx.spec.Cwd != ws {
		t.Fatalf("cwd 应为工作区根: %q want %q", sbx.spec.Cwd, ws)
	}
	// 执行后清理
	if p, _ := rpScriptPathOf(sbx.spec.Command); rpExists(p) {
		t.Fatalf("临时脚本应在执行后清理: %s", p)
	}
	if rt.callCount() != 1 {
		t.Fatalf("Ensure 应恰好调用一次: %d", rt.callCount())
	}
}

// TestRunPythonTempScriptPreferredInRuntimeRoot 临时脚本首选受管运行时目录（沙箱 carve-out
// 子树，读写两开）。断言的是「传什么路径给沙箱」——沙箱可读性的前提。
func TestRunPythonTempScriptPreferredInRuntimeRoot(t *testing.T) {
	root := t.TempDir()
	sbx := &rpRecordingSandbox{}
	rt := &rpFakeRuntime{py: goruntime.Python{Root: root, VenvPython: "/usr/bin/python3"}}
	tool := NewRunPythonTool(t.TempDir(), rt, sbx)
	if _, err := rpCall(t, tool, map[string]any{"script": "print(1)"}); err != nil {
		t.Fatal(err)
	}
	p, ok := rpScriptPathOf(sbx.spec.Command)
	if !ok {
		t.Fatalf("命令行应含脚本路径: %q", sbx.spec.Command)
	}
	if !strings.HasPrefix(p, filepath.Join(root, "tmp")+string(os.PathSeparator)) {
		t.Fatalf("临时脚本应在 <运行时根>/tmp 下（沙箱 carve-out），got %s", p)
	}
}

// TestRunPythonScriptPathInPlace script_path 直接用用户文件（保留 __file__/traceback 语义）。
func TestRunPythonScriptPathInPlace(t *testing.T) {
	ws := t.TempDir()
	p := filepath.Join(ws, "do_work.py")
	if err := os.WriteFile(p, []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sbx := &rpRecordingSandbox{}
	tool := NewRunPythonTool(ws, &rpFakeRuntime{py: rpFakePython(t)}, sbx)
	if _, err := rpCall(t, tool, map[string]any{"script_path": "do_work.py"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := rpScriptPathOf(sbx.spec.Command); got != p {
		t.Fatalf("script_path 应原样执行: got %q want %q", got, p)
	}
	if !rpExists(p) {
		t.Fatal("script_path 指向的用户文件不得被清理")
	}

	// 相对路径逃逸 → 拒绝（与 FileTools/bash 同语义）
	if _, err := rpCall(t, tool, map[string]any{"script_path": "../outside.py"}); err == nil {
		t.Fatal("相对路径逃逸应被拒绝")
	} else if !strings.Contains(err.Error(), "escapes workspace root") {
		t.Fatalf("逃逸错误信息应可操作: %v", err)
	}
	// 不存在的文件 → 可恢复错误（引导先 glob/read_file）
	if _, err := rpCall(t, tool, map[string]any{"script_path": "nope.py"}); err == nil {
		t.Fatal("不存在的脚本应报错")
	} else if !strings.Contains(err.Error(), "script_not_found") {
		t.Fatalf("错误码应为 script_not_found: %v", err)
	}
	// 目录 → 明确错误
	if _, err := rpCall(t, tool, map[string]any{"script_path": "."}); err == nil {
		t.Fatal("目录应被拒绝")
	}
}

// ---------- 受管运行时不可用 ----------

// TestRunPythonRuntimeUnavailable bootstrap 失败 → 可恢复的结构化错误（不 panic、不执行脚本、引导告知用户）。
func TestRunPythonRuntimeUnavailable(t *testing.T) {
	ws := t.TempDir()
	cases := []struct {
		name string
		py   PythonRuntime
	}{
		{"Ensure 失败（无 python3/断网）", &rpFakeRuntime{err: errors.New("未找到可用的 python3（需 >= 3.9）\n提示：macOS 可执行 `xcode-select --install`")}},
		{"未装配运行时", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sbx := &rpRecordingSandbox{out: "should-not-run"}
			tool := NewRunPythonTool(ws, c.py, sbx)
			out, err := rpCall(t, tool, map[string]any{"script": "print('nope')"})
			if err == nil {
				t.Fatalf("应返回错误, got %q", out)
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, "TOOL_ERROR\n") {
				t.Fatalf("应为结构化错误: %v", err)
			}
			for _, want := range []string{"code: python_runtime_unavailable", "next_action: report_to_user", "fix: "} {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误应含 %q:\n%s", want, msg)
				}
			}
			// 结构化字段必须单行可解析（多行错误会导致后续字段被吞）
			for _, line := range strings.Split(msg, "\n")[1:] {
				if strings.Count(line, ": ") < 1 {
					t.Fatalf("TOOL_ERROR 字段行不可解析: %q", line)
				}
			}
			if sbx.spec.Command != "" {
				t.Fatal("运行时不可用时不得执行任何命令")
			}
		})
	}
}

// ---------- 高危脚本静态拦截（窄口径） ----------

func TestRunPythonHighRiskStaticCheck(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool // true = 应拦
	}{
		{"shutil.rmtree 根", `import shutil; shutil.rmtree("/")`, true},
		{"shutil.rmtree 根（带别的参数）", `shutil.rmtree('/', ignore_errors=True)`, true},
		{"shutil.rmtree 家目录 ~", `shutil.rmtree("~")`, true},
		{"shutil.rmtree 当前目录 .", `shutil.rmtree(".")`, true},
		{"shutil.rmtree 上级 ..", `shutil.rmtree("..")`, true},
		{"shutil.rmtree 通配", `shutil.rmtree("*")`, true},
		{"shutil.rmtree $HOME", `shutil.rmtree("$HOME")`, true},
		{"shutil.rmtree 家目录通配", `shutil.rmtree("~/*")`, true},
		{"os.removedirs 根", `os.removedirs("/")`, true},
		{"Path.home 自身", `shutil.rmtree(Path.home())`, true},
		{"expanduser('~') 自身", `shutil.rmtree(os.path.expanduser("~"))`, true},
		{"os.system rm -rf 根", `os.system("rm -rf /")`, true},
		{"os.system rm -rf 家", `os.system("rm -rf ~")`, true},
		{"subprocess 列表形式 rm -rf 根", `subprocess.run(["rm", "-rf", "/"])`, true},
		{"明确子目录", `shutil.rmtree("/tmp/build-xyz")`, false},
		{"工作区子目录（相对）", `shutil.rmtree("build")`, false},
		{"变量目标（有意放行：不做语义分析）", `target = os.environ["X"]; shutil.rmtree(target)`, false},
		{"字符串里的危险文本（非调用）", `print("shutil.rmtree('/')")`, false},
		{"注释里的危险文本", `# shutil.rmtree('/')` + "\n" + `print(1)`, false},
		{"文档字符串里的危险文本", `doc = """os.system("rm -rf /")"""; print(doc)`, false},
		{"单文件删除（爆炸半径小，不拦）", `os.remove("/etc/hosts")`, false},
		{"家目录子目录（明确目标）", `shutil.rmtree("~/Downloads/old-backup")`, false},
		{"rm -rf 明确子目录（与 bash 同口径）", `os.system("rm -rf /tmp/build-xyz")`, false},
		{"普通脚本", `from docx import Document; print(len(Document("a.docx").paragraphs))`, false},
		// 真实办公脚本形态（防误伤：这些是本工具的主用途，拦了等于工具不可用）
		{"openpyxl 生成 xlsx", "import openpyxl\nwb = openpyxl.load_workbook('book.xlsx')\nws = wb.active\nws['A1'] = '产品'\nwb.save('out.xlsx')", false},
		{"os.walk 遍历", "import os\nfor root, dirs, files in os.walk('.'):\n    print(root, files)", false},
		{"先删后建自己的 build 目录", "import os, shutil\nos.makedirs('build', exist_ok=True)\nshutil.rmtree('build')", false},
		{"tempfile 清理", "import tempfile, os\nprint(tempfile.gettempdir())\nos.remove('/tmp/scratch.tmp')", false},
		{"pypdf 读元数据", "from pypdf import PdfReader\nr = PdfReader('a.pdf')\nprint(len(r.pages), r.metadata)", false},
		{"subprocess 列表形式（非 rm）", "import subprocess\nsubprocess.run(['ls', '-la'], check=True)", false},
		{"pathlib 写产物", "from pathlib import Path\nout = Path('result')\nout.mkdir(exist_ok=True)\n(out / 'a.txt').write_text('中文')", false},
		{"URL 里的斜杠", `print('ratio 3/4; see https://example.com/a/b')`, false},
	}
	for _, c := range cases {
		got := checkPythonHighRisk(c.src) != ""
		if got != c.want {
			t.Errorf("%-32s: blocked=%v want=%v (reason=%q)", c.name, got, c.want, checkPythonHighRisk(c.src))
		}
	}
}

// TestRunPythonHighRiskBlocksBeforeExec 命中拦截时不执行、返回可恢复错误（模型可自我修正）。
func TestRunPythonHighRiskBlocksBeforeExec(t *testing.T) {
	ws := t.TempDir()
	p := filepath.Join(ws, "clean.py")
	if err := os.WriteFile(p, []byte("import shutil\nshutil.rmtree('/')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range []map[string]any{
		{"script": "import shutil\nshutil.rmtree('/')\n"},
		{"script_path": "clean.py"},
	} {
		sbx := &rpRecordingSandbox{}
		tool := NewRunPythonTool(ws, &rpFakeRuntime{py: rpFakePython(t)}, sbx)
		_, err := rpCall(t, tool, args)
		if err == nil {
			t.Fatalf("高危脚本应被拦截: %v", args)
		}
		if !strings.Contains(err.Error(), "code: high_risk_script") || !strings.Contains(err.Error(), "rewrite_script") {
			t.Fatalf("错误应引导改写脚本: %v", err)
		}
		if sbx.spec.Command != "" {
			t.Fatal("被拦截的脚本不得执行")
		}
		// 拦截路径不该把危险调用原文抄回上下文（避免把危险脚本再喂一遍给模型）；
		// detail 只报「危险目标」，fix 里的示例用无害子目录（shutil.rmtree("build")）。
		if strings.Contains(err.Error(), `rmtree('/')`) || strings.Contains(err.Error(), `rmtree("/")`) {
			t.Fatalf("错误信息不应回显危险调用原文: %v", err)
		}
	}
}

// ---------- 真执行（解释器替身 /usr/bin/python3）：输出、UTF-8、非零退出、超时 ----------

// TestRunPythonExecutesAndMergesOutput 输出合并返回；中文（UTF-8）不被吞；
// 非零退出不算 error（沿用 sandbox 语义：原因并入文本让模型自纠）。
func TestRunPythonExecutesAndMergesOutput(t *testing.T) {
	ws := t.TempDir()
	tool := NewRunPythonTool(ws, &rpFakeRuntime{py: rpFakePython(t)}, nil) // nil → NoSandbox 直通
	out, err := rpCall(t, tool, map[string]any{"script": `import sys
print("中文 ok")
print("to stderr", file=sys.stderr)
`})
	if err != nil {
		t.Fatalf("Call 不应报错: %v", err)
	}
	if !strings.Contains(out, "中文 ok") {
		t.Fatalf("stdout 中文应正确回传: %q", out)
	}
	if !strings.Contains(out, "to stderr") {
		t.Fatalf("stderr 应合并: %q", out)
	}

	// 退出码 != 0：不是 Go error，文本里带原因（与 bash 同语义）
	out, err = rpCall(t, tool, map[string]any{"script": "import sys; sys.exit(3)"})
	if err != nil {
		t.Fatalf("非零退出不应是 Go error: %v", err)
	}
	if !strings.Contains(out, "[exit:") {
		t.Fatalf("非零退出应带退出原因: %q", out)
	}

	// 语法错误：模型可见 traceback 后自行修正
	out, err = rpCall(t, tool, map[string]any{"script": "def broken(:\n"})
	if err != nil {
		t.Fatalf("脚本异常不应是 Go error: %v", err)
	}
	if !strings.Contains(out, "SyntaxError") {
		t.Fatalf("traceback 应回传: %q", out)
	}
}

// ---------- 产物声明（outputs → ToolDiffProvider → 产出卡） ----------
//
// 判据（改动前先读）：脚本的 args 是源码、result 是 stdout，都不含可靠路径 —— 产物只能由
// 模型显式声明。下面这些用例钉住声明契约的四件事：① 没声明/声明为空 → 零产出、零额外文本；
// ② 落盘的声明 → FileDiff 的路径口径与事实（size/lines/指纹/Created）；③ 没落盘的声明 →
// 结果首行的显式告知（不是错误）；④ 工作区外的产物 → 绝对路径。

// rpStubRuntime 假运行时：解释器路径只用于拼命令行 —— 配合 rpRecordingSandbox 的用例
// **从不真执行进程**，故不依赖本机装没装 python3（产物事实由工具执行后从磁盘读回）。
func rpStubRuntime(t *testing.T) PythonRuntime {
	t.Helper()
	return &rpFakeRuntime{py: goruntime.Python{Root: t.TempDir(), VenvPython: "/usr/bin/python3", Base: "/usr/bin/python3"}}
}

// rpWriteOnRun 让假执行后端在 Run 时把文件写进磁盘（模拟脚本的落盘副作用；含父目录）。
func rpWriteOnRun(t *testing.T, sbx *rpRecordingSandbox, path, content string) {
	t.Helper()
	sbx.onRun = func(sandbox.ExecSpec) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("造产物失败: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("造产物失败: %v", err)
		}
	}
}

// TestRunPythonOutputsEmptyNoDiff 未声明产物（缺省/空数组/全空白项）→ 无 diff、结果文本原样
// （不产生「0/0 声明」这类噪声，也不做任何目录推断）。
func TestRunPythonOutputsEmptyNoDiff(t *testing.T) {
	ws := t.TempDir()
	for _, args := range []map[string]any{
		{"script": "print(1)"},
		{"script": "print(1)", "outputs": []string{}},
		{"script": "print(1)", "outputs": []string{"", "   "}},
	} {
		sbx := &rpRecordingSandbox{out: "ok"}
		tool := NewRunPythonTool(ws, rpStubRuntime(t), sbx)
		out, err := rpCall(t, tool, args)
		if err != nil {
			t.Fatalf("Call 不应报错: %v", err)
		}
		if tool.Diff() != nil {
			t.Fatalf("无产物声明不应产出 diff（args=%v）: %+v", args, tool.Diff())
		}
		if out != "ok" {
			t.Fatalf("未声明产物时结果文本应原样（args=%v）: %q", args, out)
		}
	}
}

// TestRunPythonOutputsDiffFacts 真解释器（/usr/bin/python3 替身）写出的声明产物 → diff 的
// 路径/尺寸/行数/指纹/Created 全部与磁盘事实一致（工具是执行后读回文件算的）。
func TestRunPythonOutputsDiffFacts(t *testing.T) {
	ws := t.TempDir()
	tool := NewRunPythonTool(ws, &rpFakeRuntime{py: rpFakePython(t)}, nil) // nil → NoSandbox 直通
	content := "第一行\nsecond\nthird"
	// 脚本用 Go 原始字符串：源码里的 `\n` 保持两字符转义（真换行会截断 Python 字面量），
	// 由 Python 解释成 0x0A —— 落盘内容即 content（中文/换行/多行都覆盖）。
	out, err := rpCall(t, tool, map[string]any{
		"script": `import os
os.makedirs('out', exist_ok=True)
with open('out/report.txt', 'w', encoding='utf-8') as f:
    f.write('第一行' + '\n' + 'second' + '\n' + 'third')
print('WROTE')
`,
		"outputs": []string{"out/report.txt"},
	})
	if err != nil {
		t.Fatalf("Call 不应报错: %v", err)
	}
	if !strings.Contains(out, "WROTE") {
		t.Fatalf("脚本应执行成功: %q", out)
	}
	if strings.Contains(out, "declared file(s) exist") {
		t.Fatalf("全部落盘时不应有缺失告知: %q", out)
	}
	d := tool.Diff()
	if d == nil {
		t.Fatal("声明的产物存在 → 应有 diff")
	}
	if d.Path != "out/report.txt" {
		t.Fatalf("工作区内产物应为工作区相对路径, got %q", d.Path)
	}
	if d.Size != int64(len(content)) || d.Lines != 2 {
		t.Fatalf("size/lines 应与磁盘事实一致: size=%d lines=%d (want %d/2)", d.Size, d.Lines, len(content))
	}
	if d.SHA256 != contentFingerprint(content) {
		t.Fatalf("指纹应与内容一致: %q", d.SHA256)
	}
	if !d.Created || d.Added != 2 {
		t.Fatalf("运行前不存在的产物应标 Created 且 +N=行数: %+v", d)
	}
}

// TestRunPythonOutputsFirstExistingWins 声明的第一个路径没落盘 → 取下一个存在的（模型按重要性
// 排序，主产物口径）；既有文件被脚本覆盖 → Created=false（运行前快照判定，非「没读过就猜」）。
func TestRunPythonOutputsFirstExistingWins(t *testing.T) {
	ws := t.TempDir()
	old := filepath.Join(ws, "out", "report.xlsx")
	if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := "PK\x03\x04binary-ish-product"
	sbx := &rpRecordingSandbox{out: "done"}
	rpWriteOnRun(t, sbx, old, content)
	tool := NewRunPythonTool(ws, rpStubRuntime(t), sbx)

	out, err := rpCall(t, tool, map[string]any{
		"script":  "print(1)",
		"outputs": []string{"out/first.pdf", "out/report.xlsx"},
	})
	if err != nil {
		t.Fatalf("Call 不应报错: %v", err)
	}
	if !strings.Contains(out, "outputs: 1/2 declared file(s) exist (missing: out/first.pdf)") {
		t.Fatalf("未落盘的声明应显式告知（首行）: %q", out)
	}
	d := tool.Diff()
	if d == nil || d.Path != "out/report.xlsx" {
		t.Fatalf("应取第一个存在的声明路径: %+v", d)
	}
	if d.Created {
		t.Fatalf("运行前已存在的文件被覆盖 → Created 必须为 false: %+v", d)
	}
	if d.Size != int64(len(content)) || d.Added != 0 || d.Removed != 0 {
		t.Fatalf("既有文件不给 ± 数字（旧内容未读），size 仍为事实: %+v", d)
	}
}

// TestRunPythonOutputsBinaryFacts 二进制产物（xlsx/docx/pdf 等 zip 容器 —— 办公主路径的大多数）：
// Lines/Added 不声明（无行语义，错数比空值更坏），Size 与指纹照给。
func TestRunPythonOutputsBinaryFacts(t *testing.T) {
	ws := t.TempDir()
	content := "PK\x03\x04\x00\x00sheet1.xml\nmore bytes"
	p := filepath.Join(ws, "out.xlsx")
	sbx := &rpRecordingSandbox{out: "done"}
	rpWriteOnRun(t, sbx, p, content)
	tool := NewRunPythonTool(ws, rpStubRuntime(t), sbx)

	if _, err := rpCall(t, tool, map[string]any{"script": "print(1)", "outputs": []string{"out.xlsx"}}); err != nil {
		t.Fatalf("Call 不应报错: %v", err)
	}
	d := tool.Diff()
	if d == nil {
		t.Fatal("二进制产物也应产出 diff（Size/指纹是可靠事实）")
	}
	if d.Size != int64(len(content)) || d.SHA256 != contentFingerprint(content) {
		t.Fatalf("二进制产物的 size/指纹应正确: %+v", d)
	}
	if d.Lines != 0 || d.Added != 0 {
		t.Fatalf("二进制无行语义 → Lines/Added 留空（错数比空值更坏）: %+v", d)
	}
}

// TestRunPythonOutputsOutsideWorkspaceKeepsAbsolutePath 工作区外的产物（用户显式声明的绝对
// 路径，如 ~/Desktop 导出）→ Path 保留绝对路径（宿主可直接打开）；相对路径越出工作区
// （../）被 resolve 拒绝 → 不算产物，但**明确告知**（不静默吞掉一条声明）。
func TestRunPythonOutputsOutsideWorkspaceKeepsAbsolutePath(t *testing.T) {
	ws := t.TempDir()
	p := filepath.Join(t.TempDir(), "exported.txt")
	sbx := &rpRecordingSandbox{out: "done"}
	rpWriteOnRun(t, sbx, p, "x\n")
	tool := NewRunPythonTool(ws, rpStubRuntime(t), sbx)

	out, err := rpCall(t, tool, map[string]any{"script": "print(1)", "outputs": []string{p, "../escape.txt"}})
	if err != nil {
		t.Fatalf("Call 不应报错: %v", err)
	}
	if !strings.Contains(out, "outputs: 1/2 declared file(s) exist (missing: ../escape.txt)") {
		t.Fatalf("越出工作区的相对声明应被如实告知: %q", out)
	}
	d := tool.Diff()
	if d == nil || d.Path != p {
		t.Fatalf("工作区外产物应保留绝对路径 %q, got %+v", p, d)
	}
}

// TestRunPythonOutputsMissingToldExplicitlyNoError 缺失告知是**信息**不是错误：
// 同一批调用经引擎走一遍 → IsError=false，且 ToolResponse.Diff 被填上（产出卡采集的输入）。
func TestRunPythonOutputsMissingToldExplicitlyNoError(t *testing.T) {
	ws := t.TempDir()
	sbx := &rpRecordingSandbox{out: "ok"}
	rpWriteOnRun(t, sbx, filepath.Join(ws, "out", "kept.txt"), "kept\n")
	tool := NewRunPythonTool(ws, rpStubRuntime(t), sbx)

	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), tool)
	var resp *events.ToolResponse
	handler := func(_ context.Context, e events.Event) {
		if tr, ok := e.(*events.ToolResponse); ok {
			resp = tr
		}
	}
	results, err := eng.Sequence(context.Background(), []core.ToolCall{{
		Id: "c1", Index: 0, Name: "run_python",
		Arguments: `{"script":"print(1)","outputs":["out/kept.txt","out/gone.pdf"]}`,
	}}, handler)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("结果数异常: %+v", results)
	}
	if results[0].IsError {
		t.Fatalf("声明的产物缺失不是错误（脚本可能成功）: %+v", results[0])
	}
	if !strings.Contains(results[0].Result, "outputs: 1/2 declared file(s) exist (missing: out/gone.pdf)") {
		t.Fatalf("结果文本应含显式告知: %q", results[0].Result)
	}
	if resp == nil || resp.Diff == nil || resp.Diff.Path != "out/kept.txt" {
		t.Fatalf("ToolResponse.Diff 应被填上（产出卡采集的唯一输入）: %+v", resp)
	}
}

// TestRunPythonOutputsTooManyRejected 超过 10 条声明是**参数错误**（不是静默丢弃）：
// ValidParams 与 Call 都拒绝，且脚本一行都没执行（不产生副作用）。
func TestRunPythonOutputsTooManyRejected(t *testing.T) {
	sbx := &rpRecordingSandbox{out: "should-not-run"}
	tool := NewRunPythonTool(t.TempDir(), rpStubRuntime(t), sbx)
	outs := make([]string, 0, maxRunPythonOutputs+1)
	for i := 0; i <= maxRunPythonOutputs; i++ {
		outs = append(outs, fmt.Sprintf("out/%d.txt", i))
	}
	raw, err := json.Marshal(map[string]any{"script": "print(1)", "outputs": outs})
	if err != nil {
		t.Fatal(err)
	}
	if err := tool.ValidParams(context.Background(), "run_python", string(raw)); err == nil {
		t.Fatal("ValidParams 应拒绝超限声明")
	} else if !strings.Contains(err.Error(), "at most 10 entries") {
		t.Fatalf("错误信息应给出上限: %v", err)
	}
	_, err = tool.Call(context.Background(), "run_python", string(raw))
	if err == nil {
		t.Fatal("Call 应拒绝超限声明")
	}
	if !strings.Contains(err.Error(), "invalid_arguments") {
		t.Fatalf("应为可恢复的结构化错误: %v", err)
	}
	if sbx.spec.Command != "" {
		t.Fatalf("参数校验失败时不得执行脚本: %q", sbx.spec.Command)
	}
}

// TestRunPythonEscapingEndToEnd 含引号/换行/$/反引号/中文的脚本真跑通（传递方式的端到端守卫）。
func TestRunPythonEscapingEndToEnd(t *testing.T) {
	tool := NewRunPythonTool(t.TempDir(), &rpFakeRuntime{py: rpFakePython(t)}, nil)
	src := `import json, sys
payload = {"text": "中文", "quote": "'\"", "dollar": "$HOME $(id) ` + "`" + `whoami` + "`" + `", "price": 1280, "note": "a && b | c > d < e"}
print(json.dumps(payload, ensure_ascii=False, sort_keys=True))
print("mode:", sys.argv[0].endswith(".py"))
`
	out, err := rpCall(t, tool, map[string]any{"script": src})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"text": "中文"`) {
		t.Fatalf("中文/引号内容被改写: %q", out)
	}
	if !strings.Contains(out, `"dollar": "$HOME $(id) `+"`"+`whoami`+"`"+`"`) {
		t.Fatalf("$ 与反引号被 shell 展开: %q", out)
	}
	if !strings.Contains(out, "mode: True") {
		t.Fatalf("脚本应以文件形式执行（argv[0] 为 .py 路径）: %q", out)
	}
}

// TestRunPythonTimeoutKillsScript 死循环脚本被终止，输出带明确原因。
func TestRunPythonTimeoutKillsScript(t *testing.T) {
	if testing.Short() {
		t.Skip("慢：真跑 1s 超时")
	}
	tool := NewRunPythonTool(t.TempDir(), &rpFakeRuntime{py: rpFakePython(t)}, nil)
	start := time.Now()
	out, err := rpCall(t, tool, map[string]any{"script": "print('started', flush=True)\nwhile True:\n    pass\n", "timeout": 1})
	if err != nil {
		t.Fatalf("超时不应是 Go error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("超时未生效，耗时 %v", elapsed)
	}
	if !strings.Contains(out, "started") {
		t.Fatalf("超时前的输出应保留: %q", out)
	}
	if !strings.Contains(out, "[exit:") || !strings.Contains(out, "deadline exceeded") {
		t.Fatalf("超时原因应明确: %q", out)
	}

	// 模型未传 timeout：走工具默认值（2 分钟）—— 用短超时替身验证「默认值确实生效」太慢，
	// 这里只断言直接调用路径把默认值落到了 spec.Timeout。
	sbx := &rpRecordingSandbox{}
	tool2 := NewRunPythonTool(t.TempDir(), &rpFakeRuntime{py: rpFakePython(t)}, sbx)
	if _, err := rpCall(t, tool2, map[string]any{"script": "print(1)"}); err != nil {
		t.Fatal(err)
	}
	if sbx.spec.Timeout != defaultRunPythonTimeout {
		t.Fatalf("直接调用应用默认脚本时限 %v, got %v", defaultRunPythonTimeout, sbx.spec.Timeout)
	}
}

// TestRunPythonScriptTimeoutExcludesBootstrapHeadroom 时限分层：引擎可见时限含 bootstrap
// 余量（首次 Ensure 不被掐断），但落到沙箱的 spec.Timeout 只有脚本时限本身（脚本不多拿时间）。
// 这是本工具与 bash 的**唯一**语义差异，最容易在重构中被合并成同一个值。
func TestRunPythonScriptTimeoutExcludesBootstrapHeadroom(t *testing.T) {
	tool := NewRunPythonTool(t.TempDir(), &rpFakeRuntime{py: rpFakePython(t)}, nil)

	// 引擎路径：ctx 带 deadline（模拟引擎 liftableTimeout）→ spec 是脚本时限，不是 ctx 剩余
	engineCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second+runPythonBootstrapHeadroom)
	defer cancel()
	sbx := &rpRecordingSandbox{}
	tool.sbx = sbx
	raw, _ := json.Marshal(map[string]any{"script": "print(1)", "timeout": 120})
	if _, err := tool.Call(engineCtx, "run_python", string(raw)); err != nil {
		t.Fatal(err)
	}
	if sbx.spec.Timeout != 120*time.Second {
		t.Fatalf("沙箱时限应为脚本时限 120s（不含 bootstrap 余量）, got %v", sbx.spec.Timeout)
	}

	// promote 后台（ctx 标记 Unlimited）→ 不限时
	sbx2 := &rpRecordingSandbox{}
	tool.sbx = sbx2
	unlimited := context.WithValue(context.Background(), tools.UnlimitedTimeoutKey, true)
	if _, err := tool.Call(unlimited, "run_python", string(raw)); err != nil {
		t.Fatal(err)
	}
	if sbx2.spec.Timeout != -1 {
		t.Fatalf("后台任务应不限时（-1）, got %v", sbx2.spec.Timeout)
	}
}

// TestRunPythonEngineTruncation 引擎 20000 字节闸门照常生效：超长输出截断后
// **头尾都在**，且中段省略量被精确量化（旧实现是纯头部截断，丢掉了尾部诊断）。
func TestRunPythonEngineTruncation(t *testing.T) {
	// 头尾可区分（"HEAD"/"TAIL"）：断言两头真的都在，而不是只留下开头那段。
	const total = 30000
	big := "HEAD" + strings.Repeat("A", total-8) + "TAIL" // 恰好 total 字节
	sbx := &rpRecordingSandbox{out: big}
	tool := NewRunPythonTool(t.TempDir(), &rpFakeRuntime{py: rpFakePython(t)}, sbx)

	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), tool)
	results, err := eng.Sequence(context.Background(), []core.ToolCall{
		{Id: "c1", Index: 0, Name: "run_python", Arguments: `{"script":"print('x')"}`},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].IsError {
		t.Fatalf("结果异常: %+v", results)
	}
	got := results[0].Result
	if len(got) >= len(big) {
		t.Fatalf("超长输出应被截断: %d >= %d", len(got), len(big))
	}
	// 默认闸门 20000 → head 10000 / tail 10000（纯 ASCII，无 rune 边界回退）。
	const headEnd, tailStart = 10000, total - 10000
	const omitted = tailStart - headEnd // 独立算出的省略量 = 10000
	if !strings.HasPrefix(got, big[:headEnd]) {
		t.Fatalf("截断后必须保留头部: %q", got[:min(80, len(got))])
	}
	if !strings.HasSuffix(got, big[tailStart:]) {
		t.Fatalf("截断后必须保留尾部（旧实现把 TAIL/诊断整段丢掉）: %q", got[max(0, len(got)-80):])
	}
	// 中段省略量必须精确（模型要知道丢了多少字节，而不是只看到「被截断」）。
	want := fmt.Sprintf("[... %d bytes omitted from the middle (%.2f%% of %d bytes total); showing the first %d and the last %d bytes",
		omitted, 100*float64(omitted)/float64(total), total, headEnd, total-tailStart)
	if !strings.Contains(got, want) {
		t.Fatalf("截断提示应明确省略量 %q: %q", want, got)
	}
}

// TestRunPythonToolSchema 工具描述必须诚实标注边界（模型据此决定要不要用、能不能指望联网）。
// 这些断言是「描述与实现同步」的守卫：改动描述时若漏掉某条边界，测试失败而不是静默说谎。
func TestRunPythonToolDescriptionHonesty(t *testing.T) {
	desc := NewRunPythonTool(t.TempDir(), nil, nil).Description()
	for _, want := range []struct{ frag, why string }{
		{"python-docx", "预装库清单（模型据此知道办公能力可用）"},
		{"fixed", "解释器固定（模型不可指定二进制/开关）"},
		{"sandbox", "沙箱内运行（文件边界）"},
		{"workspace root", "cwd = 工作区根（脚本产物该写哪里）"},
		{"$HOME and /tmp are writable", "沙箱写权限边界如实：生产默认开 WithHomeWrite（home 可写）+ /tmp 放行 —— 旧的「工作区外写入被拒」是过度承诺"},
		{"~/.go-code", "harness 自身配置目录仍 deny（settings.json 可能含 api_key）"},
		{"does not cover credential files", "凭据文件（~/.ssh/.aws 等）默认**没有**被沙箱拦 —— 不得宣称受保护（默认 deny 清单只有 .go-code）"},
		{"outputs", "产物声明参数（模型必须知道怎么让产物进产出卡）"},
		{"subprocess", "如实告知不是硬隔离（Python 可调 subprocess）"},
		{"not be assumed", "网络不做承诺（与 bash 同宿主策略，不保证可达）"},
		{"temporary file", "临时脚本会清理（不能用 __file__ 推产物目录）"},
	} {
		if !strings.Contains(desc, want.frag) {
			t.Errorf("工具描述应含 %q（%s）:\n%s", want.frag, want.why, desc)
		}
	}
	// 不得出现「已隔离/安全」之类过度承诺的措辞，也不得退回旧的误述
	// 「工作区外写入一律被拒」（生产默认 home/tmp 可写，见 sandbox/seatbelt_darwin.go）。
	for _, bad := range []string{"fully isolated", "cannot be escaped", "safe to run anything", "outside the workspace are denied"} {
		if strings.Contains(desc, bad) {
			t.Errorf("工具描述不得含过度承诺 %q", bad)
		}
	}
	// 体积数字必须自洽（旧值 ~64MB 与引导文案的 40/42MB 互相矛盾；实测 venv ≈70 MB，
	// 见 docs/office/SKILL_SMOKE_2026-09-18.md §5 第 12 条）。
	if strings.Contains(desc, "64MB") || strings.Contains(desc, "42MB") {
		t.Errorf("体积数字须为实测口径（tens of MB / about 70 MB）:\n%s", desc)
	}
}

// ---------- 沙箱文件边界（与 bash 同语义；Seatbelt 可用时） ----------

// TestRunPythonSeatbeltFileBoundary 真 Seatbelt（非 darwin / 沙箱不可用 → 跳过）：
// 工作区内可写、工作区外（/etc 与 home）被内核拒（EPERM），与 bash 走同一后端同一语义。
func TestRunPythonSeatbeltFileBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("慢：真跑 Seatbelt 子进程")
	}
	if !sandbox.Available() {
		t.Skip("sandbox-exec 不可用（非 macOS / 受限环境）")
	}
	ws := t.TempDir()
	sb, err := sandbox.NewSeatbelt(ws)
	if err != nil {
		t.Skipf("seatbelt 策略探测失败（本环境不支持真实沙箱）: %v", err)
	}
	tool := NewRunPythonTool(ws, &rpFakeRuntime{py: rpFakePython(t)}, sb)

	// 工作区内写：允许
	out, err := rpCall(t, tool, map[string]any{"script": "open('inside.txt', 'w', encoding='utf-8').write('ok 中文')\nprint('WROTE')\n"})
	if err != nil {
		t.Fatalf("Call 不应报错: %v", err)
	}
	if !strings.Contains(out, "WROTE") {
		t.Fatalf("工作区内写入应成功: %q", out)
	}
	if b, rerr := os.ReadFile(filepath.Join(ws, "inside.txt")); rerr != nil || !strings.Contains(string(b), "ok 中文") {
		t.Fatalf("工作区产物应落盘且 UTF-8 正确: %q err=%v", b, rerr)
	}

	// 工作区外写：被拦（EPERM + 沙箱标记）
	out, err = rpCall(t, tool, map[string]any{"script": "open('/etc/go-code-run-python-probe', 'w').write('x')\n"})
	if err != nil {
		t.Fatalf("沙箱拦截不应是 Go error: %v", err)
	}
	if !sandbox.IsSeatbeltBlocked(out) || !strings.Contains(out, "PermissionError") {
		t.Fatalf("/etc 写入应被沙箱拒绝: %q", out)
	}
	if _, statErr := os.Stat("/etc/go-code-run-python-probe"); statErr == nil {
		_ = os.Remove("/etc/go-code-run-python-probe")
		t.Fatal("沙箱外文件竟然被创建")
	}

	// 家目录写：被拦（Workspace 之外且未开 WithHomeWrite）
	home, herr := os.UserHomeDir()
	if herr == nil {
		probe := filepath.Join(home, ".go-code-run-python-probe")
		out, _ = rpCall(t, tool, map[string]any{"script": "open(" + strconvQuote(probe) + ", 'w').write('x')\n"})
		if !sandbox.IsSeatbeltBlocked(out) {
			t.Fatalf("家目录写入应被沙箱拒绝: %q", out)
		}
	}
}

// TestRunPythonSeatbeltProtectsGoCodeConfig 受管运行时目录是 carve-out（可读写），但
// ~/.go-code 根下的密钥文件（settings.json 含 api_key）必须保持 deny —— 读与写都被拒。
// 「运行时目录放行」与「harness 配置保护」相邻，最容易被一起放开（见 runtime/python.go
// 注释里那对「双面保护」），且这条边界正是 run_python 相对 bash 的新增暴露面（work
// persona 无 bash，此前模型拿不到任何读该文件的通道）。
func TestRunPythonSeatbeltProtectsGoCodeConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("慢：真跑 Seatbelt 子进程")
	}
	if !sandbox.Available() {
		t.Skip("sandbox-exec 不可用（非 macOS / 受限环境）")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("home 不可解析")
	}
	cfg := filepath.Join(home, ".go-code", "settings.json")
	if _, err := os.Stat(cfg); err != nil {
		t.Skipf("无 ~/.go-code/settings.json（本机未配置）: %v", err)
	}
	before, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}

	ws := t.TempDir()
	sb, err := sandbox.NewSeatbelt(ws, sandbox.WithHomeWrite()) // 与生产装配同选项（home 可写）
	if err != nil {
		t.Skipf("seatbelt 策略探测失败: %v", err)
	}
	tool := NewRunPythonTool(ws, &rpFakeRuntime{py: rpFakePython(t)}, sb)

	// 读：被拒
	out, err := rpCall(t, tool, map[string]any{"script": "print(open(" + strconvQuote(cfg) + ").read())\n"})
	if err != nil {
		t.Fatalf("沙箱拦截不应是 Go error: %v", err)
	}
	if !sandbox.IsSeatbeltBlocked(out) || !strings.Contains(out, "PermissionError") {
		t.Fatalf("读 ~/.go-code/settings.json 应被沙箱拒绝: %q", out)
	}
	if strings.Contains(out, "api_key") || strings.Contains(out, "sk-") {
		t.Fatalf("密钥内容泄漏进工具输出: %q", out)
	}
	// 写：被拒
	out, _ = rpCall(t, tool, map[string]any{"script": "open(" + strconvQuote(cfg) + ", 'w').write('pwned')\n"})
	if !sandbox.IsSeatbeltBlocked(out) {
		t.Fatalf("写 ~/.go-code/settings.json 应被沙箱拒绝: %q", out)
	}
	after, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("settings.json 被脚本改写（carve-out 应只覆盖 runtime 子树）")
	}
}

// TestRunPythonTempScriptCleanedUp 临时脚本（可能含用户数据）执行后必须清理，
// 且不落在工作区（不污染 git status / 文件监视器）。
func TestRunPythonTempScriptCleanedUp(t *testing.T) {
	if testing.Short() {
		t.Skip("慢：真跑解释器")
	}
	ws := t.TempDir()
	tool := NewRunPythonTool(ws, &rpFakeRuntime{py: rpFakePython(t)}, nil)
	out, err := rpCall(t, tool, map[string]any{"script": "import os, sys\nprint(os.path.dirname(os.path.abspath(sys.argv[0])))\n"})
	if err != nil {
		t.Fatal(err)
	}
	dir := strings.TrimSpace(out)
	if dir == "" {
		t.Fatalf("脚本应打印自身目录: %q", out)
	}
	if rpExists(dir) {
		t.Fatalf("临时脚本目录应在执行后清理: %s", dir)
	}
	// 工作区里不得留下 run-python-* 残留
	entries, _ := os.ReadDir(ws)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "run-python-") {
			t.Fatalf("工作区不应出现临时脚本目录: %s", e.Name())
		}
	}
}

// ---------- 慢测试：真受管运行时（~/.go-code/runtime/python）+ 真 Seatbelt ----------

// rpRealRuntime 复用已 bootstrap 的真实受管运行时（不自己建：会写用户 home 并下载依赖）。
// 未 bootstrap → 跳过（跑 `go test ./runtime/ -run TestRealEnsureInstallsRuntime` 可产出）。
func rpRealRuntime(t *testing.T) PythonRuntime {
	t.Helper()
	rt := goruntime.New(goruntime.Config{Timeout: 2 * time.Minute})
	st, err := rt.Status(context.Background())
	if err != nil || !st.Ready {
		// 指纹不匹配 = 运行时**存在但与源码不一致**（旧二进制写过共享运行时 / 源码刚改了
		// 依赖清单）：这是可修复的破损状态，**必须响亮失败**。若在这里 skip，整套真链路测试
		// 会全部静默跳过而套件仍显示全绿 —— 实际一条都没跑（2026-09 实测踩过两次）。
		if st.Stale {
			t.Fatalf("受管运行时与当前依赖清单不匹配（%s）：真链路测试会全部跳过、套件仍显示全绿。"+
				"修复：go test ./runtime/ -run TestRealEnsureInstallsRuntime", st.Reason)
		}
		t.Skipf("无已 bootstrap 的真实受管运行时（先跑 go test ./runtime/ -run TestRealEnsureInstallsRuntime）: ready=%v err=%v", st.Ready, err)
	}
	return rt
}

// TestRunPythonRealRuntimeOfficeImports 真受管运行时：四个办公库可 import（这是 run_python
// 存在的意义 —— 办公能力的主通道）。runtime 包已单测过解释器本身，此处验证的是
// 「经 run_python 工具链路（Ensure → 临时脚本 → 执行后端）走通」。
func TestRunPythonRealRuntimeOfficeImports(t *testing.T) {
	if testing.Short() {
		t.Skip("慢测试：需真实受管运行时")
	}
	tool := NewRunPythonTool(t.TempDir(), rpRealRuntime(t), nil)
	out, err := rpCall(t, tool, map[string]any{"script": `import docx, openpyxl, pptx, pypdf
print("OFFICE_OK", docx.__version__ if hasattr(docx, "__version__") else "docx", openpyxl.__version__ if hasattr(openpyxl, "__version__") else "openpyxl")
print("中文 ok")
`})
	if err != nil {
		t.Fatalf("Call 不应报错: %v", err)
	}
	if !strings.Contains(out, "OFFICE_OK") {
		t.Fatalf("办公依赖应可 import: %q", out)
	}
	if !strings.Contains(out, "中文 ok") {
		t.Fatalf("UTF-8 应正确: %q", out)
	}
}

// TestRunPythonRealRuntimeUnderSeatbelt 生产组合：受管 venv python + 真 Seatbelt 策略
// （~/.go-code/runtime 的 carve-out 必须生效，否则解释器直接崩 —— 见 runtime 包注释）。
func TestRunPythonRealRuntimeUnderSeatbelt(t *testing.T) {
	if testing.Short() {
		t.Skip("慢测试：需真实受管运行时")
	}
	if !sandbox.Available() {
		t.Skip("sandbox-exec 不可用")
	}
	ws := t.TempDir()
	sb, err := sandbox.NewSeatbelt(ws)
	if err != nil {
		t.Skipf("seatbelt 策略探测失败: %v", err)
	}
	tool := NewRunPythonTool(ws, rpRealRuntime(t), sb)
	src := `from docx import Document
d = Document()
d.add_heading("标题", level=1)
d.add_paragraph("正文 中文")
d.save("out.docx")
print("DOCX_WRITTEN")
`
	out, err := rpCall(t, tool, map[string]any{"script": src})
	if err != nil {
		t.Fatalf("Call 不应报错: %v", err)
	}
	if sandbox.IsSeatbeltBlocked(out) || strings.Contains(out, "init_import_site") {
		t.Fatalf("沙箱内运行时目录应可读（carve-out 回归？）: %q", out)
	}
	if !strings.Contains(out, "DOCX_WRITTEN") {
		t.Fatalf("docx 生成应成功: %q", out)
	}
	if _, err := os.Stat(filepath.Join(ws, "out.docx")); err != nil {
		t.Fatalf("产物应落在工作区: %v", err)
	}
	// 读回校验（避免自证：同一脚本写出的文件由独立进程读回）
	read := NewRunPythonTool(ws, rpRealRuntime(t), sb)
	out, err = rpCall(t, read, map[string]any{"script": "from docx import Document\nd = Document('out.docx')\nprint(d.paragraphs[0].text, d.paragraphs[1].text)\n"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "标题 正文 中文") {
		t.Fatalf("docx 读回内容不符: %q", out)
	}
}

// ---------- 辅助 ----------

// rpMustScriptPath 取命令行里的脚本路径（断言失败即终止）。
func rpMustScriptPath(t *testing.T, cmd string) string {
	t.Helper()
	p, ok := rpScriptPathOf(cmd)
	if !ok {
		t.Fatalf("命令行应含脚本路径: %q", cmd)
	}
	return p
}

func rpExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// strconvQuote 生成 Python 字符串字面量（路径可能含特殊字符；测试辅助）。
func strconvQuote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

// 运行中的输出尾巴：run_python 与 bash 共用同一条链路（events.ToolOutput）。
// 两条断言对应两种失效方式：有出口却没接（人看不到进展）、没出口却接了（白付开销）。
func TestRunPythonWiresOutputTail(t *testing.T) {
	ws := t.TempDir()
	rt := rpStubRuntime(t)

	// ① 有事件出口（ToolContext + CallId）→ OnOutput 必须接上，且回调真的把字节送到事件流。
	var mu sync.Mutex
	var frames []*events.ToolOutput
	handler := func(_ context.Context, ev events.Event) {
		if o, ok := ev.(*events.ToolOutput); ok {
			mu.Lock()
			frames = append(frames, o)
			mu.Unlock()
		}
	}
	sbx := &rpRecordingSandbox{out: "stub-output"}
	sbx.onRun = func(spec sandbox.ExecSpec) {
		if spec.OnOutput == nil {
			t.Fatal("有事件出口时 OnOutput 必须接上（否则尾部输出静默失效）")
		}
		// 第一次写不发帧（首时间片闸门）；跨过时间片后再写，必须发一帧。
		spec.OnOutput([]byte("early\n"))
		time.Sleep(tailMinInterval + 40*time.Millisecond)
		spec.OnOutput([]byte("late\n"))
	}
	tool := NewRunPythonTool(ws, rt, sbx)
	ctx := events.WithCallId(
		events.WithToolContextForRun(context.Background(), "run-py", "", 0, handler, nil, nil),
		"call-py-1",
	)
	if _, err := tool.Call(ctx, "run_python", `{"script":"print(1)"}`); err != nil {
		t.Fatalf("Call 不应报错: %v", err)
	}
	mu.Lock()
	got := append([]*events.ToolOutput(nil), frames...)
	mu.Unlock()
	if len(got) == 0 {
		t.Fatal("运行中应发出 ToolOutput 帧")
	}
	if got[0].Id != "call-py-1" || got[0].RunId != "run-py" {
		t.Fatalf("帧应归属到本次调用与运行：id=%q run=%q", got[0].Id, got[0].RunId)
	}
	if !strings.Contains(got[len(got)-1].Tail, "late") {
		t.Fatalf("最后一帧应包含最新输出：%q", got[len(got)-1].Tail)
	}

	// ② 没有 ToolContext（直接调用/测试）→ OnOutput 必须为 nil（零开销，逐字节历史一致）
	sbx2 := &rpRecordingSandbox{out: "ok"}
	var sawNonNil bool
	sbx2.onRun = func(spec sandbox.ExecSpec) { sawNonNil = spec.OnOutput != nil }
	tool2 := NewRunPythonTool(ws, rt, sbx2)
	if _, err := tool2.Call(context.Background(), "run_python", `{"script":"print(1)"}`); err != nil {
		t.Fatalf("Call 不应报错: %v", err)
	}
	if sawNonNil {
		t.Fatal("没有事件出口时 OnOutput 必须为 nil")
	}
}
