var TIMEOUT = {
  create: "\u521B\u5EFA\u7EC8\u7AEF\u8D85\u65F6",
  connect: "\u8FDE\u63A5\u7EC8\u7AEF\u8D85\u65F6"
};
function ptyIdOf(state4) {
  if (state4.s === "connecting" || state4.s === "replaying" || state4.s === "live" || state4.s === "exited") {
    return state4.ptyId;
  }
  return void 0;
}
function next4(state4, ev) {
  if (ev.t === "close") {
    return {
      state: { s: "idle" },
      fx: ptyIdOf(state4) ? [{ k: "close-socket" }, { k: "close-pty" }] : [{ k: "close-socket" }]
    };
  }
  if (ev.t === "error") {
    return { state: { s: "failed", error: ev.error }, fx: [{ k: "close-socket" }, { k: "toast", error: ev.error }] };
  }
  switch (ev.t) {
    case "open":
      if (state4.s === "idle") return { state: { s: "creating" }, fx: [{ k: "create-pty" }] };
      break;
    case "created":
      if (state4.s === "creating") return { state: { s: "connecting", ptyId: ev.ptyId }, fx: [{ k: "issue-ticket" }] };
      break;
    case "ticket":
      if (state4.s === "connecting") {
        return { state: { s: "connecting", ptyId: state4.ptyId, ticket: ev.ticket }, fx: [{ k: "open-socket" }] };
      }
      break;
    case "ws-open":
    case "hello":
      if (state4.s === "connecting") return { state: state4, fx: [] };
      break;
    case "reset":
      if (state4.s === "connecting") return { state: { s: "replaying", ptyId: state4.ptyId }, fx: [{ k: "reset-term" }] };
      break;
    case "output":
      if (state4.s === "replaying") return { state: { s: "live", ptyId: state4.ptyId }, fx: [{ k: "show" }] };
      break;
    case "exit":
      if (state4.s === "live") {
        return { state: { s: "exited", ptyId: state4.ptyId, code: ev.code }, fx: [{ k: "close-socket" }] };
      }
      break;
    case "timeout":
      if (ev.phase === "create" && state4.s === "creating") {
        const error3 = TIMEOUT.create;
        return { state: { s: "failed", error: error3 }, fx: [{ k: "toast", error: error3 }] };
      }
      if (ev.phase === "connect" && state4.s === "connecting") {
        const error3 = TIMEOUT.connect;
        return { state: { s: "failed", error: error3 }, fx: [{ k: "close-socket" }, { k: "toast", error: error3 }] };
      }
      break;
  }
  return { state: state4, fx: [] };
}
