# Reply protocol (widget → session)

How a sandboxed `visual-ui` widget sends something **back** to the conversation. This is the
contract only — the host wiring is a separate concern, and a widget must behave correctly whether or
not the host listens.

## Why this is a separate protocol

The frame runs with `sandbox="allow-scripts"` and **no** `allow-same-origin`. Consequences that shape
every rule below:

| Fact | Consequence |
|---|---|
| The frame has an opaque origin (`"null"`) | `e.origin` is useless for identification. The host must validate by `e.source`. |
| There is no bridge object, no `window.parent.api` | The only channel is `parent.postMessage`, *outward*. There is no reply channel. |
| `localStorage` / `sessionStorage` / cookies throw | A widget cannot remember that it already sent something, or what the user picked last time. |
| The widget may be re-rendered at any time | It must be able to send the same reply again without side effects. |
| The host may not implement this protocol at all | Sending must degrade silently. A widget that breaks when unlistened is broken. |

## Message envelope

One `postMessage` per user action. Nothing is batched, nothing is streamed.

```jsonc
{
  "__haireply": 1,          // protocol version + marker. Required.
  "id": "r-3f2a",           // string. Unique per action in this widget instance.
  "kind": "choice",         // string. What the reader did. See kinds below.
  "text": "选了方案 B",      // string. Human-readable message to put in the composer. Required.
  "data": { "option": "b" } // object. Structured payload, optional, echoed for auditability.
}
```

### Field rules

| Field | Type | Required | Rules |
|---|---|---|---|
| `__haireply` | number | yes | Exactly `1`. Any other value is a different protocol; the host ignores it. |
| `id` | string | yes | `[A-Za-z0-9_-]{1,64}`. Used for de-duplication. Must be stable for **one** action — the same cause re-firing must reuse it, a new action must get a new one. |
| `kind` | string | yes | One of `choice`, `submit`, `rate`, `confirm`, `custom` (table below). Unknown kinds are dropped by the host, so do not invent one for a case `custom` covers. |
| `text` | string | yes | The message the reader will see **in their composer**, before sending. Plain text, ≤ 2000 chars. Never HTML, never markdown that the reader did not ask for. |
| `data` | object | no | Flat-ish JSON, ≤ 4 KB serialised. For the model to read the structure (`data.option === "b"`), not for the reader. |

### Kinds

| `kind` | Use when | `data` shape (suggested) |
|---|---|---|
| `choice` | Reader picked one option out of N | `{ "option": "<id>", "label": "<visible text>" }` |
| `submit` | Reader filled a small form and confirmed | `{ "<field>": value, … }` |
| `rate` | Reader scored something on a scale | `{ "value": 3, "scale": 5, "max": 5 }` |
| `confirm` | Reader said yes/no to a specific question | `{ "ok": true, "question": "<what was asked>" }` |
| `custom` | Anything the four above do not cover | free-form, but keep it small |

## Sending (widget side)

### Rules

1. **One action = one message.** No timers, no auto-send on load, no send-per-slider-frame.
   Debounce continuously-dragged controls (≥ 300 ms after the last change) and send the final value.
2. **Sending must be idempotent.** The same `id` sent twice is a no-op on the host side, but do not
   rely on that — disable the control after a successful send and show the sent state.
3. **Degrade silently.** Wrap in `try/catch`. If `parent` is unreachable (opened standalone), the
   widget must still work, only without the send button doing anything visible.
4. **Never send on scroll, hover, resize or mount.** Those are not user intent.
5. **The reader must be able to see what will be sent.** Show the composed `text` in the widget
   before or at the moment of sending. A hidden message sent on the reader's behalf is a trust bug.

### Canonical sender

```js
var reply = (function () {
  var seq = 0;
  return function (kind, text, data) {
    if (typeof text !== 'string' || !text.trim()) return false;
    if (text.length > 2000) text = text.slice(0, 1997) + '…';
    var msg = {
      __haireply: 1,
      id: 'r-' + (++seq) + '-' + Math.random().toString(36).slice(2, 8),
      kind: kind,
      text: text
    };
    if (data && typeof data === 'object') {
      try {
        if (JSON.stringify(data).length <= 4096) msg.data = data;
      } catch (e) { /* 循环引用等：丢弃 data，仍发 text */ }
    }
    try {
      if (window.parent && window.parent !== window) {
        window.parent.postMessage(msg, '*');
        return true;
      }
    } catch (e) { /* opaque origin 或独立打开：静默降级 */ }
    return false;
  };
})();
```

### Minimal widget-side example

```html
<div class="card">
  <h3>这个方案可行吗？</h3>
  <div class="btns">
    <button data-ans="yes">可行，继续</button>
    <button data-ans="no">有问题，说说</button>
  </div>
  <p class="note" id="sent" aria-live="polite"></p>
</div>
<style>
  .card { background: var(--hai-surface); color: var(--hai-text);
          border: 1px solid var(--hai-line); border-radius: 10px; padding: 16px 18px;
          font-family: system-ui, -apple-system, "PingFang SC", sans-serif; }
  .card h3 { margin: 0 0 12px; font-size: 15px; font-weight: 600; }
  .btns { display: flex; gap: 8px; flex-wrap: wrap; }
  button { padding: 6px 14px; font-size: 12.5px; cursor: pointer; color: var(--hai-text);
           background: var(--hai-bg); border: 1px solid var(--hai-line); border-radius: 6px; }
  button:hover { border-color: var(--hai-accent); }
  button:focus-visible { outline: 2px solid var(--hai-accent); outline-offset: 2px; }
  button[aria-pressed="true"] { border-color: var(--hai-accent); font-weight: 600; }
  button:disabled { opacity: .55; cursor: default; }
  .note { margin: 12px 0 0; font-size: 11.5px; color: var(--hai-text-2); }
</style>
<script>
(function () {
  var sent = document.getElementById('sent');
  var btns = Array.prototype.slice.call(document.querySelectorAll('button[data-ans]'));
  var busy = false;

  btns.forEach(function (b) {
    b.addEventListener('click', function () {
      if (busy) return;
      var yes = b.getAttribute('data-ans') === 'yes';
      // 读者看得到要发什么：先写进可见区域，再发
      var text = yes ? '可行，继续往下做。' : '这个方案有问题：';
      if (sent) sent.textContent = '将发送：' + text;

      busy = reply(yes ? 'confirm' : 'custom', text, { ok: yes });
      if (!busy) {                       // 宿主没接线 / 独立打开：降级提示，不假装成功
        if (sent) sent.textContent = '此预览未连接到会话（可复制上面这句到输入框）';
        return;
      }
      btns.forEach(function (x) { x.disabled = true; x.setAttribute('aria-pressed', 'false'); });
      b.setAttribute('aria-pressed', 'true');
      if (sent) sent.textContent = '已发送：' + text;
    });
  });

  // id 必须唯一：**不要用 Date.now()** —— 同一毫秒内连点两次会撞同一个 id，
  // 宿主的去重逻辑会把第二次当成重复而丢弃（实测：连点 5 次 → 1 个唯一 id）。
  var seq = 0;
  function reply(kind, text, data) {
    var msg = {
      __haireply: 1,
      id: 'r-' + (++seq) + '-' + Math.random().toString(36).slice(2, 8),
      kind: kind, text: text
    };
    if (data) msg.data = data;
    try {
      if (window.parent && window.parent !== window) {
        window.parent.postMessage(msg, '*');
        return true;
      }
    } catch (e) {}
    return false;
  }
})();
</script>
```

## Receiving (host side) — contract only

The host, if it implements this ever, must:

1. **Validate by `e.source`**, never by `e.origin` (always `"null"` here) and never by
   `e.data.spoofable` fields alone. Compare against the specific `iframe.contentWindow` that hosts
   this widget — not "any frame in the page".
2. **Version-gate on `__haireply === 1`.** Drop anything else silently; that is how this protocol
   stays extensible without breaking old widgets.
3. **Validate the shape before use**: `id` matches `^[A-Za-z0-9_-]{1,64}$`, `kind` is known,
   `text` is a string of 1–2000 chars, `data` serialises to ≤ 4 KB. Drop, do not coerce.
4. **De-duplicate on (`widget instance`, `id`)** so a re-mount or a repeated click cannot post the
   same reply twice.
5. **Never auto-send.** The message goes into the composer as a **draft**; the reader presses send.
   Anything else turns a widget into a way to talk to the model without the reader noticing.
6. **Treat `text` as untrusted.** It is attacker-controllable in principle (the widget is generated
   code). Put it in the composer as plain text; do not render it as HTML, do not execute it, do not
   pass it to any shell.

   "Plain text" is not enforceable by shape validation — `<img src=x onerror=alert(1)>` is a valid
   1–2000 char string. So the rule is about the **sink**, not the payload:

   | Sink | Verdict |
   |---|---|
   | `textarea.value = text` / React controlled `<textarea value>` | safe — this is the only sink to use |
   | `element.innerHTML = text` | forbidden |
   | `dangerouslySetInnerHTML={{ __html: text }}` | forbidden |
   | `eval` / `new Function` / `setTimeout(text)` | forbidden |
   | a shell, a URL parameter, a SQL string | forbidden |

   The composer path is a controlled `<textarea>`, which is why this protocol is safe by
   construction — keep it that way. If a future consumer wants to render the reply, it must escape
   first and that is a new decision, not a detail.

7. **Never let the reply skip the composer.** The reader sees the text, edits it if they want, and
   presses send. `text` must not be sent to the model directly by the host, or a generated widget
   becomes a prompt-injection vector that the reader never saw.

## What this protocol deliberately does not do

- **No reply channel back into the widget.** The frame cannot receive an answer. A widget that needs
  a follow-up round-trip must ask the reader to send, and the next reply is a new widget.
- **No state persistence.** Nothing survives a re-render. If the reader's choice must persist, it
  lives in the conversation (the message they sent), not in the widget.
- **No file/image payloads.** Text and a small JSON object only. Large data belongs in a file, not in
  a message authored by generated code.
- **No auto-send, ever.** See rule 5 above; this is the one rule that is not a technical limit but a
  product decision.

## Reference wiring (as implemented in this harness)

The contract above is deliberately host-agnostic. This section records the concrete wiring that
already exists, so a future change has something to diff against.

```
iframe  ──postMessage(__haireply)──▶  markdown.tsx  (HtmlPreviewBlock)
                                        │ ① e.source === this iframe's contentWindow
                                        │ ② __haireply === 1
                                        │ ③ id / kind / text / data shape
                                        ▼
                                  useAppStore.receiveWidgetReply(text, id)
                                        │ 去重：同 id 丢弃；trim；长度/id 形状再校验一遍
                                        ▼
                                  useAppStore.widgetReply   ← 一次性信号
                                        ▼
                                  Composer.tsx  useEffect
                                        │ 先 clearWidgetReply()（防重放）；追加到当前草稿
                                        │ 不自动发送 —— 读者自己按发送
                                        ▼
                                  textarea（受控 plain text）
```

| Piece | Where | Note |
|---|---|---|
| Validation constants | `markdown.tsx` (`REPLY_ID_RE`, `WIDGET_REPLY_KINDS`, `WIDGET_REPLY_MAX_*`) | must stay in sync with this document |
| Store channel | `useAppStore.ts` (`widgetReply`, `receiveWidgetReply`, `clearWidgetReply`) | store is globally writable → re-validates |
| Sink | `Composer.tsx` | controlled `<textarea>`; appends, then focuses |
| Regression test | `desktop/app/scripts/test-widget-reply.mjs` (`npm run test:widget-reply`) | store-side contract only; iframe/DOM needs a browser |

Two design notes worth keeping:

- **Why a store field and not a prop callback.** `Composer` and `Markdown` are siblings under
  `CenterPanel`; a callback would have to be threaded through four component signatures for what is
  really a global intent ("put this in the composer").
- **Why the store re-validates after `markdown.tsx` already did.** `markdown.tsx` knows the message
  came from *its* frame; the store does not. Anything holding the store can call it, so the store
  checks only what it can check alone (type, length, id shape) and refuses the rest.
