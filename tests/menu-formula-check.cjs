// 直接在浏览器里验证 toggleMenu 的坐标公式，看它到底算出什么。
// 这一步是为了把"代码错"与"断言错"彻底分开（铁律⑨）。
const { spawn } = require('child_process');
const fs = require('fs');
const http = require('http');
const path = require('path');
const os = require('os');

const CHROME = process.env.CHROME_BIN || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const DIST = path.join(__dirname, '..', 'dist');
const PORT = 19311, CDP_PORT = 19312;
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
    '--user-data-dir=' + fs.mkdtempSync(path.join(os.tmpdir(), 'cdp-fml-')), 'about:blank'], { stdio: 'ignore' });
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

    // 在页面里手工复算一遍公式，与组件算出的结果对比
    const calc = JSON.parse(await ev(`(() => {
      const b = document.querySelector('.sub-table .row-menu .row-more');
      const r = b.getBoundingClientRect();
      const MENU_W = 280, MENU_H = 232;
      const left = Math.max(8, Math.min(r.right - MENU_W, window.innerWidth - MENU_W - 8));
      const below = window.innerHeight - r.bottom;
      const top = below >= MENU_H + 12 ? r.bottom + 6 : Math.max(8, r.top - MENU_H - 6);
      return JSON.stringify({
        btn: { right: Math.round(r.right), top: Math.round(r.top), bottom: Math.round(r.bottom) },
        vw: window.innerWidth, vh: window.innerHeight,
        expectLeft: Math.round(left), expectTop: Math.round(top),
        innerWidthMinus: window.innerWidth - MENU_W - 8,
        rightMinus: r.right - MENU_W,
      });
    })()`) || '{}');
    console.log('=== 公式复算 ===');
    console.log(JSON.stringify(calc, null, 2));

    // 打开菜单，读实际内联 style
    await ev(`document.querySelector('.sub-table .row-menu .row-more').click()`);
    await sleep(600);
    const actual = JSON.parse(await ev(`(() => {
      const pop = document.querySelector('.row-menu-pop');
      if (!pop) return JSON.stringify({ open: false });
      const r = pop.getBoundingClientRect();
      return JSON.stringify({
        open: true,
        inlineStyle: pop.getAttribute('style'),
        rect: { top: Math.round(r.top), left: Math.round(r.left) },
      });
    })()`) || '{}');
    console.log('\n=== 实际渲染 ===');
    console.log(JSON.stringify(actual, null, 2));

    console.log('\n=== 判定 ===');
    if (!actual.open) console.log('菜单没打开');
    else {
      const dLeft = Math.abs(actual.rect.left - calc.expectLeft);
      const dTop = Math.abs(actual.rect.top - calc.expectTop);
      console.log(`left: 期望 ${calc.expectLeft} / 实际 ${actual.rect.left} 差 ${dLeft}`);
      console.log(`top : 期望 ${calc.expectTop} / 实际 ${actual.rect.top} 差 ${dTop}`);
      if (dLeft > 2 || dTop > 2) {
        console.log('>> 内联 style 正确但渲染位置偏移 —— 典型症状：');
        console.log('   position:fixed 的包含块被某个祖先的 transform/filter/will-change/contain 改变了。');
        console.log('\n=== 逐级查祖先，找出那个"污染"了 fixed 包含块的元素 ===');
        const chain = await ev(`(() => {
          const pop = document.querySelector('.row-menu-pop');
          const out = [];
          let el = pop.parentElement;
          while (el && el !== document.documentElement) {
            const cs = getComputedStyle(el);
            const flags = [];
            if (cs.transform && cs.transform !== 'none') flags.push('transform=' + cs.transform);
            if (cs.filter && cs.filter !== 'none') flags.push('filter=' + cs.filter);
            if (cs.willChange && cs.willChange !== 'auto') flags.push('will-change=' + cs.willChange);
            if (cs.contain && cs.contain !== 'none') flags.push('contain=' + cs.contain);
            if (cs.backdropFilter && cs.backdropFilter !== 'none') flags.push('backdrop-filter=' + cs.backdropFilter);
            if (cs.perspective && cs.perspective !== 'none') flags.push('perspective=' + cs.perspective);
            if (cs.animationName && cs.animationName !== 'none') flags.push('animation=' + cs.animationName);
            if (flags.length) {
              const r = el.getBoundingClientRect();
              out.push({ tag: el.tagName, cls: (el.className || '').toString().slice(0, 60),
                         flags, rect: { left: Math.round(r.left), top: Math.round(r.top) } });
            }
            el = el.parentElement;
          }
          return JSON.stringify(out);
        })()`);
        console.log(chain);
      } else {
        console.log('>> 一致：公式与实现相符，问题在别处');
      }
    }
  } finally {
    try { chrome.kill(); } catch (e) {}
    server.close();
  }
}
main().catch((e) => { console.error(e); process.exit(1); });
