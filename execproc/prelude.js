// prelude.js —— codemode 沙箱侧的协议端（经 `node --import <data: URL>` 注入，
// **不在** stdin 上：stdin 整条留给模型写的脚本本身）。
//
// # 为什么经 --import 注入而不是把 prelude 与脚本拼在一起
//
// 拼接（`console.log = f; <用户脚本>`）会让 prelude 的行号占用脚本的起始行，
// 堆栈里的报错行号**全部偏移**，模型看到「第 3 行报错」却对不上自己写的第 3 行
// —— 那是模型无法自我修正的错误（一线排障反复踩到）。
// --import 是独立模块，加载完再评估 stdin 模块，行号与堆栈保持与用户脚本一致。
//
// # 职责边界（Phase 1）
//
// 本文件**只负责协议与输出截获**：握手、结果封送、错误归集。
// 工具桥接（tools.* / text / image / store）由 Phase 2 的 codemode 工具注入到
// 本 prelude 之后 —— 两者靠 process 上的一根「宿主钩子」相连，避免 Phase 1 先造
// 一套将来必然要改的桥接 API（无消费者的接口 = 接不上的接口）。
//
// # 输出为什么必须截获
//
// 协议的 hello/result/error 行走真 fd1（fs.writeSync(1, …)），而用户脚本的
// console.log/writeln/stdout.write 必须**不**出现在 fd1 上，否则宿主无法分辨
// 「哪一行是协议帧、哪一行是用户输出」。做法是替换 stdout.write 把内容攒起来，
// 退出时随 result 帧一起送回 —— 顺带白拿「stdout+stderr 合并」语义（与 bash
// 工具一致）。
//
// 注意 console.log 走的是 stdout.write，所以替换它即覆盖 console.log/log/info/
// debug/warn（warn/error/trace 走 stderr，一并替换）。
//
// # 为什么握手放在「最前面」而不是等宿主问
//
// prelude 是 ESM，顶部 await 才拿到模块命名空间，而它只能 import 具名导出。
// 握手因此在顶层 await 之后立即发出；宿主那边 stdjson.CheckHello 校验 major，
// 不兼容就把进程杀掉并把原因并入返回文本（见 execproc.Executor）。

import fs from 'node:fs';

const PROTO_MAJOR = 1;

// 真 fd1 上的协议输出：绕过一切被替换的 write。
const send = (obj) => {
  try {
    fs.writeSync(1, JSON.stringify(obj) + '\n');
  } catch {
    // fd1 已关（宿主先超时杀进程）：无处可报，静默。
  }
};

// ---- 用户 stdout/stderr 截获 ----
const chunks = [];
const realOut = process.stdout.write.bind(process.stdout);
const realErr = process.stderr.write.bind(process.stderr);

const capture = (c) => {
  try {
    chunks.push(Buffer.from(typeof c === 'string' ? c : String(c)));
  } catch {
    // 环形缓冲被脚本自己清空/改写：丢弃这段而不是让整次执行崩掉。
  }
};
// 返回值必须恒为 true 且回调必须被调用，否则 Node 认为写入失败 →
// EPIPE / 静默丢弃后续写入（console.log 的返回值语义是「是否接受」）。
const silent = (c, encOrCb, maybeCb) => {
  capture(c);
  const cb = typeof encOrCb === 'function' ? encOrCb : maybeCb;
  if (typeof cb === 'function') {
    try {
      cb();
    } catch {}
  }
  return true;
};
process.stdout.write = silent;
process.stderr.write = silent;

// ---- 结果封送 ----
const flush = () => {
  let out = '';
  try {
    out = Buffer.concat(chunks).toString();
  } catch {
    out = '';
  }
  chunks.length = 0;
  send({ notify: 'result', out, bytes: out.length });
};

// 排在**用户自己的** exit 监听器之后（append 语义）：用户退出钩子里的 console.log
// 必须先被捕获，否则 result 帧先于它发出，那部分输出就永远丢了。
// 必须是 once —— 用户可能在钩子里 process.exit()，重复 flush 会发两帧，
// 宿主读到第二帧时会误判协议不同步。
process.once('exit', flush);

// ---- 错误归集 ----
let reported = false;
const die = (kind, msg) => {
  if (reported) return; // unhandledRejection 与 uncaughtException 常成对触发，只报一次
  reported = true;
  send({ notify: 'error', kind, msg: String(msg) });
  // 不立刻 exitCode=1 就跑完：让用户自己注册的后续钩子有机会跑（部分脚本靠
  // exitCode 表达失败）；但**必须**置上，否则一次抛错的脚本会伪装成成功。
  process.exitCode = 1;
};
process.on('uncaughtException', (e) => die('script', e && e.stack ? e.stack : e));
process.on('unhandledRejection', (e) => die('script', e && e.stack ? e.stack : e));

// ---- Phase 2 桥接插槽 ----
// 宿主注入的 prelude 后缀（若存在）在此被 require/import 进来并调用。
// Phase 1 不注册该钩子（桥接属 Phase 2 的 codemode 工具），故此处是有意留空：
// 留一个明确命名、明确注释的空位，好过 Phase 1 先发明一套 Phase 2 必然要改的
// 桥接 API —— 设计文档评审意见 4：「无消费者的可选接口是最温床」。
globalThis.__codemodeHost = globalThis.__codemodeHost || null;

// ---- 握手 ----
// 放在模块顶层 await 之后：ESM 的 import 只取到具名导出，而顶层 await 之前的
// 静态 import 是 hoist 的，此时取不到。这也是「握手在脚本执行前发出」得以成立的
// 原因：模块依赖先于入口模块求值。
await 0;
send({
  notify: 'hello',
  // 字段名 version_major/version_minor 必须与 Go 侧 stdjson.Version 的 json tag
  // 逐字一致（这里是整个协议的唯一事实源：prelude.js ↔ execproc.Frame）。
  version: { version_major: PROTO_MAJOR, version_minor: 0, runtime: 'node' },
  pid: process.pid,
});
