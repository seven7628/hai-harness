var import_jsx_runtime32 = __toESM(require_jsx_runtime());
var termSurface = {
  kind: "term",
  namespace: "builtin",
  group: "workflow",
  // 排在 git(20) 之前：终端是「随手要开」的，越靠前越省一次点击
  order: 15,
  singleton: true,
  // 满幅：终端要正片区域（不要 right-body 的 10px 内边距与滚动条），
  // 否则 xterm 的列宽/行数会围着内边距算，右侧总留一条空带（§6.4）。
  fullBleed: true,
  scope: "session",
  weight: "heavy",
  capabilities: ["write"],
  title: (_tab, t4) => t4("rp.tab.term"),
  icon: () => /* @__PURE__ */ (0, import_jsx_runtime32.jsx)(TermIcon, {}),
  defaultSpec: () => ({ kind: "term" }),
  render: (ctx) => /* @__PURE__ */ (0, import_jsx_runtime32.jsx)(TerminalTab, { tabId: ctx.tab.id })
};
registerSurface(termSurface);
