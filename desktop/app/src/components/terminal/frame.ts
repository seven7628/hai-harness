var CTRL_BYTE = 0;
var encoder = new TextEncoder();
var decoder2 = new TextDecoder();
function encodeCtrl(c2) {
  const json3 = encoder.encode(JSON.stringify(c2));
  const out = new Uint8Array(json3.length + 1);
  out[0] = CTRL_BYTE;
  out.set(json3, 1);
  return out;
}
function decodeFrame(buf) {
  const bytes = buf instanceof Uint8Array ? buf : new Uint8Array(buf);
  if (bytes.length < 2 || bytes[0] !== CTRL_BYTE) return { data: bytes };
  try {
    const c2 = JSON.parse(decoder2.decode(bytes.subarray(1)));
    if (!c2 || typeof c2.t !== "string" || !c2.t) return { data: bytes };
    return { ctrl: c2 };
  } catch {
    return { data: bytes };
  }
}
