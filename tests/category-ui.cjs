// 打开「分类管理」弹窗并截图，确认界面可用。
const { spawn } = require('child_process');
const fs = require('fs');
const http = require('http');
const path = require('path');
const os = require('os');

const CHROME = process.env.CHROME_BIN || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const DIST = path.join(__dirname, '..', 'dist');
const PORT = 19371, CDP_PORT = 19372;
const API_TARGET = process.env.API_TARGET || 'http://127.0.0.1:8081';
const TOKEN = process.env.NODEOPS_TOKEN || '';
const OUT = process.env.SHOT_DIR || path.join(__dirname, 'shots');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const MIME = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript', '.css': 'text/css', '.png': 'image/png' };
fs.mkdirSync(OUT, { recursive: true });

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

const fails = [];
async function main() {
  await new Promise((r) => server.listen(PORT, '127.0.0.1', r));
  const chrome = spawn(CHROME, [`--remote-debugging-port=${CDP_PORT}`, '--headless=new', '--no-sandbox',
    '--disable-gpu', '--disable-dev-shm-usage', '--window-size=1600,900',
    '--user-data-dir=' + fs.mkdtempSync(path.join(os.tmpdir(), 'cdp-cat-')), 'about:blank'], { stdio: 'ignore' });
  try {
    await sleep(3200);
    const targets = await getJSON(`http://127.0.0.1:${CDP_PORT}/json`);
    const page = targets.find((t) => t.type === 'page');
    const WebSocket = require('ws');
    const ws = new WebSocket(page.webSocketDebuggerUrl);
    await new Promise((r) => ws.on('open', r));
    let id = 0; const pending = new Map(); const errs = [];
    ws.on('message', (raw) => {
      const m = JSON.parse(raw);
      if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
      if (m.method === 'Runtime.consoleAPICalled' && m.params.type === 'error') {
        errs.push((m.params.args || []).map((a) => a.value || '').join(' ').slice(0, 150));
      }
    });
    const send = (m, p = {}) => new Promise((res) => { const mid = ++id; pending.set(mid, res); ws.send(JSON.stringify({ id: mid, method: m, params: p })); });
    const ev = async (e) => (await send('Runtime.evaluate', { expression: e, returnByValue: true })).result?.result?.value;

    await send('Runtime.enable'); await send('Page.enable');
    const origin = `http://127.0.0.1:${PORT}`;
    await send('Page.navigate', { url: origin + '/' }); await sleep(1200);
    await ev(`localStorage.setItem('nodeops.session', JSON.stringify({token:'${TOKEN}',email:'o@t.com',role:'admin'}))`);
    await send('Page.navigate', { url: origin + '/' }); await sleep(2200);

    // 进 HY2 节点页
    await ev(`(() => { const b=[...document.querySelectorAll('.nav-item')].find(x=>x.textContent.trim()==='HY2 节点'); if(b) b.click() })()`);
    await sleep(2600);

    // 点「分类管理」
    const opened = await ev(`(() => {
      const b = [...document.querySelectorAll('button')].find(x => x.textContent.includes('分类管理'));
      if (b) { b.click(); return true } return false
    })()`);
    if (!opened) { fails.push('点不开「分类管理」'); }
    await sleep(1200);

    const info = JSON.parse(await ev(`(() => {
      const m = document.querySelector('.modal-card');
      if (!m) return JSON.stringify({open:false});
      const r = m.getBoundingClientRect();
      return JSON.stringify({
        open: true,
        title: (m.querySelector('h2')||{}).textContent || '',
        categories: m.querySelectorAll('.category-row').length,
        hasCreateForm: !!m.querySelector('input'),
        inViewport: r.top >= -1 && r.bottom <= window.innerHeight + 1,
        scrollable: m.scrollHeight > m.clientHeight,
        text: m.innerText.slice(0, 300),
      });
    })()`) || '{}');
    console.log('分类管理弹窗:');
    console.log(JSON.stringify(info, null, 2));

    if (!info.open) fails.push('分类管理弹窗没打开');
    else {
      if (info.categories < 3) fails.push(`分类行应有 3 个，实际 ${info.categories}`);
      if (!info.hasCreateForm) fails.push('缺少新建表单');
      if (!info.inViewport) fails.push('弹窗超出视口');
    }

    // 截图
    const shot = await send('Page.captureScreenshot', { format: 'png' });
    if (shot.result?.data) {
      const f = path.join(OUT, 'category-manager.png');
      fs.writeFileSync(f, Buffer.from(shot.result.data, 'base64'));
      console.log('截图:', f);
    }

    // 再打开一个节点的编辑弹窗，看分类勾选
    await send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
    await send('Input.dispatchKeyEvent', { type: 'keyUp', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
    await sleep(700);
    await ev(`(() => {
      const b = [...document.querySelectorAll('.node-table tbody tr .row-actions button')]
        .find(x => (x.getAttribute('title') || '') === '编辑节点');
      if (b) b.click()
    })()`);
    await sleep(1000);
    const editInfo = JSON.parse(await ev(`(() => {
      const m = document.querySelector('.modal-card');
      if (!m) return JSON.stringify({open:false});
      return JSON.stringify({
        open: true,
        picker: m.querySelectorAll('.cat-picker label').length,
        pickerText: (m.querySelector('.cat-picker')||{}).innerText || '',
      });
    })()`) || '{}');
    console.log('\n节点编辑弹窗的分类勾选:');
    console.log(JSON.stringify(editInfo, null, 2));
    if (!editInfo.open) fails.push('编辑弹窗没打开');
    else if (editInfo.picker < 3) fails.push(`分类勾选项应有 3 个，实际 ${editInfo.picker}`);

    const shot2 = await send('Page.captureScreenshot', { format: 'png' });
    if (shot2.result?.data) {
      fs.writeFileSync(path.join(OUT, 'node-category-picker.png'), Buffer.from(shot2.result.data, 'base64'));
      console.log('截图:', path.join(OUT, 'node-category-picker.png'));
    }

    if (errs.length) fails.push(`控制台报错 ${errs.length} 条: ${errs[0]}`);
    console.log('\n=========== 结果 ===========');
    console.log(fails.length ? 'FAIL\n  ✗ ' + fails.join('\n  ✗ ') : 'PASS —— 分类界面可用');
  } finally {
    try { chrome.kill(); } catch (e) {}
    server.close();
  }
  process.exit(fails.length ? 1 : 0);
}
main().catch((e) => { console.error(e); process.exit(1); });
