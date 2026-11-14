// 手机端主题偏好：沿用旧手机页的 localStorage key（desktop/bridge/mobile_web/app.js
// 的 LS.theme = 'hai.theme'，取值 system/ink/paper），升级到新页不掉档。
// 桌面端主题不走 localStorage（settings.json + lib/appearance.ts），所以这里是手机侧
// 自己的映射：ink ↔ dark、paper ↔ light。
import type { ThemePref } from '../lib/theme'

export const MOBILE_THEME_KEY = 'hai.theme'

export function readThemePref(): ThemePref {
  try {
    const raw = (localStorage.getItem(MOBILE_THEME_KEY) ?? '').trim()
    if (raw === 'paper' || raw === 'light') return 'light'
    if (raw === 'ink' || raw === 'dark') return 'dark'
  } catch {
    /* 隐私模式：跟随系统 */
  }
  return 'system'
}

/** 写回旧页同款取值（ink/paper/system），两版页面互相认。 */
export function writeThemePref(pref: ThemePref): void {
  const legacy = pref === 'light' ? 'paper' : pref === 'dark' ? 'ink' : 'system'
  try {
    localStorage.setItem(MOBILE_THEME_KEY, legacy)
  } catch {
    /* 写不进去只影响下次打开，不影响本次 */
  }
}
