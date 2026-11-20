var bytesOf = (text) => new TextEncoder().encode(text).length;
(0, import_node_test.default)("\u5355\u884C\u3001\u4E0D\u542B\u53CD\u5F15\u53F7 \u2192 \u4E00\u5BF9\u53CD\u5F15\u53F7\u5305\u8D77\u6765", () => {
  import_strict.default.equal(formatSelection("git status"), "`git status`");
  import_strict.default.equal(formatSelection("echo hi && pwd"), "`echo hi && pwd`");
});
(0, import_node_test.default)("\u5355\u884C\u4F46\u542B\u53CD\u5F15\u53F7 \u2192 \u539F\u6587\u8FD4\u56DE\uFF0C\u4E0D\u5305", () => {
  import_strict.default.equal(formatSelection("echo `date`"), "echo `date`");
  import_strict.default.equal(formatSelection("`"), "`");
});
(0, import_node_test.default)("\u5355\u884C\uFF1A\u524D\u540E\u7684\u7A7A\u767D\u4E0E\u884C\u5C3E\u6362\u884C\u88AB trim\uFF0C\u7ED3\u679C\u4E0D\u542B\u9996\u5C3E\u6362\u884C", () => {
  const out = formatSelection("  ls -la  \n");
  import_strict.default.equal(out, "`ls -la`");
  import_strict.default.equal(out.includes("\n"), false);
});
(0, import_node_test.default)("\u591A\u884C \u2192 \u4E09\u53CD\u5F15\u53F7\u56F4\u680F\uFF0C\u56F4\u680F\u5404\u81EA\u5355\u72EC\u6210\u884C", () => {
  import_strict.default.equal(formatSelection("a\nb"), "```\na\nb\n```");
  import_strict.default.equal(formatSelection("a\nb\n\n\n"), "```\na\nb\n```", "text \u672B\u5C3E\u7684\u6362\u884C\u5148\u53BB\u6389");
});
(0, import_node_test.default)("\u591A\u884C\u6B63\u6587\u4FDD\u7559\u9996\u884C\u7F29\u8FDB\uFF08\u7F29\u8FDB\u5C31\u662F\u4EE3\u7801\u7684\u8BED\u4E49\uFF09", () => {
  import_strict.default.equal(formatSelection("  a\n    b\n"), "```\n  a\n    b\n```");
});
(0, import_node_test.default)("\u591A\u884C\u4E14\u672C\u8EAB\u542B\u4E09\u53CD\u5F15\u53F7 \u2192 \u56F4\u680F\u6539\u7528\u4E09\u6CE2\u6D6A\u7EBF", () => {
  const out = formatSelection("x\n```\ny\n```");
  import_strict.default.equal(out, "~~~\nx\n```\ny\n```\n~~~");
});
(0, import_node_test.default)("\u7A7A\u4E32\u6216\u5168\u7A7A\u767D \u2192 \u7A7A\u4E32", () => {
  import_strict.default.equal(formatSelection(""), "");
  import_strict.default.equal(formatSelection("   "), "");
  import_strict.default.equal(formatSelection("\n\n 	\n"), "");
});
(0, import_node_test.default)("\u6070\u597D 200 \u884C\u4E0D\u622A\u65AD", () => {
  const text = Array.from({ length: 200 }, (_, i) => `line${i}`).join("\n");
  const out = formatSelection(text);
  import_strict.default.equal(out.includes("\u5DF2\u622A\u65AD"), false);
  import_strict.default.equal(out.split("\n").length, 202, "\u56F4\u680F\u4E24\u884C + \u6B63\u6587 200 \u884C");
});
(0, import_node_test.default)("\u8D85\u8FC7 200 \u884C \u2192 \u622A\u5230 200 \u884C\uFF0C\u5E76\u6CE8\u660E\u539F\u6587\u884C\u6570", () => {
  const text = Array.from({ length: 201 }, (_, i) => `line${i}`).join("\n");
  const out = formatSelection(text);
  const inner = out.slice(4, -4);
  const lines = inner.split("\n");
  import_strict.default.equal(lines.length, 201, "200 \u884C\u6B63\u6587 + 1 \u884C\u622A\u65AD\u6807\u8BB0");
  import_strict.default.equal(lines[0], "line0");
  import_strict.default.equal(lines[199], "line199");
  import_strict.default.equal(lines[200], "\u2026 \uFF08\u5DF2\u622A\u65AD\uFF0C\u5171 201 \u884C\uFF09");
});
(0, import_node_test.default)("\u8D85\u8FC7 20000 \u5B57\u8282 \u2192 \u622A\u65AD\uFF08\u8D85\u957F\u5355\u884C\u4ECE\u4E2D\u95F4\u65AD\uFF09", () => {
  const out = formatSelection("a".repeat(20001));
  const inner = out.slice(4, -4);
  const body = inner.split("\n")[0];
  import_strict.default.equal(body.length, 2e4, "\u6B63\u6587\u6309\u5B57\u8282\u4E0A\u9650\u622A\u65AD");
  import_strict.default.equal(inner.endsWith("\u2026 \uFF08\u5DF2\u622A\u65AD\uFF0C\u5171 1 \u884C\uFF09"), true, "\u5355\u884C\u4E5F\u8981\u62A5\u603B\u884C\u6570");
  import_strict.default.equal(out.startsWith("```\n"), true);
  import_strict.default.equal(out.endsWith("\n```"), true);
});
(0, import_node_test.default)("\u6070\u597D 20000 \u5B57\u8282\u4E0D\u622A\u65AD\uFF08\u8FB9\u754C\u662F\u300C\u8D85\u8FC7\u300D\uFF09", () => {
  const out = formatSelection("a".repeat(2e4));
  import_strict.default.equal(out.includes("\u5DF2\u622A\u65AD"), false);
});
(0, import_node_test.default)("\u4E2D\u6587\u6309 UTF-8 \u5B57\u8282\u622A\u65AD\uFF0C\u4E0D\u7559\u534A\u4E2A\u5B57\uFF08U+FFFD\uFF09", () => {
  const out = formatSelection("\u4E2D".repeat(1e4));
  const body = out.slice(4, -4).split("\n")[0];
  import_strict.default.equal(body.includes("\uFFFD"), false);
  import_strict.default.equal(bytesOf(body) <= 2e4, true);
  import_strict.default.equal(body, "\u4E2D".repeat(6666), "\u6574\u5B57\u8FB9\u754C\uFF1A20000 \u5B57\u8282\u91CC\u53EA\u653E\u5F97\u4E0B 6666 \u4E2A\u6C49\u5B57");
});
(0, import_node_test.default)("\u7ED3\u679C\u5F62\u72B6\uFF1A\u5355\u884C\u7ED3\u679C\u9996\u5C3E\u6CA1\u6709\u591A\u4F59\u6362\u884C\uFF0C\u591A\u884C\u7ED3\u679C\u9996\u5C3E\u5404\u6070\u597D\u4E00\u4E2A\u56F4\u680F\u884C", () => {
  for (const text of ["ls", "a\nb", "x\n```\ny", "a".repeat(20001)]) {
    const out = formatSelection(text);
    if (!out.includes("\n")) {
      import_strict.default.equal(out.startsWith("\n"), false);
      import_strict.default.equal(out.endsWith("\n"), false);
      continue;
    }
    const lines = out.split("\n");
    import_strict.default.equal(lines[0].startsWith("```") || lines[0].startsWith("~~~"), true, "\u9996\u884C\u662F\u56F4\u680F");
    import_strict.default.equal(lines[lines.length - 1], lines[0], "\u5C3E\u884C\u4E0E\u9996\u884C\u540C\u4E00\u79CD\u56F4\u680F");
    import_strict.default.equal(lines.length > 2, true, "\u56F4\u680F\u4E4B\u95F4\u81F3\u5C11\u6709\u4E00\u884C\u5185\u5BB9");
  }
});
