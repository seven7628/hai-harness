// [recovered] from /private/tmp/builtin-surfaces.test.cjs
// 编译产物（esbuild `// src/...` 模块切片），依赖由 bundle 注入，未回译。
// @ts-nocheck
/* eslint-disable */
var MAX_LINES = 200;
var MAX_BYTES = 2e4;
function byteLength(text13) {
  return new TextEncoder().encode(text13).length;
}
function cutBytes(text13, limit2) {
  const bytes = new TextEncoder().encode(text13);
  if (bytes.length <= limit2) return text13;
  return new TextDecoder("utf-8").decode(bytes.slice(0, limit2)).replace(/\uFFFD$/, "");
}
function clip3(text13) {
  const kept = text13.split("\n").slice(0, MAX_LINES).join("\n");
  return byteLength(kept) > MAX_BYTES ? cutBytes(kept, MAX_BYTES) : kept;
}
function formatSelection(text13) {
  const trimmed = text13.trim();
  if (trimmed === "") return "";
  let body3 = trimmed.includes("\n") ? text13.replace(/\n+$/, "") : trimmed;
  const lines = body3.split("\n").length;
  if (lines > MAX_LINES || byteLength(body3) > MAX_BYTES) {
    body3 = `${clip3(body3)}
\u2026 \uFF08\u5DF2\u622A\u65AD\uFF0C\u5171 ${lines} \u884C\uFF09`;
  }
  if (!body3.includes("\n")) return body3.includes("`") ? body3 : `\`${body3}\``;
  const fence3 = body3.includes("```") ? "~~~" : "```";
  return `${fence3}
${body3}
${fence3}`;
}

