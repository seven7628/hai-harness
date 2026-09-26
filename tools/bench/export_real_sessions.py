#!/usr/bin/env python3
"""从真实 session 日志导出 slim 基准夹具。

会话 jsonl 里 `state` 记录持有完整上下文消息序列（`history` 记录只是单轮片段），
本脚本取每个会话最大的一条 state.messages，筛掉过小的，导出为 agents 包可读的
JSON 夹具。字段名与 core.Message 的 json tag 对齐，故 Go 侧可直接反序列化。

用法:
    python3 scripts/export_real_sessions.py [--out .testdata/real_sessions.json] [--min-messages 200] [--max-sessions 6]
"""
import argparse
import glob
import json
import os
import sys

DEFAULT_GLOB = os.path.expanduser("~/.go-code/sessions/*/*.jsonl")


def content_bytes(messages):
    n = 0
    for m in messages:
        for c in m.get("content") or []:
            n += len(c.get("content") or "")
        for tc in m.get("tool_calls") or []:
            n += len(tc.get("name", "")) + len(tc.get("arguments", "")) + len(tc.get("id", ""))
    return n


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--glob", default=DEFAULT_GLOB)
    ap.add_argument("--out", default=".testdata/real_sessions.json")
    ap.add_argument("--min-messages", type=int, default=200)
    ap.add_argument("--max-sessions", type=int, default=6)
    args = ap.parse_args()

    files = glob.glob(args.glob)
    if not files:
        sys.exit(f"未找到会话文件: {args.glob}")
    # 先看大文件：长会话的重复率才接近真实负载
    files.sort(key=lambda p: -os.path.getsize(p))

    out = []
    for path in files:
        if len(out) >= args.max_sessions:
            break
        best = None
        try:
            with open(path, errors="ignore") as f:
                for line in f:
                    if '"state"' not in line:
                        continue
                    try:
                        o = json.loads(line)
                    except Exception:
                        continue
                    msgs = (o.get("state") or {}).get("messages") or []
                    if best is None or len(msgs) > len(best):
                        best = msgs
        except OSError:
            continue
        if not best or len(best) < args.min_messages:
            continue
        roles = {}
        for m in best:
            roles[m.get("role")] = roles.get(m.get("role"), 0) + 1
        out.append({
            "file": os.path.basename(path),
            "messages": best,
            "_stats": {"messages": len(best), "roles": roles, "bytes": content_bytes(best)},
        })
        print(f"  {os.path.basename(path)}: {len(best)} msgs {roles}", file=sys.stderr)

    os.makedirs(os.path.dirname(args.out) or ".", exist_ok=True)
    with open(args.out, "w", encoding="utf-8") as f:
        json.dump(out, f, ensure_ascii=False)
    print(f"已写出 {args.out}（{len(out)} 个会话）", file=sys.stderr)


if __name__ == "__main__":
    main()
