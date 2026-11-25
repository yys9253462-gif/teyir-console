// 诊断：为什么只截到 4 张图（8 个页面应该有 8 张）。
const { spawn } = require('child_process');
const fs = require('fs');
const http = require('http');
const path = require('path');
const os = require('os');

const CHROME = process.env.CHROME_BIN || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const DIST = path.join(__dirname, '..', 'dist');
const PORT = 19361, CDP_PORT = 19362;
const API_TARGET = process.env.API_TARGET || 'http://127.0.0.1:8081';
const TOKEN = process.env.NODEOPS_TOKEN || '';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const MIME = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript', '.css': 'text/css', '.png': 'image/png' };

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
    '--user-data-dir=' + fs.mkdtempSync(path.join(os.tmpdir(), 'cdp-diag-')), 'about:blank'], { stdio: 'ignore' });
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
    await send('Page.navigate', { url: origin + '/' }); await sleep(2200);

    // 列出导航项的真实文字（可能是全角/带空格差异）
    const navs = await ev(`JSON.stringify([...document.querySelectorAll('.nav-item')].map(x => x.textContent.trim()))`);
    console.log('导航项实际文字:');
    JSON.parse(navs || '[]').forEach((n, i) => console.log(`  ${i+1}. ${JSON.stringify(n)}`));

    // 逐个点击并截图，看哪个失败
    const list = JSON.parse(navs || '[]');
    for (const nav of list) {
      const clicked = await ev(`(() => {
        const b = [...document.querySelectorAll('.nav-item')].find(x => x.textContent.trim() === ${JSON.stringify(nav)});
        if (b) { b.click(); return true } return false
      })()`);
      await sleep(2000);
      const shot = await send('Page.captureScreenshot', { format: 'png' });
      const hasData = !!(shot.result?.data);
      const h1 = await ev(`(document.querySelector('.content-wrap h1')||{}).textContent || ''`);
      console.log(`  ${clicked ? '[点击OK]' : '[点击失败]'} ${nav}  h1=${JSON.stringify(h1)}  截图=${hasData}  ${hasData ? Math.round(shot.result.data.length/1024) + 'KB' : ''}`);
    }
  } finally {
    try { chrome.kill(); } catch (e) {}
    server.close();
  }
}
main().catch((e) => { console.error(e); process.exit(1); });
