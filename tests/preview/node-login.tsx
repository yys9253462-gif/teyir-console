import React, { useState } from 'react'
import { createRoot } from 'react-dom/client'
import { NodeControlPanel } from '../../src/components/NodeControlPanel'
import { LoginView } from '../../src/components/LoginView'
import type { Hy2Node } from '../../src/lib/api'
import '../../src/styles.css'

// Local-only fixtures. Every API request is intercepted; no backend or credentials required.
const fixture = {
  view: 'node', caps: 'error', login: 'error', holdA: false, holdAction: false,
  requests: [] as { path: string; method: string; body?: string }[],
  pending: [] as (() => void)[], notices: [] as string[],
}
Object.assign(window, { regressionFixture: fixture })
const healthy = (payload: unknown) => ({ state: 'healthy', payload })
function status(id: string) {
  return { node_id: id, probes: {
    speed: healthy({ node_rx: 12, node_tx: 34, users: {} }),
    version: healthy({ core_current: `version-${id}` }), bbr: healthy({ current: `bbr-${id}` }),
    reality: healthy({ installed: true, active: true, config: {} }),
    vmess: healthy({ installed: true, active: true }),
    vlessenc: healthy({ installed: true, active: true, config: { domain: `${id}.example.test`, port: 443 } }),
    warp: healthy({ installed: true, connected: true, rules: [] }),
    awg: healthy({ installed: true, active: true, peer_count: 0 }), gost: healthy({ gost_installed: true }),
  } }
}
function caps(mode: string, id: string) {
  if (mode === 'missing') return null
  if (mode === 'unreachable') return { node_id: id, reachable: false, supported: false, error: 'fixture node unreachable' }
  if (mode === 'unsupported') return { node_id: id, reachable: false, supported: false }
  const enc = mode === 'absent' ? undefined : mode === 'inactive' ? { active: false } : mode === 'state-error' ? { active: true, state_error: 'fixture state probe failed' } : { active: true, client_encryption_present: mode === 'ok', ...(mode === 'ok' ? {} : { delivery_error: `fixture no client key ${id}` }) }
  return { node_id: id, reachable: true, supported: true, protocols: { vlessenc: enc } }
}
window.fetch = async (input, init) => {
  const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, location.href)
  if (url.origin !== location.origin || !url.pathname.startsWith('/api/')) throw new Error('Preview blocks non-fixture requests')
  const path = url.pathname + url.search
  fixture.requests.push({ path, method: init?.method ?? 'GET', body: typeof init?.body === 'string' ? init.body : undefined })
  const id = url.pathname.split('/')[3]
  const capMode = fixture.caps
  const loginMode = fixture.login
  if (fixture.holdA && (id === 'A' || url.pathname.endsWith('/capabilities'))) await new Promise<void>(resolve => fixture.pending.push(resolve))
  if (fixture.holdAction && url.pathname.endsWith('/portal-action')) await new Promise<void>(resolve => fixture.pending.push(resolve))
  let code = 200
  let data: unknown = {}
  if (url.pathname === '/api/login/config') {
    code = loginMode === 'error' ? 503 : 200
    data = loginMode === 'malformed' ? {} : { turnstile_site_key: loginMode === 'enabled' ? 'fixture-site-key' : '' }
  } else if (url.pathname === '/api/login') {
    code = 401; data = { error: 'fixture login submitted' }
  } else if (url.pathname.endsWith('/portal-status')) data = status(id)
  else if (url.pathname.endsWith('/capabilities')) {
    code = capMode === 'error' ? 503 : 200
    const mode = capMode === 'cache' ? (url.searchParams.get('refresh') === '1' ? 'ok' : 'blocked') : capMode
    data = { nodes: ['A', 'B'].map(id => caps(mode, id)).filter(Boolean) }
  } else if (url.pathname.endsWith('/users')) data = { users: [{ user_id: `user-${id}`, status: 'active', expires_at: 0, traffic_used_bytes: 0, traffic_limit_bytes: 0 }] }
  else if (url.pathname.endsWith('/proxy-services')) data = { services: [{ id: id, type: `proxy-${id}`, port: 1234, username: 'fixture', password: 'fixture' }], gost_active: true }
  else if (url.pathname.endsWith('/overview')) data = { probes: {}, meta: { server_name: `${id}.example.test` }, summary: { users_count: id === 'A' ? 1 : 2 } }
  return new Response(JSON.stringify(data), { status: code, headers: { 'Content-Type': 'application/json' } })
}
// Render a local challenge, never load Cloudflare's script.
window.turnstile = {
  render(el, opts) {
    const button = document.createElement('button'); button.type = 'button'; button.textContent = '完成本地验证码'
    button.onclick = () => (opts.callback as (token: string) => void)('fixture-token')
    el.append(button); return 'fixture-widget'
  },
  remove() { document.querySelector('.login-captcha')?.replaceChildren() },
  reset() {},
}
const nodes = ['A', 'B'].map(id => ({ id, name: `预览节点 ${id}`, has_api: true, has_basic: true } as Hy2Node))
function Preview() {
  const [view, setView] = useState('node')
  const [node, setNode] = useState(nodes[0])
  const [revision, setRevision] = useState(0)
  const [, redraw] = useState(0)
  return <>
    <div style={{ padding: 16, display: 'flex', flexWrap: 'wrap', gap: 12 }}>
      <button id="node-view" onClick={() => { setView('node'); setRevision(n => n + 1) }}>节点预览</button>
      <button id="login-view" onClick={() => { setView('login'); setRevision(n => n + 1) }}>登录预览</button>
      <button id="switch-node" onClick={() => setNode(n => n.id === 'A' ? nodes[1] : nodes[0])}>切换 A/B</button>
      <label>能力响应 <select id="caps-mode" defaultValue={fixture.caps} onChange={e => { fixture.caps = e.target.value }}>
        {['error', 'ok', 'blocked', 'cache', 'missing', 'unreachable', 'unsupported', 'absent', 'inactive', 'state-error'].map(v => <option key={v}>{v}</option>)}
      </select></label>
      <label>登录配置 <select id="login-mode" defaultValue={fixture.login} onChange={e => { fixture.login = e.target.value }}>
        {['error', 'disabled', 'enabled', 'malformed'].map(v => <option key={v}>{v}</option>)}
      </select></label>
      <label><input id="hold-a" type="checkbox" onChange={e => { fixture.holdA = e.target.checked }} />挂起 A 请求</label>
      <label><input id="hold-action" type="checkbox" onChange={e => { fixture.holdAction = e.target.checked }} />挂起动作</label>
      <button id="release" onClick={() => { fixture.pending.splice(0).forEach(resolve => resolve()); redraw(n => n + 1) }}>释放旧请求</button>
      <button id="reset" onClick={() => { fixture.requests.length = 0; fixture.notices.length = 0; setRevision(n => n + 1) }}>重新挂载</button>
    </div>
    <div key={revision} style={{ padding: 24 }}>
      {view === 'login' ? <LoginView onLogin={() => fixture.notices.push('logged in')} /> : <NodeControlPanel node={node} notify={message => { fixture.notices.push(message); redraw(n => n + 1) }} />}
    </div>
    <output id="notices">{fixture.notices.join('\n')}</output>
  </>
}
createRoot(document.getElementById('root')!).render(<Preview />)
