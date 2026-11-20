var emptyCommandLine = { line: "", dirty: false };
function trackCommandLine(state, data) {
  let line = state.line;
  let dirty = state.dirty;
  let submitted;
  for (const ch of data) {
    if (ch === "\r" || ch === "\n") {
      const cmd = line.trim();
      if (!dirty && cmd !== "") submitted = cmd;
      line = "";
      dirty = false;
      continue;
    }
    if (ch === "\x7F" || ch === "\b") {
      line = line.slice(0, -1);
      continue;
    }
    if (ch === "") {
      line = "";
      dirty = false;
      continue;
    }
    if (ch === "") {
      line = "";
      dirty = false;
      submitted = void 0;
      continue;
    }
    if (ch === "	" || ch === "\x1B") {
      dirty = true;
      continue;
    }
    if (ch >= " " && ch !== "\x7F") {
      line += ch;
      continue;
    }
    dirty = true;
  }
  return { state: { line, dirty }, submitted };
}
function withCommandHeader(body, command, dir) {
  const cmd = (command ?? "").trim();
  if (body === "") return body;
  if (cmd === "") return body;
  const where = (dir ?? "").trim();
  const line = where ? `$ ${cmd}   (in ${where})` : `$ ${cmd}`;
  if (body.includes(cmd)) return body;
  return `${line}

${body}`;
}
function describeCwd(info) {
  if (!info) return "";
  const rel = (info.rel ?? "").trim();
  const cwd = (info.cwd ?? "").trim();
  if (info.inside) return rel;
  return cwd;
}
function findCommandLine(lines, command) {
  const cmd = (command ?? "").trim();
  if (cmd === "") return -1;
  for (let i = lines.length - 1; i >= 0; i--) {
    if (lines[i].includes(cmd)) return i;
  }
  return -1;
}

