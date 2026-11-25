import type {
  Alert, AutoHideState, BlockChecks, Hy2Node, MonitoringNode,
  NodeMonitoringResponse, Server, Session, SpeedtestSummary,
} from '../src/lib/api'

export const scenarios = ['normal', 'abnormal', 'empty', 'error'] as const
export type PreviewScenario = typeof scenarios[number]
export const scenarioLabels: Record<PreviewScenario, string> = {
  normal: '正常', abnormal: '异常', empty: '空', error: '读取失败',
}
export const previewSession: Session = {
  token: 'local-design-preview-only', email: 'preview@example.test', role: 'admin',
}
const GiB = 1024 ** 3
const carriers = [
  ['telecom', '电信'], ['unicom', '联通'], ['mobile', '移动'], ['other', '公共'],
] as const
const locations = [
  ['东京', 'AWS', 0], ['新加坡', 'Oracle', 68], ['香港', 'Vultr', 43],
  ['首尔', 'AWS', 37], ['洛杉矶', 'NTT', 128], ['阿什本', 'AWS', 183],
  ['法兰克福', 'Hetzner', 224], ['伦敦', 'Oracle', 241], ['悉尼', 'AWS', 116],
  ['大阪', 'Vultr', 12], ['台北', 'Cloud', 51], ['多伦多', 'Cloud', 176],
  ['东京 · 跨境业务专线备用长名称', 'Alibaba Cloud', 6],
] as const

function speedtest(index: number, now: number): SpeedtestSummary {
  const targets = carriers.flatMap(([carrier, label], c) => [0, 1, 2].map((t) => ({
    key: `${carrier}-${t}`, name: `示例${label}靶点 ${t + 1}`,
    host: `192.0.2.${10 + c * 3 + t}`, carrier, carrier_label: label,
    icmp_avg_ms: 24 + index * 13 + c * 9 + t * 3,
    icmp_loss_pct: 0, tcp_connect_ms: 18 + index * 11 + c * 7 + t,
    download_mbps: null, upload_mbps: null, probe_ok: true, probe_error: '',
  })))
  return {
    measured_at: new Date(now - 3 * 60_000).toISOString(), probe_ok: true, probe_error: '', targets,
    by_carrier: Object.fromEntries(carriers.map(([carrier, label]) => {
      const rows = targets.filter((t) => t.carrier === carrier)
      return [carrier, {
        carrier, label, avg_ms: rows.reduce((sum, t) => sum + t.icmp_avg_ms, 0) / rows.length,
        avg_loss_pct: 0, best_target: rows[0].name, best_ms: rows[0].icmp_avg_ms,
        measured_targets: rows.length, total_targets: rows.length,
      }]
    })),
  }
}

export function createFixture(scenario: PreviewScenario = 'abnormal', now = Date.now()) {
  const checkedAt = new Date(now - 35_000).toISOString()
  const nodes: MonitoringNode[] = scenario === 'empty' ? [] : locations.map(([region, provider, latency], i) => ({
    node_id: `preview-node-${i + 1}`, node_name: `${region}-${provider}-${14000 + i}`,
    server_id: `preview-server-${i + 1}`, server_name: `${region}-${provider}`,
    host: `198.51.100.${20 + i}`, region, provider, node_status: 'online',
    status_source: i === 4 ? 'api' : 'ssh', node_status_error: '', listen_port: 14000 + i,
    users_count: [42, 28, 73, 11, 18, 8, 6, 0, 12, 31, 23, 7, 19][i],
    server_status: 'online', server_status_error: '',
    cpu_percent: i === 4 ? 0 : [18, 37, 61, 25, 0, 9, 44, 0, 21, 16, 32, 14, 48][i],
    memory_percent: i === 4 ? 0 : 23 + i * 4, disk_percent: i === 4 ? 0 : 32 + i * 3,
    load_average: i === 4 ? '' : '0.32 0.41 0.29',
    net_latency_ms: latency, net_latency_checked_at: checkedAt,
    ssh_handshake_ms: i === 4 ? null : latency + 36,
    tx_bytes_24h: i === 7 ? 0 : Math.round((13 - i) * 4.7 * GiB),
    rx_bytes_24h: i === 4 || i === 7 ? 0 : Math.round((13 - i) * 1.3 * GiB),
    speedtest: speedtest(i, now),
  }))
  if (scenario === 'abnormal' || scenario === 'error') {
    Object.assign(nodes[3], { node_status: 'warning', node_status_error: 'unreachable', net_latency_ms: 287 })
    Object.assign(nodes[5], {
      node_status: 'offline', node_status_error: 'unreachable', server_status: 'offline',
      status_source: 'api', net_latency_ms: null, ssh_handshake_ms: null,
      net_latency_checked_at: new Date(now - 42 * 60_000).toISOString(),
      cpu_percent: 0, memory_percent: 0, disk_percent: 0, load_average: '',
      users_count: -1, speedtest: null, tx_bytes_24h: 0, rx_bytes_24h: 0,
    })
    Object.assign(nodes[7], {
      node_status: 'unknown', status_source: '', users_count: -1,
      net_latency_ms: null, net_latency_checked_at: null, ssh_handshake_ms: null,
      cpu_percent: 0, memory_percent: 0, disk_percent: 0, load_average: '', speedtest: null,
    })
    // online + unauthorized 不是离线；API 不提供资源指标，数值 0 不应显示成空闲。
    Object.assign(nodes[8], {
      status_source: 'api', node_status_error: 'unauthorized', users_count: -1,
      cpu_percent: 0, memory_percent: 0, disk_percent: 0, load_average: '',
      ssh_handshake_ms: null, speedtest: null,
    })
    Object.assign(nodes[10], {
      status_source: 'api', server_status: 'warning',
      server_status_error: 'SSH 凭据失效: unable to authenticate',
      cpu_percent: 0, memory_percent: 0, disk_percent: 0, load_average: '', ssh_handshake_ms: null,
    })
    Object.assign(nodes[11], {
      node_status: 'unknown', status_source: 'api', node_status_error: '节点 API 密钥解密失败',
      users_count: -1, cpu_percent: 0, memory_percent: 0, disk_percent: 0,
      load_average: '', ssh_handshake_ms: null, speedtest: null,
    })
    const partial = nodes[2].speedtest!
    partial.targets.filter((t) => t.carrier === 'mobile').forEach((t) => {
      t.icmp_avg_ms = null; t.icmp_loss_pct = 100; t.tcp_connect_ms = null
    })
    Object.assign(partial.by_carrier.mobile, {
      avg_ms: null, avg_loss_pct: 100, best_target: '', best_ms: null, measured_targets: 0,
    })
    nodes[12].speedtest = {
      measured_at: checkedAt, probe_ok: false, probe_error: '示例：探针未安装 ping', targets: [], by_carrier: {},
    }
  }
  const monitoring: NodeMonitoringResponse = {
    nodes, count: nodes.length,
    speedtest_note: '示例数据：测速在节点本机执行，反映节点与大陆三网的线路质量，不代表你家宽带连节点的速度。',
  }
  const servers: Server[] = nodes.map((n) => ({
    id: n.server_id, name: n.server_name, host: n.host, ssh_port: 22, ssh_user: 'root',
    region: n.region, provider: n.provider, tags: ['本地示例'], status: n.server_status,
    created_at: checkedAt, updated_at: checkedAt, cpu_percent: n.cpu_percent,
    memory_percent: n.memory_percent, disk_percent: n.disk_percent,
    tx_bytes_24h: n.tx_bytes_24h, rx_bytes_24h: n.rx_bytes_24h,
    measured_bytes_24h: n.status_source === 'ssh' ? n.tx_bytes_24h + n.rx_bytes_24h : 0,
    portal_bytes_24h: n.status_source === 'api' ? n.tx_bytes_24h : 0,
    latency_ms: n.net_latency_ms ?? 0, allow_write_ops: false, write_units: [],
    has_credential: n.status_source === 'ssh', traffic_api_url: '', traffic_secret_file: '',
  }))
  const hy2Nodes: Hy2Node[] = nodes.map((n) => ({
    id: n.node_id, server_id: n.server_id, name: n.node_name, version: '2.6.3',
    listen_port: n.listen_port, domain: `${n.node_id}.example.test`, status: n.node_status,
    last_checked_at: n.net_latency_checked_at, created_at: checkedAt, updated_at: checkedAt,
    used_bytes_total: n.tx_bytes_24h * 12, used_bytes_24h: n.tx_bytes_24h,
    api_url: `https://${n.node_id}.example.test/api/v1`, has_api: n.status_source !== '',
    has_basic: n.status_source !== '', users_count: n.users_count, api_checked_at: n.net_latency_checked_at,
    in_store: n.node_status === 'online' && !n.node_status_error,
    status_error: n.node_status_error, health_ok: n.node_status === 'online' && !n.node_status_error,
  }))
  const alerts: Alert[] = nodes.filter((n) => n.node_status === 'offline' || n.node_status === 'warning').map((n) => ({
    id: `preview-alert-${n.node_id}`, type: n.node_status === 'offline' ? 'node_offline' : 'node_unstable',
    severity: n.node_status === 'offline' ? 'critical' : 'warning', status: 'open',
    server_name: n.server_name, host: n.host, node_name: n.node_name, node_port: n.listen_port,
    message: `示例：${n.node_status === 'offline' ? '节点不可达' : '连续多轮探测未成功'}`,
    resolved_at: null, first_seen_at: checkedAt, last_seen_at: checkedAt,
  }))
  const autoHide: AutoHideState = {
    enabled: true, after_minutes: 5,
    hidden: nodes.filter((n) => n.node_status === 'offline').map((n) => ({
      node_id: n.node_id, name: n.node_name, reason: '示例：端口不通超过 5 分钟', since: checkedAt,
    })),
  }
  const blockChecks: BlockChecks = {
    probe: nodes.length ? { server_id: servers[0].id, name: '示例国内探测点', host: '203.0.113.10' } : null,
    items: nodes.map((n) => ({
      node_id: n.node_id, node_name: n.node_name, node_status: n.node_status,
      target_ip: n.host, tcp_port: n.listen_port,
      tcp_ok: n.node_status === 'unknown' ? null : n.node_status !== 'offline',
      ping_ok: n.node_status === 'unknown' ? null : n.node_status !== 'offline',
      blocked: n.node_status === 'unknown' ? null : n.node_status === 'offline',
      detail: n.node_status === 'unknown' ? '示例：尚未检测' : n.node_status === 'offline' ? '示例：TCP 与 Ping 均未通过，需进一步核实' : '示例：国内探测可达',
      checked_at: n.node_status === 'unknown' ? null : checkedAt,
    })),
  }
  return { monitoring, servers, hy2Nodes, alerts, autoHide, blockChecks }
}

// 可供其他本地设计入口直接复用；工厂允许指定时间，便于固定截图基线。
export const normalFixture = createFixture('normal')
export const designFixture = createFixture('abnormal')
export const emptyFixture = createFixture('empty')

export type MockNotice = { method: string; path: string; message: string; ok: boolean }

/** 封闭 fetch：不保存、不调用原生 fetch，包括未知路径和绝对生产 URL。 */
export function createPreviewFetch(
  scenario: PreviewScenario,
  onWrite: (notice: MockNotice) => void = () => {},
): typeof fetch {
  const fixture = createFixture(scenario)
  const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), {
    status, headers: { 'Content-Type': 'application/json', 'X-Local-Design-Preview': 'example-data' },
  })
  return async (input, init) => {
    const request = input instanceof Request ? input : null
    const url = new URL(request?.url ?? String(input), 'http://preview.invalid')
    // VITE_API_URL 即使配置了绝对地址，也只在内存中匹配 /api 路径。
    const apiStart = url.pathname.indexOf('/api/')
    const path = apiStart >= 0 ? url.pathname.slice(apiStart) : url.pathname
    const method = (init?.method ?? request?.method ?? 'GET').toUpperCase()
    if (init?.signal?.aborted || request?.signal.aborted) throw new DOMException('Aborted', 'AbortError')
    const writing = !['GET', 'HEAD', 'OPTIONS'].includes(method)
    const fail = (message: string, status = 501) => {
      if (writing) onWrite({ method, path, message, ok: false })
      return json({ error: message, mock: true }, status)
    }
    const submitted = (body: object) => {
      const message = '模拟已提交 · 仅本地示例，未执行任何远程操作'
      onWrite({ method, path, message, ok: true })
      return json({ ...body, mock: true, message })
    }
    if (method === 'POST' && path === '/api/login') return submitted(previewSession)
    if (method === 'DELETE' && path === '/api/auth/terminal-session') return submitted({ ready: false })
    if (method === 'POST' && path === '/api/auth/terminal-session') return fail('本地预览不开放 SSH 终端', 403)
    if (writing) {
      let payload: Record<string, unknown> = {}
      try {
        const body = init?.body ?? (request ? await request.clone().text() : '')
        if (body) payload = JSON.parse(String(body))
      } catch { return fail('本地 mock：请求体不是有效 JSON', 400) }
      if (method === 'POST' && path === '/api/speedtest/run') {
        const nodeID = url.searchParams.get('node_id')
        if (nodeID && !fixture.monitoring.nodes.some((n) => n.node_id === nodeID)) return fail('本地 mock：示例节点不存在', 404)
        return submitted({ ok: true, started: nodeID ? 1 : fixture.monitoring.count })
      }
      if (method === 'PUT' && path === '/api/nodes/auto-hide') {
        fixture.autoHide.enabled = payload.enabled === true
        return submitted(fixture.autoHide)
      }
      if (method === 'POST' && path === '/api/block-checks/run') return submitted({ checked: fixture.blockChecks.items.length })
      if (method === 'PUT' && path === '/api/settings/cn-probe') {
        const server = fixture.servers.find((s) => s.id === payload.server_id)
        if (payload.server_id && !server) return fail('本地 mock：示例探测主机不存在', 404)
        fixture.blockChecks.probe = server ? { server_id: server.id, name: server.name, host: server.host } : null
        return submitted({ ok: true })
      }
      return fail(`本地 mock 未定义写请求：${method} ${path}；未发送网络请求`)
    }
    if (method !== 'GET') return fail(`本地 mock 未定义：${method} ${path}`)
    if (path === '/api/monitoring/nodes') return scenario === 'error'
      ? json({ error: '本地示例：监控读取失败，请切换场景重试', mock: true }, 503)
      : json(fixture.monitoring)
    const statusMatch = path.match(/^\/api\/nodes\/([^/]+)\/portal-status$/)
    if (statusMatch) {
      const unsupported = { state: 'unsupported', error: '本地示例未提供该能力' }
      const first = statusMatch[1] === fixture.hy2Nodes[0]?.id
      // 第 2 台：通道已启用，但买家拿不到公开 encryption 参数。
      // 用于验收「已启用 ≠ 可交付」那条原因尾巴（与 capabilities 的
      // delivery_error 对齐），而不是显示成一切正常。
      const second = statusMatch[1] === fixture.hy2Nodes[1]?.id
      return json({ node_id: statusMatch[1], node_name: '本地示例', probes: {
        version: unsupported, bbr: unsupported, reality: unsupported,
        warp: unsupported, awg: unsupported, gost: unsupported, speed: unsupported,
        vmess: first
          ? { state: 'healthy', payload: { installed: true, active: true, config: { domain: 'example.test', port: 8443, ws_path: '/example' } } }
          : { state: 'unsupported', error: '旧节点门户尚未提供 VMess 端点' },
        // 新协议已启用示例；其余节点模拟旧门户（unsupported），
        // 用于检查按钮保留 + 原因说明，而不是直接消失。
        vlessenc: first
          ? { state: 'healthy', payload: { installed: true, active: true, config: { domain: 'enc.example.test', port: 2088, sni: 'enc.example.test' } } }
          : second
            ? { state: 'healthy', payload: { installed: true, active: true, config: { domain: 'enc2.example.test', port: 2088, sni: 'enc2.example.test' } } }
            : { state: 'unsupported', error: '旧节点门户尚未提供 VLESS Encryption 端点' },
      } })
    }
    const reads: Record<string, unknown> = {
      '/api/health': { status: 'ok', time: new Date().toISOString() },
      '/api/servers': fixture.servers, '/api/nodes': fixture.hy2Nodes,
      '/api/nodes/capabilities': {
        // 三种 VLESS Encryption 状态各给一台，便于浏览器验收：
        //   0 已启用且可交付（公开 encryption 可读）
        //   1 已启用但不可交付（缺 Basic / 公开参数读不到）
        //   2 旧门户：没有 vlessenc 段（且段值可能是 null，前端必须 null 安全）
        // 其余只有 Hy2。
        nodes: fixture.hy2Nodes.map((node, index) => {
          const base: Record<string, unknown> = { hysteria2: { active: true } }
          const protocols: Record<string, unknown> =
            index === 0
              ? {
                  ...base,
                  vmess: { active: true },
                  vlessenc: {
                    installed: true, active: true, configured: true,
                    domain: 'enc.example.test', sni: 'enc.example.test', port: 2088,
                    client_encryption_present: true,
                  },
                  future_protocol: { active: true },
                }
              : index === 1
                ? {
                    ...base,
                    vlessenc: {
                      installed: true, active: true, configured: true,
                      domain: 'enc2.example.test', sni: 'enc2.example.test', port: 2088,
                      client_encryption_present: false,
                      delivery_error: '未绑定 portal 网页凭据，无法读取公开的客户端 encryption 参数；该通道不可交付',
                    },
                  }
                : index === 2
                  ? { ...base, vlessenc: null }
                  : base
          return {
            node_id: node.id, node_name: node.name, reachable: true, supported: true,
            protocols, extras: {},
          }
        }),
        fetched: fixture.hy2Nodes.length, reachable: fixture.hy2Nodes.length, outdated: 0,
      },
      '/api/tasks': [], '/api/alerts': fixture.alerts,
      '/api/traffic/hourly': [], '/api/metrics/recent': {},
      '/api/nodes/auto-hide': fixture.autoHide, '/api/block-checks': fixture.blockChecks,
      '/api/categories': { categories: [] }, '/api/xui/panels': [],
      '/api/ip-purity': { items: [], count: 0 }, '/api/servers/billing': [],
      '/api/speedtest/targets': [],
    }
    return Object.hasOwn(reads, path) ? json(reads[path])
      : fail(`本地 mock 未定义读取：${method} ${path}；未发送网络请求`)
  }
}
