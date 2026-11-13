package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/desktop/bridge/conversion"
)

// blockingConverter 在飞不返回的假转换器（离线；不需要 Python）。
// 记录被取消多少次、被调用多少次。
type blockingConverter struct {
	calls    atomic.Int32
	canceled atomic.Int32
	started  chan struct{}
	once     sync.Once
}

func newBlockingConverter() *blockingConverter {
	return &blockingConverter{started: make(chan struct{}, 8)}
}

func (c *blockingConverter) Convert(ctx context.Context, _, _ string) (conversion.Output, error) {
	c.calls.Add(1)
	c.started <- struct{}{}
	go func() {
		<-ctx.Done()
		c.canceled.Add(1)
	}()
	<-ctx.Done()
	return conversion.Output{}, ctx.Err()
}

// convertSvc 用假转换器造一个转换服务（缓存进临时目录）。
func convertSvc(t *testing.T, conv conversion.Converter) *conversion.Service {
	t.Helper()
	svc, err := conversion.New(conversion.Options{
		CacheDir:   t.TempDir(),
		Converters: map[string]conversion.Converter{"xlsx": conv, "docx": conv, "pptx": conv},
	})
	if err != nil {
		t.Fatalf("conversion.New: %v", err)
	}
	return svc
}

// waitResponse 轮询等待某个 id 的 command_response。
func waitResponse(t *testing.T, buf *syncBuf, id int) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range readAllLines(buf) {
			if l["event_type"] == "command_response" && intNum(l, "id", -1) == id {
				return l
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等不到命令响应 id=%d", id)
	return nil
}

// 取消表本身：登记 → 按工作区取消 → ctx 关闭；收尾幂等；workspace 之间互不影响。
func TestPreviewCancelsRegistry(t *testing.T) {
	var p previewCancels
	ctxA, doneA := p.begin("/ws/a")
	ctxB, doneB := p.begin("/ws/b")
	defer doneB()
	if p.count() != 2 {
		t.Fatalf("登记后 count = %d，期望 2", p.count())
	}
	p.cancelWorkspace("/ws/a")
	select {
	case <-ctxA.Done():
	case <-time.After(time.Second):
		t.Fatal("cancelWorkspace 没有取消该工作区的 ctx")
	}
	if err := ctxB.Err(); err != nil {
		t.Fatalf("取消 A 波及了 B: %v", err)
	}
	doneA() // 幂等：已被 cancelWorkspace 取消过，也要能安全收尾
	doneA()
	if p.count() != 1 {
		t.Fatalf("收尾后 count = %d，期望 1", p.count())
	}
}

// 接线断言：预览命令必须用**可取消**的 ctx 调转换服务，且**工作区关闭**（真入口
// `m.shutdownWorkspace`）能真的把它取消。
//
// 这条测试的存在理由：上一版传的是 context.Background()，service 里的取消分支
// 生产里永不触发（死码）。而上一版的测试又是直接调 `m.previewCancels.cancelWorkspace`——
// 那验的是取消表本身（另一个等价物），不是「shutdownWorkspace 接上了取消表」。
// 所以这里必须走 shutdownWorkspace，并且工作区要真的注册（否则它第一行就返回）。
func TestPreviewConvertCancelOnWorkspaceClose(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "book.xlsx"), []byte("PK\x03\x04 not a workbook"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, buf := testManager(t)
	m.runtime(ws) // 注册工作区：shutdownWorkspace 对未注册路径会直接返回
	fake := newBlockingConverter()
	m.conv = convertSvc(t, fake)

	m.dispatch(cmd(7, "xlsx_preview", map[string]any{"workspace": ws, "path": "book.xlsx"}))
	select {
	case <-fake.started:
	case <-time.After(5 * time.Second):
		t.Fatal("转换没有开始")
	}
	// 真入口：工作区关闭 = 用户不再需要这个转换
	m.shutdownWorkspace(ws)
	resp := waitResponse(t, buf, 7)
	if resp["ok"] != false {
		t.Fatalf("取消后应回 ok=false: %v", resp)
	}
	if resp["code"] != conversion.CodeCanceled {
		t.Fatalf("错误码 = %v，期望 %q", resp["code"], conversion.CodeCanceled)
	}
	if fake.canceled.Load() == 0 {
		t.Fatal("转换器没观察到 ctx 取消 —— 命令侧又传了不可取消的 ctx")
	}
	if n := m.previewCancels.count(); n != 0 {
		t.Fatalf("收尾后取消表还留着 %d 条（泄漏）", n)
	}
}

// 决策钉住：**同一个文件**被两个消费者同时请求时，不许互相取消。
//
// 两个合法消费者：预览面板（loadOfficePreview 读回 HTML）与原生视图
// （previewOfficeFile 开浏览器标签）。按目标取消会让其中之一随机报错 —— 这是
// preview_cancels.go 里明确放弃的取舍，必须由测试守住，否则以后有人"顺手优化"成
// 「新请求取消旧请求」，坏的是用户看得见的那条路。
func TestPreviewSameTargetIsNotCanceledByNewRequest(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "book.xlsx"), []byte("PK\x03\x04 not a workbook"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := testManager(t)
	fake := newBlockingConverter()
	m.conv = convertSvc(t, fake)

	m.dispatch(cmd(11, "xlsx_preview", map[string]any{"workspace": ws, "path": "book.xlsx"}))
	m.dispatch(cmd(12, "xlsx_preview", map[string]any{"workspace": ws, "path": "book.xlsx"}))
	for i := 0; i < 2; i++ {
		select {
		case <-fake.started:
		case <-time.After(5 * time.Second):
			t.Fatalf("第 %d 个请求没有开始", i+1)
		}
	}
	time.Sleep(150 * time.Millisecond) // 给「新请求取消旧请求」一个发作窗口
	if n := fake.canceled.Load(); n != 0 {
		t.Fatalf("%d 个同目标请求被互相取消了（面板/原生视图会随机报错）", n)
	}
	m.previewCancels.cancelWorkspace(ws) // 收尾，别把 goroutine 留在测试进程里
}

// previewsFixture 造一份「预览产物缓存」：$HOME/.go-code/previews/<key>/index.html。
// （testManager 已把 HOME 指向临时目录。）
func previewsFixture(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(home, ".go-code", "previews", "abcdef")
	if err := os.MkdirAll(entry, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(entry, "index.html")
	if err := os.WriteFile(p, []byte("<html>cached</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// 只读放行的两条实测（R08a 安全面）：
//  1. 预览面板的读回通路（file_preview 绝对路径）必须仍然成功 —— 功能不能受影响；
//  2. 编辑器保存（file_save）必须被拒 —— 缓存条目可写就等于伪造预览。
func TestPreviewCacheIsReadableButNotSaveable(t *testing.T) {
	// 顺序不能反：testManager 会把 HOME 指向临时目录，fixture 必须建在**那个** HOME 下
	//（反过来会把产物写进真实 ~/.go-code，测试还会因为「不在新 HOME 的 .go-code 里」而漏判）。
	m, buf := testManager(t)
	p := previewsFixture(t)

	m.dispatch(cmd(21, "file_preview", map[string]any{"path": p}))
	resp := waitResponse(t, buf, 21)
	if resp["ok"] != true {
		t.Fatalf("file_preview 读不回缓存产物（面板会白屏）: %v", resp)
	}
	if d, ok := resp["data"].(map[string]any); !ok || !strings.Contains(str(d, "content"), "<html>cached</html>") {
		t.Fatalf("读回的内容不对: %v", resp["data"])
	}

	m.dispatch(cmd(22, "file_save", map[string]any{"path": p, "content": "PWNED", "allowNew": true}))
	resp = waitResponse(t, buf, 22)
	if resp["ok"] != false {
		t.Fatalf("file_save 写进了预览缓存: %v", resp)
	}
	if code := str(resp, "code"); code != "denied" {
		t.Fatalf("错误码 = %q，期望 denied", code)
	}
	if b, _ := os.ReadFile(p); string(b) != "<html>cached</html>" {
		t.Fatalf("缓存内容被改写: %q", b)
	}
}

// delete_workspace 是**另一个**关闭入口：它先把 workspace 从表里摘掉，再走清理 ——
// 所以 shutdownWorkspace（第一行就检查「是否注册」）在它的路径上根本不会执行。
// 这条测试钉住 deleteWorkspace 自己那一次 cancelWorkspace。
func TestDeleteWorkspaceCancelsPreviews(t *testing.T) {
	ws := t.TempDir()
	m, _ := testManager(t)
	m.runtime(ws) // 注册（deleteWorkspace 会摘除并清理它的元数据）
	ctx, done := m.previewCancels.begin(ws)
	defer done()
	if err := ctx.Err(); err != nil {
		t.Fatalf("begin 后不该已被取消: %v", err)
	}

	m.deleteWorkspace(ws)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("deleteWorkspace 没有取消在飞预览（摘除后 shutdownWorkspace 会提前返回）")
	}
}

// 启动清扫必须走 Service 自己的 Cache 实例（Review：另建 conversion.NewCache
// = 两把 Cache.mu = 「Sweep 与 Put 互斥」在生产里是空操作）。
//
// 断言方式：Service 记录自己被执行过几次清扫。新建一个同目录的 Cache 不会让这个
// 计数 +1 —— 于是「换回 NewCache 另建一份」会被这条测试直接抓住。
func TestStartupSweepUsesServiceCache(t *testing.T) {
	m, _ := testManager(t)
	svc, err := m.previewService(m.pythonRuntime())
	if err != nil {
		t.Fatalf("previewService: %v", err)
	}
	before := svc.Sweeps()
	// 造一条 25h 前的残留：清扫要真的作用在 Service 的缓存目录上
	dir := filepath.Join(svc.CacheDir(), "stale-entry")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}

	m.sweepPreviewCache() // 启动路径真正调用的那一个函数

	if got := svc.Sweeps(); got != before+1 {
		t.Fatalf("Service.Sweeps = %d，期望 %d：清扫没走 Service 的 Cache 实例（另建了一份）", got, before+1)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("25h 残留没被清掉（清扫没作用在 Service 的缓存目录上）: %v", err)
	}
}
