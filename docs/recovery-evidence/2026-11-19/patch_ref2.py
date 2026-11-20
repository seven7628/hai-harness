# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
def rep(old,new):
    global s
    assert old in s, old[:90]
    s=s.replace(old,new,1)

# Resolve helper
rep('''// Drop 会话删除时清掉它的全部引用（引用是会话级状态，没有跨会话价值）。''','''// Resolve 取一组 refId（发送时调用）：逐条校验属主/过期，并保持用户点选的顺序。
// 空 ids 返回 nil（不启用选区，行为与改造前一致）。
func (s *selectionRefStore) Resolve(ws, sid string, ids []string) ([]*selectionRef, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > 5 {
		return nil, errors.New("at most 5 selection refs per message")
	}
	out := make([]*selectionRef, 0, len(ids))
	for _, id := range ids {
		r, err := s.Get(ws, sid, id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Drop 会话删除时清掉它的全部引用（引用是会话级状态，没有跨会话价值）。''')

# main.go edit count + E-main.go-12
rep("### 21.10 `desktop/bridge/main.go`（11 处）+ `command_registry.go`（1 处）",
    "### 21.10 `desktop/bridge/main.go`（12 处）+ `command_registry.go`（1 处）")
rep("| `desktop/bridge/main.go` | 11 | A3/A4/B | §21.10 |",
    "| `desktop/bridge/main.go` | 12 | A3/A4/B | §21.10 |")

anchor="### 21.11 前端 store（`checkpoint.ts` 2 处、`useAppStore.ts` 6 处）"
addition = r'''#### E-main.go-12：`CmdAsk` / `CmdAskBatch` 解析 `selection_refs`

**定位**：`desktop/bridge/main.go:3250`（`case CmdAsk:` 的 `text := str(c.Payload, "text")` 之后）
与 `3273`（`CmdAskBatch` 同形）。

```go
	case CmdAsk:
		// …既有 get/忙检查…
		text := str(c.Payload, "text")
		refs, err := m.selectionRefs.Resolve(ws, sid, strSlice(c.Payload, "selection_refs"))
		if err != nil {
			m.respCode(c, false, err.Error(), "selection_ref_invalid", nil)
			return
		}
		// 有引用时：指令在前、选区在后，拼成**一条**用户消息（§23.3）。
		msg := buildSelectionMessage(text, refs)
		if err := bs.s.Ask(ctx, msg); err != nil { … }
```

`strSlice` 是既有 payload 取值族的补充（`command_registry.go` 或 `main.go` 的 str/int/bool 旁边）：

```go
// strSlice 读 payload 里的字符串数组；缺失/类型不符一律返回 nil（调用方按「没传」处理）。
func strSlice(p map[string]any, key string) []string {
	raw, ok := p[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
```

> `CmdAskBatch` 的 payload 是 `messages` 数组：**第一条**消息带引用（用户一次只能选一次），
> 其余消息不带。不要给每条都拼同一份选区（模型会以为用户选了多次）。

'''
rep(anchor, addition+anchor)
open(p,'w').write(s)
print('ok')
