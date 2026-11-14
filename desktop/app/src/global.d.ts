import type { DesktopApi } from './lib/bridge'

export {}

declare global {
  interface Window {
    /** 手机入口（mobile.html）在 main.tsx 里置位：getTransport() 据此分流到 mobileTransport。
     *  桌面/浏览器入口不置位，行为不变。 */
    __HAI_MOBILE__?: boolean
    /** L0（D6）一次性交接：preload 只暴露这一个函数；交接一次，第二次调用返回 null。
     *  拿到的东西只能存进 src/lib/bridge.ts 的模块私有常量，**不许**挂回 window。 */
    __haiClaimHostApi?: () => DesktopApi | null
    /** 探针读取口，**仅非生产构建**存在（见 src/lib/bridge.ts 的 probeCompatEnabled）。
     *  e2e 以前读 bridge.xxx，现在读这里；生产构建里这个属性不存在。 */
    __haiTestBridge?: DesktopApi
  }
}
