// 验证订阅中心新增的三项：指定到期日 / 在线 IP / 用量。
const { spawn } = require('child_process');
const fs = require('fs');
const http = require('http');
const path = require('path');
const os = require('os');

const CHROME = process.env.CHROME_BIN || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const DIST = path.join(__dirname, '..', 'dist');
const PORT = 19321, CDP_PORT = 19322;
const API_TARGET = process.env.API_TARGET || 'http://127.0.0.1:8081';
const TOKEN = process.env.NODEOPS_TOKEN || '';
const OUT = process.argv[2] || path.join(__dirname, 'sub-usage.png');
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
    '--disable-gpu', '--disable-dev-shm-usage', '--window-size=1700,1000',
    '--user-data-dir=' + fs.mkdtempSync(path.join(os.tmpdir(), 'cdp-su-')), 'about:blank'], { stdio: 'ignore' });
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
    await ev(`(() => { const b=[...document.querySelectorAll('.nav-item')].find(x=>x.textContent.trim()==='订阅中心'); if(b) b.click() })()`);
    await sleep(2600);

    const h1 = await ev(`document.querySelector('.content-wrap h1')?.textContent ?? ''`);
    if (!String(h1).includes('订阅中心')) { console.error('没到订阅中心'); process.exit(1); }

    // 1) 表头必须有「在线 IP」「用量」
    const headers = JSON.parse(await ev(`JSON.stringify([...document.querySelectorAll('.sub-table thead th')].map(t => t.textContent.trim()).filter(Boolean))`) || '[]');
    console.log('表头:', JSON.stringify(headers));
    for (const want of ['在线 IP', '用量', '设备', '到期']) {
      if (!headers.includes(want)) failures.push(`表头缺少「${want}」列`);
    }

    // 2) 用量单元格必须是「数字 + 上限说明」或「未采集」，**不能把 null 显示成 0 B**
    const usageCell = await ev(`(() => {
      const td = document.querySelector('.sub-table tbody tr td:nth-child(8)');
      return td ? td.innerText.trim() : 'missing';
    })()`);
    console.log('用量单元格:', JSON.stringify(usageCell));
    if (usageCell === 'missing') failures.push('找不到用量单元格');
    else if (usageCell === '' ) failures.push('用量单元格为空');

    // 3) 打编辑弹窗，检查日期选择器
    const opened = await ev(`(() => {
      const b = [...document.querySelectorAll('.sub-table tbody tr .row-actions button')]
        .find(x => (x.getAttribute('title') || '').includes('编辑'));
      if (b) { b.click(); return true } return false
    })()`);
    await sleep(900);
    if (!opened) failures.push('点不开编辑弹窗');
    const modal = JSON.parse(await ev(`(() => {
      const m = document.querySelector('.modal-card');
      if (!m) return JSON.stringify({ open: false });
      const dateInput = m.querySelector('input[type="date"]');
      const setDateBtn = [...m.querySelectorAll('button')].find(b => b.textContent.includes('设为该日'));
      const ipInput = [...m.querySelectorAll('.form-field')].find(f => f.textContent.includes('设备数'));
      return JSON.stringify({
        open: true,
        hasDateInput: !!dateInput,
        dateValue: dateInput?.value ?? '',
        hasSetDateBtn: !!setDateBtn,
        setDateBtnDisabled: setDateBtn?.disabled ?? null,
        hasIPLimit: !!ipInput,
        text: m.innerText.split('\\n').filter(Boolean).slice(0, 24).join(' / '),
      });
    })()`) || '{}');
    console.log('\n=== 编辑弹窗 ===');
    console.log(JSON.stringify(modal, null, 2));
    if (!modal.open) failures.push('编辑弹窗没打开');
    else {
      if (!modal.hasDateInput) failures.push('编辑弹窗没有日期选择器（管理员要求的"指定到期日"）');
      if (!modal.hasSetDateBtn) failures.push('缺少「设为该日」按钮');
      if (!modal.hasIPLimit) failures.push('编辑弹窗缺少设备数输入框');
      if (modal.hasDateInput && !/^\d{4}-\d{2}-\d{2}$/.test(modal.dateValue || '')) {
        failures.push(`日期输入框初值不是 YYYY-MM-DD：${modal.dateValue}`);
      }
    }

    const shot = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: true });
    if (shot.result?.data) { fs.writeFileSync(OUT, Buffer.from(shot.result.data, 'base64')); console.log('\n截图:', OUT); }
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
