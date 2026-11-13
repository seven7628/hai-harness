/* luna-face.js — Luna 头像动效引擎（单一实现）
   ------------------------------------------------------------------
   avatar.html（大脸预览）与 docs/demo/index.html（产品里的 17 个小头像）
   共用这一份。每颗头像由三部分组成，缺一不可：

     1. 鼠标互动  —— 视线跟指针（引擎 gaze：18°/14°，×0.55 hold，球心 0.03/0.024R）
     2. 9 种风格  —— neutral / happy / surprised / sad / sleepy / squinted /
                     calm / inspecting / confused，300ms 形状插值切换
     3. 6 种动作  —— blink / look / nod / sway / bounce / spin，各自独立时间线

   运行态（living）：每颗头每隔 3.2–7s 随机换一个风格，换风格时顺带做 1 个动作，
   做完继续保持，下一轮再换另一个风格——如此往复，永不同步（每颗头有相位偏移）。

   几何单源仍是同目录 mascot-bots.js（LUNA_BOTS）：头 r=100、两眼 ±0.32R、
   9 种眼形。本文件负责把它投影到球面，并按引擎时间轴驱动。

   用法：
     const scene = LUNA_FACE.createScene({ aim: document.body, living: true });
     scene.sync(document);   // 把所有 <svg data-luna-face> 挂进来（重渲染后再调）
     scene.start();

   svg 上的数据属性：
     data-expr="happy"       基准表情（状态表情，9 选 1）
     data-live="off"         不参与随机换风格（只跑 idle + 视线）
     data-fill="#3C82F6"     头的填充色（缺省 --face-body）
     data-eye="var(--bg)"    眼睛洞色（缺省 --face-cut）
     data-phase="1.7"        动画相位偏移（秒），避免所有头像同帧眨眼
     data-samples="48"       眼轮廓采样点（小头像 48 足够）
     data-gaze="off"         不跟指针
*/
(function (global) {
  'use strict';
  var API = global.LUNA_BOTS;
  if (!API) throw new Error('luna-face.js 需要先加载 mascot-bots.js');
  var AV = API.AVATAR;
  var D2R = Math.PI / 180;
  var FACE0 = { yaw: 0, pitch: 0, roll: 0, ox: 0, oy: 0 };

  /* ==================== 3D 眼贴片 ==================== */
  var FRAMES = (function () {
    var r = AV.r;
    return API.build.eyeBoxes().map(function (e) {
      var dx = e.x - AV.cx, dy = e.y - AV.cy;
      var z = Math.sqrt(Math.max(1, r * r - dx * dx - dy * dy));
      var n = [dx / r, dy / r, z / r];
      function tangent(tx, ty) {
        var k = tx * n[0] + ty * n[1];
        var t = [tx - k * n[0], ty - k * n[1], -k * n[2]];
        var len = Math.hypot(t[0], t[1], t[2]) || 1;
        return [t[0] / len, t[1] / len, t[2] / len];
      }
      return { side: e.side, w: e.w, h: e.h, C: [dx, dy, z], u: tangent(0, -1), v: tangent(1, 0) };
    });
  })();

  // 与引擎同序：Rz(roll)·Ry(yaw)·Rx(pitch)
  function turn3(v, pose) {
    var p = (pose.pitch || 0) * D2R, y = (pose.yaw || 0) * D2R, r = (pose.roll || 0) * D2R;
    var c = Math.cos(p), s = Math.sin(p);
    var x1 = v[0], y1 = v[1] * c - v[2] * s, z1 = v[1] * s + v[2] * c;
    c = Math.cos(y); s = Math.sin(y);
    var x2 = x1 * c + z1 * s, y2 = y1, z2 = -x1 * s + z1 * c;
    c = Math.cos(r); s = Math.sin(r);
    return [x2 * c - y2 * s, x2 * s + y2 * c, z2];
  }

  /* 画眼：outlines 是两只眼的局部闭合轮廓（a 沿长轴、b 沿短轴）。
     blink∈[0,1] 闭眼度，锚在眼睛下缘往上塌；指数映射贴到球面。 */
  function drawEyes(pose, blink, outlines) {
    blink = blink || 0;
    var parts = [];
    var inv = 1 / AV.r;
    var k = 1 - 0.935 * blink;
    for (var i = 0; i < FRAMES.length; i++) {
      var f = FRAMES[i], pts = outlines[i];
      if (!pts) continue;
      var C = turn3(f.C, pose), U = turn3(f.u, pose), V = turn3(f.v, pose);
      if (C[2] <= 0.001) continue;
      var cl = Math.hypot(C[0], C[1], C[2]) || 1;
      var cx = C[0] / cl, cy = C[1] / cl;
      var xy = [], maxY = -Infinity, j;
      for (j = 0; j < pts.length; j++) {
        var a = pts[j][1] * inv, b = pts[j][0] * inv;
        var h = Math.hypot(a, b);
        var sn = h > 1e-6 ? Math.sin(h) / h : 1, cs = Math.cos(h);
        var dx = cs * cx + sn * (a * U[0] + b * V[0]);
        var dy = cs * cy + sn * (a * U[1] + b * V[1]);
        var py = AV.cy + dy * AV.r;
        if (py > maxY) maxY = py;
        xy.push([AV.cx + dx * AV.r, py]);
      }
      var d = '';
      for (j = 0; j < xy.length; j++) {
        var qy = maxY + (xy[j][1] - maxY) * k;
        d += (j ? 'L' : 'M') + xy[j][0].toFixed(2) + ' ' + qy.toFixed(2);
      }
      parts.push(d + 'Z');
    }
    return parts.join(' ');
  }

  function faceTransform(pose, zoom, squash) {
    var ox = (pose.ox || 0) * AV.r, oy = (pose.oy || 0) * AV.r;
    var sx = zoom * (squash ? squash[0] : 1);
    var sy = zoom * (squash ? squash[1] : 1);
    return 'translate(' + (AV.cx + ox).toFixed(2) + ' ' + (AV.cy + oy).toFixed(2) +
      ') scale(' + sx.toFixed(4) + ' ' + sy.toFixed(4) + ') translate(' + (-AV.cx) + ' ' + (-AV.cy) + ')';
  }

  var faceSeq = 0;
  /* 挂一颗头：circle + clip + eyes path。 */
  function mountFace(svg, opts) {
    opts = opts || {};
    var uid = 'lfaceclip' + (++faceSeq);
    var head = opts.fill || 'var(--face-body, currentColor)';
    var eye = opts.eye || 'var(--face-cut, var(--bg, #0b0b0a))';
    svg.innerHTML =
      "<defs><clipPath id='" + uid + "'><circle cx='" + AV.cx + "' cy='" + AV.cy + "' r='" + AV.r + "'/></clipPath></defs>" +
      "<g class='face3d'><circle class='head' cx='" + AV.cx + "' cy='" + AV.cy + "' r='" + AV.r +
      "' fill='" + head + "' stroke='currentColor' stroke-width='1' vector-effect='non-scaling-stroke'/>" +
      "<g clip-path='url(#" + uid + ")'><path class='eyes3d' fill='" + eye +
      "' stroke-width='1' stroke-linejoin='round' vector-effect='non-scaling-stroke'/></g></g>";
    var g = svg.querySelector('.face3d');
    var eyePath = svg.querySelector('.eyes3d');
    return {
      svg: svg, g: g, eyes: eyePath,
      paint: function (pose, zoom, eyesD, squash) {
        g.setAttribute('transform', faceTransform(pose, zoom, squash));
        eyePath.setAttribute('d', eyesD);
      }
    };
  }

  /* ==================== 面部风格（9 种） ==================== */
  var EXPRESSIONS = API.build.EXPRESSIONS;
  var OUTLINE_CACHE = {};
  function outlinesAt(name, n) {
    var key = name + ':' + n;
    if (!OUTLINE_CACHE[key]) OUTLINE_CACHE[key] = API.build.expressionOutlines(name, n);
    return OUTLINE_CACHE[key];
  }
  function lerpOutlines(a, b, u) {
    return a.map(function (pts, i) {
      return pts.map(function (p, j) {
        var q = b[i][j];
        return [p[0] + (q[0] - p[0]) * u, p[1] + (q[1] - p[1]) * u];
      });
    });
  }

  /* ==================== idle 片段（引擎原帧） ==================== */
  function easeFn(e) {
    if (!e) return function (x) { return x; };
    var x1 = e[0], y1 = e[1], x2 = e[2], y2 = e[3];
    return function (x) {
      if (x <= 0) return 0;
      if (x >= 1) return 1;
      var t = x;
      for (var i = 0; i < 6; i++) {
        var cx = 3 * x1, bx = 3 * (x2 - x1) - cx, ax = 1 - cx - bx;
        var xt = ((ax * t + bx) * t + cx) * t;
        var dx = (3 * ax * t + 2 * bx) * t + cx;
        if (Math.abs(xt - x) < 1e-5 || Math.abs(dx) < 1e-6) break;
        t -= (xt - x) / dx;
      }
      var cy = 3 * y1, by = 3 * (y2 - y1) - cy, ay = 1 - cy - by;
      return ((ay * t + by) * t + cy) * t;
    };
  }
  function K(t, v, e) { return { t: t, v: v, e: easeFn(e) }; }

  var CLIPS = {
    Idle_A: {
      dur: 6,
      rot: [
        K(0.487, [0, 0, 0], [0.3992, -0.1381, 0.1595, 1]),
        K(1.383, [-16.084, -32.693, -11.039], [0.5825, 0.3102, 0.0462, 1.3611]),
        K(1.780, [-13.471, -31.664, -11.623], [0.2201, 0.3102, 0.0462, 0.9652]),
        K(2.750, [10.374, 13.583, 10.666], [0.7924, 0.0820, 0.2026, 0.5709]),
        K(2.926, [22.820, 5.292, 5.163], [1, -0.5, 0.4052, 0.8872]),
        K(4.080, [-24.875, 31.813, 15.074], [0.8773, 0.0940, 0, 0.9378]),
        K(5.754, [0, 360, 0], [0.9123, 0.0386, 0.4252, 0.8172])
      ],
      pos: [
        K(0.487, [0, 0], [0.3992, -0.1381, 0.1595, 1]),
        K(1.383, [-0.1465, 0.0273], [0.5825, 0.3102, 0.0462, 1.3611]),
        K(1.780, [-0.1465, 0.0273], [0.2201, 0.3102, 0.0462, 0.9652]),
        K(2.750, [-0.0746, -0.1660], [0.7924, 0.0820, 0.2026, 0.5709]),
        K(2.926, [-0.0746, -0.1660], [1, -0.5, 0.4052, 0.8872]),
        K(4.080, [0, 0], [0.8773, 0.0940, 0, 0.9378]),
        K(5.754, [0, 0], [0.9123, 0.0386, 0.4252, 0.8172])
      ],
      blinks: [1.1629, 4.0044]
    },
    Idle_B: {
      dur: 4,
      rot: [
        K(0.901, [0, 0, 0], [0.42, 0, 0.2893, 0.9624]),
        K(1.616, [0.120, -8.659, -2.543], [0.8748, -0.0363, 0.2547, 0.8980]),
        K(2.490, [0.966, 5.659, 1.713], [0.6027, -0.2308, 0.2902, 0.9815]),
        K(3.457, [0, 0, 0], [0.42, 0, 0.58, 1])
      ],
      pos: [K(1.605, [0, 0], [0.42, 0, 0.58, 1])],
      zoom: [
        K(0, [0.6], [0.6180, 0.0012, 0.1839, 0.9359]),
        K(1.605, [0.64], [0.8659, -0.0800, 0.2998, 1.0195]),
        K(3.842, [0.6], [0.6180, 0.0012, 0.1839, 0.9406])
      ],
      blinks: [1.4811, 3.4570]
    },
    Idle_D: {
      dur: 6,
      rot: [
        K(0.000, [-15.896, 33.293, 25.385], [0.6511, 0, 0.3268, 1]),
        K(0.587, [-34.323, 16.862, 15.137], [0.5997, 0, 0.2467, 1]),
        K(1.078, [-34.323, 16.862, 15.137], [0.5997, 0, 0.1066, 1]),
        K(1.963, [-6.168, 3.099, 14.349], [0.42, 0, 0.58, 1]),
        K(2.672, [-6.168, 3.099, 14.349], [0.42, 0, 0.2029, 1]),
        K(3.914, [7.573, 40.387, 25.474], [0.42, 0, 0.58, 1]),
        K(4.786, [7.573, 40.387, 25.474], [0.6316, 0, 0.2164, 1]),
        K(5.633, [-15.896, 33.293, 25.385], [0.6511, 0, 0.3268, 1])
      ],
      pos: [],
      blinks: [0.5929, 2.0691, 2.3723, 4.2475]
    }
  };
  var CLIP_ORDER = ['Idle_B', 'Idle_B', 'Idle_D', 'Idle_B', 'Idle_A'];
  var CYCLE = CLIP_ORDER.map(function (n) { return CLIPS[n].dur; });
  var CYCLE_T = CYCLE.reduce(function (a, b) { return a + b; }, 0);

  function trackV(keys, t) {
    if (!keys || !keys.length) return null;
    if (t <= keys[0].t) return keys[0].v;
    for (var i = 1; i < keys.length; i++) {
      if (t <= keys[i].t) {
        var a = keys[i - 1], b = keys[i];
        var u = (t - a.t) / (b.t - a.t);
        var e = a.e ? a.e(u) : u;
        return [
          a.v[0] + (b.v[0] - a.v[0]) * e,
          a.v[1] + (b.v[1] - a.v[1]) * e,
          a.v.length > 2 ? a.v[2] + (b.v[2] - a.v[2]) * e : 0
        ];
      }
    }
    return keys[keys.length - 1].v;
  }

  // 引擎坐标（y 向上）→ 屏幕坐标（y 向下）：pitch/roll 取反、y 取反。
  function clipPose(clip, t) {
    var r = trackV(clip.rot, t) || [0, 0, 0];
    var p = trackV(clip.pos, t) || [0, 0];
    var z = trackV(clip.zoom, t);
    return {
      pitch: -r[0], yaw: r[1], roll: -r[2],
      ox: p[0], oy: -p[1],
      zoom: z ? 1 + (z[0] - 0.62) / 0.62 : 1
    };
  }
  function align(from, to) { return to + 360 * Math.round((from - to) / 360); }
  function clipAt(T) {
    var x = ((T % CYCLE_T) + CYCLE_T) % CYCLE_T, i = 0;
    while (x >= CYCLE[i]) { x -= CYCLE[i]; i += 1; }
    return { clip: CLIPS[CLIP_ORDER[i]], ct: x, idx: i };
  }

  function ease(x) { x = Math.max(0, Math.min(1, x)); return x * x * (3 - 2 * x); }
  function clamp01(x) { return Math.max(0, Math.min(1, x)); }
  function rand(a, b) { return a + Math.random() * (b - a); }
  function pick(list) { return list[(Math.random() * list.length) | 0]; }
  function gazeOffset(gx, gy) {
    return { yaw: 18 * gx, pitch: 14 * gy, ox: 0.03 * gx, oy: -0.024 * gy };
  }
  function bump(u, a, b) {
    return u <= a || u >= b ? 0 : Math.pow(Math.sin((Math.PI * (u - a)) / (b - a)), 2);
  }

  /* 跳摆：三连跳左右横摆（中心→右→左→中心） */
  var SWAY = { A: 0.2, H: 0.24, hop: 0.62, gap: 0.035 };
  SWAY.span = 3 * SWAY.hop + 2 * SWAY.gap;
  function swayAt(u) {
    var t = u * SWAY.span;
    var step = SWAY.hop + SWAY.gap;
    var i = Math.min(2, Math.floor(t / step));
    var q = Math.min(1, (t - i * step) / SWAY.hop);
    var from = [0, SWAY.A, -SWAY.A][i];
    var to = [SWAY.A, -SWAY.A, 0][i];
    var fly = clamp01((q - 0.14) / 0.72);
    var e = ease(fly);
    var land = bump(q, 0, 0.18) + bump(q, 0.82, 1);
    var air = bump(q, 0.18, 0.82);
    return {
      ox: from + (to - from) * e,
      oy: -SWAY.H * Math.sin(Math.PI * fly),
      sx: 1 + 0.08 * land - 0.04 * air,
      sy: 1 - 0.1 * land + 0.06 * air
    };
  }

  // 环视（tH 预设：±20°、球心 0.1R）
  var LOOK_TL = [
    { t: 0.00, v: [0, 0, 0, 0] },
    { t: 0.45, v: [-20, 0, -0.10, 0] },
    { t: 1.15, v: [20, 0, 0.10, 0] },
    { t: 1.75, v: [0, 20, 0, -0.10] },
    { t: 2.45, v: [0, -20, 0, 0.10] },
    { t: 3.05, v: [0, 0, 0, 0] }
  ];

  var ACTIONS = ['blink', 'look', 'nod', 'sway', 'bounce', 'spin'];
  var ACTION_WEIGHT = { blink: 3, look: 2, nod: 2, sway: 1, bounce: 1, spin: 1 };
  var ACTION_BAG = (function () {
    var bag = [];
    ACTIONS.forEach(function (a) { for (var i = 0; i < ACTION_WEIGHT[a]; i += 1) bag.push(a); });
    return bag;
  })();

  /* ==================== 场景（一颗或多颗头共用一个时钟） ==================== */
  function createScene(opts) {
    opts = opts || {};
    var reduce = opts.reduced != null ? opts.reduced
      : !!(global.matchMedia && global.matchMedia('(prefers-reduced-motion: reduce)').matches);
    var living = opts.living !== false;
    var gapMin = (opts.gap && opts.gap[0]) || 3200;
    var gapMax = (opts.gap && opts.gap[1]) || 7000;

    var scene = {
      faces: [],
      pointer: null,
      gaze: { x: 0, y: 0, h: 0 },
      reduce: reduce,
      t0: 0, t: 0,
      last: 0, lastFrame: 0, raf: 0, running: false,
      aim: opts.aim || null
    };

    function nowMs() { return global.performance ? global.performance.now() : Date.now(); }

    /* ---------- 一颗头 ---------- */
    function makeFace(svg) {
      var ds = svg.dataset || {};
      var base = EXPRESSIONS.indexOf(ds.expr) >= 0 ? ds.expr : 'neutral';
      var face = {
        svg: svg,
        mount: mountFace(svg, { fill: ds.fill, eye: ds.eye }),
        samples: Math.max(24, Number(ds.samples) || 96),
        phase: Number(ds.phase) || rand(0, 8),
        gaze: ds.gaze !== 'off',
        // 小尺寸的衰减：44px 上 −34° 的 3/4 脸会把一只眼压成一条缝，看着像坏了。
        // 大脸保持引擎原幅度，小脸按 data-damp 收窄（默认 1）。
        damp: ds.damp == null ? 1 : Math.max(0, Math.min(1, Number(ds.damp))),
        live: ds.live !== 'off' && living,
        still: ds.still === 'on',      // 画廊里的静态样本：正视、不跑 idle/视线
        base: base, expr: base, exprFrom: null, exprT0: 0,
        pokes: [], lookT0: -1, blinkT0: -1,
        nextStyleAt: 0, lastD: null,
        visible: true
      };
      face.nextStyleAt = face.phase + rand(0.6, gapMax / 1000);
      face.paintStatic = function () { paintFace(face, face.phase, true); };
      scene.faces.push(face);
      svg.__lunaFace = face;
      return face;
    }

    /* ---------- 挂载 ---------- */
    function sync(root) {
      root = root || global.document;
      if (!root.querySelectorAll) return;
      var nodes = root.querySelectorAll('svg[data-luna-face]');
      for (var i = 0; i < nodes.length; i += 1) {
        if (!nodes[i].__lunaFace) makeFace(nodes[i]);
      }
      scene.faces = scene.faces.filter(function (f) {
        if (f.svg.isConnected && f.svg.__lunaFace === f) return true;
        return false;
      });
    }

    /* ---------- 风格（9 种） ---------- */
    function outlinesFor(face, now) {
      if (!face.exprFrom || !face.exprT0) return outlinesAt(face.expr, face.samples);
      var u = clamp01((now - face.exprT0) / 300);
      if (u >= 1) { face.exprFrom = null; face.exprT0 = 0; return outlinesAt(face.expr, face.samples); }
      return lerpOutlines(face.exprFrom, outlinesAt(face.expr, face.samples), ease(u));
    }
    function setExpr(face, name, immediate) {
      if (EXPRESSIONS.indexOf(name) < 0) return;
      if (face.expr === name && !immediate) return;
      if (!immediate) face.exprFrom = outlinesFor(face, nowMs());
      else face.exprFrom = null;
      face.expr = name;
      face.exprT0 = immediate ? 0 : nowMs();
      if (face.svg.dataset) face.svg.dataset.expr = name;
      if (scene.reduce) face.paintStatic();
    }
    function setBase(face, name) {
      face.base = EXPRESSIONS.indexOf(name) >= 0 ? name : 'neutral';
      face.svg.dataset.base = face.base;
    }
    /* 随机下一个风格：40% 回到状态基准，60% 换一个别的 */
    function nextStyle(face) {
      var pool = EXPRESSIONS.filter(function (e) { return e !== face.expr; });
      var name = Math.random() < 0.4 ? face.base : pick(pool);
      if (name === face.expr) name = pick(pool);
      setExpr(face, name);
      return name;
    }

    /* ---------- 动作（6 种，逐颗头独立） ---------- */
    function pokeFace(face, type) {
      if (scene.reduce) return;
      if (type === 'look') { face.lookT0 = scene.t + face.phase; return; }
      if (type === 'blink') { face.blinkT0 = scene.t + face.phase; return; }
      if (type === 'sway') {
        setExpr(face, 'happy');
        face.swayBack = face.expr !== 'happy';
      }
      face.pokes.push({ type: type, t0: scene.t + face.phase });
    }
    function lookPose(face, T) {
      if (face.lookT0 < 0) return null;
      var t = T - face.lookT0;
      if (t >= 3.05) { face.lookT0 = -1; return null; }
      if (t < 0) return null;
      for (var i = 1; i < LOOK_TL.length; i += 1) {
        if (t <= LOOK_TL[i].t) {
          var a = LOOK_TL[i - 1], b = LOOK_TL[i];
          var u = ease((t - a.t) / (b.t - a.t));
          return {
            yaw: a.v[0] + (b.v[0] - a.v[0]) * u,
            pitch: a.v[1] + (b.v[1] - a.v[1]) * u,
            ox: a.v[2] + (b.v[2] - a.v[2]) * u,
            oy: a.v[3] + (b.v[3] - a.v[3]) * u
          };
        }
      }
      return null;
    }
    function evalPokes(face, T) {
      var out = { yaw: 0, pitch: 0, ox: 0, oy: 0, sx: 1, sy: 1 };
      face.pokes = face.pokes.filter(function (p) {
        var dur = p.type === 'spin' ? 0.9 : p.type === 'nod' ? 0.5
          : p.type === 'sway' ? SWAY.span : 0.62;
        var u = (T - p.t0) / dur;
        if (u >= 1) {
          if (p.type === 'sway' && face.swayBack) { face.swayBack = false; setExpr(face, face.base); }
          return false;
        }
        if (u < 0) return true;
        if (p.type === 'spin') {
          out.yaw += 180 * (1 - Math.cos(Math.PI * u));
        } else if (p.type === 'nod') {
          out.pitch -= 16 * Math.pow(Math.sin(Math.PI * u), 2);
        } else if (p.type === 'bounce') {
          var e = bump(u, 0, 0.18) + bump(u, 0.82, 1);
          var n = bump(u, 0.18, 0.82);
          out.oy -= 0.3 * Math.sin(Math.PI * clamp01((u - 0.14) / 0.72));
          out.sx *= 1 + 0.08 * e - 0.04 * n;
          out.sy *= 1 - 0.1 * e + 0.06 * n;
        } else if (p.type === 'sway') {
          var s = swayAt(u);
          out.ox += s.ox; out.oy += s.oy;
          out.yaw += 10 * (s.ox / SWAY.A);
          out.sx *= s.sx; out.sy *= s.sy;
        }
        return true;
      });
      return out;
    }

    /* ---------- 眨眼 ---------- */
    function clipBlink(clip, ct) {
      var amt = 0;
      var list = clip.blinks || [];
      for (var i = 0; i < list.length; i += 1) {
        var u = (ct - list[i]) / 0.26;
        if (u > 0 && u < 1) amt = Math.max(amt, u < 0.45 ? ease(u / 0.45) : 1 - ease((u - 0.45) / 0.55));
      }
      return amt;
    }
    function manualBlink(face, T) {
      if (face.blinkT0 < 0) return 0;
      var u = (T - face.blinkT0) / 0.26;
      if (u >= 1) { face.blinkT0 = -1; return 0; }
      if (u < 0) return 0;
      return u < 0.45 ? ease(u / 0.45) : 1 - ease((u - 0.45) / 0.55);
    }

    /* ---------- 合成一帧 ---------- */
    function poseFor(face, T) {
      var at = clipAt(T);
      var ct = at.ct;
      var pose = clipPose(at.clip, Math.min(ct, at.clip.dur));
      if (ct < 0.35 && T > 0.35) {   // 场景刚起步时别从上一轮的 Idle_A 末尾（yaw 360）桥过来
        // 片段之间的 350ms 桥：Idle_D 是 3/4 脸，Idle_B 正视
        var prevName = CLIP_ORDER[(at.idx - 1 + CLIP_ORDER.length) % CLIP_ORDER.length];
        var prev = clipPose(CLIPS[prevName], CLIPS[prevName].dur);
        var q = ease(ct / 0.35);
        pose = {
          yaw: prev.yaw + (align(prev.yaw, pose.yaw) - prev.yaw) * q,
          pitch: prev.pitch + (align(prev.pitch, pose.pitch) - prev.pitch) * q,
          roll: prev.roll + (align(prev.roll, pose.roll) - prev.roll) * q,
          ox: prev.ox + (pose.ox - prev.ox) * q,
          oy: prev.oy + (pose.oy - prev.oy) * q,
          zoom: (prev.zoom || 1) + ((pose.zoom || 1) - (prev.zoom || 1)) * q
        };
      }
      var look = lookPose(face, T);
      if (look) {
        pose.yaw += look.yaw; pose.pitch += look.pitch;
        pose.ox += look.ox; pose.oy += look.oy;
      }
      if (face.gaze) {
        var g = gazeOffset(scene.gaze.x * scene.gaze.h * 0.55, scene.gaze.y * scene.gaze.h * 0.55);
        pose.yaw += g.yaw; pose.pitch += g.pitch;
        pose.ox += g.ox; pose.oy += g.oy;
      }
      var fx = evalPokes(face, T);
      pose.yaw += fx.yaw; pose.pitch += fx.pitch;
      pose.ox += fx.ox; pose.oy += fx.oy;
      if (face.damp !== 1) {
        pose.yaw *= face.damp; pose.pitch *= face.damp; pose.roll *= face.damp;
      }
      return {
        pose: pose,
        zoom: pose.zoom || 1,
        squash: [fx.sx, fx.sy],
        blink: Math.max(clipBlink(at.clip, ct), manualBlink(face, T))
      };
    }

    function paintFace(face, T, immediate) {
      if (!face.visible) return;
      var s = face.still ? { pose: FACE0, zoom: 1, squash: null, blink: 0 } : poseFor(face, T);
      var outlines = immediate ? outlinesAt(face.expr, face.samples) : outlinesFor(face, nowMs());
      var d = drawEyes(s.pose, s.blink, outlines);
      if (d === face.lastD) return;               // 静止帧不重复写 DOM
      face.lastD = d;
      face.mount.paint(s.pose, s.zoom, d, s.squash);
    }

    /* ---------- 主循环 ---------- */
    function step(now) {
      var dt = Math.min(0.1, Math.max(0.001, (now - scene.last) / 1000));
      scene.last = now;
      scene.t = (now - scene.t0) / 1000;

      var tx = scene.pointer ? scene.pointer.x : scene.gaze.x;
      var ty = scene.pointer ? scene.pointer.y : scene.gaze.y;
      var th = scene.pointer ? 1 : 0;
      var k = 1 - Math.pow(0.86, dt * 30);
      scene.gaze.x += (tx - scene.gaze.x) * k;
      scene.gaze.y += (ty - scene.gaze.y) * k;
      scene.gaze.h += (th - scene.gaze.h) * k;

      for (var i = 0; i < scene.faces.length; i += 1) {
        var face = scene.faces[i];
        if (!face.visible) continue;
        var T = scene.t + face.phase;
        // 换风格 → 顺手做 1 个动作 → 隔 3.2–7s 再换，往复
        if (face.live && T >= face.nextStyleAt) {
          nextStyle(face);
          if (Math.random() < 0.85) pokeFace(face, pick(ACTION_BAG));
          face.nextStyleAt = T + rand(gapMin, gapMax) / 1000;
        }
        paintFace(face, T);
      }
    }

    function paintStaticAll() {
      for (var i = 0; i < scene.faces.length; i += 1) {
        var f = scene.faces[i];
        if (f.still || !scene.reduce) { paintFace(f, f.phase, true); continue; }
        var g = gazeOffset(scene.gaze.x * 0.55, scene.gaze.y * 0.55);
        var pose = { yaw: g.yaw, pitch: g.pitch, roll: 0, ox: g.ox, oy: g.oy };
        var d = drawEyes(pose, 0, outlinesAt(f.expr, f.samples));
        f.lastD = d;
        f.mount.paint(pose, 1, d, null);
      }
    }

    function loop(now) {
      if (scene.running && !global.document.hidden && now - scene.lastFrame >= 32) {
        scene.lastFrame = now;
        step(now);
      }
      if (scene.running) scene.raf = global.requestAnimationFrame(loop);
    }

    function aimAt(x, y) {
      var el = scene.aim;
      var r = (el && el.getBoundingClientRect) ? el.getBoundingClientRect()
        : { left: 0, top: 0, width: global.innerWidth, height: global.innerHeight };
      var cx = r.width ? r.left + r.width / 2 : global.innerWidth / 2;
      var cy = r.height ? r.top + r.height / 2 : global.innerHeight / 2;
      var d = Math.max(220, Math.min(520, r.width || 520));
      scene.pointer = {
        x: Math.max(-1, Math.min(1, (x - cx) / d)),
        y: Math.max(-1, Math.min(1, -(y - cy) / d))
      };
    }

    function start() {
      if (scene.running) return;
      if (scene.reduce) {
        paintStaticAll();
        global.document.addEventListener('pointermove', function (e) { aimAt(e.clientX, e.clientY); paintStaticAll(); });
        return;
      }
      scene.t0 = nowMs();
      scene.last = scene.t0;
      scene.running = true;
      scene.lastFrame = 0;
      scene.raf = global.requestAnimationFrame(loop);
    }
    function stop() {
      scene.running = false;
      if (scene.raf) global.cancelAnimationFrame(scene.raf);
      scene.raf = 0;
    }

    if (!reduce) {
      global.document.addEventListener('pointermove', function (e) { aimAt(e.clientX, e.clientY); });
      global.document.addEventListener('pointerleave', function () { scene.pointer = null; });
      global.addEventListener('blur', function () { scene.pointer = null; });
    }

    return {
      reduce: reduce,
      sync: sync,
      start: start,
      stop: stop,
      setExpr: function (svg, name) {
        var f = svg.__lunaFace || svg;
        if (f && f.expr !== undefined) setExpr(f, name);
      },
      setBase: function (svg, name) {
        var f = svg.__lunaFace || svg;
        if (f && f.base !== undefined) setBase(f, name);
      },
      poke: function (svg, type) { var f = svg.__lunaFace; if (f) pokeFace(f, type); },
      pokeAll: function (type) { scene.faces.forEach(function (f) { pokeFace(f, type); }); },
      faces: function () { return scene.faces; },
      faceOf: function (svg) { return svg.__lunaFace || null; },
      paintStatic: paintStaticAll,
      aimAt: aimAt,
      step: step,
      get time() { return scene.t; }
    };
  }

  global.LUNA_FACE = {
    AVATAR: AV,
    EXPRESSIONS: EXPRESSIONS,
    CLIPS: CLIPS,
    CLIP_ORDER: CLIP_ORDER,
    ACTIONS: ACTIONS,
    SWAY: SWAY,
    createScene: createScene,
    mountFace: mountFace,
    drawEyes: drawEyes,
    outlines: outlinesAt,
    eyeFrames: function () { return FRAMES; },
    clipPose: clipPose,
    clipAt: clipAt
  };
  if (typeof module !== 'undefined' && module.exports) module.exports = global.LUNA_FACE;
})(typeof window !== 'undefined' ? window : globalThis);
