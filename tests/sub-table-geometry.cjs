// 复现订阅中心红框里的问题：量出滚动条与容器几何。
//
// 管理员圈的是表格右侧：一条竖向滚动条 + 一截空白圆角框。
// 先用真实渲染量清楚，别凭截图猜。
const { spawn } = require('child_process');
const fs = require('fs');
const http = require('http');
const path = require('path');
const os = require('os');

const CHROME = process.env.CHROME_BIN || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const DIST = path.join(__dirname, '..', 'dist');
const PORT = Number(process.env.UI_PORT || 19301);
const CDP_PORT = Number(process.env.UI_CDP_PORT || 19302);
const API_TARGET = process.env.API_TARGET || 'http://127.0.0.1:8081';
const TOKEN = process.env.NODEOPS_TOKEN || '';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const MIME = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript', '.css': 'text/css', '.png': 'image/png' };
if (!TOKEN) { console.error('缺 NODEOPS_TOKEN'); process.exit(2); }

function proxyApi(req, res) {
  const t = new URL(req.url, API_TARGET);
  const c = []; req.on('data', (d) => c.push(d));
  req.on('end', () => {
    const pr = http.request({ hostname: t.hostname, port: t.port, path: t.pathname + t.search, method: req.method,
      headers: { ...req.headers, host: t.host, authorization: `Bearer ${TOKEN}` } },
      (r2) => { res.writeHead(r2.statusCode, r2.headers); r2.pipe(res); });
    pr.on('error', (e) => { res.writeHead(502); res.end(String(e)); });
    for (const x of c) pr.write(x); pr.end();
  });
}
const server = http.createServer((req, res) => {
  if (req.url.startsWith('/api/')) return proxyApi(req, res);
  let rel = decodeURIComponent(req.url.split('?')[0]);
  if (rel === '/' || rel === '') rel = '/index.html';
  const f = path.join(DIST, rel);
  if (!f.startsWith(DIST) || !fs.existsSync(f) || fs.statSync(f).isDirectory()) { res.writeHead(404); return res.end('nf'); }
  res.writeHead(200, { 'Content-Type': MIME[path.extname(f)] || 'application/octet-stream' });
  fs.createReadStream(f).pipe(res);
});
const getJSON = (u) => new Promise((ok, no) => http.get(u, (r) => { let b = ''; r.on('data', (d) => b += d); r.on('end', () => { try { ok(JSON.parse(b)); } catch (e) { no(e); } }); }).on('error', no));

async function main() {
  await new Promise((r) => server.listen(PORT, '127.0.0.1', r));
  const chrome = spawn(CHROME, [`--remote-debugging-port=${CDP_PORT}`, '--headless=new', '--no-sandbox',
    '--disable-gpu', '--disable-dev-shm-usage', '--window-size=1600,900',
    '--user-data-dir=' + fs.mkdtempSync(path.join(os.tmpdir(), 'cdp-sub2-')), 'about:blank'], { stdio: 'ignore' });
  try {
    await sleep(3200);
    const targets = await getJSON(`http://127.0.0.1:${CDP_PORT}/json`);
    const page = targets.find((t) => t.type === 'page');
    const WebSocket = require('ws');
    const ws = new WebSocket(page.webSocketDebuggerUrl);
    await new Promise((r) => ws.on('open', r));
    let id = 0; const pending = new Map();
    ws.on('message', (raw) => { const m = JSON.parse(raw); if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } });
    const send = (m, p = {}) => new Promise((res) => { const mid = ++id; pending.set(mid, res); ws.send(JSON.stringify({ id: mid, method: m, params: p })); });
    const ev = async (e) => (await send('Runtime.evaluate', { expression: e, returnByValue: true })).result?.result?.value;

    await send('Runtime.enable'); await send('Page.enable');
    const origin = `http://127.0.0.1:${PORT}`;
    await send('Page.navigate', { url: origin + '/' }); await sleep(1200);
    await ev(`localStorage.setItem('nodeops.session', JSON.stringify({token:'${TOKEN}',email:'o@t.com',role:'admin'}))`);
    await send('Page.navigate', { url: origin + '/' }); await sleep(2500);
    await ev(`(() => { const b=[...document.querySelectorAll('.nav-item')].find(x=>x.textContent.trim()==='订阅中心'); if(b) b.click() })()`);
    await sleep(2500);

    // 先确认页面真的到了「订阅中心」—— 上一版没验这一步，
    // 选择器落空时报 found:false，看起来像"类名不存在"，
    // 实际可能只是页面没切过去（诊断方向完全不同）。
    const h1 = await ev(`document.querySelector('.content-wrap h1')?.textContent ?? ''`);
    console.log('页面标题:', h1);
    if (!String(h1).includes('订阅中心')) {
      console.error('!! 没到订阅中心页，后续测量无意义');
      process.exit(1);
    }
    // 确认表格存在
    const tableInfo = await ev(`(() => {
      const t = document.querySelector('table.sub-table');
      return t ? 'ok:' + t.className : 'missing';
    })()`);
    console.log('表格:', tableInfo);

    const geo = JSON.parse(await ev(`(() => {
      const wrap = document.querySelector('table.sub-table')?.closest('.server-table-wrap');
      const tbl = wrap?.querySelector('table');
      if (!wrap) return JSON.stringify({ found: false });
      const w = wrap.getBoundingClientRect(), t = tbl.getBoundingClientRect();
      const acts = tbl.querySelector('tbody tr td:last-child');
      const actRect = acts?.getBoundingClientRect();
      return JSON.stringify({
        found: true,
        wrapClass: wrap.className,
        tableClass: tbl.className,
        wrapW: Math.round(w.width), wrapH: Math.round(w.height),
        scrollW: wrap.scrollWidth, clientW: wrap.clientWidth,
        scrollH: wrap.scrollHeight, clientH: wrap.clientHeight,
        overflowX: getComputedStyle(wrap).overflowX,
        overflowY: getComputedStyle(wrap).overflowY,
        tableW: Math.round(t.width),
        hOverflow: wrap.scrollWidth > wrap.clientWidth,
        vOverflow: wrap.scrollHeight > wrap.clientHeight,
        // 操作列几何：它是否被推出可视区、是否 sticky
        actPos: acts ? getComputedStyle(acts).position : 'n/a',
        actRight: actRect ? Math.round(actRect.right) : null,
        wrapRight: Math.round(w.right),
        actVisible: actRect ? actRect.right <= w.right + 1 : null,
        lastThSticky: (() => { const th = tbl.querySelector('thead th:last-child'); return th ? getComputedStyle(th).position : 'n/a' })(),
      });
    })()`) || '{}');
    console.log('=== 表格容器几何（1600px 视口） ===');
    console.log(JSON.stringify(geo, null, 2));

    if (geo.hOverflow) console.log('\n>> 横向溢出：内容 ' + geo.scrollW + ' > 容器 ' + geo.clientW);
    if (geo.vOverflow) console.log('>> 纵向溢出：内容 ' + geo.scrollH + ' > 容器 ' + geo.clientH + '  ← 竖条');
    console.log('>> 操作列 position=' + geo.actPos + ' 可见=' + geo.actVisible +
                (geo.actVisible === false ? '  ← 按钮被推出可视区，用户点不到' : ''));

    // 逐档视口量：窄屏下操作列会不会被推出去
    console.log('\n=== 逐档视口 ===');
    for (const width of [1600, 1400, 1200, 1000, 860]) {
      await send('Emulation.setDeviceMetricsOverride', { width, height: 900, deviceScaleFactor: 1, mobile: false });
      await sleep(450);
      const r = JSON.parse(await ev(`(() => {
        const wrap = document.querySelector('table.sub-table')?.closest('.server-table-wrap');
        const tbl = wrap?.querySelector('table');
        const act = tbl?.querySelector('tbody tr td:last-child');
        const w = wrap.getBoundingClientRect();
        const a = act?.getBoundingClientRect();
        return JSON.stringify({
          wrapW: Math.round(w.width),
          hOver: wrap.scrollWidth > wrap.clientWidth,
          vOver: wrap.scrollHeight > wrap.clientHeight,
          actPos: act ? getComputedStyle(act).position : 'n/a',
          actRight: a ? Math.round(a.right) : null,
          wrapRight: Math.round(w.right),
          actVisible: a ? a.right <= w.right + 1 : null,
        });
      })()`) || '{}');
      console.log(`  ${width}px: 容器 ${r.wrapW} 横溢=${r.hOver} 纵溢=${r.vOver} ` +
                  `actPos=${r.actPos} 操作列可见=${r.actVisible}`);
    }
    await send('Emulation.setDeviceMetricsOverride', { width: 1600, height: 900, deviceScaleFactor: 1, mobile: false });
    await sleep(400);
  } finally {
    try { chrome.kill(); } catch (e) {}
    server.close();
  }
}
main().catch((e) => { console.error(e); process.exit(1); });
