// 文件活动聚合：来自对话 blocks 的 Write/Edit 操作 → 活动列表（右栏「文件」面板用）。
// 从 RightPanel 抽出（PR-04 的 provider 化），类型与聚合函数放在一起，避免双向 import。
import type { Diff, MsgBlock, GitFile } from '../store/useAppStore'
import type { FileActivityStatus } from './fileStatus'
import { normalizeFilePath } from './filePaths'

export type FileOperation = {
  toolId: string
  tool: string
  result: string
  diff?: Diff
  time?: number
  durMs?: number
  isError?: boolean
}

export type FileActivity = {
  path: string
  status: FileActivityStatus
  additions: number
  deletions: number
  changed: boolean
  lastAt: number
  operations: FileOperation[]
  git?: GitFile
}


export function aggregateFileActivity(blocks: MsgBlock[], gitFiles: GitFile[]): FileActivity[] {
  const byPath = new Map<string, FileActivity>()
  const ensure = (path: string) => {
    const normalized = normalizeFilePath(path)
    let item = byPath.get(normalized)
    if (!item) {
      item = { path: normalized, status: 'read', additions: 0, deletions: 0, changed: false, lastAt: 0, operations: [] }
      byPath.set(normalized, item)
    }
    return item
  }

  for (const block of blocks) {
    if (block.kind !== 'tool') continue
    // run_python 的 args 是**脚本源码**（没有 path 字段），产物路径只能取 Go 侧在产出点写的
    // Diff.path —— 有它才说明这次脚本动了文件，才与 write/edit 同等待遇；没有就说明这个调用
    // 只是算了点东西，不进文件活动列表（否则每跑一次脚本都会多出一条无路径条目）。
    const pyWrote = block.name === 'run_python' && !!block.diff?.path
    if (!pyWrote && !['read_file', 'write_file', 'edit_file'].includes(block.name)) continue
    let path = pyWrote ? String(block.diff?.path ?? '') : ''
    if (!path) {
      try { path = String(JSON.parse(block.args || '{}').path || '') } catch { /* ignore malformed tool args */ }
    }
    if (!path) continue
    const item = ensure(path)
    item.operations.push({ toolId: block.toolId, tool: block.name, result: block.result || '', diff: block.diff, time: block.startedAt, durMs: block.durMs, isError: block.isError })
    item.lastAt = Math.max(item.lastAt, block.startedAt ?? 0)
    // pyWrote 与 write/edit 同等待遇：Go 侧既然认了「脚本改了这个文件」，它在文件栏里就是变更态
    //（而不是只读列表里的一条记录）。
    if (!block.isError && block.status === 'done' && (pyWrote || block.name === 'write_file' || block.name === 'edit_file')) {
      item.changed = true
      item.status = 'modified'
    }
    if (block.diff) {
      item.additions += block.diff.added
      item.deletions += block.diff.removed
    }
  }

  for (const file of gitFiles) {
    const item = ensure(file.path)
    item.git = file
    item.changed = true
    item.status = (['added', 'modified', 'deleted', 'conflicted', 'untracked'].includes(file.status)
      ? file.status
      : 'modified') as FileActivityStatus
    if (item.additions === 0 && item.deletions === 0) {
      item.additions = file.additions || 0
      item.deletions = file.deletions || 0
    }
    item.lastAt = Math.max(item.lastAt, file.changedAt ?? 0)
  }

  return [...byPath.values()].sort((a, b) => Number(b.changed) - Number(a.changed) || b.lastAt - a.lastAt || a.path.localeCompare(b.path))
}

