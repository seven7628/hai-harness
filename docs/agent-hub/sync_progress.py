#!/usr/bin/env python3
"""把 tasks.json 同步进 PROGRESS.html 的内嵌 BASE 数据。

用法：
    python3 docs/agent-hub/sync_progress.py

PROGRESS.html 是自包含的（file:// 直接打开，不 fetch），所以任务定义以 JSON 形式内嵌。
任务增删/改标题/改验收时，改 tasks.json 后跑这个脚本；进度仍由浏览器 localStorage 管。
"""
import json
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parent
TASKS = ROOT / "tasks.json"
HTML = ROOT / "PROGRESS.html"

base = json.loads(TASKS.read_text())

# 看板 UI 读的是顶层 progress 覆盖层，不是 tasks[] 里的 status/review。
# 新增/收口任务如果只改了任务卡，UI 仍会显示「未 Review」——这里把缺失的
# progress 条目从任务卡镜像过去，避免同一事实写两遍还漏一处。
progress = base.setdefault("progress", {})
seeded = []
for task in base.get("tasks", []):
    tid = task.get("id")
    if not tid or tid in progress:
        continue
    if task.get("status") not in ("in_progress", "review", "done", "blocked") and not task.get("review"):
        continue
    progress[tid] = {k: task[k] for k in ("status", "commit", "review", "reviewNote") if task.get(k)}
    seeded.append(tid)
if seeded:
    TASKS.write_text(json.dumps(base, ensure_ascii=False, indent=2) + "\n")
    print("mirrored task-level progress: " + ", ".join(seeded))

html = HTML.read_text()

block = "const BASE = " + json.dumps(base, ensure_ascii=False, indent=2) + ";\nconst STORE_KEY"
new, n = re.subn(r"const BASE = \{.*?\};\nconst STORE_KEY", block, html, count=1, flags=re.S)
if n != 1:
    raise SystemExit("PROGRESS.html 里没有找到 `const BASE = {...};` 块")

HTML.write_text(new)
print(f"synced {len(base.get('tasks', []))} tasks -> {HTML}")
