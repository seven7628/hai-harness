package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// maxOutputLog npm 输出截断上限（错误摘要不灌日志/事件）。
const maxOutputLog = 1024

// copyDir 递归复制 src → dst（含隐藏文件；保留可执行位）。组件包整包同步用。
func copyDir(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("copyDir: %s 不是目录", src)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(s, d); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(s, d, e.Type()&0o111 != 0); err != nil {
			return err
		}
	}
	return nil
}

// copyFile 复制单文件；exec=true 时保留可执行位（0755）。
func copyFile(src, dst string, exec bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	mode := os.FileMode(0o644)
	if exec {
		mode = 0o755
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// copyVendorPackage 从 vendor 包目录复制受管组件包（vendor 内容 = 组件包本体：
// package.json/build/skills/LICENSE/README；不进 node_modules 父层——组件根只放
// 受管元数据与用户状态）。dst 为 <组件根>/node_modules/chrome-devtools-mcp。
// 先清空 dst 再整体复制（vendor 是唯一事实源，防残留旧文件）。
func copyVendorPackage(vendorDir, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return copyDir(vendorDir, dst)
}

// ensureComponentLayout 把组件根归一化为受管布局：根 package.json（私有壳）+
// node_modules/chrome-devtools-mcp 包子树 + .manifest.json。容忍两类 legacy/错误形态：
//   - 组件根缺失 node_modules：则根上散的 package.json/build/skills/LICENSE/README
//     是 npm 原包被平铺进根（vendor 旧结构整目录同步产物）→ 整个根清掉重建；
//     config.json（用户配置）与 .enabled（启用标记）若混在根上则先移出、清完放回
//     （9/5 坏版本同步产物可能有用户态在根上，不可丢）；
//   - 组件根已有 node_modules/chrome-devtools-mcp（正常/legacy npm 安装）→ 仅保证
//     node_modules 父层存在，保留 config.json/.enabled/根壳 package.json 等用户态。
//
// 返回是否需要执行 vendor 同步（true = 包缺失或不受管，应重新同步/迁移）。
func ensureComponentLayout(dir string) (needSync bool, err error) {
	pkgDir := installedPackageDir(dir)
	if dirExists(pkgDir) {
		// 包存在：正常布局。仅确保 node_modules 父层在（防根上散件残余）。
		return false, os.MkdirAll(filepath.Dir(pkgDir), 0o755)
	}
	// 包缺失：若根上是平铺的原包（vendor 旧结构产物），清掉重建（先保用户态）。
	rootPackage := filepath.Join(dir, "package.json")
	if fileExists(rootPackage) && dirExists(filepath.Join(dir, "build")) {
		// stash 到组件目录外（RemoveAll 会清掉目录内的一切，含 .presync）
		stashDir, err := os.MkdirTemp(filepath.Dir(dir), ".browser-presync-*")
		if err != nil {
			return true, err
		}
		defer os.RemoveAll(stashDir)
		for _, keep := range []string{"config.json", enabledMarker} {
			if fileExists(filepath.Join(dir, keep)) {
				if err := os.Rename(filepath.Join(dir, keep), filepath.Join(stashDir, keep)); err != nil {
					return true, err
				}
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			return true, err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return true, err
		}
		for _, keep := range []string{"config.json", enabledMarker} {
			if fileExists(filepath.Join(stashDir, keep)) {
				if err := os.Rename(filepath.Join(stashDir, keep), filepath.Join(dir, keep)); err != nil {
					return true, err
				}
			}
		}
	}
	return true, nil
}

// bootstrapPackageJSON 在组件目录写最小 package.json（npm install 到空目录需要）；
// 已有则跳过（幂等）。2026-09 起组件随应用内置（复制同步），此函数仅 legacy 保留。
func bootstrapPackageJSON(dir string) error {
	p := filepath.Join(dir, "package.json")
	if fileExists(p) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte("{\n  \"name\": \"go-code-plugin-browser\",\n  \"private\": true\n}\n"), 0o644)
}

// cmdWaitDelay 子命令**成功退出后**等待 I/O 管道关闭的上限（C10 同型站点，2026-09-18）。
//
// **为什么必需**：`CombinedOutput()` 内部就是 `cmd.Stdout = &buf`（io.Writer）⇒ Go 建
// `os.Pipe` + 拷贝协程 ⇒ `Wait()` **依赖管道关闭**；而 `exec.CommandContext` 在 ctx 到期时
// **只杀直接子进程**，npm 这类工具 fork 出的后代（生命周期脚本、校验进程等）仍存活并握住
// 写端 ⇒ `Wait()` 永不返回 —— **超时在这里是失效的**（超时管不住一个「进程自己已退出、
// 但管道被后代持有」的等待）。
//
// 这正是 2026-09-18 **C8** 在 `sandbox.runExec` 上修掉的同一缺陷（生产实测挂起 602s）；
// 本处是同型站点，故采用同一处方（与 sandbox.waitDelayAfterKill、runtime/pyWaitDelay 同值）。
//
// 语义：进程退出后最多再等 `cmdWaitDelay`，到点由 `os/exec` 强制关管道并使 `Wait` 返回
// （返回 `exec.ErrWaitDelay`）；该情形下输出可能被进程组外的后代截断，由调用点负责
// **不把成功印成失败**（见 runCmd 的 ErrWaitDelay 分支）。
const cmdWaitDelay = 3 * time.Second

// runCmd 执行命令（默认 exec.CommandContext + CombinedOutput；输出截断）。
// 2026-09 起组件随应用内置（vendor 复制），不再有 npm 安装路径——保留供
// RunCmd 注入默认值与未来任何子进程调用使用。
func runCmd(ctx context.Context, name string, args []string, dir string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	// C10：进程退出后最多再等 cmdWaitDelay 即强制关管道并返回（见 cmdWaitDelay 注释）。
	cmd.WaitDelay = cmdWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		// C10：ErrWaitDelay = 进程**已成功退出**，只是管道被进程组外的后代占着。
		// CombinedOutput 把 `Wait` 的错误**原样返回** ⇒ 不在这里放行，就等于把「罕见挂住」
		// 换成「罕见假失败」——比现状更糟。os/exec 仅在进程成功退出且未走 Cancel 时才返回它。
		//
		// 这里没有「注明输出可能被截断」的位置：本函数签名只回 error，成功路径上 out 本就
		// 被丢弃（输出只在失败分支进错误文本）。关键事实是**命令已成功 ⇒ 不判失败**。
		if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			return nil
		}
		return fmt.Errorf("%s %v: %v\n%s", name, args, err, truncate(string(out), maxOutputLog))
	}
	return nil
}

// truncate 截断长文本（npm 错误输出摘要）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// defaultUserHomeDir 用户主目录（默认组件/配置根目录解析）。
func defaultUserHomeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "~"
}

// isExecutable 判定 path 为可执行文件（存在 + 非目录 + 有执行位）。
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// nodeCandidates 已知 node 可执行文件位置（按确定性/常见度排序）。
// GUI 启动的桌面 App（Electron spawn 的 bridge）继承的是 launchd 的最小 PATH
// （/usr/bin:/bin:/usr/sbin:/sbin），不含用户 shell 配置——npm 全局前缀、Homebrew、
// nvm/fnm/volta 装的 node 都不在 PATH 上。LookPath 找不到不代表用户没装 node，
// 因此 PATH 之外还要查这些「GUI 常见安装位置」：
//   - 官网 pkg（/usr/local）与 Intel/Apple 芯片 Homebrew 前缀
//   - Homebrew/官网安装的 node 自带 npm 全局 bin 目录（npm i -g 产物常在此）
//   - nvm（多版本共存，取版本号最大者）
//   - fnm（symlink 指向实际安装目录）、volta（shim）、MacPorts
//
// GO_CODE_NODE_CANDIDATES（':' 分隔）整体覆盖候选列表——测试隔离用（屏蔽本机
// 真实安装）；正常不设。Windows/Linux 无候选（随 PATH 与记录路径判定）。
func nodeCandidates(home string) []string {
	if ov := os.Getenv("GO_CODE_NODE_CANDIDATES"); ov != "" {
		return strings.Split(ov, string(os.PathListSeparator))
	}
	cs := []string{
		"/opt/homebrew/bin/node",       // Apple Silicon Homebrew
		"/usr/local/bin/node",          // 官网 pkg / Intel Homebrew
		"/opt/local/bin/node",          // MacPorts
		"/usr/local/nvm/versions/node", // nvm 系统级安装根
	}
	if home != "" {
		cs = append(cs,
			home+"/.nvm/versions/node",             // nvm 用户级安装根（多版本目录）
			home+"/.volta/bin/node",                // volta shim
			home+"/.local/share/fnm/node-versions", // fnm 安装根（symlink 在 fnm 目录）
			home+"/.fnm/node-versions",             // fnm legacy 安装根
			home+"/.local/bin/node",                // 用户级 ~/.local/bin（pipx 类布局）
		)
	}
	return cs
}

// findNodeCandidate 在候选目录中定位可执行 node：目录型（nvm/fnm 版本根）取
// 版本号最大者；文件型直接校验可执行。返回可执行 node 绝对路径；找不到返回 ""。
func findNodeCandidate(home string) string {
	for _, c := range nodeCandidates(home) {
		info, err := os.Stat(c)
		if err != nil {
			continue
		}
		if info.IsDir() {
			// 多版本根（nvm/fnm）：子目录名 = 版本号（可带 v 前缀/后缀），
			// bin/node 须可执行；取版本号最大者（不依赖 ls 排序）。
			entries, err := os.ReadDir(c)
			if err != nil {
				continue
			}
			best := ""
			bestVer := []int(nil)
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				p := filepath.Join(c, e.Name(), "bin", "node")
				ver := versionOf(e.Name())
				if fileExists(p) && isExecutable(p) && versionGreater(ver, bestVer) {
					best = p
					bestVer = ver
				}
			}
			if best != "" {
				return best
			}
			continue
		}
		if isExecutable(c) {
			return c
		}
	}
	return ""
}

// nodeHome 用户主目录（供 nodeCandidates 兜底；测试可注入环境变量）。
func nodeHome() string {
	if h := os.Getenv("GO_CODE_NODE_HOME"); h != "" {
		return h
	}
	return defaultUserHomeDir()
}

// versionOf 解析 nvm/fnm 版本目录名的数字版本（"v20.11.1" → 20.11.1、"18" → 18）。
// 不可解析返回 nil（不参与取最新）。用于多版本根取版本号最大者。
func versionOf(name string) []int {
	n := name
	if strings.HasPrefix(n, "v") {
		n = n[1:]
	}
	if n == "" {
		return nil
	}
	var segs []int
	for _, part := range strings.Split(n, ".") {
		if part == "" {
			return nil
		}
		v := 0
		for _, r := range part {
			if r < '0' || r > '9' {
				return nil
			}
			v = v*10 + int(r-'0')
		}
		segs = append(segs, v)
	}
	return segs
}

// versionGreater 逐段比较版本号（"21.7.3" > "8.17.0"——字符串比较会误判）。
// a 不可解析视为小于一切；同为 nil 视为相等。
func versionGreater(a, b []int) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		var av, bv int
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if av != bv {
			return av > bv
		}
	}
	return false
}

// nodeExecutable 解析一个可用 node 的绝对路径：记录路径 → PATH → 候选目录兜底。
// 找不到返回 ""（不报错——调用方决定报错文案与回退）。
func nodeExecutable(recorded string) string {
	// 记录路径可用 → 沿用（LookPath 兼容「PATH 目录名 node」的普通情况）
	if strings.TrimSpace(recorded) != "" {
		if _, err := exec.LookPath(recorded); err == nil {
			return recorded
		}
		if isExecutable(recorded) {
			return recorded
		}
	}
	// PATH 上有 node → 用 PATH 解析的绝对路径（记录路径陈旧时自愈）
	if np, err := exec.LookPath("node"); err == nil {
		return np
	}
	// PATH 上没有（GUI 启动的残缺 PATH）→ 候选目录兜底
	return findNodeCandidate(nodeHome())
}

// defaultNodeCheck 默认 node 可用性检查：记录路径 → PATH → 常见安装目录兜底。
// 记录路径失效（换机 / nvm 切版本 / PATH 重排等）时回退查全局 PATH 与候选目录——
// 用户确实有 node 环境就不应误报缺失（GUI 启动的 App PATH 不含 shell 配置，
// 兜底候选目录是这类「明明装了却报没装」的关键）；全部不可用才报错。
func defaultNodeCheck(nodePath string) error {
	if nodeExecutable(nodePath) == "" {
		return fmt.Errorf("未找到 node，请先安装 Node.js（npm 随 Node 附带）")
	}
	return nil
}

// defaultLookPath 默认可执行文件定位。
func defaultLookPath(name string) (string, error) { return exec.LookPath(name) }
