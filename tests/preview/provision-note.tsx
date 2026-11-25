import React, { useState } from 'react'
import { createRoot } from 'react-dom/client'
import { SubscriptionCenter } from '../../src/components/SubscriptionCenter'
import '../../src/styles.css'

// 本地夹具：所有请求都拦截，不连后端、不需要凭据。
// 目标：验证「开户结果未确认」这条新状态真的显示出来，
// 而不是像原来那样被当成"一个都没开成、客户必定连不上"。
const fixture = {
  provision: 'unverified' as 'complete' | 'partial' | 'unverified',
  requests: [] as { path: string; method: string; body?: string }[],
  notices: [] as string[],
}
Object.assign(window, { provisionFixture: fixture })

const center = {
  fleet: { node_count: 2, node_in_store: 2 },
  store: {
    enabled: false,
    key_hint: '',
    create_endpoint: '/api/store/orders',
    renew_endpoint: '/api/store/renew',
    revoke_endpoint: '/api/store/revoke',
    query_endpoint: '/api/store/orders',
    auth_header: 'X-Store-Key',
    ip_window_hours: 24,
    expiry_grace_hours: 24,
  },
  subscriptions: [{
    id: '00000000-0000-0000-0000-000000000101',
    token: 'fixture-token',
    label: '夹具订阅',
    enabled: true,
    expires_at_text: '2026-12-31 23:59',
    ip_limit: 3,
    node_count: 2,
    order_id: 'fixture-order',
    user_email: 'buyer@example.test',
    created_at: '2026-10-30T00:00:00Z',
    last_accessed_at: null,
    access_count: 0,
    category: null,
    expired: false,
    expires_at: Math.floor(Date.now() / 1000) + 86400,
    days_left: 30,
    active_ip_count: 0,
    // 这些数组必须是数组而不是缺省：组件直接读 `.length`，
    // 缺字段会让整个页面抛异常（这正是"没采到 ≠ 没有"要分开的地方）。
    active_ips: [],
    online_ips: [],
    online_ip_count: 0,
    online_ips_readable: true,
    current_online_ips: [],
    current_online_ips_readable: true,
    current_online_ips_complete: true,
    current_online_hy2_nodes: 2,
    current_online_reality_nodes: 0,
    current_online_expected_nodes: 2,
    current_online_errors: [],
    current_online_sampled_at: null,
    auto_delete: true,
    note: '',
    delivery_url: 'https://panel.example.test/user/fixture-token',
    subscription_url: 'https://panel.example.test/sub/fixture-token',
    used_bytes: 0,
    used_bytes_legacy: 0,
    node_accounts_count: 2,
  }],
  nodes: [
    { id: 'A', name: '夹具节点-A', domain: 'a.example.test', listen_port: 443, in_store: true, status: 'online' },
    { id: 'B', name: '夹具节点-B', domain: 'b.example.test', listen_port: 443, in_store: true, status: 'online' },
  ],
  categories: [],
  orders: [],
}

window.fetch = async (input, init) => {
  const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, location.href)
  if (url.origin !== location.origin || !url.pathname.startsWith('/api/')) {
    throw new Error('Preview blocks non-fixture requests')
  }
  const method = (init?.method ?? 'GET').toUpperCase()
  fixture.requests.push({ path: url.pathname + url.search, method, body: typeof init?.body === 'string' ? init.body : undefined })
  let data: unknown = {}
  if (url.pathname === '/api/subscriptions' && method === 'POST') {
    if (fixture.provision === 'unverified') {
      data = {
        id: '00000000-0000-0000-0000-000000000202',
        token: 'new-token',
        label: '新订阅',
        expires_at_text: '2026-12-31 23:59',
        delivery_url: 'https://panel.example.test/user/new-token',
        subscription_url: 'https://panel.example.test/sub/new-token',
        provisioned_nodes: 0,
        expected_nodes: 0,
        excluded: [],
        provision_status: 'unverified',
        provision_note: '读取节点范围失败，无法判断每个节点是否已开户。请打开「账号对账」复查后再交付。',
      }
    } else if (fixture.provision === 'partial') {
      data = {
        id: '00000000-0000-0000-0000-000000000202',
        token: 'new-token',
        label: '新订阅',
        expires_at_text: '2026-12-31 23:59',
        delivery_url: 'https://panel.example.test/user/new-token',
        subscription_url: 'https://panel.example.test/sub/new-token',
        provisioned_nodes: 1,
        expected_nodes: 2,
        excluded: [{ node_id: 'B', node_name: '夹具节点-B', reason: '该节点未开户成功，此节点上的客户端会认证失败。可稍后用「账号对账」重试。' }],
        provision_status: 'partial',
      }
    } else {
      data = {
        id: '00000000-0000-0000-0000-000000000202',
        token: 'new-token',
        label: '新订阅',
        expires_at_text: '2026-12-31 23:59',
        delivery_url: 'https://panel.example.test/user/new-token',
        subscription_url: 'https://panel.example.test/sub/new-token',
        provisioned_nodes: 2,
        expected_nodes: 2,
        excluded: [],
        provision_status: 'complete',
      }
    }
  } else if (url.pathname === '/api/subscriptions') {
    data = center
  } else if (url.pathname === '/api/categories') {
    data = { categories: [] }
  } else if (url.pathname === '/api/nodes') {
    data = center.nodes
  } else if (url.pathname === '/api/servers') {
    data = []
  }
  return new Response(JSON.stringify(data), { status: 200, headers: { 'Content-Type': 'application/json' } })
}

function App() {
  const [notices, setNotices] = useState<string[]>([])
  const notify = (message: string) => { fixture.notices.push(message); setNotices([...notices, message]) }
  return (
    <div style={{ padding: 24 }}>
      <p className="form-note">
        夹具模式：<strong>{fixture.provision}</strong>。点「手工开订阅」提交后，交付弹窗应按此模式显示开户结果。
      </p>
      <div className="stack" style={{ display: 'grid', gap: 12 }}>{notices.map((n, i) => <p key={i} className="form-note">{n}</p>)}</div>
      <SubscriptionCenter notify={notify} />
    </div>
  )
}

createRoot(document.getElementById('root')!).render(<App />)