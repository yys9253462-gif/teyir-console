// 验证修复后的两个关键行为：
//   1. 容器不再出现竖向滚动条（overflow-y 必须是 hidden）
//   2. 「更多」菜单展开时**完整可见**、不被容器裁切
//      —— overflow-y:hidden 的最大风险就是切掉浮层
const { spawn } = require('child_process');
const fs = require('fs');
const http = require('http');
const path = require('path');
const os = require('os');

const CHROME = process.env.CHROME_BIN || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const DIST = path.join(__dirname, '..', 'dist');
const PORT = Number(process.env.UI_PORT || 19305);
const CDP_PORT = Number(process.env.UI_CDP_PORT || 19306);
const API_TARGET = process.env.API_TARGET || 'http://127.0.0.1:8081';
const TOKEN = process.env.NODEOPS_TOKEN || '';
const OUT = process.argv[2] || path.join(__dirname, 'sub-table-fixed.png');
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
    '--user-data-dir=' + fs.mkdtempSync(path.join(os.tmpdir(), 'cdp-fx-')), 'about:blank'], { stdio: 'ignore' });
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
    await sleep(2500);

    // 1) overflow-y 必须 hidden
    const oy = await ev(`(() => {
      const w = document.querySelector('table.sub-table')?.closest('.server-table-wrap');
      return w ? getComputedStyle(w).overflowY : 'missing';
    })()`);
    console.log('overflow-y =', oy);
    if (oy !== 'hidden') failures.push(`overflow-y 应为 hidden，实际 ${oy} —— 会冒出竖向滚动条`);

    // 2) 展开「更多」菜单，验证它完整可见。
    //
    // ⚠️ 这一段必须在**没有任何 Emulation 覆盖**的干净视口下跑。
    // 上一版把菜单测试放在逐档视口切换之后，结果 CDP 的
    // setDeviceMetricsOverride 让 window.innerWidth 报 1600，
    // 而实际布局宽度还是旧值 —— 于是按钮 right=1853、菜单被算到视口外，
    // 看着像"代码算错坐标"，其实是**测试环境的视口状态不一致**。
    // 现在把顺序倒过来：先测菜单（干净视口），再测各档视口。
    const cleanVp = await ev(`JSON.stringify({ w: window.innerWidth, h: window.innerHeight })`);
    console.log('视口（应无覆盖）:', cleanVp);

    const btnInfo = await ev(`(() => {
      const b = document.querySelector('.sub-table .row-menu .row-more');
      if (!b) return 'missing';
      const r = b.getBoundingClientRect();
      return JSON.stringify({ right: Math.round(r.right), bottom: Math.round(r.bottom), vw: window.innerWidth });
    })()`);
    console.log('更多按钮位置:', btnInfo);

    const opened = await ev(`(() => {
      const b = document.querySelector('.sub-table .row-menu .row-more');
      if (b) { b.click(); return true } return false
    })()`);
    await sleep(700);
    if (!opened) failures.push('点不开「更多」菜单');
    const menu = JSON.parse(await ev(`(() => {
      const pop = document.querySelector('.row-menu-pop');
      if (!pop) return JSON.stringify({ open: false });
      const r = pop.getBoundingClientRect();
      const cs = getComputedStyle(pop);
      return JSON.stringify({
        open: true,
        position: cs.position,
        menuW: Math.round(r.width), menuH: Math.round(r.height),
        rect: { top: Math.round(r.top), left: Math.round(r.left), right: Math.round(r.right), bottom: Math.round(r.bottom) },
        vw: window.innerWidth, vh: window.innerHeight,
        // ⚠️ 判据必须按 fixed 的语义来：它相对**视口**定位，本来就在容器之外。
        // 上一版用 "rect.bottom > wrap.bottom" 判"被容器裁切" ——
        // 那个判据对 fixed 元素必然为真（菜单天然在容器外），
        // 是**断言写错**而不是功能有问题（铁律⑨）。
        // fixed 真正要防的是"跑到视口外"，所以判据只能是视口包含关系。
        inViewport: r.left >= -1 && r.top >= -1 &&
                    r.right <= window.innerWidth + 1 && r.bottom <= window.innerHeight + 1,
        items: pop.querySelectorAll('button').length,
      });
    })()`) || '{}');
    console.log('\n=== 「更多」菜单 ===');
    console.log(JSON.stringify(menu, null, 2));
    if (!menu.open) failures.push('菜单没有打开');
    else {
      if (menu.position !== 'fixed') {
        failures.push(`菜单 position 应为 fixed（否则会被表格容器的 overflow-y:hidden 裁切），实际 ${menu.position}`);
      }
      if ((menu.items || 0) < 4) failures.push(`菜单项应为 4 个，实际 ${menu.items}`);
      if (!menu.inViewport) {
        failures.push(`菜单超出视口（rect=${JSON.stringify(menu.rect)} 视口=${menu.vw}x${menu.vh}）—— 有项点不到`);
      }
    }

    const shot = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: true });
    if (shot.result?.data) { fs.writeFileSync(OUT, Buffer.from(shot.result.data, 'base64')); console.log('\n截图:', OUT); }

    // 3) 窄屏下操作列仍然可见
    for (const width of [1200, 1000]) {
      await send('Emulation.setDeviceMetricsOverride', { width, height: 900, deviceScaleFactor: 1, mobile: false });
      await sleep(500);
      const r = JSON.parse(await ev(`(() => {
        const wrap = document.querySelector('table.sub-table')?.closest('.server-table-wrap');
        const act = wrap?.querySelector('tbody tr td:last-child');
        const wr = wrap.getBoundingClientRect(), ar = act.getBoundingClientRect();
        return JSON.stringify({ vOver: wrap.scrollHeight > wrap.clientHeight,
          visible: ar.right <= wr.right + 1 && ar.left >= wr.left - 1 });
      })()`) || '{}');
      console.log(`  ${width}px: 纵溢=${r.vOver} 操作列可见=${r.visible}`);
      if (r.vOver) failures.push(`${width}px 下仍出现纵向溢出（竖向滚动条）`);
      if (!r.visible) failures.push(`${width}px 下操作列不可见`);
    }
    await send('Emulation.setDeviceMetricsOverride', { width: 1600, height: 900, deviceScaleFactor: 1, mobile: false });
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
