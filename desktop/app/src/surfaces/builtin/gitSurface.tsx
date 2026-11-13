// git 面板的 SurfaceProvider（PR-03：第一个迁移的内置面板）
// 组件本体从 RightPanel 平移到 components/ChangesTab.tsx，这里只做「注册 + 元数据」。
import { GitBranch } from '../../components/icons'
import { ChangesTab } from '../../components/ChangesTab'
import { registerSurface } from '../registry'
import type { SurfaceProvider } from '../types'

export const gitSurface: SurfaceProvider = {
  kind: 'git',
  namespace: 'builtin',
  group: 'workflow',
  order: 20,
  singleton: true,
  scope: 'session',
  weight: 'light',
  capabilities: ['read', 'view'],
  title: (_tab, t) => t('rp.tab.git'),
  icon: () => <GitBranch size={12} />,
  defaultSpec: () => ({ kind: 'git' }),
  render: () => <ChangesTab />,
}

registerSurface(gitSurface)
