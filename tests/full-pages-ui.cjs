// 前端全页面渲染与交互测试。
//
// 覆盖 9 个主页面：真实渲染 + 关键元素存在 + 无 JS 报错 +
// 交互（点击/弹窗/菜单）+ 窄屏不溢出。
//
// 为什么必须真跑：静态检查看不出「DOM 用了不存在的 CSS 类」
// （本轮之前踩过：.drawer 类名没有对应样式，详情渲染到页面下方）。
const { spawn } = require('child_process');
const fs = require('fs');
const http = require('http');
const path = require('path');
const os = require('os');

const CHROME = process.env.CHROME_BIN || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const DIST = path.join(__dirname, '..', 'dist');
const PORT = 19351, CDP_PORT = 19352;
const API_TARGET = process.env.API_TARGET || 'http://127.0.0.1:8081';
const TOKEN = process.env.NODEOPS_TOKEN || '';
const SHOT_DIR = process.env.SHOT_DIR || path.join(__dirname, 'shots');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const MIME = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript', '.css': 'text/css', '.png': 'image/png' };
if (!TOKEN) { console.error('缺 NODEOPS_TOKEN'); process.exit(2); }
fs.mkdirSync(SHOT_DIR, { recursive: true });

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

const failures = [];
const passes = [];

function ok(m) { passes.push(m); }
function bad(m) { failures.push(m); }

async function main() {
  await new Promise((r) => server.listen(PORT, '127.0.0.1', r));
  const chrome = spawn(CHROME, [`--remote-debugging-port=${CDP_PORT}`, '--headless=new', '--no-sandbox',
    '--disable-gpu', '--disable-dev-shm-usage', '--window-size=1600,900',
    '--user-data-dir=' + fs.mkdtempSync(path.join(os.tmpdir(), 'cdp-full-')), 'about:blank'], { stdio: 'ignore' });
  try {
    await sleep(3200);
    const targets = await getJSON(`http://127.0.0.1:${CDP_PORT}/json`);
    const page = targets.find((t) => t.type === 'page');
    const WebSocket = require('ws');
    const ws = new WebSocket(page.webSocketDebuggerUrl);
    await new Promise((r) => ws.on('open', r));
    let id = 0; const pending = new Map(); const consoleErrors = [];
    ws.on('message', (raw) => {
      const m = JSON.parse(raw);
      if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
      if (m.method === 'Runtime.consoleAPICalled' && m.params.type === 'error') {
        consoleErrors.push((m.params.args || []).map((a) => a.value || a.description || '').join(' ').slice(0, 200));
      }
      if (m.method === 'Runtime.exceptionThrown') {
        consoleErrors.push('EXCEPTION: ' + (m.params.exceptionDetails?.text || '').slice(0, 200));
      }
    });
    const send = (m, p = {}) => new Promise((res) => { const mid = ++id; pending.set(mid, res); ws.send(JSON.stringify({ id: mid, method: m, params: p })); });
    const ev = async (e) => (await send('Runtime.evaluate', { expression: e, returnByValue: true })).result?.result?.value;

    await send('Runtime.enable'); await send('Page.enable');
    const origin = `http://127.0.0.1:${PORT}`;

    // 登录态
    await send('Page.navigate', { url: origin + '/' }); await sleep(1200);
    await ev(`localStorage.setItem('nodeops.session', JSON.stringify({token:'${TOKEN}',email:'o@t.com',role:'admin'}))`);
    await send('Page.navigate', { url: origin + '/' }); await sleep(2000);

    // 页面清单（导航项文字 -> 期望出现的特征）
    const pages = [
      { nav: '节点监控', must: ['节点', 'ms'], name: '节点监控' },
      { nav: 'VPS 主机', must: ['主机'], name: 'VPS 主机' },
      { nav: 'HY2 节点', must: ['节点'], name: 'HY2 节点' },
      { nav: '3X-UI 面板', must: ['面板'], name: '3X-UI 面板' },
      { nav: '订阅中心', must: ['订阅'], name: '订阅中心' },
      { nav: '运维任务', must: ['任务'], name: '运维任务' },
      { nav: '告警中心', must: ['告警'], name: '告警中心' },
      { nav: '系统设置', must: ['设置'], name: '系统设置' },
    ];

    console.log('='.repeat(78));
    console.log('前端全页面测试');
    console.log('='.repeat(78));

    for (const pg of pages) {
      console.log(`\n--- ${pg.name} ---`);
      const before = consoleErrors.length;

      const clicked = await ev(`(() => {
        const b = [...document.querySelectorAll('.nav-item')].find(x => x.textContent.trim() === ${JSON.stringify(pg.nav)});
        if (b) { b.click(); return true } return false
      })()`);
      if (!clicked) { bad(`${pg.name}: 找不到导航项「${pg.nav}」`); continue; }
      await sleep(2600);

      // 基本信息
      const info = JSON.parse(await ev(`(() => {
        const main = document.querySelector('.content-wrap') || document.body;
        const h1 = main.querySelector('h1');
        const text = (main.innerText || '');
        return JSON.stringify({
          h1: h1 ? h1.textContent.trim() : '',
          textLen: text.length,
          // 空状态或错误提示
          hasEmpty: /暂无|没有|空|未配置|尚未/.test(text),
          hasError: /加载失败|请求失败|出错了|Error/.test(text),
          // 有没有卡片/表格这类实体内容
          cards: main.querySelectorAll('.metric-card, .node-card, .panel, table').length,
          scrollW: document.documentElement.scrollWidth,
          clientW: document.documentElement.clientWidth,
        });
      })()`) || '{}');

      console.log(`  h1="${info.h1}"  文本${info.textLen}字  实体${info.cards}个`);
      if (!info.h1) bad(`${pg.name}: 没有 h1 标题`);
      else ok(`${pg.name}: 标题「${info.h1}」`);
      if (info.textLen < 50) bad(`${pg.name}: 内容过少（${info.textLen} 字）—— 可能白屏`);
      else ok(`${pg.name}: 有内容（${info.textLen} 字）`);
      if (info.hasError) bad(`${pg.name}: 页面出现错误提示`);
      if (info.scrollW > info.clientW + 2) {
        bad(`${pg.name}: 横向溢出（scrollW=${info.scrollW} > clientW=${info.clientW}）`);
      } else ok(`${pg.name}: 无横向溢出`);

      // 关键文字
      for (const kw of pg.must) {
        const has = await ev(`(document.querySelector('.content-wrap')||document.body).innerText.includes(${JSON.stringify(kw)})`);
        if (!has) bad(`${pg.name}: 缺少关键词「${kw}」`);
      }

      // 控制台报错
      const newErrs = consoleErrors.slice(before);
      if (newErrs.length) {
        bad(`${pg.name}: 控制台报错 ${newErrs.length} 条 —— ${newErrs[0].slice(0, 100)}`);
      } else ok(`${pg.name}: 无控制台报错`);

      // 截图
      const shot = await send('Page.captureScreenshot', { format: 'png' });
      if (shot.result?.data) {
        // ⚠️ 不能用 `name.replace(/[^\w]/g,'_')` 当文件名 ——
        // 中文会被全部替换成下划线，8 个页面的名字变成一样、互相覆盖，
        // 最后只剩 4 个文件（我因此误以为"有页面没截到"）。
        // 改用**序号 + 去掉路径非法字符**。
        const safe = pg.name.replace(/[\\/:*?"<>|]/g, '_');
        fs.writeFileSync(path.join(SHOT_DIR, `${String(pages.indexOf(pg) + 1).padStart(2, '0')}-${safe}.png`),
          Buffer.from(shot.result.data, 'base64'));
      }
    }

    // ---- 交互测试：弹窗行为 ----
    console.log('\n--- 交互：订阅中心编辑弹窗 ---');
    await ev(`(() => { const b=[...document.querySelectorAll('.nav-item')].find(x=>x.textContent.trim()==='订阅中心'); if(b) b.click() })()`);
    await sleep(2600);
    const opened = await ev(`(() => {
      const b = [...document.querySelectorAll('.sub-table tbody tr .row-actions button')]
        .find(x => (x.getAttribute('title') || '').includes('编辑'));
      if (b) { b.click(); return true } return false
    })()`);
    await sleep(900);
    if (!opened) bad('订阅中心: 点不开编辑弹窗');
    else {
      const modal = JSON.parse(await ev(`(() => {
        const m = document.querySelector('.modal-card');
        if (!m) return JSON.stringify({open:false});
        const r = m.getBoundingClientRect();
        return JSON.stringify({
          open: true,
          inViewport: r.top >= -1 && r.bottom <= window.innerHeight + 1,
          scrollable: m.scrollHeight > m.clientHeight,
          hasDate: !!m.querySelector('input[type="date"]'),
          hasDevice: !!m.querySelector('input'),
        });
      })()`) || '{}');
      if (!modal.open) bad('订阅中心: 编辑弹窗没出现');
      else {
        ok('订阅中心: 编辑弹窗可打开');
        if (!modal.inViewport) bad('订阅中心: 弹窗超出视口');
        else ok('订阅中心: 弹窗完整在视口内');
        if (!modal.scrollable) bad('订阅中心: 弹窗内容不可滚动（可能被截断）');
        else ok('订阅中心: 弹窗内容可滚动');
        if (!modal.hasDate) bad('订阅中心: 缺少日期选择器');
        else ok('订阅中心: 日期选择器存在');
      }
      // Esc 关闭
      await send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
      await send('Input.dispatchKeyEvent', { type: 'keyUp', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
      await sleep(600);
      const closed = await ev(`!document.querySelector('.modal-card')`);
      if (!closed) bad('订阅中心: Esc 关不掉弹窗');
      else ok('订阅中心: Esc 可关闭弹窗');
    }

    // ---- 交互测试：更多菜单不被裁切 ----
    console.log('\n--- 交互：更多菜单 ---');
    // ⚠️ 选择器必须限定在 .row-menu 里。
    // 表里所有图标按钮都带 .row-more（复制/启停/编辑/更多），
    // 只写 .sub-table .row-more 会选到**第一个**（复制链接），
    // 点它当然弹不出菜单 —— 是断言写错，不是功能坏（铁律⑨）。
    const moreOpen = await ev(`(() => {
      const b = document.querySelector('.sub-table .row-menu .row-more');
      if (b) { b.click(); return true } return false
    })()`);
    await sleep(800);
    if (!moreOpen) bad('订阅中心: 点不开更多菜单');
    else {
      const menu = JSON.parse(await ev(`(() => {
        const p = document.querySelector('.row-menu-pop');
        if (!p) return JSON.stringify({open:false});
        const r = p.getBoundingClientRect();
        const cs = getComputedStyle(p);
        return JSON.stringify({ open:true, position:cs.position,
          inViewport: r.left>=-1 && r.top>=-1 && r.right<=window.innerWidth+1 && r.bottom<=window.innerHeight+1,
          items: p.querySelectorAll('button').length, rect:{t:Math.round(r.top),l:Math.round(r.left),b:Math.round(r.bottom),r:Math.round(r.right)} });
      })()`) || '{}');
      if (!menu.open) bad('订阅中心: 更多菜单没出现');
      else {
        ok(`订阅中心: 更多菜单可打开（${menu.items} 项）`);
        if (menu.position !== 'fixed') bad(`订阅中心: 菜单 position 应为 fixed，实际 ${menu.position}`);
        if (!menu.inViewport) bad(`订阅中心: 菜单超出视口 ${JSON.stringify(menu.rect)}`);
        else ok('订阅中心: 菜单完整在视口内');
        if (menu.items < 4) bad(`订阅中心: 菜单项应 4 个，实际 ${menu.items}`);
      }
      await ev(`document.body.click()`); await sleep(300);
    }

    // ---- 窄屏测试：只看横向溢出，不重复跑全部断言 ----
    console.log('\n--- 窄屏适配（1200 / 1000） ---');
    for (const w of [1200, 1000]) {
      await send('Emulation.setDeviceMetricsOverride', { width: w, height: 900, deviceScaleFactor: 1, mobile: false });
      await sleep(700);
      const geo = JSON.parse(await ev(`(() => {
        const wrap = document.querySelector('.server-table-wrap') || document.querySelector('.content-wrap');
        const cw = wrap ? getComputedStyle(wrap) : null;
        return JSON.stringify({
          scrollW: document.documentElement.scrollWidth,
          clientW: document.documentElement.clientWidth,
          overflowY: cw ? cw.overflowY : '',
        });
      })()`) || '{}');
      const overflow = geo.scrollW > geo.clientW + 2;
      if (overflow) bad(`窄屏 ${w}px: 横向溢出（${geo.scrollW} > ${geo.clientW}）`);
      else ok(`窄屏 ${w}px: 无横向溢出`);
      if (geo.overflowY === 'visible' || geo.overflowY === '') {
        bad(`窄屏 ${w}px: 表格容器 overflow-y=${geo.overflowY}（应为 hidden，否则会冒出竖条）`);
      } else {
        ok(`窄屏 ${w}px: 纵向 overflow=${geo.overflowY}`);
      }
    }
    await send('Emulation.clearDeviceMetricsOverride');

    console.log('\n' + '='.repeat(78));
    console.log(`通过 ${passes.length} 项，失败 ${failures.length} 项`);
    if (failures.length) {
      console.log('FAIL');
      failures.forEach((f) => console.log('  ✗ ' + f));
    } else {
      console.log('PASS —— 前端全部页面渲染与交互正常');
    }
    console.log('截图目录:', SHOT_DIR);
  } finally {
    try { chrome.kill(); } catch (e) {}
    server.close();
  }
  process.exit(failures.length ? 1 : 0);
}
main().catch((e) => { console.error(e); process.exit(1); });
