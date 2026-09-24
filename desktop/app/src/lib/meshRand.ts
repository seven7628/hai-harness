// Session Mesh 随机标识生成（浏览器/Electron 通用；crypto.getRandomValues 安全随机）。
// 用途：节点标识（UUID v4）缺省值、连接凭据（auth_token）自动生成。

// meshUUID 生成 UUID v4（RFC 4122：版本 4 + variant 10）。
export function meshUUID(): string {
  const b = new Uint8Array(16)
  crypto.getRandomValues(b)
  b[6] = (b[6] & 0x0f) | 0x40 // version 4
  b[8] = (b[8] & 0x3f) | 0x80 // variant 10
  const h = [...b].map((x) => x.toString(16).padStart(2, '0'))
  return `${h[0]}${h[1]}${h[2]}${h[3]}-${h[4]}${h[5]}-${h[6]}${h[7]}-${h[8]}${h[9]}-${h[10]}${h[11]}${h[12]}${h[13]}${h[14]}${h[15]}`
}

// meshToken 生成连接凭据（唯一随机值，开箱即用）。
// 长度 32 字符，字母数字（无歧义字符 0/O/1/l/I 排除，对齐后端随机风格）。
export function meshToken(): string {
  const alphabet = 'abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789'
  const b = new Uint8Array(32)
  crypto.getRandomValues(b)
  let out = ''
  for (let i = 0; i < b.length; i++) {
    out += alphabet[b[i] % alphabet.length]
  }
  return out
}