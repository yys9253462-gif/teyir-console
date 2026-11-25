// 精确量交付弹窗的宽度链路，找出横向溢出的真正来源。
const { spawn } = require('child_process');
const fs = require('fs');
const http = require('http');
const path = require('path');
const os = require('os');

const CHROME = 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const DIST = path.join(__dirname, '..', '..', 'dist');
const PORT = 19389, CDP = 19390;
const API = process.env.API_TARGET || 'http://127.0.0.1:8081';
const TOKEN = process.env.NODEOPS_TOKEN || '';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const MIME = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript', '.css': 'text/css', '.png': 'image/png' };

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
  if (!f.startsWith(DIST) || !fs.existsSync(f)) { res.writeHead(404); return res.end('nf'); }
  res.writeHead(200, { 'Content-Type': MIME[path.extname(f)] || 'application/octet-stream' });
  fs.createReadStream(f).pipe(res);
});
const getJSON = (u) => new Promise((ok, no) => http.get(u, (r) => { let b = ''; r.on('data', (d) => b += d); r.on('end', () => { try { ok(JSON.parse(b)); } catch (e) { no(e); } }); }).on('error', no));

async function main() {
  await new Promise((r) => server.listen(PORT, '127.0.0.1', r));
  const chrome = spawn(CHROME, [`--remote-debugging-port=${CDP}`, '--headless=new', '--no-sandbox',
    '--disable-gpu', '--window-size=1600,1000',
    '--user-data-dir=' + fs.mkdtempSync(path.join(os.tmpdir(), 'cdp-w-')), 'about:blank'], { stdio: 'ignore' });
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
    await ev(`(()=>{const b=[...document.querySelectorAll('button')].find(x=>x.textContent.includes('手工开订阅'));if(b)b.click()})()`);
    await sleep(1200);
    await ev(`(()=>{
      const inp=[...document.querySelectorAll('.modal-card input')].find(i=>!i.disabled&&i.type!=='checkbox');
      if(inp){const s=Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype,'value').set;
        s.call(inp,'zzz-width-probe');inp.dispatchEvent(new Event('input',{bubbles:true}));}
    })()`);
    await sleep(400);
    await ev(`(()=>{const b=[...document.querySelectorAll('.modal-card button')].find(x=>x.textContent.includes('开通订阅'));if(b)b.click()})()`);
    for (let i = 0; i < 40; i++) { await sleep(1000); const t = await ev(`(()=>{const m=document.querySelector('.modal-card');return m?((m.querySelector('h2')||{}).textContent||''):''})()`); if ((t||'').includes('订阅已开通')) break; }
    await sleep(600);

    const dump = await ev(`JSON.stringify((()=>{
      const m=document.querySelector('.modal-card');
      if(!m) return {found:false};
      // 从 card 往下走，逐层报告宽度与计算样式
      const out=[];
      const walk=(el,depth)=>{
        if(depth>4) return;
        const cs=getComputedStyle(el);
        out.push({
          depth, tag:el.tagName,
          cls:(el.className||'').toString().slice(0,40),
          cw:el.clientWidth, sw:el.scrollWidth, ow:el.offsetWidth,
          width:cs.width, minWidth:cs.minWidth, display:cs.display,
          overflowX:cs.overflowX, flex:cs.flex, whiteSpace:cs.whiteSpace,
        });
        for(const c of el.children) walk(c,depth+1);
      };
      walk(m,0);
      const card=getComputedStyle(m);
      return {found:true, card:{cw:m.clientWidth,sw:m.scrollWidth,ow:m.offsetWidth,
        width:card.width,minWidth:card.minWidth,overflowX:card.overflowX},
        nodes:out.filter(n=>n.sw>n.cw+1).slice(0,15)};
    })())`);
    const d = JSON.parse(dump || '{}');
    console.log('弹窗在:', d.found);
    if (d.found) {
      console.log('card:', JSON.stringify(d.card));
      console.log();
      console.log('自身存在横向溢出的节点（sw > cw）:');
      for (const n of (d.nodes || [])) {
        console.log(`  ${'  '.repeat(n.depth)}<${n.tag} class="${n.cls}"> cw=${n.cw} sw=${n.sw} w=${n.width} minW=${n.minWidth} disp=${n.display} ws=${n.whiteSpace}`);
      }
    }
  } finally {
    try { chrome.kill(); } catch (e) { }
    server.close();
  }
  process.exit(0);
}
main().catch((e) => { console.error(e); process.exit(1); });
