// 变更面板（Git）：从 RightPanel 平移出来的独立组件（PR-03 provider 化）
// 依赖全部来自 store / i18n / ui，不再持有 RightPanel 的局部状态。
import { useEffect, useRef, useState } from 'react'
import { useT } from '../i18n'
import { useAppStore } from '../store/useAppStore'
import type { GitFile } from '../store/useAppStore'
import { EMPTY_GIT } from '../lib/gitStatus'
import { fileStatusLabel } from '../lib/fileStatus'
import type { FileActivityStatus } from '../lib/fileStatus'
import { GitBranch } from './icons'
import { UnifiedDiff } from './ui'
import ConfirmModal from './ConfirmModal'
const GIT_STALE_MS = 30_000 // 变更 tab 打开时工作区级快照的 stale 阈值
const GIT_POLL_MS = 10_000 // 变更 tab 打开时的快照轮询周期（确认是仓库后才启动）
const GIT_GRAPH_COLORS = ['var(--running)', 'var(--success)', 'var(--warning)', 'var(--danger)', 'var(--text-2)']

// git log --graph 图示前缀按列着色（泳道感）；空格原样占位
function GitGraph({ g }: { g?: string }) {
  if (!g) return null
  return (
    <span className="git-graph mono">
      {g.split('').map((ch, i) => (ch === ' '
        ? ' '
        : <span key={i} style={{ color: GIT_GRAPH_COLORS[i % GIT_GRAPH_COLORS.length] }} className={ch === '*' ? 'dot' : undefined}>{ch}</span>
      ))}
    </span>
  )
}

// iso-strict 时间 → 「YYYY-MM-DD HH:mm」（当年省略年份）
function fmtGitDate(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const pad = (n: number) => String(n).padStart(2, '0')
  const ymd = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
  const hm = `${pad(d.getHours())}:${pad(d.getMinutes())}`
  return d.getFullYear() === new Date().getFullYear() ? `${ymd.slice(5)} ${hm}` : `${ymd} ${hm}`
}

// 变更 tab：Git 面板（设计见 docs/DESKTOP_GIT.md）。
// 双层数据源：有活跃会话读会话级快照（随工具调用自动刷新）；
// 无会话读工作区级探测结果（loadWsGit/loadWsGitHistory，添加工作区即拉取）。
export function ChangesTab() {
  const t = useT()
  const hasSession = useAppStore((s) => !!s.activeSessionId)
  const git = useAppStore((s) => (hasSession ? s.git : s.wsGit ?? EMPTY_GIT))
  const gitFiles = useAppStore((s) => (hasSession ? s.gitFiles : s.wsGitFiles))
  const loading = useAppStore((s) => (hasSession ? s.gitLoading : s.wsGitLoading))
  const error = useAppStore((s) => (hasSession ? s.gitError : s.wsGitError))
  const commits = useAppStore((s) => (hasSession ? s.gitHistory : s.wsGitHistory))
  const hasMore = useAppStore((s) => (hasSession ? s.gitHasMore === true : s.wsGitHasMore === true))
  const histLoading = useAppStore((s) => (hasSession ? s.gitHistLoading === true : s.wsGitHistLoading === true))
  const fileDiffs = useAppStore((s) => s.wsFileDiffs)
  const refresh = useAppStore((s) => s.refreshGit)
  const loadHistory = useAppStore((s) => s.loadGitHistory)
  const loadCommitDiff = useAppStore((s) => s.loadCommitDiff)
  const loadGitDiff = useAppStore((s) => s.loadGitDiff)
  const loadWsGit = useAppStore((s) => s.loadWsGit)
  const loadWsGitHistory = useAppStore((s) => s.loadWsGitHistory)
  const loadWsFileDiff = useAppStore((s) => s.loadWsFileDiff)
  const loadWsCommitDiff = useAppStore((s) => s.loadWsCommitDiff)
  // —— 写操作（P1）——
  const gitOpBusy = useAppStore((s) => s.gitOpBusy)
  const gitOpError = useAppStore((s) => s.gitOpError)
  const gitStage = useAppStore((s) => s.gitStage)
  const gitUnstage = useAppStore((s) => s.gitUnstage)
  const gitDiscard = useAppStore((s) => s.gitDiscard)
  const gitCommitOp = useAppStore((s) => s.gitCommit)
  const clearGitOpError = useAppStore((s) => s.clearGitOpError)
  const openFilePreview = useAppStore((s) => s.openFile)
  // —— 分支与仓库初始化（P2）——
  const gitBranches = useAppStore((s) => s.gitBranches)
  const loadGitBranches = useAppStore((s) => s.loadGitBranches)
  const gitCheckout = useAppStore((s) => s.gitCheckout)
  const gitInitRepo = useAppStore((s) => s.gitInitRepo)
  const [openPath, setOpenPath] = useState<string | null>(null) // 展开的文件（staged 前缀 s: 区分同路径双态）
  const [openCommit, setOpenCommit] = useState<string | null>(null)
  const [confirmDiscard, setConfirmDiscard] = useState<GitFile | null>(null) // 丢弃/删除确认（破坏性）
  const [commitMsg, setCommitMsg] = useState('')
  const [stageAll, setStageAll] = useState(false) // 提交时是否包含未暂存变更（git add -A）
  const [filter, setFilter] = useState('') // 变更文件过滤（大仓库快速定位）
  const [branchOpen, setBranchOpen] = useState(false) // 分支切换弹层（点开才拉清单；不占常驻空间）
  const [secOpen, setSecOpen] = useState({ files: true, history: true }) // 分区折叠（空间有限时按需收起）

  // 挂载/stale 重拉：工作区级快照缺失或超过 30s → 重拉 snapshot + history
  useEffect(() => {
    if (hasSession) { refresh(); loadHistory(); return }
    const st = useAppStore.getState()
    if (st.wsGitAt == null || Date.now() - st.wsGitAt > GIT_STALE_MS) { loadWsGit(); loadWsGitHistory() }
  }, [hasSession, refresh, loadHistory, loadWsGit, loadWsGitHistory])

  // 定时刷新（仅确认是仓库后启动；no-git / 未识别时不轮询）：
  // 10s 快照轮询；切 session / 切 workspace 由既有触发链（setActiveSession / setActiveWorkspace）覆盖，
  // tick 内经 getState() 取当前数据源，切换后的下一次 tick 自然路由到新目标。
  useEffect(() => {
    if (!git.is_repo) return
    const timer = setInterval(() => {
      if (document.hidden) return // 窗口不可见时跳过，省电省 git 调用
      const st = useAppStore.getState()
      if (st.activeSessionId) st.refreshGit()
      else st.loadWsGit()
    }, GIT_POLL_MS)
    return () => clearInterval(timer)
  }, [git.is_repo])

  // HEAD 变化（新提交/切换分支/checkout）→ 重拉提交历史；unborn HEAD（head 为空）不触发
  const lastHeadRef = useRef<string | undefined>(undefined)
  useEffect(() => {
    const head = git.head
    if (head && lastHeadRef.current != null && lastHeadRef.current !== head) {
      if (hasSession) loadHistory()
      else loadWsGitHistory()
    }
    if (head) lastHeadRef.current = head
  }, [git.head, hasSession, loadHistory, loadWsGitHistory])

  // 窗口重新可见/聚焦 → 立即刷新快照（覆盖「切到终端操作 git 后切回」场景；
  // 与 10s 轮询互补，P2 记录了不引入 fs watcher 的取舍：.git watch 感知不到工作区编辑）
  useEffect(() => {
    if (!git.is_repo) return
    const refreshNow = () => {
      if (document.hidden) return
      const st = useAppStore.getState()
      if (st.activeSessionId) st.refreshGit()
      else st.loadWsGit()
    }
    window.addEventListener('focus', refreshNow)
    document.addEventListener('visibilitychange', refreshNow)
    return () => {
      window.removeEventListener('focus', refreshNow)
      document.removeEventListener('visibilitychange', refreshNow)
    }
  }, [git.is_repo])

  const refreshAll = () => { if (hasSession) { refresh(); loadHistory() } else { loadWsGit(); loadWsGitHistory() } }

  // 提交成功（busy 从 git_commit 归零且无错误）→ 清空输入框；失败保留供修改重试
  const prevBusyRef = useRef<string | undefined>(undefined)
  useEffect(() => {
    if (prevBusyRef.current === 'git_commit' && !gitOpBusy && !gitOpError) setCommitMsg('')
    prevBusyRef.current = gitOpBusy
  }, [gitOpBusy, gitOpError])

  const doCommit = () => {
    const msg = commitMsg.trim()
    if (!msg || gitOpBusy) return
    if (!stagedFiles.length && !stageAll) return // 没有可提交内容
    gitCommitOp(msg, stageAll)
  }

  if (loading) return <div className="empty"><div className="big">{t('rp.git.loading')}</div></div>
  if (error) {
    return (
      <div className="empty">
        <div className="big">{t('rp.git.loadFailed')}</div>
        <div className="hint">{error}</div>
        <button className="git-more" onClick={refreshAll}>{t('rp.git.retry')}</button>
      </div>
    )
  }
  if (!git.is_repo) {
    return (
      <div className="empty">
        <div className="big">{t('rp.git.notRepo')}</div>
        <div className="hint">{t('rp.git.notRepoHint')}</div>
        <button className="git-more" disabled={!!gitOpBusy} onClick={() => gitInitRepo()}>
          {gitOpBusy === 'git_init' ? t('rp.git.initBusy') : t('rp.git.init')}
        </button>
      </div>
    )
  }

  const branchLabel = git.branch || (git.head ? `detached@${git.head}` : 'HEAD')

  const stagedFiles = gitFiles.filter((f) => f.staged)
  const conflictFiles = gitFiles.filter((f) => f.status === 'conflicted')
  const untrackedFiles = gitFiles.filter((f) => !f.staged && f.status === 'untracked')
  const workFiles = gitFiles.filter((f) => !f.staged && f.status !== 'conflicted' && f.status !== 'untracked')
  const restageable = [...workFiles, ...untrackedFiles]
  const ft = filter.trim().toLowerCase()
  const fileKey = (f: GitFile) => (f.staged ? `s:${f.path}` : f.path)
  const fileDiff = (f: GitFile) => (hasSession ? gitFiles.find((x) => x.path === f.path)?.diff : fileDiffs[f.path])

  const toggleFile = (f: GitFile) => {
    if (f.status === 'conflicted') {
      // 冲突文件：跳转文件预览直接查看冲突标记（<<<<<<< / ======= / >>>>>>>）
      openFilePreview(f.path)
      return
    }
    const key = fileKey(f)
    if (openPath === key) { setOpenPath(null); return }
    setOpenPath(key)
    if (hasSession) loadGitDiff(f.path, f.staged)
    else loadWsFileDiff(f.path, f.staged)
  }
  const toggleCommit = (hash: string) => {
    if (openCommit === hash) { setOpenCommit(null); return }
    setOpenCommit(hash)
    const c = commits.find((x) => x.hash === hash)
    if (!c?.diff) { if (hasSession) loadCommitDiff(hash); else loadWsCommitDiff(hash) }
  }

  const renderGroup = (title: string, list: GitFile[]) => {
    const shown = ft ? list.filter((f) => f.path.toLowerCase().includes(ft)) : list
    if (shown.length === 0) return null
    return (
      <>
        <div className="git-sub-title">{title} · {shown.length}</div>
        {shown.map((f) => {
          const key = fileKey(f)
          const open = openPath === key
          const diff = open ? fileDiff(f) : undefined
          const isUntracked = f.status === 'untracked'
          return (
            <div key={key} className="git-file-wrap">
              <div
                className={`git-file${open ? ' open' : ''}${f.status === 'conflicted' ? ' conflict' : ''}`}
                onClick={() => toggleFile(f)}
                title={f.status === 'conflicted' ? t('rp.git.conflicted') : f.path}
              >
                <span className={`git-file-status st-${f.status || 'modified'}${f.staged ? ' staged' : ''}`}>{fileStatusLabel((f.status || 'modified') as FileActivityStatus)}</span>
                <span className="git-file-path">{f.path}</span>
                <span className="git-file-stats mono">{f.additions > 0 ? `+${f.additions}` : ''}{f.deletions > 0 ? ` −${f.deletions}` : ''}{!f.additions && !f.deletions ? '·' : ''}</span>
                <span className="git-file-acts">
                  {f.staged && (
                    <button className="git-act" title={t('rp.git.unstage')} disabled={!!gitOpBusy} onClick={(e) => { e.stopPropagation(); gitUnstage([f.path]) }}>−</button>
                  )}
                  {!f.staged && f.status !== 'conflicted' && (
                    <button className="git-act" title={t('rp.git.stage')} disabled={!!gitOpBusy} onClick={(e) => { e.stopPropagation(); gitStage([f.path]) }}>+</button>
                  )}
                  {!f.staged && f.status !== 'conflicted' && (
                    <button className="git-act danger" title={isUntracked ? t('rp.git.deleteUntracked') : t('rp.git.discard')} disabled={!!gitOpBusy} onClick={(e) => { e.stopPropagation(); setConfirmDiscard(f) }}>↩</button>
                  )}
                  <button className="git-act" title={t('rp.git.openPreview')} onClick={(e) => { e.stopPropagation(); openFilePreview(f.path) }}>⤢</button>
                </span>
              </div>
              {open && <div className="git-diff">{diff ? <UnifiedDiff unified={diff} /> : <div className="git-diff-loading">{t('rp.git.loadingDiff')}</div>}</div>}
            </div>
          )
        })}
      </>
    )
  }

  return (
    <div className="git-panel">
      <div className="git-header">
        <GitBranch />
        <button
          className="git-branch-btn"
          title={t('rp.git.switchBranch')}
          onClick={() => { setBranchOpen((v) => { if (!v) loadGitBranches(); return !v }) }}
        >
          <strong className="git-branch-name">{branchLabel}</strong>
          <span className="git-branch-caret">▾</span>
        </button>
        {(git.ahead > 0 || git.behind > 0) && (
          <span className="git-ab mono">{git.ahead > 0 ? `↑${git.ahead}` : ''}{git.behind > 0 ? ` ↓${git.behind}` : ''}</span>
        )}
        <span className="grow" />
        <button className="git-refresh" onClick={refreshAll} title={t('rp.git.refresh')}>↻</button>
      </div>
      <div className="git-summary mono">{git.root || ''}</div>

      {branchOpen && (
        <>
          <div className="git-pop-backdrop" onClick={() => setBranchOpen(false)} />
          <div className="git-branch-pop">
            <div className="git-branch-pop-title">{t('rp.git.localBranches')}</div>
            {(gitBranches ?? []).length === 0 && <div className="git-empty">{gitOpBusy === 'git_branch_list' ? t('rp.git.loadingBranches') : t('rp.git.noBranches')}</div>}
            {(gitBranches ?? []).map((b) => (
              <button
                key={b.name}
                className={`git-branch-item${b.current ? ' current' : ''}`}
                disabled={b.current || !!gitOpBusy}
                onClick={() => { setBranchOpen(false); gitCheckout(b.name) }}
              >
                <span className="git-branch-item-name">{b.name}</span>
                {b.current && <span className="git-branch-item-check">✓</span>}
              </button>
            ))}
          </div>
        </>
      )}

      {gitOpError && (
        <div className="git-op-error">
          <span>{gitOpError}</span>
          <button className="git-op-error-x" onClick={clearGitOpError} title={t('rp.git.close')}>✕</button>
        </div>
      )}

      <div className="git-section">
        <div
          className="git-section-title as-toggle"
          role="button"
          aria-expanded={secOpen.files}
          onClick={() => setSecOpen((v) => ({ ...v, files: !v.files }))}
        >
          <span className={`git-sec-caret${secOpen.files ? ' open' : ''}`}>▾</span>
          <span>{t('rp.git.changedFiles').replace('{n}', String(gitFiles.length))}</span>
          <span className="grow" />
          {secOpen.files && restageable.length > 0 && (
            <button
              className="git-title-act"
              disabled={!!gitOpBusy}
              onClick={(e) => { e.stopPropagation(); gitStage(restageable.map((f) => f.path)) }}
            >
              {t('rp.git.stageAll')}
            </button>
          )}
        </div>
        {secOpen.files && (
          <>
            {gitFiles.length === 0 && <div className="git-empty">{t('rp.git.clean')}</div>}
            {gitFiles.length >= 20 && (
              <input className="git-filter" placeholder={t('rp.git.filterPh')} value={filter} onChange={(e) => setFilter(e.target.value)} spellCheck={false} />
            )}
            {renderGroup(t('rp.git.staged'), stagedFiles)}
            {renderGroup(t('rp.git.unstaged'), workFiles)}
            {renderGroup(t('rp.git.untracked'), untrackedFiles)}
            {renderGroup(t('rp.git.conflicts'), conflictFiles)}
          </>
        )}
      </div>

      {(stagedFiles.length > 0 || stageAll) && secOpen.files && (
        <div className="git-commit-box">
          <textarea
            className="git-commit-input"
            placeholder={t('rp.git.commitPh')}
            value={commitMsg}
            onChange={(e) => setCommitMsg(e.target.value)}
            onKeyDown={(e) => { if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') doCommit() }}
            rows={2}
            spellCheck={false}
          />
          <div className="git-commit-acts">
            <label className="git-stage-all-toggle">
              <input type="checkbox" checked={stageAll} onChange={(e) => setStageAll(e.target.checked)} />
              {t('rp.git.includeUnstaged').replace('{n}', String(gitFiles.length - stagedFiles.length))}
            </label>
            <span className="grow" />
            <button
              className="btn primary git-commit-btn"
              disabled={(!stagedFiles.length && !stageAll) || !!gitOpBusy || !commitMsg.trim()}
              onClick={doCommit}
            >
              {gitOpBusy === 'git_commit' ? t('rp.git.commitBusy') : t('rp.git.commit')}
            </button>
          </div>
        </div>
      )}

      <div className="git-section">
        <div
          className="git-section-title as-toggle"
          role="button"
          aria-expanded={secOpen.history}
          onClick={() => setSecOpen((v) => ({ ...v, history: !v.history }))}
        >
          <span className={`git-sec-caret${secOpen.history ? ' open' : ''}`}>▾</span>
          <span>{t('rp.git.history').replace('{n}', String(commits.length))}</span>
        </div>
        {secOpen.history && (
          <>
            {commits.length === 0 && <div className="git-empty">{t('rp.git.noCommits')}</div>}
            {commits.map((c) => (
              <div key={c.hash} className="git-commit-wrap">
                <div className={`git-commit${openCommit === c.hash ? ' open' : ''}`} onClick={() => toggleCommit(c.hash)}>
                  <div className="git-commit-line"><GitGraph g={c.graph} /><span className="mono git-commit-hash">{c.short_hash}</span><span className="git-commit-subject">{c.subject}</span></div>
                  {c.graph_after && <div className="git-graph-after mono">{c.graph_after}</div>}
                  <div className="git-commit-meta">{c.author} · {fmtGitDate(c.date)}</div>
                </div>
                {openCommit === c.hash && (
                  <div className="git-diff">{c.diff ? <UnifiedDiff unified={c.diff} /> : <div className="git-diff-loading">{t('rp.git.loadingCommitDiff')}</div>}</div>
                )}
              </div>
            ))}
            {hasMore && (
              <button className="git-more" disabled={histLoading} onClick={() => (hasSession ? loadHistory(true) : loadWsGitHistory(true))}>
                {histLoading ? t('rp.git.loadingMore') : t('rp.git.loadMore')}
              </button>
            )}
          </>
        )}
      </div>

      {confirmDiscard && (
        <ConfirmModal
          title={confirmDiscard.status === 'untracked' ? t('rp.git.deleteUntracked') : t('rp.git.discardTitle')}
          message={
            confirmDiscard.status === 'untracked'
              ? t('rp.git.discardMsgUntracked').replace('{path}', confirmDiscard.path)
              : t('rp.git.discardMsg').replace('{path}', confirmDiscard.path)
          }
          confirmLabel={confirmDiscard.status === 'untracked' ? t('rp.git.delete') : t('rp.git.discard2')}
          cancelLabel={t('rp.git.cancel')}
          danger
          onConfirm={() => {
            gitDiscard([confirmDiscard.path], confirmDiscard.status === 'untracked')
            if (openPath === fileKey(confirmDiscard)) setOpenPath(null)
            setConfirmDiscard(null)
          }}
          onCancel={() => setConfirmDiscard(null)}
        />
      )}
    </div>
  )
}


// 文件扩展名 → shiki 语言（未知回退 text）





