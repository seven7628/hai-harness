package browser

// §1.8.0 兼容补丁：chrome-devtools-mcp 的 McpPage.init → createTargetUniverse
// （DevTools universe 初始化）在 Electron 内嵌 CDP 上挂起 ~12s——McpPage.init 用
// Promise.allSettled 等它落定，new_page/fill 等工具被拖到超时（实测 15-20s）。
// DevTools universe 只服务 lighthouse/performance 工具（wrapper 已排除），
// 给 #initDevToolsUniverseNoThrow 加 2s 超时快速放弃，核心浏览工具不受影响。
//
// patch 目标：<组件包目录>/build/src/McpPage.js（entry 上溯后定位；见 patchDevToolsTimeout）
// 替换：
//   const session = await this.pptrPage.createCDPSession();
//   this.#devtoolsUniverse = await createTargetUniverse(session);
// 为：
//   const session = await this.pptrPage.createCDPSession();
//   this.#devtoolsUniverse = await Promise.race([
//       createTargetUniverse(session),
//       new Promise((_, rej) => setTimeout(() => rej(new Error('DevTools universe init timeout (go-code patch)')), 2000)),
//   ]);
//
// 幂等：已含 "go-code patch" 标记则跳过。失败仅记日志（不阻断 Enable）。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const devToolsPatchMarker = "go-code patch"

// devToolsPatchOld 目标片段（McpPage.js #initDevToolsUniverseNoThrow 的原版两行；
// 与 chrome-devtools-mcp@1.8.0 build/src/McpPage.js L185-186 一致——注意必须用
// **上游原版**（无 Promise.race/无 marker），曾误填为补丁后形态导致匹配失败、
// 补丁静默不生效（9/5 引入回归，测试假绿掩盖）。
const devToolsPatchOld = `            const session = await this.pptrPage.createCDPSession();
            this.#devtoolsUniverse = await createTargetUniverse(session);`

// devToolsPatchNew 替换为带 2s 超时的版本（createCDPSession 也可能挂起）。
const devToolsPatchNew = `            const session = await Promise.race([
                this.pptrPage.createCDPSession(),
                new Promise((_, rej) => setTimeout(() => rej(new Error('DevTools createCDPSession timeout (go-code patch)')), 2000)),
            ]);
            this.#devtoolsUniverse = await Promise.race([
                createTargetUniverse(session),
                new Promise((_, rej) => setTimeout(() => rej(new Error('DevTools universe init timeout (go-code patch)')), 2000)),
            ]);`

// patchDevToolsTimeout 给 chrome-devtools-mcp 打 DevTools init 超时等 Electron 兼容
// 补丁（幂等）。entry 为包内入口 bin 路径（resolveEntry 返回值）；补丁目标是同包
// build/src 下的文件——包根 = 从 entry 目录逐级上溯到含 package.json 处（真实 bin
// 在 build/src/bin/，两层深；测试包 dist/ 一层深，统一用 package.json 定位，不用
// 固定层级）。修正历史 bug：旧实现把 entry 当组件目录拼 node_modules，路径恒不存在
// → 补丁静默永不生效（9/5 重构引入）。
func patchDevToolsTimeout(entry string) {
	base := packageRootOf(entry)
	if base == "" {
		fmt.Fprintf(os.Stderr, "✗ patch: 无法定位组件包根（entry=%s），跳过 Electron 兼容补丁\n", entry)
		return
	}
	base = filepath.Join(base, "build", "src")
	// 1) McpPage.js：DevTools init 超时（createCDPSession + createTargetUniverse 各 2s）
	patchFile(filepath.Join(base, "McpPage.js"), devToolsPatchOld, devToolsPatchNew)
	// 2) third_party/index.js：goto 默认 waitUntil=load → domcontentloaded
	//    （Electron CDP 不产 name='load' 的 lifecycleEvent，goto 等 load 必超时；
	//    domcontentloaded 由 Electron 正常产出）
	patchFile(filepath.Join(base, "third_party", "index.js"), gotoPatchOld, gotoPatchNew)
	// 3) tools/pages.js：new_page 跳过 createMcpPage DevTools init（Electron 上挂起 ~15s）
	patchFile(filepath.Join(base, "tools", "pages.js"), newPagePatchOld, newPagePatchNew)
}

// packageRootOf 从包内文件路径逐级上溯，返回含 package.json 的包根目录；
// 找不到返回空串（entry 悬空/损坏）。
func packageRootOf(entry string) string {
	dir := filepath.Dir(entry)
	for {
		if fileExists(filepath.Join(dir, "package.json")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "" // 到文件系统根仍未找到
		}
		dir = parent
	}
}

// newPagePatchOld/new：new_page handler 跳过 createMcpPage（DevTools init 在 Electron 挂起）。
const newPagePatchOld = `        handler: async (request, response, context) => {
            const page = await context.newPage(request.params.background, request.params.isolatedContext);
            await page.waitForEventsAfterAction(async () => {
                await page.pptrPage.goto(request.params.url, {
                    timeout: request.params.timeout,
                });
            }, { timeout: request.params.timeout });`

const newPagePatchNew = `        handler: async (request, response, context) => {
            // go-code patch: 跳过 createMcpPage 的 DevTools init（Electron 内嵌 CDP 上挂起
            // ~15s，且 DevTools universe 只服务 lighthouse/performance 已排除工具）。
            // 直接 browser.newPage() + goto，页面对象由后续 take_snapshot 等懒初始化。
            const page = await context.browser.newPage(request.params.background);
            await page.goto(request.params.url, {
                timeout: request.params.timeout,
            });`

// gotoPatchOld/new：Puppeteer goto 的默认 waitUntil（L58921）。
const gotoPatchOld = `const { referer = this._frameManager.networkManager.extraHTTPHeaders()['referer'], referrerPolicy = this._frameManager.networkManager.extraHTTPHeaders()['referer-policy'], waitUntil = ['load'], timeout = this._frameManager.timeoutSettings.navigationTimeout(), } = options;`

const gotoPatchNew = `const { referer = this._frameManager.networkManager.extraHTTPHeaders()['referer'], referrerPolicy = this._frameManager.networkManager.extraHTTPHeaders()['referer-policy'], waitUntil = ['domcontentloaded'], timeout = this._frameManager.timeoutSettings.navigationTimeout(), } = options; // go-code patch: domcontentloaded (Electron CDP lacks load lifecycle)`

// patchFile 读文件、替换片段、写回（幂等：已含 marker 则跳过；失败仅记日志）。
func patchFile(path, old, neu string) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ patch %s: 读取失败: %v\n", path, err)
		return
	}
	src := string(data)
	if strings.Contains(src, devToolsPatchMarker) {
		return // 已 patch（幂等）
	}
	if !strings.Contains(src, old) {
		fmt.Fprintf(os.Stderr, "✗ patch %s: 未找到目标片段（上游版本可能已变），跳过\n", path)
		return
	}
	patched := strings.Replace(src, old, neu, 1)
	if err := os.WriteFile(path, []byte(patched), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "✗ patch %s: 写入失败: %v\n", path, err)
		return
	}
	fmt.Fprintf(os.Stderr, "✓ patch %s: 已应用\n", path)
}
