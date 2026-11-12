import type { GitStatus } from '../store/useAppStore'

/** 无会话（工作区级探测尚未返回）时的空快照：0 变更、clean */
export const EMPTY_GIT: GitStatus = { is_repo: false, ahead: 0, behind: 0, staged: 0, modified: 0, untracked: 0, conflicted: 0, clean: true }
