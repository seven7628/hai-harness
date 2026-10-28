/* three faces, four states — ported from the v2 design canvas
   (design/agent-runtime/v2/ui.js, "the mark"). Kept as a string builder rather
   than JSX so the drawing is byte-identical to the one that was reviewed. */

export type MarkSystem = 'pip' | 'cat' | 'ghost'
export type MarkState = 'run' | 'work' | 'done' | 'engaged'
interface Eyes { open: string; wide: string; shut: string }

const MK_SEQ = { n: 0 };
const P = (d: string) => '<path d="' + d + '"/>';
const S = (d: string) => '<path d="' + d + '" fill="none" stroke="#fff" stroke-width="1.1" stroke-linecap="round"/>';
const O = (x: number, y: number, w: number, h: number, r: number) => '<rect x="' + x + '" y="' + y + '" width="' + w + '" height="' + h + '" rx="' + r + '"/>';
const C = (cx: number, cy: number, r: number, paint?: string) => '<circle cx="' + cx + '" cy="' + cy + '" r="' + r + '"' + (paint ? ' fill="' + paint + '"' : '') + '/>';

/* every creature is the same three-layer trick; only the drawing changes */
const creature = (kind: string, sil: string, eyes: Eyes, mouth: string, extras?: (st: string) => string) =>
  (st: string): string => {
  const id = 'mk' + (++MK_SEQ.n);
  const eye = st === 'done' ? eyes.shut : st === 'engaged' ? eyes.wide : eyes.open;
  const smile = st === 'engaged' ? mouth : '';
  return '<svg class="rt-mk ' + kind + '" viewBox="0 0 16 16" aria-hidden="true" data-mk-state="' + st + '">' +
    '<defs><mask id="' + id + '" maskUnits="userSpaceOnUse" x="0" y="0" width="16" height="16">' +
      '<g fill="#fff">' + sil + '</g>' +
      '<g fill="#000"><g class="mk-look"><g class="mk-wide"><g class="mk-blink">' + eye + '</g></g></g>' +
        smile + '</g>' +
    '</mask></defs>' +
    '<circle class="mk-halo" cx="8" cy="8" r="7.6"/>' +
    '<g class="mk-pop"><g class="mk-body">' +
      '<rect x="0" y="0" width="16" height="16" fill="currentColor" mask="url(#' + id + ')"/>' +
    '</g>' + (extras ? extras(st) : '') + '</g>' +
  '</svg>';
};

const MARK_SYSTEMS = {
  /* A · Pip — a squircle with two dot eyes. The plainest face of the six, and the
     one that stays legible at 11px in a dense row. */
  pip: creature('mk-pip',
    P('M6.2 2h3.6c2.6 0 4.2 1.6 4.2 4.2v3.6c0 2.6-1.6 4.2-4.2 4.2H6.2C3.6 14 2 12.4 2 9.8V6.2C2 3.6 3.6 2 6.2 2Z'),
    { open: O(4.9, 5.9, 1.8, 2.5, 0.9) + O(9.3, 5.9, 1.8, 2.5, 0.9),
      wide: C(5.9, 7.2, 1.2) + C(10.1, 7.2, 1.2),
      shut: O(4.8, 6.7, 1.9, 0.9, 0.45) + O(9.3, 6.7, 1.9, 0.9, 0.45) },
    O(6.9, 10.1, 2.2, 0.9, 0.45)),
  /* B · Cat — same face under two ears, slit eyes. The ears are what makes this
     one readable at 12px when the eyes are just two pixels. */
  cat: creature('mk-cat',
    P('M8 3a5.6 5.6 0 1 1 0 11.2A5.6 5.6 0 0 1 8 3Z') + P('M3.7 5.7 2.4 1.7 6 3.2Z') + P('M12.3 5.7 13.6 1.7 10 3.2Z'),
    { open: O(5.3, 5.9, 1.4, 2.6, 0.7) + O(9.3, 5.9, 1.4, 2.6, 0.7),
      wide: C(6.1, 7.2, 1.15) + C(9.9, 7.2, 1.15),
      shut: O(5.2, 6.7, 1.6, 0.85, 0.42) + O(9.2, 6.7, 1.6, 0.85, 0.42) },
    O(7.1, 9.9, 1.8, 0.85, 0.42)),
  /* C · Ghost — a wavy hem and an o-mouth. The only one whose silhouette is not
     closed by a curve at the bottom. */
  ghost: creature('mk-ghost',
    P('M2.6 8.3C2.6 4.9 5 2.4 8 2.4C11 2.4 13.4 4.9 13.4 8.3V13.3C12.4 12.4 11.5 14 10.4 13.3' +
      'C9.4 12.6 8.7 14 7.6 13.3C6.6 12.6 5.6 14 4.6 13.3C3.7 12.6 2.6 13.3 2.6 13.3Z'),
    { open: C(6.2, 7, 1) + C(9.8, 7, 1),
      wide: C(6.2, 7, 1.25) + C(9.8, 7, 1.25),
      shut: O(5.3, 6.5, 1.7, 0.9, 0.45) + O(9, 6.5, 1.7, 0.9, 0.45) },
    O(7.2, 9.6, 1.6, 1.9, 0.8)),
};

export function faceHTML(system: MarkSystem | string, state: MarkState | string): string {
  const draw = (MARK_SYSTEMS as Record<string, (st: string) => string>)[system]
    || (MARK_SYSTEMS as Record<string, (st: string) => string>).pip
  const st = state === 'work' ? 'run' : (state || 'done')
  let svg = draw(st)
  if (state === 'work') svg = svg.replace('class="rt-mk ', 'class="rt-mk mk-work ')
  return svg
}
