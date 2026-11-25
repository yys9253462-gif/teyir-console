/**
 * 第 5 轮：交付页三种状态的真实渲染验收。
 *
 * 前面几轮都在接口层。这一轮只证明一件事：**用户在浏览器里到底看得见什么**。
 *
 * 三种状态各造一条订阅，然后用 CDP 打开 /user/<token> 把 DOM 抓出来：
 *   1. 正常（enabled=true, expired=false）
 *        必须给出可复制的订阅链接、非空二维码、三个格式的下载链接。
 *   2. 已暂停（enabled=false, expired=false）
 *        必须给「订阅已被暂停」提示，且**一行可用链接都不能有** ——
 *        服务端此时不下发任何 URL，页面若照常渲染就会得到一堆空输入框
 *        和 href=undefined 的按钮，这是本轮要盯的缺陷。
 *   3. 已到期（expired=true）
 *        必须给到期横幅 + 锁定文案 + 续费入口，且同样没有任何链接。
 *
 * 断言尽量落在「渲染出来的文本 / DOM 属性」上：
 * 截图只作为人工复核的旁证，不作为判定依据。
 *
 * 用法：
 *   node tests/delivery-ui.cjs \
 *     --base https://panel.example.com \
 *     --store-key shop_xxx \
 *     --json-out .workbuddy/scratch/shots/report-round5-ui.json
 */
const { spawn } = require('node:child_process')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')

// 优先读取 CHROME_PATH 环境变量，再按当前平台动态探测 Playwright 或系统浏览器路径（严禁硬编码个人用户名）。
function collectChromeCandidates() {
  const list = []
  if (process.env.CHROME_PATH) list.push(process.env.CHROME_PATH)
  const localAppData = process.env.LOCALAPPDATA || path.join(os.homedir(), 'AppData', 'Local')
  const pwRoot = path.join(localAppData, 'ms-playwright')
  try {
    if (fs.existsSync(pwRoot)) {
      const dirs = fs.readdirSync(pwRoot)
        .filter((d) => d.startsWith('chromium_headless_shell-') || d.startsWith('chromium-'))
        .sort()
        .reverse()
      for (const dir of dirs) {
        list.push(path.join(pwRoot, dir, 'chrome-headless-shell-win64', 'chrome-headless-shell.exe'))
        list.push(path.join(pwRoot, dir, 'chrome-win', 'chrome.exe'))
      }
    }
  } catch { /* 目录不可读时忽略，继续探测系统路径 */ }
  list.push(
    'C:/Program Files/Google/Chrome/Application/chrome.exe',
    'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
    '/usr/bin/chromium',
    '/usr/bin/chromium-browser',
    '/usr/bin/google-chrome',
  )
  return list
}

const CHROME_CANDIDATES = collectChromeCandidates()

function arg(name, fallback = null) {
  const i = process.argv.indexOf('--' + name)
  if (i >= 0 && i + 1 < process.argv.length) return process.argv[i + 1]
  return fallback
}

const BASE = (arg('base', 'https://panel.example.com') || '').replace(/\/$/, '')
const STORE_KEY = arg('store-key', '')
const JSON_OUT = arg('json-out', null)
const OUT = path.resolve(__dirname, '..', '.workbuddy', 'scratch', 'shots')
const DAY = 86400

fs.mkdirSync(OUT, { recursive: true })
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const cases = []
function check(label, ok, detail = '') {
  cases.push({ label, pass: !!ok, detail: String(detail) })
}

// ------------------------------------------------------------------ HTTP
async function api(method, url, { headers = {}, body = null } = {}) {
  const h = { 'Content-Type': 'application/json', ...headers }
  const res = await fetch(url, {
    method, headers: h,
    body: body ? JSON.stringify(body) : undefined,
  })
  const text = await res.text()
  let json = null
  try { json = JSON.parse(text) } catch { /* 非 JSON */ }
  return { status: res.status, text, json }
}

async function makeOrder(tag, extra) {
  const order_id = `UIR5-${Date.now()}-${tag}`
  const { status, json } = await api('POST', BASE + '/api/store/orders', {
    headers: { 'X-Store-Key': STORE_KEY },
    body: { order_id, label: `UI验收-${tag}`, ip_limit: 3, ...extra },
  })
  return { status, token: json && json.token, order_id, json }
}

// ------------------------------------------------------------- CDP 驱动
class Page {
  constructor(ws) {
    this.ws = ws
    this.id = 0
    this.pending = new Map()
    this.errors = []
    ws.onmessage = (ev) => {
      const msg = JSON.parse(ev.data)
      if (msg.id && this.pending.has(msg.id)) {
        const { resolve, reject } = this.pending.get(msg.id)
        this.pending.delete(msg.id)
        msg.error ? reject(new Error(JSON.stringify(msg.error))) : resolve(msg.result)
        return
      }
      if (msg.method === 'Runtime.exceptionThrown') {
        const d = msg.params.exceptionDetails
        this.errors.push(d.exception?.description || d.text || 'unknown')
      }
      if (msg.method === 'Runtime.consoleAPICalled' && msg.params.type === 'error') {
        this.errors.push((msg.params.args || [])
          .map(a => a.value ?? a.description ?? '').join(' '))
      }
    }
  }
  send(method, params = {}) {
    const id = ++this.id
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject })
      this.ws.send(JSON.stringify({ id, method, params }))
    })
  }
  async goto(url, settleMs = 2200) {
    await this.send('Page.navigate', { url })
    await sleep(settleMs)
  }

  /**
   * 等到交付页脱离加载态再抓 DOM。
   *
   * 固定 sleep 不够稳：容器刚重建时首个请求要等编译/预热，页面会停在
   * 「正在读取订阅…」，这时抓出来整页皆空，看起来像功能坏了，其实只是
   * 快照拍早了。这里改成轮询「.delivery-card 或错误卡片出现、且不再显示
   * 加载文案」，超时才放弃。
   */
  async waitForSettled(timeoutMs = 15000) {
    const deadline = Date.now() + timeoutMs
    while (Date.now() < deadline) {
      const state = await this.evaluate(`(() => {
        const body = document.body ? document.body.innerText : '';
        return {
          loading: body.includes('正在读取订阅'),
          ready: !!document.querySelector('.delivery-card'),
          text: body.slice(0, 40),
        };
      })()`)
      if (state && state.ready && !state.loading) return true
      await sleep(300)
    }
    return false
  }

  async evaluate(expr) {
    const r = await this.send('Runtime.evaluate', {
      expression: expr, returnByValue: true, awaitPromise: true,
    })
    if (r.exceptionDetails) {
      throw new Error(r.exceptionDetails.exception?.description || 'evaluate failed')
    }
    return r.result.value
  }
  async screenshot(file) {
    const r = await this.send('Page.captureScreenshot', { format: 'png' })
    fs.writeFileSync(path.join(OUT, file), Buffer.from(r.data, 'base64'))
  }
}

async function launch() {
  const chrome = CHROME_CANDIDATES.find(p => fs.existsSync(p))
  if (!chrome) throw new Error('找不到 headless chrome')
  const port = 9351
  const profileDir = path.join(OUT, 'cdp-profile-r5-' + Date.now())
  const proc = spawn(chrome, [
    `--remote-debugging-port=${port}`, '--headless', '--disable-gpu',
    '--no-sandbox', '--hide-scrollbars', '--window-size=1440,1400',
    `--user-data-dir=${profileDir}`, 'about:blank',
  ], { stdio: 'ignore' })

  let wsUrl = null
  for (let i = 0; i < 80; i++) {
    await sleep(250)
    try {
      const r = await fetch(`http://127.0.0.1:${port}/json/version`)
      wsUrl = (await r.json()).webSocketDebuggerUrl
      if (wsUrl) break
    } catch { /* 还没起来 */ }
  }
  if (!wsUrl) throw new Error('chrome 没起来')

  const tRes = await fetch(
    `http://127.0.0.1:${port}/json/new?${encodeURIComponent('about:blank')}`,
    { method: 'PUT' })
  const target = await tRes.json()
  const ws = new WebSocket(target.webSocketDebuggerUrl)
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej })
  const page = new Page(ws)
  await page.send('Runtime.enable')
  await page.send('Page.enable')
  return { proc, page }
}

// 把交付页的关键 DOM 抓成一个结构化对象
const PROBE = `(() => {
  const txt = (sel) => { const e = document.querySelector(sel); return e ? e.innerText.trim() : null };
  const inputs = Array.from(document.querySelectorAll('input.sub-link-input'))
      .map(i => i.value);
  const anchors = Array.from(document.querySelectorAll('a.button'))
      .map(a => ({ text: a.innerText.trim(), href: a.href || null, raw: a.getAttribute('href') }));
  const qr = document.querySelector('.delivery-qr svg, .delivery-qr canvas, .delivery-qr img');
  const body = document.body.innerText;
  // 「复制」「下载」只该出现在交付区（.delivery-section）里。
  // 不能拿全页 innerText 去 includes —— 页面底部的「使用提示」里也有
  // 「复制」二字，那样断言永远为真，等于没测。
  const deliverySections = Array.from(document.querySelectorAll('.delivery-section'));
  const sectionText = deliverySections.map(s => s.innerText).join('\\n');
  return {
    hasLocked: body.includes('订阅链接已锁定'),
    hasPausedBanner: body.includes('订阅已被暂停'),
    hasExpiredBanner: body.includes('订阅已到期'),
    hasRenew: body.includes('前往续费'),
    statusPill: txt('.delivery-fact .status-pill'),
    linkInputs: inputs,
    nonEmptyLinkInputs: inputs.filter(v => v && v.length > 0).length,
    anchors,
    anchorsWithEmptyHref: anchors.filter(a => !a.raw || a.raw === 'undefined' || a.raw === '').length,
    anchorsLiteralUndefined: anchors.filter(a => (a.raw || '').includes('undefined')).length,
    hasQr: !!qr,
    qrEmpty: qr ? (qr.tagName.toLowerCase() === 'img'
        ? !qr.getAttribute('src') : qr.innerHTML.trim().length === 0) : true,
    sectionCount: deliverySections.length,
    hasCopyButton: sectionText.includes('复制'),
    hasDownloadButton: sectionText.includes('下载'),
    nodeChipCount: document.querySelectorAll('.sub-node-chip').length,
  };
})()`

async function main() {
  const result = { round: 'delivery-ui', total: 0, passed: 0, cases: [] }
  if (!STORE_KEY) {
    console.error('需要 --store-key')
    process.exit(2)
  }

  // ---- 造三条不同状态的订阅 ----
  const now = Math.floor(Date.now() / 1000)

  const live = await makeOrder('live', { duration_days: 30 })
  check('造「正常」订阅成功', live.status === 200 && !!live.token,
        `status=${live.status} token=${String(live.token).slice(0, 10)}`)

  const paused = await makeOrder('paused', { duration_days: 30 })
  check('造「暂停」订阅成功', paused.status === 200 && !!paused.token,
        `status=${paused.status}`)

  const expired = await makeOrder('expired',
        { expires_at: now - 2 * DAY, ip_limit: 3, auto_delete: false })
  check('造「已到期」订阅成功', expired.status === 200 && !!expired.token,
        `status=${expired.status}`)

  // 把 paused 那条真的撤销掉（走商城撤销接口，走的是真实产品路径）
  let revoked = false
  if (paused.token) {
    const r = await api('POST', BASE + '/api/store/revoke', {
      headers: { 'X-Store-Key': STORE_KEY },
      body: { order_id: paused.order_id || paused.json?.order_id },
    })
    revoked = r.status === 200
    // 兜底：若 order_id 未回显，用 token 撤
    if (!revoked && paused.token) {
      const r2 = await api('POST', BASE + '/api/store/revoke', {
        headers: { 'X-Store-Key': STORE_KEY },
        body: { token: paused.token },
      })
      revoked = r2.status === 200
    }
  }
  check('「暂停」订阅已被成功撤销（走真实撤销接口）', revoked, `revoked=${revoked}`)

  // 确认服务端确实不吐链接（这是本轮 UI 缺陷的前提）
  if (paused.token) {
    const { json } = await api('GET', `${BASE}/api/sub/view/${paused.token}`)
    check('前置条件：暂停态服务端不下发 subscription_url',
          json && !json.subscription_url, `got=${json && json.subscription_url}`)
    check('前置条件：暂停态 enabled=false 且 expired=false',
          json && json.enabled === false && json.expired === false,
          `enabled=${json && json.enabled} expired=${json && json.expired}`)
  }

  // ---- 浏览器 ----
  const { proc, page } = await launch()
  try {
    // ============ 1. 正常态 ============
    await page.goto(`${BASE}/user/${live.token}`)
    await page.waitForSettled()
    const vLive = await page.evaluate(PROBE)
    await page.screenshot('r5-live.png')

    check('正常态：给出订阅链接且非空',
          vLive.nonEmptyLinkInputs >= 1, `nonEmpty=${vLive.nonEmptyLinkInputs}`)
    check('正常态：二维码已渲染且非空',
          vLive.hasQr && !vLive.qrEmpty, `hasQr=${vLive.hasQr} empty=${vLive.qrEmpty}`)
    check('正常态：没有空 href 的按钮',
          vLive.anchorsWithEmptyHref === 0, `emptyHref=${vLive.anchorsWithEmptyHref}`)
    check('正常态：状态标签为「正常」', vLive.statusPill === '正常',
          `pill=${vLive.statusPill}`)
    check('正常态：不显示锁定/暂停/到期横幅',
          !vLive.hasLocked && !vLive.hasPausedBanner && !vLive.hasExpiredBanner,
          `locked=${vLive.hasLocked} paused=${vLive.hasPausedBanner} expired=${vLive.hasExpiredBanner}`)
    check('正常态：有续费入口', vLive.hasRenew, `hasRenew=${vLive.hasRenew}`)
    check('正常态：无 JS 异常', page.errors.length === 0,
          page.errors.slice(0, 2).join(' | '))

    // ============ 2. 暂停态 ============
    page.errors.length = 0
    await page.goto(`${BASE}/user/${paused.token}`)
    await page.waitForSettled()
    const vPaused = await page.evaluate(PROBE)
    await page.screenshot('r5-paused.png')

    check('暂停态：显示「订阅已被暂停」横幅',
          vPaused.hasPausedBanner, `pausedBanner=${vPaused.hasPausedBanner}`)
    check('暂停态：状态标签为「已暂停」', vPaused.statusPill === '已暂停',
          `pill=${vPaused.statusPill}`)
    check('暂停态：有续费入口（客户有出路）',
          vPaused.hasRenew, `hasRenew=${vPaused.hasRenew}`)

    // —— 本轮重点：暂停态不得渲染任何可用链接 ——
    check('暂停态：不出现空的订阅链接输入框',
          vPaused.linkInputs.length === 0,
          `inputs=${JSON.stringify(vPaused.linkInputs)}`)
    check('暂停态：不渲染二维码',
          !vPaused.hasQr || vPaused.qrEmpty,
          `hasQr=${vPaused.hasQr} empty=${vPaused.qrEmpty}`)
    check('暂停态：按钮里没有指向 undefined 的 href',
          vPaused.anchorsLiteralUndefined === 0,
          `literalUndefined=${vPaused.anchorsLiteralUndefined}`)
    check('暂停态：不显示「复制」按钮（没有可复制的东西）',
          !vPaused.hasCopyButton, `hasCopy=${vPaused.hasCopyButton}`)
    check('暂停态：不显示「下载」按钮', !vPaused.hasDownloadButton,
          `hasDownload=${vPaused.hasDownloadButton}`)
    check('暂停态：不渲染节点清单（不泄露节点 host:port）',
          vPaused.nodeChipCount === 0, `chips=${vPaused.nodeChipCount}`)

    // ============ 3. 到期态 ============
    page.errors.length = 0
    await page.goto(`${BASE}/user/${expired.token}`)
    await page.waitForSettled()
    const vExp = await page.evaluate(PROBE)
    await page.screenshot('r5-expired.png')

    check('到期态：显示「订阅已到期」横幅', vExp.hasExpiredBanner,
          `expiredBanner=${vExp.hasExpiredBanner}`)
    check('到期态：显示「订阅链接已锁定」', vExp.hasLocked,
          `locked=${vExp.hasLocked}`)
    check('到期态：状态标签为「已到期」', vExp.statusPill === '已到期',
          `pill=${vExp.statusPill}`)
    check('到期态：没有任何订阅链接输入框', vExp.linkInputs.length === 0,
          `inputs=${JSON.stringify(vExp.linkInputs)}`)
    check('到期态：不渲染二维码', !vExp.hasQr || vExp.qrEmpty,
          `hasQr=${vExp.hasQr} empty=${vExp.qrEmpty}`)
    check('到期态：仍给续费入口', vExp.hasRenew, `hasRenew=${vExp.hasRenew}`)
    check('到期态：不渲染节点清单（不泄露节点 host:port）',
          vExp.nodeChipCount === 0, `chips=${vExp.nodeChipCount}`)
    check('到期态：无 JS 异常', page.errors.length === 0,
          page.errors.slice(0, 2).join(' | '))

    // ============ 4. 不存在的令牌 ============
    page.errors.length = 0
    await page.goto(`${BASE}/user/${'x'.repeat(43)}`)
    await page.waitForSettled()
    const vMissing = await page.evaluate(`document.body.innerText`)
    await page.screenshot('r5-missing.png')
    check('不存在的令牌：给出「无法打开这个订阅」而不是白屏',
          typeof vMissing === 'string' && vMissing.includes('无法打开这个订阅'),
          String(vMissing).slice(0, 60))
    check('不存在的令牌：给出重新购买入口',
          typeof vMissing === 'string' && vMissing.includes('重新购买'),
          '')
  } finally {
    try { proc.kill() } catch { /* ignore */ }
  }

  result.total = cases.length
  result.passed = cases.filter(c => c.pass).length
  console.log(`\n第 5 轮（交付页真实渲染）  ${result.passed}/${result.total} 通过`)
  for (const c of cases) {
    if (!c.pass) console.log(`    x ${c.label}  ${c.detail}`)
  }
  result.cases = cases
  if (JSON_OUT) {
    fs.mkdirSync(path.dirname(path.resolve(JSON_OUT)), { recursive: true })
    fs.writeFileSync(path.resolve(JSON_OUT),
      JSON.stringify(result, null, 2), 'utf-8')
    console.log(`报告已写入 ${JSON_OUT}`)
  }
  process.exit(result.passed === result.total ? 0 : 1)
}

main().catch((e) => { console.error('脚本异常:', e); process.exit(3) })
