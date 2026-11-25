// 量 .drawer 的实际渲染方式：它到底是浮动面板，还是排在文档流下面。
const { spawn } = require('child_process');
const fs = require('fs');
const http = require('http');
const path = require('path');
const os = require('os');

const CHROME = process.env.CHROME_BIN || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const DIST = path.join(__dirname, '..', 'dist');
const PORT = 19331, CDP_PORT = 19332;
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
    '--user-data-dir=' + fs.mkdtempSync(path.join(os.tmpdir(), 'cdp-dw-')), 'about:blank'], { stdio: 'ignore' });
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
    await ev(`(() => { const b=[...document.querySelectorAll('.nav-item')].find(x=>x.textContent.trim()==='节点监控'); if(b) b.click() })()`);
    await sleep(2500);

    // 点第一张节点卡
    const clicked = await ev(`(() => { const c = document.querySelector('.node-card'); if (c) { c.click(); return true } return false })()`);
    console.log('点了节点卡:', clicked);
    await sleep(900);

    const geo = JSON.parse(await ev(`(() => {
      const d = document.querySelector('.drawer');
      const bd = document.querySelector('.drawer-backdrop');
      if (!d) return JSON.stringify({ exists: false });
      const cs = getComputedStyle(d);
      const r = d.getBoundingClientRect();
      const bcs = bd ? getComputedStyle(bd) : null;
      const br = bd ? bd.getBoundingClientRect() : null;
      return JSON.stringify({
        exists: true,
        drawer: {
          position: cs.position, display: cs.display,
          top: Math.round(r.top), left: Math.round(r.left),
          width: Math.round(r.width), height: Math.round(r.height),
          maxHeight: cs.maxHeight, overflowY: cs.overflowY,
          zIndex: cs.zIndex,
          // 是否在视口内可见
          inViewport: r.top < window.innerHeight && r.bottom > 0,
        },
        backdrop: bd ? { position: bcs.position, width: Math.round(br.width), height: Math.round(br.height) } : null,
        viewport: { w: window.innerWidth, h: window.innerHeight },
        pageScrollHeight: document.body.scrollHeight,
      });
    })()`) || '{}');
    console.log('\n=== .drawer 实测 ===');
    console.log(JSON.stringify(geo, null, 2));

    if (geo.exists) {
      const d = geo.drawer;
      if (d.position === 'static') {
        console.log('\n>> position=static —— 它排在文档流里，所以显示在页面下方（管理员的问题）');
      }
      if (!d.inViewport) {
        console.log('>> 抽屉不在视口内 —— 点了卡片后看不到详情，得往下滚');
      }
      if (d.height > geo.viewport.h) {
        console.log(`>> 抽屉高 ${d.height}px > 视口 ${geo.viewport.h}px —— 内容溢出一屏`);
      }
    }
  } finally {
    try { chrome.kill(); } catch (e) {}
    server.close();
  }
}
main().catch((e) => { console.error(e); process.exit(1); });
