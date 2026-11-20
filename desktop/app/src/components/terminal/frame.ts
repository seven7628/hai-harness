// [recovered] from /private/tmp/builtin-surfaces.test.cjs（模块标记 `// src/components/terminal/frame.ts`）
// [implemented] 依据 docs/TERMINAL_E2E_CONTRACTS.md「帧编解码 6 条」：
//   原文是 esbuild 切片，函数**没有 export**（依赖由 bundle 注入），类型全丢。
//   本文件补上导出名（TerminalPane.tsx 的 import：encodeCtrl / decodeFrame）与类型标注，
//   **逻辑逐字未改**。移除 @ts-nocheck 的依据：本文件已被 tsc 覆盖（desktop/app/tsconfig.json）。
//
// 协议（docs/TERMINAL_BRIDGE_SPEC.md §2）：WS 二进制帧，首字节 0x00 + UTF-8 JSON = 控制帧；
// 其余（含**中途**出现的 NUL）= 终端数据帧，原样喂给 xterm。
// 畸形的控制帧（空、截断、JSON 非法、t 缺失/非字符串）**不判死连接**，按数据帧交出去 ——
// 宁可让 xterm 显示一串怪字节，也不要因为一条坏帧把整个会话拆掉。

/** 控制帧载荷。`t` 是判别器（resize / hello / reset / gap / exit），其余字段按 t 解释。 */
export interface CtrlFrame {
  t: string
  [k: string]: unknown
}

/** 解码结果：控制帧给 `ctrl`，数据帧给 `data`（两者互斥）。 */
export interface DecodedFrame {
  ctrl?: CtrlFrame
  data?: Uint8Array
}

const CTRL_BYTE = 0
const encoder = new TextEncoder()
const decoder = new TextDecoder()

/** 控制帧编码：`[0x00, ...UTF-8(JSON)]`。 */
export function encodeCtrl(c: CtrlFrame): Uint8Array {
  const json = encoder.encode(JSON.stringify(c))
  const out = new Uint8Array(json.length + 1)
  out[0] = CTRL_BYTE
  out.set(json, 1)
  return out
}

/**
 * 数据帧编码：**原样 UTF-8 字节，不加任何前缀**。
 *
 * 为什么必须有这个函数（2026-11-20 的真 bug）：发送侧原本直接 `sock.send(d)`（d 是 string），
 * 浏览器会把 string 发成 **text frame**，而服务端 `handlePty` 只收 binary：
 * `if typ != websocket.MessageBinary { continue }` —— 于是每一次按键都被**静默丢弃**：
 * 前端 onData 有日志、WS 也不报错、shell 就是不回显。这类「两端都正常、中间约定不一致」
 * 的问题只能靠把编码点收敛成一个函数来防：调用方拿不到「顺手发个字符串」的机会。
 *
 * 与 encodeCtrl 的分界：首字节 0x00 是控制帧（见 CTRL_BYTE），所以数据帧**不能**以 0x00 开头。
 * 终端数据里出现 0x00 时（Ctrl+@ / 二进制输出），服务端 decodeFrame 会因为「JSON 解析失败」
 * 或「t 缺失」把它退回成数据帧，语义仍然是正确的。
 */
export function encodeData(text: string): Uint8Array {
  return encoder.encode(text)
}

/** 帧解码。入参兼容 WebSocket 的 ArrayBuffer 与 Uint8Array 两种形态。 */
export function decodeFrame(buf: Uint8Array | ArrayBuffer): DecodedFrame {
  const bytes = buf instanceof Uint8Array ? buf : new Uint8Array(buf)
  // 长度 < 2 的「控制帧」不可能带 payload（0x00 单独一个字节是最常见的截断形态）→ 数据帧。
  if (bytes.length < 2 || bytes[0] !== CTRL_BYTE) return { data: bytes }
  try {
    const c = JSON.parse(decoder.decode(bytes.subarray(1))) as unknown
    if (!c || typeof (c as CtrlFrame).t !== 'string' || !(c as CtrlFrame).t) return { data: bytes }
    return { ctrl: c as CtrlFrame }
  } catch {
    return { data: bytes }
  }
}
