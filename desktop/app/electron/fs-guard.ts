// fs-guard.ts — 敏感路径判定（主进程内**唯一**一份实现）。
//
// 为什么单独成文件：@ 引用读取（main.ts readFsRef）与 Markdown 本地图片通道
// （read-image.ts）都是「渲染进程给绝对路径 → 主进程读盘」，两处必须同源——各留一份
// 迟早漂移（一边补了 .kube，另一边忘了），而漂移出来的正是最该堵住的那类口子。
// 同族的 permission-policy.ts 也是这个理由。
import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'

// isSensitiveFsPath 敏感路径判断：home 下常见密钥/配置目录（.ssh/.aws/.gnupg 等）
// 与系统敏感区。文件树/引用读取不得进入（对齐 bridge denyHarnessConfigPath 思路）。
// 注意：.go-code 不在列表——应用自身配置目录，文件树浏览特意放行（产品决策；
// 密钥靠 0600 文件权限保护，见 #46）。
export function isSensitiveFsPath(abs: string): boolean {
  // 两边都取 realpath 再比：调用方交进来的常常是已经 realpath 过的绝对路径（例如媒体
  // 协议的路径校验、软链解开之后），而 os.homedir() 给的是**未经 realpath** 的形态
  // （macOS 上 /var → /private/var、大小写不敏感卷上大小写不归一）。不归一就会出现
  // 「realpath 之后是 /private/var/.../home/.ssh，而 home 是 /var/.../home」这种
  // 前缀不相等 → 敏感目录被放行。realpath 失败（路径不存在）时退回 path.resolve，
  // 保持原来的判据（不因为「文件还没建」就放行）。
  const canon = (p: string): string => {
    try {
      return fs.realpathSync(p)
    } catch {
      return path.resolve(p)
    }
  }
  const home = canon(os.homedir())
  const sensitive = ['.ssh', '.aws', '.gnupg', '.config/gcloud', '.kube', '.docker', '.netrc', '.npmrc', '.git-credentials']
  const clean = canon(abs)
  for (const s of sensitive) {
    const p = path.join(home, s)
    if (clean === p || clean.startsWith(p + path.sep)) return true
    // 大小写不敏感卷上 realpath 不归一大小写（实测 macOS）：再比一次小写形态，
    // 否则 `~/.SSH` 会绕过。只对**敏感目录判定**放宽大小写，不影响其它包含判断。
    if (process.platform === 'darwin' || process.platform === 'win32') {
      const lc = clean.toLowerCase()
      const lp = p.toLowerCase()
      if (lc === lp || lc.startsWith(lp + path.sep)) return true
    }
  }
  return false
}
