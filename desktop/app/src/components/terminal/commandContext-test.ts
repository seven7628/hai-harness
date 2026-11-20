// [recovered] from /private/tmp/term-context.test.cjs
// 编译产物（esbuild `// src/...` 模块切片），依赖由 bundle 注入，未回译。
// @ts-nocheck
/* eslint-disable */
function feed(chunks, start = emptyCommandLine) {
  let state = start;
  const submitted = [];
  for (const c of chunks) {
    const step = trackCommandLine(state, c);
    state = step.state;
    if (step.submitted !== void 0) submitted.push(step.submitted);
  }
  return { state, submitted };
}
(0, import_node_test.default)("\u9010\u5B57\u7B26\u8F93\u5165 + \u56DE\u8F66 \u2192 \u63D0\u4EA4\u53BB\u6389\u9996\u5C3E\u7A7A\u767D\u7684\u547D\u4EE4", () => {
  const { submitted, state } = feed(["n", "p", "m", " ", "t", "e", "s", "t", "\r"]);
  import_strict.default.deepEqual(submitted, ["npm test"]);
  import_strict.default.deepEqual(state, emptyCommandLine);
});
(0, import_node_test.default)("\u9000\u56DE\uFF08DEL 0x7f\uFF09\u53EA\u5220\u4E00\u4E2A\u5B57\u7B26", () => {
  const { submitted } = feed(["l", "s", "x", "\x7F", "\r"]);
  import_strict.default.deepEqual(submitted, ["ls"]);
});
(0, import_node_test.default)("\u7A7A\u56DE\u8F66\u4E0D\u63D0\u4EA4\uFF08\u63D0\u793A\u7B26\u4E0A\u76F4\u63A5\u56DE\u8F66\u4E0D\u662F\u547D\u4EE4\uFF09", () => {
  const { submitted } = feed(["\r", "  ", "\r"]);
  import_strict.default.deepEqual(submitted, []);
});
(0, import_node_test.default)("\u2303U \u6E05\u884C\u540E\u91CD\u65B0\u8F93\u5165 \u2192 \u63D0\u4EA4\u7684\u662F\u65B0\u547D\u4EE4", () => {
  const { submitted } = feed(["g", "i", "t", " ", "x", "", "l", "s", "\r"]);
  import_strict.default.deepEqual(submitted, ["ls"]);
});
(0, import_node_test.default)("Tab \u8865\u5168 \u21D2 \u8FD9\u4E00\u884C dirty\uFF0C\u56DE\u8F66\u4E0D\u63D0\u4EA4\u547D\u4EE4", () => {
  const { submitted, state } = feed(["g", "i", "t", " ", "s", "	", "\r"]);
  import_strict.default.deepEqual(submitted, []);
  import_strict.default.equal(state.dirty, false, "\u56DE\u8F66\u540E\u56DE\u5230\u5E72\u51C0\u72B6\u6001");
});
(0, import_node_test.default)("\u65B9\u5411\u952E\uFF08ESC \u5E8F\u5217\uFF09\u540C\u6837 dirty", () => {
  const { submitted } = feed(["\x1B", "[", "A", "\r"]);
  import_strict.default.deepEqual(submitted, []);
});
(0, import_node_test.default)("dirty \u4E4B\u540E\u6309\u9000\u683C\u4E0D\u4F1A\u300C\u6D17\u767D\u300D\uFF1A\u4ECD\u7136\u4E0D\u63D0\u4EA4", () => {
  const { submitted } = feed(["n", "p", "m", "	", "\x7F", "\r"]);
  import_strict.default.deepEqual(submitted, []);
});
(0, import_node_test.default)("\u2303C \u4E2D\u65AD\u540E\u518D\u8F93\u5165\u7684\u547D\u4EE4\u662F\u5E72\u51C0\u7684", () => {
  const { submitted, state } = feed(["s", "l", "e", "e", "p", " ", "9", "", "l", "s", "\r"]);
  import_strict.default.deepEqual(submitted, ["ls"]);
  import_strict.default.equal(state.line, "");
});
(0, import_node_test.default)("\u591A\u884C\u7C98\u8D34\uFF08\u542B \\r\uFF09\u6309\u591A\u6B21\u63D0\u4EA4\u5904\u7406", () => {
  const { submitted } = feed(["ls\rpwd\r"]);
  import_strict.default.deepEqual(submitted, ["pwd"]);
});
(0, import_node_test.default)("\u6B63\u6587\u91CC\u6CA1\u6709\u8FD9\u6761\u547D\u4EE4 \u2192 \u8865\u4E00\u884C `$ cmd` \u5E76\u7A7A\u4E00\u884C", () => {
  import_strict.default.equal(withCommandHeader("`boom`", "npm test"), "$ npm test\n\n`boom`");
});
(0, import_node_test.default)("\u6B63\u6587\u5DF2\u7ECF\u542B\u8FD9\u6761\u547D\u4EE4\uFF08\u5F15\u7528\u6574\u5C4F\uFF09\u2192 \u4E0D\u52A0\u5934", () => {
  const body = "```\nadmin@host ~ % npm test\nboom\n```";
  import_strict.default.equal(withCommandHeader(body, "npm test"), body);
});
(0, import_node_test.default)("\u6CA1\u6709\u547D\u4EE4 / \u7A7A\u6B63\u6587 \u2192 \u539F\u6837\u8FD4\u56DE", () => {
  import_strict.default.equal(withCommandHeader("`boom`", void 0), "`boom`");
  import_strict.default.equal(withCommandHeader("`boom`", "   "), "`boom`");
  import_strict.default.equal(withCommandHeader("", "npm test"), "");
});
(0, import_node_test.default)("\u547D\u4EE4\u524D\u540E\u7A7A\u767D\u88AB trim\uFF08\u7EC8\u7AEF\u91CC\u5E38\u5E26\u5C3E\u90E8\u7A7A\u683C\uFF09", () => {
  import_strict.default.equal(withCommandHeader("`boom`", "  npm test  "), "$ npm test\n\n`boom`");
});
(0, import_node_test.default)("\u4ECE\u540E\u5F80\u524D\u627E\uFF1A\u540C\u540D\u547D\u4EE4\u51FA\u73B0\u4E24\u6B21\u65F6\u547D\u4E2D\u6700\u540E\u4E00\u6761", () => {
  const lines = ["% npm test", "ok", "% npm test", "fail"];
  import_strict.default.equal(findCommandLine(lines, "npm test"), 2);
});
(0, import_node_test.default)("\u63D0\u793A\u7B26\u4E0E\u547D\u4EE4\u540C\u4E00\u884C\u4E5F\u80FD\u547D\u4E2D", () => {
  import_strict.default.equal(findCommandLine(["admin@m ~ % git status", "clean"], "git status"), 0);
});
(0, import_node_test.default)("\u6CA1\u627E\u5230 / \u6CA1\u547D\u4EE4 \u2192 -1\uFF08\u8C03\u7528\u65B9\u9000\u56DE\u6574\u5C4F\uFF09", () => {
  import_strict.default.equal(findCommandLine(["a", "b"], "npm test"), -1);
  import_strict.default.equal(findCommandLine(["a"], ""), -1);
  import_strict.default.equal(findCommandLine(["a"], void 0), -1);
});
(0, import_node_test.default)("\u540C\u4E00\u4E2A\u547D\u4EE4 + \u76EE\u5F55 \u2192 `$ cmd   (in dir)`", () => {
  import_strict.default.equal(withCommandHeader("`boom`", "npm test", "src/api"), "$ npm test   (in src/api)\n\n`boom`");
});
(0, import_node_test.default)("\u76EE\u5F55\u4E3A\u7A7A / \u672A\u7ED9 \u2192 \u4E0E\u539F\u6765\u4E00\u81F4\uFF08\u4E0D\u5199\u62EC\u53F7\uFF09", () => {
  import_strict.default.equal(withCommandHeader("`boom`", "npm test", ""), "$ npm test\n\n`boom`");
  import_strict.default.equal(withCommandHeader("`boom`", "npm test", "   "), "$ npm test\n\n`boom`");
});
(0, import_node_test.default)("\u6B63\u6587\u5DF2\u542B\u547D\u4EE4 \u2192 \u5373\u4F7F\u7ED9\u4E86\u76EE\u5F55\u4E5F\u4E0D\u91CD\u590D", () => {
  import_strict.default.equal(withCommandHeader("```\n% npm test\nboom\n```", "npm test", "src"), "```\n% npm test\nboom\n```");
});
(0, import_node_test.default)("describeCwd\uFF1A\u4E0E\u5DE5\u4F5C\u533A\u76F8\u540C\uFF08inside \u4E14 rel \u7A7A\uFF09\u2192 \u7A7A\u4E32\uFF08\u4E0D\u5236\u9020\u566A\u97F3\uFF09", () => {
  import_strict.default.equal(describeCwd({ cwd: "/w/proj", rel: "", inside: true }), "");
});
(0, import_node_test.default)("describeCwd\uFF1A\u5DE5\u4F5C\u533A\u4E4B\u5185 \u2192 \u76F8\u5BF9\u8DEF\u5F84", () => {
  import_strict.default.equal(describeCwd({ cwd: "/w/proj/src/api", rel: "src/api", inside: true }), "src/api");
});
(0, import_node_test.default)("describeCwd\uFF1A\u5DE5\u4F5C\u533A\u4E4B\u5916 \u2192 \u7EDD\u5BF9\u8DEF\u5F84\uFF08\u76F8\u5BF9\u8DEF\u5F84\u4F1A\u8BEF\u5BFC\uFF09", () => {
  import_strict.default.equal(describeCwd({ cwd: "/Users/x/other", rel: "", inside: false }), "/Users/x/other");
});
(0, import_node_test.default)("describeCwd\uFF1A\u62FF\u4E0D\u5230 / undefined \u2192 \u7A7A\u4E32", () => {
  import_strict.default.equal(describeCwd(void 0), "");
  import_strict.default.equal(describeCwd({}), "");
  import_strict.default.equal(describeCwd({ inside: true, rel: "   " }), "");
});
