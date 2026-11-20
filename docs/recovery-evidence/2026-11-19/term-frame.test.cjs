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

// src/components/terminal/frame-test.ts
var import_node_test = __toESM(require("node:test"));
var import_strict = __toESM(require("node:assert/strict"));

// src/components/terminal/frame.ts
var CTRL_BYTE = 0;
var encoder = new TextEncoder();
var decoder = new TextDecoder();
function encodeCtrl(c) {
  const json = encoder.encode(JSON.stringify(c));
  const out = new Uint8Array(json.length + 1);
  out[0] = CTRL_BYTE;
  out.set(json, 1);
  return out;
}
function decodeFrame(buf) {
  const bytes = buf instanceof Uint8Array ? buf : new Uint8Array(buf);
  if (bytes.length < 2 || bytes[0] !== CTRL_BYTE) return { data: bytes };
  try {
    const c = JSON.parse(decoder.decode(bytes.subarray(1)));
    if (!c || typeof c.t !== "string" || !c.t) return { data: bytes };
    return { ctrl: c };
  } catch {
    return { data: bytes };
  }
}

// src/components/terminal/frame-test.ts
var enc = new TextEncoder();
var dec = new TextDecoder();
(0, import_node_test.default)("\u63A7\u5236\u5E27\uFF1A0x00 \u5F00\u5934 + JSON\uFF0C\u5F80\u8FD4\u4E00\u81F4", () => {
  const frame = encodeCtrl({ t: "resize", cols: 120, rows: 40 });
  import_strict.default.equal(frame[0], 0);
  const { ctrl, data } = decodeFrame(frame);
  import_strict.default.equal(data, void 0);
  import_strict.default.deepEqual(ctrl, { t: "resize", cols: 120, rows: 40 });
});
(0, import_node_test.default)("\u6570\u636E\u5E27\uFF1A\u975E 0x00 \u5F00\u5934\u539F\u6837\u8FD4\u56DE\uFF08\u542B\u4E2D\u95F4\u7684 NUL \u4E5F\u7B97\u6570\u636E\uFF09", () => {
  const raw = new Uint8Array([104, 0, 105]);
  const { ctrl, data } = decodeFrame(raw);
  import_strict.default.equal(ctrl, void 0);
  import_strict.default.deepEqual(data, raw);
});
(0, import_node_test.default)("\u7578\u5F62\u63A7\u5236\u5E27\u6309\u6570\u636E\u5E27\u5904\u7406\uFF08\u4E0D\u5224\u6B7B\u8FDE\u63A5\uFF09", () => {
  const cases = [
    new Uint8Array([0]),
    new Uint8Array([0, 123]),
    // "{"
    new Uint8Array([0, 120, 121]),
    // "xy"
    enc.encode('\0{"t":""}')
    // t 为空
  ];
  for (const c of cases) {
    const { ctrl, data } = decodeFrame(c);
    import_strict.default.equal(ctrl, void 0);
    import_strict.default.deepEqual(data, c);
  }
});
(0, import_node_test.default)("\u8D85\u957F\u8F93\u51FA\u4E0D\u88AB JSON \u89E3\u6790\uFF08\u53EA\u8BA4\u9996\u5B57\u8282\uFF09", () => {
  const big = new Uint8Array(256 << 10).fill(97);
  const { ctrl, data } = decodeFrame(big);
  import_strict.default.equal(ctrl, void 0);
  import_strict.default.equal(data?.length, big.length);
});
(0, import_node_test.default)("UTF-8 \u591A\u5B57\u8282\u5B57\u7B26\u539F\u6837\u900F\u4F20\uFF08\u7EC8\u7AEF\u8F93\u51FA\u4F1A\u5207\u5728\u5B57\u7B26\u4E2D\u95F4\uFF09", () => {
  const raw = enc.encode("\u4E2D\u6587\u65E5\u5FD7");
  const { data } = decodeFrame(raw);
  import_strict.default.equal(dec.decode(data), "\u4E2D\u6587\u65E5\u5FD7");
});
(0, import_node_test.default)("ArrayBuffer \u4E0E Uint8Array \u4E24\u79CD\u5165\u53C2\u7B49\u4EF7", () => {
  const frame = encodeCtrl({ t: "exit", code: 3 });
  const ab = new ArrayBuffer(frame.byteLength);
  new Uint8Array(ab).set(frame);
  const fromBuf = decodeFrame(ab);
  const fromU8 = decodeFrame(frame);
  import_strict.default.deepEqual(fromBuf.ctrl, fromU8.ctrl);
});
