var import_jsx_runtime29 = __toESM(require_jsx_runtime());
var RESIZE_DEBOUNCE_MS = 100;
function TerminalPane({ tabId, ptyId }) {
  const t4 = useT();
  const tRef = (0, import_react27.useRef)(t4);
  tRef.current = t4;
  const hostRef = (0, import_react27.useRef)(null);
  const termRef = (0, import_react27.useRef)(null);
  const sockRef = (0, import_react27.useRef)(null);
  const sendRef = (0, import_react27.useRef)(() => {
  });
  const stateRef = (0, import_react27.useRef)({ s: "connecting", ptyId });
  const [state4, setState] = (0, import_react27.useState)(stateRef.current);
  const [selection2, setSelection] = (0, import_react27.useState)("");
  const [inserted, setInserted] = (0, import_react27.useState)(false);
  const [menu2, setMenu] = (0, import_react27.useState)(null);
  const apply5 = (0, import_react27.useCallback)((ev) => {
    const { state: ns, fx } = next4(stateRef.current, ev);
    stateRef.current = ns;
    for (const f2 of fx) {
      if (f2.k === "reset-term") termRef.current?.reset();
      if (f2.k === "close-socket") {
        sockRef.current?.close();
        sockRef.current = null;
      }
    }
    setState(ns);
  }, []);
  const quote = (0, import_react27.useCallback)((raw2) => {
    const text13 = formatSelection(raw2).slice(0, 8 << 10);
    if (!text13) return false;
    emitUI("composer_quote", { text: text13, source: { id: `term:${tabId}`, title: tRef.current("term.quoteSource") } });
    setInserted(true);
    window.setTimeout(() => setInserted(false), 800);
    return true;
  }, [tabId]);
  const quoteSelection = (0, import_react27.useCallback)(() => quote(termRef.current?.getSelection() ?? ""), [quote]);
  const quoteScreen = (0, import_react27.useCallback)(() => {
    const term = termRef.current;
    if (!term) return false;
    const buf = term.buffer.active;
    const lines = [];
    for (let i2 = 0; i2 < term.rows; i2++) lines.push(buf.getLine(buf.viewportY + i2)?.translateToString(true) ?? "");
    while (lines.length && lines[lines.length - 1].trim() === "") lines.pop();
    return quote(lines.join("\n"));
  }, [quote]);
  const quoteSmart = (0, import_react27.useCallback)(() => {
    if (!quoteSelection()) quoteScreen();
  }, [quoteSelection, quoteScreen]);
  const openMenu = (0, import_react27.useCallback)((e) => {
    e.preventDefault();
    setMenu({ x: e.clientX, y: e.clientY });
  }, []);
  const paste = (0, import_react27.useCallback)(async () => {
    try {
      const text13 = await navigator.clipboard.readText();
      if (text13) termRef.current?.paste(text13);
    } catch {
    }
  }, []);
  (0, import_react27.useEffect)(() => {
    if (!menu2) return;
    const close4 = () => setMenu(null);
    const onKey = (e) => {
      if (e.key === "Escape") setMenu(null);
    };
    window.addEventListener("mousedown", close4);
    window.addEventListener("wheel", close4, { passive: true });
    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("mousedown", close4);
      window.removeEventListener("wheel", close4);
      window.removeEventListener("keydown", onKey);
    };
  }, [menu2]);
  (0, import_react27.useEffect)(() => {
    const host = hostRef.current;
    if (!host) return;
    let disposed = false;
    const term = new import_xterm.Terminal({
      // Nerd Font 优先（p10k 的 powerline/图标都在私用区，见 §6.6）；没装就退到等宽字体，
      // 提示符会出现方框 —— 那是字体问题，不是终端坏了（i18n 有 term.fontMissing 提示位）。
      fontFamily: "'MesloLGL Nerd Font Mono','Meslo LG L','JetBrains Mono','IBM Plex Mono',monospace",
      fontSize: 12,
      // powerline 分隔符要上下无缝：1.0 是唯一正确的行高（1.2 会露出接缝）
      lineHeight: 1,
      letterSpacing: 0,
      scrollback: 5e3,
      cursorBlink: true,
      allowProposedApi: true,
      theme: readTermTheme()
    });
    const fit3 = new import_addon_fit.FitAddon();
    term.loadAddon(fit3);
    term.open(host);
    termRef.current = term;
    const writer = terminalWriter((data6, done) => term.write(data6, done));
    const doFit = () => {
      if (!host.clientWidth || !host.clientHeight) return;
      try {
        fit3.fit();
      } catch {
      }
    };
    doFit();
    void document.fonts?.ready?.then(() => {
      if (!disposed) doFit();
    });
    let timer3 = null;
    const scheduleFit = () => {
      if (timer3) clearTimeout(timer3);
      timer3 = setTimeout(() => {
        timer3 = null;
        if (disposed) return;
        doFit();
        const sock = sockRef.current;
        if (sock && sock.readyState === WebSocket.OPEN) {
          sock.send(encodeCtrl({ t: "resize", cols: term.cols, rows: term.rows }));
        }
      }, RESIZE_DEBOUNCE_MS);
    };
    const ro = new ResizeObserver(scheduleFit);
    ro.observe(host);
    const offTheme = watchTheme(() => {
      term.options.theme = readTermTheme();
    });
    term.onData((d3) => sendRef.current(d3));
    term.onSelectionChange(() => setSelection(term.getSelection()));
    term.attachCustomKeyEventHandler((e) => {
      if (e.type !== "keydown") return true;
      const mod = e.metaKey || e.ctrlKey;
      if (mod && e.shiftKey && e.key === "Enter") {
        quoteSmart();
        return false;
      }
      if (mod && !e.shiftKey && e.key.toLowerCase() === "c") {
        if (term.getSelection()) return false;
        sendRef.current("");
        return false;
      }
      if (mod && !e.shiftKey && e.key.toLowerCase() === "v") return false;
      return true;
    });
    const onKeyDown = (e) => {
      e.stopPropagation();
    };
    host.addEventListener("keydown", onKeyDown);
    void (async () => {
      const r4 = await getTransport().ptyTicket(ptyId);
      if (disposed) return;
      if (!r4.ok || !r4.url || !r4.ticket) {
        apply5({ t: "error", error: r4.error ?? tRef.current("term.failed").replace("{error}", "ticket") });
        return;
      }
      apply5({ t: "ticket", ticket: r4.ticket });
      const sock = new WebSocket(`${r4.url}?ticket=${encodeURIComponent(r4.ticket)}`);
      sock.binaryType = "arraybuffer";
      sockRef.current = sock;
      sendRef.current = (d3) => {
        if (sock.readyState === WebSocket.OPEN) sock.send(d3);
      };
      sock.onopen = () => apply5({ t: "ws-open" });
      sock.onmessage = (ev) => {
        const { ctrl, data: data6 } = decodeFrame(ev.data);
        if (ctrl) {
          switch (ctrl.t) {
            case "hello":
              apply5({ t: "hello" });
              break;
            case "reset":
              apply5({ t: "reset" });
              break;
            case "gap":
              term.writeln(`\x1B[2m${tRef.current("term.truncated")}\x1B[0m`);
              break;
            case "exit":
              apply5({ t: "exit", code: ctrl.code });
              break;
            default:
              break;
          }
          return;
        }
        if (data6 && data6.length) {
          writer.push(data6);
          apply5({ t: "output" });
        }
      };
      sock.onerror = () => {
        if (!disposed) apply5({ t: "error", error: tRef.current("term.failed").replace("{error}", "socket") });
      };
      sock.onclose = () => {
        if (disposed) return;
        if (stateRef.current.s === "exited") return;
        apply5({ t: "error", error: tRef.current("term.failed").replace("{error}", "closed") });
      };
    })();
    return () => {
      disposed = true;
      if (timer3) clearTimeout(timer3);
      ro.disconnect();
      offTheme();
      host.removeEventListener("keydown", onKeyDown);
      writer.clear();
      sockRef.current?.close();
      sockRef.current = null;
      term.dispose();
      termRef.current = null;
    };
  }, [ptyId, apply5, quoteSmart]);
  const reopen = (0, import_react27.useCallback)(() => {
    useAppStore.getState().termClose(tabId);
    useAppStore.getState().termEnsure(tabId);
  }, [tabId]);
  const overlay = state4.s === "failed" ? { kind: "failed", text: state4.error } : state4.s === "exited" ? { kind: "exited", text: t4("term.exit").replace("{code}", String(state4.code ?? "?")) } : state4.s === "live" ? null : { kind: "loading", text: state4.s === "replaying" ? t4("term.replaying") : t4("term.starting") };
  return /* @__PURE__ */ (0, import_jsx_runtime29.jsxs)("div", { className: "term-pane", "data-tab": tabId, "data-state": state4.s, onContextMenu: openMenu, children: [
    /* @__PURE__ */ (0, import_jsx_runtime29.jsx)("div", { className: "term-host", ref: hostRef }),
    selection2.trim() !== "" && state4.s === "live" && /* @__PURE__ */ (0, import_jsx_runtime29.jsxs)("button", { type: "button", className: "term-insert", onClick: quoteSelection, children: [
      inserted ? t4("term.inserted") : t4("term.insert"),
      /* @__PURE__ */ (0, import_jsx_runtime29.jsx)("kbd", { children: "\u2318\u21E7\u21B5" })
    ] }),
    menu2 && // 右键菜单：快捷键之外的「操作」入口（触控板/不记快捷键的人靠它）
    /* @__PURE__ */ (0, import_jsx_runtime29.jsxs)("div", { className: "term-menu", role: "menu", style: { left: menu2.x, top: menu2.y }, onMouseDown: (e) => e.stopPropagation(), children: [
      /* @__PURE__ */ (0, import_jsx_runtime29.jsxs)("button", { role: "menuitem", disabled: !selection2.trim(), onClick: () => {
        setMenu(null);
        quoteSelection();
      }, children: [
        t4("term.menu.selection"),
        /* @__PURE__ */ (0, import_jsx_runtime29.jsx)("kbd", { children: "\u2318\u21E7\u21B5" })
      ] }),
      /* @__PURE__ */ (0, import_jsx_runtime29.jsx)("button", { role: "menuitem", onClick: () => {
        setMenu(null);
        quoteScreen();
      }, children: t4("term.menu.screen") }),
      /* @__PURE__ */ (0, import_jsx_runtime29.jsx)("div", { className: "term-menu-sep", role: "separator" }),
      /* @__PURE__ */ (0, import_jsx_runtime29.jsx)("button", { role: "menuitem", disabled: !selection2.trim(), onClick: () => {
        setMenu(null);
        void copyText(selection2);
      }, children: t4("term.menu.copy") }),
      /* @__PURE__ */ (0, import_jsx_runtime29.jsx)("button", { role: "menuitem", onClick: () => {
        setMenu(null);
        void paste();
      }, children: t4("term.menu.paste") }),
      /* @__PURE__ */ (0, import_jsx_runtime29.jsx)("button", { role: "menuitem", onClick: () => {
        setMenu(null);
        termRef.current?.clear();
      }, children: t4("term.menu.clear") }),
      /* @__PURE__ */ (0, import_jsx_runtime29.jsx)("button", { role: "menuitem", onClick: () => {
        setMenu(null);
        reopen();
      }, children: t4("term.menu.reopen") })
    ] }),
    overlay && /* @__PURE__ */ (0, import_jsx_runtime29.jsxs)("div", { className: `term-overlay ${overlay.kind}`, role: "status", children: [
      /* @__PURE__ */ (0, import_jsx_runtime29.jsx)("span", { className: "term-overlay-text", children: overlay.text }),
      (overlay.kind === "exited" || overlay.kind === "failed") && /* @__PURE__ */ (0, import_jsx_runtime29.jsx)("button", { type: "button", className: "btn btn-xs", onClick: reopen, children: t4("term.reopen") })
    ] })
  ] });
}
