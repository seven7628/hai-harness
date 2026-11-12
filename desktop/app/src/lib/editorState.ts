// 内嵌编辑器的**纯状态**（PR-05b / F32：零依赖受控编辑器）。
//
// 为什么把逻辑抽到组件外面：冲突三分支、外部变更、只读、失效，这几条全是「同一份数据
// 在不同输入下该显示什么」的判断，写进 React 就只能靠点界面来验（e2e 慢且脆）。这里
// 全是纯函数 + 一个 reducer，`scripts/test-editor.mjs` 直接 import 真实现断言。
//
// 数据口径：content 是 textarea 的值；baseContent / baseHash 是**最后一次与服务端一致**
// 的内容与 sha256。dirty 永远是派生的（content !== baseContent），不单独存 —— 两份真相
// 一旦不同步，就会出现「标签上有圆点但 ⌘S 说没改动」这种没人信的界面。

/** 只读原因：'' = 可写；binary / too_large 由 file_open 的 binary/truncated 判定。 */
export type EditorReadonly = '' | 'binary' | 'too_large'

/** loading = file_open 在途；invalid = 服务端说这个路径不再属于我（denied / LRU 淘汰）。 */
export type EditorStatus = 'loading' | 'ready' | 'invalid'

/**
 * 顶部条的五种形态（黄条三类 + 红条 + 打开失败）：
 *   conflict  保存时 baseHash 不匹配（三条动作：重新加载 / 覆盖保存 / 取消）
 *   external  干净时外部改过 —— 不弹条（静默重载）；脏时才是 conflict 的前身，故用同一条
 *   deleted   磁盘上已删除（脏时禁止保存：保存会变成「新建」）
 *   invalid   denied（被 LRU 淘汰 / 越权）：只能重新打开
 *   error     其它失败（写失败、超限…）的兜底文案
 */
export type EditorBannerKind = 'conflict' | 'external' | 'deleted' | 'invalid' | 'error'

export type Eol = '\n' | '\r\n'

export interface EditorBanner {
  kind: EditorBannerKind
  /** conflict 时带上磁盘上的当前 hash（file_save 的 data.currentHash） */
  currentHash?: string
  /** error 的文案（服务端 error 字符串；i18n 文案在组件侧兜底） */
  message?: string
}

export interface EditorDoc {
  path: string
  content: string
  /**
   * 文件原本的行尾（PR-05b 评审 #3）。textarea 的 value **只可能是 LF**（HTML 规范把它
   * 归一成 LF），拿它去和 CRLF 的磁盘原文比会让「打开即脏」，保存更会把整份文件改成 LF
   * ——一次无害的编辑变成全文件 diff。所以：编辑器内部一律 LF，只有写盘时按 eol 还原。
   */
  eol: Eol
  baseContent: string
  baseHash: string
  status: EditorStatus
  readonly: EditorReadonly
  dirty: boolean
  saving: boolean
  /** 磁盘上已不存在（file_changed deleted:true / 保存回 not_found） */
  deleted: boolean
  banner?: EditorBanner
}

export type EditorAction =
  | { type: 'loading'; path: string }
  | { type: 'opened'; path?: string; content: string; hash: string; truncated?: boolean; binary?: boolean }
  | { type: 'open-failed'; code?: string; message?: string }
  | { type: 'change'; content: string }
  | { type: 'save-start' }
  | { type: 'saved'; content: string; hash: string }
  | { type: 'save-failed'; code?: string; currentHash?: string; message?: string }
  | { type: 'external'; hash?: string; deleted?: boolean }
  | { type: 'banner-close' }

/** 内容是否脏。**严格比较**：只差一个尾随换行也算脏（保存会把那个换行写进磁盘）。 */
export function isDirty(content: string, baseContent: string): boolean {
  return content !== baseContent
}

export function emptyDoc(path: string): EditorDoc {
  return {
    path, content: '', baseContent: '', baseHash: '', eol: '\n',
    status: 'loading', readonly: '', dirty: false, saving: false, deleted: false,
  }
}

/** 磁盘原文 → 编辑器空间（LF）。只做行尾归一，别的一概不动（不 trim、不补尾换行）。 */
export function toEditorText(raw: string): string {
  return raw.indexOf('\r') === -1 ? raw : raw.replace(/\r\n?/g, '\n')
}

/** 编辑器内容（LF）→ 写盘形态。CRLF 文件按原行尾还原，否则原样。 */
export function toWireText(text: string, eol: Eol): string {
  return eol === '\r\n' ? text.replace(/\n/g, '\r\n') : text
}

/** 判定文件原本的行尾。混用时按 CRLF 处理（改写最小：CRLF 文件的每一行本来就是 CRLF）。 */
export function eolOf(raw: string): Eol {
  return raw.indexOf('\r\n') !== -1 ? '\r\n' : '\n'
}

/** file_open / file_save 的只读判定：二进制优先（截断的大文件同时 binary 是不可能的，但先写死顺序）。 */
export function readonlyOf(flags: { binary?: boolean; truncated?: boolean }): EditorReadonly {
  if (flags.binary) return 'binary'
  if (flags.truncated) return 'too_large'
  return ''
}

/**
 * 媒体类型守卫：这些扩展名不该进编辑器（只有内嵌浏览器/转换服务能渲染）。
 * 只是入口处的「不给编辑按钮」，不是安全边界 —— 真边界在 bridge 的 binary 判定。
 */
const NON_TEXT_EXT = new Set([
  'png', 'jpg', 'jpeg', 'gif', 'webp', 'bmp', 'ico', 'svg', 'pdf',
  'xlsx', 'xlsm', 'xls', 'docx', 'doc', 'pptx', 'ppt', 'zip', 'gz', 'tar',
  'mp3', 'mp4', 'mov', 'wav', 'woff', 'woff2', 'ttf', 'otf',
])
export function isTextLikePath(path: string): boolean {
  const ext = (path.split('.').pop() ?? '').toLowerCase()
  return !NON_TEXT_EXT.has(ext)
}

/** 回滚开关：settings.editor === false 时「编辑」入口不出现（缺省 = 开启）。 */
export function isEditorEnabled(settings: { editor?: boolean } | undefined): boolean {
  return settings?.editor !== false
}

/**
 * 保存载荷。overwrite = 冲突条上的「覆盖保存」：显式清空 baseHash（服务端=无条件写）。
 * content 在这里**还原行尾**：编辑器空间恒为 LF，写盘按文件原本的 eol。
 */
export function savePayload(doc: EditorDoc, overwrite = false): { path: string; content: string; baseHash: string; allowNew: boolean } {
  return { path: doc.path, content: toWireText(doc.content, doc.eol), baseHash: overwrite ? '' : doc.baseHash, allowNew: false }
}

export type SaveBlock = '' | 'loading' | 'readonly' | 'invalid' | 'deleted' | 'saving'

/**
 * 能不能发 file_save。返回非空 = 不发，且**原因要显示出来**（静默拒绝是编辑器最糟的
 * 一种失败：用户按了 ⌘S，什么都不发生）。
 *
 * deleted 单独一条：文件被外部删除后，保存等于**新建**一个同名文件（后端走 allowNew
 * 分支或直接 not_found）。那必须是用户在黄条上显式确认过的动作，不能由 ⌘S 代劳。
 */
export function saveBlock(doc: EditorDoc | undefined, overwrite = false): SaveBlock {
  if (!doc || doc.status === 'loading') return 'loading'
  if (doc.saving) return 'saving'
  if (doc.status === 'invalid') return 'invalid'
  if (doc.readonly) return 'readonly'
  if (doc.deleted && !overwrite) return 'deleted'
  return ''
}

/**
 * 外部变更的处置：干净 → 重新 file_open（保留光标行）；脏 → 只弹条，**不动内容**。
 * 自动重载脏内容等于用磁盘版本静默覆盖用户没保存的修改。
 */
export function externalEffect(doc: EditorDoc, ev: { deleted?: boolean }): { reload: true } | { banner: EditorBanner } {
  if (!doc.dirty) return { reload: true }
  return { banner: { kind: ev.deleted ? 'deleted' : 'external' } }
}

export function reduceDoc(doc: EditorDoc, a: EditorAction): EditorDoc {
  switch (a.type) {
    case 'loading':
      // 显式重载（冲突条上的「重新加载」/ 干净态的外部变更）→ 条先清掉；读失败时
      // open-failed 会重新挂一条，不会出现「点了重新加载，条还在但那句话已经过期」。
      return { ...emptyDoc(a.path), readonly: doc.readonly }
    case 'opened': {
      const readonly = readonlyOf(a)
      // baseContent 存**归一后**的文本：dirty 判定必须在同一个空间里做（评审 #3）
      const text = toEditorText(a.content)
      return {
        path: a.path ?? doc.path,
        content: text,
        baseContent: text,
        baseHash: a.hash,
        eol: eolOf(a.content),
        status: 'ready',
        readonly,
        dirty: false,
        saving: false,
        deleted: false,
        // 只读态本身就是一条常驻说明，不再叠一个可关的黄条
        banner: readonly ? undefined : doc.banner,
      }
    }
    case 'open-failed':
      return {
        ...doc,
        status: a.code === 'denied' || a.code === 'not_found' ? 'invalid' : doc.status,
        saving: false,
        banner: { kind: a.code === 'denied' || a.code === 'not_found' ? 'invalid' : 'error', message: a.message },
      }
    case 'change':
      return { ...doc, content: a.content, dirty: isDirty(a.content, doc.baseContent) }
    case 'save-start':
      return { ...doc, saving: true }
    case 'saved':
      return {
        ...doc,
        saving: false,
        baseContent: a.content,
        // 响应缺 hash（老 bridge / 异常响应）时**保留旧 baseHash**：写成空串等于把下一次
        // 保存变成 F36 的「无条件覆盖」，冲突检测从此失效。宁可下一次撞 conflict。
        baseHash: a.hash || doc.baseHash,
        // 保存期间用户可能又敲了字：那是新的 dirty，不能被这次成功吞掉
        dirty: isDirty(doc.content, a.content),
        deleted: false,
        banner: undefined,
      }
    case 'save-failed': {
      const base = { ...doc, saving: false }
      switch (a.code) {
        case 'conflict':
          return { ...base, banner: { kind: 'conflict', currentHash: a.currentHash } }
        case 'denied':
          // denied = 这个路径不再被授权编辑（LRU 淘汰 / 越权 / 只读）。**不能自动重试**
          // （每次重试都会失败），必须由用户点「重新打开」重新拿授权。
          return { ...base, status: 'invalid', banner: { kind: 'invalid' } }
        case 'not_found':
          return { ...base, deleted: true, banner: { kind: 'deleted' } }
        case 'too_large':
          return { ...base, readonly: 'too_large', banner: { kind: 'error', message: a.message } }
        default:
          return { ...base, banner: { kind: 'error', message: a.message } }
      }
    }
    case 'external': {
      const eff = externalEffect(doc, a)
      if ('banner' in eff) return { ...doc, deleted: !!a.deleted, banner: eff.banner }
      return doc // 干净：由调用方发一次 file_open（reducer 不产生副作用）
    }
    case 'banner-close':
      // Esc 只关条，**不改内容、不改 baseContent**（脏仍然是脏，⌘S 仍可保存）
      return { ...doc, banner: undefined }
  }
}
