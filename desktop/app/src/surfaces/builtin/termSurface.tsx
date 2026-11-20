// [recovered] from /private/tmp/builtin-surfaces.test.cjs
// 编译产物（esbuild `// src/...` 模块切片），依赖由 bundle 注入，未回译。
// @ts-nocheck
/* eslint-disable */
var import_jsx_runtime32 = __toESM(require_jsx_runtime());
var termSurface = {
  kind: "term",
  namespace: "builtin",
  group: "workflow",
  // 排在 git(20) 之前：终端是「随手要开」的，越靠前越省一次点击
  order: 15,
  singleton: true,
  // 满幅：终端要正片区域（不要 right-body 的 10px 内边距与滚动条），
  // 否则 xterm 的列宽/行数会围着内边距算，右侧总留一条空带（§6.4）。
  fullBleed: true,
  scope: "session",
  weight: "heavy",
  capabilities: ["write"],
  title: (_tab, t4) => t4("rp.tab.term"),
  icon: () => /* @__PURE__ */ (0, import_jsx_runtime32.jsx)(TermIcon, {}),
  defaultSpec: () => ({ kind: "term" }),
  render: (ctx) => /* @__PURE__ */ (0, import_jsx_runtime32.jsx)(TerminalTab, { tabId: ctx.tab.id })
};
registerSurface(termSurface);

// <stdin>
init_useAppStore();
// Annotate the CommonJS export names for ESM import in node:
0 && (module.exports = {
  BUILTIN_PROVIDER_IDS,
  BUILTIN_SLASH,
  COMPRESS_THRESHOLD_DEFAULT,
  COMPRESS_THRESHOLD_MAX,
  COMPRESS_THRESHOLD_MIN,
  DEFAULT_SETTINGS,
  FILE_PREVIEWS_MAX,
  FIRST_CHUNK_TIMEOUT_DEFAULT,
  FIRST_CHUNK_TIMEOUT_MAX,
  FIRST_CHUNK_TIMEOUT_MIN,
  KNOWN_MODEL_INPUT_TYPES,
  KNOWN_MODEL_MAX_TOKENS,
  KNOWN_MODEL_WINDOWS,
  LS_SESSION_TABS,
  MAX_SESSION_PINS,
  MAX_WORKSPACE_PINS,
  MODEL_MAX_TOKENS_DEFAULT,
  MODEL_WINDOW,
  MODEL_WINDOW_1M,
  QUEUE_CAP,
  SUBAGENT_CONCURRENCY_DEFAULT,
  TABS_MAX_VISIBLE,
  TABS_PERSIST_MAX,
  TABS_PERSIST_MAX_SCOPES,
  TAB_CLOSED_MAX,
  TRACE_EVENTS_MAX,
  __clearNavDiscards,
  __clearRunSettles,
  __navDiscards,
  __runSettles,
  __traceSpy,
  activePageIdOf,
  agentQueueKey,
  applySessionPrefs,
  approvalOwnerIsSub,
  autoOpenVisualFile,
  autoOpenVisualState,
  countRunningAgents,
  defaultModelFor,
  describeEmbedAction,
  dispatchEvent,
  fileKeyOf,
  focusNodeIdFor,
  focusSpec,
  genMsOf,
  hydrateCheckpoint,
  inputTypesForProvider,
  isHubSessionId,
  isPendingWebTab,
  isSubAgentRun,
  localDayKey,
  mainRunActive,
  mainRunEnded,
  materializeSnapshot,
  messageImageBytes,
  metricsWindowFrom,
  modelInputTypes,
  modelMaxTokensOf,
  modelSupportsImage,
  modelWindowOf,
  putFilePreview,
  queuedHasAttach,
  queuedText,
  rebuildWebTab,
  runningTaskCount,
  samePreviewTarget,
  selectRightTab,
  sessionBusy,
  skeletonFinishExit,
  titleKey,
  useAppStore,
  webTabKey
});
/*! Bundled license information:

react/cjs/react.production.min.js:
  (**
   * @license React
   * react.production.min.js
   *
   * Copyright (c) Facebook, Inc. and its affiliates.
   *
   * This source code is licensed under the MIT license found in the
   * LICENSE file in the root directory of this source tree.
   *)

react/cjs/react.development.js:
  (**
   * @license React
   * react.development.js
   *
   * Copyright (c) Facebook, Inc. and its affiliates.
   *
   * This source code is licensed under the MIT license found in the
   * LICENSE file in the root directory of this source tree.
   *)

react/cjs/react-jsx-runtime.production.min.js:
  (**
   * @license React
   * react-jsx-runtime.production.min.js
   *
   * Copyright (c) Facebook, Inc. and its affiliates.
   *
   * This source code is licensed under the MIT license found in the
   * LICENSE file in the root directory of this source tree.
   *)

react/cjs/react-jsx-runtime.development.js:
  (**
   * @license React
   * react-jsx-runtime.development.js
   *
   * Copyright (c) Facebook, Inc. and its affiliates.
   *
   * This source code is licensed under the MIT license found in the
   * LICENSE file in the root directory of this source tree.
   *)

dompurify/dist/purify.es.mjs:
  (*! @license DOMPurify 3.4.14 | (c) Cure53 and other contributors | Released under the Apache license 2.0 and Mozilla Public License 2.0 | github.com/cure53/DOMPurify/blob/3.4.14/LICENSE *)

lodash-es/lodash.js:
  (**
   * @license
   * Lodash (Custom Build) <https://lodash.com/>
   * Build: `lodash modularize exports="es" --repo lodash/lodash#4.18.1 -o ./`
   * Copyright OpenJS Foundation and other contributors <https://openjsf.org/>
   * Released under MIT license <https://lodash.com/license>
   * Based on Underscore.js 1.8.3 <http://underscorejs.org/LICENSE>
   * Copyright Jeremy Ashkenas, DocumentCloud and Investigative Reporters & Editors
   *)

cytoscape/dist/cytoscape.esm.mjs:
  (*!
  Embeddable Minimum Strictly-Compliant Promises/A+ 1.1.1 Thenable
  Copyright (c) 2013-2014 Ralf S. Engelschall (http://engelschall.com)
  Licensed under The MIT License (http://opensource.org/licenses/MIT)
  *)
  (*!
  Event object based on jQuery events, MIT license
  
  https://jquery.org/license/
  https://tldrlegal.com/license/mit-license
  https://github.com/jquery/jquery/blob/master/src/event.js
  *)
  (*! Bezier curve function generator. Copyright Gaetan Renaudeau. MIT License: http://en.wikipedia.org/wiki/MIT_License *)
  (*! Runge-Kutta spring physics function generator. Adapted from Framer.js, copyright Koen Bok. MIT License: http://en.wikipedia.org/wiki/MIT_License *)

@mermaid-js/parser/dist/chunks/mermaid-parser.core/chunk-FOHPRMQF.mjs:
  (*! Bundled license information:
  
  lodash-es/lodash.js:
    (**
     * @license
     * Lodash (Custom Build) <https://lodash.com/>
     * Build: `lodash modularize exports="es" -o ./`
     * Copyright OpenJS Foundation and other contributors <https://openjsf.org/>
     * Released under MIT license <https://lodash.com/license>
     * Based on Underscore.js 1.8.3 <http://underscorejs.org/LICENSE>
     * Copyright Jeremy Ashkenas, DocumentCloud and Investigative Reporters & Editors
     *)
  *)

mermaid/dist/mermaid.core.mjs:
  (*! Check if previously processed *)
  (*!
   * Wait for document loaded before starting the execution
   *)
*/
