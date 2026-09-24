//go:build darwin

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// seatbeltBin sandbox-exec：普通用户可用，无需 root，不受 SIP 开关影响。
// （macOS 10.15 起 Apple 标记为 deprecated，但系统仍完整支持，见 docs/SEATBELT_SANDBOX.md）
const seatbeltBin = "sandbox-exec"

// probeTimeout 构造期策略探测时限（/bin/true，正常 <10ms）。
const probeTimeout = 5 * time.Second

// Available 探测 sandbox-exec 是否可用（非 darwin 恒 false）。
func Available() bool {
	_, err := exec.LookPath(seatbeltBin)
	return err == nil
}

// NewSeatbelt 创建 macOS Seatbelt 执行后端。
//
// workspace 为允许读写的根；策略运行时动态生成（注入 workspace/home/敏感路径，
// 不硬编码用户名）。构造末尾用目标策略跑一次 /usr/bin/true 探测 —— 策略初始化
// 失败（最小策略缺 mach-lookup 等会静默 SIGABRT exit 134）立即返回错误，装配方
// 降级 NoSandbox + 可见警告，而非每条命令静默 134。
func NewSeatbelt(workspace string, opts ...SeatbeltOption) (*Seatbelt, error) {
	if !Available() {
		return nil, fmt.Errorf("%s: 未找到（Seatbelt 沙箱为 macOS 专有能力）", seatbeltBin)
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("workspace %q: %w", workspace, err)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, fmt.Errorf("home 解析失败: %v", err)
	}
	home = resolveRealPath(home)
	s := &Seatbelt{Home: filepath.Clean(home), Workspace: resolveRealPath(filepath.Clean(abs))}
	for _, p := range defaultSensitivePaths {
		s.Sensitive = append(s.Sensitive, filepath.Join(home, p))
	}
	for _, o := range opts {
		o(s)
	}
	// 用户显式敏感路径（WithSensitivePaths）并入统一清单，与默认路径一起归一化。
	var explicitSensitive []string // 仅用户显式（剔除放行子树只用显式，默认 deny 不覆盖默认 carve-out）
	for _, p := range s.userSensitive {
		resolved := resolveRealPath(resolveHome(s.Home, p))
		s.Sensitive = append(s.Sensitive, resolved)
		explicitSensitive = append(explicitSensitive, resolved)
	}
	// 敏感 deny 路径统一归一化为真实路径（seatbelt 按 VFS 解析后的真实 vnode 路径
	// 检查，/var → /private/var 的字面符号链接路径匹配不上——实测）。
	for i := range s.Sensitive {
		s.Sensitive[i] = resolveRealPath(s.Sensitive[i])
	}
	// 受管放行子树（默认 ~/.go-code/plugins）：真实路径归一化（同 deny 的 VFS 语义）；
	// 用户显式 deny 命中放行子树（相等或其父目录）→ 剔除该放行（显式 deny 意图优先）。
	// 注意：默认敏感路径（.go-code）是 plugins 的父目录，但它是「默认 deny 的 carve-out」
	// 而非「用户显式拒绝」——不参与剔除（否则默认放行恒被删，Browser Use 不可用）。
	for _, p := range defaultOpenSubpaths {
		s.OpenSubpaths = append(s.OpenSubpaths, resolveRealPath(resolveHome(s.Home, p)))
	}
	s.OpenSubpaths = filterExplicitDenied(s.OpenSubpaths, explicitSensitive)
	s.Policy = s.buildPolicy()
	if !s.noProbe {
		if err := probePolicy(s.Policy); err != nil {
			return nil, fmt.Errorf("seatbelt 策略探测失败: %w", err)
		}
	}
	return s, nil
}

// probePolicy 用目标策略跑一次 /usr/bin/true：非零退出/SIGABRT/被拦均为策略异常。
// 注意不要用 /bin/true —— 新版 macOS 已移除该二进制（仅 /usr/bin/true 存在）。
func probePolicy(policy string) error {
	cmd := exec.Command(seatbeltBin, "-p", policy, "/usr/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	if err := cmd.Start(); err != nil {
		return err
	}
	// 与 runExec 同一套上界（见 sandbox.go 中 waitDelayAfterKill 的注释）：探测命令输出
	// 走 os.DevNull（无管道），WaitDelay 在这里只是便宜的一致性兜底；真正兜底是下面
	// awaitExit 的预算 —— 旧代码 `_ = syscall.Kill(...)` 吞错 + `<-done` 无上界：
	// kill 没杀到（错被吞掉）时探测会永久挂住。
	cmd.WaitDelay = waitDelayAfterKill
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("探测命令非零退出（策略初始化失败?）: %v", err)
		}
		return nil
	case <-cctx.Done():
		firstErr := killProcGroup(cmd.Process.Pid)
		o := awaitExit(done, killWaitBudget, killEscalationWait, func() error {
			return escalateKill(cmd.Process.Pid, cmd.Process)
		})
		note := killErrNote(firstErr, o.EscErr)
		if o.Failed {
			return fmt.Errorf("探测超时: %v；%s%s（已带部分结论返回，不再等待）", cctx.Err(), killFailedMark, note)
		}
		return fmt.Errorf("探测超时: %v%s", cctx.Err(), note)
	}
}

// buildPolicy 生成 SBPL 策略。节点顺序关键：
//   - (deny default) 默认全拒，再按需 allow（最小安全原则，反向写会漏放行）；
//   - import "system.sb" 补齐系统运行基线；缺 mach-lookup/ipc-posix-shm/
//     system-socket → 进程初始化 SIGABRT（实测）；
//   - allow 系统读 / home 读 / workspace 读写之后，Sensitive deny 收尾
//     （deny-after-allow 生效，实测：home 的 allow 不覆盖其下敏感子目录）。
func (s *Seatbelt) buildPolicy() string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(import \"system.sb\")\n")
	b.WriteString("(deny default)\n")
	// 基础能力：Shell 要 fork/exec 大量子命令（git/npm/python/cargo），不放开全挂。
	// 不做二进制白名单（brew/conda 路径各异，兼容性税过高）——用户决策，靠文件+网络约束。
	b.WriteString("(allow process-fork)\n")
	b.WriteString("(allow process-exec)\n")
	b.WriteString("(allow process-info*)\n")
	b.WriteString("(allow sysctl-read)\n")
	b.WriteString("(allow file-read-metadata)\n")
	b.WriteString("(allow signal)\n")
	// mach/IPC/系统套接字：进程初始化必需（缺失 → SIGABRT，实测）。
	b.WriteString("(allow mach-lookup)\n")
	b.WriteString("(allow ipc-posix-shm)\n")
	b.WriteString("(allow system-socket)\n")
	// 网络：v1 放开（用户决策）——保住 npm install / go mod download / git fetch / curl；
	// 外泄防护由敏感路径 deny-read + bash 静态高危拦截兜底。域名白名单需代理（二期）。
	b.WriteString("(allow network-outbound)\n")
	// 本地监听/绑定：M2 CDP 多路转发器需在本地起 ws server（bridge 监听 127.0.0.1）。
	// 仅放行 bind+inbound（入站监听），不放行通用 network（双向）。转发器须在代码显式绑 127.0.0.1。
	// 注：seatbelt 无 network-listen 操作（实测 unbound variable），入站监听用 network-inbound。
	b.WriteString("(allow network-bind)\n")
	b.WriteString("(allow network-inbound)\n")
	// 系统/工具链只读（bash/python/git/npm 解释器、动态库、证书、homebrew）。
	for _, p := range systemReadPaths {
		fmt.Fprintf(&b, "(allow file-read* (subpath %q))\n", p)
	}
	// 设备白名单（误伤修复 2026-08-28）：/dev/fd 进程替换（brew 等脚本）、
	// /dev/null、urandom/random、zero、tty。/dev/fd 用 subpath（/dev/stdin|out|err
	// 符号链接到 /dev/fd/* 一并覆盖），其余用 literal 精确放行。
	for _, p := range deviceReadPaths {
		if p == "/dev/fd" {
			fmt.Fprintf(&b, "(allow file-read* (subpath %q))\n", p)
		} else {
			fmt.Fprintf(&b, "(allow file-read* (literal %q))\n", p)
		}
	}
	for _, p := range deviceWritePaths {
		if p == "/dev/fd" {
			fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", p)
		} else {
			fmt.Fprintf(&b, "(allow file-write* (literal %q))\n", p)
		}
	}
	// home 允许读（Agent 浏览用户文件）。WriteHome=true 时允许写 home——
	// 放行用户级包安装（pip/npm/go/cargo），否则命令写不到已装包目录。
	// 写权限（workspace 之外）默认仅 home（可选）+ 临时目录。
	fmt.Fprintf(&b, "(allow file-read* (subpath %q))\n", s.Home)
	if s.HomeWrite {
		fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", s.Home)
	}
	fmt.Fprintf(&b, "(allow file-read* (subpath %q))\n", s.Workspace)
	fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", s.Workspace)
	b.WriteString("(allow file-write* (subpath \"/private/tmp\"))\n")
	b.WriteString("(allow file-write* (subpath \"/tmp\"))\n")
	// 敏感路径强制 deny 读+写（放最后：deny-after-allow 生效，home 宽 allow 不覆盖）。
	// 注意：home 可写后若只 deny 读、漏 deny 写，凭据目录仍可被写（污染/植入）——
	// 故统一 deny 读写。
	for _, p := range s.Sensitive {
		fmt.Fprintf(&b, "(deny file-read* (subpath %q))\n", p)
		fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", p)
	}
	// 受管放行子树（默认 ~/.go-code/plugins 与 ~/.go-code/runtime）：置于敏感 deny
	// 之后 —— SBPL 后规则胜，精确重开这两个子树（Browser Use 等受管组件的
	// node_modules/config/profile 安装调试，以及受管 Python 运行时的 venv 执行 +
	// __pycache__ 写入），.go-code 其余（settings.json 密钥 / sessions / events）仍 deny。
	// 用户显式敏感路径命中放行子树时已在构造期剔除（显式 deny 意图优先）。
	for _, p := range s.OpenSubpaths {
		fmt.Fprintf(&b, "(allow file-read* (subpath %q))\n", p)
		fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", p)
	}
	return b.String()
}

// filterExplicitDenied 放行子树被显式敏感路径命中（相等或为其父目录）时剔除 ——
// 用户显式 deny 意图优先于默认 carve-out（如 settings sensitive_paths 配了
// ~/.go-code/plugins 整体拒绝）。
func filterExplicitDenied(open, sensitive []string) []string {
	out := make([]string, 0, len(open))
	for _, o := range open {
		denied := false
		for _, sp := range sensitive {
			if sp == o || strings.HasPrefix(o, sp+string(filepath.Separator)) {
				denied = true
				break
			}
		}
		if !denied {
			out = append(out, o)
		}
	}
	return out
}

// Run 在 sandbox-exec 沙箱内执行 sh -c 命令；语义与 NoSandbox 完全对齐
// （含 ExecSpec.ExitCode/TimedOut/Canceled 结局回填）。
// 策略与命令是独立 argv（Go exec 直传，无中间 shell 引号注入面）；沿用独立进程组
// + 超时杀整组 + 最小环境白名单（isolatedEnv 不泄漏 agent 密钥/token）。
func (s *Seatbelt) Run(ctx context.Context, spec ExecSpec) (string, error) {
	cmd := exec.Command(seatbeltBin, "-p", s.Policy, "sh", "-c", spec.Command)
	cmd.Dir = spec.Cwd
	cmd.Env = isolatedEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 独立进程组
	return runExec(ctx, cmd, spec)
}
