// known-workspaces.ts — 主进程的「用户**真的打开过**的工作区」清单。
//
// 这是媒体协议的信任锚（R08c 评审裁决：安全条目 1）。背景：`hai-media://` 的字节通道
// 按「已登记的工作区根」做包含判断，而**登记谁**这件事原来只听渲染进程的
// （`media:url {workspace}`）——评审实测传 `workspace=/Users` 就能把整个家目录登记成
// 媒体根，协议随即退化成「任意目录的媒体文件流式读取」。
//
// 现在的规则：只有**用户在系统选择器里亲手指过**的目录才进这份清单
//   · pick-workspace（「添加工作区」/「新建会话」弹的原生目录选择器）
//   · create-workspace-folder（选择器里新建的文件夹）
//   · handleWorkspacePick（手机端请求「在电脑上选目录」，同一台机器上的同一个选择器）
// 渲染进程传什么都没用：它只能**查询**清单（isKnownWorkspace），不能写入。
//
// 为什么落盘而不是只放内存：重启后渲染层会从 localStorage 恢复上次的工作区，如果清单
// 在内存里，重启后第一次打开视频就会「工作区未登记」——那等于每次重启都要重新选目录。
// 落盘后，只要这个目录**曾经**被用户亲手选过，就一直可信（直到用户删除该工作区）。
//
// 删除：工作区被移除（store 的 deleteWorkspace）→ 渲染层调 `workspace:forget`
// （主进程写，仍然不是「渲染层说加就加」那条路）。这样也不会出现评审说的
// 「关闭工作区后根永久驻留到进程退出」。
import * as fs from 'node:fs'
import * as path from 'node:path'

// **没有上限、没有淘汰**（复评裁决 1）：这里曾经是 `slice(-512)` —— 与 media-protocol 里
// 的 `MAX_ROOTS` 是同一个 bug 的第二次实现（「活跃根被后来的登记挤掉」）。条目是用户亲自
// 用系统选择器选过的目录，没有挤占的理由；删除只有一条路：forget（工作区被移除）。

interface KnownFile {
  version: 1
  workspaces: string[]
}

let filePath = ''
let loaded = false
let known: string[] = []

/** configureKnownWorkspaces 指定落盘位置（main.ts 在 ready 之后调用一次）。 */
export function configureKnownWorkspaces(file: string): void {
  filePath = file
  loaded = false
  known = []
}

function load(): void {
  if (loaded) return
  loaded = true
  if (!filePath) return
  try {
    const raw = JSON.parse(fs.readFileSync(filePath, 'utf8')) as Partial<KnownFile>
    const list = Array.isArray(raw?.workspaces) ? raw.workspaces : []
    known = list.filter((x): x is string => typeof x === 'string' && path.isAbsolute(x))
  } catch {
    known = [] // 不存在/坏了 → 空清单（fail-closed：宁可多选一次目录）
  }
}

function save(): void {
  if (!filePath) return
  try {
    fs.mkdirSync(path.dirname(filePath), { recursive: true, mode: 0o700 })
    const data: KnownFile = { version: 1, workspaces: known }
    fs.writeFileSync(filePath, JSON.stringify(data, null, 2) + '\n', { mode: 0o600 })
  } catch {
    /* 写不进去不影响本进程内的判定（内存里已经记了） */
  }
}

function canonical(p: string): string | undefined {
  if (typeof p !== 'string' || !p || !path.isAbsolute(p)) return undefined
  try {
    return fs.realpathSync(p)
  } catch {
    return undefined
  }
}

/**
 * rememberWorkspace 只在「用户亲手选了一个目录」的三处调用。
 * 返回 realpath（成功）或 undefined（不是绝对路径 / 不存在 / 不是目录）。
 */
export function rememberWorkspace(p: unknown): string | undefined {
  const raw = typeof p === 'string' ? p : ''
  const real = canonical(raw)
  if (!real) return undefined
  try {
    if (!fs.statSync(real).isDirectory()) return undefined
  } catch {
    return undefined
  }
  load()
  // 去重 + 提到最前，**不截断**
  known = [real, ...known.filter((x) => x !== real)]
  save()
  return real
}

/** forgetWorkspace 工作区被移除时调用（按 realpath 匹配；目录已不存在也能删掉记录）。 */
export function forgetWorkspace(p: unknown): void {
  const raw = typeof p === 'string' ? p : ''
  if (!raw) return
  load()
  const real = canonical(raw) ?? path.resolve(raw)
  const next = known.filter((x) => x !== real && x !== path.resolve(raw))
  if (next.length === known.length) return
  known = next
  save()
}

/** isKnownWorkspace 渲染进程交上来的 workspace 是否在清单内（媒体协议的两条 IPC 都要过这关）。 */
export function isKnownWorkspace(p: unknown): boolean {
  const real = canonical(typeof p === 'string' ? p : '')
  if (!real) return false
  load()
  return known.includes(real)
}

/** knownWorkspaces 只给测试/诊断看（不含任何写能力）。 */
export function knownWorkspaces(): string[] {
  load()
  return [...known]
}
