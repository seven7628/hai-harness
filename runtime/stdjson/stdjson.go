// Package stdjson 长驻子进程的 NDJSON(stdio) 协议骨架 —— 从 computer/stdio.go
// 抽出、被 computer 与 codemode 的 execproc 共同依赖的**最薄一层**。
//
// # 为什么只抽「解码」不抽「连接」
//
// 两条使用方对「谁来 spawn 子进程」的答案不同，且不可统一：
//
//   - computer.StdioClient 自己 exec helper 二进制（macOS 桌面 helper 不能经
//     sandbox 的 sh -c + 环境白名单跑，它要的就是宿主完整环境与 TCC 授权链）；
//   - execproc.Executor **必须**经 sandbox.Sandbox 拉起（否则丢掉隔离与那三层
//     kill 预算，见 sandbox/sandbox.go 文件头 C8 事故说明）。
//
// 强行统一会让任一方拿到自己不该有的 spawn 能力（computer 绕过沙箱，或
// execproc 绕过 kill 预算），所以连接管理各留一份，共用的只有「一行一个 JSON、
// 怎么解码、怎么判版本不兼容」——这部分一旦各写一遍就会各自漂移（v 字段名、
// 4MB 上限、major 比较方向），而它恰是协议两端最容易各自演进的地方。
//
// # 与 computer 的关系
//
// 本包**不 import computer**，computer 也不 import 本包的连接管理；computer 侧
// 的 StdioClient 保持原样（行为零变化，见 stdjson_test.go 里的对齐说明）。Phase 1
// 的取舍是「新件先立起来、execproc 用起来」，是否让 computer 切过来留给后续 ——
// computer 是在产线上的桌面自动化链路，改它的进程管理风险高于收益，而 stdjson
// 的解码层本来就是**新增**代码，漂移风险为零。
package stdjson

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxLine 单行 JSON 字节上限（与 computer/stdio.go 的 4MB 同值）。
//
// 为什么不调小：computer 侧的快照文本树可达数百 KB，而 codemode 的脚本结果
// 一行可达整段输出（out 字段）；调小会让「本来能跑通的大结果」退化成读失败，
// 而这类失败在模型看来是「工具报错了」——排查成本远高于多占几 MB 常驻内存。
// 为什么不用 Scanner 默认的 64KB：那是本仓两个使用方都会踩的坑。
const MaxLine = 4 << 20

// scanner 初始缓冲（与 computer 同值：64KB 起，MaxLine 封顶）。
const initialBuf = 64 << 10

// Version 协议版本握手载荷（hello 请求/响应共用形状）。
//
// 为什么只有 major/minor 两个字段、不做能力协商：与 computer/stdio.go:95-107
// 同口径 —— major 不兼容即拒（协议形状变了），minor 差异向后兼容（只加命令）。
// Phase 1 的 execproc 沿用这条规则，避免现在就发明一套协商机制（无消费者的
// 协商 = 永远配错的协商）。
type Version struct {
	Major int `json:"version_major"`
	Minor int `json:"version_minor"`
	// Runtime 自报运行时标识（node / bun / fake-helper 等）。握手时回给宿主，
	// 让错误文本能说清「到底是哪个解释器在应答」。
	Runtime string `json:"runtime,omitempty"`
}

// Hello 握手请求：宿主 → 子进程，声明自己支持的协议版本。
type Hello struct {
	Version Version `json:"version"`
}

// HelloResult 握手应答：子进程 → 宿主，回自己的协议版本。
type HelloResult struct {
	Version Version `json:"version"`
}

// CheckHello 校验握手应答与宿主侧 major 是否兼容；不兼容时返回**给模型看的**
// 可读原因（宿主与子进程来自同一次构建时不该发生，但它确实可能发生：用户
// 升级了 Node 大版本、旧版 helper 还留在磁盘上）。
//
// 错误文案刻意写明**双方版本**：模型看到「沙箱协议不兼容」时无从自我修正，
// 看到「沙箱端 v2.x / 解释器端 v1.y」才知道该做什么（本工具不可用，别重试）。
func CheckHello(got HelloResult, want Version) error {
	if got.Version.Major == want.Major {
		return nil
	}
	rt := got.Version.Runtime
	if rt == "" {
		rt = "sandbox runtime"
	}
	return fmt.Errorf("sandbox protocol mismatch: host speaks v%d.%d but the %s speaks v%d.%d "+
		"(incompatible major version — this build cannot drive this runtime; "+
		"the codemode tool is unavailable, do not retry)",
		want.Major, want.Minor, rt, got.Version.Major, got.Version.Minor)
}

// Reader 逐行读取子进程 stdout 并解码为 JSON 行。
//
// 用 bufio.Scanner 而非 bufio.Reader.ReadString：Scanner 自带缓冲上限（本包
// MaxLine），超限即显式报错；而 ReadString 在失控子进程持续输出时会无界增长
// —— 那正是「4MB 上限」当初被引入要防的事（见 MaxLine 注释）。
type Reader struct {
	sc  *bufio.Scanner
	err error
}

// NewReader 用 r 建行解码器（不预读；第一次 Next/Decode 时才读）。
func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, initialBuf), MaxLine)
	return &Reader{sc: sc}
}

// Next 推进到下一行并把该行解码进 v。
//
// 返回值：
//   - nil：成功读到一个 JSON 行（含 JSON null → 此时 v 保持零值）；
//   - io.EOF：子进程正常写完并关闭 stdout（**不是**故障，由调用方结合退出码判断）；
//   - 其他 error：传输级故障（行超限、JSON 损坏、管道断）。
//
// **扫描错误会被记忆**（sticky）：Scanner 出错后行为未定义，后续 Next 一律返回
// 同一条错误。这与 computer/stdio.go 的行为一致（那里靠调用方在出错后重启进程、
// 不再复用同一个 Scanner）；显式记忆是把它从「隐式约定」变成「调用方可以查到的
// 事实」，因为 codemode 侧要在超时路径上分辨「没读到结果」与「读到了但超限」。
func (r *Reader) Next(v any) error {
	if r.err != nil {
		return r.err
	}
	if !r.sc.Scan() {
		if err := r.sc.Err(); err != nil {
			r.err = err
			return err
		}
		r.err = io.EOF
		return io.EOF
	}
	// 空行容忍：子进程在 hello 前打了一行启动 banner（helper/解释器常这样）
	// 不该让整次执行失败 —— 协议只要求「能解出一行 JSON」。
	for {
		line := r.sc.Bytes()
		if len(line) > 0 {
			if err := json.Unmarshal(line, v); err != nil {
				// 非 JSON 也可能是子进程写了日志到 stdout —— 但流已不同步，
				// 不能继续读下去，统一当传输级错误交给调用方重启/放弃。
				r.err = fmt.Errorf("decode sandbox line: %w", err)
				return r.err
			}
			return nil
		}
		if !r.scanMore() {
			return io.EOF
		}
	}
}

// scanMore 推进到下一行；false = 读完/出错（错误已记入 r.err）。
func (r *Reader) scanMore() bool {
	if !r.sc.Scan() {
		if err := r.sc.Err(); err != nil {
			r.err = err
			return false
		}
		r.err = io.EOF
		return false
	}
	return true
}

// Err 返回记忆下来的传输错误（尚未出错时返回 nil）。
//
// 用途：超时路径上调用方先等 ctx，再问「到底是没读到结果（Err()==nil）还是
// 读崩了（Err()!=nil）」—— 两种情况的给模型文案不同（前者是脚本没跑完，后者
// 是沙箱通信坏了，建议重试）。
func (r *Reader) Err() error { return r.err }

// LineTooLongErr 行超长的判别（给模型的可读文案要用）。
//
// 判 errors.Is(err, bufio.ErrTooLong) 而非比对错误串：各 Go 版本文案不同
// （1.24 是 "token too long"，更早是 "too long"），比串会随工具链升级失效。
func LineTooLongErr(err error) bool { return errors.Is(err, bufio.ErrTooLong) }
