// 3X-UI 面板页的真实渲染验收。
//
// 起一个假后端（同时服务 dist/ 与固定 /api/* 响应），用真实 Chrome 打开
// 「3X-UI 面板」页，验证三件事：
//   1. 导航里确实出现了新页签，点击后页面渲染出面板列表；
//   2. 点进抽屉能看到入站筛选、客户端、在线状态、用量与到期；
//   3. 浏览器控制台没有报错。
//
// 用法：node tests/xui-ui-verify.cjs
const { spawn } = require('child_process');
const fs = require('fs');
const http = require('http');
const path = require('path');
const os = require('os');

const CHROME = process.env.CHROME_BIN || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const DIST = path.join(__dirname, '..', 'dist');
const PORT = 19261;
const CDP_PORT = 19262;
const OUT = process.argv[2] || path.join(__dirname, 'xui-panels.png');

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ---- 假后端：静态 dist + 固定 API ----

const PANELS = [
  {
    id: '11111111-2222-3333-4444-555555555555',
    name: '香港-3XUI-01',
    base_url: 'https://hk1.example.com:2053',
    username: 'admin',
    last_status: 'ok',
    last_error: '',
    last_checked_at: new Date().toISOString(),
    inbounds_count: 2,
    clients_count: 35,
    online_count: 4,
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  },
  {
    id: 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee',
    name: '东京-3XUI-02',
    base_url: 'https://tokyo.example.com/xui',
    username: 'ops',
    last_status: 'error',
    last_error: '3X-UI 返回 401/403：会话已失效或凭据被拒绝，请重新保存面板凭据',
    last_checked_at: new Date().toISOString(),
    inbounds_count: 0,
    clients_count: 0,
    online_count: 0,
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  },
];

const CLIENTS = [
  { email: 'vip@example.com', client_id: 'uuid-vip', inbound_id: 1, inbound_name: '香港主入口', protocol: 'vless', online: true, up: 3 * 1024 ** 3, down: 12 * 1024 ** 3, total: 100 * 1024 ** 3, expiry_time: Date.now() + 12 * 86400000 },
  { email: 'trial-a@example.com', client_id: 'uuid-ta', inbound_id: 1, inbound_name: '香港主入口', protocol: 'vless', online: false, up: 512 * 1024 ** 2, down: 2 * 1024 ** 3, total: 0, expiry_time: 0 },
  { email: 'soon@example.com', client_id: 'pw-1', inbound_id: 2, inbound_name: '香港备用', protocol: 'trojan', online: true, up: 900 * 1024 ** 2, down: 4 * 1024 ** 3, total: 5 * 1024 ** 3, expiry_time: Date.now() + 3 * 86400000 },
];

const INBOUNDS = [
  { id: 1, port: 443, protocol: 'vless', remark: '香港主入口', enable: true, up: 3.5 * 1024 ** 3, down: 14 * 1024 ** 3, total: 0, expiryTime: 0, clients: [] },
  { id: 2, port: 8443, protocol: 'trojan', remark: '香港备用', enable: true, up: 900 * 1024 ** 2, down: 4 * 1024 ** 3, total: 200 * 1024 ** 3, expiryTime: 0, clients: [] },
];

const ROUTES = {
  '/api/health': { status: 'ok' },
  '/api/servers': [],
  '/api/tasks': [],
  '/api/nodes': [],
  '/api/alerts': [],
  '/api/traffic/hourly': [],
  '/api/metrics/recent': {},
  '/api/xui/panels': PANELS,
  [`/api/xui/panels/${PANELS[0].id}/inbounds`]: { panel_id: PANELS[0].id, panel_name: PANELS[0].name, inbounds: INBOUNDS, clients: CLIENTS, onlines: ['vip@example.com'] },
};

const MIME = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' };

const server = http.createServer((req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${PORT}`);
  if (url.pathname.startsWith('/api/')) {
    const body = ROUTES[url.pathname];
    if (body === undefined) { res.writeHead(404); res.end('{}'); return; }
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify(body));
    return;
  }
  const rel = url.pathname === '/' ? '/index.html' : url.pathname;
  const file = path.join(DIST, rel);
  if (!fs.existsSync(file)) { res.writeHead(404); res.end('not found'); return; }
  res.writeHead(200, { 'Content-Type': MIME[path.extname(file)] || 'application/octet-stream' });
  res.end(fs.readFileSync(file));
});

function getJSON(url) {
  return new Promise((resolve, reject) => {
    http.get(url, (res) => { let b = ''; res.on('data', (d) => (b += d)); res.on('end', () => { try { resolve(JSON.parse(b)); } catch (e) { reject(e); } }); }).on('error', reject);
  });
}

// ---- 主流程 ----

async function main() {
  await new Promise((r) => server.listen(PORT, '127.0.0.1', r));
  console.log(`假后端就绪 http://127.0.0.1:${PORT}`);

  const chrome = spawn(CHROME, [
    `--remote-debugging-port=${CDP_PORT}`,
    '--headless=new', '--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage',
    '--window-size=1400,1800',
    '--user-data-dir=' + fs.mkdtempSync(path.join(os.tmpdir(), 'cdp-xui-')),
    'about:blank',
  ], { stdio: 'ignore' });

  let ws;
  const failures = [];
  const consoleErrors = [];

  try {
    await sleep(3500);
    const targets = await getJSON(`http://127.0.0.1:${CDP_PORT}/json`);
    const page = targets.find((t) => t.type === 'page');
    const WebSocket = require('ws');
    ws = new WebSocket(page.webSocketDebuggerUrl);
    await new Promise((r) => ws.on('open', r));

    let id = 0;
    const pending = new Map();
    ws.on('message', (raw) => {
      const msg = JSON.parse(raw);
      if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg); pending.delete(msg.id); }
      if (msg.method === 'Runtime.consoleAPICalled' && msg.params.type === 'error') {
        consoleErrors.push(msg.params.args.map((a) => a.value ?? a.description ?? '').join(' '));
      }
      if (msg.method === 'Runtime.exceptionThrown') {
        consoleErrors.push('exception: ' + (msg.params.exceptionDetails?.text ?? ''));
      }
    });
    const send = (method, params = {}) => new Promise((resolve) => { const mid = ++id; pending.set(mid, resolve); ws.send(JSON.stringify({ id: mid, method, params })); });

    await send('Runtime.enable');
    await send('Page.enable');

    // 先写入会话，跳过登录页。
    const origin = `http://127.0.0.1:${PORT}`;
    await send('Page.navigate', { url: origin + '/' });
    await sleep(1200);
    await send('Runtime.evaluate', {
      expression: `localStorage.setItem('nodeops.session', JSON.stringify({token:'fake-token',email:'admin@example.com',role:'admin'}))`,
    });
    await send('Page.navigate', { url: origin + '/' });
    await sleep(2500);

    // 1. 导航里应有「3X-UI 面板」。
    const navCheck = await send('Runtime.evaluate', {
      expression: `[...document.querySelectorAll('.nav-item')].map(b => b.textContent.trim()).join(' | ')`,
      returnByValue: true,
    });
    const navText = navCheck.result?.result?.value ?? '';
    console.log('导航项:', navText);
    if (!navText.includes('3X-UI 面板')) failures.push('导航里没有出现「3X-UI 面板」');

    // 2. 点击该页签。
    await send('Runtime.evaluate', {
      expression: `(() => { const b = [...document.querySelectorAll('.nav-item')].find(x => x.textContent.includes('3X-UI 面板')); if (b) { b.click(); return true } return false })()`,
      returnByValue: true,
    });
    await sleep(1800);

    const pageInfo = await send('Runtime.evaluate', {
      expression: `(() => {
        const h1 = document.querySelector('.content-wrap h1')?.textContent ?? '';
        const rows = [...document.querySelectorAll('.server-table tbody tr')].map(r => r.innerText.replace(/\\n/g, ' / '));
        const badges = [...document.querySelectorAll('.status-badge')].map(b => b.innerText.trim());
        return JSON.stringify({ h1, rows, badges, bodyLen: document.body.innerText.length });
      })()`,
      returnByValue: true,
    });
    const info = JSON.parse(pageInfo.result?.result?.value ?? '{}');
    console.log('页面标题:', info.h1);
    console.log('面板行数:', (info.rows || []).length);
    (info.rows || []).forEach((r) => console.log('  ·', r));
    console.log('状态徽章:', (info.badges || []).join(', '));

    if (!String(info.h1).includes('3X-UI')) failures.push('页面标题不含 3X-UI');
    if ((info.rows || []).length !== 2) failures.push(`期望 2 行面板，实际 ${(info.rows || []).length}`);
    if (!(info.badges || []).some((b) => b.includes('已连接'))) failures.push('缺少「已连接」状态徽章');
    if (!(info.badges || []).some((b) => b.includes('连接失败'))) failures.push('缺少「连接失败」状态徽章（错误态面板）');

    // 列表页整页截图（抽屉打开前）。
    const listShot = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: true });
    if (listShot.result?.data) {
      fs.writeFileSync(OUT.replace(/\.png$/, '-list.png'), Buffer.from(listShot.result.data, 'base64'));
      console.log('列表页截图:', OUT.replace(/\.png$/, '-list.png'));
    }

    // 3. 点进第一行，打开抽屉。
    await send('Runtime.evaluate', { expression: `document.querySelector('.server-table tbody tr')?.click()` });
    await sleep(2200);

    const drawer = await send('Runtime.evaluate', {
      expression: `(() => {
        const d = document.querySelector('.detail-drawer');
        if (!d) return JSON.stringify({ open: false });
        const text = d.innerText;
        const cards = [...d.querySelectorAll('.xui-client-card')];
        const first = cards[0];
        return JSON.stringify({
          open: true,
          hasInboundFilter: text.includes('按入站筛选'),
          hasClients: text.includes('CLIENTS'),
          hasOnlineBadge: text.includes('在线'),
          hasUsage: text.includes('GB'),
          hasExpiry: text.includes('剩') || text.includes('永不过期'),
          hasAddClient: text.includes('新增客户端'),
          clientCards: cards.length,
          // 卡片内部信息必须分层，不能糊成一坨。
          cardLayers: first ? {
            head: !!first.querySelector('.xui-client-head strong'),
            meta: !!first.querySelector('.xui-client-meta'),
            usage: !!first.querySelector('.xui-client-usage'),
            actions: first.querySelectorAll('.xui-client-actions button').length,
          } : null,
          usageBars: d.querySelectorAll('.xui-usage-bar').length,
          excerpt: text.split('\\n').filter(Boolean).slice(0, 30).join(' / '),
        });
      })()`,
      returnByValue: true,
    });
    const d = JSON.parse(drawer.result?.result?.value ?? '{}');
    console.log('抽屉已打开:', d.open);
    console.log('客户端卡片数:', d.clientCards, '| 用量条:', d.usageBars);
    console.log('首张卡片分层:', JSON.stringify(d.cardLayers));
    console.log('抽屉摘要:', d.excerpt);

    if (!d.open) failures.push('点击面板行后抽屉没有打开');
    else {
      if (!d.hasInboundFilter) failures.push('抽屉缺少入站筛选');
      if (!d.hasClients) failures.push('抽屉缺少客户端区块');
      if (!d.hasOnlineBadge) failures.push('抽屉缺少在线标记');
      if (!d.hasExpiry) failures.push('抽屉缺少到期展示');
      if (!d.hasAddClient) failures.push('抽屉缺少「新增客户端」入口');
      if ((d.clientCards || 0) < 3) failures.push(`期望至少 3 张客户端卡片，实际 ${d.clientCards}`);
      if (!d.cardLayers?.head) failures.push('客户端卡片缺少邮箱标题');
      if (!d.cardLayers?.meta) failures.push('客户端卡片缺少归属信息行');
      if (!d.cardLayers?.usage) failures.push('客户端卡片缺少用量行');
      if ((d.cardLayers?.actions || 0) !== 2) failures.push('客户端卡片操作按钮数不为 2');
      // 三个客户端里两个有配额上限，应渲染出两条用量进度条。
      if ((d.usageBars || 0) !== 2) failures.push(`期望 2 条用量进度条，实际 ${d.usageBars}`);
    }

    // 4. 新增客户端对话框。
    if (d.open) {
      await send('Runtime.evaluate', {
        expression: `(() => { const b = [...document.querySelectorAll('.detail-drawer button')].find(x => x.textContent.includes('新增客户端')); if (b) b.click() })()`,
      });
      await sleep(1200);
      const modal = await send('Runtime.evaluate', {
        expression: `(() => {
          const m = document.querySelector('.modal-card');
          if (!m) return JSON.stringify({ open: false });
          const selects = m.querySelectorAll('select').length;
          const inputs = [...m.querySelectorAll('input')].map(i => i.placeholder || i.value);
          return JSON.stringify({ open: true, selects, inputs, text: m.innerText.split('\\n').filter(Boolean).slice(0,12).join(' / ') });
        })()`,
        returnByValue: true,
      });
      const md = JSON.parse(modal.result?.result?.value ?? '{}');
      console.log('新增客户端弹窗:', md.open, '| 入站下拉:', md.selects, '| 输入框:', (md.inputs || []).join(', '));
      if (!md.open) failures.push('「新增客户端」没有打开弹窗');
      else if (!md.selects) failures.push('新增客户端弹窗缺少入站下拉');
    }

    // 截图（关掉弹窗，回到列表 + 抽屉状态）。
    await send('Runtime.evaluate', { expression: `document.querySelector('.modal-head .icon-button')?.click()` });
    await sleep(600);
    const shot = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: true });
    if (shot.result?.data) {
      fs.writeFileSync(OUT, Buffer.from(shot.result.data, 'base64'));
      console.log('截图已保存:', OUT);
    }
  } catch (err) {
    failures.push('脚本异常: ' + err.message);
  } finally {
    try { ws?.close(); } catch {}
    chrome.kill();
    server.close();
  }

  if (consoleErrors.length) {
    console.log('\n控制台错误:');
    consoleErrors.slice(0, 10).forEach((e) => console.log('  !', e));
    failures.push(`浏览器控制台有 ${consoleErrors.length} 条错误`);
  } else {
    console.log('\n控制台无错误');
  }

  if (failures.length) {
    console.log('\n失败项:');
    failures.forEach((f) => console.log('  ✗', f));
    process.exit(1);
  }
  console.log('\n全部通过');
}

main();
