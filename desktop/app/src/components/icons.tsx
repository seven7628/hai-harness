import type { ReactElement } from 'react'

type P = { size?: number; className?: string }

function base(size: number, children: ReactElement, className?: string): ReactElement {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" className={className}>
      {children}
    </svg>
  )
}

export const ChevronLeft = ({ size = 12, className }: P) => base(size, <path d="m15 18-6-6 6-6" />, className)
export const ChevronRight = ({ size = 12, className }: P) => base(size, <path d="m9 18 6-6-6-6" />, className)
export const PaperclipIcon = ({ size = 13, className }: P) => base(size, <path d="m21.44 11.05-9.19 9.19a6 6 0 0 1-8.49-8.49l8.57-8.57A4 4 0 1 1 18 8.84l-8.59 8.57a2 2 0 0 1-2.83-2.83l8.49-8.48" />, className)
export const CheckCircleIcon = ({ size = 13, className }: P) => base(size, <><path d="M22 11.08V12a10 10 0 1 1-5.93-9.14" /><path d="m9 11 3 3L22 4" /></>, className)
export const ChevronDown = ({ size = 12, className }: P) => base(size, <path d="m6 9 6 6 6-6" />, className)
export const Globe = ({ size = 13 }: P) => base(size, <><circle cx="12" cy="12" r="10" /><path d="M2 12h20M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z" /></>)
export const FileIcon = ({ size = 13 }: P) => base(size, <><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" /><path d="M14 2v6h6" /></>)
export const FolderIcon = ({ size = 15 }: P) => base(size, <path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z" />)
export const FolderOpenIcon = ({ size = 15 }: P) => base(size, <path d="m6 14 1.5-2.9A2 2 0 0 1 9.24 10H20a2 2 0 0 1 1.94 2.5l-1.54 6a2 2 0 0 1-1.95 1.5H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h3.9a2 2 0 0 1 1.69.9l.81 1.2a2 2 0 0 0 1.67.9H18a2 2 0 0 1 2 2v2" />)
export const Sparkles = ({ size = 15 }: P) => base(size, <path d="M12 3l1.9 5.7a2 2 0 0 0 1.3 1.3L21 12l-5.8 1.9a2 2 0 0 0-1.3 1.3L12 21l-1.9-5.8a2 2 0 0 0-1.3-1.3L3 12l5.8-1.9a2 2 0 0 0 1.3-1.3z" />)
export const AccessIcon = ({ size = 15 }: P) => base(size, <><path d="M12 3 5 6v5c0 4.6 2.8 8.1 7 10 4.2-1.9 7-5.4 7-10V6z" /><path d="m9 12 2 2 4-4" /></>)
// HookIcon 钩子（生命周期 hook）：锚形（设置导航「钩子」+ 来源卡片头；对齐参考截图的钩子图标）
export const HookIcon = ({ size = 15, className }: P) => base(size, <><circle cx="12" cy="5" r="2.6" /><path d="M12 7.6V21" /><path d="M5 12H2.6a9.4 9.4 0 0 0 18.8 0H19" /></>, className)
export const EffortIcon = ({ size = 15 }: P) => base(size, <><path d="M4 6h10M18 6h2M4 12h3M11 12h9M4 18h8M16 18h4" /><circle cx="16" cy="6" r="2" /><circle cx="9" cy="12" r="2" /><circle cx="14" cy="18" r="2" /></>)
export const Clock = ({ size = 15 }: P) => base(size, <><circle cx="12" cy="12" r="9" /><path d="M12 7v5l3 2" /></>)
export const Gear = ({ size = 15 }: P) => base(size, <><circle cx="12" cy="12" r="3" /><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" /></>)
export const SendIcon = ({ size = 14 }: P) => base(size, <path d="m22 2-7 20-4-9-9-4z" />)
export const ImageIcon = ({ size = 15 }: P) => base(size, <><rect x="3" y="4" width="18" height="16" rx="2" /><circle cx="8.5" cy="9" r="1.5" /><path d="m3 16 4.5-4 3.5 3 3-2.5 7 6" /></>)
export const Activity = ({ size = 15 }: P) => base(size, <path d="M22 12h-4l-3 9L9 3l-3 9H2" />)
export const GitBranch = ({ size = 15 }: P) => base(size, <><circle cx="6" cy="6" r="3" /><circle cx="6" cy="18" r="3" /><circle cx="18" cy="6" r="3" /><path d="M6 9v6M18 9a9 9 0 0 1-9 9" /></>)
export const PanelLeft = ({ size = 15 }: P) => base(size, <><rect x="3" y="3" width="18" height="18" rx="2" /><path d="M9 3v18" /></>)
export const PanelRight = ({ size = 15 }: P) => base(size, <><rect x="3" y="3" width="18" height="18" rx="2" /><path d="M15 3v18" /></>)
export const PinIcon = ({ size = 14 }: P) => base(size, <><path d="M9 4h6M12 4v6l4 4v2H8v-2l4-4V4" /><path d="M12 16v4" /></>)
export const Plus = ({ size = 15 }: P) => base(size, <><path d="M12 5v14M5 12h14" /></>)
// 溢出菜单（工作区行操作收窄成一个「⋯」）：与 lucide more-horizontal 同形
export const MoreIcon = ({ size = 15 }: P) => base(size, <><circle cx="5" cy="12" r="1.3" /><circle cx="12" cy="12" r="1.3" /><circle cx="19" cy="12" r="1.3" /></>)
// 文件夹 + 加号：添加工作区（折叠图标条与展开态菜单共用语义）
export const FolderPlus = ({ size = 15 }: P) => base(size, <><path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z" /><path d="M12 11v6M9 14h6" /></>)
export const Back = ({ size = 15 }: P) => base(size, <path d="m9 18 6-6-6-6" />)
export const SessionIcon = ({ size = 15 }: P) => base(size, <path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />)
// 新建会话（消息气泡 + 加号）：与 SessionIcon（气泡）、FolderPlus（文件夹）同族，
// 三者在展开态与折叠图标条里一一对应（不再是含义模糊的裸加号）。
export const SessionPlus = ({ size = 15 }: P) => base(size, <><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" /><path d="M12 7v6M9 10h6" /></>)
export const StopIcon = ({ size = 14 }: P) => (
  <svg width={size} height={size} viewBox="0 0 24 24" fill="currentColor"><rect x="6" y="6" width="12" height="12" rx="2" /></svg>
)
// 话筒（语音输入）：录音中填色 + 脉冲动画（CSS .mic-btn.recording）
export const MicIcon = ({ size = 15, className }: P) => base(size, <><rect x="9" y="2.5" width="6" height="11" rx="3" /><path d="M19 10v1a7 7 0 0 1-14 0v-1" /><path d="M12 18v3" /></>, className)
export const Bot = ({ size = 15 }: P) => base(size, <><rect x="4" y="8" width="16" height="12" rx="2" /><path d="M12 8V4M9 13h.01M15 13h.01" /></>)
export const BarChart = ({ size = 15 }: P) => base(size, <><path d="M3 20h18M7 20v-7M12 20V6M17 20v-11" /></>)
export const CheckList = ({ size = 15 }: P) => base(size, <><path d="M9 6h12M9 12h12M9 18h12M3.5 6l1 1 1.5-1.5M3.5 12l1 1 1.5-1.5M3.5 18l1 1 1.5-1.5" /></>)
export const EyeIcon = ({ size = 14 }: P) => base(size, <><path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7z" /><circle cx="12" cy="12" r="3" /></>)
export const EyeOffIcon = ({ size = 14 }: P) => base(size, <><path d="M9.9 4.24A9.12 9.12 0 0 1 12 4c6.5 0 10 8 10 8a18.5 18.5 0 0 1-2.16 3.19M6.61 6.61A18.6 18.6 0 0 0 2 12s3.5 8 10 8a9.12 9.12 0 0 0 5.39-1.61M2 2l20 20M14.12 14.12a3 3 0 1 1-4.24-4.24" /></>)

/* ── 会话过程行的行首图标（V3 皮肤用；默认皮肤下不渲染，见 narrative-skin.css 的 .ic 规则）──
 * 语义固定：读文件=文档(FileIcon) · 检索=放大镜 · 执行=终端 · 改文件=笔 · 子代理=Bot(已有)
 *          计划=CheckList(已有) · 思考=Sparkles(已有) · 压缩=收拢 · 重试=警告 · 后台=播放
 * 画法与既有图标同一口径（base(): 24 格 viewBox、stroke 1.8、currentColor）。 */
export const SearchIcon = ({ size = 13, className }: P) => base(size, <><circle cx="11" cy="11" r="7" /><path d="m16.5 16.5 4 4" /></>, className)
export const TerminalIcon = ({ size = 13, className }: P) => base(size, <><path d="m5 7 4 5-4 5" /><path d="M12 17h7" /></>, className)
export const PencilIcon = ({ size = 13, className }: P) => base(size, <><path d="M4 20h4L18.5 9.5a2.1 2.1 0 0 0-3-3L5 17z" /><path d="m13.5 6.5 3 3" /></>, className)
export const MinimizeIcon = ({ size = 13, className }: P) => base(size, <><path d="M9 4v6H3M15 20v-6h6" /><path d="m3 10 6-6M21 14l-6 6" /></>, className)
export const AlertIcon = ({ size = 13, className }: P) => base(size, <><path d="M12 4l8.5 15h-17z" /><path d="M12 10v4M12 17h.01" /></>, className)
export const PlayIcon = ({ size = 13, className }: P) => base(size, <path d="M7 4.5l12 7.5-12 7.5z" />, className)
export const PaletteIcon = ({ size = 14, className }: P) => base(size, <><path d="M12 3a9 9 0 1 0 0 18h1.2a2.3 2.3 0 0 0 0-4.6H12a2.4 2.4 0 0 1 0-4.8h4.6A4.4 4.4 0 0 0 21 7.2 4.2 4.2 0 0 0 16.8 3z" /><circle cx="8" cy="9.5" r="1.1" /><circle cx="12" cy="7" r="1.1" /><circle cx="16" cy="9" r="1.1" /></>, className)
