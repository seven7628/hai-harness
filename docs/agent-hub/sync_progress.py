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
html = HTML.read_text()

block = "const BASE = " + json.dumps(base, ensure_ascii=False, indent=2) + ";\nconst STORE_KEY"
new, n = re.subn(r"const BASE = \{.*?\};\nconst STORE_KEY", block, html, count=1, flags=re.S)
if n != 1:
    raise SystemExit("PROGRESS.html 里没有找到 `const BASE = {...};` 块")

HTML.write_text(new)
print(f"synced {len(base.get('tasks', []))} tasks -> {HTML}")
