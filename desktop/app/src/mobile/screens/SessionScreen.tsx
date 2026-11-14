// 手机会话页（P8-02）：**直接挂桌面 CenterPanel**。
//
// 桌面会话页的组合是 Narrative + ApprovalPanel + AskUserQuestionPanel + Composer
// （见 components/CenterPanel.tsx）。手机要的历史 / 流式 / 工具折叠 / Markdown /
// 审批 / 提问 / 发送 / 停止全在里面 —— 所以这里一行转录渲染都不写，只做三件事：
//   1. 接线：ensureStoreStarted()（wire 事件流，整个进程只做一次）+ setActiveSession(sid, ws)
//      + connectChat(ws, sid)；
//   2. 导航：44px 返回 + 当前会话标题（CenterPanel 自己的 RecentBar 在手机上隐藏，
//      标题只能由这一层给）；
//   3. 版面：`.m-session` 撑满 100dvh，composer 贴底（见 mobile.css 的会话段）。
//
// 为什么激活放在这里而不是列表的点击回调里：同一个页面也是「Hub 任务卡 → 打开会话」
// （host/panels 的 openProjectSession）的落点，那条路径不经过列表。放到挂载 effect
// 里只有一份逻辑，两条入口都走 useAppStore.setActiveSession —— 手机不另建会话状态。
//
// 事件订阅：store 的 wire() 只注册一次（模块级 wired 闸门），不会因 StrictMode 重复
// 挂 handler；chat WS 由 transport.connectChat 管，它内部先 closeLink('chat') 再开，
// 所以切 sid 不会留下旧会话的订阅（transport 没有「按 sid 断开」的接口，只有整条
// chat 链路，cleanup 就是 disconnect('chat')）。
import { useEffect } from 'react'
import CenterPanel from '../../components/CenterPanel'
import { useT } from '../../i18n'
import { useAppStore, titleKey } from '../../store/useAppStore'
import { ensureStoreStarted } from '../session-boot'
import { sharedMobileTransport } from '../transport/mobile'
import { pathTail } from '../session-data'

export default function SessionScreen({
  ws,
  sid,
  title: initialTitle,
  onBack,
}: {
  ws: string
  sid: string
  /** 列表屏已经知道标题时直接传进来（首帧就有标题，不用等 store 水合）。 */
  title?: string
  onBack: () => void
}) {
  const t = useT()
  const title = useAppStore((s) => {
    const override = s.titleOverrides[titleKey(ws, sid)]
    const fromList = s.sessions.find((x) => x.id === sid)?.title
    const fromProject = s.projects.find((p) => p.path === ws)?.sessions.find((x) => x.id === sid)?.title
    return override || fromList || fromProject || ''
  })

  useEffect(() => {
    const st = useAppStore.getState()
    // 接线只做一次（session-boot.ts）：重复 start() 在 workspace 已设时会跑桌面启动的
    // 后半段（ui 快照 / workspace_sync / skills / git），手机既不需要也多半没被放行。
    ensureStoreStarted()
    // 激活 = 桌面同一条路径：命中缓存视图就直接渲染，否则发 new_session 恢复历史。
    st.setActiveSession(sid, ws)
    const transport = sharedMobileTransport()
    // 先连 chat WS 再等响应：恢复期间/之后的流式帧都不能丢（弱网重连由 transport 自己退避）。
    transport.connectChat(ws, sid)
    return () => {
      // 离开会话页：断开这条 chat 链路（旧 sid 的订阅随之结束）。
      transport.disconnect('chat')
    }
  }, [ws, sid])

  const label = title || initialTitle || sid.slice(-8)

  return (
    <section className="m-session" data-testid="mobile-session" aria-label={label}>
      <header className="m-session-head">
        <button
          type="button"
          className="m-icon-btn m-session-back"
          onClick={onBack}
          aria-label={t('mobile.session.back')}
          title={t('mobile.session.back')}
          data-testid="mobile-session-back"
        >
          <span aria-hidden="true">←</span>
        </button>
        <span className="m-session-title" data-testid="mobile-session-title" title={label}>
          {label}
        </span>
        <span className="m-session-ws" data-testid="mobile-session-ws" title={ws}>
          {pathTail(ws)}
        </span>
      </header>
      <div className="m-session-body">
        <CenterPanel variant="mobile" />
      </div>
    </section>
  )
}
