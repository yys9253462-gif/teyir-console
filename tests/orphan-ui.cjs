// 专门截孤儿账号区块，并断言渲染内容。
const { spawn } = require('child_process');
const fs = require('fs');
const http = require('http');
const path = require('path');
const os = require('os');

const CHROME = 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const DIST = path.join(__dirname, '..', 'dist');
const PORT = 19383, CDP = 19384;
const API = process.env.API_TARGET || 'http://127.0.0.1:8081';
const TOKEN = process.env.NODEOPS_TOKEN || '';
const OUT = process.env.SHOT_DIR || path.join(__dirname, 'shots');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const MIME = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript', '.css': 'text/css', '.png': 'image/png' };
fs.mkdirSync(OUT, { recursive: true });

function proxy(req, res) {
  const t = new URL(req.url, API);
  const c = []; req.on('data', (d) => c.push(d));
  req.on('end', () => {
    const pr = http.request({ hostname: t.hostname, port: t.port, path: t.pathname + t.search, method: req.method,
      headers: { ...req.headers, host: t.host, authorization: 'Bearer ' + TOKEN } },
      (r2) => { res.writeHead(r2.statusCode, r2.headers); r2.pipe(res); });
    pr.on('error', (e) => { res.writeHead(502); res.end(String(e)); });
    for (const x of c) pr.write(x); pr.end();
  });
}
const server = http.createServer((req, res) => {
  if (req.url.startsWith('/api/')) return proxy(req, res);
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
  const chrome = spawn(CHROME, [`--remote-debugging-port=${CDP}`, '--headless=new', '--no-sandbox',
    '--disable-gpu', '--window-size=1600,1100',
    '--user-data-dir=' + fs.mkdtempSync(path.join(os.tmpdir(), 'cdp-or-')), 'about:blank'], { stdio: 'ignore' });
  try {
    await sleep(3200);
    const targets = await getJSON(`http://127.0.0.1:${CDP}/json`);
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
    await send('Page.navigate', { url: origin + '/' }); await sleep(2400);
    await ev(`(()=>{const b=[...document.querySelectorAll('.nav-item')].find(x=>x.textContent.trim()==='运维任务');if(b)b.click()})()`);
    await sleep(3800);

    // 读出孤儿区块的真实渲染内容
    const info = await ev(`JSON.stringify((()=>{
      const secs=[...document.querySelectorAll('section.panel')];
      const sec=secs.find(s=>s.textContent.includes('孤儿账号'));
      if(!sec) return {found:false};
      const rows=[...sec.querySelectorAll('tbody tr')].map(tr=>[...tr.querySelectorAll('td')].map(td=>td.textContent.trim().slice(0,40)));
      return {
        found:true,
        heading:(sec.querySelector('h2')||{}).textContent||'',
        note:(sec.querySelector('.form-note')||{}).textContent.trim().slice(0,150)||'',
        groups:[...sec.querySelectorAll('h2')].map(h=>h.textContent.trim()),
        rows:rows,
        buttons:[...sec.querySelectorAll('button')].map(b=>b.textContent.trim()),
      };
    })())`);
    const d = JSON.parse(info || '{}');
    console.log('找到孤儿区块:', d.found);
    if (d.found) {
      console.log('  标题:', d.heading);
      console.log('  分组:', JSON.stringify(d.groups));
      console.log('  说明:', d.note);
      console.log('  按钮:', JSON.stringify(d.buttons));
      console.log('  行数:', d.rows.length);
      for (const r of d.rows) console.log('    ', JSON.stringify(r));
      const hasRetry = d.buttons.some((b) => b.includes('补删'));
      const hasManual = d.groups.some((g) => g.includes('人工'));
      console.log();
      console.log('  可补删行有按钮:', hasRetry ? 'OK' : '★ 缺');
      console.log('  人工分组存在:', hasManual ? 'OK' : '★ 缺');
    }

    // 滚到孤儿区块再截图
    await ev(`(()=>{const secs=[...document.querySelectorAll('section.panel')];const s=secs.find(x=>x.textContent.includes('孤儿账号'));if(s)s.scrollIntoView({block:'start'})})()`);
    await sleep(800);
    const shot = await send('Page.captureScreenshot', { format: 'png' });
    if (shot.result?.data) {
      const f = path.join(OUT, 'orphan-accounts.png');
      fs.writeFileSync(f, Buffer.from(shot.result.data, 'base64'));
      console.log('截图:', f);
    }
  } finally {
    try { chrome.kill(); } catch (e) { }
    server.close();
  }
  process.exit(0);
}
main().catch((e) => { console.error(e); process.exit(1); });
