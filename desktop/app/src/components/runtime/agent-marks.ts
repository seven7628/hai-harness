/* ten subagent marks, three states — ported verbatim from
   design/agent-runtime/v2/parts/marks-agent.js. One body, one pair of eyes,
   ten tools: the family reads as a family. */

export type AgentKind = 'scout' | 'foreman' | 'fork' | 'gleaner' | 'courier'
  | 'lifter' | 'packer' | 'rover' | 'digger' | 'gantry'
export type AgentMarkState = 'run' | 'stop' | 'done'

  const P = (d: string) => '<path d="' + d + '"/>';
  const R = (x: number, y: number, w: number, h: number, r: number) => '<rect x="' + x + '" y="' + y + '" width="' + w + '" height="' + h + '" rx="' + r + '"/>';
  const C = (cx: number, cy: number, r: number) => '<circle cx="' + cx + '" cy="' + cy + '" r="' + r + '"/>';
  /* a feature that is a stroke rather than a fill: the arch is the only one,
     and climbing it with arcs would cost more than a 1.35 wide line */
  const S = (d: string, w: number) => '<path d="' + d + '" fill="none" stroke="#fff" stroke-width="' + w + '" stroke-linecap="round"/>';
  const T = (g: string, deg: number, cx: number, cy: number) => '<g transform="rotate(' + deg + ' ' + cx + ' ' + cy + ')">' + g + '</g>';

  /* The body: flat top rail, round heavy base. Flat on top so a tool can be
     mounted on it without floating, round at the bottom so it reads as weight
     the worker is standing under — and so it is never mistaken for the squircle
     face the rest of the page uses. */
  const BODY = 'M4.7 5H11.3A1.1 1.1 0 0 1 12.4 6.1V9.4A3.8 3.8 0 0 1 8.6 13.2H7.4A3.8 3.8 0 0 1 3.6 9.4V6.1A1.1 1.1 0 0 1 4.7 5Z';

  /* One eye pair for all ten, because the eyes are the family, not the mark.
     The lid sits one full pixel lower than the open slot's centre at 14px (the
     size that matters): a closed eye rests, and a full-pixel row is the only way
     that row's darkness survives 0.875 units per pixel. A lid on a half pixel
     came out as a smudge, and the wink -- the whole point of `stop` -- with it. */
  const OPEN = R(5.8, 6.8, 1.55, 2.5, 0.78) + R(8.65, 6.8, 1.55, 2.5, 0.78);
  const SHUT = R(5.7, 8, 1.8, 1.15, 0.55) + R(8.5, 8, 1.8, 1.15, 0.55);
  const WINK = R(5.7, 8, 1.8, 1.15, 0.55) + R(8.65, 6.8, 1.55, 2.5, 0.78);

  /* ---------- the ten ----------------------------------------------------- */
  const KINDS = [
    {
      id: 'scout', letter: 'A', name: 'Scout', zh: '探路',
      build: 'a mast with a ball on it, above the top rail',
      add: R(7.32, 2.4, 1.36, 3.7, 0.68) + C(8, 1.7, 1.6),
    },
    {
      id: 'foreman', letter: 'B', name: 'Foreman', zh: '领工',
      build: 'a flat brim and a low crown, across the top',
      add: R(6.35, 1.55, 3.3, 3.1, 1) + R(3.15, 4.05, 9.7, 1.4, 0.7),
    },
    {
      id: 'fork', letter: 'C', name: 'Fork', zh: '分叉',
      build: 'two prongs splayed out of the top rail',
      add: T(R(5.25, 1.75, 1.5, 4.8, 0.75), -15, 6.1, 6.5) + T(R(9.25, 1.75, 1.5, 4.8, 0.75), 15, 9.9, 6.5),
    },
    {
      id: 'gleaner', letter: 'D', name: 'Gleaner', zh: '拾取',
      build: 'an arch handle over the top, open underneath',
      add: S('M4.5 6.4C4.5 2.8 5.95 1.4 8 1.4C10.05 1.4 11.5 2.8 11.5 6.4', 1.45),
    },
    {
      id: 'courier', letter: 'E', name: 'Courier', zh: '差遣',
      build: 'a long pennant off the right shoulder, past the body',
      add: P('M11.05 5.7 15.9 7.9 11.05 10.1Z'),
    },
    {
      id: 'lifter', letter: 'F', name: 'Lifter', zh: '起吊',
      build: 'a crane hook off the left shoulder, curling down',
      add: R(0.35, 5.6, 3.9, 1.6, 0.8) + R(0.35, 5.6, 1.7, 5.3, 0.8) + R(0.35, 9.3, 3.95, 1.6, 0.8),
    },
    {
      id: 'packer', letter: 'G', name: 'Packer', zh: '载货',
      build: 'a crate on the right hip, breaking the outline',
      add: R(9.7, 8.3, 5.4, 5.2, 1.1),
    },
    {
      id: 'rover', letter: 'H', name: 'Rover', zh: '巡行',
      build: 'a wheel under the base, drilled through',
      add: C(8, 13.1, 3.05),
      cut: C(8, 13.1, 1.45),
    },
    {
      id: 'digger', letter: 'I', name: 'Digger', zh: '钻探',
      build: 'a peg tapering out of the bottom edge',
      add: P('M5.5 9.9 8 15.5 10.5 9.9Z'),
    },
    {
      id: 'gantry', letter: 'J', name: 'Gantry', zh: '承台',
      build: 'a flat skid under the round base, air in the corners',
      add: R(3.9, 13, 8.2, 2.05, 1),
    },
  ];

  const BY_ID = KINDS.reduce((m: Record<string, (typeof KINDS)[number]>, k) => { m[k.id] = k; return m; }, {});

export const AGENT_KINDS = KINDS.map((k) => ({ id: k.id, letter: k.letter, name: k.name, zh: k.zh, build: k.build }))

const STATE: Record<string, string> = { run: OPEN, stop: WINK, done: SHUT }
const ALIAS: Record<string, string> = {
  run: 'run', running: 'run', live: 'run', work: 'run',
  stop: 'stop', stopped: 'stop', interrupted: 'stop', interrupt: 'stop', held: 'stop',
  done: 'done', ok: 'done', finished: 'done', settled: 'done', end: 'done',
}

let seq = 0
/* a machine-unique mask id: two marks in one tree must not share a mask, and
   React re-renders must not either */
export function agentMarkHTML(kind: string, state: string, opts?: { size?: number }): string {
  const k = (BY_ID as Record<string, { id: string; add?: string; cut?: string }>)[kind] || KINDS[0]
  const s = ALIAS[state] || 'run'
  const id = 'sa-mk' + (++seq)
  const size = opts && opts.size ? ` style="--sa-size:${opts.size}px"` : ''
  return '<svg class="rt-sa" viewBox="0 0 16 16"'
    + ` data-sa-kind="${k.id}" data-sa-state="${s}"${size} aria-hidden="true" focusable="false">`
    + `<defs><mask id="${id}" maskUnits="userSpaceOnUse" x="0" y="0" width="16" height="16">`
    + `<g fill="#fff"><g class="sa-body">${P(BODY)}</g><g class="sa-feat">${k.add || ''}</g></g>`
    + `<g fill="#000">${k.cut || ''}<g class="sa-blink">${STATE[s]}</g></g>`
    + '</mask></defs>'
    + `<g class="sa-pop"><rect x="0" y="0" width="16" height="16" fill="currentColor" mask="url(#${id})"/></g>`
    + '</svg>'
}
