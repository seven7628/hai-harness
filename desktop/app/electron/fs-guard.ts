// fs-guard.ts — 敏感路径判定（主进程内**唯一**一份实现）。
//
// 为什么单独成文件：@ 引用读取（main.ts readFsRef）与 Markdown 本地图片通道
// （read-image.ts）都是「渲染进程给绝对路径 → 主进程读盘」，两处必须同源——各留一份
// 迟早漂移（一边补了 .kube，另一边忘了），而漂移出来的正是最该堵住的那类口子。
// 同族的 permission-policy.ts 也是这个理由。
import * as os from 'node:os'
import * as path from 'node:path'

// isSensitiveFsPath 敏感路径判断：home 下常见密钥/配置目录（.ssh/.aws/.gnupg 等）
// 与系统敏感区。文件树/引用读取不得进入（对齐 bridge denyHarnessConfigPath 思路）。
// 注意：.go-code 不在列表——应用自身配置目录，文件树浏览特意放行（产品决策；
// 密钥靠 0600 文件权限保护，见 #46）。
export function isSensitiveFsPath(abs: string): boolean {
  const home = os.homedir()
  const sensitive = ['.ssh', '.aws', '.gnupg', '.config/gcloud', '.kube', '.docker', '.netrc', '.npmrc', '.git-credentials']
  const clean = path.resolve(abs)
  for (const s of sensitive) {
    const p = path.join(home, s)
    if (clean === p || clean.startsWith(p + path.sep)) return true
  }
  return false
}
