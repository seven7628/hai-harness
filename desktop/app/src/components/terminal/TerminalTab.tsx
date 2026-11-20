var import_jsx_runtime30 = __toESM(require_jsx_runtime());
function TerminalTab({ tabId }) {
  const t4 = useT();
  const rt = useAppStore((s3) => s3.term[tabId]);
  const termEnsure = useAppStore((s3) => s3.termEnsure);
  (0, import_react28.useEffect)(() => {
    termEnsure(tabId);
  }, [tabId, termEnsure]);
  if (rt?.ptyId) return /* @__PURE__ */ (0, import_jsx_runtime30.jsx)(TerminalPane, { tabId, ptyId: rt.ptyId });
  if (rt?.status === "failed") {
    return /* @__PURE__ */ (0, import_jsx_runtime30.jsx)("div", { className: "term-pane", "data-state": "failed", children: /* @__PURE__ */ (0, import_jsx_runtime30.jsxs)("div", { className: "term-overlay failed", role: "status", children: [
      /* @__PURE__ */ (0, import_jsx_runtime30.jsx)("span", { className: "term-overlay-text", children: t4("term.failed").replace("{error}", rt.error ?? "") }),
      /* @__PURE__ */ (0, import_jsx_runtime30.jsx)("button", { type: "button", className: "btn btn-xs", onClick: () => termEnsure(tabId), children: t4("term.reopen") })
    ] }) });
  }
  return /* @__PURE__ */ (0, import_jsx_runtime30.jsx)("div", { className: "term-pane", "data-state": "loading", children: /* @__PURE__ */ (0, import_jsx_runtime30.jsx)("div", { className: "term-overlay loading", role: "status", children: /* @__PURE__ */ (0, import_jsx_runtime30.jsx)("span", { className: "term-overlay-text", children: t4("term.starting") }) }) });
}
