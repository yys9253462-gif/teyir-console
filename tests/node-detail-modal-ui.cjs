// 验证节点详情改成了弹窗，且点开就能看到（不用滚）。
const { spawn } = require('child_process');
const fs = require('fs');
const http = require('http');
const path = require('path');
const os = require('os');

const CHROME = process.env.CHROME_BIN || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const DIST = path.join(__dirname, '..', 'dist');
const PORT = 19341, CDP_PORT = 19342;
const API_TARGET = process.env.API_TARGET || 'http://127.0.0.1:8081';
const TOKEN = process.env.NODEOPS_TOKEN || '';
const OUT = process.argv[2] || path.join(__dirname, 'node-detail-modal.png');
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
    '--user-data-dir=' + fs.mkdtempSync(path.join(os.tmpdir(), 'cdp-nm-')), 'about:blank'], { stdio: 'ignore' });
  const failures = [];
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

    console.log('弹窗打开前页面高度:', await ev(`document.body.scrollHeight`));

    const clicked = await ev(`(() => { const c = document.querySelector('.node-card'); if (c) { c.click(); return true } return false })()`);
    if (!clicked) failures.push('点不到节点卡');
    await sleep(900);

    const geo = JSON.parse(await ev(`(() => {
      const card = document.querySelector('.modal-card');
      const bd = document.querySelector('.modal-backdrop');
      const inner = document.querySelector('.node-detail');
      if (!card) return JSON.stringify({ exists: false });
      const cs = getComputedStyle(card);
      const r = card.getBoundingClientRect();
      const bcs = bd ? getComputedStyle(bd) : null;
      const ir = inner ? inner.getBoundingClientRect() : null;
      return JSON.stringify({
        exists: true,
        position: cs.position,
        maxHeight: cs.maxHeight,
        overflowY: cs.overflowY,
        rect: { top: Math.round(r.top), left: Math.round(r.left), bottom: Math.round(r.bottom), height: Math.round(r.height) },
        backdropPosition: bcs ? bcs.position : null,
        innerHeight: ir ? Math.round(ir.height) : 0,
        viewport: { w: window.innerWidth, h: window.innerHeight },
        // 关键：弹窗是否**完整落在视口内**（不用滚动就能看到）
        fullyInViewport: r.top >= -1 && r.bottom <= window.innerHeight + 1,
        scrollable: card.scrollHeight > card.clientHeight,
        title: document.querySelector('.modal-head h2')?.textContent ?? '',
        hasBasic: !!document.querySelector('.node-detail .detail-section'),
        sections: document.querySelectorAll('.node-detail .detail-section').length,
      });
    })()`) || '{}');
    console.log('\n=== 弹窗实测 ===');
    console.log(JSON.stringify(geo, null, 2));

    if (!geo.exists) failures.push('没出现弹窗（.modal-card）');
    else {
      // ⚠️ 不要断言 .modal-card 的 position 是 fixed —— 它是
      // .modal-backdrop（fixed）的**子元素**，靠 flex 居中，本身就该是 static。
      // 第一版写了这条断言，结果功能全对却报 FAIL（是断言错，不是代码错，铁律⑨）。
      // 真正要验的是：**遮罩定位正确 + 弹窗完整落在视口内**。
      if (geo.backdropPosition !== 'fixed') {
        failures.push(`遮罩 position 应为 fixed，实际 ${geo.backdropPosition} —— 遮罩不固定会随页面滚走`);
      }
      if (!geo.fullyInViewport) {
        failures.push(`弹窗没有完整落在视口内（rect=${JSON.stringify(geo.rect)} 视口高=${geo.viewport.h}）—— ` +
          '这正是改造前的问题：点完看不到，得往下滚');
      }
      if (geo.overflowY !== 'auto') {
        failures.push(`弹窗内容超出时必须可滚动（overflow-y 应为 auto），实际 ${geo.overflowY}`);
      }
      if ((geo.sections || 0) < 3) failures.push(`弹窗内容区块太少（${geo.sections}），可能没渲染出来`);
      if (!geo.title) failures.push('弹窗没有标题');
    }

    // Esc 关闭
    await send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
    await send('Input.dispatchKeyEvent', { type: 'keyUp', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
    await sleep(600);
    const closed = await ev(`!document.querySelector('.modal-card')`);
    console.log('Esc 能关闭:', closed);
    if (!closed) failures.push('Esc 关不掉弹窗');

    // 重新打开截图
    await ev(`document.querySelector('.node-card').click()`);
    await sleep(800);
    const shot = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: false });
    if (shot.result?.data) { fs.writeFileSync(OUT, Buffer.from(shot.result.data, 'base64')); console.log('截图:', OUT); }
  } finally {
    try { chrome.kill(); } catch (e) {}
    server.close();
  }
  console.log('\n=========== 结果 ===========');
  if (failures.length === 0) console.log('PASS');
  else { console.log('FAIL'); failures.forEach((f) => console.log('  ✗ ' + f)); }
  process.exit(failures.length === 0 ? 0 : 1);
}
main().catch((e) => { console.error(e); process.exit(1); });
