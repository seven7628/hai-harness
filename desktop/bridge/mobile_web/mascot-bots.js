/* ============================================================
   Luna · 黑白机器人头像 + 字母标   （2026-10-06 定稿；2026-10-20 照 Grok Bot 重做）
   （2026-10-06 晚由 ui/mascot/ 迁入 scripts/；现供 gen-robot-svg.mjs（Buddy / Wordmark /
   LunaMark）、gen-app-icon.mjs / gen-share-card.mjs 取 build 出口组装 App 资产）
   ------------------------------------------------------------
   头像定稿「胶囊」一款（BOTS，旧黑砖画法，已归档），词标定稿「lüna」一条（WORDS，u 上两点是眼睛）。
   小程序里落地的是下面 AVATAR 那张几何表。

   2026-10-20 重做：**推倒「黑白机器人 + 牛仔帽」，照 xAI 的 Grok Bot 头像重建**。
   帽子（含爆圈/声呐锚点）整体退役，状态指示器改锚到头本身。几何依据是在 x.ai/bot 上
   量真页面的 SVG 路径（不是目测）：
     · 头 = 正圆，径向偏差 < 0.7%，不是方圆形；
     · 眼 = 真胶囊（直边平行 + 半圆端），**两只都往左倒**（`\`，偏离竖直线 −26°）——
       社区普遍以为是 `//`，实测是反的；
     · 两只眼**都在头的右上方**且大小不等（右上那只约 0.92×）；
     · 两眼连线与各自长轴**近乎垂直**（球面正交投影的结果）。
   规矩：
   · 只用两个色块，纯黑白。不画嘴、不画鼻、不画眉、不画腮红，表情全交给眼睛。
   · 眼睛：睁→眨→半闭（思考）→睁大（听）→两道弧（开心）→内八半闭（答错）。
   · 环（爆圈/声呐）与头**同心**，是正圆——旧设计里环锚在帽上、被迫做成椭圆，随帽一起退役。
   · 字母的笔画就是它的身体：思考整块下沉、听支起来、开心跳一下、答错歪掉。

   坐标系：头像 240×240；字母标方版 240×240、横版 480×220（字高 78，基线 170）。
   几何全部写在下面，改数字即改脸。
   ============================================================ */
(function (global) {
  'use strict';

  var C = {
    black: '#000000', // 黑舞台
    white: '#FFFFFF', // 白舞台 / 白脸
    ink:   '#0E0E0E'  // 黑脸 / 黑眼：比纯黑抬一点点，压在纯黑底上也有边
  };

  /* ---------------- 零件 ---------------- */

  // 竖胶囊眼（长宽约 3:1）
  function capsule(w, h, fill, side) {
    return '<rect class="eye eye-' + side + '" x="' + (-w / 2) + '" y="' + (-h / 2) + '" width="' + w +
           '" height="' + h + '" rx="' + (w / 2) + '" fill="' + fill + '"/>';
  }
  // 开心时的弧眼。曲线高度**随宽度等比**（基线 6/21·w、顶点 17/21·w）——
  // 默认 w=21 时与旧稿逐像素相同，缩小时弧才不会被拉成一道瘦高的「∩」。
  // 2026-10-20 重做后眼睛变窄（20 宽）且偏心在头右上方，弧必须跟着缩到
  // w=13/sw=8，否则弧端会顶出头轮廓、在脸上咬出一块缺口（实测过）。
  function arc(fill, sw, w) {
    w = w || 21;
    var r2 = function (v) { return Math.round(v * 100) / 100; };
    var b = r2((w * 6) / 21);
    var a = r2((w * 17) / 21);
    return '<path class="eye" d="M ' + (-w) + ' ' + b + ' Q 0 ' + (-a) + ' ' + w + ' ' + b +
           '" fill="none" stroke="' + fill + '" stroke-width="' + sw + '" stroke-linecap="round"/>';
  }
  // 圆点眼
  function dot(r, fill, side) {
    return '<circle class="eye eye-' + side + '" r="' + r + '" fill="' + fill + '"/>';
  }
  // 把一个零件摆到 (x,y) 并自带倾斜（倾斜放在外层 g 上，CSS 改内层 .eye 不会打架）
  function at(x, y, rot, inner) {
    return '<g transform="translate(' + x + ' ' + y + ') rotate(' + rot + ')">' + inner + '</g>';
  }
  // 状态灯：外圈=听 (slow ring)，中圈=开心 (burst)，芯=本体
  function bead(x, y, r, fill) {
    return '<g class="bead">' +
      '<circle class="bead-halo" cx="' + x + '" cy="' + y + '" r="' + (r + 9) + '" fill="none" stroke="' + fill + '" stroke-width="2.5" opacity="0"/>' +
      '<circle class="bead-ring" cx="' + x + '" cy="' + y + '" r="' + r + '" fill="none" stroke="' + fill + '" stroke-width="3" opacity="0"/>' +
      '<circle class="bead-core" cx="' + x + '" cy="' + y + '" r="' + r + '" fill="' + fill + '"/>' +
      '</g>';
  }
  /* 环（爆圈 / 声呐）：与头**同心**的正圆——2026-10-20 重做后环锚在头上，
     不再是旧设计里「围住那顶帽子」的椭圆（帽退役，椭圆/旋转的复杂度一并消失）。
     半径 = 头半径 + pad；pad 的取值受画框预算约束（见 gen-robot-svg.mjs 的断言）。 */
  function headRing(cx, cy, r, pad, fill, sw) {
    return '<circle class="head-ring" cx="' + cx + '" cy="' + cy + '" r="' + (r + pad) +
      '" fill="none" stroke="' + fill + '" stroke-width="' + sw + '"/>';
  }
  /* ---------------- 换形（处理中 / 出错 / 完成） ----------------
     2026-10-22 增补，照 x.ai/bot 的 Avatar motion system：状态可以整块换剪影。
     三个符号都画在同一个 240 网格里，几何取 AVATAR.swap；调用方只给两种颜色
     （body=剪影色、cut=镂空色），暗色主题沿用既有的逐通道反相。 */
  function swapDot(i, fill) {
    var d = AVATAR.swap.dots;
    return '<circle class="swap-dot" cx="' + (AVATAR.cx + (i - 1) * d.gapX) +
      '" cy="' + d.y + '" r="' + d.r + '" fill="' + fill + '"/>';
  }
  function dotsMark(fill) {
    return swapDot(0, fill) + swapDot(1, fill) + swapDot(2, fill);
  }
  function alertMark(fill) {
    var a = AVATAR.swap.alert;
    return '<g class="swap-alert">' +
      '<rect x="' + (a.stemX - a.stemW / 2) + '" y="' + (a.stemY - a.stemH / 2) +
        '" width="' + a.stemW + '" height="' + a.stemH + '" rx="' + a.stemW / 2 + '" fill="' + fill + '"/>' +
      '<circle cx="' + a.stemX + '" cy="' + a.dotY + '" r="' + a.dotR + '" fill="' + fill + '"/>' +
      '</g>';
  }
  /* 绶带（照 grok 的 celebrate）：每条带子画两遍——先 body 色描边、再 cut 色内芯，
     于是压在头上是白带、越过轮廓落到页面底色上只剩一道黑描边，两个底色都读得出。
     眼睛上方再垫一枚 body 色「衬底胶囊」：带子从眼后穿过时，白带不会和白眼糊成一片
     （衬底比眼大 13，形成一圈 6.5 的黑边）。 */
  function celebrateMark(body, cut) {
    var s = AVATAR.swap;
    var strokes = function (w, color) {
      return s.ribbon.arcs.map(function (d) {
        return '<path class="ribbon" d="' + d + '" fill="none" stroke="' + color +
          '" stroke-width="' + w + '" stroke-linecap="round"/>';
      }).join('');
    };
    var e = eyeBoxes()[0];
    var backing = at(e.x, e.y, 0, capsule(e.w + 13, e.h + 13, body, 'l'));
    return '<g class="ribbons">' + strokes(s.ribbon.outlineW, body) + strokes(s.ribbon.strokeW, cut) + '</g>' +
      '<g class="eye-backing">' + backing + '</g>';
  }
  // 两只眼睛（字母标用）
  function pair(a, b, w, h, fill) {
    return at(a[0], a[1], 0, capsule(w, h, fill, 'l')) + at(b[0], b[1], 0, capsule(w, h, fill, 'r'));
  }
  // 开机零件：每个笔画一个 .p，按 --i 依次入场（入场方向由 dir 决定）
  function part(i, inner, dir, amount) {
    var cls = 'p', v = '--i:' + i + ';';
    if (dir === 'x') v += '--fx:' + amount + 'px;';
    else if (dir === 'y') v += '--fy:' + amount + 'px;';
    else if (dir === 'grow') cls += ' grow';
    else if (dir === 'pop') cls += ' pop';
    return '<g class="' + cls + '" style="' + v + '">' + inner + '</g>';
  }
  function eyesGroup(inner) {
    return '<g class="eyes"><g class="e-idle">' + inner + '</g></g>';
  }

  /* ---------------- 头像：定稿一款 ---------------- */

  var BOTS = [
    {
      id: '01', slug: 'pill', name: '胶囊', en: 'Capsule',
      note: '最贴 Grok bot 那一挂：纯黑底、白脸，左脸和下巴被画框切掉，黑胶囊眼顺着歪 14°。',
      bg: C.black, light: false,
      tilt: 12, ox: 96, oy: 152,
      bead: [196, 52, 11, C.white],   // 落在黑舞台上的一颗白灯
      draw: function () {
        return '<circle cx="96" cy="152" r="118" fill="' + C.white + '"/>' +
          '<g class="eyes">' +
            '<g class="e-idle">' + at(50, 126, 2, capsule(24, 68, C.ink, 'l')) +
                                   at(142, 126, 2, capsule(24, 68, C.ink, 'r')) + '</g>' +
            '<g class="e-happy">' + at(50, 126, 2, arc(C.ink, 12)) +
                                    at(142, 126, 2, arc(C.ink, 12)) + '</g>' +
          '</g>';
      }
    }
  ];

  /* ---------------- 头像几何表（小程序侧共用真源） ----------------
     上面 BOTS[0].draw 是旧「胶囊」的黑砖画法（规范页场景，已随 ui/ 归档）；
     小程序里落地的是下面这张表：无底剪影 + 完整圆形，240×240 网格。
     分层资产（scripts/gen-robot-svg.mjs）、小程序头像出图（scripts/gen-app-icon.mjs）
     与分享卡（scripts/gen-share-card.mjs）都消费这张表——改数字即改脸，三处同步。

     2026-10-20 重做（照 Grok Bot）：**牛仔帽退役**，头回到画面正中、放大到 r=100。
     2026-10-24 修正（ADR-0021）：斜眼（右上偏心、−26°、大小不等）是引擎 Idle_D
     的**静息朝向**，不是眼睛几何。真几何是**正脸**——两眼在赤道上左右对称
     ±0.32R，同为 0.26R × 0.57R 的竖直胶囊；9 种眼形见 SHAPES。
     品牌符号（LunaMark / app icon / 分享卡）也吃这张正脸表。 */
  var AVATAR = {
    cx: 120, cy: 120, r: 100,          // 正圆头，居中；四周留 20 —— 环（爆圈/声呐）的预算
    /* 眼（正脸）：两眼在赤道上关于头心左右对称，±0.32R = (88,120)/(152,120)；
       竖直胶囊 0.26R × 0.57R = 26 × 57。9 种眼形由 SHAPES 在各自眼睛局部空间里
       缩放/旋转/偏移，见 scripts/preview/avatar.html 的 1:1 移植。 */
    eyes: [
      { x: 88.0, y: 120.0 },   // 左眼：120 − 0.32·100
      { x: 152.0, y: 120.0 }   // 右眼：120 + 0.32·100
    ],
    eyeW: 26, eyeH: 57,
    /* 汗滴已退役（2026-10-24）：窘迫 / 眩晕一律用 confused 眼形表达。 */
    /* 环（爆圈/声呐）：与头同心、正圆。pad 由**画框预算**反推——
       半帧 120 除以 WXSS 里的最大缩放倍数（炸圈 1.16× / 声呐 1.12×，见 Buddy/index.scss），
       减去描边半宽，得 ring pad ≤ 1.5、halo pad ≤ 5。
       ⚠️ 改 r 或 pad 必须先看 gen-robot-svg.mjs 的出图前断言，它会直接抛错。 */
    ring: { pad: 1.5, haloPad: 5, ringW: 3, haloW: 2.5 },
    /* 静态品牌符号 LunaMark（2026-10-23）：头圆 + 两只眼洞合成一个 d，
       solid 用 fill-rule="evenodd" 挖出**真透明洞**；outline 用同一 d 描边。
       strokeW 是 240 网格上的描边宽——头圆与眼洞共用；只用于大尺寸（预览再调 1–3）。
       取 1：启动页 520 设计 px（手机上约 424 CSS px）下约 1.77px，与 x.ai/bot footer
       水印的 1.75px 非缩放描边同量级。 */
    mark: { strokeW: 1 },
    /* 换形（2026-10-22，照 x.ai/bot 的 Avatar motion system）：三个高光状态不再
       只换眼形，而是**整块剪影换成别的符号**——处理中=三个点、出错=叹号、
       完成=绶带球。全部画在同一个 240 网格里，头隐没时符号自己撑住画面重心。 */
    swap: {
      /* 三点：等距横排、落在头心高度（视觉重心仍是 120,120）。拆成三个独立
         图层出图（gen-robot-svg.mjs），WXSS 才能给每颗点错峰 animation-delay。 */
      // 圆心 (70,120) (120,120) (170,120)；比例照 grok 实测（点径约 0.35R、心距约 0.5R）
      dots: { r: 16, gapX: 50, y: 120 },
      /* 叹号：胶囊竖杆 + 圆点。杆 26×88、帽端全圆；整体纵向 52..175，重心仍在头心。 */
      alert: { stemW: 26, stemH: 88, stemX: 120, stemY: 96, dotR: 13, dotY: 162 },
      /* 绶带：照 grok 的 celebrate 实测——**六条粗细相近的圆头带状曲线斜穿球体**，
         端点越过头的轮廓、落进画框四周的 20px 余量里（grok 原图里它们甚至越出画框，
         我们的画框预算只允许到这里）。没有纸屑，grok 的庆祝态只有绶带。
         黑白翻法：每条带子先画一道 19 的 body 色（描边），再叠一道 13 的 cut 色——
         压在黑头上是白带，越过轮廓落到白底上时只剩黑描边，两侧都读得出来。
         最远控制点到头心 107.7，含 19 描边半宽 9.5 后仍 < 120，不触画框。 */
      ribbon: { strokeW: 13, outlineW: 19, arcs: [
        'M 40 190 C 104.5 150.6 144.5 94 160 20',
        'M 200 50 C 177.3 98.9 138.9 122.3 85 120',
        'M 30 150 C 85.4 112.4 145.4 102.4 210 120',
        'M 195 195 C 137.3 200.7 90.7 180.7 55 135',
        'M 70 215 C 123.4 198.4 158.4 163.4 175 110',
        'M 60 60 C 121.8 78.5 166.8 116.9 195 175'
      ] }
    }
  };

  /* 两只眼的实际落点与尺寸：正脸两眼同尺寸，中心由 AVATAR.eyes 给。
     三个生成脚本共用（改比例时只改这里）。 */
  function eyeBoxes() {
    return AVATAR.eyes.map(function (e, i) {
      return {
        x: e.x, y: e.y,
        w: AVATAR.eyeW, h: AVATAR.eyeH,
        side: i === 0 ? 'l' : 'r'
      };
    });
  }

  /* ---------------- 正脸 9 表情（ADR-0021） ----------------
     1:1 移植自 scripts/preview/avatar.html 的引擎形状表。单位都是 R（=100 px），
     `off` 也是；先在眼睛局部空间（y 向上、长轴竖直）造轮廓，再按每只眼的尺寸
     缩放（正脸两眼同为 26×57 ⇒ sx = sy = 100）。预览页直接消费这些函数，
     生成器再映射到 240 网格（Y = 120 − ly）。 */
  var EXPRESSIONS = ['neutral', 'happy', 'surprised', 'sad', 'sleepy', 'squinted', 'calm', 'inspecting', 'confused'];
  var NORM = { w: 0.26, h: 0.57 };
  var SHAPES = {
    neutral: [{ type: 'cap', w: 0.26, h: 0.57, rot: 0, off: [0, 0] }],
    happy: [{ type: 'stroke', p0: [0.16, -0.04], p1: [0, 0.12], p2: [-0.16, -0.04], wid: 0.1305, off: [0.03, 0.03] }],
    surprised: [{ type: 'cap', w: 0.42, h: 0.86, rot: 0, off: [0.03, 0.03] }],
    sad: [
      { type: 'cap', w: 0.26, h: 0.50, rot: -28, off: [-0.05, 0] },
      { type: 'cap', w: 0.26, h: 0.50, rot: 28, off: [-0.05, 0] }
    ],
    sleepy: [{ type: 'stroke', p0: [-0.11, 0.025], p1: [0, -0.05], p2: [0.11, 0.025], wid: 0.13, off: [0.01, 0] }],
    squinted: [{ type: 'cap', w: 0.26, h: 0.50, rot: 90, off: [0.01, 0] }],
    calm: [{ type: 'oval', rx: 0.296, ry: 0.247, rot: 0, off: [0.05, 0] }],
    inspecting: [
      { type: 'cap', w: 0.54, h: 0.54, rot: 0, off: [0.04, 0] },
      { type: 'cap', w: 0.27, h: 0.27, rot: 0, off: [0.04, 0] }
    ],
    confused: [{ type: 'stroke', p0: [0.01, 0.16], p1: [-0.17, 0], p2: [0.01, -0.16], wid: 0.1305, off: [-0.1, 0] }]
  };

  var r2 = function (v) { return Math.round(v * 100) / 100; };

  function capRaw(w, h) {
    var rc = Math.min(w, h) / 2;
    var hl = Math.max(0, h / 2 - rc);
    var pts = [];
    for (var i = 0; i < 96; i++) {
      var a = (i / 96) * Math.PI * 2;
      pts.push([Math.cos(a) * rc, (Math.sin(a) >= 0 ? hl : -hl) + Math.sin(a) * rc]);
    }
    return pts;
  }

  function ovalRaw(rx, ry) {
    var pts = [];
    for (var i = 0; i < 96; i++) {
      var a = (i / 96) * Math.PI * 2;
      pts.push([Math.cos(a) * rx, Math.sin(a) * ry]);
    }
    return pts;
  }

  // 过三点的圆弧（引擎 happy 的 isArc 形态；其余笔划也用同一圆弧近似）
  function curvePts(p0, p1, p2) {
    var ax = p0[0], ay = p0[1], bx = p1[0], by = p1[1], cx = p2[0], cy = p2[1];
    var d = 2 * (ax * (by - cy) + bx * (cy - ay) + cx * (ay - by));
    if (Math.abs(d) < 1e-9) {
      var mx = 2 * bx - (ax + cx) / 2, my = 2 * by - (ay + cy) / 2;
      var out = [];
      for (var k = 0; k <= 32; k++) {
        var t = k / 32, u = 1 - t;
        out.push([u * u * ax + 2 * u * t * mx + t * t * cx, u * u * ay + 2 * u * t * my + t * t * cy]);
      }
      return out;
    }
    var ux = ((ax * ax + ay * ay) * (by - cy) + (bx * bx + by * by) * (cy - ay) + (cx * cx + cy * cy) * (ay - by)) / d;
    var uy = ((ax * ax + ay * ay) * (cx - bx) + (bx * bx + by * by) * (ax - cx) + (cx * cx + cy * cy) * (bx - ax)) / d;
    var r = Math.hypot(ax - ux, ay - uy);
    var a0 = Math.atan2(ay - uy, ax - ux);
    var a1 = Math.atan2(by - uy, bx - ux);
    var a2 = Math.atan2(cy - uy, cx - ux);
    var TAU = Math.PI * 2;
    var d02 = ((a2 - a0) % TAU + TAU) % TAU;
    var d01 = ((a1 - a0) % TAU + TAU) % TAU;
    var sweep = d01 <= d02 ? d02 : d02 - TAU;
    var pts = [];
    for (var i = 0; i <= 32; i++) {
      var a = a0 + sweep * (i / 32);
      pts.push([ux + r * Math.cos(a), uy + r * Math.sin(a)]);
    }
    return pts;
  }

  /* 有宽度的曲线 → 闭合轮廓。笔幅相对曲线尺寸很大时退化成支撑点凸包
     （sleepy 这种粗短笔划），否则用两侧偏移 + 圆头端帽（happy/confused）。
     笑眼是小半径粗拱，必须保留内侧的小孔，凸包会把它填死成黑豆。 */
  function strokeRaw(shape) {
    var P = curvePts(shape.p0, shape.p1, shape.p2);
    var wid = shape.wid, i, k;
    var cx = 0, cy = 0, maxR = 0;
    for (i = 0; i < P.length; i++) { cx += P[i][0]; cy += P[i][1]; }
    cx /= P.length; cy /= P.length;
    for (i = 0; i < P.length; i++) {
      maxR = Math.max(maxR, Math.hypot(P[i][0] - cx, P[i][1] - cy));
    }
    if (wid > 1.05 * maxR) {
      var hull = [], M = 96;
      for (k = 0; k < M; k++) {
        var th = (k / M) * Math.PI * 2;
        var dx = Math.cos(th), dy = Math.sin(th);
        var best = -Infinity, bx = 0, by = 0;
        for (i = 0; i < P.length; i++) {
          var d = P[i][0] * dx + P[i][1] * dy;
          if (d > best) { best = d; bx = P[i][0]; by = P[i][1]; }
        }
        hull.push([bx + dx * wid, by + dy * wid]);
      }
      return hull;
    }
    var L = [], R = [], NRM = [];
    for (i = 0; i < P.length; i++) {
      var a = P[Math.max(0, i - 1)], b = P[Math.min(P.length - 1, i + 1)];
      var tx = b[0] - a[0], ty = b[1] - a[1];
      var len = Math.hypot(tx, ty) || 1;
      var nx = -ty / len, ny = tx / len;
      NRM.push([nx, ny]);
      L.push([P[i][0] + nx * wid, P[i][1] + ny * wid]);
      R.push([P[i][0] - nx * wid, P[i][1] - ny * wid]);
    }
    function cap(c, a0) {
      var pts = [];
      for (var j = 1; j < 10; j++) {
        var th = a0 - Math.PI * (j / 10);
        pts.push([c[0] + Math.cos(th) * wid, c[1] + Math.sin(th) * wid]);
      }
      return pts;
    }
    var last = P.length - 1;
    return L.concat(
      cap(P[last], Math.atan2(NRM[last][1], NRM[last][0])),
      R.slice().reverse(),
      cap(P[0], Math.atan2(-NRM[0][1], -NRM[0][0]))
    );
  }

  function signedArea(pts) {
    var s = 0;
    for (var i = 0; i < pts.length; i++) {
      var a = pts[i], b = pts[(i + 1) % pts.length];
      s += a[0] * b[1] - b[0] * a[1];
    }
    return s / 2;
  }

  // 统一起点（最高点）并按弧长重采样到 n 点——不同表情的 morph 靠这个对齐
  function resample(raw, n) {
    if (signedArea(raw) < 0) raw = raw.slice().reverse();
    var m = raw.length, start = 0, i;
    for (i = 1; i < m; i++) {
      if (raw[i][1] > raw[start][1] + 1e-9 ||
        (Math.abs(raw[i][1] - raw[start][1]) < 1e-9 && raw[i][0] > raw[start][0])) start = i;
    }
    var ord = raw.slice(start).concat(raw.slice(0, start));
    var lens = [], total = 0;
    for (i = 0; i < m; i++) {
      var a = ord[i], b = ord[(i + 1) % m];
      var d = Math.hypot(b[0] - a[0], b[1] - a[1]);
      lens.push(d); total += d;
    }
    var out = [], seg = 0, acc = 0;
    for (var j = 0; j < n; j++) {
      var target = (total * j) / n;
      while (acc + lens[seg] < target && seg < m - 1) { acc += lens[seg]; seg++; }
      var u = lens[seg] > 1e-9 ? (target - acc) / lens[seg] : 0;
      var p = ord[seg], q = ord[(seg + 1) % m];
      out.push([p[0] + (q[0] - p[0]) * u, p[1] + (q[1] - p[1]) * u]);
    }
    return out;
  }

  // 把归一化形状映射到某只眼的局部尺寸（sx = 眼宽/0.26，sy = 眼高/0.57）
  function shapeToLocal(shape, sx, sy, n) {
    var raw = shape.type === 'cap' ? capRaw(shape.w, shape.h)
      : shape.type === 'oval' ? ovalRaw(shape.rx, shape.ry)
        : strokeRaw(shape);
    var rot = (shape.rot || 0) * Math.PI / 180, c = Math.cos(rot), s = Math.sin(rot);
    var off = shape.off || [0, 0];
    var pts = raw.map(function (p) {
      var x = p[0] * c - p[1] * s, y = p[0] * s + p[1] * c;
      return [(x + off[0]) * sx, (y + off[1]) * sy];
    });
    return resample(pts, n || 96);
  }

  function shapeFor(name, side) {
    var shapes = SHAPES[name] || SHAPES.neutral;
    return shapes.length > 1 ? shapes[side === 'l' ? 0 : 1] : shapes[0];
  }

  // 9 表达 × 2 眼：眼局部坐标（grid px，y 向上）
  function expressionOutlines(name, n) {
    var boxes = eyeBoxes();
    return boxes.map(function (e, i) {
      var shape = shapeFor(name, e.side);
      return shapeToLocal(shape, e.w / NORM.w, e.h / NORM.h, n);
    });
  }

  // 映射到 240 网格（y 向下）：X = 眼心 + lx，Y = 120 − ly
  function eyeGridOutline(name, side, n) {
    var i = side === 'l' ? 0 : 1;
    var e = eyeBoxes()[i];
    return expressionOutlines(name, n || 64)[i].map(function (p) {
      return [e.x + p[0], AVATAR.cy - p[1]];
    });
  }

  /* 生成单个眼层（240×240 资产）：cap/oval 用精确图元，stroke 用闭合多边形。
     cap 的旋转取负（局部 y 向上 → SVG y 向下）。 */
  function eyeLayerSvg(name, side, fill) {
    var e = eyeBoxes()[side === 'l' ? 0 : 1];
    var shape = shapeFor(name, side);
    var sx = e.w / NORM.w, sy = e.h / NORM.h;
    var off = shape.off || [0, 0];
    var cx = r2(e.x + off[0] * sx), cy = r2(AVATAR.cy - off[1] * sy);
    var rot = -(shape.rot || 0);
    if (shape.type === 'cap') {
      var w = r2(shape.w * sx), h = r2(shape.h * sy);
      return '<g transform="translate(' + cx + ' ' + cy + ') rotate(' + rot + ')">' +
        '<rect x="' + r2(-w / 2) + '" y="' + r2(-h / 2) + '" width="' + w + '" height="' + h +
        '" rx="' + r2(Math.min(w, h) / 2) + '" fill="' + fill + '"/></g>';
    }
    if (shape.type === 'oval') {
      return '<g transform="translate(' + cx + ' ' + cy + ') rotate(' + rot + ')">' +
        '<ellipse rx="' + r2(shape.rx * sx) + '" ry="' + r2(shape.ry * sy) + '" fill="' + fill + '"/></g>';
    }
    var pts = eyeGridOutline(name, side, 64);
    var d = pts.map(function (p, i) { return (i ? 'L' : 'M') + r2(p[0]) + ' ' + r2(p[1]); }).join(' ') + ' Z';
    return '<path d="' + d + '" fill="' + fill + '"/>';
  }

  function eyeGroupSvg(name, fill) {
    return eyeLayerSvg(name, 'l', fill) + eyeLayerSvg(name, 'r', fill);
  }

  // 眼层轴心 = 该眼外接盒的底边中点（预览页眨眼就是下缘不动）
  function eyeOrigin(name, side, n) {
    var pts = eyeGridOutline(name, side, n || 64);
    var minX = Infinity, maxX = -Infinity, maxY = -Infinity;
    pts.forEach(function (p) {
      if (p[0] < minX) minX = p[0];
      if (p[0] > maxX) maxX = p[0];
      if (p[1] > maxY) maxY = p[1];
    });
    return {
      xPct: +(((minX + maxX) / 2 / 240) * 100).toFixed(4),
      yPct: +((maxY / 240) * 100).toFixed(4)
    };
  }

  /* ---------------- 静态品牌符号（LunaMark，2026-10-23） ----------------
     同一张脸的「单路径负空间」画法：头圆 + 两只胶囊眼各是一条子路径，拼成一个 d。
     solid 用 fill-rule="evenodd" 把眼挖成**透明洞**——眼睛透出所在表面，不再需要
     cut 底色；outline 用同一条 d 描边，头与眼洞同时成为线稿。
     活体 Buddy 仍走分层 cut 眼：眨眼/视线需要独立眼层，不能并成一条路径。
     坐标一律保留 3 位小数：90° 弧的弦长是 √2·r、180° 弧是 2r，端点各自取整会让
     渲染器按规范放大半径，两侧盖帽出现细微不对称。 */

  /* 静态 3/4 朝向（LunaMark 水印变体用）：引擎 Idle_D 的静息帧，即预览页说的
     「logo 那种右上 3/4 脸」。引擎的 rot 关键帧是 [pitch, yaw, roll] 后再取反
     （engine.ts: pitch = −r[0]、roll = −r[2]），静息帧 [−15.896, 33.293, 25.385]
     → 下面这组。改朝向就改这三个数：生成脚本会重投影并断言眼洞仍在头内。 */
  var MARK_LOOK = { pitch: 15.896, yaw: 33.293, roll: -25.385 };
  var D2R = Math.PI / 180;

  // 与引擎同序：Rz(roll)·Ry(yaw)·Rx(pitch)（预览页 turn3 的 1:1 移植）
  function turn3(v, pose) {
    var p = pose.pitch * D2R, y = pose.yaw * D2R, r = pose.roll * D2R;
    var c = Math.cos(p), s = Math.sin(p);
    var x1 = v[0], y1 = v[1] * c - v[2] * s, z1 = v[1] * s + v[2] * c;
    c = Math.cos(y); s = Math.sin(y);
    var x2 = x1 * c + z1 * s, y2 = y1, z2 = -x1 * s + z1 * c;
    c = Math.cos(r); s = Math.sin(r);
    return [x2 * c - y2 * s, x2 * s + y2 * c, z2];
  }

  /* 正脸眼睛 → 转过 pose 的球面投影点（240 网格，屏幕坐标 y 向下）。
     映射照引擎：眼心先贴到球面，局部轮廓按「局部长度当弧长/R」走指数映射，
     贴片因此永远落在头圆以内（预览页 drawEyes 同款）；两只眼因透视自然
     不等大、各自斜倒。 */
  function markLookEyePoints(pose) {
    var r = AVATAR.r;
    return eyeBoxes().map(function (e) {
      var dx = e.x - AVATAR.cx, dy = e.y - AVATAR.cy;
      var z = Math.sqrt(Math.max(1, r * r - dx * dx - dy * dy));
      var normal = [dx / r, dy / r, z / r];
      var tangent = function (tx, ty) {
        var k = tx * normal[0] + ty * normal[1];
        var t = [tx - k * normal[0], ty - k * normal[1], -k * normal[2]];
        var len = Math.sqrt(t[0] * t[0] + t[1] * t[1] + t[2] * t[2]) || 1;
        return [t[0] / len, t[1] / len, t[2] / len];
      };
      var c = turn3([dx, dy, z], pose);
      var cl = Math.sqrt(c[0] * c[0] + c[1] * c[1] + c[2] * c[2]) || 1;
      var cx = c[0] / cl, cy = c[1] / cl;
      var u = turn3(tangent(0, -1), pose); // 长轴（引擎局部 y 向上）
      var v = turn3(tangent(1, 0), pose);  // 短轴
      return capRaw(e.w, e.h).map(function (p) {
        var a = p[1] / r, b = p[0] / r;
        var h = Math.sqrt(a * a + b * b);
        var sn = h > 1e-6 ? Math.sin(h) / h : 1, cs = Math.cos(h);
        var px = cs * cx + sn * (a * u[0] + b * v[0]);
        var py = cs * cy + sn * (a * u[1] + b * v[1]);
        return [AVATAR.cx + px * r, AVATAR.cy + py * r];
      });
    });
  }

  function markPath(pose) {
    var f = function (v) { return Math.round(v * 1000) / 1000; };
    var arc = function (r, p) {
      return 'A ' + f(r) + ' ' + f(r) + ' 0 0 1 ' + f(p[0]) + ' ' + f(p[1]);
    };
    // 正脸（ADR-0021）：竖直胶囊，无倾角
    var pt = function (x, y, e) {
      return [e.x + x, e.y + y];
    };
    // 头：两段 180° 弧（一条弧起终点相同会被当 no-op）
    var head = 'M ' + f(AVATAR.cx - AVATAR.r) + ' ' + f(AVATAR.cy) +
      ' A ' + f(AVATAR.r) + ' ' + f(AVATAR.r) + ' 0 0 1 ' +
      f(AVATAR.cx + AVATAR.r) + ' ' + f(AVATAR.cy) +
      ' A ' + f(AVATAR.r) + ' ' + f(AVATAR.r) + ' 0 0 1 ' +
      f(AVATAR.cx - AVATAR.r) + ' ' + f(AVATAR.cy) + ' Z';
    // 眼：局部竖直胶囊（4 段 90° 弧，flag 统一 large-arc=0 / sweep=1），
    // 逐点旋转 + 平移到眼心；rx=ry，A 的 x-axis-rotation 写 0。
    var eye = function (e) {
      var r = e.w / 2;
      var hy = e.h / 2 - r;
      var top = pt(0, -e.h / 2, e);
      var rightTop = pt(r, -hy, e);
      var rightBottom = pt(r, hy, e);
      var bottom = pt(0, e.h / 2, e);
      var leftBottom = pt(-r, hy, e);
      var leftTop = pt(-r, -hy, e);
      return 'M ' + f(top[0]) + ' ' + f(top[1]) +
        ' ' + arc(r, rightTop) +
        ' L ' + f(rightBottom[0]) + ' ' + f(rightBottom[1]) +
        ' ' + arc(r, bottom) +
        ' ' + arc(r, leftBottom) +
        ' L ' + f(leftTop[0]) + ' ' + f(leftTop[1]) +
        ' ' + arc(r, top) + ' Z';
    };
    if (pose) {
      // 3/4 朝向：投影后是闭合多边形（不再能用 4 段弧解析表达），逐点 L 连
      var poly = function (pts) {
        return 'M ' + pts.map(function (p, i) {
          return (i ? 'L' : '') + f(p[0]) + ' ' + f(p[1]);
        }).join(' ') + ' Z';
      };
      return head + ' ' + markLookEyePoints(pose).map(poly).join(' ');
    }
    var boxes = eyeBoxes();
    return head + ' ' + eye(boxes[0]) + ' ' + eye(boxes[1]);
  }

  /* ---------------- 环度量（断言用，不参与出图） ----------------
     环与头同心 ⇒ 度量退化成一句话：半径 × 缩放倍数 ≤ 半帧（120）就画得下。
     生成脚本据此断言「炸圈/声呐在 WXSS 的最大缩放下不出画框」——改几何忘了同步，
     断言立刻抛错，不靠肉眼。 */
  function ringExtent(pad, sw) {
    var r = AVATAR.r + pad + sw / 2;      // 含描边外缘
    return { r: r, clipScale: 120 / r };
  }

  /* ---------------- 字母款：字母自己当脸 ----------------
     同一套 240 网格、同一双眼睛。字身是几何画的（字母本身就是画面），
     两条词标按字排。入场用 .p 零件编排：笔画先到位，眼睛最后落下。 */

  // 几何小写 luna：四个字母各是一段原始笔画（x 高 78、竖笔 20、基线 170）。
  // 拆出 raw shapes 供小程序生成脚本按层消费（lunaLetters 是带开机零件的包装版）。
  function lunaLetterShapes(W) {
    var s = ' stroke="' + W + '" stroke-width="20" fill="none" stroke-linecap="butt"';
    return [
      '<rect x="66" y="52" width="20" height="118" fill="' + W + '"/>',            // l
      '<path d="M 116 92 V 129 A 31 31 0 0 0 178 129 M 178 92 V 170"' + s + '/>',   // u
      '<path d="M 227 92 V 170 M 227 133 A 31 31 0 0 1 289 133 V 170"' + s + '/>',  // n
      '<circle cx="372" cy="129" r="31"' + s + '/><path d="M 403 92 V 170"' + s + '/>' // a
    ];
  }
  function lunaLetters() {
    return lunaLetterShapes(C.ink).map(function (shape, i) { return part(i, shape, 'y', 22); }).join('');
  }

  /* ---------------- 词标：u 上那两点是圆眼睛（定稿唯一一条） ---------------- */
  var WORDS = [
    {
      id: 'W', slug: 'luna', name: 'lüna', en: 'Wordmark',
      note: 'u 上那两点是圆眼睛：字母一个个打出来，眼睛最后睁开。⚠️ 拼出来像 lüna，别出现在教拼写的页面里。',
      bg: C.white, light: true, viewBox: '0 0 480 220',
      draw: function () {
        return lunaLetters() +
          part(4, eyesGroup(at(133, 70, 0, dot(10, C.ink, 'l')) + at(161, 70, 0, dot(10, C.ink, 'r'))), 'pop');
      }
    }
  ];

  /* ---------------- 出图 ---------------- */

  function renderSVG(item, opts) {
    opts = opts || {};
    var vb = item.viewBox || '0 0 240 240';
    var size = vb.split(' ').slice(2);           // [w, h]
    var extraStyle = opts.standalone
      ? '<style>' +
          '.body{transform-box:view-box;transform-origin:var(--ox) var(--oy);transform:rotate(var(--tilt))}' +
          '.eyes{transform:translate(var(--gx,0),var(--gy,0))}' +
          '.eye{transform-box:fill-box;transform-origin:center;animation:lunaBlink 4.6s cubic-bezier(.5,0,.5,1) infinite}' +
          '@keyframes lunaBlink{0%,92%,100%{transform:scaleY(1)}95%{transform:scaleY(.06)}}' +
          // 单文件也自己开一次机：笔画先到位，眼睛最后落下
          '.p{transform-box:fill-box;transform-origin:center;animation:lunaBoot .62s cubic-bezier(.22,1,.36,1) both;' +
            'animation-delay:calc(var(--i,0) * 90ms)}' +
          '.p.grow{transform-origin:center bottom;animation-name:lunaGrow}' +
          '.p.pop{animation-name:lunaPop}' +
          '@keyframes lunaBoot{from{opacity:0;transform:translate(var(--fx,0),var(--fy,0))}to{opacity:1;transform:none}}' +
          '@keyframes lunaGrow{from{opacity:0;transform:scaleY(.12)}to{opacity:1;transform:none}}' +
          '@keyframes lunaPop{from{opacity:0;transform:scale(.2)}to{opacity:1;transform:none}}' +
          '@media (prefers-reduced-motion:reduce){.eye,.p{animation:none}}' +
        '</style>'
      : '';

    return '<svg viewBox="' + vb + '" class="bot bot-' + item.id + '" data-state="' + (opts.state || 'idle') + '"' +
      (opts.standalone ? ' xmlns="http://www.w3.org/2000/svg" width="' + size[0] + '" height="' + size[1] + '"' : '') +
      ' role="img" aria-label="Luna · ' + item.name + '"' +
      ' style="--tilt:' + (item.tilt || 0) + 'deg;--ox:' + (item.ox || 120) + 'px;--oy:' + (item.oy || 120) + 'px">' +
      extraStyle +
      '<rect x="0" y="0" width="' + size[0] + '" height="' + size[1] + '" fill="' + item.bg + '"/>' +
      '<g class="body">' + item.draw() + '</g>' +
      (item.bead ? bead(item.bead[0], item.bead[1], item.bead[2], item.bead[3]) : '') +
      '</svg>';
  }

  var API = {
    BOTS: BOTS, WORDS: WORDS, COLORS: C, AVATAR: AVATAR, renderSVG: renderSVG,
    // 分层构建件出口（2026-10 增补；2026-10-06 三改随状态灯退场移除 bead——
    // 旧生成脚本若还引用它会直接报错，而不是静默出圆点）：
    // scripts/gen-robot-svg.mjs 用它们按层组装小程序资产——几何保持单一真源，
    // 改数字即改脸，重跑生成脚本即可同步到 App。
    build: {
      capsule: capsule, arc: arc, dot: dot, at: at,
      headRing: headRing, ringExtent: ringExtent, eyeBoxes: eyeBoxes, markPath: markPath,
      MARK_LOOK: MARK_LOOK, markLookEyePoints: markLookEyePoints,
      // 正脸 9 表情（ADR-0021）：预览页与生成器共用同一份形状表/轮廓/出图层
      EXPRESSIONS: EXPRESSIONS, SHAPES: SHAPES, NORM: NORM,
      capRaw: capRaw, ovalRaw: ovalRaw, curvePts: curvePts, strokeRaw: strokeRaw,
      signedArea: signedArea, resample: resample, shapeToLocal: shapeToLocal,
      expressionOutlines: expressionOutlines, eyeGridOutline: eyeGridOutline,
      eyeOrigin: eyeOrigin, eyeLayerSvg: eyeLayerSvg, eyeGroupSvg: eyeGroupSvg,
      // 换形符号（2026-10-22）：三点的单颗/整组、叹号、绶带+纸屑
      swapDot: swapDot, dotsMark: dotsMark, alertMark: alertMark, celebrateMark: celebrateMark,
      part: part, eyesGroup: eyesGroup, lunaLetters: lunaLetters, lunaLetterShapes: lunaLetterShapes
    }
  };
  global.LUNA_BOTS = API;
  if (typeof module !== 'undefined' && module.exports) module.exports = API;
})(typeof window !== 'undefined' ? window : globalThis);
