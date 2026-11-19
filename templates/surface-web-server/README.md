# 带服务的网页插件示例

这个目录同时是 **manifest** 和 **前端服务**：没有 `panel.js`，宿主按 `web.start` 拉起
`node server.mjs`，就绪后用原生视图打开 `http://127.0.0.1:5235`，一个 entry 一个标签。

## 运行

```bash
# 只需要 Node >= 18，没有 npm install
node server.mjs
# 浏览器打开 http://127.0.0.1:5235 —— 此时没有 window.haiHost，页面会降级
```

把整个目录交给 CreateMode 安装（`surface_install({ target: "<绝对路径>" })`），或者：

```bash
bash desktop/vendor/create-mode-skills/create-mode/scripts/install-smoke.sh "$PWD/templates/surface-web-server"
```

然后在应用右栏 `＋ → 面板插件 → 带服务的网页示例` 打开。**不需要先手动启动 server**：
第一次打开标签时宿主会探活，没起才 spawn；关标签不杀服务，关插件/退出应用才停。

> 如果应用是从 Finder 启动的，`PATH` 里可能没有 `node`；把 `web.start.command` 改成
> `which node` 的绝对路径（例如 `/opt/homebrew/bin/node`）。

## 验证点

- 打开首页 → 满幅（没有右栏内边距/第二层滚动条），地址是 `http://127.0.0.1:5235/`。
- 打开「后台」→ 第二个标签；两个标签各自保活，切回来页面状态还在。
- 首页的「问 Agent」→ 走 `window.haiHost.command('agent.session')` + `agent.run`；
  会话出现在左栏插件分组，不属于用户当前对话。
- 杀掉 server 进程 → 宿主错误态 + 重试；点重试会重新 spawn。
- 改 `surface.json` / `server.mjs`（dev 安装）→ 插件热重载，不需要重启应用。

## 自动验证（真 Electron）

```bash
cd desktop/app
npm run build:electron
npx vite build
(cd ../.. && go build -o desktop/app/dist/hai-bridge ./desktop/bridge)
npm run e2e:web-server-surface
```

探针会把本目录复制到临时目录、改成空闲端口，并在**应用已经启动之后**安装它，验证：
外部安装 → index watcher → surfaces_changed（不重启应用）→ 首次打开自动起服务 → 满幅 →
两个标签共享同一进程 → kill/重试 → 关标签不杀服务、关应用杀服务。
