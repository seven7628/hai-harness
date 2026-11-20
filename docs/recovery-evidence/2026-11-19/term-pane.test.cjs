"use strict";
var __create = Object.create;
var __defProp = Object.defineProperty;
var __getOwnPropDesc = Object.getOwnPropertyDescriptor;
var __getOwnPropNames = Object.getOwnPropertyNames;
var __getProtoOf = Object.getPrototypeOf;
var __hasOwnProp = Object.prototype.hasOwnProperty;
var __copyProps = (to, from, except, desc) => {
  if (from && typeof from === "object" || typeof from === "function") {
    for (let key of __getOwnPropNames(from))
      if (!__hasOwnProp.call(to, key) && key !== except)
        __defProp(to, key, { get: () => from[key], enumerable: !(desc = __getOwnPropDesc(from, key)) || desc.enumerable });
  }
  return to;
};
var __toESM = (mod, isNodeMode, target) => (target = mod != null ? __create(__getProtoOf(mod)) : {}, __copyProps(
  // If the importer is in node compatibility mode or this is not an ESM
  // file that has been converted to a CommonJS file using a Babel-
  // compatible transform (i.e. "__esModule" has not been set), then set
  // "default" to the CommonJS "module.exports" for node compatibility.
  isNodeMode || !mod || !mod.__esModule ? __defProp(target, "default", { value: mod, enumerable: true }) : target,
  mod
));

// src/components/terminal/paneMachine-test.ts
var import_node_test = __toESM(require("node:test"));
var import_strict = __toESM(require("node:assert/strict"));

// src/components/terminal/paneMachine.ts
var TIMEOUT = {
  create: "\u521B\u5EFA\u7EC8\u7AEF\u8D85\u65F6",
  connect: "\u8FDE\u63A5\u7EC8\u7AEF\u8D85\u65F6"
};
function ptyIdOf(state) {
  if (state.s === "connecting" || state.s === "replaying" || state.s === "live" || state.s === "exited") {
    return state.ptyId;
  }
  return void 0;
}
var initialPaneState = { s: "idle" };
function next(state, ev) {
  if (ev.t === "close") {
    return {
      state: { s: "idle" },
      fx: ptyIdOf(state) ? [{ k: "close-socket" }, { k: "close-pty" }] : [{ k: "close-socket" }]
    };
  }
  if (ev.t === "error") {
    return { state: { s: "failed", error: ev.error }, fx: [{ k: "close-socket" }, { k: "toast", error: ev.error }] };
  }
  switch (ev.t) {
    case "open":
      if (state.s === "idle") return { state: { s: "creating" }, fx: [{ k: "create-pty" }] };
      break;
    case "created":
      if (state.s === "creating") return { state: { s: "connecting", ptyId: ev.ptyId }, fx: [{ k: "issue-ticket" }] };
      break;
    case "ticket":
      if (state.s === "connecting") {
        return { state: { s: "connecting", ptyId: state.ptyId, ticket: ev.ticket }, fx: [{ k: "open-socket" }] };
      }
      break;
    case "ws-open":
    case "hello":
      if (state.s === "connecting") return { state, fx: [] };
      break;
    case "reset":
      if (state.s === "connecting") return { state: { s: "replaying", ptyId: state.ptyId }, fx: [{ k: "reset-term" }] };
      break;
    case "output":
      if (state.s === "replaying") return { state: { s: "live", ptyId: state.ptyId }, fx: [{ k: "show" }] };
      break;
    case "exit":
      if (state.s === "live") {
        return { state: { s: "exited", ptyId: state.ptyId, code: ev.code }, fx: [{ k: "close-socket" }] };
      }
      break;
    case "timeout":
      if (ev.phase === "create" && state.s === "creating") {
        const error = TIMEOUT.create;
        return { state: { s: "failed", error }, fx: [{ k: "toast", error }] };
      }
      if (ev.phase === "connect" && state.s === "connecting") {
        const error = TIMEOUT.connect;
        return { state: { s: "failed", error }, fx: [{ k: "close-socket" }, { k: "toast", error }] };
      }
      break;
  }
  return { state, fx: [] };
}

// src/components/terminal/paneMachine-test.ts
var idle = { s: "idle" };
var creating = { s: "creating" };
var connecting = { s: "connecting", ptyId: "p1" };
var replaying = { s: "replaying", ptyId: "p1" };
var live = { s: "live", ptyId: "p1" };
var exited = { s: "exited", ptyId: "p1", code: 0 };
var failed = { s: "failed", error: "boom" };
(0, import_node_test.default)("initialPaneState \u662F idle\uFF1A\u6302\u8F7D\u5373\u53EF\u6E32\u67D3\uFF0C\u4E0D\u4F1A\u51ED\u7A7A\u5EFA PTY", () => {
  import_strict.default.deepEqual(initialPaneState, { s: "idle" });
  import_strict.default.deepEqual(next(initialPaneState, { t: "open" }).fx, [{ k: "create-pty" }]);
});
(0, import_node_test.default)("idle + open \u2192 creating\uFF0C\u5E76\u8BF7\u6C42\u5EFA PTY", () => {
  const r = next(idle, { t: "open" });
  import_strict.default.deepEqual(r.state, { s: "creating" });
  import_strict.default.deepEqual(r.fx, [{ k: "create-pty" }]);
});
(0, import_node_test.default)("creating + created \u2192 connecting\uFF08\u5E26 ptyId\uFF09\uFF0C\u5E76\u8BF7\u6C42\u7968\u636E", () => {
  const r = next(creating, { t: "created", ptyId: "p1" });
  import_strict.default.deepEqual(r.state, { s: "connecting", ptyId: "p1" });
  import_strict.default.deepEqual(r.fx, [{ k: "issue-ticket" }]);
});
(0, import_node_test.default)("creating + timeout(create) \u2192 failed\uFF0C\u53EA toast\uFF08\u8FD8\u6CA1\u6709 socket \u53EF\u5173\uFF09", () => {
  const r = next(creating, { t: "timeout", phase: "create" });
  import_strict.default.equal(r.state.s, "failed");
  import_strict.default.deepEqual(r.fx, [{ k: "toast", error: "\u521B\u5EFA\u7EC8\u7AEF\u8D85\u65F6" }]);
});
(0, import_node_test.default)("connecting + ticket \u2192 connecting \u5E26\u4E0A\u7968\u636E\uFF0C\u5E76\u5F00 socket", () => {
  const r = next(connecting, { t: "ticket", ticket: "tk" });
  import_strict.default.deepEqual(r.state, { s: "connecting", ptyId: "p1", ticket: "tk" });
  import_strict.default.deepEqual(r.fx, [{ k: "open-socket" }]);
});
(0, import_node_test.default)("connecting + ws-open / hello \u2192 \u72B6\u6001\u4E0E fx \u90FD\u4E0D\u52A8", () => {
  for (const ev of [{ t: "ws-open" }, { t: "hello" }]) {
    const r = next(connecting, ev);
    import_strict.default.equal(r.state, connecting, `\u63E1\u624B\u4E8B\u4EF6\u4E0D\u8BE5\u6362\u72B6\u6001\uFF1A${ev.t}`);
    import_strict.default.deepEqual(r.fx, []);
  }
});
(0, import_node_test.default)("connecting + reset \u2192 replaying\uFF0C\u5E76\u91CD\u7F6E\u7EC8\u7AEF", () => {
  const r = next({ s: "connecting", ptyId: "p1", ticket: "tk" }, { t: "reset" });
  import_strict.default.deepEqual(r.state, { s: "replaying", ptyId: "p1" });
  import_strict.default.deepEqual(r.fx, [{ k: "reset-term" }]);
});
(0, import_node_test.default)("replaying + output \u2192 live\uFF0C\u4E14 show \u53EA\u51FA\u73B0\u4E00\u6B21", () => {
  const r = next(replaying, { t: "output" });
  import_strict.default.deepEqual(r.state, { s: "live", ptyId: "p1" });
  import_strict.default.deepEqual(r.fx, [{ k: "show" }]);
  const again = next(r.state, { t: "output" });
  import_strict.default.equal(again.state, r.state);
  import_strict.default.deepEqual(again.fx, []);
});
(0, import_node_test.default)("connecting + timeout(connect) \u2192 failed\uFF0C\u5173 socket \u5E76 toast", () => {
  const r = next(connecting, { t: "timeout", phase: "connect" });
  import_strict.default.equal(r.state.s, "failed");
  import_strict.default.deepEqual(r.fx, [{ k: "close-socket" }, { k: "toast", error: "\u8FDE\u63A5\u7EC8\u7AEF\u8D85\u65F6" }]);
});
(0, import_node_test.default)("\u4EFB\u610F\u72B6\u6001 + error \u2192 failed\uFF0C\u5173 socket \u5E76\u628A\u539F\u6587\u9012\u7ED9 toast", () => {
  for (const s of [idle, creating, connecting, replaying, live, exited, failed]) {
    const r = next(s, { t: "error", error: "ws \u65AD\u4E86" });
    import_strict.default.deepEqual(r.state, { s: "failed", error: "ws \u65AD\u4E86" }, `\u4ECE ${s.s} \u8FDB\u6765\u4E5F\u8981\u843D\u5230 failed`);
    import_strict.default.deepEqual(r.fx, [{ k: "close-socket" }, { k: "toast", error: "ws \u65AD\u4E86" }]);
  }
});
(0, import_node_test.default)("live + exit \u2192 exited\uFF08\u5E26\u9000\u51FA\u7801\uFF09\uFF0C\u5E76\u5173 socket", () => {
  import_strict.default.deepEqual(next(live, { t: "exit", code: 7 }), {
    state: { s: "exited", ptyId: "p1", code: 7 },
    fx: [{ k: "close-socket" }]
  });
  import_strict.default.deepEqual(next(live, { t: "exit" }), {
    state: { s: "exited", ptyId: "p1", code: void 0 },
    fx: [{ k: "close-socket" }]
  });
});
(0, import_node_test.default)("close\uFF1A\u6709 ptyId \u65F6\u8FDE PTY \u4E00\u8D77\u6536", () => {
  for (const s of [connecting, replaying, live, exited]) {
    import_strict.default.deepEqual(
      next(s, { t: "close" }),
      { state: { s: "idle" }, fx: [{ k: "close-socket" }, { k: "close-pty" }] },
      `\u4ECE ${s.s} \u5173\u6807\u7B7E\u8981\u8FDE\u8FDB\u7A0B\u4E00\u8D77\u6536`
    );
  }
});
(0, import_node_test.default)("close\uFF1A\u6CA1\u6709 ptyId \u65F6\u53EA\u5173 socket", () => {
  for (const s of [idle, creating, failed]) {
    import_strict.default.deepEqual(next(s, { t: "close" }), { state: { s: "idle" }, fx: [{ k: "close-socket" }] });
  }
});
(0, import_node_test.default)("\u672A\u5217\u51FA\u7684\u7EC4\u5408\u539F\u6837\u8FD4\u56DE\uFF0Cfx \u4E3A\u7A7A", () => {
  const cases = [
    [idle, { t: "output" }],
    [idle, { t: "ticket", ticket: "tk" }],
    [idle, { t: "exit", code: 0 }],
    [creating, { t: "open" }],
    [creating, { t: "reset" }],
    [connecting, { t: "output" }],
    [connecting, { t: "exit", code: 0 }],
    [live, { t: "reset" }],
    [live, { t: "open" }],
    [live, { t: "created", ptyId: "p2" }],
    [replaying, { t: "hello" }],
    [exited, { t: "output" }],
    [failed, { t: "ws-open" }]
  ];
  for (const [s, ev] of cases) {
    const r = next(s, ev);
    import_strict.default.equal(r.state, s, `${s.s} + ${ev.t} \u5E94\u5F53\u539F\u6837\u8FD4\u56DE\u540C\u4E00\u4E2A\u5BF9\u8C61`);
    import_strict.default.deepEqual(r.fx, []);
  }
});
(0, import_node_test.default)("\u8D85\u65F6\u843D\u5728\u9519\u8BEF\u7684\u9636\u6BB5\u65F6\u4E0D\u7B97\u6570\uFF08\u8FDF\u5230\u7684 timeout \u4E0D\u8BE5\u6BC1\u6389\u4E00\u4E2A\u5DF2\u8FDE\u4E0A\u7684\u7EC8\u7AEF\uFF09", () => {
  import_strict.default.deepEqual(next(live, { t: "timeout", phase: "connect" }), { state: live, fx: [] });
  import_strict.default.deepEqual(next(connecting, { t: "timeout", phase: "create" }), { state: connecting, fx: [] });
});
(0, import_node_test.default)("\u7EAF\u51FD\u6570\uFF1A\u4EFB\u4F55\u8F6C\u79FB\u90FD\u4E0D\u6539\u5165\u53C2", () => {
  const snapshot = structuredClone(connecting);
  next(connecting, { t: "ticket", ticket: "tk" });
  next(connecting, { t: "error", error: "x" });
  next(connecting, { t: "close" });
  import_strict.default.deepEqual(connecting, snapshot);
});
