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
