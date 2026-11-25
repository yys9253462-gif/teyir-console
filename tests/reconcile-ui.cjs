// 截订阅中心，并断言"全量对账"按钮与行内"账号对账"菜单项存在。
const { spawn } = require('child_process');
const fs = require('fs');
const http = require('http');
const path = require('path');
const os = require('os');

const CHROME = 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const DIST = path.join(__dirname, '..', 'dist');
const PORT = 19385, CDP = 19386;
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
    '--disable-gpu', '--window-size=1600,1000',
    '--user-data-dir=' + fs.mkdtempSync(path.join(os.tmpdir(), 'cdp-sub-')), 'about:blank'], { stdio: 'ignore' });
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
    await ev(`(()=>{const b=[...document.querySelectorAll('.nav-item')].find(x=>x.textContent.trim()==='订阅中心');if(b)b.click()})()`);
    await sleep(3600);

    const hasAll = await ev(`(()=>{const b=[...document.querySelectorAll('button')].find(x=>x.textContent.includes('全量对账'));return b?{found:true,title:b.title||'',disabled:b.disabled}:{found:false}})()`);
    console.log('全量对账按钮:', JSON.stringify(hasAll));

    // 打开第一条订阅的行内菜单。
    //
    // ⚠️ 选择器不能用 `.row-more` —— 订阅表右侧还有几个图标按钮
    // （复制/开关/换链接/删除）也带类似的类，第一个未必是菜单触发器。
    // 用 aria-label 精确匹配（RowMenu 里写的是「<label> 的更多操作」）。
    const opened = await ev(`(()=>{
      const btns=[...document.querySelectorAll('button[aria-label*="更多操作"]')];
      if(!btns.length) return {found:false,count:0};
      btns[0].click();
      return {found:true,count:btns.length};
    })()`);
    console.log('菜单触发器:', JSON.stringify(opened));
    await sleep(1000);
    const menu = await ev(`JSON.stringify([...document.querySelectorAll('[role=menuitem]')].map(b=>b.textContent.trim().slice(0,40)))`);
    console.log('行内菜单项:', menu);

    const shot = await send('Page.captureScreenshot', { format: 'png' });
    if (shot.result?.data) {
      const f = path.join(OUT, 'sub-reconcile.png');
      fs.writeFileSync(f, Buffer.from(shot.result.data, 'base64'));
      console.log('截图:', f);
    }

    const items = JSON.parse(menu || '[]');
    console.log();
    console.log('  含「账号对账」:', items.some((i) => i.includes('账号对账')) ? 'OK' : '★ 缺');
    console.log('  含「全量对账」按钮:', hasAll && hasAll.found ? 'OK' : '★ 缺');
  } finally {
    try { chrome.kill(); } catch (e) { }
    server.close();
  }
  process.exit(0);
}
main().catch((e) => { console.error(e); process.exit(1); });
