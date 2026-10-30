// fs-ops.ts — 文件树写操作（重命名 / 移动 / 新建 / 复制副本）的**唯一**实现。
//
// 为什么单独成文件：这些操作改的是用户的真实文件，而渲染进程会把**绝对路径**交给主进程。
// 与只读那侧（fs-guard.ts 的敏感目录判定）同一条理由 —— 守卫只能有一份。散在 main.ts 的
// IPC 分支里，迟早出现「rename 挡了 ..，move 忘了挡」这种漂移，而漏掉的那一处正是最该堵的。
//
// 口径（与 fs:list / fs:read-ref 一致，但**更严**）：
//   · 源与目标都必须落在**当前工作区**内（realpath 包含判断，symlink 逃逸同样挡住）。
//     只读那侧允许浏览工作区外（用户可能配了别处的技能），写不行：文件树显示的根就是
//     工作区，拖动/删除的作用域不该超出用户眼前这一棵树。
//   · 工作区根目录本身**不可**移动/删除/复制（src === ws 一律拒）。这条是防灾难的：
//     一次误拖就能把整个项目搬走。
//   · 敏感目录（~/.ssh 等）与 harness 配置目录（~/.go-code，含 api_key 明文）双向拒绝。
//   · 名字只做最小校验（禁 `/`、`\0`、`.`、`..`、空）：用户要建 `.env` 就让他建。
//
// 删除**不在这里**：删到废纸篓要用 Electron 的 shell.trashItem（主进程能力，纯 Node 拿不到），
// 本文件只导出 guardTrash 供 main.ts 在调用前校验。这样本模块保持零 Electron 依赖，可单测。
//
// —— 同步 / 异步的分界（2026-10 用户口径「改成 fs.promises.cp，能不能做异步的」）——
//   对外七个写操作**全部 async**，调用方 await 即可，不必去记哪个是 O(1)、哪个是 O(bytes)。
//   内部只有两类同步调用，都是 O(1) 元数据操作（微秒级，不占事件循环）：
//     · 守卫：realpath / lstat / stat（判定路径归属与是否存在）
//     · rename / mkdir / open('wx')：单条 syscall
//   真正 O(bytes) 的是**内容拷贝**（复制副本、拷贝到别处、跨卷移动、从访达导入）→
//   fs.promises.cp（分块流式，Node 16.7+）。此前用 cpSync，拷几百 MB 的目录会把主进程
//   整段占住：渲染进程不卡，但窗口输入/其它 IPC 全部迟滞（单测 async-not-blocking 那条
//   用例就是钉这个：拷贝期间定时器必须还能跑）。
import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'
import { isSensitiveFsPath } from './fs-guard'

// 失败原因用 code 表达（渲染层可映射文案），error 是给用户看的中文短句（与主进程既有风格一致）。
// EXISTS 单列：与用户选择的口径一致 —— 同名冲突**直接拒绝**并提示，不弹窗、不自动改名。
export type FsOpCode = 'EXISTS' | 'DENIED' | 'INVALID_NAME' | 'NOT_FOUND' | 'IS_ROOT' | 'CYCLE' | 'IO'

export interface FsOpResult {
  ok: boolean
  path?: string // 操作结果绝对路径（rename/move/duplicate/mkdir/create 返回，供前端选中/重载）
  error?: string
  code?: FsOpCode
}

// 默认配置目录：与 main.ts 的 cfgDir() 同值（主进程会显式传入，MULTI_DATA_DIR 场景才不会错）。
const defaultCfgDir = () => path.join(os.homedir(), '.go-code')

// realOrSelf：realpath，路径不存在时**对存在的父级求 realpath 再拼回名字**。
// 为什么不用 resolve 就完事：symlink 可以把「看起来在工作区里」的路径指到工作区外
// （或指到 ~/.ssh），只比字符串前缀是挡不住的。
// 为什么不存在时不能直接 resolve：macOS 上 /tmp 是 /private/tmp 的符号链接，工作区路径
// 走 realpath 后是 /private/tmp/…，而 resolve 出来的新名字是 /tmp/… —— 两者永远不互相
// 包含，于是「在工作区里新建/删除一个还不存在的名字」会被误判成越界。逐级往上找到存在的
// 那一层再拼，两个命名空间就对齐了。
function realOrSelf(p: string): string {
  try {
    return fs.realpathSync(p)
  } catch {
    const dir = path.dirname(p)
    if (dir === p) return path.resolve(p)
    return path.join(realOrSelf(dir), path.basename(p))
  }
}

function inside(child: string, parent: string): boolean {
  return child === parent || child.startsWith(parent + path.sep)
}

// validateName：文件名/目录名校验。返回错误文案；合法 → null。
export function validateName(raw: unknown): string | null {
  if (typeof raw !== 'string') return '名称无效'
  const name = raw.trim()
  if (!name) return '名称不能为空'
  if (name === '.' || name === '..') return '名称无效'
  if (name.includes('/') || name.includes('\0')) return '名称不能包含斜杠'
  // 255 字节是 APFS/HFS+ 的单段上限；按字节算，中文名 3 字节/字。
  if (Buffer.byteLength(name, 'utf8') > 255) return '名称过长'
  return null
}

// normalizedName：校验通过后的实际落盘名（去首尾空白）。
export function normalizedName(raw: string): string {
  return raw.trim()
}

// guardPath 源/目标路径的共同校验：绝对路径、工作区内、非敏感目录、非配置目录。
// allowRoot=false（源）时拒绝工作区根；allowRoot=true（目标目录）时允许（= 根下新建/拖回根）。
//
// 返回两个路径，**别混用**（踩过）：
//   · abs    = realpath，**只用于校验与比较**（符号链接可以把「看着在工作区里」的路径指到外面）；
//   · target = path.resolve(入参)，**实际 fs 调用与返回值都用它**。
// 为什么不能用 abs 干活：macOS 上 /tmp 就是 /private/tmp 的符号链接，返回 realpath 会让前端
// 拿到一个自己从没见过的路径 —— 左栏选中、目录重载、右栏标签匹配全部对不上，且只在
// workspace 经过符号链接时复现（本机 /tmp、外接卷、iCloud 场景）。
function guardPath(
  wsRaw: string,
  pRaw: string,
  cfgDir: string,
  allowRoot: boolean,
): { ok: true; ws: string; abs: string; target: string } | { ok: false; error: string; code: FsOpCode } {
  if (typeof wsRaw !== 'string' || !wsRaw || !path.isAbsolute(wsRaw)) {
    return { ok: false, error: '工作区路径无效', code: 'DENIED' }
  }
  if (typeof pRaw !== 'string' || !pRaw || !path.isAbsolute(pRaw)) {
    return { ok: false, error: '无效路径', code: 'DENIED' }
  }
  const ws = realOrSelf(wsRaw)
  const abs = realOrSelf(pRaw)
  const target = path.resolve(pRaw)
  if (!inside(abs, ws)) return { ok: false, error: '只能操作当前工作区内的文件', code: 'DENIED' }
  if (!allowRoot && abs === ws) return { ok: false, error: '不能操作工作区根目录', code: 'IS_ROOT' }
  if (isSensitiveFsPath(abs) || isSensitiveFsPath(target)) {
    return { ok: false, error: '该路径受保护（含敏感凭据）', code: 'DENIED' }
  }
  const cfg = path.resolve(cfgDir || defaultCfgDir())
  const cfgReal = realOrSelf(cfg)
  if (inside(abs, cfgReal) || inside(target, cfg)) {
    return { ok: false, error: '该路径受保护（应用配置目录）', code: 'DENIED' }
  }
  return { ok: true, ws, abs, target }
}

// 供 main.ts 删除前复用（trashItem 在调用方）。
export function guardTrash(ws: string, p: string, cfgDir = defaultCfgDir()): FsOpResult {
  const g = guardPath(ws, p, cfgDir, false)
  if (!g.ok) return { ok: false, error: g.error, code: g.code }
  let st: fs.Stats
  try {
    st = fs.lstatSync(g.target)
  } catch {
    return { ok: false, error: '文件不存在', code: 'NOT_FOUND' }
  }
  // 符号链接：lstat 通过即可删（删的是链接本身，不会顺着链接删目标）。
  void st
  return { ok: true, path: g.target }
}

// existsAs 目标是否已存在；同 inode（macOS 大小写不敏感卷上的「只改大小写」）不算冲突 ——
// 否则 Auth.go → auth.go 这种重命名会被自己的存在挡住（Finder 允许）。
function sameInode(a: string, b: string): boolean {
  try {
    const sa = fs.statSync(a)
    const sb = fs.statSync(b)
    return sa.ino === sb.ino && sa.dev === sb.dev
  } catch {
    return false
  }
}

function existsConflict(dest: string, src: string): boolean {
  if (!fs.existsSync(dest)) return false
  return !sameInode(dest, src)
}

// renameEntry 原地重命名（父目录不变）。
export async function renameEntry(ws: string, p: string, name: string, cfgDir = defaultCfgDir()): Promise<FsOpResult> {
  const nameErr = validateName(name)
  if (nameErr) return { ok: false, error: nameErr, code: 'INVALID_NAME' }
  const g = guardPath(ws, p, cfgDir, false)
  if (!g.ok) return { ok: false, error: g.error, code: g.code }
  const dest = path.join(path.dirname(g.target), normalizedName(name))
  if (dest === g.target) return { ok: true, path: g.target }
  if (existsConflict(dest, g.target)) return { ok: false, error: '目标位置已存在同名项目', code: 'EXISTS' }
  try {
    fs.renameSync(g.target, dest)
  } catch (e) {
    return { ok: false, error: (e as NodeJS.ErrnoException).message, code: 'IO' }
  }
  return { ok: true, path: dest }
}

// copyTree 跨卷兜底 / 复制副本 / 拷贝到别处 / 访达导入共用。
//
// 用 fs.promises.cp 而不是 cpSync：目录拷贝是 O(bytes)，同步版会把主进程整段占住
// （窗口输入迟滞、其它 IPC 排队）。cp 是分块流式的，期间事件循环照常转。
// errorOnExist + force:false → 目标已存在时抛 EEXIST（我们前面已用 existsConflict 挡过
// 一次，这里是竞态兜底：两次并发粘贴同名文件时后一个会拿到错误而不是静默覆盖）。
async function copyTree(src: string, dest: string): Promise<void> {
  await fs.promises.cp(src, dest, { recursive: true, errorOnExist: true, force: false, preserveTimestamps: true })
}

// copyFailure 把 cp 的异常翻成用户能读的结果。
// ERR_FS_CP_EEXIST 是**竞态**才可能出现的那条（前面 existsConflict 已经挡过一次）：
// 两次并发粘贴同名文件、或用户在拷贝途中往目标里放了同名条目。直接暴露这个内部码
// 毫无意义，映射成与其它路径一致的「已存在同名项目」。
function copyFailure(e: unknown): FsOpResult {
  const err = e as NodeJS.ErrnoException
  if (err?.code === 'ERR_FS_CP_EEXIST') return { ok: false, error: '目标目录已存在同名项目', code: 'EXISTS' }
  return { ok: false, error: err?.message ?? String(e), code: 'IO' }
}

// removeTree 失败清理用：删掉可能残留的目标（拷贝失败时别让用户看到半成品）。
async function removeTree(p: string): Promise<void> {
  try {
    await fs.promises.rm(p, { recursive: true, force: true })
  } catch {
    /* 清不掉就留给用户，别把原始错误盖掉 */
  }
}

// moveEntry 把 src 搬进 destDir（保持名字）。同目录 → 无操作（拖动到当前父目录不该报错）。
export async function moveEntry(ws: string, srcRaw: string, destDirRaw: string, cfgDir = defaultCfgDir()): Promise<FsOpResult> {
  const gs = guardPath(ws, srcRaw, cfgDir, false)
  if (!gs.ok) return { ok: false, error: gs.error, code: gs.code }
  const gd = guardPath(ws, destDirRaw, cfgDir, true)
  if (!gd.ok) return { ok: false, error: gd.error, code: gd.code }
  let destStat: fs.Stats
  try {
    destStat = fs.statSync(gd.target)
  } catch {
    return { ok: false, error: '目标目录不存在', code: 'NOT_FOUND' }
  }
  if (!destStat.isDirectory()) return { ok: false, error: '目标不是目录', code: 'IO' }
  if (path.dirname(gs.target) === gd.target) return { ok: true, path: gs.target }
  // 环检测：目录不能搬进自己或自己的子孙（realpath 比字符串前缀可靠）。
  if (inside(gd.abs, gs.abs)) return { ok: false, error: '不能把目录移动到它自己里面', code: 'CYCLE' }
  const dest = path.join(gd.target, path.basename(gs.target))
  if (existsConflict(dest, gs.target)) return { ok: false, error: '目标目录已存在同名项目', code: 'EXISTS' }
  try {
    fs.renameSync(gs.target, dest)
  } catch (e) {
    // EXDEV：跨卷（工作区里的挂载点/符号链接）。rename 不能跨设备 → 拷贝 + 删源。
    // 只有确认 dest 尚未产生时才拷贝：rename 失败不会留下半成品，但 cp 可能（失败要清干净）。
    if ((e as NodeJS.ErrnoException).code !== 'EXDEV') {
      return { ok: false, error: (e as NodeJS.ErrnoException).message, code: 'IO' }
    }
    try {
      await copyTree(gs.target, dest)
      await fs.promises.rm(gs.target, { recursive: true, force: true })
    } catch (e2) {
      // 拷贝失败 → 清掉可能残留的目标，避免用户看到「两份都不完整」。
      await removeTree(dest)
      return copyFailure(e2)
    }
  }
  return { ok: true, path: dest }
}

// copyEntry 拷贝到另一个目录（⌘C/⌘V 的落地）：目标保持原名字，同名 → EXISTS。
// 与 moveEntry 共用同一套守卫与环检测 —— 把目录拷进自己的子孙一样会无限递归。
export async function copyEntry(ws: string, srcRaw: string, destDirRaw: string, cfgDir = defaultCfgDir()): Promise<FsOpResult> {
  const gs = guardPath(ws, srcRaw, cfgDir, false)
  if (!gs.ok) return { ok: false, error: gs.error, code: gs.code }
  const gd = guardPath(ws, destDirRaw, cfgDir, true)
  if (!gd.ok) return { ok: false, error: gd.error, code: gd.code }
  let destStat: fs.Stats
  try {
    destStat = fs.statSync(gd.target)
  } catch {
    return { ok: false, error: '目标目录不存在', code: 'NOT_FOUND' }
  }
  if (!destStat.isDirectory()) return { ok: false, error: '目标不是目录', code: 'IO' }
  if (inside(gd.abs, gs.abs)) return { ok: false, error: '不能把目录复制到它自己里面', code: 'CYCLE' }
  const dest = path.join(gd.target, path.basename(gs.target))
  if (existsConflict(dest, gs.target)) return { ok: false, error: '目标目录已存在同名项目', code: 'EXISTS' }
  try {
    await copyTree(gs.target, dest)
  } catch (e) {
    await removeTree(dest)
    return copyFailure(e)
  }
  return { ok: true, path: dest }
}

// importEntry 从**工作区外**（访达）拖进来的文件/目录 → 拷进 destDir。
//
// 为什么不能复用 copyEntry：那一条要求源也在工作区内（防渲染进程被注入后去搬任意文件）。
// 导入的源天然在工作区外，所以这里单独放行「源」这一侧，但**仍然**不许碰敏感目录与
// harness 配置目录 —— 用户把 ~/.ssh/id_rsa 拖进项目里，是这个功能最不该发生的事。
// 目标侧照旧必须落在工作区内（guardPath allowRoot=true）。
export async function importEntry(ws: string, destDirRaw: string, srcRaw: string, cfgDir = defaultCfgDir()): Promise<FsOpResult> {
  const gd = guardPath(ws, destDirRaw, cfgDir, true)
  if (!gd.ok) return { ok: false, error: gd.error, code: gd.code }
  const src = typeof srcRaw === 'string' && srcRaw ? path.resolve(srcRaw) : ''
  if (!src || !path.isAbsolute(srcRaw)) return { ok: false, error: '无效的源路径', code: 'DENIED' }
  if (isSensitiveFsPath(src)) return { ok: false, error: '该路径受保护（含敏感凭据）', code: 'DENIED' }
  if (inside(src, realOrSelf(path.resolve(cfgDir || defaultCfgDir()))) || inside(src, path.resolve(cfgDir || defaultCfgDir()))) {
    return { ok: false, error: '该路径受保护（应用配置目录）', code: 'DENIED' }
  }
  try {
    fs.lstatSync(src)
  } catch {
    return { ok: false, error: '源文件不存在', code: 'NOT_FOUND' }
  }
  let destStat: fs.Stats
  try {
    destStat = fs.statSync(gd.target)
  } catch {
    return { ok: false, error: '目标目录不存在', code: 'NOT_FOUND' }
  }
  if (!destStat.isDirectory()) return { ok: false, error: '目标不是目录', code: 'IO' }
  const dest = path.join(gd.target, path.basename(src))
  if (fs.existsSync(dest)) return { ok: false, error: '目标目录已存在同名项目', code: 'EXISTS' }
  try {
    await copyTree(src, dest)
  } catch (e) {
    await removeTree(dest)
    return copyFailure(e)
  }
  return { ok: true, path: dest }
}

// createDir 在 dir 下新建目录；已存在 → EXISTS（不静默复用）。
export async function createDir(ws: string, dir: string, name: string, cfgDir = defaultCfgDir()): Promise<FsOpResult> {
  const nameErr = validateName(name)
  if (nameErr) return { ok: false, error: nameErr, code: 'INVALID_NAME' }
  const gd = guardPath(ws, dir, cfgDir, true)
  if (!gd.ok) return { ok: false, error: gd.error, code: gd.code }
  const dest = path.join(gd.target, normalizedName(name))
  if (fs.existsSync(dest)) return { ok: false, error: '已存在同名项目', code: 'EXISTS' }
  try {
    fs.mkdirSync(dest)
  } catch (e) {
    return { ok: false, error: (e as NodeJS.ErrnoException).message, code: 'IO' }
  }
  return { ok: true, path: dest }
}

// createFile 在 dir 下新建空文件（'wx' = 排他创建，天然挡同名）。
export async function createFile(ws: string, dir: string, name: string, cfgDir = defaultCfgDir()): Promise<FsOpResult> {
  const nameErr = validateName(name)
  if (nameErr) return { ok: false, error: nameErr, code: 'INVALID_NAME' }
  const gd = guardPath(ws, dir, cfgDir, true)
  if (!gd.ok) return { ok: false, error: gd.error, code: gd.code }
  const dest = path.join(gd.target, normalizedName(name))
  try {
    const fd = fs.openSync(dest, 'wx')
    fs.closeSync(fd)
  } catch (e) {
    const code = (e as NodeJS.ErrnoException).code
    return code === 'EEXIST'
      ? { ok: false, error: '已存在同名项目', code: 'EXISTS' }
      : { ok: false, error: (e as NodeJS.ErrnoException).message, code: 'IO' }
  }
  return { ok: true, path: dest }
}

// duplicateEntry 生成副本（Mac Finder 的「复制」）：a.go → a copy.go → a copy 2.go。
// 副本名是**我们自己生成的**，不做冲突提示（用户没有为它命名），因此这里自动递增；
// 用户手动命名的重命名/移动仍严格拒绝同名（与产品口径一致）。
export async function duplicateEntry(ws: string, p: string, cfgDir = defaultCfgDir()): Promise<FsOpResult> {
  const g = guardPath(ws, p, cfgDir, false)
  if (!g.ok) return { ok: false, error: g.error, code: g.code }
  const dir = path.dirname(g.target)
  const base = path.basename(g.target)
  const isDir = fs.statSync(g.target).isDirectory()
  const ext = isDir ? '' : path.extname(base)
  const stem = ext ? base.slice(0, -ext.length) : base
  let dest = ''
  for (let i = 1; i <= 99; i++) {
    const suffix = i === 1 ? ' copy' : ` copy ${i}`
    const candidate = path.join(dir, `${stem}${suffix}${ext}`)
    if (!fs.existsSync(candidate)) {
      dest = candidate
      break
    }
  }
  if (!dest) return { ok: false, error: '副本过多，无法命名', code: 'EXISTS' }
  try {
    await copyTree(g.target, dest)
  } catch (e) {
    await removeTree(dest)
    return copyFailure(e)
  }
  return { ok: true, path: dest }
}
