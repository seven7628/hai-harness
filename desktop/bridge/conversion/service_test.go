package conversion

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeConverter 离线假转换器：计数 / 观察并发 / 可控失败 / 可控阻塞。
// 所有测试都不碰 Python（真正需要受管运行时的那条路由 tools/builtin 的慢测试覆盖）。
type fakeConverter struct {
	calls atomic.Int32
	cur   atomic.Int32
	maxIn atomic.Int32
	block chan struct{} // 非 nil：每次调用在此阻塞，模拟「慢转换占着额度」
	err   error         // 非 nil：恒失败
	body  string        // 产物内容（空 = 默认）
	mu    sync.Mutex
	dirs  []string // 每次调用建的临时目录（测试断言 Service 调了 Cleanup）
}

func (f *fakeConverter) Convert(ctx context.Context, src, _ string) (Output, error) {
	f.calls.Add(1)
	cur := f.cur.Add(1)
	for {
		old := f.maxIn.Load()
		if cur <= old || f.maxIn.CompareAndSwap(old, cur) {
			break
		}
	}
	defer f.cur.Add(-1)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return Output{}, ctx.Err()
		}
	}
	if f.err != nil {
		return Output{}, f.err
	}
	dir, err := os.MkdirTemp("", "conv-fake-")
	if err != nil {
		return Output{}, err
	}
	f.mu.Lock()
	f.dirs = append(f.dirs, dir)
	f.mu.Unlock()
	p := filepath.Join(dir, indexName)
	body := f.body
	if body == "" {
		body = "<html><body>fake</body></html>"
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		return Output{}, err
	}
	return Output{HTMLPath: p, Cleanup: func() { _ = os.RemoveAll(dir) }}, nil
}

// service 构造一个用假转换器、把缓存关进临时目录的服务。overrides 可选。
func testService(t *testing.T, conv Converter, opts Options) *Service {
	t.Helper()
	if opts.CacheDir == "" {
		opts.CacheDir = t.TempDir()
	}
	opts.Converters = map[string]Converter{"xlsx": conv, "docx": conv, "pptx": conv}
	svc, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

// writeSrc 造一个输入文件（内容可控、名字可控）。
func writeSrc(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// ①缓存命中：同一文件两次 → 第二次不调 converter（且产物路径一致、转换器临时目录已回收）。
func TestCacheHitSkipsConverter(t *testing.T) {
	fake := &fakeConverter{}
	svc := testService(t, fake, Options{})
	src := writeSrc(t, t.TempDir(), "book.xlsx", "content-abc")

	first, err := svc.Convert(context.Background(), Request{Path: src, Kind: "xlsx"})
	if err != nil {
		t.Fatalf("第一次 Convert: %v", err)
	}
	if first.Cached {
		t.Fatal("第一次不该是缓存命中")
	}
	second, err := svc.Convert(context.Background(), Request{Path: src, Kind: "xlsx"})
	if err != nil {
		t.Fatalf("第二次 Convert: %v", err)
	}
	if got := fake.calls.Load(); got != 1 {
		t.Fatalf("converter 调用次数 = %d，期望 1（第二次必须命中缓存）", got)
	}
	if !second.Cached {
		t.Fatal("第二次应标记 Cached=true")
	}
	if second.HTMLPath != first.HTMLPath {
		t.Fatalf("产物路径不一致：%q vs %q", first.HTMLPath, second.HTMLPath)
	}
	if _, err := os.Stat(second.HTMLPath); err != nil {
		t.Fatalf("产物文件不存在: %v", err)
	}
	// 转换器自己的临时目录必须被回收（产物已在缓存里，留着就是双倍占用）
	fake.mu.Lock()
	dirs := append([]string(nil), fake.dirs...)
	fake.mu.Unlock()
	for _, d := range dirs {
		if _, err := os.Stat(d); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("转换器临时目录未被回收: %s", d)
		}
	}
	// 口径要求「命中直接返回（<50ms）」。这里放宽到 200ms：断言的对象是「没有重转」，
	// 不是调度器抖动（同包并发 + CI 负载下 50ms 会偶发误报）。
	if second.Took > 200*time.Millisecond {
		t.Fatalf("命中耗时 %v 过长（疑似重转）", second.Took)
	}
	t.Logf("命中耗时 %v（转换耗时 %v）", second.Took, first.Took)
}

// ②too_large：>100 MiB 直接拒（用 Truncate 造稀疏文件，不真写 100 MiB）。
func TestTooLargeRejectedWithoutConversion(t *testing.T) {
	fake := &fakeConverter{}
	svc := testService(t, fake, Options{})
	dir := t.TempDir()
	src := filepath.Join(dir, "huge.pptx")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(DefaultMaxInputBytes + 1); err != nil {
		f.Close()
		t.Skipf("本机不支持稀疏文件: %v", err)
	}
	f.Close()

	_, err = svc.Convert(context.Background(), Request{Path: src, Kind: "pptx"})
	if err == nil {
		t.Fatal("超限文件应被拒绝")
	}
	if code := CodeOf(err); code != CodeTooLarge {
		t.Fatalf("错误码 = %q，期望 %q（err=%v）", code, CodeTooLarge, err)
	}
	if !strings.Contains(err.Error(), "文件过大") {
		t.Fatalf("错误文案缺少「文件过大」: %v", err)
	}
	if got := fake.calls.Load(); got != 0 {
		t.Fatalf("超限文件不该进转换器，调用次数 = %d", got)
	}
	// 缓存目录里也不该留下任何条目
	if ents, _ := os.ReadDir(svc.CacheDir()); len(ents) != 0 {
		t.Fatalf("超限请求写了缓存: %v", ents)
	}
}

// ③失败不缓存：converter 连续失败两次 → 调用计数 2（不是 1）。
func TestFailureIsNotCached(t *testing.T) {
	fake := &fakeConverter{err: errors.New("转换失败: Traceback ... ModuleNotFoundError: No module named 'mammoth'")}
	svc := testService(t, fake, Options{})
	src := writeSrc(t, t.TempDir(), "spec.docx", "doc-bytes")

	for i := 0; i < 2; i++ {
		_, err := svc.Convert(context.Background(), Request{Path: src, Kind: "docx"})
		if err == nil {
			t.Fatalf("第 %d 次应失败", i+1)
		}
		if code := CodeOf(err); code != CodeFailed {
			t.Fatalf("第 %d 次错误码 = %q，期望 %q", i+1, code, CodeFailed)
		}
		if !strings.Contains(err.Error(), "ModuleNotFoundError") {
			t.Fatalf("错误文本丢了 stderr 关键行: %v", err)
		}
	}
	if got := fake.calls.Load(); got != 2 {
		t.Fatalf("失败被缓存了：调用次数 = %d，期望 2", got)
	}
	if ents, err := os.ReadDir(svc.CacheDir()); err == nil && len(ents) != 0 {
		t.Fatalf("失败路径写了缓存条目: %v", ents)
	}
}

// ④并发限 2：起 5 个并发，最大同时在跑 = 2。
func TestConcurrencyLimitIsTwo(t *testing.T) {
	release := make(chan struct{})
	fake := &fakeConverter{block: release}
	svc := testService(t, fake, Options{Concurrency: 2})
	dir := t.TempDir()
	// 5 个不同内容 → 5 个不同键，不会互相命中
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		p := writeSrc(t, dir, fmt.Sprintf("f%d.xlsx", i), fmt.Sprintf("body-%d", i))
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			_, err := svc.Convert(context.Background(), Request{Path: p, Kind: "xlsx"})
			errs <- err
		}(p)
	}
	// 等到 2 个已经在跑
	waitFor(t, func() bool { return fake.calls.Load() == 2 })
	time.Sleep(150 * time.Millisecond) // 给「本不该启动的第三个」一个启动机会
	if got := fake.calls.Load(); got != 2 {
		t.Fatalf("同时在跑 = %d，期望恰好 2（信号量没起作用）", got)
	}
	if got := fake.maxIn.Load(); got != 2 {
		t.Fatalf("观察到的最大并发 = %d，期望 2", got)
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发转换出错: %v", err)
		}
	}
	if got := fake.maxIn.Load(); got != 2 {
		t.Fatalf("全程最大并发 = %d，期望 2", got)
	}
	if got := fake.calls.Load(); got != 5 {
		t.Fatalf("调用次数 = %d，期望 5", got)
	}
}

// ⑤60s 超时：假转换器起一个**独立进程组**的子进程并等它（与 sandbox.runExec 同语义：
// ctx 到点 → kill 整个进程组）。断言 ctx 被取消、且那个进程组真的没了。
func TestTimeoutCancelsAndKillsProcessGroup(t *testing.T) {
	kc := &groupKiller{pid: make(chan int, 1), ctxErr: make(chan error, 1)}
	svc := testService(t, kc, Options{Timeout: 200 * time.Millisecond})

	src := writeSrc(t, t.TempDir(), "slow.xlsx", "slow")
	start := time.Now()
	_, err := svc.Convert(context.Background(), Request{Path: src, Kind: "xlsx"})
	if err == nil {
		t.Fatal("超时应报错")
	}
	if code := CodeOf(err); code != CodeTimeout {
		t.Fatalf("错误码 = %q，期望 %q（err=%v）", code, CodeTimeout, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("超时没有及时生效：耗时 %v", took)
	}
	// 转换器看到的 ctx 必须是被取消的（它是进程组能被杀的唯一原因）
	select {
	case cerr := <-kc.ctxErr:
		if !errors.Is(cerr, context.DeadlineExceeded) {
			t.Fatalf("转换器看到的 ctx.Err() = %v，期望 DeadlineExceeded", cerr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("转换器没观察到 ctx 取消")
	}
	// 进程组已被杀：pid 不再存在（kill(pid, 0) → ESRCH）
	pid := <-kc.pid
	waitFor(t, func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("子进程 %d 仍存活（err=%v）—— 进程组没被杀", pid, err)
	}
}

// ⑥同一内容不同路径 → 命中同一缓存键（内容寻址的意义）。
func TestSameContentDifferentPathSharesCacheKey(t *testing.T) {
	fake := &fakeConverter{}
	svc := testService(t, fake, Options{})
	const body = "same-workbook-bytes"
	a := writeSrc(t, t.TempDir(), "周报.xlsx", body)
	b := writeSrc(t, t.TempDir(), "周报.xlsx", body) // 不同目录、同名、同内容

	ra, err := svc.Convert(context.Background(), Request{Path: a, Kind: "xlsx", Workspace: "/ws/a"})
	if err != nil {
		t.Fatal(err)
	}
	rb, err := svc.Convert(context.Background(), Request{Path: b, Kind: "xlsx", Workspace: "/ws/b"})
	if err != nil {
		t.Fatal(err)
	}
	if got := fake.calls.Load(); got != 1 {
		t.Fatalf("同内容不同路径调用了 %d 次，期望 1（内容寻址失效）", got)
	}
	if ra.Key != rb.Key || ra.HTMLPath != rb.HTMLPath {
		t.Fatalf("缓存键/产物不一致：%q/%q vs %q/%q", ra.Key, ra.HTMLPath, rb.Key, rb.HTMLPath)
	}
	// 反向：同内容**换文件名**必须是另一个键 —— 产物里写着文件名（<title>/<h1>），
	// 混用会让「另存为」后的预览标题停在旧名字上。
	c := writeSrc(t, t.TempDir(), "月报.xlsx", body)
	rc, err := svc.Convert(context.Background(), Request{Path: c, Kind: "xlsx"})
	if err != nil {
		t.Fatal(err)
	}
	if rc.Key == ra.Key {
		t.Fatal("换文件名后键没变：缓存会返回写着旧文件名的产物")
	}
	if got := fake.calls.Load(); got != 2 {
		t.Fatalf("换文件名后应重转一次，调用次数 = %d", got)
	}
	// 参数进指纹：参数不同 → 不同键
	rd, err := svc.Convert(context.Background(), Request{Path: a, Kind: "xlsx", Params: map[string]string{"dpi": "144"}})
	if err != nil {
		t.Fatal(err)
	}
	if rd.Key == ra.Key {
		t.Fatal("参数不同却没有改变缓存键")
	}
}

// groupKiller 起一个独立进程组的子进程并等它；ctx 到点就 kill 整个进程组。
// 这就是 sandbox.runExec 对「超时/取消」的处理方式（exec.Command + Setpgid +
// kill(-pgid)），此处复刻以便在**离线**环境里断言「Service 的时限真的落到了进程上」。
type groupKiller struct {
	pid    chan int
	ctxErr chan error
}

func (g *groupKiller) Convert(ctx context.Context, _, _ string) (Output, error) {
	cmd := exec.Command("sleep", "30") // 不用 CommandContext：要显式验证组杀
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return Output{}, err
	}
	g.pid <- cmd.Process.Pid
	killed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-killed:
		}
	}()
	err := cmd.Wait()
	close(killed)
	g.ctxErr <- ctx.Err()
	if err != nil {
		return Output{}, fmt.Errorf("子进程被终止: %w", err)
	}
	return Output{}, errors.New("子进程本不该正常退出")
}

// waitFor 轮询等待条件成立（最多 5s）。
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}
