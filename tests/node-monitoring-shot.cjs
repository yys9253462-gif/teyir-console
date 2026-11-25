// 用无头 Chrome 真实渲染「节点监控」页并截图 + 抓 DOM 事实。
//
// 目的（对应记忆里的两条铁律）：
//   ① 改完 UI 一律截图自检 —— 自动化测试全绿但"观感上错误"只有眼睛能发现；
//   ② 断言要抓 DOM 事实，不能只看截图好看。
//
// 这里同时做三件事：
//   - 截图（人工看）
//   - 抓关键文本断言（机器判）
//   - 收集 console 错误与未捕获异常（这类问题不会让页面白屏，
//     只会让某个数字不显示，测试很容易漏）
//
// 用法：
//   node tests/node-monitoring-shot.cjs <email> <password> [输出png]
const { spawn } = require('child_process');
const fs = require('fs');
const http = require('http');
const path = require('path');
const os = require('os');

const CHROME = process.env.CHROME_BIN
  || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const EMAIL = process.argv[2];
const PASSWORD = process.argv[3];
const OUT = process.argv[4] || path.join(__dirname, 'node-monitoring.png');
const PORT = 19241;
const ORIGIN = process.env.PANEL_ORIGIN || 'http://127.0.0.1:8082';
// API 端口（用于登录拿 token）。与 ORIGIN 不同端口。
// 缺省「同端口」——本机跑时 web/api 都在 8082；走 Windows 隧道时
// 改成不同端口（脚本作者注：之前硬编码 18097 导致换主机就崩）。
const API_ORIGIN = process.env.PANEL_API_ORIGIN || ORIGIN;

function sleep(ms) { return new Promise(r => setTimeout(r, ms)); }

function getJSON(url) {
  return new Promise((resolve, reject) => {
    http.get(url, res => {
      let b = '';
      res.on('data', d => b += d);
      res.on('end', () => { try { resolve(JSON.parse(b)); } catch (e) { reject(e); } });
    }).on('error', reject);
  });
}

// 先用真密码换 token，再注入 localStorage。
// 不把 token 写死在脚本里 —— 它一小时后过期，写死会让脚本某天突然失效。
function login() {
  return new Promise((resolve, reject) => {
    const body = JSON.stringify({ email: EMAIL, password: PASSWORD });
    // 从 API_ORIGIN 解析 host/port，避免硬编码。
    const u = new URL(API_ORIGIN + '/api/login');
    const req = http.request({
      hostname: u.hostname, port: u.port, path: u.pathname, method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) },
    }, res => {
      let b = '';
      res.on('data', d => b += d);
      res.on('end', () => {
        try { resolve(JSON.parse(b)); } catch (e) { reject(new Error(b.slice(0, 200))); }
      });
    });
    req.on('error', reject);
    req.write(body);
    req.end();
  });
}

async function main() {
  if (!EMAIL || !PASSWORD) { console.error('用法: node node-monitoring-shot.cjs <email> <password> [png]'); process.exit(1); }
  if (!fs.existsSync(CHROME)) { console.error('找不到 Chrome：' + CHROME); process.exit(1); }

  const loginResult = await login();
  if (!loginResult.token) { console.error('登录失败:', JSON.stringify(loginResult).slice(0, 200)); process.exit(1); }
  console.log('登录成功，token 长度', loginResult.token.length);

  const userDataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'cdp-mon-'));
  const chrome = spawn(CHROME, [
    `--remote-debugging-port=${PORT}`,
    '--headless=new', '--no-sandbox', '--disable-gpu',
    '--disable-dev-shm-usage',
    '--window-size=1600,1400',
    '--hide-scrollbars',
    '--user-data-dir=' + userDataDir,
    'about:blank',
  ], { stdio: 'ignore' });

  try {
    await sleep(3500);
    const targets = await getJSON(`http://127.0.0.1:${PORT}/json`);
    const page = targets.find(t => t.type === 'page');
    if (!page) throw new Error('没找到 page target');

    const WebSocket = require('ws');
    const ws = new WebSocket(page.webSocketDebuggerUrl);
    await new Promise(r => ws.on('open', r));

    let id = 0;
    const pending = new Map();
    const consoleErrors = [];
    const exceptions = [];
    ws.on('message', m => {
      const msg = JSON.parse(m);
      if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg); pending.delete(msg.id); return; }
      if (msg.method === 'Runtime.consoleAPICalled' && msg.params.type === 'error') {
        consoleErrors.push((msg.params.args || []).map(a => a.value || a.description || '').join(' ').slice(0, 300));
      }
      if (msg.method === 'Runtime.exceptionThrown') {
        const d = msg.params.exceptionDetails;
        exceptions.push((d.exception && d.exception.description) || d.text || '未知异常');
      }
    });

    const send = (method, params) => new Promise(resolve => {
      const msgId = ++id;
      pending.set(msgId, resolve);
      ws.send(JSON.stringify({ id: msgId, method, params: params || {} }));
    });

    await send('Runtime.enable');
    await send('Page.enable');

    // 先打开一次同源页面，才能往 localStorage 写登录态。
    await send('Page.navigate', { url: ORIGIN + '/' });
    await sleep(2000);

    const session = JSON.stringify({ token: loginResult.token, email: EMAIL, role: 'admin' });
    // 键名必须是 'nodeops.session' —— 与 src/lib/api.ts 的 TOKEN_KEY 一致。
    // 写错的话页面会停在登录页，而截图里看起来"只是没登录"，
    // 很容易被误判成"截图脚本坏了"而不是"键名写错了"。
    await send('Runtime.evaluate', {
      expression: `localStorage.setItem('nodeops.session', ${JSON.stringify(session)}); 'ok'`,
    });

    // 带登录态重新加载。
    await send('Page.navigate', { url: ORIGIN + '/' });
    await sleep(5000);

    // 抓 DOM 事实。断言点选的是「这个页面必须显示的东西」：
    // 页面标题、三网卡片、节点卡片的延迟数字。
    //
    // ⚠️ 取值路径：CDP 的 Runtime.evaluate 返回
    //   { result: { result: { type, value } } }
    // 外层 result 是命令信封，内层 result 才是求值结果。
    // 只取一层会拿到 undefined —— 我第一版就写错了，
    // 表现为 "undefined is not valid JSON"，而截图其实是成功的。
    const facts = await send('Runtime.evaluate', {
      expression: `JSON.stringify({
        title: document.querySelector('h1') ? document.querySelector('h1').innerText : '',
        navActive: document.querySelector('.nav-item.active') ? document.querySelector('.nav-item.active').innerText : '',
        metricCards: Array.from(document.querySelectorAll('.metric-card')).map(e => e.innerText.replace(/\\n/g, ' ')),
        carrierCards: Array.from(document.querySelectorAll('.carrier-card')).map(e => e.innerText.replace(/\\n/g, ' ')),
        nodeCards: Array.from(document.querySelectorAll('.node-card')).map(e => e.innerText.replace(/\\n/g, ' ')),
        hasUndefined: document.body.innerText.includes('undefined'),
        hasNaN: document.body.innerText.includes('NaN'),
        hasObjectObject: document.body.innerText.includes('[object Object]'),
        bodySample: document.body.innerText.slice(0, 900),
      })`,
      returnByValue: true,
    });

    const shot = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: true });
    if (shot.result && shot.result.data) {
      fs.writeFileSync(OUT, Buffer.from(shot.result.data, 'base64'));
      console.log('已截图:', OUT, fs.statSync(OUT).size, '字节');
    } else {
      console.error('截图失败:', JSON.stringify(shot).slice(0, 300));
    }

    const raw = facts && facts.result && facts.result.result && facts.result.result.value;
    if (!raw) {
      console.error('DOM 抓取失败，原始返回:', JSON.stringify(facts).slice(0, 400));
      ws.close();
      return;
    }
    const data = JSON.parse(raw);
    console.log('\n===== DOM 事实 =====');
    console.log('页面标题:', data.title);
    console.log('高亮导航项:', data.navActive);
    console.log('指标卡:', data.metricCards.length, '个');
    data.metricCards.forEach(c => console.log('   ·', c.slice(0, 90)));
    console.log('三网卡片:', data.carrierCards.length, '个');
    data.carrierCards.forEach(c => console.log('   ·', c.slice(0, 90)));
    console.log('节点卡:', data.nodeCards.length, '个');
    data.nodeCards.forEach(c => console.log('   ·', c.slice(0, 160)));

    console.log('\n===== 渲染健康检查 =====');
    console.log('含 undefined:', data.hasUndefined ? '是 [!!]' : '否 [OK]');
    console.log('含 NaN:', data.hasNaN ? '是 [!!]' : '否 [OK]');
    console.log('含 [object Object]:', data.hasObjectObject ? '是 [!!]' : '否 [OK]');
    console.log('console 错误数:', consoleErrors.length);
    consoleErrors.slice(0, 5).forEach(e => console.log('   !', e));
    console.log('未捕获异常数:', exceptions.length);
    exceptions.slice(0, 5).forEach(e => console.log('   !', String(e).slice(0, 200)));

    console.log('\n===== 页面正文（前 800 字）=====');
    console.log(data.bodySample);

    ws.close();
  } finally {
    chrome.kill();
    try { fs.rmSync(userDataDir, { recursive: true, force: true }); } catch (e) { /* 忽略 */ }
  }
}

main().catch(e => { console.error('失败:', e); process.exit(1); });
