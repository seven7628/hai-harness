#!/usr/bin/env node
// Static preflight for a surface plugin directory.
// This is deliberately *not* a second installer: the real installer remains the authority for
// hashes, file modes and index writes. This catches the mistakes a model makes before installing.
import fs from 'node:fs'
import path from 'node:path'

const dir = path.resolve(process.argv[2] || '')
const errors = []
const warnings = []
const fail = (m) => errors.push(m)
const warn = (m) => warnings.push(m)
// Keep in sync with panel-api.md / the host theme. A builder copying the template must not see a
// token that the host never provides.
const TOKENS = new Set(['bg', 'fg', 'border', 'accent', 'ok', 'warn', 'err', 'info'])

function read(rel) {
  try { return fs.readFileSync(path.join(dir, rel), 'utf8') } catch { return null }
}

if (!fs.existsSync(dir)) {
  console.error(`plugin dir does not exist: ${dir}`)
  process.exit(1)
}
const raw = read('surface.json')
if (raw == null) {
  fail('surface.json is missing')
} else {
  let mf = null
  try { mf = JSON.parse(raw) } catch (e) { fail(`surface.json is not valid JSON: ${e.message}`) }
  if (mf && typeof mf === 'object') {
    const id = typeof mf.id === 'string' ? mf.id.trim() : ''
    if (!/^[a-z][a-z0-9-]{1,40}$/.test(id)) fail(`id must match ^[a-z][a-z0-9-]{1,40}$ (got ${JSON.stringify(id)})`)
    if (mf.kind !== `plugin:${id}`) fail(`kind must be exactly plugin:<id> (got ${JSON.stringify(mf.kind)})`)
    if (typeof mf.version !== 'string' || !mf.version.trim()) fail('version is required')
    if (mf.version && !/^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$/.test(mf.version)) {
      warn('version is not semver (use x.y.z for upgrades)')
    }
    if (mf.name != null) {
      if (typeof mf.name === 'string' || !mf.name || typeof mf.name !== 'object' || Array.isArray(mf.name)) {
        fail('name must be an object keyed by language, e.g. {"zh":"成本看板","en":"Cost board"} (a plain string is rejected by the real installer)')
      } else {
        for (const [lang, label] of Object.entries(mf.name)) {
          if (typeof label !== 'string' || !label.trim()) fail(`name.${lang} must be a non-empty string`)
        }
      }
    }
    const entry = typeof mf.entry === 'string' && mf.entry.trim() ? mf.entry : 'panel.js'
    if (!fs.existsSync(path.join(dir, entry))) fail(`entry "${entry}" does not exist`)
    if (mf.commands != null && !Array.isArray(mf.commands)) fail('commands must be an array')
    if (mf.data != null && !Array.isArray(mf.data)) fail('data must be an array')
    if (mf.placement != null) warn('placement is not part of the manifest contract; remove it')
    if (mf.api != null) warn('api is not consumed by the installer/loader; remove it')
    if (mf.entries != null) {
      if (!Array.isArray(mf.entries)) fail('entries must be an array')
      else {
        if (mf.entries.length > 20) fail('entries must be ≤ 20')
        const ids = new Set()
        for (const [i, e] of mf.entries.entries()) {
          if (!e || typeof e !== 'object' || Array.isArray(e)) { fail(`entries[${i}] must be an object`); continue }
          const eid = typeof e.id === 'string' ? e.id.trim() : ''
          if (!/^[a-z][a-z0-9-]{1,40}$/.test(eid)) fail(`entries[${i}].id must match ^[a-z][a-z0-9-]{1,40}$`)
          else if (ids.has(eid)) fail(`entries[${i}].id "${eid}" is duplicated`)
          else ids.add(eid)
          const title = e.title
          if (typeof title === 'string') {
            if (!title.trim() || [...title].length > 64) fail(`entries[${i}].title must be 1–64 characters`)
          } else if (title && typeof title === 'object' && !Array.isArray(title)) {
            const labels = Object.values(title).filter((v) => typeof v === 'string' && v.trim())
            if (!labels.length) fail(`entries[${i}].title object needs at least one non-empty language`)
            for (const v of Object.values(title)) if (typeof v === 'string' && [...v].length > 64) fail(`entries[${i}].title.${Object.keys(title).find((k) => title[k] === v)} is > 64 characters`)
          } else fail(`entries[${i}].title must be a string or a language object`)
        }
      }
    }
    if (mf.tools != null) {
      if (!Array.isArray(mf.tools)) fail('tools must be an array')
      else {
        if (mf.tools.length > 16) fail('tools must be ≤ 16 per plugin')
        const names = new Set()
        for (const [i, t] of mf.tools.entries()) {
          if (!t || typeof t !== 'object' || Array.isArray(t)) { fail(`tools[${i}] must be an object`); continue }
          const name = typeof t.name === 'string' ? t.name.trim() : ''
          if (!/^[a-z][a-z0-9_]{0,40}$/.test(name) || name.includes('__')) fail(`tools[${i}].name must match ^[a-z][a-z0-9_]{0,40}$ and contain no "__"`)
          else if (names.has(name)) fail(`tools[${i}].name "${name}" is duplicated`)
          else names.add(name)
          if (Buffer.byteLength(`plugin__${id}__${name}`) > 64) fail(`tools[${i}] full provider name plugin__${id}__${name} is > 64 bytes`)
          if (typeof t.description !== 'string' || !t.description.trim()) fail(`tools[${i}].description is required`)
          else if ([...t.description].length > 300) fail(`tools[${i}].description is > 300 characters (installer hard cap)`)
          if (!t.input || typeof t.input !== 'object' || Array.isArray(t.input)) fail(`tools[${i}].input must be a JSON object`)
          if (t.timeout_ms != null) {
            const ms = Number(t.timeout_ms)
            if (!Number.isFinite(ms) || (ms !== 0 && (ms < 100 || ms > 60000))) fail(`tools[${i}].timeout_ms must be 0 or 100–60000`)
          }
        }
      }
    }
    const banned = ['workspace_list', 'session_list', 'git_snapshot', 'ui_inspect']
    for (const c of Array.isArray(mf.commands) ? mf.commands : []) {
      if (banned.includes(c)) fail(`D11: ${c} cannot be declared as a plugin command (use the host tool / agent.* instead)`)
    }
    const commands = new Set(Array.isArray(mf.commands) ? mf.commands : [])
    if (Array.isArray(mf.tools) && mf.tools.length) {
      if (!Array.isArray(mf.data) || !mf.data.includes('tool.call')) fail('tools[] requires data to include "tool.call"')
      if (!commands.has('tool.result')) fail('tools[] requires commands to include "tool.result"')
    }
  }
}
const panel = read('panel.js')
if (panel == null) {
  fail('panel.js is missing')
} else {
  // Strip comments before syntax-ish checks: the template's own comments mention `import x`
  // and JSX as counter-examples, which must not be mistaken for executable code.
  const code = panel
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .replace(/(^|[^:])\/\/.*$/gm, '$1')
  if (!/export\s+const\s+surface\s*=/.test(code)) fail('panel.js must export `const surface = ...`')
  if (!/\bapply\s*\(/.test(code)) fail('surface.apply({ host, React, surfaces }) is required')
  if (/\bimport\s+[^('\"]/.test(code)) fail('panel.js must not use bare npm imports (no bundler in the panel)')
  if (!/createElement|\bh\s*\(/.test(code)) warn('panel.js does not look like it uses createElement/h(...) helpers')
  if (/<\/[A-Z]/.test(code)) fail('JSX-looking closing tag found; panel.js is not compiled')
  // The host theme has exactly eight tokens. A typo here is silent in JSX (undefined colour) and
  // has already shipped in a template, so catch it statically.
  for (const m of code.matchAll(/(?:theme|host\.theme\(\))\.tokens\.([A-Za-z_][A-Za-z0-9_]*)/g)) {
    if (!TOKENS.has(m[1])) fail(`unknown theme token "tokens.${m[1]}" (allowed: ${[...TOKENS].join(', ')})`)
  }
}
if (warnings.length) for (const w of warnings) console.warn('WARN:', w)
if (errors.length) {
  for (const e of errors) console.error('FAIL:', e)
  console.error(`\n${errors.length} error(s) in ${dir}`)
  process.exit(1)
}
console.log(`OK: ${dir} (static preflight; run install-smoke.sh for the real installer)`)
