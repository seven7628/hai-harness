// prelude_bridge.js —— codemode 沙箱侧的**桥接端**：宿主 ↔ 沙箱的双向 NDJSON 桥。
//
// 经第二个 `--import` 注入，排在 prelude.js（协议端：握手 / 输出截获 / result 帧）
// 之后。**用户脚本由本模块求值**（`new AsyncFunction`，见 #求值形态），不再作为
// 入口模块 —— init 在这里被同步读掉、工具表也在这里建好，脚本才第一次被求值，
// 故脚本里的 `tools.x()` 不会先于工具表就位而报「未知工具」。
//
// # 两条通道各管一个方向（Phase 2 决策，见 IMPLEMENTATION-SPEC §7.1-1）
//
//	宿主 → 沙箱：stdin，一行一条 JSON：init（首条，**含脚本源码**）/ reply / shutdown
//	沙箱 → 宿主：fd1（**协议行的唯一归属**），带哨兵前缀的帧：
//	             hello（prelude.js 发）/ call（本模块发）/ result · error（prelude.js 发）
//
// 脚本源码走 **init.script**（Phase 1-A2）：宿主不再把它写成临时文件 —— 临时目录
// 里只剩帧 spool。Phase 1 用 stdin 换来「脚本不落盘」，而 Phase 2 把 stdin 让给了
// 桥接通道，这一条（脚本可能含密钥/路径，属凭据面）必须继续成立，故源码与 init
// 同一条消息送出、在内存里求值。
//
// 为什么宿主能**边跑边**读到 fd1：宿主经 sandbox.Sandbox.Run 拿到的输出是「跑完才
// 返回的整段字符串」（sandbox 只提供这一个面，且那份实现不允许新增面），请求/应答
// 却必须在脚本运行期间往返。故宿主在命令行里把子进程的 fd1/fd2 重定向到一个
// **宿主可 tail 的 spool 文件**（见 execproc/script.go 的 scriptCommand）。对本模块
// 而言 fd1 仍是「协议行出口」这件事没有任何变化 —— 帧形状、哨兵、白名单全同 Phase 1。
//
// # 求值形态（Phase 1-A2，改之前先读完）
//
// 脚本是 `new AsyncFunction('tools', <源码>)` 的**函数体**，入参 tools（现有 registry）：
//
//   - 顶层 `return` 合法（这就是本形态存在的理由）：模型侧契约承诺
//     「the value you return becomes this tool's result」（DESCRIPTION_INTRO），
//     而入口模块形态下 `return` 是 `SyntaxError: Illegal return statement`（实测）；
//   - 顶层 `await` 合法（函数体里 await 本就合法，不必依赖 ESM）；
//   - 堆栈行号与用户脚本 1:1：`//# sourceURL=codemode-script.mjs` + **构造时不加
//     前置行**（源码从第 1 行开始）+ 对本进程实测出的包装头偏移做回补（#行号对齐）；
//   - **静态 `import` 不可用**（函数体不是模块）：需要模块的脚本用 `await import()`。
//     pi 同样不支持静态 import，教学正文也只承诺 `tools.*` 是走出去的受支持方式。
//     该取舍的报错文案有专门提示（见 compileErrorText），不会让模型以为沙箱坏了。
//
// # 事件循环保活：本文件最反直觉的一处，改之前先读完
//
// Node 在一件事都没有（无 handle、无在途请求）时**立刻**退出，而「脚本 await 一个
// 还没到的回执」本身不持有事件循环。所以：
//
//   - stdin 平时是 **unref** 的（脚本不用工具时能正常收尾退出，不会吊在等 stdin 上）；
//   - **脚本还在跑**、或**有 call 未回执**就 **ref** 回来（那两种情况下进程都还不能死）；
//   - 脚本跑完（或报错收尾）、且再无未决调用时 **unref**（脚本跑完 → 事件循环空 →
//     Node 发 'exit' → prelude.js 补发 result 帧 → 宿主收工）。
//
// 这条 ref/unref 曲线是「脚本自己的收尾」与「宿主读 stdin 的等待」之间唯一的分界线；
// 少了它，要么脚本 await 回执时进程自杀，要么脚本跑完进程永不退出（宿主挂到超时）。
//
// **Phase 1-A2 把条件从「有未决调用」扩到「脚本还在跑 ∨ 有未决调用」，理由是实测的**：
// 本模块现在是**预加载模块**而不是入口模块，node 不为预加载模块的顶层 await 保命
// （实测：预加载模块顶层 await 悬空 → 进程直接以 0 退出，连 'exit' 都与脚本的结局
// 无关 —— 入口模块至少还会打一行 "unsettled top-level await" 并以 13 退出）。
// 没有「脚本还在跑」这一条，一个既不调工具、又不留 handle 的脚本
// （例如 `await new Promise(() => {})`）会让进程静默退出，宿主看到的是
// 「有握手、没有 result 帧」—— 最坏的一种失败形态（比超时难查得多）。
// 代价（明知并接受）：这类挂死不再秒退，而是走宿主墙钟/沙箱的三层 kill 预算，
// 报错形态统一为 [TIMEOUT …]，与「死循环」「调用不回执」两种挂死一致。
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
//     tools/builtin/codemode/protocol.go）。本模块只提供**槽**：`globalThis.exit`
//     （传输层语义）与 `ScriptOpts.Scaffold` 的求值路径；Wave 2 的 scaffold 用
//     `globalThis.<name> = …` 在这些槽旁边装自己的全局；
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

// process.exit 的**真身**：exit(value)（见 §6）用它收尾，若被脚本改写就用不了。
// 这是本模块唯一一处「先抓住再调用」的内建函数，其余调用照常走 process.*。
const hardExit = process.exit;

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

// ---- 1b) 脚本源码与 scaffold：从 init 里取出来（**永不落盘**）-------------------
//
// 源码是 init 的两个**传输字段**，不是给脚本看的初始化载荷：取走后立刻从 init 上删掉，
// 让 `__codemode.init`（§5 暴露给脚本的那份）与宿主 `ScriptOpts.Init` 传进来的原文
// 逐键一致 —— Wave 2 的 protocol 组合 init 时不必知道传输层往里塞了什么。
const scriptSource = typeof init.script === 'string' ? init.script : '';
const scaffoldSource = typeof init.scaffold === 'string' ? init.scaffold : '';
delete init.script;
delete init.scaffold;

if (scriptSource === '') {
  // 宿主与本模块同一版本时不会发生；真发生了就是协议失步，说清楚而不是让脚本空跑。
  die('codemode bridge: the init payload carried no script source ' +
    '(the host sends the script as init.script — it never lands on disk in this design)');
  process.exit(1);
}

// ---- 2) 回执通道（异步）+ 事件循环保活 -----------------------------------

const pending = new Map(); // id → {resolve, reject, name}
let armed = false; // stdin 当前是否 ref 着（= 有在途调用或脚本还在跑；见文件头保活说明）

// scriptRunning 脚本（含 scaffold）是否还在求值。**保活曲线的第二个条件**，见文件头
// 「事件循环保活」一节：本模块是预加载模块，node 不为它的顶层 await 保命，故脚本
// 期间必须由我们自己 ref 住 stdin，否则「不调工具也不留 handle」的脚本会让进程静默退出。
let scriptRunning = false;

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

// syncKeepAlive 按**当前的两个条件**重新决定 ref/unref。所有会改变条件的点都必须
// 调它一次（脚本起止、调用入表/出表、批量失败）—— 散落的 if (pending.size === 0)
// disarm() 正是 Phase 1-A2 之前「最后一条回执落地就 unref」的写法，那时还没有
// 「脚本还在跑」这个条件，留着它会把脚本期间的保活提前撤掉。
const syncKeepAlive = () => {
  if (scriptRunning || pending.size > 0) arm();
  else disarm();
};

// failAllPending 把所有在途调用一次性拒掉：宿主关了通道（shutdown / stdin EOF）时，
// 等着的 await 必须有个结局 —— 悬空 = 脚本挂到超时，且看不出原因。
let closedReason = '';
const failAllPending = (reason) => {
  closedReason = closedReason || reason;
  const victims = [...pending.values()];
  pending.clear();
  syncKeepAlive();
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
      syncKeepAlive();
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
  syncKeepAlive(); // 有在途调用 ⇒ 保住事件循环（见文件头）；先保活再发帧，避免回执抢跑
  if (!sendRaw(json)) {
    pending.delete(id);
    syncKeepAlive();
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

// 宿主注入的原始初始化载荷（**不含** script/scaffold 两个传输字段，见 §1b）。
// 下划线前缀 = 非公共 API：脚本可以用（模型排查时能看到自己有哪些工具），
// 但 Wave 2 若要把 store()/text() 做起来，应当**提升**成正式全局而不是在这里继续
// 堆字段（无消费者的接口是最温床，见 prelude.js 的同款注释）。
globalThis.__codemode = {
  init,
  toolNames, // 归一化名列表（不含原始名双键）—— 只读快照，registerTool 之后不再变
};

// ---- 6) 求值：行号对齐 → exit → scaffold（可选）→ 用户脚本 ------------------

const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;

// 两个「函数体形态」的求值单元。名字（sourceURL）不同、入参不同，其余全同：
// 编译/运行错误走同一条收尾路径（见 runBody），两处不会各自漂移。
const SCRIPT_FILE = 'codemode-script.mjs';
const SCAFFOLD_FILE = 'codemode-scaffold.mjs';
const PROBE_FILE = 'codemode-line-probe.mjs';

// # 行号对齐（Phase 1 为它付过代价，这条必须继续成立）
//
// `//# sourceURL=` 只解决「堆栈里显示什么名字」，**不解决行号偏移**：V8 的
// Function/AsyncFunction 构造器不是把 body 当源码用，而是先生成
//
//	async function anonymous(tools
//	) {
//	<body 第 1 行>
//
// 再编译 —— 那两行包装头把 body 的第 N 行顶成堆栈里的第 N+2 行（本机 node v25.8.1
// 实测；「不加前置行就能 1:1」对构造函数不成立）。故偏移量用**探针在本进程里实测**
// 一次，而不是把 2 写死：V8 换代/换引擎后探针自己给出新的数，测不到就不偏移
// （差一个常数总好过乱改行号）。探针把 throw 放在 body 第 2 行，堆栈里读到的行号
// 减 2 就是包装头的行数。
let lineOffset = 0;
const probeRe = new RegExp(PROBE_FILE.replace(/\./g, '\\.') + ':(\\d+):');

const measureLineOffset = async () => {
  try {
    const probe = new AsyncFunction('tools',
      '\nthrow new Error("codemode line-offset probe");\n//# sourceURL=' + PROBE_FILE);
    await probe({}); // 必然 reject（body 第 2 行就 throw）
    return 0;
  } catch (e) {
    const m = probeRe.exec((e && e.stack) || '');
    if (!m) return 0;
    const n = Number(m[1]) - 2;
    return Number.isInteger(n) && n > 0 ? n : 0;
  }
};

// 堆栈里只有这两个名字是本模块给出去的，别的帧（bridge 自己、node internal、
// data: URL）不归我们改。
const frameRe = new RegExp(
  '(' + [SCRIPT_FILE, SCAFFOLD_FILE].map((f) => f.replace(/\./g, '\\.')).join('|') +
  '):(\\d+):(\\d+)', 'g');

// rewriteStack 回补包装头偏移（只动上面两个文件名的帧；列号不受影响 —— body 的每行
// 都从第 0 列开始，实测列号本来就对）。prelude.js 的 die 也走这里（见那里的插槽注释）：
// 用户脚本里 setTimeout 回调抛的错不走本模块的 await，那条路径同样需要回补。
const rewriteStack = (text) => {
  if (!lineOffset || typeof text !== 'string') return text;
  return text.replace(frameRe, (whole, file, line, col) => {
    const n = Number(line) - lineOffset;
    return `${file}:${n >= 1 ? n : 1}:${col}`;
  });
};

// # 结局值（result 帧的 value 字段）
//
// 形状 {value} | null：null = 无返回值 ⇒ 帧里不带 value 字段 ⇒ 宿主 RunResult.Value
// 为 nil。**归一化成 JSON 安全的值**是这里做的（不是交给 prelude 的 JSON.stringify）：
// 脚本可能返回函数/BigInt/循环引用，JSON.stringify 会抛或静默丢键，而丢帧（宿主看到
// 「没有 result 帧」）比一条「返回值没法编码」的文本难查得多。往返一次 JSON 之后，
// prelude 那边的序列化就不可能再失败。
//
// 声明放在挂插槽**之前**：插槽里的 result() 会在 'exit' 那一刻被 prelude 调，
// 而 die 可能在模块求值中途就触发（那时 const/let 还在 TDZ —— prelude 侧有 try/catch
// 兜底，但能不给它兜的机会就别给）。
let result = null;

// 挂给协议端（prelude.js）：result() 取结局值、rewriteStack() 回补行号。挂的时机
// 必须在**任何用户代码之前**（脚本里 setTimeout 的报错、scaffold 的报错都要走到它）。
globalThis.__codemodeBridge = {
  result: () => result,
  rewriteStack,
};

const safeString = (v) => {
  try {
    return String(v);
  } catch {
    return '[unprintable value]';
  }
};

// 结局值的传输上限（字节，JSON 文本长度）。
//
// 为什么必须有：result 帧是**一行** JSON，而宿主的解码器（runtime/stdjson.MaxLine =
// 4 MiB）按行读。超限的那一行解析不了 → 宿主既拿不到 value 也拿不到 out（整条帧被
// 当成「非帧文本」），而且那行 JSON（含全部内容）会被当作 tail 灌进模型上下文 ——
// 一次「返回了大对象」的笔误能顶掉整次执行的结局。故超限就**降级成一句可读文本**
// （模型据此改成返回路径/摘要），与「值不可编码」同一条路。
//
// 1 MiB 的依据：同一行里还有 out 与 JSON 包装，与 4 MiB 之间要留足余量；而 1 MiB 的
// 返回值在任何上层预算下都用不上（Wave 2 的 codemode 工具结果预算是 40 KB）。
// 只压 value 这一半：out 的大小由上层输出预算管（那是 Phase 1 既有形态，不在本层）。
const MAX_VALUE_BYTES = 1 << 20;

const normalizeResult = (v) => {
  if (v === undefined) return null;
  try {
    const s = JSON.stringify(v);
    if (s === undefined) return null; // 函数/Symbol：JSON 里没有表示，按无值处理
    const bytes = Buffer.byteLength(s);
    if (bytes > MAX_VALUE_BYTES) {
      return { value: `[the return value was dropped: ${bytes} bytes of JSON exceeds the ` +
        `${MAX_VALUE_BYTES}-byte transport limit for one result frame — return a path, ` +
        'an id, or a summary instead]' };
    }
    return { value: JSON.parse(s) };
  } catch (e) {
    const why = (e && e.message) || safeString(e);
    return { value: `[the return value is not JSON-serializable: ${why}] ${safeString(v)}` };
  }
};

const setResult = (v) => {
  result = normalizeResult(v);
};

// # globalThis.exit(value?)（IMPLEMENTATION-SPEC §7.9-3）
//
// 语义：置结果 + **立刻、干净地**结束脚本。刻意不是异常路径 —— 抛哨兵会被用户自己的
// try/catch 吞掉，脚本继续往下跑，那就不叫「提前结束」了。与 process.exitCode 无关：
// 本调用恒以 0 收尾（脚本自己声明了「这就是结果」；用它表达失败的脚本请自己写
// `process.exitCode = 1` 后 `return`）。hardExit 走 prelude.js 的 'exit' 钩子 ⇒
// 输出与带 value 的 result 帧都已落地（两处都是同步写 fd1）。
globalThis.exit = (value) => {
  setResult(value);
  hardExit.call(process, 0);
};

// # 报错收尾
//
// fail 发 error 帧后**立刻结束进程**（不是「置个 exitCode 就撒手」）：脚本报错时若
// 还留着脚本自己注册的 handle（interval/子进程），自然退出要等到天荒地老，宿主那一侧
// 就变成 [TIMEOUT] —— 而 Phase 1-A（脚本作入口模块时 node 的模块求值失败是致命错误）
// 的行为是立刻退出，这条必须保持：报错就是报错，不该伪装成超时。
const fail = (kind, text) => {
  send({ notify: 'error', kind, msg: rewriteStack(text) });
  process.exitCode = 1;
  hardExit.call(process, 1); // 触发 prelude 的 flush：输出先于进程消失落地
};

// 脚本自己抛的错：文本形态与 Phase 1-A 一致（e.stack 原文，行号已回补），
// 宿主的 assemble 会把它印在 [script error] 下面。
const errorText = (err) => {
  if (err && typeof err.stack === 'string' && err.stack !== '') return err.stack;
  return safeString(err);
};

// 静态 import 的提示：这是 AsyncFunction 形态**唯一**的语法级取舍（见文件头
// 「求值形态」），而 V8 的原文 "Cannot use import statement outside a module" 会被
// 模型读成「沙箱坏了」。只对真的写了静态 import 的源码加这句。
const staticImportRe = /(^|[\n;{])\s*import\b/;

const compileErrorText = (label, err, source) => {
  const name = (err && err.name) || 'SyntaxError';
  const msg = (err && err.message) || safeString(err);
  let text = `${name}: ${msg} (while compiling the ${label})\n` +
    'the script is evaluated as a function body, so `return` and top-level `await` work, ' +
    'but static `import` does not';
  if (staticImportRe.test(source)) {
    text += " — use `await import('node:fs')` (dynamic import) instead of a static import";
  }
  return text;
};

// fail 的文案归属：脚本错误保持 Phase 1-A 的原样（宿主把它印在 [script error] 下，
// 形态不变）；scaffold 错误加一句归属说明 —— 那是**宿主自己**的 bug，不该让模型
// 去怀疑自己的脚本。
const failureText = (kind, text) => (kind === 'scaffold'
  ? 'the host-injected scaffold failed before the script ran (the script was not executed):\n' + text
  : text);

// runBody 编译并求值一段「函数体形态」的 JS，返回它的返回值。
//
// 编译与运行分开 catch 是有意的：运行期脚本自己抛的 SyntaxError（例如 JSON.parse 失败）
// 不该被当成「编译失败」去加 import 提示 —— 两类错误的可行动建议完全不同。
const runBody = async (label, kind, param, source, file, arg) => {
  let fn;
  try {
    fn = new AsyncFunction(param, source + '\n//# sourceURL=' + file);
  } catch (err) {
    return fail(kind, failureText(kind, compileErrorText(label, err, source)));
  }
  try {
    return await fn(arg);
  } catch (err) {
    return fail(kind, failureText(kind, errorText(err)));
  }
};

// ---- 求值开始：先保活（见文件头），再求值 ----
//
// 顺序不能反：下面每一个 await 都会让本模块的顶层 await 悬空，而 node **不会**为
// 预加载模块的悬空顶层 await 保命（实测：直接以 0 退出）。先 ref 住 stdin，进程才
// 一定活到脚本收尾；脚本收尾后再 syncKeepAlive() 交还给「有没有未决调用」这一个条件。
scriptRunning = true;
syncKeepAlive();

lineOffset = await measureLineOffset();

// scaffold（宿主注入，可选）：在用户脚本**之前**求值，签名 (ctx)，
// ctx = { init, hostCall }。宿主用它装自己的全局（Wave 2：text/image/store/…）。
//
// 契约（宿主 execproc.ScriptOpts.Scaffold 的注释里有同一份）：
//   - 本片段是**函数体**（不是函数表达式），入参 ctx；
//   - 它要用 `globalThis.<name> = …` 定义全局 —— 函数体里 `const`/`var` 都是局部的，
//     用户脚本看不到；用户脚本与它是同一个 realm，globalThis 上的键彼此可见；
//   - 报错即收尾：scaffold 装不上全局时脚本不该硬跑（那会报一堆 "x is not defined"，
//     把宿主的一个 bug 伪装成模型写错脚本）。
if (scaffoldSource !== '') {
  await runBody('scaffold', 'scaffold', 'ctx', scaffoldSource, SCAFFOLD_FILE,
    { init, hostCall: callTool });
}

// 用户脚本。入参 tools（现有 registry）—— 与 Phase 1-A 一样，`tools.<name>(args)`
// 是走出去的受支持方式；返回值（return 值）即本次结果（与 process.exitCode 无关：
// 「脚本返回了什么」与「进程怎么结束的」是两件事，各报各的）。
const returned = await runBody('script', 'script', 'tools', scriptSource, SCRIPT_FILE, globalThis.tools);
setResult(returned);

// 脚本收尾：交还保活（若脚本还留着在途调用，由 pending 那一半继续撑着）。
scriptRunning = false;
syncKeepAlive();

// 本模块的文件名（堆栈里显示的名字）。不写这一行，本模块的每一帧都会以
// `data:text/javascript;base64,<整份源码的 base64>` 出现在报错文本里 —— 那是要进
// 模型上下文的东西，一条报错能塞进上万字节的 base64（实测：修复前的 callTool 帧）。
// 与 prelude.js 的分工：prelude 是协议端（帧形状的唯一事实源），它那侧的帧名保持
// 原样（Phase 1 的 Execute 路径同样受益于本行不存在，行为零变化）。
//# sourceURL=codemode-bridge.mjs
