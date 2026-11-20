var bytes = (text) => new TextEncoder().encode(text);
function sink() {
  const calls = [];
  const log = [];
  let live = 0;
  let peak = 0;
  const write = (data, done) => {
    const text = new TextDecoder().decode(data);
    live += 1;
    peak = Math.max(peak, live);
    log.push(`write:${text}`);
    let counted = false;
    calls.push({
      text,
      done: () => {
        if (!counted) {
          counted = true;
          live -= 1;
        }
        done();
      }
    });
  };
  const settle = () => {
    const last = calls[calls.length - 1];
    import_strict.default.ok(last, "\u6CA1\u6709 in-flight \u7684 write \u53EF\u4EE5\u653E\u884C");
    last.done();
  };
  return { write, calls, log, settle, peak: () => peak, texts: () => calls.map((c) => c.text) };
}
(0, import_node_test.default)("\u4E0D\u53D8\u91CF 1\uFF1Adone \u4E4B\u524D\u4E0D\u5199\u4E0B\u4E00\u5757\uFF0C\u540C\u65F6\u6700\u591A\u4E00\u4E2A in-flight", () => {
  const s = sink();
  const w = terminalWriter(s.write);
  w.push(bytes("a"));
  import_strict.default.deepEqual(s.texts(), ["a"], "\u961F\u5934\u7ACB\u523B\u5F00\u5199");
  w.push(bytes("b"));
  s.settle();
  w.push(bytes("c"));
  s.settle();
  import_strict.default.deepEqual(s.texts(), ["a", "b", "c"], "\u6BCF\u653E\u884C\u4E00\u6B21\u624D\u63A8\u8FDB\u4E00\u683C");
  import_strict.default.equal(s.peak(), 1);
});
(0, import_node_test.default)("\u4E0D\u53D8\u91CF 2\uFF1A\u7D27\u90BB\u7684\u8F93\u51FA\u5757\u5408\u5E76\u6210\u4E00\u6B21 write", () => {
  const s = sink();
  const w = terminalWriter(s.write);
  w.push(bytes("a"));
  w.push(bytes("b"));
  w.push(bytes("c"));
  s.settle();
  import_strict.default.deepEqual(s.texts(), ["a", "bc"], "\u6392\u961F\u7684\u4E24\u5757\u5408\u6210\u4E00\u6B21 write \u4E14\u987A\u5E8F\u4E0D\u53D8");
  import_strict.default.equal(s.peak(), 1);
});
(0, import_node_test.default)("\u4E0D\u53D8\u91CF 2\uFF1A\u6B65\u9AA4\u662F\u5408\u5E76\u7684\u5206\u754C\uFF0C\u8DE8\u4E0D\u8FC7\u53BB", () => {
  const s = sink();
  const w = terminalWriter(s.write);
  w.push(bytes("a"));
  w.push(bytes("b"));
  w.push(() => {
  });
  w.push(bytes("c"));
  w.push(bytes("d"));
  s.settle();
  s.settle();
  import_strict.default.deepEqual(s.texts(), ["a", "b", "cd"], "\u6B65\u9AA4\u524D\u540E\u7684\u8F93\u51FA\u5757\u5404\u81EA\u6210\u4E00\u6B21 write");
});
(0, import_node_test.default)("\u4E0D\u53D8\u91CF 3\uFF1A\u8F93\u51FA A \u2192 \u6B65\u9AA4 \u2192 \u8F93\u51FA B\uFF0C\u8C03\u7528\u987A\u5E8F\u4E0E\u5408\u5E76\u60C5\u51B5", () => {
  const s = sink();
  const w = terminalWriter(s.write);
  const step = () => s.log.push("step");
  w.push(bytes("A"));
  w.push(bytes("B"));
  w.push(step);
  w.push(bytes("C"));
  w.push(bytes("D"));
  import_strict.default.deepEqual(s.log, ["write:A"], "\u6B65\u9AA4\u5FC5\u987B\u7B49\u5230\u5B83\u524D\u9762\u90A3\u4E00\u683C\u5199\u5B8C");
  s.settle();
  import_strict.default.deepEqual(s.log, ["write:A", "write:B"], "\u6B65\u9AA4\u4E5F\u4E0D\u8BB8\u88AB\u540E\u9762\u7684\u8F93\u51FA\u62A2\u5148");
  s.settle();
  import_strict.default.deepEqual(s.log, ["write:A", "write:B", "step", "write:CD"]);
});
(0, import_node_test.default)("\u4E0D\u53D8\u91CF 3\uFF1A\u7A7A\u95F2\u65F6 push \u7684\u6B65\u9AA4\u7ACB\u523B\u6267\u884C\uFF0C\u8F93\u51FA\u8DDF\u5728\u5B83\u540E\u9762", () => {
  const s = sink();
  const w = terminalWriter(s.write);
  w.push(() => s.log.push("step"));
  w.push(bytes("a"));
  import_strict.default.deepEqual(s.log, ["step", "write:a"]);
});
(0, import_node_test.default)("\u4E0D\u53D8\u91CF 4\uFF1Aclear \u4E22\u5F03\u6392\u961F\u5185\u5BB9\uFF0C\u540E\u7EED push \u5168\u88AB\u5FFD\u7565", () => {
  const s = sink();
  const w = terminalWriter(s.write);
  w.push(bytes("a"));
  w.push(bytes("b"));
  w.clear();
  s.settle();
  w.push(bytes("c"));
  import_strict.default.deepEqual(s.texts(), ["a"], "clear \u4E4B\u540E\u4E00\u4E2A\u5B57\u90FD\u4E0D\u518D\u5199");
});
(0, import_node_test.default)("\u4E0D\u53D8\u91CF 4\uFF1Aclear \u4E4B\u540E\u8FDF\u5230\u7684 done \u4E0D\u4F1A\u6267\u884C\u961F\u5217\u91CC\u7684\u6B65\u9AA4", () => {
  const s = sink();
  const w = terminalWriter(s.write);
  let ran = 0;
  w.push(bytes("a"));
  w.push(() => {
    ran += 1;
  });
  w.clear();
  s.settle();
  import_strict.default.equal(ran, 0);
  import_strict.default.deepEqual(s.texts(), ["a"]);
});
(0, import_node_test.default)("\u4E0D\u53D8\u91CF 5\uFF1A\u7A7A Uint8Array \u4E0D\u4EA7\u751F write", () => {
  const s = sink();
  const w = terminalWriter(s.write);
  w.push(new Uint8Array(0));
  import_strict.default.deepEqual(s.log, [], "\u53EA\u6709\u7A7A\u5757\u65F6\u4E00\u6B21\u4E5F\u4E0D\u5199");
  w.push(bytes("a"));
  w.push(new Uint8Array(0));
  w.push(bytes("b"));
  s.settle();
  import_strict.default.deepEqual(s.texts(), ["a", "b"], "\u7A7A\u5757\u88AB\u8DF3\u8FC7\uFF0C\u4E0D\u5360\u4E00\u683C");
});
(0, import_node_test.default)("\u4E0D\u53D8\u91CF 6\uFF1A\u6B65\u9AA4\u629B\u5F02\u5E38\u53EA\u4E22\u5B83\u81EA\u5DF1\uFF0C\u961F\u5217\u7EE7\u7EED", () => {
  const s = sink();
  const w = terminalWriter(s.write);
  w.push(bytes("a"));
  w.push(() => {
    throw new Error("\u6B65\u9AA4\u70B8\u4E86");
  });
  w.push(bytes("b"));
  import_strict.default.doesNotThrow(() => s.settle());
  import_strict.default.deepEqual(s.texts(), ["a", "b"]);
  import_strict.default.equal(s.peak(), 1);
});
(0, import_node_test.default)("\u540C\u4E00\u4E2A done \u88AB\u56DE\u8C03\u4E24\u6B21\u4E0D\u4F1A\u8BA9\u4E24\u4E2A write \u540C\u65F6\u5728\u98DE", () => {
  const s = sink();
  const w = terminalWriter(s.write);
  w.push(bytes("a"));
  w.push(bytes("b"));
  w.push(bytes("c"));
  const first = s.calls[0];
  first.done();
  first.done();
  import_strict.default.equal(s.calls.length, 2, "\u7B2C\u4E8C\u6B21 done \u4E0D\u8BE5\u518D\u63A8\u8FDB\u4E00\u683C");
  import_strict.default.equal(s.peak(), 1);
  s.settle();
  import_strict.default.deepEqual(s.texts(), ["a", "bc"]);
});
