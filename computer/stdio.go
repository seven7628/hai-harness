package computer

// stdio.go：Go 侧 stdio NDJSON 客户端（协议 v1）——拉起/管理 helper 子进程，
// 请求/响应按 id 关联，按命令面暴露类型安全方法。Swift helper（或未来
// Windows/Linux helper）以同协议被本客户端驱动。
//
// M0：hello / perm_status / screen_info / snapshot 四命令 + 关闭。
// P0 起补 press/click/type/key/verify/find/open_app/activate 等（§6.6 表）。
//
// 健康守卫（§3.6）：启动即 hello 握手（校验 major）；每次调用失败/进程退出
// 允许一次重启重试（对齐 wrapper 懒探活与 transport-closed 重试思想）。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// DefaultHelperTimeout 单次命令缺省超时（防 helper 卡死，§3.3 ToolTimeout 对应）。
const DefaultHelperTimeout = 10 * time.Second

// StdioConfig 客户端配置。
type StdioConfig struct {
	// HelperPath helper 可执行文件路径（必填）。
	HelperPath string
	// Timeout 单次命令超时（默认 DefaultHelperTimeout；<=0 用默认）。
	Timeout time.Duration
	// Env 追加传给 helper 的环境变量（可选；默认继承 os.Environ）。
	Env []string
}

// StdioClient 一个 helper 子进程上的 NDJSON 客户端。非并发安全：调用方
// （Executor/工具）全局串行执行桌面动作（§6.7），无需内部锁请求面。
type StdioClient struct {
	cfg     StdioConfig
	cmd     *exec.Cmd
	in      io.WriteCloser // helper stdin
	sc      *bufio.Scanner // helper stdout（按行）
	mu      sync.Mutex     // 保护 write/restart（串行命令 + 重启互斥）
	nextID  int
	closing bool
	closed  chan struct{}
}

// NewStdioClient 建客户端（不拉起进程；首次调用时 Start）。
func NewStdioClient(cfg StdioConfig) *StdioClient {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultHelperTimeout
	}
	return &StdioClient{cfg: cfg, closed: make(chan struct{})}
}

// Start 拉起 helper 子进程并完成 hello 握手（校验 major 兼容）。幂等。
func (c *StdioClient) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil && c.cmd.Process != nil {
		return nil // 已在运行
	}
	return c.startLocked(ctx)
}

// startLocked 实际拉起 + 握手。调用方持锁。
func (c *StdioClient) startLocked(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, c.cfg.HelperPath)
	cmd.Env = append(cmd.Environ(), c.cfg.Env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("computer helper stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("computer helper stdout: %w", err)
	}
	var stderrBuf syncBuffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("computer helper start: %w", err)
	}
	c.cmd = cmd
	c.in = stdin
	c.sc = bufio.NewScanner(stdout)
	c.sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // 快照文本树可达数百 KB

	// hello 握手：校验 major 兼容
	ctx2, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	var hr HelloResult
	if err := c.roundTripLocked(ctx2, "hello", HelloParams{VersionMajor: ProtoMajor, VersionMinor: ProtoMinor}, &hr); err != nil {
		c.killLocked()
		return fmt.Errorf("computer helper hello: %w", err)
	}
	if hr.VersionMajor != ProtoMajor {
		c.killLocked()
		return fmt.Errorf("computer helper 协议不兼容: helper v%d.%d, 客户端 v%d.%d",
			hr.VersionMajor, hr.VersionMinor, ProtoMajor, ProtoMinor)
	}
	return nil
}

// Call 执行一条命令（自动 Start；进程退出时重启一次重试）。
func (c *StdioClient) Call(ctx context.Context, cmd string, params, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == nil || c.cmd.Process == nil {
		if err := c.startLocked(ctx); err != nil {
			return err
		}
	}
	ctx2, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	err := c.roundTripLocked(ctx2, cmd, params, result)
	if err == nil {
		return nil
	}
	// 进程级故障（管道断/helper 崩）→ 重启一次再试；命令级错误（perm/not_found）
	// 不重启（结构化错误直接返回，Executor 转模型可见文本）。
	if isTransportErr(err) {
		c.killLocked()
		if serr := c.startLocked(ctx); serr != nil {
			return fmt.Errorf("%v; helper 重启失败: %w", err, serr)
		}
		return c.roundTripLocked(ctx2, cmd, params, result)
	}
	return err
}

// roundTripLocked 写请求 → 读匹配 id 的响应（跳过 notify）。调用方持锁且
// ctx 已带超时。
func (c *StdioClient) roundTripLocked(ctx context.Context, cmd string, params, result any) error {
	c.nextID++
	id := c.nextID
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("computer params 编码: %w", err)
		}
		rawParams = b
	}
	line, err := json.Marshal(Request{ID: id, Cmd: cmd, Params: rawParams})
	if err != nil {
		return fmt.Errorf("computer request 编码: %w", err)
	}
	if _, err := c.in.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("computer helper 写入: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if !c.sc.Scan() {
			if err := c.sc.Err(); err != nil {
				return fmt.Errorf("computer helper 读取: %w", err)
			}
			return io.EOF // helper 退出（transport 级，触发重启重试）
		}
		var msg json.RawMessage
		if err := json.Unmarshal(c.sc.Bytes(), &msg); err != nil {
			return fmt.Errorf("computer helper 响应解析: %w", err)
		}
		// notify 预留：非 {id,...} 形状（有 notify 键）→ 跳过
		var probe struct {
			Notify string `json:"notify"`
		}
		if json.Unmarshal(msg, &probe) == nil && probe.Notify != "" {
			continue
		}
		var resp Response
		if err := json.Unmarshal(msg, &resp); err != nil {
			return fmt.Errorf("computer helper 响应解析: %w", err)
		}
		if resp.ID != id {
			continue // 乱序/陈旧响应（不应发生，防御跳过）
		}
		if !resp.OK {
			if resp.Error == nil {
				resp.Error = &ProtoError{Code: ErrInternal, Msg: "未知错误"}
			}
			return &HelperError{Code: resp.Error.Code, Msg: resp.Error.Msg}
		}
		if result != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("computer 结果解码: %w", err)
			}
		}
		return nil
	}
}

// HelperError 命令级结构化错误（对应 ProtoError.code）。Executor 据此转
// 模型可见纠错文本（perm_denied → 引导授权；not_found → 提示重新 snapshot）。
type HelperError struct {
	Code string
	Msg  string
}

func (e *HelperError) Error() string {
	return fmt.Sprintf("[%s] %s", e.Code, e.Msg)
}

// IsTransportErr 判断错误是否进程/传输级（可重启重试）。命令级 HelperError 不算。
func isTransportErr(err error) bool {
	if err == nil {
		return false
	}
	var he *HelperError
	if errors.As(err, &he) {
		return false
	}
	return true // io.EOF / 写失败 / ctx 超时（helper 未按时应答）
}

// Close 关闭 helper（SIGTERM 优雅 → 兜底 Kill）。幂等。
func (c *StdioClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return nil
	}
	c.closing = true
	close(c.closed)
	if c.cmd == nil || c.cmd.Process == nil {
		return nil
	}
	return c.killLocked()
}

func (c *StdioClient) killLocked() error {
	proc := c.cmd.Process
	if proc == nil {
		return nil
	}
	_ = proc.Kill()
	_, _ = proc.Wait()
	c.cmd = nil
	c.in = nil
	c.sc = nil
	return nil
}

// syncBuffer 并发安全 stderr 收集（helper 日志；M0 仅保留不读）。
type syncBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b = append(s.b, p...)
	return len(p), nil
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.b)
}

// ---- Backend 适配层：StdioClient → Backend（desktop/bridge 装配端用） ----

// stdioBackend 把 StdioClient 包成 Backend 接口（§6.6：Go 侧薄翻译层，
// 不承载业务逻辑）。M0 只实现前四命令，其余返回 unsupported（P0 补齐）。
type stdioBackend struct {
	cl *StdioClient
}

// NewStdioBackend 用 helper 可执行路径建 Backend（进程懒启动）。
func NewStdioBackend(helperPath string) Backend {
	return &stdioBackend{cl: NewStdioClient(StdioConfig{HelperPath: helperPath})}
}

func (b *stdioBackend) ScreenInfo() (ScreenInfo, error) {
	var si ScreenInfo
	err := b.cl.Call(context.Background(), "screen_info", nil, &si)
	return si, err
}

func (b *stdioBackend) Snapshot(opts SnapshotOpts) (*Snapshot, error) {
	var sn Snapshot
	err := b.cl.Call(context.Background(), "snapshot", opts, &sn)
	if err != nil {
		return nil, err
	}
	return &sn, nil
}

func (b *stdioBackend) PermStatus() (PermStatus, error) {
	var ps PermStatus
	err := b.cl.Call(context.Background(), "perm_status", nil, &ps)
	return ps, err
}

func (b *stdioBackend) Find(query FindQuery) ([]Element, error) {
	var out []Element
	err := b.cl.Call(context.Background(), "find", query, &out)
	return out, err
}
func (b *stdioBackend) Press(uid string) error {
	var res json.RawMessage
	return b.cl.Call(context.Background(), "press", PressParams{UID: uid}, &res)
}
func (b *stdioBackend) PressMenu(app string, path []string) error {
	var res json.RawMessage
	return b.cl.Call(context.Background(), "press", PressParams{App: app, MenuPath: path}, &res)
}
func (b *stdioBackend) Click(pt Point) error {
	var res json.RawMessage
	return b.cl.Call(context.Background(), "click", ClickParams{X: pt.X, Y: pt.Y}, &res)
}
func (b *stdioBackend) TypeText(text string) error {
	var res json.RawMessage
	return b.cl.Call(context.Background(), "type", TypeParams{Text: text}, &res)
}
func (b *stdioBackend) KeyChord(chord string) error {
	var res json.RawMessage
	return b.cl.Call(context.Background(), "key", KeyParams{Keys: chord}, &res)
}
func (b *stdioBackend) OpenApp(name string) error {
	var res json.RawMessage
	return b.cl.Call(context.Background(), "open_app", OpenAppParams{Name: name}, &res)
}
func (b *stdioBackend) Activate(pid int, app, windowTitle string) error {
	var res json.RawMessage
	return b.cl.Call(context.Background(), "activate", ActivateParams{Pid: pid, App: app, WindowTitle: windowTitle}, &res)
}
func (b *stdioBackend) Verify(query VerifyQuery) (bool, string, error) {
	var res VerifyResult
	err := b.cl.Call(context.Background(), "verify", query, &res)
	return res.OK, res.Detail, err
}

// Screenshot 捕获屏幕。实现：**直接 exec 系统 screencapture + sips 压缩**，
// 不经过 helper 的 screenshot 命令——2026-09 真机排障教训：helper 进程（无论
// 裸二进制还是 .app bundle、无论 TCC 授权与否）调 screencapture/ScreenCaptureKit
// 在本机恒得壁纸（窗口内容被屏蔽），而 bridge 进程（Electron 宿主链）exec
// screencapture 正常返回真实内容。故截图职责从 helper 移到本客户端（bridge
// 进程内执行 = 宿主授权链）。跨平台：非 darwin 返回 unsupported（未来适配器
// 各自实现）。
func (b *stdioBackend) Screenshot(region Rect, maxDim int) (*Image, error) {
	if runtime.GOOS != "darwin" {
		return nil, &HelperError{Code: ErrUnsupported, Msg: "screenshot 仅 macOS 支持"}
	}
	dir, err := os.MkdirTemp("", "cu-shot-")
	if err != nil {
		return nil, &HelperError{Code: ErrInternal, Msg: "创建临时目录失败: " + err.Error()}
	}
	defer os.RemoveAll(dir)
	png := filepath.Join(dir, "shot.png")
	jpg := filepath.Join(dir, "shot.jpg")
	// 1) screencapture 全屏（-x 无声音）。**必须经 bash -c 执行**——2026-09 真机
	// 排障：screencapture 的 TCC 按「直接父进程」判定，直接 exec（helper/Go 编译
	// 二进制为父）恒被拒（窗口内容屏蔽成壁纸）；经 /bin/bash -c 派生（bash 为
	// 直接父）则正常继承宿主屏幕录制授权。python/bash 解释器链实测可行。
	if out, err := exec.Command("/bin/bash", "-c", "/usr/sbin/screencapture -x "+shellQuote(png)).CombinedOutput(); err != nil {
		return nil, &HelperError{Code: ErrInternal, Msg: "screencapture 失败: " + err.Error() + " " + string(out)}
	}
	// 2) sips 压缩：最长边 ≤1280 + JPEG q0.7（对齐 helper 原压缩参数——
	// 9MB PNG → ~200-300KB；视觉模型够用且省上下文）
	if out, err := exec.Command("/usr/bin/sips", "-Z", "1280", "-s", "format", "jpeg",
		"-s", "formatOptions", "70", png, "--out", jpg).CombinedOutput(); err != nil {
		return nil, &HelperError{Code: ErrInternal, Msg: "sips 压缩失败: " + err.Error() + " " + string(out)}
	}
	data, err := os.ReadFile(jpg)
	if err != nil {
		return nil, &HelperError{Code: ErrInternal, Msg: "读压缩截图失败: " + err.Error()}
	}
	if len(data) < 100 {
		return nil, &HelperError{Code: ErrInternal, Msg: "截图内容为空（屏幕录制权限未生效？）"}
	}
	return &Image{Format: "jpeg", Data: data}, nil
}

// shellQuote 单引号包裹（bash -c 拼接用；临时路径由 MkdirTemp 生成，无单引号，
// 防御性转义即可）。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
func (b *stdioBackend) Close() error { return b.cl.Close() }
