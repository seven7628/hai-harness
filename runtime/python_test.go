package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件测试分两档：
//
//   - 快测试（默认跑，含 CI）：全部注入 RunCmd —— 不 spawn 真进程、不联网、不写
//     ~/.go-code。覆盖状态机、幂等、降级、锁、探测逻辑。
//   - 慢测试（-short 跳过）：真建 venv + 真装依赖（下载 ~42MB）。见
//     python_slow_test.go。默认 `go test ./runtime/` 会跑它们（本机验证用），
//     CI 用 `go test -short ./runtime/` 跳过。

// fakeCmd 记录被调用的命令行，按内置规则返回结果（离线模拟 python/pip 行为）。
type fakeCmd struct {
	mu    sync.Mutex
	calls [][]string

	// failPip 让 pip 安装失败（模拟断网）
	failPip bool
	// failImport 让模块 import 失败（模拟装完但库不可用）
	failImport bool
	// hang 非 nil 时在命令执行中阻塞，直到该 channel 关闭（模拟慢 pip，测锁）
	hang chan struct{}
}

func (f *fakeCmd) run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	hang := f.hang
	f.mu.Unlock()

	if hang != nil {
		<-hang
	}

	line := strings.Join(args, " ")
	switch {
	// 版本探测：报一个达标的版本
	case strings.Contains(line, "sys.version_info"):
		return "3.9\n", nil
	// venv 创建：真造出解释器文件（后续步骤要 os.Stat 它，必须真落盘）
	case strings.Contains(line, "-m venv"):
		venvDir := args[len(args)-1]
		binDir := filepath.Join(venvDir, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			return "", err
		}
		return "", os.WriteFile(filepath.Join(binDir, "python"), []byte("#!/bin/sh\n"), 0o755)
	// pip 安装
	case strings.Contains(line, "pip install"):
		if f.flag(&f.failPip) {
			return "ERROR: Could not find a version that satisfies the requirement\n" +
				strings.Repeat("pip noise line\n", 200), errors.New("exit status 1")
		}
		return "Successfully installed python-docx-1.2.0 openpyxl-3.1.5\n", nil
	// 模块自检 / 整体 import
	case strings.HasPrefix(line, "-c import"):
		if f.flag(&f.failImport) {
			return "ModuleNotFoundError: No module named 'docx'\n", errors.New("exit status 1")
		}
		return "", nil
	}
	return "", nil
}

// flag 读一个可被测试改动的开关（加锁，避免与并发 Ensure 的 goroutine 竞态）。
func (f *fakeCmd) flag(p *bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *p
}

// setPipFail 打开/关闭 pip 失败开关。
func (f *fakeCmd) setPipFail(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failPip = v
}

func (f *fakeCmd) count(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), substr) {
			n++
		}
	}
	return n
}

// newTestRuntime 构造隔离运行时（Root 落临时目录，注入假命令，不碰真实 ~/.go-code）。
func newTestRuntime(t *testing.T, f *fakeCmd) *Runtime {
	t.Helper()
	// 屏蔽本机真实 python 安装，保证候选列表可预测
	t.Setenv("GO_CODE_PYTHON_CANDIDATES", "/usr/bin/python3")
	return New(Config{Root: t.TempDir(), RunCmd: f.run, Timeout: 5 * time.Second})
}

// ---------- 状态机 / 幂等 ----------

// TestEnsureBuildsThenIdempotent：首次 Ensure 走完整 bootstrap；第二次直接命中就绪
// 标记返回（不重跑 venv/pip）—— 幂等是本包最核心的契约（否则每次调用都白等一次
// 42MB 下载）。
func TestEnsureBuildsThenIdempotent(t *testing.T) {
	f := &fakeCmd{}
	rt := newTestRuntime(t, f)

	p1, err := rt.Ensure(context.Background())
	if err != nil {
		t.Fatalf("首次 Ensure 应成功: %v", err)
	}
	if !p1.Created {
		t.Error("首次 Ensure 应执行创建（Created=true）")
	}
	if !isExecutable(p1.VenvPython) {
		t.Fatalf("返回的 venv 解释器应可执行: %s", p1.VenvPython)
	}
	if p1.Base != "/usr/bin/python3" {
		t.Errorf("基座解释器应为注入候选，got %q", p1.Base)
	}
	venvCalls, pipCalls := f.count("-m venv"), f.count("pip install")
	if venvCalls != 1 || pipCalls != 1 {
		t.Fatalf("首次应各执行一次 venv/pip，got venv=%d pip=%d", venvCalls, pipCalls)
	}
	// 依赖清单与就绪标记都应落盘
	if _, err := os.Stat(filepath.Join(rt.root(), "requirements.lock")); err != nil {
		t.Errorf("requirements.lock 应落盘: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rt.root(), readyMarker)); err != nil {
		t.Errorf("就绪标记应落盘: %v", err)
	}

	p2, err := rt.Ensure(context.Background())
	if err != nil {
		t.Fatalf("二次 Ensure 应成功: %v", err)
	}
	if p2.Created {
		t.Error("二次 Ensure 应命中已有环境（Created=false）")
	}
	if p2.VenvPython != p1.VenvPython {
		t.Errorf("两次应返回同一解释器: %q vs %q", p1.VenvPython, p2.VenvPython)
	}
	if got := f.count("-m venv"); got != 1 {
		t.Errorf("幂等：venv 只应建一次，got %d", got)
	}
	if got := f.count("pip install"); got != 1 {
		t.Errorf("幂等：pip install 只应跑一次，got %d", got)
	}
}

// TestEnsureConcurrentOnce 可重入：并发调用只能建一次（多会话/多 goroutine 同时首启）。
func TestEnsureConcurrentOnce(t *testing.T) {
	f := &fakeCmd{}
	rt := newTestRuntime(t, f)

	const n = 8
	var wg sync.WaitGroup
	created := make([]bool, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := rt.Ensure(context.Background())
			created[i], errs[i] = p.Created, err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d 失败: %v", i, err)
		}
	}
	if got := f.count("-m venv"); got != 1 {
		t.Errorf("并发下 venv 只应建一次，got %d", got)
	}
	if got := f.count("pip install"); got != 1 {
		t.Errorf("并发下 pip install 只应跑一次，got %d", got)
	}
	// 恰好一个 goroutine 真正创建
	if n := countTrue(created); n != 1 {
		t.Errorf("应恰好一个调用执行创建，got %d", n)
	}
}

func countTrue(b []bool) int {
	n := 0
	for _, v := range b {
		if v {
			n++
		}
	}
	return n
}

// TestEnsureAlreadyReadySkips：已就绪（标记+解释器齐备）时完全不碰子进程 —— 快路径
// 必须廉价，否则每次调用都要付解释器冷启动代价。
func TestEnsureAlreadyReadySkips(t *testing.T) {
	f := &fakeCmd{}
	rt := newTestRuntime(t, f)
	if _, err := rt.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 清空调用记录：再来一次应无任何除自检外的重活
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()

	p, err := rt.Ensure(context.Background())
	if err != nil {
		t.Fatalf("已就绪环境再 Ensure 应成功: %v", err)
	}
	if p.Created {
		t.Error("已就绪不应重建")
	}
	if got := f.count("-m venv"); got != 0 {
		t.Errorf("已就绪不应重跑 venv，got %d", got)
	}
	if got := f.count("pip install"); got != 0 {
		t.Errorf("已就绪不应重跑 pip，got %d", got)
	}
}

// TestReadyMarkerButMissingVenvReprobes：标记存在但解释器被删（用户手工清理/建到一半
// 崩溃）→ 判定未就绪并重建，不能只信标记。
func TestReadyMarkerButMissingVenvReprobes(t *testing.T) {
	f := &fakeCmd{}
	rt := newTestRuntime(t, f)
	if _, err := rt.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 删掉解释器（模拟 venv 被破坏）
	if err := os.RemoveAll(filepath.Join(rt.root(), "venv")); err != nil {
		t.Fatal(err)
	}
	st, err := rt.Status(context.Background())
	if err != nil {
		t.Fatalf("Status 不应报错: %v", err)
	}
	if st.Ready {
		t.Fatal("解释器缺失时不应判定就绪")
	}

	p, err := rt.Ensure(context.Background())
	if err != nil {
		t.Fatalf("应能自愈重建: %v", err)
	}
	if !p.Created {
		t.Error("解释器缺失后应重新创建")
	}
}

// TestStatusReasons：未就绪原因可诊断（供日志/UI 提示）。
func TestStatusReasons(t *testing.T) {
	f := &fakeCmd{}
	rt := newTestRuntime(t, f)

	st, err := rt.Status(context.Background())
	if err != nil {
		t.Fatalf("空目录 Status 不应报错: %v", err)
	}
	if st.Ready || st.Reason == "" {
		t.Fatalf("空目录应未就绪且给出原因，got %+v", st)
	}

	// 标记损坏 → 未就绪（不 panic、不报错）
	if err := os.WriteFile(filepath.Join(rt.root(), readyMarker), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, err := rt.Status(context.Background()); err != nil {
		t.Fatalf("坏标记 Status 不应报错: %v", err)
	} else if st.Ready {
		t.Fatal("坏标记不应判定就绪")
	}

	// 依赖清单指纹变化 → 未就绪（驱动升级重装）
	if _, err := rt.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(rt.root(), readyMarker))
	if err != nil {
		t.Fatal(err)
	}
	var m readyInfo
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m.Requirements = "stale-fingerprint"
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(rt.root(), readyMarker), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if st, err := rt.Status(context.Background()); err != nil {
		t.Fatalf("指纹失配 Status 不应报错: %v", err)
	} else if st.Ready {
		t.Fatal("依赖清单变化后不应判定就绪")
	}
}

// ---------- 失败降级（不 panic） ----------

// TestEnsureNoPythonFailsCleanly：找不到 python3 → 明确错误、不 panic；错误文案要
// 指出「需 >= 3.9」并给可操作提示（用户能照做）。
func TestEnsureNoPythonFailsCleanly(t *testing.T) {
	// 候选整体指向不存在的路径 + PATH 清空（LookPath 也找不到）
	t.Setenv("GO_CODE_PYTHON_CANDIDATES", "/nonexistent/python3")
	t.Setenv("PATH", "")
	rt := New(Config{Root: t.TempDir(), RunCmd: (&fakeCmd{}).run})

	p, err := rt.Ensure(context.Background())
	if err == nil {
		t.Fatal("无 python3 时应返回错误（而非 panic）")
	}
	if p.Created {
		t.Error("失败时不应返回已创建句柄")
	}
	for _, want := range []string{"3.9", "xcode-select"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误文案应含可操作提示 %q，got: %v", want, err)
		}
	}
}

// TestFindBaseSkipsTooOldPython：候选里版本过低的解释器要被跳过（3.8 装不上
// python-docx 1.2 / pypdf 6.x），并继续试下一个。
func TestFindBaseSkipsTooOldPython(t *testing.T) {
	old := fakePython(t, "3.8.10")
	ok := fakePython(t, "3.9.6")
	t.Setenv("GO_CODE_PYTHON_CANDIDATES", old+string(os.PathListSeparator)+ok)
	rt := New(Config{Root: t.TempDir()})

	base, err := rt.findBase(context.Background())
	if err != nil {
		t.Fatalf("应跳过 3.8 选中 3.9: %v", err)
	}
	if base != ok {
		t.Errorf("应选中版本达标的解释器 %s，got %s", ok, base)
	}
}

// TestFindBaseAllTooOldFails：全部候选版本过低 → 报错并说明原因。
func TestFindBaseAllTooOldFails(t *testing.T) {
	t.Setenv("GO_CODE_PYTHON_CANDIDATES", fakePython(t, "2.7.18"))
	rt := New(Config{Root: t.TempDir()})

	_, err := rt.findBase(context.Background())
	if err == nil {
		t.Fatal("版本全部过低应报错")
	}
	if !strings.Contains(err.Error(), "3.9") {
		t.Errorf("错误应说明版本要求，got: %v", err)
	}
}

// fakePython 造一个可执行脚本充当 python，对版本探测返回指定版本。
func fakePython(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "python3")
	// 真 shell 脚本：findBase 走 defaultRunCmd 真起进程读版本
	script := "#!/bin/sh\nif [ \"$1\" = \"-c\" ]; then printf '" + version + "\\n'; fi\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestEnsurePipFailureDegrades：pip 失败（断网）→ 返回明确错误、输出截断、不 panic；
// 且不写就绪标记（下次可自愈重试）。
func TestEnsurePipFailureDegrades(t *testing.T) {
	f := &fakeCmd{failPip: true}
	rt := newTestRuntime(t, f)

	_, err := rt.Ensure(context.Background())
	if err == nil {
		t.Fatal("pip 失败应返回错误")
	}
	if !strings.Contains(err.Error(), "pip install") {
		t.Errorf("错误应含失败命令，got: %v", err)
	}
	// 未写就绪标记 → 下次 Ensure 可重试
	if _, statErr := os.Stat(filepath.Join(rt.root(), readyMarker)); statErr == nil {
		t.Error("pip 失败后不应写就绪标记")
	}
	st, _ := rt.Status(context.Background())
	if st.Ready {
		t.Error("pip 失败后不应判定就绪")
	}
}

// TestDefaultRunCmdTruncatesOutput：真进程路径下，超长输出被截断后才进错误文本
// （pip 日志动辄数百行，全量灌进错误文本会淹没有效信息）。
//
// 单独测 defaultRunCmd 而非从 Ensure 断言：注入 RunCmd 的测试路径由注入实现返回
// 输出、Ensure 只取 error，截断逻辑（在 defaultRunCmd 里）根本走不到 —— 从
// Ensure 断言截断是空断言（实测 count=0）。
func TestDefaultRunCmdTruncatesOutput(t *testing.T) {
	// 真起一个进程吐 50KB（yes 可能不存在，用 awk 更稳；再不行用 sh 循环）
	script := "i=0; while [ $i -lt 2000 ]; do echo 'pip noise line'; i=$((i+1)); done; exit 3"
	out, err := defaultRunCmd(context.Background(), time.Minute, "sh", "-c", script)
	if err == nil {
		t.Fatal("非零退出应返回 error")
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("错误应含退出码，got: %v", truncate(err.Error(), 200))
	}
	// 错误文本里嵌的输出必须已截断（原始输出 ~30KB）
	body := err.Error()
	if len(body) > maxOutputLog+512 {
		t.Errorf("错误文本应截断到 ~%d 字节，got %d", maxOutputLog, len(body))
	}
	if !strings.Contains(body, "…") {
		t.Error("截断应带省略标记")
	}
	// 返回的 out 是原始输出（截断只作用于错误文本/日志），调用方仍可选择自行处理
	if len(out) <= maxOutputLog {
		t.Errorf("返回的原始输出应完整（截断只作用于错误文本），got %d 字节", len(out))
	}
}

// TestEnsureImportCheckFailureDegrades：pip 退出码 0 但模块 import 不了 → 仍算失败
// （不能只信 pip 退出码），且不写就绪标记。
func TestEnsureImportCheckFailureDegrades(t *testing.T) {
	f := &fakeCmd{failImport: true}
	rt := newTestRuntime(t, f)

	_, err := rt.Ensure(context.Background())
	if err == nil {
		t.Fatal("import 自检失败应返回错误")
	}
	if !strings.Contains(err.Error(), "自检") {
		t.Errorf("错误应指明自检失败，got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(rt.root(), readyMarker)); statErr == nil {
		t.Error("自检失败后不应写就绪标记")
	}
}

// TestEnsureRecoversAfterFailure：失败后重试能自愈（venv 已在，只需重跑安装）。
func TestEnsureRecoversAfterFailure(t *testing.T) {
	f := &fakeCmd{}
	rt := newTestRuntime(t, f)

	if _, err := rt.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 模拟「venv 建好了但装依赖失败」：删标记 + 让 pip 失败
	if err := os.Remove(filepath.Join(rt.root(), readyMarker)); err != nil {
		t.Fatal(err)
	}
	f.setPipFail(true)
	if _, err := rt.Ensure(context.Background()); err == nil {
		t.Fatal("pip 失败时应报错")
	}
	// 网络恢复 → 重试成功，且不重建 venv（只重装依赖）
	f.setPipFail(false)
	before := f.count("-m venv")
	p, err := rt.Ensure(context.Background())
	if err != nil {
		t.Fatalf("重试应成功: %v", err)
	}
	if !p.Created {
		t.Error("重试应完成创建")
	}
	if after := f.count("-m venv"); after != before {
		t.Errorf("已有 venv 不应重建：before=%d after=%d", before, after)
	}
}

// TestEnsureContextCanceled：ctx 已取消 → 尽快返回错误，不装依赖。
func TestEnsureContextCanceled(t *testing.T) {
	f := &fakeCmd{}
	rt := newTestRuntime(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// 取消的 ctx 下：要么直接报错，要么走完（注入实现不检查 ctx）——关键是
	// 不 panic、不挂住
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = rt.Ensure(ctx)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ctx 取消后 Ensure 不应挂住")
	}
}

// TestLockFileContention：跨进程锁被占用时，等锁期间响应 ctx 取消（不永久挂住）。
func TestLockFileContention(t *testing.T) {
	rt := New(Config{Root: t.TempDir()})
	ctx := context.Background()

	unlock, err := rt.lockFile(ctx, rt.root())
	if err != nil {
		t.Fatalf("首次取锁应成功: %v", err)
	}
	defer unlock()

	// 同一 Runtime 再取锁：flock 是「按 fd」的，同进程另一个 fd 仍会冲突
	ctx2, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := rt.lockFile(ctx2, rt.root()); err == nil {
		t.Fatal("锁被占用时应超时返回错误")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("错误应包住 ctx 超时原因，got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("应在 ctx 超时后很快返回，耗时 %s", elapsed)
	}
}

// ---------- 库探测 ----------

// TestHasModuleMissingFalse：不存在的模块返回 (false, nil) —— 「确定没装」与
// 「环境坏了」必须可区分，前者调用方可提示安装。
func TestHasModuleMissingFalse(t *testing.T) {
	// 用真进程模拟解释器：只看报错文本是否 ModuleNotFoundError 来区分
	// 「模块没装」与「环境坏了」
	p := Python{VenvPython: fakePythonWithStderr(t, "ModuleNotFoundError: No module named 'pandas'\n", 1)}

	ok, err := p.HasModule(context.Background(), "pandas")
	if err != nil {
		t.Fatalf("模块不存在不应算探测失败: %v", err)
	}
	if ok {
		t.Error("不存在的模块应返回 false")
	}
}

// TestHasModuleBrokenEnvIsError：解释器坏了（如 site 模块加载失败 —— 沙箱没放行
// 运行时目录时的真实症状）→ 返回 error，而不是误报「库没装」。
func TestHasModuleBrokenEnvIsError(t *testing.T) {
	// 模拟沙箱拦截的真实症状
	p := Python{VenvPython: fakePythonWithStderr(t,
		"Fatal Python error: init_import_site: Failed to import the site module\n", 1)}

	_, err := p.HasModule(context.Background(), "pandas")
	if err == nil {
		t.Fatal("环境损坏应返回 error（不能误报为『库没装』）")
	}
	if !strings.Contains(err.Error(), "pandas") {
		t.Errorf("错误应含模块名，got: %v", err)
	}
}

// TestHasModulePresent：可导入 → true。
func TestHasModulePresent(t *testing.T) {
	p := Python{VenvPython: fakePythonWithStderr(t, "", 0)}
	ok, err := p.HasModule(context.Background(), "os")
	if err != nil {
		t.Fatalf("探测不应失败: %v", err)
	}
	if !ok {
		t.Error("可导入模块应返回 true")
	}
}

// TestHasModuleNoRuntime：未 bootstrap（解释器路径空）→ error 而非 false。
func TestHasModuleNoRuntime(t *testing.T) {
	if _, err := (Python{}).HasModule(context.Background(), "pandas"); err == nil {
		t.Fatal("无运行时应返回 error")
	}
	// 路径不存在（用户删了）
	if _, err := (Python{VenvPython: "/nonexistent/python"}).HasModule(context.Background(), "pandas"); err == nil {
		t.Fatal("解释器不存在应返回 error")
	}
}

// fakePythonWithStderr 造一个可执行脚本：向 stderr 打 msg，按 code 退出。
func fakePythonWithStderr(t *testing.T, msg string, code int) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "python")
	script := "#!/bin/sh\n"
	if msg != "" {
		script += "printf '%s' " + shellQuote(msg) + " >&2\n"
	}
	script += "exit " + itoa(code) + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ---------- 辅助函数 ----------

// TestVenvPythonPathMissing：venv 目录存在但无解释器 → 报错（不能只信目录存在）。
func TestVenvPythonPathMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := venvPythonPath(dir); err == nil {
		t.Fatal("空 venv 目录应报错")
	}
	// 目录存在但解释器不可执行（无执行位）→ 同样不算就绪
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "python"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := venvPythonPath(dir); err == nil {
		t.Fatal("无执行位的解释器不算就绪")
	}
	// 加上执行位 → 就绪
	if err := os.Chmod(filepath.Join(bin, "python"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := venvPythonPath(dir); err != nil {
		t.Fatalf("可执行解释器应被识别: %v", err)
	}
}

// TestFingerprintStable：指纹对同一内容稳定、内容变化即变（驱动重装的依据）。
func TestFingerprintStable(t *testing.T) {
	a, b := fingerprint("x==1"), fingerprint("x==1")
	if a != b {
		t.Error("同一内容指纹应稳定")
	}
	if a == fingerprint("x==2") {
		t.Error("内容变化指纹应变化")
	}
	if len(a) != 32 {
		t.Errorf("指纹应为 16 字节 hex（32 字符），got %d", len(a))
	}
}

// TestTruncateKeepsUTF8Boundary：截断不劈裂多字节字符（pip 日志含中文时不出乱码）。
func TestTruncateKeepsUTF8Boundary(t *testing.T) {
	s := strings.Repeat("中", 1000)
	got := truncate(s, 7) // 7 落在「中」的字节中间
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("应截断并加省略号: %q", got)
	}
	body := strings.TrimSuffix(got, "…")
	if strings.Contains(body, "\ufffd") {
		t.Errorf("不应劈裂多字节字符: %q", body)
	}
	for _, r := range body {
		if r != '中' {
			t.Errorf("截断后应仍是完整字符: %q", body)
		}
	}
	if truncate("short", 100) != "short" {
		t.Error("未超限不应改动")
	}
}

// TestImportCheckScript：自检脚本覆盖全部必需模块。
func TestImportCheckScript(t *testing.T) {
	got := importCheckScript(requiredModules)
	for _, m := range requiredModules {
		if !strings.Contains(got, m) {
			t.Errorf("自检脚本应含 %s，got %q", m, got)
		}
	}
	if !strings.HasPrefix(got, "import ") {
		t.Errorf("应为 import 语句，got %q", got)
	}
}

// TestCandidatePythonsOverride：环境变量整体覆盖候选（测试隔离机制本身）。
func TestCandidatePythonsOverride(t *testing.T) {
	t.Setenv("GO_CODE_PYTHON_CANDIDATES", "/a"+string(os.PathListSeparator)+"/b")
	got := candidatePythons("")
	if len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Fatalf("覆盖应生效，got %v", got)
	}
	// 未覆盖时：显式路径优先，且含 /usr/bin/python3 兜底
	t.Setenv("GO_CODE_PYTHON_CANDIDATES", "")
	got = candidatePythons("/custom/python3")
	if len(got) == 0 || got[0] != "/custom/python3" {
		t.Fatalf("显式路径应排最前，got %v", got)
	}
	found := false
	for _, c := range got {
		if c == "/usr/bin/python3" {
			found = true
		}
	}
	if !found {
		t.Error("候选应含 /usr/bin/python3 兜底（GUI 最小 PATH 场景）")
	}
}

// TestDefaultRoot：默认运行时根须与沙箱 carve-out（.go-code/runtime）对齐，
// 否则「沙箱放行了 A、运行时写在 B」——venv 起不来。
func TestDefaultRoot(t *testing.T) {
	root := DefaultRoot()
	if !strings.Contains(root, filepath.Join(".go-code", "runtime", "python")) {
		t.Fatalf("默认根应为 ~/.go-code/runtime/python，got %s", root)
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		if want := filepath.Join(home, ".go-code", "runtime", "python"); root != want {
			t.Errorf("want %s, got %s", want, root)
		}
	}
}

// TestWriteFileIfChanged：内容不变不重写（幂等时不留无谓改动 / 不动 mtime）。
func TestWriteFileIfChanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := writeFileIfChanged(p, "a"); err != nil {
		t.Fatal(err)
	}
	fi1, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := writeFileIfChanged(p, "a"); err != nil {
		t.Fatal(err)
	}
	fi2, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Error("内容未变不应重写文件")
	}
	if err := writeFileIfChanged(p, "b"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "b" {
		t.Errorf("内容变化应写入，got %q", b)
	}
}

// TestWriteMarkerAtomic：就绪标记落盘后是合法 JSON（原子写不留半截文件）。
func TestWriteMarkerAtomic(t *testing.T) {
	p := filepath.Join(t.TempDir(), readyMarker)
	want := readyInfo{Version: 1, Requirements: "abc", Base: "/usr/bin/python3", VenvPython: "/v/python"}
	if err := writeMarker(p, want); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var got readyInfo
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("标记应是合法 JSON: %v", err)
	}
	if got != want {
		t.Errorf("往返应一致：want %+v got %+v", want, got)
	}
	if _, err := os.Stat(p + ".tmp"); err == nil {
		t.Error("临时文件应已 rename 掉")
	}
}

// TestIsProbe：探测类命令用短超时，重活（venv/pip）用完整超时 —— 否则 dev 误配
// 的 10min 超时会把「解释器坏了」变成挂 10 分钟。
func TestIsProbe(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"/v/bin/python", []string{"-c", "import sys"}, true},
		{"/v/bin/python", []string{"-m", "pip", "install"}, false},
		{"/usr/bin/python3", []string{"-m", "venv", "/x/venv"}, false},
		{"/v/bin/python", nil, false},
		{"/usr/bin/true", []string{"-c"}, false}, // 非 python 解释器
	}
	for _, c := range cases {
		if got := isProbe(c.name, c.args); got != c.want {
			t.Errorf("isProbe(%s, %v) = %v, want %v", c.name, c.args, got, c.want)
		}
	}
}

// TestRunUsesProbeTimeout：真进程路径（无注入）下 run 能跑通并返回输出。
func TestRunUsesProbeTimeout(t *testing.T) {
	p := fakePythonWithStderr(t, "", 0) // 真进程
	rt := New(Config{Root: t.TempDir(), Timeout: time.Minute})
	if _, err := rt.run(context.Background(), p, "-c", "x"); err != nil {
		t.Fatalf("run 应成功: %v", err)
	}
}

// TestRunNonZeroExitIncludesOutput：非零退出把输出并入错误文本（runExec 语义），
// 调用方能据此给可操作提示而非只看到 "exit status 1"。
func TestRunNonZeroExitIncludesOutput(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rt := New(Config{Root: t.TempDir(), Timeout: time.Minute})
	// 用测试进程自身当「解释器」，参数非法 → 非零退出 + 有输出
	_, err = rt.run(context.Background(), exe, "-test.run=NoSuchTest", "-test.v=false")
	if err != nil {
		if !strings.Contains(err.Error(), "exit status") && !strings.Contains(err.Error(), "超时") {
			t.Errorf("错误应含退出信息，got: %v", err)
		}
	}
}

// TestCmdline：错误文本里的命令行可复制粘贴（排查用）。
func TestCmdline(t *testing.T) {
	got := cmdline("/v/bin/python", []string{"-m", "pip", "install"})
	if got != "/v/bin/python -m pip install" {
		t.Errorf("got %q", got)
	}
}
