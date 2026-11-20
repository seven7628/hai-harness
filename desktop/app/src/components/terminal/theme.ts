// [recovered] from /private/tmp/builtin-surfaces.test.cjs
// 编译产物（esbuild `// src/...` 模块切片），依赖由 bundle 注入，未回译。
// @ts-nocheck
/* eslint-disable */
function readVar(name2) {
  if (typeof window === "undefined") return "";
  const v3 = getComputedStyle(document.documentElement).getPropertyValue(name2);
  return v3.trim();
}
function pick4(...names) {
  for (const n of names) {
    const v3 = readVar(n);
    if (v3) return v3;
  }
  return void 0;
}
function readTermTheme() {
  const bg = pick4("--code-block-bg", "--paper-3", "--paper");
  const fg = pick4("--ink");
  const bright = pick4("--ink");
  const dim = pick4("--ink-3");
  return {
    background: bg,
    foreground: fg,
    cursor: pick4("--cinnabar", "--cinnabar-2"),
    cursorAccent: bg,
    selectionBackground: pick4("--accent-weak") || "rgba(217, 122, 102, 0.28)",
    black: pick4("--paper-3", "--ink-line-2"),
    red: pick4("--crimson"),
    green: pick4("--bamboo"),
    yellow: pick4("--ochre"),
    blue: pick4("--indigo"),
    magenta: pick4("--cat-write"),
    cyan: pick4("--cat-cmd"),
    white: pick4("--ink-2"),
    brightBlack: dim,
    brightRed: pick4("--crimson"),
    brightGreen: pick4("--bamboo"),
    brightYellow: pick4("--ochre"),
    brightBlue: pick4("--indigo"),
    brightMagenta: pick4("--cat-write"),
    brightCyan: pick4("--cat-cmd"),
    brightWhite: bright
  };
}
function watchTheme(onChange) {
  if (typeof MutationObserver === "undefined") return () => {
  };
  const mo = new MutationObserver(onChange);
  mo.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
  return () => mo.disconnect();
}

