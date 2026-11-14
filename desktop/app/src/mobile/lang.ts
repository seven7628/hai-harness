// 手机语言偏好（P8-01b）：i18n 的语言来源从「桌面 settings」换成 host/lang.tsx，
// 手机壳在这里读写自己的偏好。
//
// key 用 `hai.mobile.lang`（旧手机页没有语言开关，所以没有历史 key 要沿用）；
// 桌面语言仍由 settings.json 持有，两边互不覆盖。
import { normalizeHostLang, type HostLang } from '../host/lang'

export const MOBILE_LANG_KEY = 'hai.mobile.lang'

export function readLangPref(): HostLang {
  try {
    return normalizeHostLang(localStorage.getItem(MOBILE_LANG_KEY))
  } catch {
    /* 隐私模式：默认中文 */
    return 'zh'
  }
}

export function writeLangPref(lang: HostLang): void {
  try {
    localStorage.setItem(MOBILE_LANG_KEY, lang)
  } catch {
    /* 写不进去只影响下次打开，不影响本次 */
  }
}
