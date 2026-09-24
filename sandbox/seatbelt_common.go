package sandbox

import (
	"path/filepath"
	"strings"
)

// defaultSensitivePaths 敏感路径默认清单（相对 home）：沙箱内强制 deny 读写。
//
// 编码场景采用 allow-by-default：home、SSH、Git credential helper、云 SDK、
// 包管理器缓存和 Keychain 默认可用，避免沙箱阻断正常开发工作流（git push、
// go mod、npm、cargo、云 CLI 等）。这里只保护 go-code 自身的配置和密钥；
// 用户若需要完全宿主等价行为，可通过 sandbox.mode=none 关闭 Seatbelt。
var defaultSensitivePaths = []string{
	".go-code", // go-code 配置：settings.json（可能含 api_key）/ keys.json
}

// defaultOpenSubpaths 敏感 deny 中的受管放行子树（相对 home，SBPL 后规则胜 ——
// 策略里置于敏感 deny 之后精确重开）。两个子树都是「受管 + 只含公开内容」：
//
//   - .go-code/plugins：受管插件目录安装 Browser Use 等组件
//     （chrome-devtools-mcp 的 node_modules / config / profile），安装、调试与日志
//     排查都要求沙箱内可读写，否则 MCP Browser Use 不可用。
//   - .go-code/runtime：受管 Python 运行时（venv + requirements.lock，见 runtime 包）。
//     不放行则沙箱内跑 venv python 直接崩（2026-09 实测）：
//     `Fatal Python error: init_import_site: Failed to import the site module`
//     —— .go-code 的 deny 罩住了运行时目录，site 模块加载失败。写权限同样必需
//     （解释器要写 __pycache__）。该目录只含解释器与 PyPI 公开库，无任何密钥。
//
// 注意：文件工具侧（tools/builtin.denyHarnessConfigPath 及其同语义副本）必须同步
// 放行同样的子树，否则「沙箱能跑但文件工具读写不了」两边行为不一致。
// .go-code 其余（settings.json 明文密钥 / sessions / events 历史）仍保持 deny 读写。
var defaultOpenSubpaths = []string{
	".go-code/plugins",
	".go-code/runtime",
}

// deviceReadPaths / deviceWritePaths 常规 shell 运行必需设备白名单（2026-08-28 误伤修复）。
// 实测：bash 进程替换 <(...) 读 /dev/fd/N —— brew.sh 等脚本依赖，缺省默认全拒会直接
// EPERM（`brew --version` 都跑不起来）；/dev/stdin|out|err 是 /dev/fd/* 的符号链接，
// subpath /dev/fd 一并覆盖。/dev/null、urandom/random（openssl/python os.urandom 等）、
// zero、tty 为解释器/工具链常见依赖，虽由 system.sb 兜底放行，仍显式声明（自文档化、
// 防 system.sb 版本漂移）。只放行白名单设备，不放开 /dev 全局（防裸盘 /dev/disk* 读取外泄）。
var deviceReadPaths = []string{
	"/dev/fd",
	"/dev/null",
	"/dev/urandom",
	"/dev/random",
	"/dev/zero",
	"/dev/tty",
}

var deviceWritePaths = []string{
	"/dev/fd", // 写 /dev/fd/1|2 = stdout/stderr（如 `echo x > /dev/stderr`）
	"/dev/null",
	"/dev/tty",
}

// systemReadPaths macOS 系统/工具链运行必需路径：bash/python/git/npm 解释器+动态库、
// CA 证书/时区、homebrew 工具。只读（file-read*）；写权限仅限 workspace + 临时目录。
// brew/conda 路径各用户不同 → 策略运行时注入，不硬编码；toolchain 兼容优先（用户决策：
// 不做二进制白名单）。
var systemReadPaths = []string{
	"/System/Library",
	"/System/Volumes/Preboot/Cryptexes/App/System",
	"/Library",
	"/usr",
	"/bin",
	"/sbin",
	"/opt/homebrew",
	"/opt/local",
	"/Applications",
	// /var /tmp /etc 是 /private/* 符号链接；内核按 syscall 传入的字面路径检查，
	// 故双份放行（程序可能传任一种形态）。
	"/private/tmp",
	"/private/var",
	"/private/etc",
	"/private/var/run",
	"/tmp",
	"/var",
	"/etc",
	"/Volumes",
}

// Seatbelt macOS sandbox-exec（Seatbelt）执行后端。
//
// 强制访问控制，非虚拟机/非 chroot：只拦截本进程（及其后代）的系统调用，
// 进程仍跑在本机用户与本机内核；主 Agent 进程不进沙箱，只罩 shell 命令。
type Seatbelt struct {
	Workspace string   // 允许读写的根
	Home      string   // 允许读；WriteHome=true 时允许写（用户级包安装：pip/npm/go/cargo）
	Sensitive []string // 强制 deny file-read*/file-write* 的绝对路径（默认 = home 下 defaultSensitivePaths + options）
	HomeWrite bool     // 允许写入 home（用户级包安装放行）；敏感路径仍 deny-write 兜底
	// OpenSubpaths 敏感 deny 中的放行子树（绝对路径，策略里置于 deny 之后重开）：
	// 默认 = home 下 defaultOpenSubpaths（受管插件目录 + 受管运行时目录）。用户经
	// WithSensitivePaths 显式 deny 命中放行子树（相等或父目录）时剔除 —— 显式 deny
	// 意图优先。
	OpenSubpaths []string
	Policy       string // 编译好的 SBPL 策略（NewSeatbelt 构造时生成并探测）

	noProbe       bool     // WithNoProbe：跳过构造期探测（测试/CI）
	userSensitive []string // WithSensitivePaths 注入的显式敏感路径（相对 home 或绝对；构造期归一）
}

// SeatbeltOption 构造选项。
type SeatbeltOption func(*Seatbelt)

// WithSensitivePaths 追加敏感 deny 路径（绝对路径，或相对 home 的子路径）。
// 显式 deny 命中默认放行子树（defaultOpenSubpaths）时该放行被剔除。
func WithSensitivePaths(paths []string) SeatbeltOption {
	return func(s *Seatbelt) {
		for _, p := range paths {
			if p != "" {
				s.userSensitive = append(s.userSensitive, p)
			}
		}
	}
}

// WithHomeWrite 允许写入 home 目录（用户级包安装：pip user-site、npm/nvm、~/go、
// ~/.cargo、~/.gem 等）。风险权衡：home 可写后，持久化安全交给
//  1. bash 静态高危命令拦截（rm -rf 家目录/根、sudo、磁盘操作、curl|sh 等）
//  2. Sensitive 路径强制 deny 读+写（.ssh/.aws/.go-code 等私钥/凭证目录不受 home 放宽影响）
//  3. WithRelease 放行门（命中拦截时需用户确认才绕过沙箱重跑）
func WithHomeWrite() SeatbeltOption {
	return func(s *Seatbelt) { s.HomeWrite = true }
}

// WithNoProbe 跳过构造期策略探测（测试/CI；生产保留探测防 SIGABRT 静默）。
func WithNoProbe() SeatbeltOption {
	return func(s *Seatbelt) { s.noProbe = true }
}

// resolveHome 解析为绝对路径：相对路径按 home 展开。
func resolveHome(home, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(home, p)
}

// resolveRealPath 归一化到真实路径（EvalSymlinks）。seatbelt 按 VFS 解析后的真实
// vnode 路径检查过滤条件 —— /var fold → /private/var，字面符号链接路径匹配不上。
// 路径不存在时回退字面（如新工作区尚未建目录、.kube 尚未存在）。
func resolveRealPath(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

// seatbeltBlockMarkers 沙箱拦截标记。Seatbelt 拒绝访问返回 EPERM(1)，与普通程序
// 错误码一样 —— 只能靠 stderr 关键字区分「业务代码报错」还是「被沙箱拦截」。
// 中英文都收录：中文 macOS 把 EPERM 本地化为「操作不被允许」「权限被拒绝」，
// 只匹配英文会漏判 → 放行门不触发 → 命令静默失败（实测 hdiutil 报中文即如此）。
var seatbeltBlockMarkers = []string{
	"Operation not permitted",
	"deny file-",
	"deny network-",
	"操作不被允许",
	"权限被拒绝",
}

// IsSeatbeltBlocked 探测输出是否含沙箱拦截标记。
func IsSeatbeltBlocked(out string) bool {
	for _, m := range seatbeltBlockMarkers {
		if strings.Contains(out, m) {
			return true
		}
	}
	return false
}
