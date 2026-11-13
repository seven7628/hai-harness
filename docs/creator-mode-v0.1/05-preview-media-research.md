# 文档与媒体预览 —— DSH 实现拆解 + 我们的补齐方案

> 日期：2026-11-11　状态：调研 + 设计决策（待并入 `01-design.md` 的 §4.7 与 `02-pr-plan.md` 的 PR-08 系列）
> 结论先行：**DSH 的文档预览是"一个预览子系统 + 可插拔渲染器 + Host 侧 LibreOffice 转换服务"；它没有音视频预览。**
> 我们的改法：把 `office` 从"一种 kind"降级成"一种渲染器"，照抄它的**限额与缓存**，音视频自己设计。

---

## 1. DSH 怎么做的

### 1.1 客户端：一个 Sidebar 预览子系统，不是一堆面板

`packages/client/ui-sidebar-documentpreview` 的定位写得很直白：**共享文件加载与控件，可选 Markdown / 代码 / 图片 / PDF / Office / HTML 渲染器，未知扩展名用纯文本兜底**。

| 能力 | 实现要点 |
|---|---|
| 文本 / Markdown / 代码 | **分页读取**（`workspaceFiles.read(offset)`），首屏一页，滚到底或点"加载更多"再读；增量渲染累计前缀，不把每页当独立文档；第一页之后到的版本变化会让读取从头开始（防混版本） |
| 图片 | Blob URL + `<img>`；适应宽度（不放大）、25%–400% 逐级、拖拽平移、SVG 永不进 DOM（防脚本） |
| PDF | **pdf.js 单独 chunk 懒加载**（不进启动 bundle）；按视口惰性渲染页面、未渲染页用 3:4 占位块保持位置；文字层用 pdf.js 官方 TextLayer（可选中/复制）；单页位图上限 16,777,216 px |
| Office（doc/docx/ppt/pptx） | **不自己渲染**：向 Host 要 PDF（`officeKit.render`），然后**复用 PDF 渲染器**——所以排版保真是 LibreOffice 给的，客户端只做一次"借字节 → 转 PDF → pdf.js" |
| 表格（xls/xlsx/csv/tsv） | **浏览器内解析，不走转换服务**：FortuneSheet + ExcelJS(XLSX) + SheetJS(XLS) + PapaParse(CSV)；工作表标签、单元格选择、复制、只读公式栏；xlsx 预览会剥离 DrawingML 并**明确提示**"图表/图片/形状/条件格式未展示" |
| HTML | DOMPurify 清洗 + 无权限 iframe（CSP 禁脚本/外链/表单）；可选"带脚本的 Blob 预览"（交互式） |
| 统一控件 | 缩放（适应宽度 / 25%–400%）、换行、重新加载、滚动位置记忆（像素距离）、加载态、失败**给出口**（重试 / 不可预览 / 用系统应用打开） |
| 缓存 | Office PDF 缓存 **8 条 / 64 MiB**，身份 = 渲染 generation + Session + 源绝对路径 + 源版本；失败不缓存；**PDF 永不持久化** |

### 1.2 Host：`office-kit` 是一个有准入控制的转换服务

`packages/document/office-kit`（配 `@deepseek-ai/libreoffice-kit`，native 优先、WASM 兜底）：

| 方法 | 用途 |
|---|---|
| `convert()` | Office → **PDF**（预览用），返回字节 + 缓存键 |
| `renderImages()` | 指定页/幻灯片/工作表区域 → **PNG**（按 dpi），给模型"看" |
| `convertTo()` | 格式转换（docx/pdf/txt/xlsx/csv/pptx…），CSV 要指定 sheet |
| `recalculate()` | 重算 xls/xlsx/ods（因为预览显示的是缓存值） |

工程细节（这才是真正值得抄的部分）：

- **准入队列**：`maxConcurrentConversions=2`、`timeoutMs=60s`、`maxInputBytes=50 MiB`、`maxOutputBytes=100 MiB`；排队中的调用可取消；
- **内容寻址缓存**：key = 转换 generation + 扩展名 + 操作 + 参数 + **源字节 SHA-256**；跨路径共享同一份转换；
- **输出不落工作区**：`$DSH_HOME/office-kit/outputs/<session-…>/<uuid>/`，保留 24h、总上限 512 MiB、LRU 淘汰；启动时清理上个进程的过期目录（只认真实目录，不碰符号链接）；
- **降级是显式的**：kit 缺失时 provider 照常注册，调用返回 `unavailable`（不是崩溃）；缺字体时返回缺失字体列表，客户端在工具栏显示黄色警告 → 点开看字体清单；
- **只读**：源文件从不修改；源版本变化直接拒绝（防止"转的是旧文件"）。

配套的两个工具（给模型用）：`office_convert` / `office_recalculate` / `office_render`（渲染成 PNG 让模型自己看版式，做视觉验收）。

### 1.3 音视频：DSH 明确没做

`packages/attachment/attachment` 原文：**"音频和视频暂无专门处理"**；通用文件只按字节原样存、模型拿到路径自己去读。所以这一块没有可抄的实现，只能自己设计（见 §4）。

---

## 2. 我们（go-code）的现状

| 格式 | 现状 | 文件 |
|---|---|---|
| 图片 | 在 markdown 预览里 `<img>`，无缩放/平移 | `markdown.tsx` |
| PDF | 内嵌浏览器 `file://`，Chromium 自带 viewer（有缩放/翻页/搜索，但不受我们控制） | `store.openHtmlInBrowser` |
| docx | mammoth → **语义 HTML**（标题/表格/图片 data URI），够用 | `docx_preview.go`（390 行） |
| xlsx | openpyxl → **静态 HTML**：多 sheet 堆叠、公式无缓存值时显示公式文本、行列/总格数硬闸、说明条如实告知截断 | `xlsx_preview.go`（474 行） |
| pptx | python-pptx → **绝对定位近似重建**：页面尺寸/形状几何/文本字号颜色/图片；不支持项**原位占位**（"图表未渲染"） | `pptx_preview.go`（938 行） |
| csv | 解析 → GFM 表格 → 复用 Markdown 管线（零新依赖），行列三重闸门 | `CsvPreview.tsx` + `lib/csv.ts` |
| 分类 | `visualFile.ts` 是"哪些算可视类"的**单一事实源**（image/browser/pdf/xlsx/docx/pptx） | `lib/visualFile.ts` |
| 音视频 | **没有** | — |

**已有的好底子**：三个转换器都是"受管 Python 运行时 + 沙箱 + 独立临时目录 + 按年龄清理"的形状，天然适合升级成统一转换服务；`visualFile.ts` 已经是单一事实源，适合挂渲染器注册表。

**关键约束**：本机**没有 LibreOffice**（有 WPS/Keynote，但没有可靠 CLI），`ffmpeg`/`ffprobe` 在 PATH 上；Electron 自带 `libffmpeg.dylib`（H.264/AAC 可播）。

---

## 3. 差距矩阵

| 格式 | DSH | 我们 | 差距 | 结论 |
|---|---|---|---|---|
| PDF | pdf.js + 文本层 + 虚拟化 | Chromium viewer | 控件不可控、无法自定义工具栏/页码 | **保持 Chromium**（够用），补元数据与"外部打开"；不自研 pdf.js（引依赖） |
| docx | LibreOffice→PDF | mammoth→HTML | 版式（页眉页脚/分栏/精确字号）偏弱 | **保持 mammoth**，可选 LibreOffice 时切 PDF |
| pptx | LibreOffice→PDF | 近似重建 | **保真度差距最大** | 有 kit → PDF；无 kit → 现有近似 + 顶部能力提示 |
| xlsx | 客户端表格（标签/选择/公式栏） | 静态 HTML（多 sheet 堆叠） | 无交互、无冻结、无单元格选择 | **增强服务端产物**（sheet 导航/冻结/数字格式/合并），不引前端依赖 |
| csv | PapaParse + 表格/文本双视图 | GFM 表格 | 无文本视图、编码检测弱 | 补"文本/表格"切换 + BOM 检测 |
| 图片 | 缩放/平移/SVG 隔离 | `<img>` | 无缩放平移 | 补一个轻量 viewport（transform，不加依赖） |
| 音视频 | **无** | **无** | 全新 | 自研 `media` 渲染器（Range + ffprobe + 可选转码） |
| 文本分页 | 有（offset） | 1 MiB 截断 | 大文件只能看前 1 MiB | 补 offset 分页 |

---

## 4. 设计决策

### D-I：取消独立 `office` kind，改成一个 `document` 渲染器注册表

DSH 的做法证明：**"能预览什么"是渲染器的问题，不是面板的问题**。把 `office` 做成一种 kind，会让 `file`/`office`/`media` 三套标签状态各自演化（页码、sheet、播放进度）。

**结论**：

```ts
// surfaces/preview/types.ts
export interface PreviewRenderer {
  id: string                       // 'pdf' | 'office' | 'sheet' | 'image' | 'media' | 'text' | 'html'
  match: (f: FileRef) => number    // 0 = 不接；>0 优先级（多个渲染器竞争时取最大）
  weight?: 'light' | 'heavy'       // heavy 走转换服务的准入队列
  load: () => Promise<React.ComponentType<PreviewProps>>   // 懒加载
}
```

- `file` 标签的 `mode='preview'` 内部按 `match` 选渲染器；
- 插件将来可以注册渲染器（M2，和 Surface 同一套 `register/unregister` 纪律）；
- 已持久化的 `office` 标签**迁移**为 `file{mode:'preview'}`（结构版本 +1 时做）。
- **`01-design.md` 的 `office` kind 相应删除**（不再新增 kind，保持 4 个）。

### D-J：Office 保真 = **沿用 DSH 的 LibreOffice kit**（不采用 Keynote/PowerPoint 脚本）

**决策**：pptx/docx/xls/xlsx/ppt 的保真路径统一为 `soffice → PDF → 现有 PDF 渲染器`，与 DSH 的 `office-kit` 同构；kit 是**可下载的可选组件**，缺失时降级到现有近似渲染器并显式提示。

**为什么放弃 Keynote/PowerPoint 脚本后端**（上一轮考虑过）：

| 问题 | 影响 |
|---|---|
| 只有 macOS/Windows，Linux 无解 | 跨平台要写 3 套，且 CI/无 GUI 环境跑不了 |
| `open` 会把 Keynote 拉到前台 | 后台预览抢焦点；只能改成"用户点一下才转" |
| 需要 TCC 自动化授权，拒绝后不可恢复 | 首次体验不可控 |
| 无法服务 agent（`office_render` 要无人值守） | 创造模式拿不到"看图验收"的能力 |

DSH 的四面方法 + 限额是现成的正确解，抄它比自创便宜。详见 §8。

**三档（每一步都显式告知）**

| 条件 | 路径 | 保真度 | UI |
|---|---|---|---|
| kit 可用 | `soffice --headless --convert-to pdf` → PDF 渲染器 | 高（DSH 同级） | 正常 |
| kit 缺失 | mammoth / python-pptx 近似 / openpyxl | 中 | 顶部横幅「轻量预览 · 安装 Office 组件可获得排版保真」 |
| 转换失败/超时 | 错误码 + 「用系统程序打开」（WPS/Keynote/PowerPoint） | — | 面板内错误卡，不转圈 |

**给模型的第四个能力**：`office_render`（页/幻灯片 → PNG）让 agent 能"看见"版式做验收——这是 DSH 里 `office_render` 的真正价值，不只是给人看。

### D-K：表格不引前端依赖，增强服务端产物

DSH 能上 FortuneSheet 是因为它接受前端依赖；我们的硬约束是不加。所以：

- **多 sheet → 顶部 sheet 导航条**（锚点 + sticky），不再一路堆到底；
- 冻结窗格（`ws.freeze_panes`）→ 用 `position: sticky` 实现表头/首列固定；
- 数字格式/合并单元格/列宽 → openpyxl 已有信息，补到 HTML；
- 单元格选择与复制 → 用浏览器原生选区 + `user-select: text`，不做 Excel 式框选；
- CSV 补"表格 / 文本"双视图（文本走代码渲染器）。

### D-L：音视频自研（DSH 没有）

**播放**：右栏 `media` 渲染器，原生 `<video controls>` / `<audio controls>`。

**字节通道**：**必须支持 Range**（否则不能拖动进度条）。Electron 主进程注册自定义协议：

```ts
// desktop/app/electron/media-protocol.ts
protocol.handle('hai-media', async (req) => {
  const p = decodeURIComponent(new URL(req.url).pathname)
  const st = await fs.promises.stat(p)
  const mime = mimeOf(p)
  const range = req.headers.get('range')
  if (!range) return new Response(fs.createReadStream(p), {
    headers: { 'content-type': mime, 'accept-ranges': 'bytes', 'content-length': String(st.size) },
  })
  const m = /^bytes=(\d*)-(\d*)$/.exec(range)
  const start = m?.[1] ? Number(m[1]) : 0
  const end = m?.[2] ? Math.min(Number(m[2]), st.size - 1) : st.size - 1
  if (start >= st.size) return new Response(null, { status: 416, headers: { 'content-range': `bytes */${st.size}` } })
  return new Response(fs.createReadStream(p, { start, end }), {
    status: 206,
    headers: {
      'content-type': mime, 'accept-ranges': 'bytes',
      'content-range': `bytes ${start}-${end}/${st.size}`,
      'content-length': String(end - start + 1),
    },
  })
})
```

**探测**：`media_probe` 命令走 `ffprobe -print_format json`，返回时长/码率/流信息/宽高，按 `(path,size,mtime)` 缓存。

**编解码矩阵**（Electron 自带 ffmpeg，但只含常见组合）：

| 容器/编码 | 能否直接播 | 策略 |
|---|---|---|
| mp4 / h264 + aac | ✅ | 直接播 |
| webm / vp9 + opus | ✅ | 直接播 |
| mp3 / wav / ogg / flac | ✅ | `<audio>` |
| mov (h264) | ✅（多数） | 直接播 |
| mkv / avi / ts / m2ts | ❌（容器不支持） | 提示 + 「转码为 MP4」 |
| hevc / av1 / prores | ⚠️ 视构建 | 探测 → 不能播则转码 |
| ac3 / dts 音轨 | ❌ | 转码为 AAC |

**转码（可选，M2）**：`ffmpeg -i in -c:v libx264 -crf 23 -preset veryfast -c:a aac -movflags +faststart out.mp4`，输出到 `~/.go-code/media/transcodes/`（保留 24h、总上限 2 GiB、LRU），带进度与取消；**没有 ffmpeg 就只给"用系统播放器打开"**。

**缩略图**：`ffmpeg -ss <10%> -frames:v 1 -vf scale=320:-1` 出 poster（失败静默，不阻塞播放）。

### D-M：转换服务统一（照抄 DSH 的限额，不照抄它的依赖）

把三个 previewer 收进 `desktop/bridge/conversion/`：

```go
type Service struct {
    sem        chan struct{}          // 并发 2
    timeout    time.Duration          // 60s
    maxIn      int64                  // 50 MiB
    maxOut     int64                  // 100 MiB
    cache      *lru.Cache             // key = sha256(源字节)+op+params+converterVersion
    outDir     string                 // ~/.go-code/previews/<session>/<uuid>/
}
func (s *Service) Convert(ctx, req) (Result, error)   // 准入 → 缓存 → 转 → 落盘 → 记账
```

要点：**失败不缓存**；**输出不落工作区**；保留 24h + 512 MiB LRU；启动时清理过期目录；`unavailable` 与 `timeout` 是两种不同错误码（前者提示"没装运行时"，后者提示"文档太大/太复杂"）。

---

## 5. PR 拆分（并入 `02-pr-plan.md`）

| # | 标题 | 估时 | 依赖 | 说明 |
|---|---|---|---|---|
| 08 | ~~office provider~~ → **document 渲染器注册表** + 现有渲染器接入 | 1.5d | 04 | 不新增 kind（保持 4）；`file{mode:'preview'}` |
| 08a | **转换服务**（准入/限额/缓存/输出保留）+ 三个 previewer 接入 | 2d | 08 | 照 D-M；无行为变化，纯收口 |
| 08b | **表格增强**（sheet 导航、冻结、数字格式、合并、CSV 双视图） | 1.5d | 08a | 服务端 HTML 增强，不加前端依赖 |
| 08c | **媒体渲染器**（Range 协议 + ffprobe + 播放器 + 编解码矩阵） | 2d | 08 | 新能力；无 ffmpeg 时降级 |
| 08d | **LibreOffice kit**（核心）：下载/探测/校验 + profile 隔离 + `soffice→PDF` + `renderImages`(pypdfium2) | 3d | 08a | 与 DSH 对齐的唯一保真路径；详见 §8 |
| 08e | （可选）**转码 + 缩略图**（ffmpeg 任务队列、进度、取消、保留策略） | 2d | 08c | 只在真有需求时做 |

总计 +12 人日（08e 转码 2 人日为可选；不做则 +10）。与 `02-pr-plan.md` 的 34.5 人日口径一致。

---

## 6. 验收

| 用例 | 判据 |
|---|---|
| docx / pptx / xlsx / pdf / csv / png / mp4 / mp3 各一个 | 全部能在右栏打开，且**渲染器选择可见**（不是"猜"） |
| 同一文件第二次打开 | 命中转换缓存（<50ms），日志/回执可见 `cache: hit` |
| 100 MiB pptx | 明确 `too-large`，不卡 UI、不 OOM |
| 转换器崩溃（Python 异常） | 返回 `failed` + 原因；**不影响其它面板**；不缓存失败 |
| 播放 1 小时 mp4 | 拖动进度条即时（Range 生效）；切走再切回不重新下载 |
| mkv 文件 | 明确"容器不支持" + 「系统播放器打开」/「转码」（有 ffmpeg 时） |
| 无 LibreOffice 的机器 | docx/pptx 走近似 + 顶部能力提示；不出现"转圈到死" |
| xlsx 20 个 sheet | sheet 导航可用；单 sheet 超限时如实告知截断 |

---

## 7. 风险

| 风险 | 缓解 |
|---|---|
| LibreOffice kit 体积（~700 MB）与签名/更新 | 做成**可选下载**，不内置；DSH 也是 optionalDependencies |
| `soffice` 转换并发吃满 CPU | 复用 D-M 的并发 2 + 超时 + 队列 |
| 媒体协议成为任意文件读取通道 | 只允许 workspace 内 + 已登记路径；路径规范化后校验；不接受 URL 中的 `..` |
| 转码产物堆积 | 保留策略 + LRU + 启动清理（与转换服务同一套） |
| Electron 编解码差异 | 以 `ffprobe` 为准做能力判断，不做硬编码假设 |

---

*本文件是 `01-design.md` §4.7 的展开版；决策 D-I…D-M 需回写进该文档的 §11 决策登记。*


---

## 8. LibreOffice kit 的落地设计（决策 D-J 的展开）

### 8.1 形态：不内置，可下载，按平台分发

```
~/.go-code/office-kit/<version>/<platform>/
  soffice            # 或 LibreOffice.app 的裁剪运行时
  resources/…
  KIT.json           # {version, platform, sha256, builtAt, license}
```

- **不打包进安装包**（macOS 安装包 +700 MB 不可接受，DSH 也是 `optionalDependencies`）；
- 首次打开 Office 文件时探测：`KIT.json` 存在且 sha256 校验通过 → 可用；否则走近似 + 安装引导；
- 下载源：自有 CDN；**sha256 + 版本 pin**，拒绝"最新版随便下"；
- 平台矩阵：macOS arm64 / x64、Windows x64、Linux x64；Linux 直接复用系统 `soffice`（存在即用，不下载）；
- 许可：LibreOffice 是 MPL 2.0 / LGPLv3 混合，分发需带 NOTICE 与源码获取链接（`KIT.json` 里记录）。

### 8.2 进程模型（三个必须踩对的坑）

```go
// 每个 job 独立 profile，否则第二个转换会撞上 profile 锁（最典型的坑）
args := []string{
  "--headless", "--norestore", "--nolockcheck", "--nodefault",
  "-env:UserInstallation=file://" + jobProfileDir,
  "--convert-to", "pdf", "--outdir", outDir, src,
}
cmd := exec.CommandContext(ctx, sofficePath, args...)
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}   // kill 进程组，别留 soffice.bin
```

1. **profile 隔离**：`-env:UserInstallation` 必须是每个 job 独立目录。共用会直接失败（"user installation is locked"）或串行阻塞——这是所有封装 LibreOffice 的项目都会踩的第一坑；
2. **杀进程组**：`soffice` 会 fork `soffice.bin`；只 kill 父进程会留孤儿并占住 profile；
3. **首启冷**：第一次转换要初始化用户配置，可能 3–6s；之后 1–2s。用**预热点**（安装完成时跑一次空转换）把首启成本挪到安装期。

### 8.3 四个方法（与 DSH 一一对应）

| 方法 | 实现 | 备注 |
|---|---|---|
| `convert()` | `soffice --convert-to pdf` | 预览用；结果进内容寻址缓存 |
| `renderImages()` | PDF → **pypdfium2** 逐页 PNG（dpi 可配） | **运行时已装 pypdfium2 5.9.0**（`runtime/python.go`），不用 poppler/mutool |
| `convertTo()` | `soffice --convert-to docx/xlsx/csv/pptx/odt/ods` | CSV 需 `sheet` 参数（DSH 同款） |
| `recalculate()` | 走 LibreOffice 的 load+save 触发重算 | V0.1 可不做，留给"表格公式要对"的真实需求 |

**关键**：`renderImages` 不需要新二进制——我们已经把 pypdfium2 打进受管 Python 运行时了，这正是 DSH 用 PDF 页栅格化的等价物。

### 8.4 限额（复用 D-M，数值照 DSH）

| 项 | 值 |
|---|---|
| 并发转换 | 2（超出排队，可取消） |
| 单次超时 | 60s（冷启 90s，仅首次） |
| 输入 / 输出上限 | 50 MiB / 100 MiB |
| 缓存键 | `sha256(源字节) + 操作 + 参数 + kit 版本` |
| 输出目录 | `~/.go-code/previews/<session>/<uuid>/`，24h + 512 MiB LRU |
| 失败 | 不缓存，稳定错误码（`unavailable` / `timeout` / `too-large` / `convert-failed`） |

### 8.5 与现有近似渲染器的关系

**保留**，但降级为 fallback：

- kit 可用 → 默认走 PDF 路径（`data-renderer="libreoffice"`）；
- kit 缺失 → 近似路径 + 顶部横幅；设置里提供"安装 Office 组件"入口；
- 近似路径继续维护（它是零安装体验的兜底，也是 pptx 主题/占位符改进的落点）。

### 8.6 验收

| 用例 | 判据 |
|---|---|
| 装 kit 后开 pptx | 走 PDF 渲染器，页码/缩放/文本选择可用；`data-renderer="libreoffice"` |
| 连续转换 3 个文件 | 并发 2 生效，第三个排队；不出现 profile 锁错误 |
| 转一个 30 页 pptx | <15s（热态）；超时给 `timeout` 且 soffice 进程组被清理 |
| 卸载 kit | 立刻回到近似路径 + 横幅；不残留后台进程 |
| `office_render` 指定第 3 页 | 返回 PNG，dpi 生效，写入缓存 |
