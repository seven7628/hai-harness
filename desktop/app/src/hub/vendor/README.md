# Luna avatar vendor

原文件：

- `hai-super-assistant/docs/demo/luna-avatar-preview/mascot-bots.js`
- `hai-super-assistant/docs/demo/luna-avatar-preview/luna-face.js`

逐字复制，唯一改动是 `mascot-bots.js` 末尾在 CommonJS 导出之外**始终**写
`globalThis.LUNA_BOTS`，保证 Vite/Rollup 打包后 `luna-face.js` 仍能先拿到几何表。
头像的颜色/表情/相位由 `../lunaAvatar.ts` 按 demo 的规则接入 Hub 数据；
几何与动效不在 React 里重画。
