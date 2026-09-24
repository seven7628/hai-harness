// 直接组 .icns（2026-08-18）：iconutil 在本沙箱环境下无法生成临时文件，
// 而 ICNS 容器格式很简单：'icns' 头 + 总长 + 若干条 [4B type + 4B len + PNG 数据]。
// 故手工拼装，绕过 iconutil（macOS 识别不受影响）。
const fs = require('fs')
const path = require('path')

const base = __dirname
const set = path.join(base, 'icon.iconset')

// iconset 文件名 → ICNS 条目类型（macOS 接受 PNG 载荷的小尺寸用 icp4-6/ic11-12，大尺寸 ic07-14）
const mapping = [
  ['icon_16x16.png', 'icp4'],
  ['icon_16x16@2x.png', 'ic11'],
  ['icon_32x32.png', 'icp5'],
  ['icon_32x32@2x.png', 'ic12'],
  ['icon_128x128.png', 'ic07'],
  ['icon_128x128@2x.png', 'ic13'],
  ['icon_256x256.png', 'ic08'],
  ['icon_256x256@2x.png', 'ic14'],
  ['icon_512x512.png', 'ic09'],
  ['icon_512x512@2x.png', 'ic10'],
]

const entries = mapping.map(([name, type]) => {
  const png = fs.readFileSync(path.join(set, name))
  const be = Buffer.alloc(8 + png.length)
  be.write(type, 0, 4, 'ascii')
  be.writeUInt32BE(8 + png.length, 4)
  png.copy(be, 8)
  return be
})

const total = 8 + entries.reduce((a, b) => a + b.length, 0)
const out = Buffer.alloc(total)
out.write('icns', 0, 4, 'ascii')
out.writeUInt32BE(total, 4)
let off = 8
for (const e of entries) {
  e.copy(out, off)
  off += e.length
}
fs.writeFileSync(path.join(base, 'HAI.icns'), out)
console.log('HAI.icns written:', total, 'bytes,', entries.length, 'entries')