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
