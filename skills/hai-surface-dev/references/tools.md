# Exposing tools to the agent (P2)

Part of `hai-surface-dev`. Read this only when the panel needs the model to call plugin tools.

A plugin can give its own agent session tools (for example a PDF panel exposing `update_note`).
Declare them in `surface.json`, **and** declare the two channels — the installer rejects tools
without them:

```json
{
  "commands": ["agent.session", "agent.run", "tool.result"],
  "data": ["agent.delta", "agent.done", "agent.error", "tool.call"],
  "tools": [
    {
      "name": "update_note",
      "description": "Replace the note shown in this panel",
      "input": {"type": "object", "properties": {"text": {"type": "string"}}, "required": ["text"]},
      "read_only": false,
      "requires_panel": true,
      "timeout_ms": 10000
    }
  ]
}
```

Reply from the plugin (no per-tool registration API):

```js
host.onEvent('tool.call', async (ev) => {
  const { call_id, tool, args } = ev.data
  try {
    const result = await runMyTool(tool, args) // your code
    await host.command('tool.result', { call_id, ok: true, result })
  } catch (e) {
    await host.command('tool.result', { call_id, ok: false, error: String(e && e.message || e) })
  }
})
```

Subscribe in `apply` scope (as above) when the tool must work while the panel is closed; the host
**tracks the subscription and disposes it on plugin unload/reload**. If you deliberately tie it to a
component, call the returned `off()` on unmount. `apply`'s return value is ignored — it is not a
teardown hook.

- `read_only: true` may run without a card. `read_only: false` **always** asks the user, even in
  Auto / Full Access — `MandatoryApproval` bypasses the mode approver. A plugin cannot approve it.
- `requires_panel: true` means the host refuses with `panel_not_open` unless a tab of your kind is
  open **in the current scope**; the callback is not called. Any entry counts.
- Cancel a run with `host.command('agent.cancel', { run_id })` (keep the id from the `agent.run`
  ack). The terminal event is `agent.error{reason:"cancelled"}` — **not** `agent.done`; there is no
  partial done to reconcile. The approval card shows your plugin name + tool name, not the raw
  `plugin__<id>__<name>`.
- The model sees `plugin__<id>__<name>`; keep the full name ≤ 64 bytes (OpenAI function-name hard
  limit). `name` matches `^[a-z][a-z0-9_]{0,40}$`, no `__`, ≤ 16 tools/plugin. The installer hard
  cap for `description` is **300 chars**; design new tools to **≤ 120 chars** (see
  [`panel-api.md`](panel-api.md#tool-descriptions-stay-short)). `input` must be a JSON `object`
  (≤ 16 KiB, no `$ref`), `timeout_ms` 0 (=10s) or 100ms–60s.
- Results are capped at 64 KiB JSON (truncated with `truncated:true`). Timeout / unload fail the
  call (`timeout` / `plugin_unloaded`); the run continues. Unknown or already-settled `call_id` is
  ignored, so a late `tool.result` is harmless.
- Tools exist only in **your plugin's own sessions** (D10) — never in a real Project session, never
  in another plugin. Do not try to register tools globally.
