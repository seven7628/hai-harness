// 链路（activity）面板的 SurfaceProvider（PR-07）。组件本体从 RightPanel 逐字平移到
// components/ActivityTab.tsx，这里只做「注册 + 元数据 + 渲染委派」。
//
// 为什么 render 里还要自己判一次 kind：这一条**不是**防御性编程的洁癖 —— provider 对别的
// kind 明确返回 null，调用点才能用 `?.render(...) ?? 回退` 表达「provider 缺失时才回退」。
// 但这也决定了调用点**必须**保留 kind 守卫：`null ?? <ActivityTab/>` 的结果是
// `<ActivityTab/>`（?? 只在 null/undefined 时取右边，而这里恰好返回 null），
// 无条件挂会让 git/web 标签连带渲出链路面板。RightPanel 侧因此与 git/web 同形。
// 想让那行守卫消失，得把「非 file 面板」整体收口成一条 `getSurface(activeTab.kind)` 委派
//（PR-11 的口径），不是本轮顺手能做的（会动 R06c 刚落的 web 渲染点）。
//
// 身份：activity 是会话级单例（一个会话只有一条「当前链路」），与 git 同类，不像 file/web
// 那样多实例。判据见 03-code-plan §2.3「池是基础设施，不是某个 provider 的私产」——
// trace 的 store 逻辑（fetchTraces / selectTrace / projectAllTraces）一行未动。
import { Activity } from '../../components/icons'
import { ActivityTab } from '../../components/ActivityTab'
import { registerSurface } from '../registry'
import type { SurfaceProvider } from '../types'

export const activitySurface: SurfaceProvider = {
  kind: 'activity',
  namespace: 'builtin',
  group: 'content',
  order: 30,
  singleton: true,
  allowDuplicate: false,
  scope: 'session',
  weight: 'light',
  capabilities: ['read', 'view'],
  // 与 RightPanel 的 kindLabel('activity') 同一份 i18n（rp.tab.activity），文案不另起一份。
  title: (_tab, t) => t('rp.tab.activity'),
  icon: () => <Activity size={12} />,
  defaultSpec: () => ({ kind: 'activity' }),
  render: ({ tab }) => (tab.kind === 'activity' ? <ActivityTab /> : null),
}

registerSurface(activitySurface)
