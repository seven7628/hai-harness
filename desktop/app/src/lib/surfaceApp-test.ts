// surfaceApp-test.ts —— URL 型插件地址拼接的纯函数单测。
//
// 为什么单测这一行：它决定 `WebContentsView.loadURL` 最终打到哪个 origin。安装期
// （bridge/surfaces.go）与索引读侧（electron/surface-index.ts）已经各拒了一次绝对 URL /
// 协议相对路径，这里是第三道；任何一道单独失效都不该让 `//evil.com` 变成外站。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { appUrlOf } from './surfaceApp.ts'

test('根地址 + 空 path = 带尾斜杠的根', () => {
  assert.equal(appUrlOf('http://127.0.0.1:5173', ''), 'http://127.0.0.1:5173/')
  assert.equal(appUrlOf('http://127.0.0.1:5173/', '/'), 'http://127.0.0.1:5173/')
})

test('path 的头斜杠被去掉，base 的尾斜杠被去掉（不产生双斜杠 / 不换 origin）', () => {
  assert.equal(appUrlOf('http://127.0.0.1:5173/', '/admin'), 'http://127.0.0.1:5173/admin')
  assert.equal(appUrlOf('http://127.0.0.1:5173', 'admin'), 'http://127.0.0.1:5173/admin')
  assert.equal(appUrlOf('http://host/app/', '/admin'), 'http://host/app/admin')
})

test('hash 路由 / 查询串原样保留', () => {
  assert.equal(appUrlOf('http://host', '#/detail?id=1'), 'http://host/#/detail?id=1')
  assert.equal(appUrlOf('http://host/', '?x=1'), 'http://host/?x=1')
})

test('协议相对路径不会被拼成外站（先去掉前导斜杠）', () => {
  assert.equal(appUrlOf('http://host', '//evil.com/x'), 'http://host/evil.com/x')
})
