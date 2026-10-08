package codemode

// protocol.go：宿主 ↔ scaffold 的**协议**（Wave 2）。
//
// 三个部分必须一起改（本文件是它们唯一的事实源）：
//
//	1. init 载荷（宿主 → 沙箱）：工具表 + store 快照 —— execproc.ScriptOpts.Init；
//	2. hostCall 保留名与消息体（scaffold → 宿主）：store / image / search / describe
//	   —— 这些能力在宿主侧实现（碰 store 文件、读图片、查目录），沙箱侧只是薄壳；
//	3. scaffold 源码（宿主注入的 JS 片段，execproc.ScriptOpts.Scaffold）：在用户脚本
//	   **之前**求值，把 text/image/store/load/searchTools/describeTool 装到 globalThis。
//
// 为什么不复用 execproc 的帧类型：那层的帧（hello/call/result/error）是**传输**协议，
// 对脚本不可见；本文件这些是**能力**协议，只有 hostCall 的名字与参数形状。两层之间有
// 一条窄腰：hostCall(name, args) 与 ScriptResult{Value, IsError}（见 execproc/script.go）。
//
// 与 prelude_bridge.js 的分工再明确一次（别在这里重复实现）：
//   - 工具表（tools.<name>）、call/reply 帧、并发、事件循环保活 = 传输层；
//   - exit(value) = 传输层（本文件不定义它，也不该覆盖它）；
//   - 本文件只装「不属于传输层」的全局，且**只认 init 里给的最终名**（名字归一化与
//     撞名消解是 descriptions.go 的事，两处各归一化一次会让撞名判定失真）。

import "encoding/json"

// hostCall 保留名。都带 `$codemode:` 前缀，与脚本可调用名**不可能**撞：归一化会把
// 冒号变成下划线（normalize），所以目录表里的名字永远不含 `:` —— 保留名在分派时先于
// 目录表判定是安全的（见 bridge.onCall）。
const (
	opStore    = "$codemode:store"
	opImage    = "$codemode:image"
	opSearch   = "$codemode:search"
	opDescribe = "$codemode:describe"
)

// reservedOp 判定一个 hostCall 名是否是保留操作。
func reservedOp(name string) (string, bool) {
	switch name {
	case opStore, opImage, opSearch, opDescribe:
		return name, true
	}
	return "", false
}

// ---- 1) init 载荷（宿主 → 沙箱）----

// initPayload 注入沙箱的初始化载荷。
//
// 形状与 prelude_bridge.js 的宽容解析器约定一致：`tools` 数组项可以是字符串（只有
// 归一化名）或对象；`name` = 脚本可调用名（已归一 + 撞名消解），`rawName` = 引擎内
// 原始名 —— 脚本用 `tools["mcp__dev-radius__get"](args)` 双键调用时走的就是它。
//
// **只有可编排档进这张表**（direct/codemode/deferred）：hidden（撤下）与 model-only
// （禁自嵌套，codemode 自己那一档）不进 —— 这就是「分派只认目录表」的物理载体。
type initPayload struct {
	Tools []initTool `json:"tools"`
	// Store 当前快照（load() 的初值）。脚本内 store()/load() 走本地视图 + 宿主 Pending，
	// 见 scaffold 的注释。
	Store map[string]json.RawMessage `json:"store,omitempty"`
	// StoreLimits 限额（由 Go 侧的常量填，沙箱侧只**读**）。
	//
	// 为什么要送过去：单值上限在沙箱侧先查一次，模型就越早（且可 catch）看到错误 ——
	// 而不是等脚本跑完才被判「写入没生效」。限额的**数值**只有一份真源（store.go 的常量），
	// 沙箱不自己写常数；宿主侧 Pending.Set 仍是权威（纵深）。
	StoreLimits *storeLimits `json:"store_limits,omitempty"`
}

// storeLimits store 的双限额（沙箱侧 store() 的快速失败用；数值来自 store.go）。
type storeLimits struct {
	ValueBytes int `json:"value_bytes"`
	TotalBytes int `json:"total_bytes"`
}

// initTool 工具表的一项（name = 脚本名，rawName = 引擎内 raw 名）。
type initTool struct {
	Name    string `json:"name"`
	RawName string `json:"rawName,omitempty"`
}

// ---- 2) hostCall 消息体 ----

// storeRequest 一次 store(key, value) / store(key, undefined)。
// Delete=true 时 Value 不参与判定（store(k, undefined) 的语义）。
type storeRequest struct {
	Key    string          `json:"key"`
	Value  json.RawMessage `json:"value,omitempty"`
	Delete bool            `json:"delete,omitempty"`
}

// storeReply 一次 store() 的回执（脚本可据此确认写入了多少字节）。
type storeReply struct {
	Key     string `json:"key"`
	Bytes   int    `json:"bytes"`
	Deleted bool   `json:"deleted,omitempty"`
}

// imageRequest 一次 image(path)。
type imageRequest struct {
	Path string `json:"path"`
}

// imageReply 一次 image() 的回执（宿主已把图片放进本次调用的 ImageSink）。
type imageReply struct {
	Path  string `json:"path"`
	Mime  string `json:"mime"`
	Bytes int    `json:"bytes"`
}

// searchRequest 一次 searchTools(query[, limit])。Limit<=0 = 用默认条数。
type searchRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

// searchReply searchTools 的回执。
type searchReply struct {
	Tools []searchHit `json:"tools"`
}

// searchHit 一条检索命中。
type searchHit struct {
	Name      string `json:"name"`
	Summary   string `json:"summary,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// describeRequest 一次 describeTool(name)。名字可以是脚本名或引擎内 raw 名。
type describeRequest struct {
	Name string `json:"name"`
}

// describeReply describeTool 的回执（含所属 namespace 的 instructions —— 长使用
// 指引刻意不进描述（预算只有 3000 token），要它就读这里）。
type describeReply struct {
	Name                  string      `json:"name"`
	ScriptName            string      `json:"script_name"`
	Description           string      `json:"description,omitempty"`
	Params                []paramView `json:"params,omitempty"`
	Namespace             string      `json:"namespace,omitempty"`
	NamespaceDescription  string      `json:"namespace_description,omitempty"`
	NamespaceInstructions string      `json:"namespace_instructions,omitempty"`
}

// paramView 参数签名的一项（脚本可读形状；descriptions.Param 是渲染用的内部形状）。
type paramView struct {
	Name     string `json:"name"`
	Optional bool   `json:"optional,omitempty"`
}

// ---- 3) scaffold 源码（宿主注入的 JS）----
//
// 契约（execproc.ScriptOpts.Scaffold 的注释里是同一份）：
//   - 本片段是**函数体**，入参 ctx = {init, hostCall}；
//   - 必须用 `globalThis.<name> = …` 定义全局才对用户脚本可见；
//   - 报错即收尾（用户脚本不执行）—— 装不上全局就硬跑，只会把宿主的 bug 伪装成
//     「模型写了不存在的函数」；
//   - 与用户脚本同 realm、永不落盘（随 init 走 stdin）。
//
// 刻意**不**做的事：不覆盖 exit（传输层的）、不重建 tools（传输层的 Proxy），
// 不在这里做名字归一化（见文件头）。
const scaffoldSource = `
const { hostCall, init } = ctx;

const OP_STORE = '$codemode:store';
const OP_IMAGE = '$codemode:image';
const OP_SEARCH = '$codemode:search';
const OP_DESCRIBE = '$codemode:describe';

// 输出：与 console.* 汇进同一条通道（prelude 替换了 process.stdout.write，内容随
// result 帧的 out 字段回传），于是 text() 与 console.log 在宿主侧是同一段文本。
globalThis.text = (value) => {
  let s;
  if (typeof value === 'string') {
    s = value;
  } else {
    try {
      s = JSON.stringify(value, null, 2);
    } catch (e) {
      s = String(value);
    }
  }
  if (s === undefined) s = String(value);
  process.stdout.write(s + '\n');
};

// 图片：读文件与格式校验都在宿主（沙箱只递路径）。非法图片必须**在这里**就失败 ——
// 一个坏块写进对话历史会让之后每一个请求失败（设计文档 §20.3 / pi #10215）。
globalThis.image = async (path) => {
  if (typeof path !== 'string' || path === '') {
    throw new Error('image(path): path must be a non-empty string');
  }
  return await hostCall(OP_IMAGE, { path: path });
};

// store / load：脚本内维护一份**本地视图**（同步可读），宿主是权威（写入先进 Pending，
// 脚本成功收尾才 Commit 落盘）。为什么给本地视图：教学正文承诺的形态就是
// store(k, v) / load(k) 的同步调用；若每次 load 都要往返宿主，模型必须记住 await 才能
// 读到值 —— 那是纯陷阱（而且读自己刚写的值本来就是同步语义）。
const storeValues = Object.create(null);
if (init && typeof init.store === 'object' && init.store !== null) {
  for (const k of Object.keys(init.store)) storeValues[k] = init.store[k];
}
const clone = (v) => (v === undefined ? undefined : JSON.parse(JSON.stringify(v)));

globalThis.load = (key) => {
  if (key === undefined) {
    const all = {};
    for (const k of Object.keys(storeValues)) all[k] = clone(storeValues[k]);
    return all;
  }
  if (typeof key !== 'string' || key === '') {
    throw new Error('load(key): key must be a non-empty string');
  }
  return clone(Object.prototype.hasOwnProperty.call(storeValues, key) ? storeValues[key] : undefined);
};

globalThis.store = async (key, value) => {
  if (typeof key !== 'string' || key === '') {
    throw new Error('store(key, value): key must be a non-empty string');
  }
  const limits = (init && init.store_limits) || {};
  if (value === undefined) {
    delete storeValues[key];
    return await hostCall(OP_STORE, { key: key, delete: true });
  }
  let raw;
  try {
    raw = JSON.stringify(value);
  } catch (e) {
    throw new Error('store(' + JSON.stringify(key) + '): value is not JSON-serializable (' + e.message + ')');
  }
  if (raw === undefined) {
    throw new Error('store(' + JSON.stringify(key) + '): value is not JSON-serializable ' +
      '(functions and symbols have no JSON form)');
  }
  // 单值上限**先在这里**查（限额数值来自宿主，沙箱不自己写常数）：同步抛出 ⇒ 脚本能当场
  // catch 并修正，而不是等脚本跑完才被判「写入没生效」。宿主侧仍是权威（纵深）。
  const size = Buffer.byteLength(raw, 'utf8');
  if (limits.value_bytes && size > limits.value_bytes) {
    throw new Error('store(' + JSON.stringify(key) + '): value is ' + size + ' bytes, over the ' +
      limits.value_bytes + '-byte (256 KiB) limit for one value - store a path or a summary instead');
  }
  storeValues[key] = JSON.parse(raw); // 本地视图立刻生效：同一段脚本里的 load(key) 必须看得见
  return await hostCall(OP_STORE, { key: key, value: JSON.parse(raw) });
};

// searchTools / describeTool：检索与描述都在宿主侧（目录与 namespace 的 instructions
// 都在那里）。宿主侧实现的价值是「换检索算法不动沙箱」—— BM25 排在 P2-b。
globalThis.searchTools = async (query, limit) => {
  if (typeof query !== 'string') {
    throw new Error('searchTools(query): query must be a string');
  }
  const res = await hostCall(OP_SEARCH, { query: query, limit: typeof limit === 'number' ? limit : 0 });
  return res && Array.isArray(res.tools) ? res.tools : [];
};

globalThis.describeTool = async (name) => {
  if (typeof name !== 'string' || name === '') {
    throw new Error('describeTool(name): name must be a non-empty string');
  }
  return await hostCall(OP_DESCRIBE, { name: name });
};
`
