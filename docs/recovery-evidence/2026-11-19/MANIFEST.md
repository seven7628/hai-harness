# 恢复证据归档 —— 2026-11-19 误删事故

> 归档时间：2026-11-20 01:14
>
> ⚠️ **这些文件原本只存在于 `/private/tmp`（系统临时目录，会被定期清理）。
> 它们是本次事故恢复的唯一依据——补丁脚本里的字符串字面量就是丢失文档的正文。
> 一旦 `/private/tmp` 被清空，`docs/` 下那些恢复稿就再也无法复核或二次挖掘。**
>
> 共 108 个文件，1,602,481 字节。以下 sha256 可用于校验未被篡改。

## 文件清单

| 文件 | 大小 | mtime | 用途 | sha256（前 16） |
|---|---|---|---|---|
| `add_handler.py` | 4,577 | 11-19 21:25 | 补丁脚本 / 素材 | `f36527e5abf27f90` |
| `add_inspector.py` | 4,900 | 11-19 21:59 | 补丁脚本 / 素材 | `3bb20b7cf3cd404f` |
| `add_kinds.py` | 5,606 | 11-19 21:57 | 补丁脚本 / 素材 | `12478bb664c3b80d` |
| `add_mig.py` | 1,492 | 11-19 21:28 | 补丁脚本 / 素材 | `17e197c16ad329ba` |
| `add_sweepfns.py` | 1,714 | 11-19 21:27 | 补丁脚本 / 素材 | `54bf468bc41770bc` |
| `cwd_baseline.json` | 246 | 11-19 18:50 | 补丁脚本 / 素材 | `83c6cd4e0c009190` |
| `cwd_s11.md` | 25,212 | 11-19 18:48 | AGENT_CWD_DESIGN 恢复（tbody/修改） | `d050b96e399fe39a` |
| `cwd_s12.md` | 23,780 | 11-19 18:49 | AGENT_CWD_DESIGN 恢复（tbody/修改） | `1d150aa6da03daf7` |
| `cwd_s14.md` | 20,052 | 11-19 19:05 | AGENT_CWD_DESIGN 恢复（tbody/修改） | `5c483ddce88b8325` |
| `cwd_s15.md` | 11,157 | 11-19 19:11 | AGENT_CWD_DESIGN 恢复（tbody/修改） | `941377f66aa76ef9` |
| `cwd_s16.md` | 5,991 | 11-19 20:05 | AGENT_CWD_DESIGN 恢复（tbody/修改） | `31f43f30fbfb01b0` |
| `cwd_s17.md` | 4,374 | 11-19 21:14 | AGENT_CWD_DESIGN 恢复（tbody/修改） | `9908191809822b2f` |
| `dbg-term.mjs` | 2,322 | 11-19 19:01 | 终端调试脚本 | `d9ba075da61d037f` |
| `fix_nested.py` | 1,654 | 11-19 21:58 | 补丁脚本 / 素材 | `41758691c3a01925` |
| `fix_test_helper.py` | 1,446 | 11-19 23:26 | 补丁脚本 / 素材 | `7da8ebd7c9a51448` |
| `main.go.bak` | 392,659 | 11-19 19:53 | desktop/bridge/main.go 原件（含 S7 改动） | `a6e9836c70e76a3f` |
| `main.go.bak2` | 392,659 | 11-19 19:55 | desktop/bridge/main.go 原件（含 S7 改动） | `a6e9836c70e76a3f` |
| `main.go.bak3` | 393,971 | 11-19 22:05 | desktop/bridge/main.go 原件（含 S7 改动） | `9e53b12c2d5b05cc` |
| `mkindex.py` | 3,052 | 11-19 23:32 | 补丁脚本 / 素材 | `00ef150cd31b3809` |
| `mkindex2.py` | 9,789 | 11-19 23:32 | 补丁脚本 / 素材 | `0ac827aea3516eac` |
| `part22.md` | 28,351 | 11-19 21:21 | ARTIFACT 文档 §22+§23+§24 全文 | `2b29619cd98a8d91` |
| `patch10.py` | 1,187 | 11-19 20:39 | ARTIFACT 文档恢复（rep 原块→新块） | `d91f5cdcc381430f` |
| `patch11.py` | 3,348 | 11-19 20:39 | ARTIFACT 文档恢复（rep 原块→新块） | `242f3b68ed380be3` |
| `patch12.py` | 1,614 | 11-19 20:40 | ARTIFACT 文档恢复（rep 原块→新块） | `68272005485cb56f` |
| `patch13.py` | 2,001 | 11-19 20:40 | ARTIFACT 文档恢复（rep 原块→新块） | `e32a1f634ccb67fa` |
| `patch14.py` | 2,803 | 11-19 20:41 | ARTIFACT 文档恢复（rep 原块→新块） | `090d1d1e000083c1` |
| `patch15.py` | 738 | 11-19 20:42 | ARTIFACT 文档恢复（rep 原块→新块） | `7cf44b618dd7aef3` |
| `patch16.py` | 2,232 | 11-19 20:42 | ARTIFACT 文档恢复（rep 原块→新块） | `095089b2cb406b0c` |
| `patch17.py` | 1,668 | 11-19 20:43 | ARTIFACT 文档恢复（rep 原块→新块） | `22452a83a9998cde` |
| `patch2.py` | 5,569 | 11-19 20:34 | ARTIFACT 文档恢复（rep 原块→新块） | `35af449f198ab2f1` |
| `patch3.py` | 3,281 | 11-19 20:35 | ARTIFACT 文档恢复（rep 原块→新块） | `f28d60077d951d67` |
| `patch4.py` | 2,476 | 11-19 20:36 | ARTIFACT 文档恢复（rep 原块→新块） | `4ed0c4e1f4c71cc3` |
| `patch5.py` | 1,310 | 11-19 20:36 | ARTIFACT 文档恢复（rep 原块→新块） | `e6841009c1cabf50` |
| `patch6.py` | 961 | 11-19 20:37 | ARTIFACT 文档恢复（rep 原块→新块） | `208c7473eaf6fd92` |
| `patch7.py` | 1,033 | 11-19 20:37 | ARTIFACT 文档恢复（rep 原块→新块） | `89e8b3331f716365` |
| `patch8.py` | 1,901 | 11-19 20:38 | ARTIFACT 文档恢复（rep 原块→新块） | `8b0ea4e028b42853` |
| `patch9.py` | 862 | 11-19 20:39 | ARTIFACT 文档恢复（rep 原块→新块） | `a54dbdab64072f93` |
| `patch_bash_comment.py` | 905 | 11-19 23:31 | ARTIFACT 文档恢复（rep 原块→新块） | `e7e40feec2aedb4e` |
| `patch_budget.py` | 631 | 11-18 23:59 | ARTIFACT 文档恢复（rep 原块→新块） | `a45a2853cde276c7` |
| `patch_calib.py` | 1,043 | 11-18 23:57 | ARTIFACT 文档恢复（rep 原块→新块） | `ac128a577026fc3b` |
| `patch_calib2.py` | 600 | 11-18 23:57 | ARTIFACT 文档恢复（rep 原块→新块） | `f8f905220e3238d0` |
| `patch_chip.py` | 1,932 | 11-19 21:58 | ARTIFACT 文档恢复（rep 原块→新块） | `a44196c778a4072f` |
| `patch_codemode_guard.py` | 535 | 11-19 23:31 | ARTIFACT 文档恢复（rep 原块→新块） | `8028bb5814ccd0ab` |
| `patch_codemode_helper.py` | 1,459 | 11-19 23:35 | ARTIFACT 文档恢复（rep 原块→新块） | `e09ba613309ca9dd` |
| `patch_codemode_helper2.py` | 1,200 | 11-19 23:35 | ARTIFACT 文档恢复（rep 原块→新块） | `d7e9380968e74dc8` |
| `patch_codemode_helper3.py` | 822 | 11-19 23:35 | ARTIFACT 文档恢复（rep 原块→新块） | `5c9865a007f6b80c` |
| `patch_cwd_doc.py` | 4,302 | 11-19 18:46 | AGENT_CWD_DESIGN 恢复（tbody/修改） | `011bbaf3d6335d96` |
| `patch_cwd_v3.py` | 4,158 | 11-19 19:11 | AGENT_CWD_DESIGN 恢复（tbody/修改） | `c52090c30e825c02` |
| `patch_cwd_v3b.py` | 5,543 | 11-19 19:12 | AGENT_CWD_DESIGN 恢复（tbody/修改） | `dda5c22b5c99b171` |
| `patch_cwdkey_guard.py` | 526 | 11-19 23:36 | AGENT_CWD_DESIGN 恢复（tbody/修改） | `037a6a42c7c31f98` |
| `patch_desc.py` | 3,743 | 11-19 23:29 | ARTIFACT 文档恢复（rep 原块→新块） | `9a90ab4aa3292eea` |
| `patch_doc.py` | 6,137 | 11-19 20:34 | ARTIFACT 文档恢复（rep 原块→新块） | `e7a6e98d7dad0e73` |
| `patch_doc_header.py` | 1,402 | 11-19 04:44 | ARTIFACT 文档恢复（rep 原块→新块） | `ec6f078c810606d7` |
| `patch_drag.py` | 3,719 | 11-19 21:59 | ARTIFACT 文档恢复（rep 原块→新块） | `53121ca80d6ec7ab` |
| `patch_emit.py` | 779 | 11-19 03:23 | ARTIFACT 文档恢复（rep 原块→新块） | `a94e52cf9454edc5` |
| `patch_emit_sites.py` | 3,067 | 11-19 03:25 | ARTIFACT 文档恢复（rep 原块→新块） | `bd7a64231bd5980c` |
| `patch_events_fields.py` | 2,338 | 11-19 03:23 | ARTIFACT 文档恢复（rep 原块→新块） | `07ea84547812a792` |
| `patch_events_fields2.py` | 1,496 | 11-19 03:24 | ARTIFACT 文档恢复（rep 原块→新块） | `2f9629a718003016` |
| `patch_evict.py` | 2,175 | 11-18 23:55 | ARTIFACT 文档恢复（rep 原块→新块） | `0d456ed52af7378a` |
| `patch_explore_guard.py` | 942 | 11-19 23:33 | ARTIFACT 文档恢复（rep 原块→新块） | `832473e00c11696f` |
| `patch_final.py` | 3,954 | 11-19 21:21 | ARTIFACT 文档恢复（rep 原块→新块） | `5b9a2ec534ba66ad` |
| `patch_guard.py` | 1,975 | 11-19 21:18 | ARTIFACT 文档恢复（rep 原块→新块） | `cfa491981890dbb0` |
| `patch_iface.py` | 1,145 | 11-19 21:24 | ARTIFACT 文档恢复（rep 原块→新块） | `c5adb9b9af87e6dc` |
| `patch_kind2.py` | 4,270 | 11-19 21:57 | ARTIFACT 文档恢复（rep 原块→新块） | `05e678cb2f90ec55` |
| `patch_logical.py` | 1,269 | 11-19 00:20 | ARTIFACT 文档恢复（rep 原块→新块） | `07ef64fe970ed2fa` |
| `patch_marshaltail.py` | 2,311 | 11-19 02:05 | ARTIFACT 文档恢复（rep 原块→新块） | `566bef2b3f1ce42f` |
| `patch_memory.py` | 2,942 | 11-19 04:36 | ARTIFACT 文档恢复（rep 原块→新块） | `c6ff25f69e9fb50d` |
| `patch_meta.py` | 2,455 | 11-19 21:22 | ARTIFACT 文档恢复（rep 原块→新块） | `f252f97bbc6c0df2` |
| `patch_p1.py` | 9,293 | 11-19 19:59 | ARTIFACT 文档恢复（rep 原块→新块） | `457778c40d09b6d8` |
| `patch_paged_split.py` | 2,719 | 11-19 01:50 | ARTIFACT 文档恢复（rep 原块→新块） | `597db7499147a33f` |
| `patch_proto.py` | 2,526 | 11-19 01:00 | ARTIFACT 文档恢复（rep 原块→新块） | `e0eeeb16a03c6656` |
| `patch_pub.py` | 1,951 | 11-19 21:18 | ARTIFACT 文档恢复（rep 原块→新块） | `3826b4af93c52431` |
| `patch_readers.py` | 1,271 | 11-19 04:35 | ARTIFACT 文档恢复（rep 原块→新块） | `747932c6bdd0a446` |
| `patch_readers2.py` | 829 | 11-19 04:36 | ARTIFACT 文档恢复（rep 原块→新块） | `990d9cad1a7bb157` |
| `patch_reducer.py` | 3,791 | 11-19 02:03 | ARTIFACT 文档恢复（rep 原块→新块） | `673b56e762d5642b` |
| `patch_ref2.py` | 2,926 | 11-19 21:23 | ARTIFACT 文档恢复（rep 原块→新块） | `f021e866b9f08246` |
| `patch_review2.py` | 3,914 | 11-19 21:23 | ARTIFACT 文档恢复（rep 原块→新块） | `474cd3790a83b7ac` |
| `patch_s3.py` | 8,657 | 11-19 19:32 | ARTIFACT 文档恢复（rep 原块→新块） | `303423d8b52c522e` |
| `patch_s4.py` | 7,064 | 11-19 19:39 | ARTIFACT 文档恢复（rep 原块→新块） | `b9b72f177077f34e` |
| `patch_s5.py` | 5,817 | 11-19 19:47 | ARTIFACT 文档恢复（rep 原块→新块） | `6c6f3579013943b2` |
| `patch_s5_tests.py` | 1,414 | 11-19 19:48 | ARTIFACT 文档恢复（rep 原块→新块） | `9c36fde467d4250f` |
| `patch_s6.py` | 3,459 | 11-19 19:54 | ARTIFACT 文档恢复（rep 原块→新块） | `d166703434f27b08` |
| `patch_s7.py` | 9,672 | 11-19 21:56 | ARTIFACT 文档恢复（rep 原块→新块） | `2ab92dcb470676ec` |
| `patch_s7_apply.py` | 10,822 | 11-19 23:18 | ARTIFACT 文档恢复（rep 原块→新块） | `40c0d3f9b9de0eb8` |
| `patch_s7_apply2.py` | 6,279 | 11-19 23:18 | ARTIFACT 文档恢复（rep 原块→新块） | `bd5043d65d894c5a` |
| `patch_s7_apply3.py` | 1,013 | 11-19 23:18 | ARTIFACT 文档恢复（rep 原块→新块） | `e7f8928288672df2` |
| `patch_s7_apply4.py` | 1,917 | 11-19 23:20 | ARTIFACT 文档恢复（rep 原块→新块） | `3f455bbb008a367b` |
| `patch_s7_apply5.py` | 1,201 | 11-19 23:20 | ARTIFACT 文档恢复（rep 原块→新块） | `a385af2fc6b12c5b` |
| `patch_s7_apply6.py` | 2,505 | 11-19 23:21 | ARTIFACT 文档恢复（rep 原块→新块） | `e2631968ad6c829b` |
| `patch_s7_apply_all.py` | 853 | 11-19 23:26 | ARTIFACT 文档恢复（rep 原块→新块） | `3d400d7a114a32a8` |
| `patch_s7_tests.py` | 1,849 | 11-19 21:57 | ARTIFACT 文档恢复（rep 原块→新块） | `3ceb38905f6a8aeb` |
| `patch_s7b.py` | 4,274 | 11-19 22:06 | ARTIFACT 文档恢复（rep 原块→新块） | `1f101950b648bf3a` |
| `patch_sweep.py` | 1,295 | 11-19 21:27 | ARTIFACT 文档恢复（rep 原块→新块） | `1ffa6cfe043c21f0` |
| `patch_sweep_compact.py` | 1,611 | 11-19 03:39 | ARTIFACT 文档恢复（rep 原块→新块） | `aee611d3bafe4b53` |
| `patch_test_to_prod.py` | 1,897 | 11-19 01:49 | ARTIFACT 文档恢复（rep 原块→新块） | `50a28d3bdef77baf` |
| `patch_transient.py` | 2,146 | 11-19 00:19 | ARTIFACT 文档恢复（rep 原块→新块） | `c10af11bbe4f5f34` |
| `patch_transient2.py` | 873 | 11-19 00:20 | ARTIFACT 文档恢复（rep 原块→新块） | `0df088cc07dc9a78` |
| `patch_transient3.py` | 4,354 | 11-19 00:20 | ARTIFACT 文档恢复（rep 原块→新块） | `1c710b8cd4d83412` |
| `patch_unsafe.py` | 393 | 11-18 23:55 | ARTIFACT 文档恢复（rep 原块→新块） | `42b84b710f9ea78e` |
| `patch_winparse.py` | 5,442 | 11-19 02:02 | ARTIFACT 文档恢复（rep 原块→新块） | `27006f2fdae23181` |
| `patch_ws.py` | 6,700 | 11-19 21:17 | ARTIFACT 文档恢复（rep 原块→新块） | `ee3e5577a01ba37c` |
| `t_pty.py` | 807 | 11-19 05:48 | 终端 PTY 行为调研探针 | `99e38e964d740780` |
| `t_pty2.py` | 1,219 | 11-19 05:48 | 终端 PTY 行为调研探针 | `9de35f30971f2587` |
| `term-context.test.cjs` | 9,933 | 11-19 22:05 | 右栏终端 e2e 契约 | `fda1bff80a766c97` |
| `term-format.test.cjs` | 8,134 | 11-19 21:10 | 右栏终端 e2e 契约 | `cf261dd3ca8ecd2b` |
| `term-frame.test.cjs` | 4,452 | 11-19 21:10 | 右栏终端 e2e 契约 | `783c8d70401b46a7` |
| `term-pane.test.cjs` | 10,513 | 11-19 21:10 | 右栏终端 e2e 契约 | `922b918f0925e2e0` |
| `term-writer.test.cjs` | 8,933 | 11-19 21:10 | 右栏终端 e2e 契约 | `8dfb92a398cd029d` |

## 这些证据各自救回了什么

| 证据 | 救回的东西 | 保真度 |
|---|---|---|
| `patch*.py`（35 个，指向 ARTIFACT 文档） | 177 段代码块 + 28 条标题 + 82 行表格 + 261 条散文行 | 逐字 |
| `part22.md` | ARTIFACT 文档 §22/§23/§24 全文 580 行 | 逐字 |
| `cwd_s11/12/14/15/16/17.md` + `patch_cwd*.py` | AGENT_CWD_DESIGN 2,438 行（§7.3/8/9/11/12/14–17） | 逐字 |
| `main.go.bak3` | `desktop/bridge/main.go` 8130 行原件 | 逐字 |
| `t_pty*.py` | 右栏终端的 PTY 行为前提（登录 shell、别名、TERM） | 行为证据 |
| `term-*.test.cjs` | 终端前后端契约（帧/状态机/写入泵/命令追踪） | 契约 |

## 仍然找不回来的

- `docs/TERMINAL_DESIGN.md` 正文（直接写进仓库，无 /tmp 中转）
- ARTIFACT 文档 §1–§20 的标题与正文（补丁脚本从未引用）
- Go 侧 PTY 桥源码（从未落过 .bak）
- 两个 worktree 的 git 历史（分支从未推送）
