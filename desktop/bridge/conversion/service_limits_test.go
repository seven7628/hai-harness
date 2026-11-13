package conversion

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// delayConverter 每次转换耗时 delay（可被 ctx 提前打断）。
type delayConverter struct {
	delay time.Duration
	calls atomic.Int32
}

func (d *delayConverter) Convert(ctx context.Context, _, _ string) (Output, error) {
	d.calls.Add(1)
	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
		return Output{}, ctx.Err()
	}
	dir, err := os.MkdirTemp("", "conv-delay-")
	if err != nil {
		return Output{}, err
	}
	p := filepath.Join(dir, indexName)
	if err := os.WriteFile(p, []byte("<html>delayed</html>"), 0o600); err != nil {
		return Output{}, err
	}
	return Output{HTMLPath: p, Cleanup: func() { _ = os.RemoveAll(dir) }}, nil
}

// 排队时间**不计入**单次转换时限：三条请求、并发 2、单条 0.6×timeout，
// 第三条要排到第二轮才开跑（总墙钟 > timeout），但它照样必须成功。
//
// 为什么这是核心语义：反过来做（时限从入队开始算），用户只要前面有两条在跑，
// 「点第三份文件」就必然超时 —— 而它根本还没开始转。这条只有测试能守住。
func TestQueueWaitDoesNotCountTowardTimeout(t *testing.T) {
	conv := &delayConverter{delay: 120 * time.Millisecond}
	svc := testService(t, conv, Options{Concurrency: 2, Timeout: 200 * time.Millisecond})
	dir := t.TempDir()
	var wg sync.WaitGroup
	results := make([]Result, 3)
	errs := make([]error, 3)
	for i := 0; i < 3; i++ {
		p := writeSrc(t, dir, string(rune('a'+i))+".xlsx", "body")
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			results[i], errs[i] = svc.Convert(context.Background(), Request{Path: p, Kind: "xlsx"})
		}(i, p)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 条（排队后开跑）不该失败: %v", i+1, err)
		}
	}
	if last := results[2]; last.Cached {
		t.Fatal("第三条是本轮第一次转换，不可能命中缓存")
	}
	if _, err := os.Stat(results[2].HTMLPath); err != nil {
		t.Fatalf("第三条的产物不存在: %v", err)
	}
	if conv.calls.Load() != 3 {
		t.Fatalf("调用次数 = %d，期望 3", conv.calls.Load())
	}
}

// 真·「排队时间 > timeout」的形态（评审给的构造）：并发 1、单次 100ms、每条跑 60ms，
// 4 条并发 → 第 4 条要排 3×60ms = 180ms 才开始跑，排队时间**超过**了单次时限，
// 但它照样必须成功。上一个用例的排队只有 120ms < 200ms，咬得住「时限从入队开始计」，
// 却没构造出「排队本身比时限还长」这个形态 —— 而这才是排队不该计入时限的极端情形。
//
// QueueTimeout 显式放大到 2s：本用例要断言的是「排队不计入转换时限」，
// 不是「排队上界」（后者由 TestQueueTimeoutIsBounded 负责），别让两者互相干扰。
func TestQueueLongerThanTimeoutStillSucceeds(t *testing.T) {
	conv := &delayConverter{delay: 60 * time.Millisecond}
	svc := testService(t, conv, Options{
		Concurrency:  1,
		Timeout:      100 * time.Millisecond,
		QueueTimeout: 2 * time.Second,
	})
	dir := t.TempDir()
	var wg sync.WaitGroup
	results := make([]Result, 4)
	errs := make([]error, 4)
	start := time.Now()
	for i := 0; i < 4; i++ {
		p := writeSrc(t, dir, string(rune('a'+i))+".xlsx", "body-"+string(rune('a'+i)))
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			results[i], errs[i] = svc.Convert(context.Background(), Request{Path: p, Kind: "xlsx"})
		}(i, p)
	}
	wg.Wait()
	elapsed := time.Since(start)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 条（排队后开跑）不该失败: %v", i+1, err)
		}
	}
	if results[3].Cached {
		t.Fatal("第 4 条是首次转换，不该命中缓存")
	}
	if conv.calls.Load() != 4 {
		t.Fatalf("调用次数 = %d，期望 4", conv.calls.Load())
	}
	// 排队 ≥ 180ms > Timeout(100ms)：整轮实际耗时必须明显超过单次时限，
	// 否则说明这个用例根本没构造出「排队超过时限」的形态。
	if elapsed < 180*time.Millisecond {
		t.Fatalf("整轮只用了 %v：没构造出「排队超过单次时限」的形态", elapsed)
	}
}

// 排队上界：等不到槽位时给明确的 queue_timeout（而不是无限期挂着）。
func TestQueueTimeoutIsBounded(t *testing.T) {
	slow := &delayConverter{delay: 400 * time.Millisecond}
	// 排队上界（40ms）必须明显小于 A 的转换时限（200ms）：否则「A 到点放槽位」与
	// 「B 排队到点」会撞在一起，测出来的就不再是排队上界。
	svc := testService(t, slow, Options{
		Concurrency:  1,
		Timeout:      200 * time.Millisecond,
		QueueTimeout: 40 * time.Millisecond,
	})
	dir := t.TempDir()
	a := writeSrc(t, dir, "a.xlsx", "a")
	b := writeSrc(t, dir, "b.xlsx", "b")

	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = svc.Convert(context.Background(), Request{Path: a, Kind: "xlsx"}) // 占住唯一槽位
	}()
	<-started
	time.Sleep(20 * time.Millisecond) // 让 A 真的进到转换里
	start := time.Now()
	_, err := svc.Convert(context.Background(), Request{Path: b, Kind: "xlsx"})
	if err == nil {
		t.Fatal("排不到槽位应报错")
	}
	if code := CodeOf(err); code != CodeQueueTimeout {
		t.Fatalf("错误码 = %q，期望 %q（err=%v）", code, CodeQueueTimeout, err)
	}
	if took := time.Since(start); took > 150*time.Millisecond {
		t.Fatalf("排队上界没生效：等了 %v（上界 40ms）", took)
	}
}

// 排队期间 ctx 取消 → canceled（不是 queue_timeout，也不是 timeout）：
// 用户主动关掉了工作区，答案应该是「已取消」。
func TestCancelWhileQueued(t *testing.T) {
	slow := &delayConverter{delay: 400 * time.Millisecond}
	svc := testService(t, slow, Options{Concurrency: 1, Timeout: 200 * time.Millisecond})
	dir := t.TempDir()
	a := writeSrc(t, dir, "a.xlsx", "a")
	b := writeSrc(t, dir, "b.xlsx", "b")

	go func() { _, _ = svc.Convert(context.Background(), Request{Path: a, Kind: "xlsx"}) }()
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := svc.Convert(ctx, Request{Path: b, Kind: "xlsx"})
	if code := CodeOf(err); code != CodeCanceled {
		t.Fatalf("错误码 = %q，期望 %q（err=%v）", code, CodeCanceled, err)
	}
}

// 哈希闸门：算内容键要整读文件，必须与转换分离**且**有上界（否则 5 个 100 MiB 请求
// 会同时全量读盘）。用注入的回调把重叠窗口拉大后数峰值。
func TestHashGateBoundsConcurrentHashing(t *testing.T) {
	var cur, peak int32
	var mu sync.Mutex
	hook := func(enter bool) {
		mu.Lock()
		if enter {
			cur++
			if cur > peak {
				peak = cur
			}
		} else {
			cur--
		}
		mu.Unlock()
		if enter {
			time.Sleep(30 * time.Millisecond) // 把「同时在哈希」的窗口撑开
		}
	}
	conv := &fakeConverter{err: errors.New("停了：本用例只关心哈希阶段")}
	svc := testService(t, conv, Options{Concurrency: 2, HashWaitHook: hook})
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		p := writeSrc(t, dir, string(rune('a'+i))+".xlsx", "不同内容-"+string(rune('a'+i)))
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			_, _ = svc.Convert(context.Background(), Request{Path: p, Kind: "xlsx"})
		}(p)
	}
	wg.Wait()
	mu.Lock()
	got := peak
	mu.Unlock()
	if got > 2 {
		t.Fatalf("同时哈希的峰值 = %d，期望 ≤ 2（哈希没进闸门）", got)
	}
	if got < 2 {
		t.Fatalf("同时哈希的峰值 = %d：闸门把并行度压到 1 了（应允许 2）", got)
	}
}

// 清理契约：转换器返回错误时，Service **也必须**调它的 Cleanup。
//
// 为什么：错误路径正是最容易留下垃圾的路径（转换超时后产物可能已经写了一半）。
// 三个 previewer 适配器自己内部会清（它们的 defer 覆盖失败路径），但接口契约不能
// 靠「恰好每个实现都自己清了」—— 这层收着 Cleanup 就得用。
func TestCleanupRunsOnErrorPath(t *testing.T) {
	dir := t.TempDir()
	conv := &cleanupOnErrorConverter{dir: dir}
	svc := testService(t, conv, Options{})
	src := writeSrc(t, t.TempDir(), "x.xlsx", "x")
	if _, err := svc.Convert(context.Background(), Request{Path: src, Kind: "xlsx"}); err == nil {
		t.Fatal("转换器返回了错误，Service 不该成功")
	}
	select {
	case <-conv.cleaned:
	default:
		t.Fatal("错误路径没有调用转换器的 Cleanup（临时目录留在了磁盘上）")
	}
}

type cleanupOnErrorConverter struct {
	dir     string
	cleaned chan struct{}
	once    sync.Once
}

func (c *cleanupOnErrorConverter) Convert(context.Context, string, string) (Output, error) {
	if c.cleaned == nil {
		c.cleaned = make(chan struct{})
	}
	// 产物存在但转换失败（超时后写了一半的真实形态）
	p := filepath.Join(c.dir, "half.html")
	_ = os.WriteFile(p, []byte("half"), 0o600)
	return Output{HTMLPath: p, Cleanup: func() { c.once.Do(func() { close(c.cleaned) }) }},
		errors.New("转换失败: 模拟超时后的半个产物")
}

// 产物落盘权限 0600：缓存里装的是用户文档内容（可能敏感），同机他人不该直读。
func TestCachedArtifactModeIs0600(t *testing.T) {
	conv := &fakeConverter{}
	svc := testService(t, conv, Options{})
	src := writeSrc(t, t.TempDir(), "secret.xlsx", "confidential")
	res, err := svc.Convert(context.Background(), Request{Path: src, Kind: "xlsx"})
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(res.HTMLPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("产物 mode = %v，期望 0600", st.Mode().Perm())
	}
	// 条目目录必须是 0700（路径本身也不给同机他人枚举）
	if d, err := os.Stat(filepath.Dir(res.HTMLPath)); err != nil || d.Mode().Perm() != 0o700 {
		t.Fatalf("条目目录 mode = %v（err=%v），期望 0700", d.Mode().Perm(), err)
	}
	// meta.json 也要 0600：它记的是源文件名（文档名本身就是信息）
	meta := filepath.Join(filepath.Dir(res.HTMLPath), metaName)
	if st, err := os.Stat(meta); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("meta.json mode = %v（err=%v），期望 0600", st.Mode().Perm(), err)
	}
}

// Sweep 与 Put 必须互斥（评审指出的窄窗口）。
//
// 危险交错：Sweep 读到某条目目录是 25h 前（快照），此时 Put 正往那个 key 里写新产物，
// Sweep 按快照 RemoveAll —— 用户拿到一个刚被删掉的产物路径（面板白屏）。
//
// 断言方式选「互斥」而不是「拼时序」：在 Put 的写入过程中另起一个 goroutine 跑 Sweep，
// 它必须**等 Put 结束**才能完成。不互斥的话 Sweep 在几十微秒内就跑完了（小目录），
// 这里的 50ms 窗口足以抓到它。
func TestSweepDoesNotRunInsidePut(t *testing.T) {
	dir := t.TempDir()
	cache := NewCache(dir, DefaultMaxCacheBytes, DefaultMaxCacheAge)
	entry := makeEntry(t, dir, "k1", 8, time.Now().Add(-25*time.Hour)) // 25h 前：Sweep 会想删它

	var sweepDone atomic.Bool
	var once sync.Once
	payload := strings.NewReader("<html>fresh</html>")
	reader := readerFunc(func(p []byte) (int, error) {
		once.Do(func() {
			go func() {
				_, _ = cache.Sweep()
				sweepDone.Store(true)
			}()
			time.Sleep(50 * time.Millisecond) // 不互斥的话，Sweep 早就跑完了
		})
		return payload.Read(p)
	})

	p, err := cache.Put("k1", reader, Meta{Kind: "xlsx", Name: "a.xlsx"})
	if err != nil {
		t.Fatal(err)
	}
	if sweepDone.Load() {
		t.Fatal("Sweep 与 Put 并发跑完了（没互斥）：刚写好的条目可能被当成过期条目删掉")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !sweepDone.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !sweepDone.Load() {
		t.Fatal("Put 结束后 Sweep 没有完成（死锁？）")
	}
	// 最终状态：条目还在，内容是新写的（mtime 已刷新 → 不算过期）
	if b, err := os.ReadFile(p); err != nil || string(b) != "<html>fresh</html>" {
		t.Fatalf("最终产物不对: %q err=%v", b, err)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("条目被清扫删掉了: %v", err)
	}
}

// readerFunc 把函数适配成 io.Reader。
type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// 缓存键里的「转换器版本」位确实来自脚本指纹（不是摆设）。
func TestCacheKeyUsesScriptFingerprint(t *testing.T) {
	src := writeSrc(t, t.TempDir(), "a.xlsx", "bytes")
	before, err := CacheKey("xlsx", "a.xlsx", nil, src)
	if err != nil {
		t.Fatal(err)
	}
	saved := keyScriptFingerprint
	keyScriptFingerprint = "0000000000000000"
	defer func() { keyScriptFingerprint = saved }()
	after, err := CacheKey("xlsx", "a.xlsx", nil, src)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("脚本指纹没进缓存键：改了脚本仍会命中旧产物")
	}
}
