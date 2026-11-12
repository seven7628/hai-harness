// 文件活动状态的单字母缩写：git 面板（ChangesTab）与右侧「文件活动」列表共用。
export type FileActivityStatus = 'added' | 'modified' | 'deleted' | 'conflicted' | 'untracked' | 'read'

export function fileStatusLabel(status: FileActivityStatus): string {
  if (status === 'added' || status === 'untracked') return 'A'
  if (status === 'deleted') return 'D'
  if (status === 'conflicted') return '!'
  if (status === 'modified') return 'M'
  return '○'
}
