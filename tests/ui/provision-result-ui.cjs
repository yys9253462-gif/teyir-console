// 截「手工开订阅 → 交付弹窗」的三种开户状态。
//
// 通过真实走 UI 流程触发：点「手工开订阅」→ 填表 → 提交 → 截交付弹窗。
const { spawn } = require('child_process');
const fs = require('fs');
const http = require('http');
const path = require('path');
const os = require('os');

const CHROME = 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const DIST = path.join(__dirname, '..', '..', 'dist');
const PORT = 19387, CDP = 19388;
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
    '--user-data-dir=' + fs.mkdtempSync(path.join(os.tmpdir(), 'cdp-partial-')), 'about:blank'], { stdio: 'ignore' });
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
    const ev = async (e) => (await send('Runtime.evaluate', { expression: e, returnByValue: true, awaitPromise: true })).result?.result?.value;

    await send('Runtime.enable'); await send('Page.enable');
    const origin = `http://127.0.0.1:${PORT}`;
    await send('Page.navigate', { url: origin + '/' }); await sleep(1200);
    await ev(`localStorage.setItem('nodeops.session', JSON.stringify({token:'${TOKEN}',email:'o@t.com',role:'admin'}))`);
    await send('Page.navigate', { url: origin + '/' }); await sleep(2400);
    await ev(`(()=>{const b=[...document.querySelectorAll('.nav-item')].find(x=>x.textContent.trim()==='订阅中心');if(b)b.click()})()`);
    await sleep(3400);

    // 点「手工开订阅」
    const opened = await ev(`(()=>{const b=[...document.querySelectorAll('button')].find(x=>x.textContent.includes('手工开订阅'));if(!b)return false;b.click();return true})()`);
    console.log('打开手工开订阅弹窗:', opened);
    await sleep(1200);

    // 填名称后提交（其它保持默认）
    await ev(`(()=>{
      const inp=[...document.querySelectorAll('.modal-card input')].find(i=>!i.disabled&&i.type!=='checkbox');
      if(inp){const setter=Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype,'value').set;
        setter.call(inp,'zzz-ui-partial-demo');
        inp.dispatchEvent(new Event('input',{bubbles:true}));}
    })()`);
    await sleep(500);
    // 先把弹窗里所有按钮的文案打出来 —— 别再猜了。
    const btnTexts = await ev(`JSON.stringify([...document.querySelectorAll('.modal-card button')].map(b=>({t:b.textContent,trim:b.textContent.trim(),disabled:b.disabled})))`);
    console.log('弹窗内按钮:', btnTexts);

    const submitted = await ev(`(()=>{
      const btns=[...document.querySelectorAll('.modal-card button')];
      // 用 includes 而不是全等 —— 文案可能带不可见字符或图标文本
      const b=btns.find(x=>x.textContent.includes('开通')&&!x.disabled)
           || btns.find(x=>/开通/.test(x.textContent));
      if(!b)return null;
      b.click();return b.textContent.trim();
    })()`);
    console.log('提交按钮:', submitted);

    // 等交付弹窗出现。
    //
    // ⚠️ 判断条件用 modal-card 里的 h2，不用 body.innerText ——
    // innerText 受 CSS 可见性影响，且整页文字里出现「订阅已开通」
    // 未必就是这个弹窗。
    for (let i = 0; i < 40; i++) {
      await sleep(1000);
      const t = await ev(`(()=>{const m=document.querySelector('.modal-card');return m?((m.querySelector('h2')||{}).textContent||''):''})()`);
      if (i % 6 === 0) console.log(`  ${i}s 弹窗标题: ${t || '(无)'}`);
      if ((t || '').includes('订阅已开通')) break;
    }
    await sleep(800);

    const info = await ev(`JSON.stringify((()=>{
      const m=document.querySelector('.modal-card');
      if(!m) return {found:false};
      const txt=m.innerText;
      // 找出**把容器撑宽**的那个元素：scrollWidth 超过 clientWidth 的直接子级。
      const cw=m.clientWidth;
      const wide=[...m.querySelectorAll('*')].filter(el=>el.scrollWidth>cw+2).slice(0,6)
        .map(el=>({tag:el.tagName,cls:el.className&&el.className.toString?el.className.toString().slice(0,50):'',
                   sw:el.scrollWidth,text:(el.textContent||'').trim().slice(0,60)}));
      return {
        found:true,
        hasOk: txt.includes('开户完成'),
        hasPartial: txt.includes('未成功'),
        hasNone: txt.includes('没有在任何节点上开成账号'),
        hasRetryHint: txt.includes('账号对账'),
        cardClientWidth: cw,
        cardScrollWidth: m.scrollWidth,
        overflowing: wide,
        text: txt.slice(0,600),
      };
    })())`);
    const d = JSON.parse(info || '{}');
    console.log();
    console.log('交付弹窗:', d.found ? '已出现' : '★ 没出现');
    if (d.found) {
      console.log('  含"全部成功"提示:', d.hasOk ? 'OK' : '-');
      console.log('  含"部分成功"提示:', d.hasPartial ? 'OK' : '-');
      console.log('  含"一个都没成"提示:', d.hasNone ? 'OK' : '-');
      console.log('  含"账号对账"补救指引:', d.hasRetryHint ? 'OK' : '-');
      console.log('  容器 clientWidth=%s scrollWidth=%s  %s' % (
        d.cardClientWidth, d.cardScrollWidth,
        d.cardScrollWidth > d.cardClientWidth + 2 ? '★ 有横向溢出' : '无横向溢出'));
      if (d.overflowing && d.overflowing.length) {
        console.log('  撑宽容器的元素:');
        for (const el of d.overflowing) {
          console.log(`     <${el.tag} class="${el.cls}"> sw=${el.sw}  ${el.text}`);
        }
      }
    }

    const shot = await send('Page.captureScreenshot', { format: 'png' });
    if (shot.result?.data) {
      const f = path.join(OUT, 'provision-result.png');
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
