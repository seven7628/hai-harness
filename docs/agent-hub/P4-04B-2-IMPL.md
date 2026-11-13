# P4-04b-2 实现契约：Hub 冷归档

> 设计来源：`P4-04B-DESIGN.md` §2。04b-2 只处理群日志 `groups/<gid>/log.jsonl`
> 与 DM 索引 `dms/<aid>/log.jsonl`；activity、session checkpoint、artifact 不动。
> **删除默认关闭，不实现自动删除。** 本阶段只做冷归档与透明读写。

## 1. 目录与 manifest

```
archive/groups/<gid>/manifest.json
archive/groups/<gid>/<startSeq>-<endSeq>.jsonl
archive/dms/<aid>/manifest.json
archive/dms/<aid>/<startSeq>-<endSeq>.jsonl
```

manifest（每个会话一份，原子写）：

```json
{
  "schema_version": 1,
  "scope": "groups",
  "id": "grp_x",
  "segments": [
    {
      "start_seq": 1,
      "end_seq": 14000,
      "count": 14000,
      "bytes": 1234567,
      "sha256": "hex",
      "file": "1-14000.jsonl",
      "archived_at": "2026-11-09T..."
    }
  ],
  "updated_at": "..."
}
```

不可变段：文件只写一次、写后 `chmod 0400`；manifest 追加段。段按 `start_seq`
升序、互不重叠。允许 seq 有空洞（手工修复/未来删除），但同一 seq 不得出现在
两个段或同时出现在热+冷里（读侧仍按 seq 去重，热优先）。

## 2. 阈值与策略

`ArchivePolicy` 默认：

- `MaxMessages = 20000`：热日志消息数 **>** 20000 触发；
- `MaxBytes = 50 << 20`：热日志文件 **>** 50MB 触发；
- `KeepRecent = 2000`：热日志至少保留最近 2000 条；
- `Percent = 70`：本次归档热日志最旧的 70%。

本次归档条数 = `min(count*Percent/100, count-KeepRecent)`，必须 >0。按字节触发
但 count<=KeepRecent 时不做任何事（不能为降体积破坏 2k 保留承诺）。归档后若仍
超体积但已到 2k 下限，保持原状并报告 skipped，不自动删除。

## 3. 归档算法（失败不丢消息）

在 `FileStore.archiveMu`（串行归档）+ `s.mu`（快照/热日志重写）下：

1. 读热日志（tolerant tail），读 manifest；先把热日志里**已经存在于 manifest 段**
   的 seq 去掉（修复“manifest 已提交、热日志未删”的崩溃窗口）。
2. 重新判断阈值；选出最旧的 N 条；`start/end` 从实际消息取。
3. **不持 s.mu** 写段：临时文件 → 写 JSONL → fsync → rename 到最终名 → fsync 目录
   → chmod 0400。若最终文件已存在：内容逐字节相同则复用（幂等），否则
   `ErrConflict`，绝不覆盖。
4. 持有 `s.mu`：重读 manifest，校验新段不与已有段重叠；原子写 manifest（临时文件
   + fsync + rename + fsync 目录）。**此时还没有删热日志**。
5. 仍持 `s.mu`：把热日志重写为“不在任何 manifest 段里的消息”（临时文件 + fsync
   + rename + fsync 目录）。这里可能包含第 3 步之后新 append 的消息，必须全部保留。
6. 成功后才更新 `s.seq` / `s.dmSeq`（实际上 seq 不回退）；失败返回错误，热日志
   原文保留或已由原子 rename 保护。

崩溃点：
- 段写完、manifest 前：孤儿段，无 manifest 不参与读取；下次复用。
- manifest 后、热日志重写前：同 seq 同时在热+冷；读侧按 seq 去重，下一次归档先修复。
- 热日志重写失败：manifest 已提交，热原文仍在；不丢消息，只重复。

## 4. 透明读写矩阵（FileStore）

所有以下路径都必须把热日志 + 所有 archive 段合并，按 seq 升序、同 seq 去重（热优先）：

| 路径 | 要求 |
|---|---|
| `ReadMessages`（群） | 返回 `seq < beforeSeq` 的最新 limit 条；按段从新到旧读，凑够即停 |
| `ReadDirectMessages` | 同上（DM） |
| `SearchMessages`（agent-safe） | 搜索热+冷；保持 `VisibleTo(false)` + `AgentContextKind` 过滤 |
| `SearchMessagesHuman` | 搜索热+冷；保持 owner-only 可见 + 噪声排除 |
| `SearchDirectMessages` | 同上（DM） |
| `FindGroupMessageByClientID` | 热找不到再搜冷（幂等重发跨归档） |
| `FindDirectMessageByClientID` | 同上 |
| `AppendMessageWithRound` 的 client_msg_id 去重 | 热找不到再搜冷；不能跨归档重复写 |
| `DMUnread` | 未读数/最后一条跨热+冷 |
| `OpenFileStore.rebuildIndex` | 每个会话的 seq 取 `max(热最大 seq, manifest 最大 end_seq)`；只有冷段、热文件为空时也不能从 1 重号 |

`ReadMessages`/`ReadDirectMessages` 不要为了取 50 条无条件加载全部段：按
manifest 段从新到旧，`seq < beforeSeq` 的段才读，凑够 limit 停止。搜索要全量扫
（v1 无 FTS）。读取段时校验 `bytes` + `sha256`；不匹配返回 `ErrCorrupt`。

## 5. 启动与 seq

`OpenFileStore` 的 `rebuildIndex` 必须同时扫描 `archive/*/*/manifest.json`：

- manifest schema 过新/损坏/段文件缺失/段重叠 → `ErrCorrupt`（fail-fast）；
- 段内容损坏在读取时返回 `ErrCorrupt`；
- 同一 ID 的 `scope`/`id` 必须与目录一致；
- 热日志被完全归档后，重启仍从 `max(end_seq)+1` 分配。

## 6. 后台触发（bridge）

- `FileStore.ArchiveIfNeeded(ctx)` 扫未删除群 + 智能体 DM，逐个 `ArchiveGroup`/`ArchiveDM`；
  单会话失败不阻塞其它会话，返回汇总错误。
- bridge 在 `hubStore()` 首次打开后启动一个 Hub 后台 ticker（默认 5 分钟，测试可注入短间隔）；
  启动时先跑一次，随后周期跑。使用现有 `spawnHubBackground` / `stopHubBackground` 生命周期，
  不新增裸 goroutine。
- 归档失败通过现有 `hub_alert` 出口发 `hub_archive_failed`（同一 code 去重，避免每 tick 刷屏）；
  成功不刷 UI 事件。**不自动删除**。
- 不新增 agent 工具；归档是存储维护，不是模型能力。

## 7. 测试门槛（至少）

1. 阈值：20,001 条 / 2k 保留 / 70% 归档；按字节触发但 <=2k 时 skipped。
2. 归档后热文件只剩预期条数；manifest 段字段正确；文件 0400。
3. 读：热+冷边界、跨段、beforeSeq、limit、去重；完全冷归档后仍能读。
4. 搜：群/DM 热+冷；agent-safe 的 owner-only/噪声过滤不退化；人类 owner-only 可见。
5. 删除不自动发生：归档后 artifact/activity 不动；无任何删除调用。
6. 崩溃窗口：
   - 段存在 manifest 不存在 → 重新归档复用，不重复写；
   - manifest 已提交、热未删 → 读不重复，下一次归档修复热日志；
   - manifest 段缺失/字节/hash 不匹配 → ErrCorrupt。
7. 重启 seq：全部归档 + 热空 → 新 append 的 seq 不重号；热+冷都有时取最大。
8. client_msg_id：消息归档后重发仍 dedupe（群/DM）。
9. 并发：append 与 archive 并发，归档期间新增消息不丢；`-race` 通过。
10. bridge：短间隔 ticker 实际触发归档；shutdown 可停；失败发一次 hub_alert；无删除。
11. 性能：10k 消息归档后搜索/读路径 p95 不出现数量级回退；归档 20k 条耗时有日志。

## 8. 非目标

- 不实现自动删除/保留期 UI；删除永远默认关闭。
- 不改 artifact 生命周期；不做 FTS/索引；不做跨设备同步。
- 不改 agent-safe 搜索的可见性口径。
