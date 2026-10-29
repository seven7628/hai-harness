/* 命令输出的着色（对应的设计稿：#m-tail 的 .tl / .ok / .warn / .err / .dim，
   ui.components.css:837-860）。

   设计里每条尾巴行都**自带**等级（demo 数据里写死的），真实输出得自己判：只看行首
   能认出来的东西，宁可认不出（保持正文色）也不要猜错 —— 把一句正文染成红色，
   比不染色糟得多。判定规则（与 pi 的终端着色同一档，只认前缀）：
     · err  : FAIL / FAILED / ERROR / FATAL / panic: / ✗ / 未通过的 go test 行
     · warn : WARN / WARNING / W: / deprecated
     · ok   : PASS / ok\s / ✓ / SUCCESS / --- PASS
     · dim  : go test 的 === RUN / --- 分隔行，以及 +1.02 这类时间戳前缀
   行首的时间戳（+1.02 / 12:03:44）单独成一段、更淡：它只是坐标，不是内容。 */

export type LineLevel = '' | 'ok' | 'warn' | 'err' | 'dim'

export interface OutLine {
  /** 行首刻度（时间戳/相对秒），没有就是空串 */
  stamp: string
  text: string
  level: LineLevel
}

const STAMP = /^([+−-]?\d+(?:\.\d+)?\s?(?:s|ms|KiB|MiB|B)?|\d{2}:\d{2}:\d{2}(?:\.\d+)?)\s+/

const ERR = /^(?:FAIL(?:ED|URE)?\b|ERROR\b|FATAL\b|panic:|✗|×|Uncaught\b|error:)/i
const WARN = /^(?:WARN(?:ING)?\b|W:\s|deprecated\b|⚠)/i
const OK = /^(?:---\s*PASS|PASS\b|ok\s|✓|SUCCESS\b)/i
const DIM = /^(?:===\s|RUN\s|---\s)/

/** 单行定级。`text` 是去掉刻度之后的正文。 */
export function levelOf(text: string): LineLevel {
  if (!text) return ''
  if (ERR.test(text)) return 'err'
  if (WARN.test(text)) return 'warn'
  if (OK.test(text)) return 'ok'
  if (DIM.test(text)) return 'dim'
  return ''
}

/** 把一段工具输出切成「刻度 + 正文 + 等级」。超过 limit 行就整段不切（大结果没必要染色）。 */
export function splitOutput(s: string, limit = 300): OutLine[] | null {
  if (!s) return null
  const lines = s.split('\n')
  if (lines.length > limit) return null
  return lines.map((raw) => {
    const m = STAMP.exec(raw)
    const stamp = m ? m[1] : ''
    const text = m ? raw.slice(m[0].length) : raw
    // 只有「非 go test 分隔行」才按内容定级；刻度行里 `1.24 PASS:` 这种照样算 ok
    return { stamp, text, level: levelOf(text) }
  })
}
