'use strict';

// ─── helpers ────────────────────────────────────────────────────────────────
const $ = (id) => document.getElementById(id);
const clamp = (v, a, b) => (v < a ? a : v > b ? b : v);
const lerp = (a, b, t) => a + (b - a) * t;
const easeOutCubic = (t) => 1 - Math.pow(1 - t, 3);
const FONT = "'JetBrains Mono', ui-monospace, monospace";
const reduceMotion = matchMedia('(prefers-reduced-motion: reduce)').matches;

function fmtBytes(n) {
  n = Number(n) || 0;
  if (n < 1024) return n.toFixed(0) + ' B';
  const u = ['KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < u.length - 1);
  return n.toFixed(1) + ' ' + u[i];
}
const fmtBps = (n) => fmtBytes(n) + '/s';
function fmtEta(s) {
  if (!isFinite(s) || s <= 0) return '--:--';
  s = Math.round(s);
  return String(Math.floor(s / 60)).padStart(2, '0') + ':' + String(s % 60).padStart(2, '0');
}
function shortDigest(d) {
  const h = (d || '').split(':')[1] || d || '';
  return h.slice(0, 9) + '…';
}
function escapeHtml(s) {
  return String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
}

function parseHex(hex) {
  hex = (hex || '').trim().replace('#', '');
  return [parseInt(hex.slice(0, 2), 16), parseInt(hex.slice(2, 4), 16), parseInt(hex.slice(4, 6), 16)];
}
const rgba = (c, a) => `rgba(${c[0]},${c[1]},${c[2]},${a})`;
const mix = (a, b, t) => [Math.round(lerp(a[0], b[0], t)), Math.round(lerp(a[1], b[1], t)), Math.round(lerp(a[2], b[2], t))];

const cs = getComputedStyle(document.documentElement);
const COL = {
  bg: parseHex(cs.getPropertyValue('--bg')),
  bgInset: parseHex(cs.getPropertyValue('--bg-inset')),
  grid: parseHex(cs.getPropertyValue('--grid')),
  phos: parseHex(cs.getPropertyValue('--phos')),
  phosDim: parseHex(cs.getPropertyValue('--phos-dim')),
  cyan: parseHex(cs.getPropertyValue('--cyan')),
  amber: parseHex(cs.getPropertyValue('--amber')),
  amberHot: parseHex(cs.getPropertyValue('--amber-hot')),
  inkDim: parseHex(cs.getPropertyValue('--ink-dim')),
};

function roundRect(ctx, x, y, w, h, r) {
  if (w < 2 * r) r = w / 2;
  if (h < 2 * r) r = h / 2;
  ctx.beginPath();
  ctx.moveTo(x + r, y);
  ctx.arcTo(x + w, y, x + w, y + h, r);
  ctx.arcTo(x + w, y + h, x, y + h, r);
  ctx.arcTo(x, y + h, x, y, r);
  ctx.arcTo(x, y, x + w, y, r);
  ctx.closePath();
}

// ─── state ───────────────────────────────────────────────────────────────────
let _knownJobId = null; // tracks the last job we connected to so the poller doesn't re-open it

const S = {
  kind: null, ref: '', name: '',
  packets: [], byIndex: new Map(),
  total: 0, received: 0, bps: 0, eta: 0, served: 0, platforms: [],
  dispRecv: 0, dispBps: 0,
  chart: new Float32Array(256), chartHead: 0, chartLen: 256, chartMax: 1,
  flashes: [],
  running: false, clock: 0, dash: 0, lastTime: 0, _activeLanes: 0,
  _lastP: 0, _lastRecvP: 0,
};

function packColor(i, cached) {
  if (cached) return COL.phosDim;
  return mix(COL.phos, COL.cyan, (i % 6) / 5);
}

// ─── canvas plumbing ──────────────────────────────────────────────────────────
function setupCanvas(canvas) {
  const o = { canvas, ctx: canvas.getContext('2d'), w: 1, h: 1, dpr: 1, gridDirty: true, grid: null };
  function resize() {
    const r = canvas.getBoundingClientRect();
    const dpr = Math.max(1, Math.min(2.5, window.devicePixelRatio || 1));
    o.w = Math.max(1, Math.floor(r.width));
    o.h = Math.max(1, Math.floor(r.height));
    o.dpr = dpr;
    canvas.width = Math.floor(o.w * dpr);
    canvas.height = Math.floor(o.h * dpr);
    o.ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    o.gridDirty = true;
  }
  new ResizeObserver(resize).observe(canvas);
  resize();
  return o;
}

function buildGrid(o) {
  const g = document.createElement('canvas');
  g.width = o.canvas.width;
  g.height = o.canvas.height;
  const c = g.getContext('2d');
  c.setTransform(o.dpr, 0, 0, o.dpr, 0, 0);
  c.fillStyle = rgba(COL.bg, 1);
  c.fillRect(0, 0, o.w, o.h);
  c.strokeStyle = rgba(COL.grid, 0.55);
  c.lineWidth = 1;
  c.beginPath();
  const step = 26;
  for (let x = 0; x <= o.w; x += step) { c.moveTo(x + 0.5, 0); c.lineTo(x + 0.5, o.h); }
  for (let y = 0; y <= o.h; y += step) { c.moveTo(0, y + 0.5); c.lineTo(o.w, y + 0.5); }
  c.stroke();
  o.grid = g;
  o.gridDirty = false;
}

let stage, chart;

// ─── transit render ─────────────────────────────────────────────────────────
function accentRGB() { return S.kind === 'unpack' ? COL.amber : COL.phos; }

function renderStage(dt) {
  const o = stage;
  if (o.gridDirty) buildGrid(o);
  const { ctx, w, h } = o;
  ctx.setTransform(o.dpr, 0, 0, o.dpr, 0, 0);
  ctx.drawImage(o.grid, 0, 0, w, h);

  const accent = accentRGB();
  const outsideX = w * 0.18, insideX = w * 0.82;
  const xStart = outsideX + 8, xEnd = insideX - 8;
  const top = 22, bot = h - 24, span = Math.max(1, bot - top);
  const n = S.packets.length || 1;
  const laneH = span / n;

  // THE GAP
  ctx.fillStyle = rgba(COL.bgInset, 0.75);
  ctx.fillRect(outsideX, top - 8, insideX - outsideX, span + 16);
  drawRim(ctx, outsideX, insideX, top - 8, bot + 8, accent, dt);
  drawScan(ctx, outsideX, insideX, top - 8, span + 16);

  let active = 0;
  for (const p of S.packets) {
    const y = top + (p.i + 0.5) * laneH;
    const target = p.size > 0 ? clamp(p.received / p.size, 0, 1) : (p.done ? 1 : 0);
    const k = reduceMotion ? 1 : 1 - Math.exp(-7 * dt);
    p.t += (target - p.t) * k;
    if (p.done && p.t < 1) p.t += (1 - p.t) * (reduceMotion ? 1 : 1 - Math.exp(-9 * dt));
    p.glow *= Math.exp(-dt * 2.2);
    const x = lerp(xStart, xEnd, easeOutCubic(p.t));
    if (!p.done && p.t > 0.002 && p.t < 0.985) active++;
    p.trail.push(x);
    if (p.trail.length > 7) p.trail.shift();
    drawPacket(ctx, p, x, y, laneH, accent);
    if (p.done && !p.slammed && p.t > 0.97) {
      p.slammed = true; p.slam = 1;
      S.flashes.push({ x: xEnd, y, life: 1 });
    }
    if (p.slam > 0) p.slam = Math.max(0, p.slam - dt * 4);
  }

  // bracket / rack decoration
  ctx.strokeStyle = rgba(accent, 0.35);
  ctx.lineWidth = 1;
  ctx.beginPath();
  ctx.moveTo(outsideX - 5, top - 8); ctx.lineTo(outsideX - 10, top - 8); ctx.lineTo(outsideX - 10, bot + 8); ctx.lineTo(outsideX - 5, bot + 8);
  ctx.moveTo(insideX + 5, top - 8); ctx.lineTo(insideX + 10, top - 8); ctx.lineTo(insideX + 10, bot + 8); ctx.lineTo(insideX + 5, bot + 8);
  ctx.stroke();

  for (const f of S.flashes) { f.life -= dt * 2.2; drawFlash(ctx, f); }
  S.flashes = S.flashes.filter((f) => f.life > 0);

  drawLabels(ctx, outsideX, insideX, w, h);
  S._activeLanes = active;
}

function drawPacket(ctx, p, x, y, laneH, accent) {
  const ph = Math.min(13, Math.max(7, laneH * 0.55));
  const wgt = p.size > 0 ? Math.log2(p.size / 1e6 + 2) : 1;
  const pw = clamp(10 + 4 * wgt, 10, 26);
  const col = p.cached ? COL.phosDim : (p.rgb || accent);

  for (let i = 0; i < p.trail.length - 1; i++) {
    const tx = p.trail[i];
    const a = (i / p.trail.length) * 0.18 * (0.4 + clamp(p.glow, 0, 1) * 0.6);
    ctx.fillStyle = rgba(col, a);
    roundRect(ctx, tx - pw / 2, y - ph / 2, pw, ph, 2); ctx.fill();
  }

  const bodyA = p.done ? 0.92 : (0.42 + 0.5 * clamp(p.glow, 0, 1));
  ctx.fillStyle = rgba(col, bodyA);
  roundRect(ctx, x - pw / 2, y - ph / 2, pw, ph, 2); ctx.fill();
  ctx.strokeStyle = rgba(p.done ? COL.phos : col, 0.7);
  ctx.lineWidth = 1; ctx.stroke();

  const pr = p.size > 0 ? clamp(p.received / p.size, 0, 1) : (p.done ? 1 : 0);
  if (pr > 0) {
    ctx.fillStyle = rgba(p.done ? COL.phos : mix(col, COL.amberHot, pr * 0.4), 0.95);
    roundRect(ctx, x - pw / 2 + 1.5, y + ph / 2 - 3, (pw - 3) * pr, 2, 1); ctx.fill();
  }

  if (laneH >= 17) {
    ctx.fillStyle = rgba(COL.amber, 0.65);
    ctx.font = '9px ' + FONT;
    ctx.textAlign = 'center';
    ctx.fillText(p.label.length > 16 ? p.label.slice(0, 15) + '…' : p.label, x, y - ph / 2 - 3);
  }

  if (p.slam > 0) {
    ctx.strokeStyle = rgba(COL.amberHot, p.slam * 0.8);
    ctx.lineWidth = 2;
    ctx.beginPath(); ctx.arc(x, y, (1 - p.slam) * 16 + 4, 0, 7); ctx.stroke();
  }
}

function drawScan(ctx, x0, x1, y0, span) {
  const period = S.kind === 'unpack' ? 1.8 : 3.2;
  const y = y0 + ((S.clock % period) / period) * span;
  const accent = accentRGB();
  const grad = ctx.createLinearGradient(0, y - 11, 0, y + 2);
  grad.addColorStop(0, rgba(accent, 0));
  grad.addColorStop(1, rgba(accent, 0.45));
  ctx.fillStyle = grad;
  ctx.fillRect(x0, y - 11, x1 - x0, 11);
  ctx.strokeStyle = rgba(accent, 0.7);
  ctx.lineWidth = 1;
  ctx.beginPath(); ctx.moveTo(x0, y + 0.5); ctx.lineTo(x1, y + 0.5); ctx.stroke();
}

function drawRim(ctx, x0, x1, y0, y1, accent, dt) {
  const dir = S.kind === 'unpack' ? -1 : 1;
  S.dash += dir * clamp(S.bps / (50 * 1024 * 1024), 0.08, 1) * 40 * dt;
  ctx.save();
  ctx.setLineDash([4, 5]);
  ctx.lineDashOffset = S.dash;
  ctx.strokeStyle = rgba(accent, 0.5);
  ctx.lineWidth = 1;
  ctx.beginPath();
  ctx.moveTo(x0 + 0.5, y0); ctx.lineTo(x0 + 0.5, y1);
  ctx.moveTo(x1 + 0.5, y0); ctx.lineTo(x1 + 0.5, y1);
  ctx.stroke();
  ctx.restore();
}

function drawFlash(ctx, f) {
  const r = (1 - f.life) * 22 + 3;
  ctx.strokeStyle = rgba(COL.amberHot, f.life * 0.7);
  ctx.lineWidth = 2;
  ctx.beginPath(); ctx.arc(f.x, f.y, r, 0, 7); ctx.stroke();
}

function drawLabels(ctx, x0, x1, w, h) {
  ctx.font = '10px ' + FONT;
  ctx.textAlign = 'left';
  ctx.fillStyle = rgba(COL.inkDim, 0.55);
  ctx.fillText('OUTSIDE · upstream', 6, h - 7);
  ctx.textAlign = 'center';
  ctx.fillStyle = rgba(accentRGB(), 0.45);
  ctx.fillText('▲ AIRGAP ▲', (x0 + x1) / 2, h - 7);
  ctx.textAlign = 'right';
  ctx.fillStyle = rgba(COL.inkDim, 0.55);
  ctx.fillText(S.kind === 'unpack' ? 'INSIDE · :5000' : 'INSIDE · ./store', w - 6, h - 7);
}

// ─── throughput chart ─────────────────────────────────────────────────────────
function pushChart(v) {
  S.chart[S.chartHead] = Math.max(0, v || 0);
  S.chartHead = (S.chartHead + 1) % S.chartLen;
}

function renderChart(dt) {
  const o = chart;
  const { ctx, w, h } = o;
  ctx.setTransform(o.dpr, 0, 0, o.dpr, 0, 0);
  ctx.fillStyle = rgba(COL.bgInset, 1);
  ctx.fillRect(0, 0, w, h);

  S.chartMax = Math.max(1, S.chartMax * Math.pow(0.96, dt));
  let mx = 1;
  for (let i = 0; i < S.chartLen; i++) if (S.chart[i] > mx) mx = S.chart[i];
  S.chartMax = Math.max(S.chartMax, mx);

  const accent = accentRGB();
  const N = S.chartLen;
  const yOf = (idx) => h - 5 - (S.chart[idx] / S.chartMax) * (h - 12);

  ctx.beginPath();
  for (let k = 0; k < N; k++) {
    const idx = (S.chartHead + k) % N;
    const x = (k / (N - 1)) * w;
    if (k === 0) ctx.moveTo(x, yOf(idx)); else ctx.lineTo(x, yOf(idx));
  }
  ctx.lineTo(w, h); ctx.lineTo(0, h); ctx.closePath();
  const grad = ctx.createLinearGradient(0, 0, 0, h);
  grad.addColorStop(0, rgba(accent, 0.42));
  grad.addColorStop(1, rgba(accent, 0.02));
  ctx.fillStyle = grad; ctx.fill();

  ctx.beginPath();
  for (let k = 0; k < N; k++) {
    const idx = (S.chartHead + k) % N;
    const x = (k / (N - 1)) * w;
    if (k === 0) ctx.moveTo(x, yOf(idx)); else ctx.lineTo(x, yOf(idx));
  }
  ctx.strokeStyle = rgba(accent, 0.9);
  ctx.lineWidth = 1.4;
  ctx.stroke();

  const lastIdx = (S.chartHead + N - 1) % N;
  ctx.fillStyle = rgba(accent, 1);
  ctx.beginPath(); ctx.arc(w - 1, yOf(lastIdx), 2.5, 0, 7); ctx.fill();
}

// ─── animation loop ───────────────────────────────────────────────────────────
function frame(ts) {
  const dt = Math.min(0.05, (ts - (S.lastTime || ts)) / 1000);
  S.lastTime = ts;
  if (!document.hidden) {
    S.clock += dt;
    renderStage(dt);
    renderChart(dt);
  }
  tickCounters(dt);
  requestAnimationFrame(frame);
}

function tickCounters(dt) {
  S.dispRecv += (S.received - S.dispRecv) * 0.18;
  S.dispBps += (S.bps - S.dispBps) * 0.18;
  if (Math.abs(S.dispRecv - S.received) < 1) S.dispRecv = S.received;
  $('mRecv').textContent = fmtBytes(S.dispRecv);
  $('mTotal').textContent = fmtBytes(S.total);
  const doneCount = S.packets.reduce((a, p) => a + (p.done ? 1 : 0), 0);
  if (S.kind === 'unpack') {
    $('mBlobsLabel').textContent = 'SERVED';
    $('mBlobs').textContent = S.served + '/' + S.packets.length;
  } else {
    $('mBlobsLabel').textContent = 'BLOBS';
    $('mBlobs').textContent = doneCount + '/' + S.packets.length;
  }
  $('mEta').textContent = fmtEta(S.eta);
  $('mBps').textContent = fmtBps(S.dispBps);
  $('mLanes').textContent = S._activeLanes || 0;
  $('bpsLabel').textContent = '↑ ' + fmtBps(S.dispBps);
}

// ─── SSE handling ─────────────────────────────────────────────────────────────
let es = null;

function openStream(jobId) {
  _knownJobId = jobId;
  if (es) { es.close(); es = null; }
  es = new EventSource('/api/stream?job=' + encodeURIComponent(jobId));
  es.addEventListener('job', (e) => onJob(JSON.parse(e.data)));
  es.addEventListener('progress', (e) => onProgress(JSON.parse(e.data)));
  es.addEventListener('log', (e) => onLog(JSON.parse(e.data)));
  es.addEventListener('served', (e) => onServed(JSON.parse(e.data)));
  es.addEventListener('done', (e) => onDone(JSON.parse(e.data)));
  es.addEventListener('error', (e) => { if (e.data) { try { onErr(JSON.parse(e.data)); } catch (_) {} } });
}

function onJob(d) {
  S.kind = d.kind;
  S.ref = d.ref || '';
  S.name = d.name || d.addr || '';
  S.total = d.totalBytes || 0;
  S.received = 0; S.dispRecv = 0; S.bps = 0; S.dispBps = 0; S.eta = 0; S.served = 0;
  S.platforms = d.platforms || [];
  S.packets = []; S.byIndex.clear(); S.flashes = [];
  S.chart.fill(0); S.chartHead = 0; S.chartMax = 1;
  S._lastP = performance.now(); S._lastRecvP = 0;

  document.body.classList.toggle('unpack', d.kind === 'unpack');

  if (d.kind === 'pack') {
    for (const l of d.layers || []) {
      const p = {
        i: l.i, label: shortDigest(l.digest), size: l.size || 0,
        received: l.received || 0, cached: !!l.cached, done: !!l.done || !!l.cached,
        rgb: packColor(l.i, l.cached), t: 0, glow: 0, trail: [], slammed: false, slam: 0,
      };
      if (p.done) { p.received = p.size; p.t = 1; p.slammed = true; }
      S.packets.push(p); S.byIndex.set(p.i, p);
    }
  } else {
    for (const im of d.images || []) {
      const p = {
        i: im.i, label: im.ref, size: im.total || 0, received: 0, cached: false, done: false,
        rgb: COL.amber, t: 0, glow: 0, trail: [], slammed: false, slam: 0,
      };
      S.packets.push(p); S.byIndex.set(p.i, p);
    }
  }

  const tm = $('transitMode');
  tm.className = 'mode ' + d.kind;
  tm.textContent = (d.kind === 'pack' ? 'PACK' : 'UNPACK') + ' ◉ LIVE';
  $('transitPlats').innerHTML = (S.platforms || []).slice(0, 5).map((p) => `<span class="p">${escapeHtml(p)}</span>`).join('');
  $('transitRef').textContent = S.name || S.ref || (d.addr ? '→ ' + d.addr : '');

  renderLanes();
}

function onProgress(d) {
  S.received = d.received || 0;
  if (d.total) S.total = d.total;
  if (d.servedCount != null) S.served = d.servedCount;
  if (d.etaSec != null) S.eta = d.etaSec;

  const arr = d.layers || d.images || [];
  for (const e of arr) {
    const p = S.byIndex.get(e.i);
    if (!p) continue;
    const inc = (e.received || 0) - p.received;
    if (inc > 0) p.glow = 1;
    p.received = e.received || 0;
    if (e.total) p.size = e.total;
    if (e.done) p.done = true;
    updateLane(p);
  }

  const now = performance.now();
  let bps = d.bps;
  if (bps == null) {
    const dtp = (now - S._lastP) / 1000;
    bps = dtp > 0 ? Math.max(0, (S.received - S._lastRecvP) / dtp) : 0;
  }
  S.bps = bps;
  S._lastP = now; S._lastRecvP = S.received;
  pushChart(bps);
}

function onLog(d) { log('', d.msg); }

function onServed(d) {
  const p = S.byIndex.get(d.i);
  if (p) { p.done = true; p.received = p.size; updateLane(p); }
  log('ok', 'serving ' + d.ref);
}

function onDone(d) {
  S.running = false;
  setRunning(false);
  if (es) { es.close(); es = null; }
  if (S.kind === 'pack') {
    for (const p of S.packets) { p.received = p.size; p.done = true; updateLane(p); }
    S.received = S.total;
    log('ok', `saved ${d.ref} · ${fmtBytes(d.totalBytes)} · ${d.durationMs}ms${d.cached ? ' (cached)' : ''}`);
    $('transitMode').textContent = 'COMPLETE';
    loadImages().then(() => flashCard(d.ref));
  } else {
    for (const p of S.packets) { p.done = true; updateLane(p); }
    log('ok', `registry LIVE on ${d.addr} · ${d.count} images · ${d.durationMs}ms`);
    $('regStatus').classList.add('live');
    $('regState').textContent = 'LIVE';
    $('transitMode').textContent = 'SERVING';
    loadImages();
  }
}

function onErr(d) {
  S.running = false;
  setRunning(false);
  if (es) { es.close(); es = null; }
  log('err', d.msg || 'error');
  $('transitMode').textContent = 'ERROR';
}

// ─── lanes ────────────────────────────────────────────────────────────────────
function renderLanes() {
  const el = $('lanes');
  el.innerHTML = '';
  $('lanesCount').textContent = S.packets.length ? S.packets.length + (S.kind === 'unpack' ? ' images' : ' layers') : '';
  if (!S.packets.length) { el.innerHTML = '<div class="empty">idle</div>'; return; }
  for (const p of S.packets) {
    const row = document.createElement('div');
    row.className = 'lane';
    row.innerHTML = `<span class="ld">${escapeHtml(p.label)}</span><span class="bar"><i></i></span><span class="sz"></span><span class="glyph">·</span>`;
    el.appendChild(row);
    p.row = row;
    p.barI = row.querySelector('i');
    p.szEl = row.querySelector('.sz');
    p.glyphEl = row.querySelector('.glyph');
    if (p.cached) row.classList.add('cached');
    updateLane(p);
  }
}

function updateLane(p) {
  if (!p.row) return;
  const pr = p.size > 0 ? clamp(p.received / p.size, 0, 1) : (p.done ? 1 : 0);
  p.barI.style.width = (pr * 100) + '%';
  p.szEl.textContent = p.size ? fmtBytes(p.received) + ' / ' + fmtBytes(p.size) : (p.done ? 'manifest' : '');
  p.row.classList.toggle('done', !!p.done);
  p.glyphEl.textContent = p.done ? '✓' : (pr > 0 ? '⟳' : '·');
}

// ─── browse / store ───────────────────────────────────────────────────────────
async function loadImages() {
  try {
    const r = await fetch('/api/images');
    const d = await r.json();
    $('storePath').textContent = d.storeDir || './store';
    $('sCount').textContent = d.count || 0;
    $('sCharts').textContent = d.chartCount || 0;
    $('sSize').textContent = fmtBytes((d.totalBytes || 0) + (d.chartBytes || 0));
    const arch = new Set();
    (d.images || []).forEach((im) => (im.platforms || []).forEach((p) => arch.add(p)));
    $('sArch').textContent = arch.size;
    if (d.version) $('appVersion').textContent = d.version;
    if (d.registryRunning) {
      $('regStatus').classList.add('live');
      $('regState').textContent = 'LIVE';
    }
    renderCards(d.images || [], d.charts || []);
    return d;
  } catch (e) {
    return null;
  }
}

function renderCards(images, charts) {
  charts = charts || [];
  const el = $('cards');
  el.innerHTML = '';
  $('search').value = '';
  if (!images.length && !charts.length) {
    el.innerHTML = '<div class="empty">store empty — add an image below</div>';
    return;
  }
  for (const im of images) {
    const card = document.createElement('div');
    card.className = 'card clickable';
    card.dataset.ref = im.ref;
    card.dataset.digest = im.digest;
    card.addEventListener('click', () => openModal(im.digest));
    const badge = im.isIndex ? '<span class="badge idx">idx</span>' : '<span class="badge">img</span>';
    const plats = (im.platforms || []).map((p) => `<span class="chip">${escapeHtml(p)}</span>`).join('');
    card.innerHTML =
      `<div class="ref">${badge}<span>${escapeHtml(im.ref)}</span><span class="state">STORED</span></div>` +
      `<div class="digest">${shortDigest(im.digest)}</div>` +
      `<div class="meta"><span>${fmtBytes(im.size)}</span><span>${im.layerCount} layers</span>${plats}</div>` +
      `<div class="microbar"><i style="width:100%"></i></div>`;
    el.appendChild(card);
  }
  if (charts.length) {
    const div = document.createElement('div');
    div.className = 'cards-divider';
    div.textContent = `HELM CHARTS · ${charts.length}`;
    el.appendChild(div);
  }
  for (const c of charts) {
    const card = document.createElement('div');
    card.className = 'card chart';
    const ver = c.version ? `<span class="chip">v${escapeHtml(c.version)}</span>` : '';
    const repo = c.repo ? `<span class="chip">${escapeHtml(c.repo)}</span>` : '';
    card.innerHTML =
      `<div class="ref"><span class="badge helm">helm</span><span>${escapeHtml(c.name || c.file)}</span><span class="state">STORED</span></div>` +
      (c.description ? `<div class="digest desc">${escapeHtml(c.description)}</div>` : '') +
      `<div class="meta"><span>${fmtBytes(c.size)}</span>${ver}${repo}</div>` +
      `<div class="microbar"><i style="width:100%"></i></div>`;
    el.appendChild(card);
  }
}

// ─── image detail modal ───────────────────────────────────────────────────────
async function openModal(digest) {
  if (!digest) return;
  try {
    const r = await fetch('/api/image-detail?digest=' + encodeURIComponent(digest));
    if (!r.ok) return;
    const d = await r.json();
    renderModal(d);
    $('modal').classList.remove('hidden');
  } catch (_) {}
}

function closeModal() {
  $('modal').classList.add('hidden');
}

let _modalPlatIdx = 0;

function renderModal(d) {
  _modalPlatIdx = 0;
  $('modalRef').textContent = d.ref || '';
  $('modalDigest').textContent = shortDigest(d.digest);
  const platforms = d.platforms || [];

  const tabs = $('modalPlats');
  tabs.innerHTML = '';
  if (platforms.length > 1) {
    platforms.forEach((p, i) => {
      const tab = document.createElement('span');
      tab.className = 'plat-tab' + (i === 0 ? ' active' : '');
      tab.textContent = p.platform || '?';
      tab.onclick = () => {
        _modalPlatIdx = i;
        tabs.querySelectorAll('.plat-tab').forEach((t, ti) => t.classList.toggle('active', ti === i));
        renderPlatform(platforms[i]);
      };
      tabs.appendChild(tab);
    });
  }

  if (platforms.length > 0) renderPlatform(platforms[0]);
}

function renderPlatform(p) {
  const layers = p.layers || [];
  const maxSize = Math.max(1, ...layers.map((l) => l.size));
  const layersEl = $('modalLayers');
  const countEl = $('modalLayerCount');
  layersEl.innerHTML = '';
  countEl.textContent = layers.length + ' layer' + (layers.length !== 1 ? 's' : '') + ' · ' + fmtBytes(p.layerTotal || 0);

  for (const l of layers) {
    const row = document.createElement('div');
    row.className = 'modal-layer';
    const pct = ((l.size / maxSize) * 100).toFixed(1);
    row.innerHTML =
      `<span class="ld">${shortDigest(l.digest)}</span>` +
      `<span class="bar"><i style="width:${pct}%"></i></span>` +
      `<span class="sz">${fmtBytes(l.size)}</span>`;
    layersEl.appendChild(row);
  }

  const cfg = p.config || {};
  const cfgEl = $('modalCfg');
  cfgEl.innerHTML = '';
  const rows = [
    ['platform', p.platform],
    ['created', cfg.created ? cfg.created.slice(0, 10) : null],
    ['entrypoint', (cfg.entrypoint || []).length ? cfg.entrypoint.join(' ') : null],
    ['cmd', (cfg.cmd || []).length ? cfg.cmd.join(' ') : null],
    ...Object.entries(cfg.labels || {}).slice(0, 8).map(([k, v]) => [k, v]),
  ].filter(([, v]) => v);

  for (const [k, v] of rows) {
    const kEl = document.createElement('span');
    kEl.className = 'k';
    kEl.textContent = k;
    const vEl = document.createElement('span');
    vEl.className = 'v';
    vEl.textContent = v;
    cfgEl.appendChild(kEl);
    cfgEl.appendChild(vEl);
  }

  $('modalCfgSection').style.display = rows.length ? '' : 'none';
}

function filterCards() {
  const term = ($('search').value || '').trim().toLowerCase();
  const cards = $('cards');
  let visibleCharts = 0, visibleImages = 0;
  let inChartSection = false;
  for (const el of cards.children) {
    if (el.classList.contains('cards-divider')) {
      inChartSection = true;
      el.style.display = '';
      continue;
    }
    if (!el.classList.contains('card')) { el.style.display = ''; continue; }
    const match = !term || el.textContent.toLowerCase().includes(term);
    el.style.display = match ? '' : 'none';
    if (match) { inChartSection ? visibleCharts++ : visibleImages++; }
  }
  // hide the divider when no chart cards are visible
  for (const el of cards.children) {
    if (el.classList.contains('cards-divider')) {
      el.style.display = visibleCharts === 0 ? 'none' : '';
    }
  }
}

function flashCard(ref) {
  const card = document.querySelector(`.card[data-ref="${(window.CSS && CSS.escape) ? CSS.escape(ref) : ref}"]`);
  if (!card) return;
  card.classList.add('captured');
  setTimeout(() => card.classList.remove('captured'), 800);
}

// ─── log ──────────────────────────────────────────────────────────────────────
function log(cls, msg) {
  const el = $('log');
  const row = document.createElement('div');
  row.className = 'row' + (cls ? ' ' + cls : '');
  const t = new Date().toLocaleTimeString('en-GB', { hour12: false });
  row.innerHTML = `<span class="t">${t}</span> ${escapeHtml(msg)}`;
  el.appendChild(row);
  while (el.children.length > 200) el.removeChild(el.firstChild);
  el.scrollTop = el.scrollHeight;
}

// ─── controls ──────────────────────────────────────────────────────────────────
function setRunning(b) {
  S.running = b;
  $('packBtn').disabled = b;
  $('unpackBtn').disabled = b;
}

async function doPack() {
  if (S.running) return;
  const ref = $('ref').value.trim();
  if (!ref) { $('ref').focus(); return; }
  setRunning(true);
  log('', 'pack ' + ref);
  try {
    const r = await fetch('/api/pack', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ref }),
    });
    const d = await r.json();
    if (!r.ok || !d.jobId) { onErr({ msg: d.error || 'pack rejected' }); return; }
    openStream(d.jobId);
  } catch (e) {
    onErr({ msg: String(e) });
  }
}

async function doUnpack() {
  if (S.running) return;
  setRunning(true);
  log('', 'unpack → 127.0.0.1:5000');
  try {
    const r = await fetch('/api/unpack', { method: 'POST' });
    const d = await r.json();
    if (!r.ok || !d.jobId) { onErr({ msg: d.error || 'unpack rejected' }); return; }
    openStream(d.jobId);
  } catch (e) {
    onErr({ msg: String(e) });
  }
}

// ─── job poller ───────────────────────────────────────────────────────────────
// Polls /api/latest-job every second so CLI-initiated packs appear automatically
// without the user needing to open the ?job= URL manually.
function pollForJobs() {
  fetch('/api/latest-job')
    .then((r) => r.json())
    .then((d) => { if (d.jobId && d.jobId !== _knownJobId) openStream(d.jobId); })
    .catch(() => {});
}

// ─── boot ──────────────────────────────────────────────────────────────────────
$('search').addEventListener('input', filterCards);
$('packBtn').addEventListener('click', doPack);
$('unpackBtn').addEventListener('click', doUnpack);
$('ref').addEventListener('keydown', (e) => { if (e.key === 'Enter') doPack(); });
$('modalClose').addEventListener('click', closeModal);
$('modalBd').addEventListener('click', closeModal);
document.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeModal(); });

stage = setupCanvas($('stage'));
chart = setupCanvas($('chart'));
loadImages();
requestAnimationFrame(frame);

const _jobParam = new URLSearchParams(location.search).get('job');
if (_jobParam) { openStream(_jobParam); } else { $('ref').focus(); }
setInterval(pollForJobs, 1000);
