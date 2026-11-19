# Examples

Part of `create-mode`. Minimum shape and the agent/quote pattern.

## Minimum working example

```js
export const surface = {
  kind: 'plugin:hello',
  apply({ host, React, surfaces }) {
    const h = React.createElement
    surfaces.register({
      kind: 'plugin:hello',
      group: 'plugin',
      title: () => 'Hello',
      render: () => h('div', { style: { padding: 12, color: host.theme().tokens.fg } }, 'hi'),
    })
  },
}
```

with `surface.json`: `{"id":"hello","name":{"zh":"你好","en":"Hello"},"version":"1.0.0","kind":"plugin:hello","entry":"panel.js","commands":[],"data":[]}`.
(`name` must be an object; a bare string makes the real installer report a misleading JSON error.)

A panel that talks to the Agent and quotes into the draft (the shape `templates/surface` ships):

```json
{
  "commands": ["agent.session", "agent.run", "agent.cancel", "composer.quote", "ui.toast"],
  "data": ["agent.delta", "agent.done", "agent.error", "surfaces_changed"],
  "entries": [{"id": "main", "title": {"zh": "主面板", "en": "Main"}}]
}
```

For a declared entry the tab title comes from `entries[].title`; provider `title()` is for the
default (zero-entry) case and tabs the plugin opens itself.

```js
apply({ host, React }) {
  const h = React.createElement
  let sessionId = ''
  async function ask(input) {
    if (!sessionId) {
      const s = await host.command('agent.session', { title: 'my panel' })
      if (!s.ok) return s.reason
      sessionId = s.session_id
    }
    const r = await host.command('agent.run', { session_id: sessionId, input })
    return r.ok ? r.run_id : r.reason
  }
  host.onEvent('agent.delta', (ev) => render(ev.data.text)) // plus agent.done / agent.error
}
```

## Web-app shape: React adapter for `window.haiHost`

The page is your own React app (no `panel.js`). Put this in a small module and use it from
components; keep the guard so the same build still runs in a normal browser.

```ts
type Reply = { ok: boolean; reason?: string; data?: unknown }
type Host = {
  pluginId: string
  command: (name: string, payload?: unknown) => Promise<Reply>
  onEvent: (type: string, fn: (ev: { data?: any; session_id?: string; run_id?: string }) => void) => () => void
}

const host = (globalThis as { haiHost?: Host }).haiHost

export async function askAgent(input: string, onDelta: (text: string) => void) {
  if (!host) throw new Error('Not running inside HAI')
  const s = await host.command('agent.session', { title: '学习世界' })
  if (!s.ok) throw new Error(s.reason || 'agent.session failed')
  const off = host.onEvent('agent.delta', (ev) => onDelta(String(ev.data?.text ?? '')))
  const r = await host.command('agent.run', { session_id: s.data && (s.data as any).session_id, input })
  if (!r.ok) off()
  return { ...r, off }
}
```

with `surface.json`:

```json
{
  "id": "learning-world",
  "name": { "zh": "学习世界", "en": "Learning world" },
  "version": "1.0.0",
  "kind": "plugin:learning-world",
  "web": { "url": "http://127.0.0.1:5173" },
  "commands": ["agent.session", "agent.run", "agent.cancel", "composer.quote", "ui.toast"],
  "data": ["agent.delta", "agent.done", "agent.error", "surfaces_changed"],
  "entries": [
    { "id": "learn", "title": "学习世界", "path": "/" },
    { "id": "admin", "title": "课程后台", "path": "/admin" }
  ]
}
```

The `agent.run` reply is flat (`r.run_id`, `r.session_id`) as well as under `r.data`; read the flat
fields when using the real host. This example reads `s.data.session_id` defensively because the
loader normalizes both shapes.
