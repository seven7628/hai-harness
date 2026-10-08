// prelude_bridge.js —— codemode 沙箱侧的**桥接端**：宿主 ↔ 沙箱的双向 NDJSON 桥。
//
// 经第二个 `--import` 注入，排在 prelude.js（协议端：握手 / 输出截获 / result 帧）
// 之后、用户脚本之前 —— 顺序是硬语义：本模块的顶层 await 必须**等到 init** 才放行
// 用户脚本（`--import` 的模块依赖先于入口模块求值，实测确认），否则脚本里
// `tools.x()` 会先于工具表就位而报「未知工具」。
//
// # 两条通道各管一个方向（Phase 2 决策，见 IMPLEMENTATION-SPEC §7.1-1）
//
//	宿主 → 沙箱：stdin，一行一条 JSON：init（首条）/ reply（call 回执）/ shutdown
//	沙箱 → 宿主：fd1（**协议行的唯一归属**），带哨兵前缀的帧：
//	             hello（prelude.js 发）/ call（本模块发）/ result · error（prelude.js 发）
//
// 为什么宿主能**边跑边**读到 fd1：宿主经 sandbox.Sandbox.Run 拿到的输出是「跑完才
// 返回的整段字符串」（sandbox 只提供这一个面，且那份实现不允许新增面），请求/应答
// 却必须在脚本运行期间往返。故宿主在命令行里把子进程的 fd1/fd2 重定向到一个
// **宿主可 tail 的 spool 文件**（见 execproc/script.go 的 scriptCommand）。对本模块
// 而言 fd1 仍是「协议行出口」这件事没有任何变化 —— 帧形状、哨兵、白名单全同 Phase 1。
//
// # 事件循环保活：本文件最反直觉的一处，改之前先读完
//
// Node 在一件事都没有（无 handle、无在途请求）时**立刻**退出，而「脚本 await 一个
// 还没到的回执」本身不持有事件循环（顶层 await 悬空时 Node 甚至只打一行
// "unsettled top-level await" 就退，退出码 13）。所以：
//
//   - stdin 平时是 **unref** 的（脚本不用工具时能正常收尾退出，不会吊在等 stdin 上）；
//   - 只要有 call 未回执就 **ref** 回来（否则脚本 await 期间进程会自己死掉）；
//   - 最后一条回执落地、且再无未决调用时 **unref**（脚本跑完 → 事件循环空 → Node 发
//     'exit' → prelude.js 补发 result 帧 → 宿主收工）。
//
// 这条 ref/unref 曲线是「脚本自己的收尾」与「宿主读 stdin 的等待」之间唯一的分界线；
// 少了它，要么脚本 await 回执时进程自杀，要么脚本跑完进程永不退出（宿主挂到超时）。
//
// # 收尾不新造帧
//
// 脚本结束时**沿用**既有 result/error 帧（prelude.js 在 'exit' 上发）。本模块不插入
// 自己的 "done" 帧：宿主侧的 assemble 只消费 hello/result/error 三类（call 是另一条
// 通路的事），多造一种就得多一条分叉 —— 而「谁在什么时刻发收尾帧」这件事只该有一个
// 答案（Phase 1 已经给过）。
//
// # 边界（Phase 2 明确不做，别在这里补）
//
//   - store / text / image / ALL_TOOLS：需要 done/writes 帧与落盘策略（Wave 2 的
//     tools/builtin/codemode/protocol.go），本波只做传输层；
//   - 工具名归一化与撞名解决在 codemode 侧（datasheet §15.1）—— 本模块只认 init 里
//     给的**最终名**，不做二次改写（否则同一个名字在两处被归一化，撞名判定会失真）；
//   - 常驻 worker 池不做：一次执行一个进程（见 execproc 包注释取舍 1），
//     池化要重做「进程生命周期 = 脚本生命周期」这条前提，不是本模块能局部改的。

import fs from 'node:fs';

// 协议行的哨兵前缀（ASCII RS，0x1e）。**与 prelude.js / execproc.frameSentinel 同源**，
// 三处改一处必须改三处；protoMajor 是这条耦合的兜底检查。
const SENTINEL = '\x1e';

// call 帧单行字节上限 = 宿主解码器 runtime/stdjson.MaxLine（4 MiB）**减去哨兵/换行
// 的余量**（宿主按整行做 token，哨兵也在那一行里）。跨语言没有共享常量的办法，故在
// 这里复述一遍；超限**本地拒绝**而不是照发 —— 宿主的分帧器会丢掉解不开的超长行，
// 那次调用就永远等不到回执（脚本挂到超时），比一条可 catch 的「参数太大」坏得多。
const MAX_LINE_BYTES = (4 << 20) - 1024;

// 真 fd1 上的协议输出：绕过 prelude.js 替换过的 process.stdout.write。
// 返回 false = 没写出去（fd 已关 / 被宿主杀掉），调用方据此把在途调用立刻拒掉。
const sendRaw = (json) => {
  try {
    fs.writeSync(1, SENTINEL + json + '\n');
    return true;
  } catch {
    return false;
  }
};
const send = (obj) => sendRaw(JSON.stringify(obj));

// 桥接级故障（不是脚本的错）：与 prelude.js 的 error 帧同形状（kind 区分归属），
// 宿主侧 assemble 会把第一条 error 帧的 msg 作为 [script error] 交给模型。
const die = (msg) => {
  send({ notify: 'error', kind: 'bridge', msg: String(msg) });
  process.exitCode = 1;
};

// ---- 1) init：同步读首行 ------------------------------------------------
//
// 为什么用阻塞 readSync 而不是异步读：init 是**唯一一条必须在用户脚本求值前落地**
// 的消息（工具表没建好，脚本里的 tools.x 就是未定义）。此时事件循环里没有任何别的
// 活，同步阻塞不牺牲任何并发；而异步等待又要额外操心「等 init 期间进程会不会因为
// 循环空转而退出」。首行之后的所有消息走异步通道（见 2），不能再用同步读 ——
// 那会把事件循环钉死，脚本内 Promise.all 的并发就没了。
let inbound = Buffer.alloc(0); // 已读出但尚未成行的字节（同步段与异步段共用）
const readLineSync = () => {
  for (;;) {
    const i = inbound.indexOf(0x0a);
    if (i >= 0) {
      const line = inbound.subarray(0, i).toString();
      inbound = inbound.subarray(i + 1);
      return line;
    }
    const chunk = Buffer.alloc(64 << 10);
    let n;
    try {
      n = fs.readSync(0, chunk, 0, chunk.length, null);
    } catch (e) {
      // 少数环境把 stdio 管道置成非阻塞（O_NONBLOCK）→ readSync 抛 EAGAIN 而不是
      // 阻塞等待。这里没有别的活可干，只能小睡重试（Atomics.wait 是唯一可用的同步
      // 睡眠）；不处理的话「等 init」会变成启动即崩。
      if (e.code === 'EAGAIN') {
        Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 1);
        continue;
      }
      throw e;
    }
    if (n <= 0) return null; // stdin 已关：宿主没送 init
    inbound = Buffer.concat([inbound, chunk.subarray(0, n)]);
  }
};

let init = {};
{
  const first = readLineSync();
  if (first === null) {
    // 宿主必须先发 init。走到这里说明宿主没接上（或提前关了 stdin）——
    // 报一条给模型看得懂的错，而不是让脚本跑成一堆 "tools is not defined"。
    die('codemode bridge: stdin closed before the init payload arrived ' +
      '(the host must send init as the first line; the bridge cannot build the tool table)');
    process.exit(1);
  }
  let msg;
  try {
    msg = JSON.parse(first);
  } catch (e) {
    die(`codemode bridge: first stdin line is not valid JSON (${e.message})`);
    process.exit(1);
  }
  if (!msg || msg.notify !== 'init') {
    die(`codemode bridge: expected an init message first, got ${JSON.stringify(msg && msg.notify)}`);
    process.exit(1);
  }
  init = msg.init ?? {};
}

// ---- 2) 回执通道（异步）+ 事件循环保活 -----------------------------------

const pending = new Map(); // id → {resolve, reject, name}
let armed = false; // stdin 当前是否 ref 着（= 有在途调用；见文件头保活说明）

const arm = () => {
  if (armed) return;
  armed = true;
  // unref/ref 在极端环境可能不存在（stdin 不是 pipe 的形态）：不是致命条件，
  // 缺了它只是退化成「必须等宿主关 stdin」—— 故不抛错，只降级。
  if (typeof process.stdin.ref === 'function') process.stdin.ref();
};
const disarm = () => {
  if (!armed) return;
  armed = false;
  if (typeof process.stdin.unref === 'function') process.stdin.unref();
};

// failAllPending 把所有在途调用一次性拒掉：宿主关了通道（shutdown / stdin EOF）时，
// 等着的 await 必须有个结局 —— 悬空 = 脚本挂到超时，且看不出原因。
let closedReason = '';
const failAllPending = (reason) => {
  closedReason = closedReason || reason;
  const victims = [...pending.values()];
  pending.clear();
  disarm();
  for (const p of victims) {
    const err = new Error(reason);
    err.codemodeTransport = true;
    p.reject(err);
  }
};

const handle = (line) => {
  if (line === '') return;
  let msg;
  try {
    msg = JSON.parse(line);
  } catch (e) {
    // 宿主是这条通道的唯一写者，解不开就是协议失步：继续读下去只会让在途调用
    // 一个个悬空，故一次性收摊并说清原因。
    failAllPending('codemode bridge: host sent a line that is not valid JSON ' +
      `(${e.message}) — the bridge is out of sync`);
    die(`codemode bridge: undecodable host message (${e.message})`);
    return;
  }
  switch (msg && msg.notify) {
    case 'reply': {
      const p = pending.get(msg.id);
      if (!p) return; // 迟到/未知 id：宿主与沙箱各记一份在途表，对不上时唯一安全动作是丢弃
      pending.delete(msg.id);
      if (pending.size === 0) disarm();
      if (msg.error !== undefined) p.reject(new Error(String(msg.error)));
      else p.resolve(msg.result);
      return;
    }
    case 'shutdown':
      failAllPending(`codemode bridge: the host shut the bridge down${
        msg.reason ? ` (${msg.reason})` : ''}` +
        ' — tool calls cannot complete without a host');
      return;
    default:
      // 未知 notify（宿主比沙箱新）：不静默吞，但不致命 —— 仍然让脚本先失败，
      // 免得它对着一个永远不来的回执等满整段时限。
      failAllPending(`codemode bridge: unknown host message ${JSON.stringify(msg && msg.notify)}`);
      die(`codemode bridge: unknown host message ${JSON.stringify(msg && msg.notify)}`);
      return;
  }
};

const drainLines = () => {
  for (;;) {
    const i = inbound.indexOf(0x0a);
    if (i < 0) break;
    const line = inbound.subarray(0, i).toString();
    inbound = inbound.subarray(i + 1);
    handle(line);
  }
};

const onChunk = (chunk) => {
  inbound = inbound.length === 0 ? Buffer.from(chunk) : Buffer.concat([inbound, chunk]);
  drainLines();
};

// 起读：attach 即进入 flowing 模式（一直有在途 read），unref 后它不持有事件循环 ——
// 「数据照收、不影响退出」，这正是保活曲线需要的行为（见文件头）。
process.stdin.on('data', onChunk);
process.stdin.on('error', () => { /* 宿主先死：写作失败自会报错，这里只吞掉 EPIPE 噪声 */ });
process.stdin.on('end', () => {
  if (pending.size > 0) failAllPending('codemode bridge: host closed stdin while calls were in flight');
});
process.stdin.unref();
drainLines(); // 同步段可能把 init 之后的字节一起读进来了（正常为空，是兜底）

// ---- 3) 一次工具调用 = 一条 call 帧 + 一个等回执的 Promise -----------------

let seq = 0;
const callTool = (name, args) => new Promise((resolve, reject) => {
  if (closedReason) {
    reject(new Error(`${closedReason} (call to ${JSON.stringify(name)} was not sent)`));
    return;
  }
  // 参数必须是**单个 JSON 对象**（宿主侧 ScriptCall.Args 的契约）。宽容到「undefined
  // = 无参」，其余基本类型一律本地报错：把 "a.txt" 当参数发出去，工具侧只会拿到一个
  // schema 校验失败，模型看不到「我少写了一对大括号」这个真正的原因。
  let payload;
  if (args === undefined || args === null) payload = {};
  else if (typeof args === 'object') payload = args;
  else {
    reject(new Error(`tool ${JSON.stringify(name)}: arguments must be a single JSON object, ` +
      `got ${typeof args} — call it as tools.${name}({...})`));
    return;
  }

  const id = String(++seq); // 单次执行内唯一（一次一进程 ⇒ 跨执行天然不会重号）
  let json;
  try {
    json = JSON.stringify({ notify: 'call', id, name, args: payload });
  } catch (e) {
    // 循环引用 / BigInt：JSON.stringify 会抛，若直接让它冒到 send 里，帧发不出去而
    // Promise 已经建好 —— 那次 await 就永远等不到回执。
    reject(new Error(`tool ${JSON.stringify(name)}: arguments are not JSON-serializable (${e.message})`));
    return;
  }
  if (Buffer.byteLength(json) > MAX_LINE_BYTES) {
    reject(new Error(`tool ${JSON.stringify(name)}: arguments too large ` +
      `(${Buffer.byteLength(json)} bytes > ${MAX_LINE_BYTES}) — pass a path or a smaller slice`));
    return;
  }

  pending.set(id, { resolve, reject, name });
  arm(); // 有在途调用 ⇒ 保住事件循环（见文件头）；先 arm 再发帧，避免回执抢跑
  if (!sendRaw(json)) {
    pending.delete(id);
    if (pending.size === 0) disarm();
    reject(new Error(`tool ${JSON.stringify(name)}: the sandbox could not write to fd1 ` +
      '(the host is gone)'));
  }
});

// ---- 4) tools：按 init 的工具表建函数 -------------------------------------

// 工具表形状（宿主侧 execproc.ScriptOpts.Init 的契约，写成宽容解析是为了不给
// Wave 2 的字段命名绑定死——integration 面越窄越好）：
//
//	{"tools": [{"name": "read_file", "rawName": "mcp__dev-radius__read-file"}, "grep"], ...}
//	{"tools": {"read_file": {...}, "grep": {...}}}            // map 形态也认
//
// 其余键（store 等）本波不用，原样挂在 __codemode.init 上供脚本/宿主排查。
const toolNames = [];
const registry = Object.create(null); // 无原型：'toString' 之类的键不会被原型链劫持
const registerTool = (name, rawName) => {
  if (typeof name !== 'string' || name === '') return;
  const fn = (args) => callTool(name, args);
  // 归一化名先到先得（对齐 pi）：两个工具归一化后撞名时，让先注册的那个守住位置 ——
  // 静默覆盖会让脚本调到「另一个工具」，那是最难查的一类错。后到的仍可经自己的
  // 原始名双键调用。
  if (registry[name] === undefined) {
    registry[name] = fn;
    toolNames.push(name);
  }
  // 双键：原始名（如 mcp__dev-radius__x，含连字符，不是合法标识符，只能下标访问）。
  // 同样不覆盖既有键：一个工具的原始名可能恰好等于另一个工具的归一化名。
  if (typeof rawName === 'string' && rawName !== '' && registry[rawName] === undefined) {
    registry[rawName] = fn;
  }
};

{
  const list = init && init.tools;
  if (Array.isArray(list)) {
    for (const t of list) {
      if (typeof t === 'string') registerTool(t);
      else if (t && typeof t === 'object') registerTool(t.name, t.rawName ?? t.raw);
    }
  } else if (list && typeof list === 'object') {
    for (const name of Object.keys(list)) registerTool(name);
  }
}

// 未知工具：**在沙箱内**给出可 catch 的错误。为什么不让 tools.nope 变成 undefined
// （那样调用报的是 "tools.nope is not a function"）：模型看到「目录里没这个名字」
// 才知道自己该去核对工具表，而不是去猜调用语法。代价是 `if (tools.x)` 这种
// 特性探测恒真 —— 编排列出了工具表，本就不该靠探测猜能力（`'x' in tools` 仍准确）。
globalThis.tools = new Proxy(registry, {
  get(target, prop, recv) {
    if (typeof prop === 'symbol' || prop in target) return Reflect.get(target, prop, recv);
    return function unknownTool() {
      throw new Error(`unknown tool ${JSON.stringify(String(prop))}: not in this session's ` +
        'codemode tool table (check the tool list in the tool description)');
    };
  },
});

// ---- 5) 载荷暴露（调试/宿主归属）------------------------------------------

// 宿主注入的原始初始化载荷。下划线前缀 = 非公共 API：脚本可以用（模型排查时能看到
// 自己有哪些工具），但 Wave 2 若要把 store()/text() 做起来，应当**提升**成正式全局
// 而不是在这里继续堆字段（无消费者的接口是最温床，见 prelude.js 的同款注释）。
globalThis.__codemode = {
  init,
  toolNames, // 归一化名列表（不含原始名双键）—— 只读快照，registerTool 之后不再变
};
