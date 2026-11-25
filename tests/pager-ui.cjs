// Verify the subscription-center pager: 62 rows should paginate into 3 pages,
// search should narrow within the current page, and no-match should say so.
const { spawn } = require('child_process');
const fs = require('fs');
const http = require('http');
const path = require('path');
const os = require('os');

const CHROME = process.env.CHROME_BIN || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const DIST = path.join(__dirname, '..', 'dist');
const PORT = 19381, CDP = 19382;
const API_TARGET = 'http://127.0.0.1:18080';
const TOKEN = process.env.NODEOPS_TOKEN || '';
const OUT = process.env.SHOT_DIR || path.join(__dirname, 'shots');
fs.mkdirSync(OUT, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const MIME = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript', '.css': 'text/css', '.png': 'image/png' };

const server = http.createServer((req, res) => {
  if (req.url.startsWith('/api/')) {
    const t = new URL(req.url, API_TARGET);
    const c = []; req.on('data', (d) => c.push(d));
    req.on('end', () => {
      const pr = http.request({ hostname: t.hostname, port: t.port, path: t.pathname + t.search,
        method: req.method, headers: { ...req.headers, host: t.host,
          ...(TOKEN ? { authorization: 'Bearer ' + TOKEN } : {}) } },
        (r2) => { res.writeHead(r2.statusCode, r2.headers); r2.pipe(res); });
      pr.on('error', (e) => { res.writeHead(502); res.end(String(e)); });
      for (const x of c) pr.write(x); pr.end();
    });
    return;
  }
  let rel = decodeURIComponent(req.url.split('?')[0]);
  if (rel === '/') rel = '/index.html';
  const f = path.join(DIST, rel);
  if (!f.startsWith(DIST) || !fs.existsSync(f) || fs.statSync(f).isDirectory()) { res.writeHead(404); return res.end('nf'); }
  res.writeHead(200, { 'Content-Type': MIME[path.extname(f)] || 'application/octet-stream' });
  fs.createReadStream(f).pipe(res);
});

const getJSON = (u) => new Promise((ok, no) => http.get(u, (r) => { let b = ''; r.on('data', (d) => b += d); r.on('end', () => { try { ok(JSON.parse(b)); } catch (e) { no(e); } }); }).on('error', no));

(async () => {
  await new Promise((r) => server.listen(PORT, '127.0.0.1', r));
  const chrome = spawn(CHROME, [`--remote-debugging-port=${CDP}`, '--headless=new', '--no-sandbox',
    '--disable-gpu', '--disable-dev-shm-usage', '--window-size=1600,900',
    '--user-data-dir=' + fs.mkdtempSync(path.join(os.tmpdir(), 'cdp-page-')), 'about:blank'], { stdio: 'ignore' });
  try {
    await sleep(3000);
    const targets = await getJSON(`http://127.0.0.1:${CDP}/json`);
    const page = targets.find((t) => t.type === 'page');
    const WebSocket = require('ws');
    const ws = new WebSocket(page.webSocketDebuggerUrl);
    await new Promise((r) => ws.on('open', r));
    let id = 0; const pending = new Map();
    ws.on('message', (raw) => { const m = JSON.parse(raw); if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } });
    const send = (m, p = {}) => new Promise((res) => { const mid = ++id; pending.set(mid, res); ws.send(JSON.stringify({ id: mid, method: m, params: p })); });
    const ev = async (e) => (await send('Runtime.evaluate', { expression: e, returnByValue: true })).result?.result?.value;
    const shot = async (name) => {
      const s = await send('Page.captureScreenshot', { format: 'png' });
      if (s.result?.data) fs.writeFileSync(path.join(OUT, name), Buffer.from(s.result.data, 'base64'));
    };
    const setSearch = (val) => ev(`(() => {
      const i = document.querySelector('.sub-search input');
      if (!i) return false;
      const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
      setter.call(i, ${JSON.stringify(val)});
      i.dispatchEvent(new Event('input', { bubbles: true }));
      return true;
    })()`);

    await send('Runtime.enable'); await send('Page.enable');
    const origin = `http://127.0.0.1:${PORT}`;
    await send('Page.navigate', { url: origin + '/' }); await sleep(1200);
    await ev(`localStorage.setItem('nodeops.session', JSON.stringify({token:'${TOKEN}',email:'o@t.com',role:'admin'}))`);
    await send('Page.navigate', { url: origin + '/' }); await sleep(2500);
    await ev(`(() => { const b=[...document.querySelectorAll('.nav-item')].find(x=>x.textContent.trim()==='订阅中心'); if(b) b.click() })()`);
    await sleep(6000);

    const info = JSON.parse(await ev(`(() => {
      const rows = document.querySelectorAll('.sub-table tbody tr').length;
      const pager = document.querySelector('.pager');
      const btns = pager ? [...pager.querySelectorAll('.pager-btn')].map(b=>b.textContent.trim()) : [];
      return JSON.stringify({
        rows, hasPager: !!pager, btns,
        current: (document.querySelector('.pager-btn.is-current')||{}).innerText||'',
        jump: (document.querySelector('.pager-jump')||{}).innerText||'',
        hasSearch: !!document.querySelector('.sub-search input'),
        count: (document.querySelector('.sub-count')||{}).innerText||'',
        first: (document.querySelector('.sub-table tbody tr strong')||{}).innerText||'',
      });
    })()`) || '{}');
    console.log('PAGE1 ' + JSON.stringify(info));
    await shot('pager-page1.png');

    await ev(`(() => { const b=[...document.querySelectorAll('.pager-btn')].find(x=>x.textContent.trim()==='3'); if(b) b.click(); })()`);
    await sleep(1000);
    const p3 = JSON.parse(await ev(`(() => JSON.stringify({
      rows: document.querySelectorAll('.sub-table tbody tr').length,
      current: (document.querySelector('.pager-btn.is-current')||{}).innerText||'',
      first: (document.querySelector('.sub-table tbody tr strong')||{}).innerText||'',
    }))()`) || '{}');
    console.log('PAGE3 ' + JSON.stringify(p3));
    await shot('pager-page3.png');

    await setSearch('分页验证-042');
    await sleep(1000);
    const sr = JSON.parse(await ev(`(() => JSON.stringify({
      rows: document.querySelectorAll('.sub-table tbody tr').length,
      first: (document.querySelector('.sub-table tbody tr strong')||{}).innerText||'',
      hasPager: !!document.querySelector('.pager'),
    }))()`) || '{}');
    console.log('SEARCH ' + JSON.stringify(sr));
    await shot('pager-search.png');

    await setSearch('zzz-nope-xyz');
    await sleep(900);
    const miss = await ev(`(document.querySelector('.empty-state strong')||{}).innerText||''`);
    console.log('NOMATCH ' + JSON.stringify(miss));
    await shot('pager-nomatch.png');
  } finally {
    try { chrome.kill(); } catch (e) {}
    server.close();
  }
  process.exit(0);
})().catch((e) => { console.error(e); process.exit(1); });
