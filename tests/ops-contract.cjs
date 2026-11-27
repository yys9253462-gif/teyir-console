// 隔离前端组件与接口契约测试：不发起真实网络请求、不读写宿主机持久化存储、不依赖生产配置。
const assert = require('node:assert/strict')
const fs = require('node:fs')
const vm = require('node:vm')
const path = require('node:path')
const ts = require('typescript')

const root = path.resolve(__dirname, '..')
let states = [], cursor = 0, effects = [], timers = [], calls = [], pollResults = []

function resetHooks() { states = []; cursor = 0; effects = [] }

const react = {
  memo(comp) { return comp },
  lazy(loader) { return loader },
  Suspense({ children }) { return children },
  useState(initial) {
    const bag = states
    const i = cursor++
    if (!(i in bag)) bag[i] = typeof initial === 'function' ? initial() : initial
    return [bag[i], (v) => { bag[i] = typeof v === 'function' ? v(bag[i]) : v }]
  },
  useEffect(fn) { effects.push(fn) },
  useLayoutEffect(fn) { effects.push(fn) },
  useRef(v) { return { current: v } },
  useCallback(fn) { return fn },
  useMemo(fn) { return fn() },
}

const jsx = (type, props) => ({ type, props: props || {} })

const api = {
  installJob: async () => {
    const result = pollResults.shift()
    if (result instanceof Error) throw result
    return result
  },
  updateSubscriptionRecord: async (_id, payload) => { calls.push(payload) },
}

function load(file, extra = {}, env = {}) {
  let source = fs.readFileSync(path.join(root, file), 'utf8')
  if (file.endsWith('SubscriptionCenter.tsx')) source += '\nexport { EditSubscriptionDialog };'
  if (file.endsWith('RenewDialog.tsx')) source += '\nexport { sameOriginPath, safeExternalUrl };'
  source = source.replace(/import\.meta\.env/g, '({ VITE_API_URL: "" })')
  const code = ts.transpileModule(source, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX, target: ts.ScriptTarget.ES2022 },
  }).outputText
  const exports = {}
  const sandbox = {
    exports,
    require(name) {
      if (name in extra) return extra[name]
      if (name === 'react') return react
      if (name === 'react-dom') return { createPortal: (node) => node }
      if (name === 'react/jsx-runtime') return { jsx, jsxs: jsx, Fragment: 'Fragment' }
      if (name.includes('/api')) return { api, loadSession: () => ({ role: 'admin' }), isAdminSession: () => true, ADMIN_ONLY_HINT: '仅管理员可执行此操作' }
      return new Proxy({}, { get: (_, key) => key })
    },
    window: {
      setTimeout(fn) { timers.push(fn); return timers.length },
      clearTimeout() {},
      addEventListener() {},
      removeEventListener() {},
    },
    document: {
      hidden: false,
      body: {},
      addEventListener() {},
      removeEventListener() {},
    },
    URL,
    URLSearchParams,
    AbortController,
    Date,
    console,
    setTimeout,
    clearTimeout,
    ...env,
  }
  vm.runInNewContext(code, sandbox, { filename: file })
  return exports
}

function flatten(node) {
  if (!node || typeof node !== 'object') return []
  return [node, ...[node.props?.children].flat(Infinity).flatMap(flatten)]
}

function text(node) {
  if (node == null || typeof node === 'boolean') return ''
  if (typeof node === 'string' || typeof node === 'number') return String(node)
  return [node?.props?.children].flat(Infinity).map(text).join('')
}

async function main() {
  // 1. 链接协议、外部链接与同源相对路径安全校验：拦截危险协议、双斜杠、反斜杠与控制字符绕过。
  const { safeHttpUrl, safeExternalHttpUrl, safeSameOriginPath } = load('src/lib/safeUrl.ts')
  assert.equal(safeHttpUrl('https://panel.example.com/path?a=1'), 'https://panel.example.com/path?a=1')
  assert.equal(safeHttpUrl('http://127.0.0.1:8080/renew'), 'http://127.0.0.1:8080/renew')
  assert.equal(safeHttpUrl('/sub/view/token123'), 'https://example.invalid/sub/view/token123')
  assert.equal(safeHttpUrl('//evil.example/phish'), undefined)
  assert.equal(safeHttpUrl('/\\evil.com'), undefined)
  assert.equal(safeHttpUrl('/\\evil.com/phish'), undefined)
  assert.equal(safeHttpUrl('/\t/evil.com'), undefined)
  assert.equal(safeHttpUrl('/\n/evil.com'), undefined)
  assert.equal(safeHttpUrl('/sub/view/\tabc'), undefined)
  assert.equal(safeHttpUrl('https://panel.example.com/\r\nabc'), undefined)
  assert.equal(safeHttpUrl('javascript:alert(1)'), undefined)
  assert.equal(safeHttpUrl('JAVASCRIPT:alert(1)'), undefined)
  assert.equal(safeHttpUrl('java\tscript:alert(1)'), undefined)
  assert.equal(safeHttpUrl('data:text/html,<script>alert(1)</script>'), undefined)
  assert.equal(safeHttpUrl('vbscript:msgbox(1)'), undefined)
  assert.equal(safeHttpUrl('not a url'), undefined)
  assert.equal(safeHttpUrl(''), undefined)
  assert.equal(safeHttpUrl(undefined), undefined)

  assert.equal(safeExternalHttpUrl('https://shop.example.com/pay?o=1'), 'https://shop.example.com/pay?o=1')
  assert.equal(safeExternalHttpUrl('http://127.0.0.1:9000/pay'), 'http://127.0.0.1:9000/pay')
  assert.equal(safeExternalHttpUrl('/relative/path'), undefined)
  assert.equal(safeExternalHttpUrl('//evil.example/pay'), undefined)
  assert.equal(safeExternalHttpUrl('https://evil.example/a\\b'), undefined)
  assert.equal(safeExternalHttpUrl('javascript:alert(1)'), undefined)

  assert.equal(safeSameOriginPath('/store-api/qr.png?id=1'), '/store-api/qr.png?id=1')
  assert.equal(safeSameOriginPath('/\\evil.com/px.png'), undefined)
  assert.equal(safeSameOriginPath('//evil.com/px.png'), undefined)
  assert.equal(safeSameOriginPath('/\t/evil.com/px.png'), undefined)
  assert.equal(safeSameOriginPath('https://evil.com/px.png'), undefined)

  const { sameOriginPath, safeExternalUrl } = load('src/components/RenewDialog.tsx', {
    '../lib/safeUrl': { safeHttpUrl, safeExternalHttpUrl, safeSameOriginPath },
  })
  assert.equal(sameOriginPath('/store-api/qr.png?id=1'), '/store-api/qr.png?id=1')
  assert.equal(sameOriginPath('/\\evil.com/px.png'), undefined)
  assert.equal(sameOriginPath('/\t/evil.com/px.png'), undefined)
  assert.equal(safeExternalUrl('https://pay.example.com/ok'), 'https://pay.example.com/ok')
  assert.equal(safeExternalUrl('/not-external'), undefined)
  console.log('通过：链接协议、外链与同源相对路径安全校验')

  // 2. 安装/迁移任务轮询契约：临时故障自动重试、卸载取消、401/403 立即停止轮询。
  const { useInstallJobPolling, pendingInstallJob } = load('src/lib/useInstallJobPolling.ts')
  let latest = pendingInstallJob('fixture', 'server'), error = '', completed = 0
  useInstallJobPolling(latest, (v) => { latest = v }, (v) => { error = v }, () => { completed++ })
  const cleanup = effects.pop()()
  pollResults = [new Error('temporary failure'), { ...latest, status: 'running' }, { ...latest, status: 'success' }]
  await timers.shift()(); assert.match(error, /自动重试/); assert.equal(timers.length, 1)
  await timers.shift()(); assert.equal(error, ''); assert.equal(timers.length, 1)
  await timers.shift()(); assert.equal(completed, 1); assert.equal(timers.length, 0); cleanup()
  effects = []; timers = []; pollResults = [{ ...latest, status: 'running' }]
  useInstallJobPolling(pendingInstallJob('fixture2', 'server'), () => { throw Error('updated after close') }, () => {}, () => {})
  const cancel = effects.pop()(); cancel(); await timers.shift()(); assert.equal(timers.length, 0)
  effects = []; timers = []; error = ''
  const authErr = Object.assign(new Error('Unauthorized'), { status: 401 })
  pollResults = [authErr]
  useInstallJobPolling(pendingInstallJob('fixture3', 'server'), () => {}, (v) => { error = v }, () => {})
  const cleanupAuth = effects.pop()(); await timers.shift()()
  assert.match(error, /无权限|已停止轮询/); assert.equal(timers.length, 0); cleanupAuth()
  console.log('通过：安装任务轮询重试、卸载取消与 401 停止契约')

  // 3. 人机验证脚本加载失败后自动清理残留标签，支持后续重试。
  const headChildren = []
  const fakeDoc = {
    querySelector(sel) { return headChildren.find((el) => el.src && sel.includes(el.src)) || null },
    createElement() {
      const listeners = { load: [], error: [] }
      const el = {
        src: '', async: false, defer: false, dataset: {},
        addEventListener(type, fn) { (listeners[type] || (listeners[type] = [])).push(fn) },
        removeEventListener(type, fn) { const a = listeners[type]; if (!a) return; const i = a.indexOf(fn); if (i >= 0) a.splice(i, 1) },
        emit(type) { for (const fn of [...(listeners[type] || [])]) fn() },
        remove() { const i = headChildren.indexOf(el); if (i >= 0) headChildren.splice(i, 1) },
      }
      return el
    },
    head: { appendChild(el) { headChildren.push(el) } },
  }
  const fakeWin = { setTimeout() { return 1 }, clearTimeout() {}, turnstile: undefined }
  const { loadTurnstile } = load('src/lib/turnstile.ts', {}, { document: fakeDoc, window: fakeWin })
  const firstLoad = loadTurnstile()
  assert.equal(headChildren.length, 1)
  headChildren[0].emit('error')
  await assert.rejects(firstLoad, /人机验证加载失败/)
  assert.equal(headChildren.length, 0, '失败的人机验证脚本标签必须移除以便后续重试')
  const secondLoad = loadTurnstile()
  assert.equal(headChildren.length, 1)
  fakeWin.turnstile = { render() { return 'w1' }, reset() {}, remove() {} }
  headChildren[0].emit('load')
  assert.equal(await secondLoad, fakeWin.turnstile)
  console.log('通过：人机验证脚本加载失败清理残留标签并可恢复重试')

  // 4. 被墙检测面板允许将已配置的国内探测点切回「不使用」并保存取消。
  resetHooks()
  let savedProbeId = null
  api.blockChecks = async () => ({ probe: { server_id: 'srv-cn', name: 'CN', host: '203.0.113.10' }, items: [] })
  api.servers = async () => [{ id: 'srv-cn', name: 'CN', host: '203.0.113.10', has_credential: true }]
  api.setCNProbe = async (id) => { savedProbeId = id }
  const { BlockCheckPanel } = load('src/components/BlockCheckPanel.tsx')
  BlockCheckPanel({ notify() {} })
  // 状态槽位：0=data, 1=servers, 2=running, 3=savingProbe, 4=choosing
  states[0] = { probe: { server_id: 'srv-cn', name: 'CN', host: '203.0.113.10' }, items: [] }
  states[1] = [{ id: 'srv-cn', name: 'CN', host: '203.0.113.10', has_credential: true }]
  cursor = 0; let bp = BlockCheckPanel({ notify() {} })
  const select = flatten(bp).find((n) => n.type === 'select')
  assert.ok(select)
  select.props.onChange({ target: { value: '' } })
  assert.equal(states[4], '')
  cursor = 0; bp = BlockCheckPanel({ notify() {} })
  const saveBtn = flatten(bp).find((n) => n.type === 'button' && text(n) === '保存')
  assert.ok(saveBtn, '切回空探测点时必须展示保存按钮')
  saveBtn.props.onClick()
  await new Promise((resolve) => setImmediate(resolve))
  assert.equal(savedProbeId, '')
  console.log('通过：被墙检测面板支持清空国内探测点配置')

  // 5. 订阅编辑弹窗：续期/改永久绝不挟带未保存字段，也不触发无关校验。
  resetHooks()
  const { EditSubscriptionDialog } = load('src/components/SubscriptionCenter.tsx', { '../lib/safeUrl': { safeHttpUrl } })
  const editProps = {
    item: { id: 'fixture', label: 'original', ip_limit: 2, user_email: 'fixture@example.invalid', note: '', auto_delete: false, expires_at: 0 },
    notify() {},
    onClose() {},
    onSaved() {},
  }
  EditSubscriptionDialog(editProps)
  // 未保存的非法在线网络数与破坏性勾选绝不能跟随快捷续期提交。
  states[0] = 'unsaved'; states[2] = 'invalid'; states[5] = true; states[6] = true
  cursor = 0; const tree = EditSubscriptionDialog(editProps)
  const buttons = flatten(tree).filter((n) => n.type === 'button')
  await buttons.find((n) => text(n) === '+30 天').props.onClick()
  assert.deepEqual(JSON.parse(JSON.stringify(calls.pop())), { duration_days: 30 })
  await buttons.find((n) => text(n) === '改为永久有效').props.onClick()
  assert.deepEqual(JSON.parse(JSON.stringify(calls.pop())), { expires_at: 0 })
  console.log('通过：订阅快捷续期/改永久隔离未保存字段与无关校验')

  // 6. 节点出口设置：保存失败保持弹窗与输入域，成功后关闭。
  resetHooks()
  let fail = true
  api.nodeOutbounds = async () => ({ mode: 'rules', outbounds: [{ name: 'proxy', type: 'direct' }], rules: [], builtin: [] })
  api.nodePortalAction = async () => { if (fail) throw Error('fixture rejected') }
  const { NodeEgress } = load('src/components/NodeEgress.tsx')
  const ep = { node: { id: 'fixture' }, notify() {} }
  NodeEgress(ep); await effects.shift()()
  await new Promise((resolve) => setImmediate(resolve))
  states[0] = await api.nodeOutbounds(); states[4] = true; states[5] = 'example.invalid'
  cursor = 0; const e = NodeEgress(ep)
  const dialog = flatten(e).find((n) => typeof n.type === 'function' && n.props.onSave)
  assert.ok(dialog)
  await dialog.props.onSave({ name: 'draft', addr: '127.0.0.1:1', password: 'synthetic' })
  assert.equal(states[4], true)
  const add = flatten(e).find((n) => n.type === 'button' && text(n).includes('添加规则'))
  await add.props.onClick(); await Promise.resolve(); assert.equal(states[5], 'example.invalid')
  fail = false; await dialog.props.onSave({ name: 'draft' }); assert.equal(states[4], false)
  console.log('通过：节点出口保存失败保留草稿、成功后关闭')

  // 7. 前端权限门禁与后端路由/动词严格对齐（portalActionAdminOnly 与 viewerMayUseRequest）。
  const storageMap = new Map()
  const localStorageMock = {
    getItem(k) { return storageMap.has(k) ? storageMap.get(k) : null },
    setItem(k, v) { storageMap.set(k, String(v)) },
    removeItem(k) { storageMap.delete(k) },
  }
  const dispatchedEvents = []
  const apiWin = {
    location: { protocol: 'https:', host: 'panel.example.com', origin: 'https://panel.example.com' },
    localStorage: localStorageMock,
    setTimeout(fn) { timers.push(fn); return timers.length },
    clearTimeout() {},
    dispatchEvent(evt) { dispatchedEvents.push(evt) },
  }
  const fetchQueue = []
  const fakeFetch = async (url) => {
    if (String(url).includes('/api/auth/terminal-cookie')) {
      return { ok: true, status: 204, headers: { get: () => '' }, text: async () => '', json: async () => ({}) }
    }
    const next = fetchQueue.shift()
    if (!next) throw new Error(`unexpected fetch: ${url}`)
    return typeof next === 'function' ? next() : next
  }
  class FakeEvent {
    constructor(type) { this.type = type }
  }
  const apiMod = load('src/lib/api.ts', {}, {
    window: apiWin,
    localStorage: localStorageMock,
    fetch: fakeFetch,
    Event: FakeEvent,
    Headers: globalThis.Headers,
  })

  // 7a. 门户动作管理员白名单与后端 portalaction.go 一致：manage_proxy 全管，manage_awg 仅 state/peer_add/peer_del 允许运维。
  assert.equal(apiMod.portalActionAdminOnly('manage_proxy'), true)
  assert.equal(apiMod.portalActionAdminOnly('manage_awg', false, { action: 'update' }), true)
  assert.equal(apiMod.portalActionAdminOnly('manage_awg', false, { action: 'set_line' }), true)
  assert.equal(apiMod.portalActionAdminOnly('manage_awg', false, { action: 'stop' }), true)
  assert.equal(apiMod.portalActionAdminOnly('manage_awg', false, { action: 'state' }), false)
  assert.equal(apiMod.portalActionAdminOnly('manage_awg', false, { action: 'peer_add' }), false)
  assert.equal(apiMod.portalActionAdminOnly('manage_awg', false, { action: 'peer_del' }), false)
  assert.equal(apiMod.portalActionAdminOnly('manage_awg', false, {}), false)

  // 7b. 只读角色请求守卫与后端 server.go 一致：拦写请求与 GET/HEAD /api/nodes/{id}/api，放行修改自己密码。
  assert.equal(apiMod.viewerMayUseRequest('GET', '/api/servers'), true)
  assert.equal(apiMod.viewerMayUseRequest('GET', '/api/nodes/n1/api'), false)
  assert.equal(apiMod.viewerMayUseRequest('HEAD', '/api/nodes/n1/api?x=1'), false)
  assert.equal(apiMod.viewerMayUseRequest('POST', '/api/servers'), false)
  assert.equal(apiMod.viewerMayUseRequest('POST', '/api/auth/change-password'), true)
  console.log('通过：前端门户动作与只读权限守卫与后端严格对齐')

  // 8. 迟到 401 竞态防护：旧令牌在途请求返回 401 时绝不误踢已重新登录的新会话。
  apiMod.saveSession({ token: 'tok-old-111', email: 'ops@example.com', role: 'admin' })
  let releaseOld401
  fetchQueue.push(() => new Promise((resolve) => {
    releaseOld401 = () => resolve({
      ok: false,
      status: 401,
      headers: { get: () => 'application/json' },
      json: async () => ({ error: '登录已过期' }),
      text: async () => JSON.stringify({ error: '登录已过期' }),
    })
  }))
  const staleReq = apiMod.api.servers()
  // 请求尚未返回前，用户已完成重新登录，本地已换成新令牌。
  apiMod.saveSession({ token: 'tok-new-222', email: 'ops@example.com', role: 'admin' })
  dispatchedEvents.length = 0
  releaseOld401()
  await assert.rejects(staleReq, /登录已过期/)
  assert.equal(apiMod.loadSession()?.token, 'tok-new-222', '旧令牌迟到 401 不得清空新会话')
  assert.equal(dispatchedEvents.length, 0, '旧令牌迟到 401 不得派发会话过期事件')

  // 当在途请求令牌与当前会话一致时，401 仍正常清空会话并派发过期事件。
  fetchQueue.push({
    ok: false,
    status: 401,
    headers: { get: () => 'application/json' },
    json: async () => ({ error: '登录已过期' }),
    text: async () => JSON.stringify({ error: '登录已过期' }),
  })
  await assert.rejects(apiMod.api.servers(), /登录已过期/)
  assert.equal(apiMod.loadSession(), null)
  assert.equal(dispatchedEvents.length, 1)
  console.log('通过：迟到 401 竞态防护不误踢新会话，当前会话 401 正常登出')

  // 9. IPPurityBadge 双 ISP 正向语义：true 为绿色正面 tone-good，false 为中性 tone-muted，不反转标红。
  resetHooks()
  const { IPPurityBadge } = load('src/components/IPPurityBadge.tsx')
  IPPurityBadge({ nodeId: 'n1', nodeName: 'Tokyo', readOnly: false, notify() {} })
  // 状态槽位：0=state, 1=loading, 2=refreshing, 3=open, 4=pos
  states[0] = {
    checked: true,
    purity: {
      ip: '198.51.100.1',
      connection_type: 'isp',
      is_dual_isp: true,
      fraud_score: 5,
      is_proxy: false,
      is_vpn: false,
      is_abuser: false,
      asn: 'AS64500',
      asn_org: 'Example ISP',
      source: 'api',
      checked_at: '2026-11-27T00:00:00Z',
    },
  }
  states[3] = true
  cursor = 0
  const badgeTrue = IPPurityBadge({ nodeId: 'n1', nodeName: 'Tokyo', readOnly: false, notify() {} })
  const dualRowTrue = flatten(badgeTrue).find((n) => n.type === 'div' && text(n).startsWith('双 ISP'))
  assert.ok(dualRowTrue, '弹层中应包含双 ISP 行')
  const dualDdTrue = flatten(dualRowTrue).find((n) => n.type === 'dd')
  assert.equal(dualDdTrue.props.className, 'tone-good', '双 ISP 为 true 必须使用正面绿色 tone-good')
  assert.equal(text(dualDdTrue), '是')

  states[0] = { checked: true, purity: { ...states[0].purity, is_dual_isp: false } }
  cursor = 0
  const badgeFalse = IPPurityBadge({ nodeId: 'n1', nodeName: 'Tokyo', readOnly: false, notify() {} })
  const dualRowFalse = flatten(badgeFalse).find((n) => n.type === 'div' && text(n).startsWith('双 ISP'))
  const dualDdFalse = flatten(dualRowFalse).find((n) => n.type === 'dd')
  assert.equal(dualDdFalse.props.className, 'tone-muted', '双 ISP 为 false 必须使用中性灰 tone-muted')
  assert.equal(text(dualDdFalse), '否')
  console.log('通过：IPPurityBadge 双 ISP 正向色彩语义正确')

  // 10. OrphanAccounts 勾选「显示已处理」且无待处理条目时，必须渲染已处理历史表格。
  resetHooks()
  const { OrphanAccounts } = load('src/components/OrphanAccounts.tsx')
  OrphanAccounts({ notify() {} })
  // 状态槽位：0=orphans, 1=showResolved, 2=loading
  states[0] = [{
    id: 'orp-1',
    node_id: 'n1',
    node_name: 'Tokyo-01',
    user_id: 'sub_alice_001',
    sub_label: '月付套餐',
    has_credential: false,
    node_deleted: true,
    created_at: '2026-11-20T00:00:00Z',
    resolved_at: '2026-11-25T10:00:00Z',
    resolved_note: '机器已下线结案',
  }]
  states[1] = true
  states[2] = false
  cursor = 0
  const orphanTree = OrphanAccounts({ notify() {} })
  const orphanAllText = text(orphanTree)
  assert.match(orphanAllText, /已处理历史\s*1/)
  assert.match(orphanAllText, /sub_alice_001/)
  assert.match(orphanAllText, /机器已下线结案/)
  console.log('通过：OrphanAccounts 勾选显示已处理后正确渲染已处理历史表')

  // 11. RenewDialog 人机验证脚本加载失败后展示恢复按钮，点击触发重新加载，避免下单永久锁死。
  resetHooks()
  let turnstileCalls = 0
  const { RenewDialog } = load('src/components/RenewDialog.tsx', {
    '../lib/safeUrl': { safeHttpUrl, safeExternalHttpUrl, safeSameOriginPath },
    '../lib/turnstile': {
      loadTurnstile: () => {
        turnstileCalls += 1
        return Promise.reject(new Error('人机验证加载失败：网络超时'))
      },
    },
    '../lib/copiedFlash': { useCopiedFlash: () => ['', () => {}] },
  })
  const renewProps = { target: { order: 'prdord_12345678', sub: 'a'.repeat(40) }, onClose() {}, onRenewed() {} }
  RenewDialog(renewProps)
  // 状态槽位（按 RenewDialog 声明顺序）：
  // 0=loading, 1=error, 2=email, 3=productID, 4=productName, 5=products, 6=denominations,
  // 7=denomID, 8=method, 9=voucher, 10=siteKey, 11=captcha, 12=captchaAttempt, 13=captchaError
  states[0] = false
  states[6] = [{ id: 'd1', label: '月付', price_cny: '20', methods: [{ id: 'ALIPAY', label: '支付宝', amount: '20', currency: 'CNY' }], voucher_available: false, voucher_amount: '0' }]
  states[7] = 'd1'
  states[8] = 'ALIPAY'
  states[10] = 'site-key-fixture'
  states[13] = ''
  effects = []
  cursor = 0
  RenewDialog(renewProps)
  // 执行人机验证加载副作用，等待 Promise 拒绝写入 captchaError。
  const turnstileEffect = effects[3]
  turnstileEffect()
  await new Promise((resolve) => setImmediate(resolve))
  assert.equal(turnstileCalls, 1)
  assert.match(states[13], /人机验证加载失败/)
  cursor = 0
  const renewFailedTree = RenewDialog(renewProps)
  const retryCaptchaBtn = flatten(renewFailedTree).find((n) => n.type === 'button' && text(n) === '重新加载人机验证')
  assert.ok(retryCaptchaBtn, '人机验证失败后必须提供「重新加载人机验证」按钮')
  retryCaptchaBtn.props.onClick()
  assert.equal(states[12], 1, '点击重新加载应递增 captchaAttempt 触发重新渲染人机验证')
  assert.equal(states[13], '')
  console.log('通过：RenewDialog 人机验证失败可点击恢复重试，无按钮死锁')

  // 12. ServersPage 运维账号（operator）改主机名称时，绝不回传管理员专属 traffic_api_url / traffic_secret_file。
  resetHooks()
  const serverUpdateCalls = []
  const operatorApi = {
    ...api,
    serverBilling: async () => [],
    updateServer: async (id, payload) => { serverUpdateCalls.push({ id, payload }) },
  }
  const { ServersPage } = load('src/components/ServersPage.tsx', {
    '../lib/api': {
      api: operatorApi,
      loadSession: () => ({ role: 'operator', email: 'op@example.com', token: 'tok-op' }),
      isAdminSession: () => false,
    },
    './HostTools': {
      BillingDialog: 'BillingDialog',
      ChangeIPDialog: 'ChangeIPDialog',
      CURRENCY_SYMBOL: { CNY: '¥', USD: '$' },
      ExpiryBadge: 'ExpiryBadge',
      daysUntil: () => null,
    },
    './HostToolbox': {
      HostPicker: 'HostPicker',
      HostToolbox: 'HostToolbox',
      TOOL_ICONS: new Proxy({}, { get: (_, k) => k }),
    },
    './InstallHy2Dialog': {
      InstallHy2Dialog: 'InstallHy2Dialog',
      installBlockedReason: () => '',
    },
  })
  const sampleServer = {
    id: 'srv-1',
    name: 'Tokyo-A',
    host: '203.0.113.20',
    ssh_port: 22,
    ssh_user: 'root',
    region: '日本',
    provider: 'AWS',
    tags: ['prod'],
    status: 'online',
    has_credential: true,
    allow_write_ops: false,
    traffic_api_url: 'http://127.0.0.1:29862',
    traffic_secret_file: '/etc/hysteria/traffic.secret',
  }
  const srvProps = { servers: [sampleServer], nodes: [], createSignal: 0, onOpen() {}, onChanged() {}, notify() {} }
  let srvTree = ServersPage(srvProps)
  const editHostBtn = flatten(srvTree).find((n) => n.type === 'button' && n.props['aria-label'] === '编辑 Tokyo-A')
  assert.ok(editHostBtn)
  editHostBtn.props.onClick({ stopPropagation() {} })
  cursor = 0
  srvTree = ServersPage(srvProps)
  const nameInput = flatten(srvTree).find((n) => n.type === 'input' && n.props.value === 'Tokyo-A')
  assert.ok(nameInput)
  nameInput.props.onChange({ target: { value: 'Tokyo-Renamed' } })
  cursor = 0
  srvTree = ServersPage(srvProps)
  const saveHostBtn = flatten(srvTree).find((n) => n.type === 'button' && text(n) === '保存修改')
  assert.ok(saveHostBtn)
  await saveHostBtn.props.onClick()
  assert.equal(serverUpdateCalls.length, 1)
  assert.equal(serverUpdateCalls[0].payload.name, 'Tokyo-Renamed')
  assert.equal('traffic_api_url' in serverUpdateCalls[0].payload, false, '运维编辑非端点字段绝不能携带 traffic_api_url')
  assert.equal('traffic_secret_file' in serverUpdateCalls[0].payload, false, '运维编辑非端点字段绝不能携带 traffic_secret_file')
  console.log('通过：ServersPage 运维账号编辑主机不提交管理员专属采集端点字段')

  // 13. SubscriptionCenter 生成一次性商城对接密钥后，即使 store.enabled 为 true 仍保持折叠面板展开，防止刷新把密钥藏起来。
  resetHooks()
  const { SubscriptionCenter } = load('src/components/SubscriptionCenter.tsx', {
    '../lib/safeUrl': { safeHttpUrl },
    '../lib/copiedFlash': { useCopiedFlash: () => ['', () => {}] },
  })
  SubscriptionCenter({ notify() {} })
  // 状态槽位：0=data, 1=note, 2=loading, 3=creating, 4=editing, 5=pendingDelete, 6=rotating, 7=rotated, 8=rotatingKey, 9=freshKey, 10=storeOpen
  states[0] = {
    subscriptions: [],
    store: {
      enabled: true,
      key_hint: 'sk_live_****9999',
      auth_header: 'X-Store-Key: <key>',
      create_endpoint: 'POST /api/store/orders',
      renew_endpoint: 'POST /api/store/orders/renew',
      revoke_endpoint: 'POST /api/store/orders/revoke',
      query_endpoint: 'GET /api/store/orders/:id',
    },
    fleet: { node_count: 1, node_in_store: 1 },
  }
  states[9] = 'STORE-KEY-FAKE-0000-TEST-ONLY'
  states[10] = false
  cursor = 0
  const subTree = SubscriptionCenter({ notify() {} })
  const detailsEl = flatten(subTree).find((n) => n.type === 'details' && n.props.className === 'sub-collapse')
  assert.ok(detailsEl, '商城对接折叠面板应存在')
  assert.equal(detailsEl.props.open, true, '存在一次性明文密钥 freshKey 时商城对接面板必须保持展开')
  assert.match(text(detailsEl), /STORE-KEY-FAKE-0000-TEST-ONLY/)
  console.log('通过：SubscriptionCenter 展示一次性对接密钥时保持折叠面板展开')
}

main().catch((error) => {
  console.error(error)
  process.exitCode = 1
})
