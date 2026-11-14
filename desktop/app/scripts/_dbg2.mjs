import { build } from 'esbuild'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
const require = createRequire(import.meta.url)
const mem = new Map()
global.localStorage = { getItem: (k) => (mem.has(k) ? mem.get(k) : null), setItem: (k, v) => mem.set(k, String(v)), removeItem: (k) => mem.delete(k) }
global.document = { documentElement: { classList: { add() {}, remove() {} } } }
const dir = mkdtempSync(join(tmpdir(), 'dbg2-'))
const outfile = join(dir, 'store.cjs')
await build({ entryPoints: ['src/store/useAppStore.ts'], bundle: true, format: 'cjs', platform: 'node', outfile, logLevel: 'silent', external: ['electron'], loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' },
  plugins: [{ name: 'a', setup(b) { b.onResolve({ filter: /\?(raw|url|no-inline)$/ }, () => ({ path: 'asset', namespace: 'asset-stub' })); b.onLoad({ filter: /.*/, namespace: 'asset-stub' }, () => ({ contents: 'module.exports = ""', loader: 'js' })) } }] })
const { useAppStore, __traceSpy } = require(outfile)
const sent = []
__traceSpy.sent = sent
mem.set('mock.pluginProjects', JSON.stringify([{ path: '/u/.go-code/plugin-sessions/alpha/home', plugin_id: 'alpha', name: 'A', sessions: [{ id: 'ps-1', title: 't' }] }]))
useAppStore.setState({ workspace: '/real', activeSessionId: 'r1', bridgeStatus: 'running', projects: [{ path: '/real', sessions: [{ id: 'r1', title: 'r' }] }], sessions: [{ id: 'r1', title: 'r' }], views: {} })
useAppStore.getState().loadPluginProjects()
await new Promise((r) => setTimeout(r, 100))
console.log('sent:', JSON.stringify(sent.map((c) => c.type)))
console.log('projects:', JSON.stringify(useAppStore.getState().projects))
process.exit(0)
