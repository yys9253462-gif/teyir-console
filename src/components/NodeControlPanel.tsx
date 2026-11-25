import { useCallback, useEffect, useRef, useState } from 'react'
import { AlertTriangle, Ban, Boxes, Cpu, Globe, KeyRound, Loader2, Plus, Power, RefreshCw, Shield, Trash2, Users, Zap } from 'lucide-react'
import {
  api,
  loadSession,
  ApiError,
  type Hy2Node,
  type NodeOverview,
  type NodeProxyService,
  type NodeUser,
  type NodeUserCreated,
  type PortalNodeStatus,
  type PortalVerb,
  type ProxyProtocol,
  isAdminSession, portalActionAdminOnly, ADMIN_ONLY_HINT } from '../lib/api'
import { Modal } from './Modal'
import { NodeEgress } from './NodeEgress'
import { askConfirm } from './ConfirmHost'
import { formatBytes } from '../lib/format'
import { copyToClipboard } from '../lib/clipboard'
import { useCopiedFlash } from '../lib/copiedFlash'

// NodeControlPanel is the node-side control surface, shown when a node with a
// bound portal API is opened.
//
// Two credential tiers are in play, and the panel reflects both:
//
//  1. The Bearer api_key (always present once a node is bound) reaches exactly
//     three GET paths and four write paths under /api/v1/: users, gost proxy
//     services, and a read-only overview. That is the 用户 / 代理服务 / 状态 tabs.
//
//  2. The portal's web-session credential — portal-access.json's username and
//     password, sent as HTTP Basic — reaches everything else: version and
//     self-upgrade, BBR, Reality, WARP, AmneziaWG, gost/xray installs, and a
//     hard reboot. That is the 节点运维 tab, and it is only usable when
//     node.has_basic is true. Until then the tab renders an explanation instead
//     of a wall of buttons that would all 401.
//
// Design notes:
//  - Nothing here is polled on a timer. Each read is a round trip to the node;
//    the operator refreshes when they care.
//  - traffic_* values from the portal are quota accounting, not live traffic.
//    They are labelled as such in the UI so they are never misread as a rate.
//    The one true rate shown here comes from portal's traffic-speed endpoint,
//    which is a self-maintained sliding window and needs no counter draining.

function formatExpiry(epochSeconds: number): string {
  if (!epochSeconds || epochSeconds <= 0) return '—'
  const date = new Date(epochSeconds * 1000)
  return date.toLocaleString('zh-CN', { hour12: false })
}

// daysLeft renders the remaining validity in days. A negative value means the
// user has already lapsed, which the portal enforces on the next auth.
function daysLeft(epochSeconds: number): number | null {
  // 没有到期时间（0/空）表示永久有效，不能显示成「剩 0 天」。
  if (!epochSeconds || epochSeconds <= 0) return null
  return Math.ceil((epochSeconds * 1000 - Date.now()) / 86400000)
}

// showTitle 为 false 时不渲染面板自身的标题块。面板有两种宿主：
//  - 节点列表弹窗（NodesPage 的 Modal）已经在头部写了「节点控制 · <节点名>」，
//    再画一次就成了同一句话连出两行。
//  - 抽屉/内联场景没有 Modal 包裹，需要面板自带标题。
export function NodeControlPanel({ node, notify, showTitle = true }: {
  node: Hy2Node
  notify: (message: string) => void
  showTitle?: boolean
}) {
  const [tab, setTab] = useState<'users' | 'proxy' | 'egress' | 'status' | 'ops'>('users')

  // 页签在两种宿主里都必须显示，所以头部始终存在；标题块是可选的。
  const head = (
    <div className="service-panel-head">
      {showTitle && (
        <div className="service-panel-title">
          <div className="section-kicker">NODE CONTROL</div>
          <strong>节点控制 · {node.name}</strong>
        </div>
      )}
      <div className="node-tabs">
          <button className={`button button-compact ${tab === 'users' ? 'button-primary' : 'button-quiet'}`} onClick={() => setTab('users')}><Users size={13} />用户</button>
          <button className={`button button-compact ${tab === 'proxy' ? 'button-primary' : 'button-quiet'}`} onClick={() => setTab('proxy')}><Boxes size={13} />代理服务</button>
          <button className={`button button-compact ${tab === 'egress' ? 'button-primary' : 'button-quiet'}`} onClick={() => setTab('egress')}><Globe size={13} />出口<span className="new-pill">新</span></button>
          <button className={`button button-compact ${tab === 'status' ? 'button-primary' : 'button-quiet'}`} onClick={() => setTab('status')}><RefreshCw size={13} />状态</button>
          <button className={`button button-compact ${tab === 'ops' ? 'button-primary' : 'button-quiet'}`} onClick={() => setTab('ops')}><Cpu size={13} />节点运维</button>
        </div>
    </div>
  )

  return (
    <div className="service-panel">
      {head}
      {tab === 'users' && <NodeUsers key={node.id} node={node} notify={notify} />}
      {tab === 'proxy' && <NodeProxyServices key={node.id} node={node} notify={notify} />}
      {tab === 'egress' && <NodeEgress key={node.id} node={node} notify={notify} />}
      {tab === 'status' && <NodeStatus key={node.id} node={node} />}
      {tab === 'ops' && <NodeOps key={node.id} node={node} notify={notify} />}
    </div>
  )
}

function NodeUsers({ node, notify }: { node: Hy2Node; notify: (message: string) => void }) {
  const readOnly = loadSession()?.role === "viewer"
  const [users, setUsers] = useState<NodeUser[]>([])
  const [note, setNote] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [creating, setCreating] = useState(false)
  const [renewing, setRenewing] = useState<NodeUser | null>(null)
  const [pendingDelete, setPendingDelete] = useState<NodeUser | null>(null)
  const [deleting, setDeleting] = useState(false)
  const [created, setCreated] = useState<NodeUserCreated | null>(null)

  // 2026-11-06：只采用最后一次请求的结果。在抽屉里切换节点时，上一台节点的用户列表晚回来会显示在
  // 当前节点下 —— 删除 / 续期按钮随即拿着别的节点的用户去操作当前节点。
  const usersSeq = useRef(0)
  const load = useCallback(async () => {
    const seq = ++usersSeq.current
    setLoading(true)
    try {
      const response = await api.nodeUsers(node.id)
      if (seq !== usersSeq.current) return
      setUsers(response.users ?? [])
      setNote(null)
    } catch (err) {
      if (seq !== usersSeq.current) return
      setUsers([])
      setNote(err instanceof Error ? `读取用户列表失败：${err.message}` : '读取用户列表失败')
    } finally {
      if (seq === usersSeq.current) setLoading(false)
    }
  }, [node.id])

  useEffect(() => { void load() }, [load])

  async function confirmDelete() {
    if (!pendingDelete || deleting) return
    const target = pendingDelete
    setDeleting(true)
    try {
      await api.deleteNodeUser(node.id, target.user_id)
      setPendingDelete(null)
      notify(`已删除用户：${target.user_id}`)
      await load()
    } catch (err) {
      notify(err instanceof Error ? `删除失败：${err.message}` : '删除失败')
    } finally {
      setDeleting(false)
    }
  }

  return (
    <>
      <div className="node-toolbar">
        <span className="form-note">共 {users.length} 个用户 · 真实累计从节点计量起算；未初始化时显示旧口径并标明质量</span>
        <div className="node-toolbar-actions">
          <button className="button button-quiet button-compact" onClick={() => void load()} disabled={loading}>
            {loading ? <Loader2 size={13} className="spin" /> : <RefreshCw size={13} />}刷新
          </button>
          <button className="button button-primary button-compact" disabled={loadSession()?.role === "viewer"} title={loadSession()?.role === "viewer" ? "只读账号不能创建" : undefined} onClick={() => setCreating(true)}><Plus size={13} />新建用户</button>
        </div>
      </div>

      {note && <p className="form-note service-warn">{note}</p>}

      {users.length === 0 && !note && !loading && (
        <div className="empty-state compact"><Users size={18} /><strong>该节点还没有用户</strong><span>点「新建用户」由 portal 生成订阅链接。</span></div>
      )}

      {users.map((user) => {
        const left = daysLeft(user.expires_at)
        const managed = user.user_id === 'admin_master' || user.user_id.startsWith('sub_')
        const managedHint = user.user_id === 'admin_master' ? '机主账号不可在这里续期或删除' : '订阅账号请在订阅中心续费、停用或删除'
        return (
          <div className="service-row" key={user.user_id}>
            <div className="service-identity">
              <strong>{user.user_id}</strong>
              <div className="service-badges">
                <span className={`status-pill ${user.status === 'active' ? 'online' : 'offline'}`}>{user.status === 'active' ? '启用' : user.status}</span>
                <span className="service-badge-muted">
                  {user.online_devices == null ? 'Hy2 连接数未知' : `Hy2 已连接 ${user.online_devices} 个实例（不含 Reality）`}
                </span>
                <span className={`service-badge-muted ${left !== null && left < 0 ? 'service-error' : ''}`}>
                  {left === null ? '永久有效' : left < 0 ? `已过期 ${-left} 天` : `剩 ${left} 天`}{left === null ? '' : ` · ${formatExpiry(user.expires_at)}`}
                </span>
                {user.measured_bytes == null && <span className="service-badge-muted service-error">真实计量未初始化</span>}
                {user.measured_bytes != null && !user.measurement_ok && <span className="service-badge-muted service-error">计量有缺口或采集异常</span>}
                {user.traffic_limit_bytes > 0 && user.traffic_used_bytes >= user.traffic_limit_bytes && <span className="service-badge-muted service-error">配额已耗尽，新连接将被拒绝</span>}
                {user.ip_limit > 0 && <span className="service-badge-muted">IP≤{user.ip_limit}</span>}
                <span className="service-badge-muted">
                  {user.traffic_limit_bytes > 0
                    ? `${formatBytes(user.traffic_used_bytes)} / ${formatBytes(user.traffic_limit_bytes)}`
                    : `已用 ${formatBytes(user.traffic_used_bytes)} · 不限量`}
                </span>
              </div>
            </div>
            <div className="service-actions">
              {/* 2026-11-06：机主账号与订阅账号（sub_*）后端拒绝在这里续期 / 删除（到期、停用由订阅中心统一管理）；
                  原来按钮照常可点，填完表单才收到 400。 */}
              <button className="button button-quiet button-compact" disabled={readOnly || managed} title={managed ? managedHint : undefined} onClick={() => setRenewing(user)}>续期</button>
              <button className="button button-quiet button-compact" disabled={readOnly || !isAdminSession() || managed} title={managed ? managedHint : "删除用户"} onClick={() => setPendingDelete(user)}><Trash2 size={13} />删除</button>
            </div>
          </div>
        )
      })}

      {creating && (
        <CreateUserDialog node={node} notify={notify} onClose={() => setCreating(false)} onCreated={async (result) => { setCreating(false); setCreated(result); await load() }} />
      )}
      {renewing && (
        <RenewUserDialog node={node} user={renewing} notify={notify} onClose={() => setRenewing(null)} onDone={async () => { setRenewing(null); await load() }} />
      )}
      {pendingDelete && (
        <Modal title="删除节点用户" kicker="CONFIRM DELETE" onClose={() => { if (!deleting) setPendingDelete(null) }}>
          <p className="form-note">即将从节点 <strong>{node.name}</strong> 的 portal 删除用户 <strong>{pendingDelete.user_id}</strong>。该用户的订阅链接会立即失效，且无法恢复。</p>
          <div className="modal-actions"><button className="button button-quiet" disabled={deleting} onClick={() => setPendingDelete(null)}>取消</button><button className="button button-primary" disabled={deleting} onClick={() => void confirmDelete()}>{deleting ? '删除中…' : '确认删除'}</button></div>
        </Modal>
      )}
      {created && <CreatedUserDialog result={created} node={node} notify={notify} onClose={() => setCreated(null)} />}
    </>
  )
}

function CreateUserDialog({ node, notify, onClose, onCreated }: {
  node: Hy2Node
  notify: (message: string) => void
  onClose: () => void
  onCreated: (result: NodeUserCreated) => void
}) {
  const [userID, setUserID] = useState('')
  const [duration, setDuration] = useState('30')
  const [ipLimit, setIPLimit] = useState('0')
  const [trafficGB, setTrafficGB] = useState('0')
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)

  async function submit() {
    const days = Number(duration)
    const ips = Number(ipLimit)
    const gb = Number(trafficGB)
    // 流量配额后端按 int 解码，填 1.5 会整单 400「invalid payload」（2026-11-03）。
    if (!Number.isInteger(days) || !Number.isInteger(ips) || !Number.isInteger(gb) || days < 0 || ips < 0 || gb < 0) {
      notify('有效期、IP 限制与流量配额需为不小于 0 的整数')
      return
    }
    setBusy(true)
    try {
      const result = await api.createNodeUser(node.id, {
        user_id: userID.trim() || undefined,
        duration_days: days,
        ip_limit: ips,
        traffic_gb: gb,
        note: note.trim() || undefined,
      })
      notify(`已在 ${node.name} 上创建用户：${result.user_id}`)
      onCreated(result)
    } catch (err) {
      notify(err instanceof Error ? `创建失败：${err.message}` : '创建失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal title={`新建用户 · ${node.name}`} kicker="CREATE NODE USER" onClose={onClose}>
      <div style={{ display: 'grid', gap: 12 }}>
        <label className="form-field"><span>用户 ID（留空由 portal 生成）</span><input value={userID} onChange={(e) => setUserID(e.target.value)} placeholder="user-01" spellCheck={false} /></label>
        <div className="form-row">
          <label className="form-field"><span>有效期（天）</span><input value={duration} onChange={(e) => setDuration(e.target.value)} inputMode="numeric" /></label>
          <label className="form-field"><span>流量配额（GB，0=不限）</span><input value={trafficGB} onChange={(e) => setTrafficGB(e.target.value)} inputMode="numeric" /></label>
        </div>
        <label className="form-field"><span>IP 数限制（0=不限）</span><input value={ipLimit} onChange={(e) => setIPLimit(e.target.value)} inputMode="numeric" /></label>
        <label className="form-field"><span>备注</span><input value={note} onChange={(e) => setNote(e.target.value)} placeholder="可选" /></label>
        <p className="form-note">口令与订阅链接由节点上的 portal 生成，创建后只显示这一次，请及时复制。</p>
        <div className="modal-actions"><button className="button button-quiet" onClick={onClose}>取消</button><button className="button button-primary" disabled={busy} onClick={() => void submit()}>{busy ? '创建中…' : '创建用户'}</button></div>
      </div>
    </Modal>
  )
}

function RenewUserDialog({ node, user, notify, onClose, onDone }: {
  node: Hy2Node
  user: NodeUser
  notify: (message: string) => void
  onClose: () => void
  onDone: () => void | Promise<void>
}) {
  const [days, setDays] = useState('30')
  const [addTraffic, setAddTraffic] = useState('0')
  const [busy, setBusy] = useState(false)

  async function submit() {
    const extend = Number(days)
    const addGB = Number(addTraffic)
    if (!Number.isInteger(extend) || !Number.isInteger(addGB) || extend < 0 || addGB < 0) {
      notify('续期天数与新增流量需为不小于 0 的整数')
      return
    }
    if (extend === 0 && addGB === 0) {
      notify('请至少填写续期天数或新增流量')
      return
    }
    setBusy(true)
    try {
      const result = await api.renewNodeUser(node.id, user.user_id, { extend_days: extend, add_traffic_gb: addGB })
      notify(`已续期 ${user.user_id}，新到期：${formatExpiry(result.expires_at)}`)
      await onDone()
    } catch (err) {
      notify(err instanceof Error ? `续期失败：${err.message}` : '续期失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal title={`续期用户 · ${user.user_id}`} kicker="RENEW NODE USER" onClose={onClose}>
      <div style={{ display: 'grid', gap: 12 }}>
        <p className="form-note">当前到期：{formatExpiry(user.expires_at)}。续期从「当前到期时间」起算，过期用户则从此刻起算。</p>
        <label className="form-field"><span>延长天数</span><input value={days} onChange={(e) => setDays(e.target.value)} inputMode="numeric" /></label>
        <label className="form-field"><span>新增流量（GB，0=不加）</span><input value={addTraffic} onChange={(e) => setAddTraffic(e.target.value)} inputMode="numeric" /></label>
        <div className="modal-actions"><button className="button button-quiet" onClick={onClose}>取消</button><button className="button button-primary" disabled={busy} onClick={() => void submit()}>{busy ? '提交中…' : '确认续期'}</button></div>
      </div>
    </Modal>
  )
}

// CreatedUserDialog shows the one-time credentials and lets the operator copy
// each artifact. Nothing here is persisted by the panel.
function CreatedUserDialog({ result, node, notify, onClose }: {
  result: NodeUserCreated
  node: Hy2Node
  notify: (message: string) => void
  onClose: () => void
}) {
  const [copied, flashCopied] = useCopiedFlash()

  async function copy(label: string, value: string) {
    if (!value) return
    const ok = await copyToClipboard(value)
    if (ok) {
      flashCopied(label)
      notify(`已复制${label}`)
    } else {
      notify('复制失败，请手动选中文本复制')
    }
  }

  const rows: Array<{ label: string; value: string; mono: boolean }> = [
    { label: '用户 ID', value: result.user_id, mono: true },
    { label: '密码', value: result.password, mono: true },
    { label: '到期时间', value: formatExpiry(result.expires_at), mono: false },
    { label: 'Hysteria2 订阅链接', value: result.uri, mono: true },
    { label: 'Reality 链接', value: result.reality_uri, mono: true },
    { label: 'Clash 配置', value: result.clash, mono: true },
    { label: 'sing-box 配置', value: result.sing_box, mono: true },
  ]

  return (
    <Modal title={`已创建 · ${result.user_id}`} kicker="CREDENTIALS (SHOW ONCE)" onClose={onClose}>
      <div style={{ display: 'grid', gap: 10 }}>
        <p className="form-note service-warn">
          <KeyRound size={13} /> 这些凭据只在本次响应里返回，面板不保存。关闭后请到 portal 上重新获取。
        </p>
        {rows.filter((row) => row.value).map((row) => (
          <div className="cred-row" key={row.label}>
            <div className="cred-label">{row.label}</div>
            <div className={`cred-value ${row.mono ? 'mono' : ''}`} title={row.value}>{row.value}</div>
            <button className="button button-quiet button-compact" onClick={() => void copy(row.label, row.value)}>{copied === row.label ? '已复制' : '复制'}</button>
          </div>
        ))}
        <p className="form-note">节点：{node.name}</p>
        <div className="modal-actions"><button className="button button-primary" onClick={onClose}>我已保存</button></div>
      </div>
    </Modal>
  )
}

function NodeProxyServices({ node, notify }: { node: Hy2Node; notify: (message: string) => void }) {
  const [services, setServices] = useState<NodeProxyService[]>([])
  const [gostState, setGostState] = useState<{ installed?: boolean; active?: boolean; host?: string }>({})
  const [note, setNote] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [creating, setCreating] = useState(false)
  const [pendingDelete, setPendingDelete] = useState<NodeProxyService | null>(null)
  const [deleting, setDeleting] = useState(false)

  const servicesSeq = useRef(0)
  const load = useCallback(async () => {
    const seq = ++servicesSeq.current
    setLoading(true)
    try {
      const response = await api.nodeProxyServices(node.id)
      if (seq !== servicesSeq.current) return
      setServices(response.services ?? [])
      setGostState({ installed: response.gost_installed, active: response.gost_active, host: response.host })
      setNote(null)
    } catch (err) {
      if (seq !== servicesSeq.current) return
      setServices([])
      setGostState({})
      setNote(err instanceof Error ? `读取代理服务失败：${err.message}` : '读取代理服务失败')
    } finally {
      if (seq === servicesSeq.current) setLoading(false)
    }
  }, [node.id])

  useEffect(() => { void load() }, [load])

  async function confirmDelete() {
    if (!pendingDelete || deleting) return
    const target = pendingDelete
    setDeleting(true)
    try {
      await api.deleteNodeProxyService(node.id, target.id)
      setPendingDelete(null)
      notify(`已删除代理服务：${target.type} :${target.port}`)
      await load()
    } catch (err) {
      notify(err instanceof Error ? `删除失败：${err.message}` : '删除失败')
    } finally {
      setDeleting(false)
    }
  }

  return (
    <>
      <div className="node-toolbar">
        <span className="form-note">gost 代理服务 · 共 {services.length} 条（凭据由节点生成）</span>
        <div className="node-toolbar-actions">
          <button className="button button-quiet button-compact" onClick={() => void load()} disabled={loading}>
            {loading ? <Loader2 size={13} className="spin" /> : <RefreshCw size={13} />}刷新
          </button>
          <button className="button button-primary button-compact" disabled={!isAdminSession()} title={isAdminSession() ? undefined : ADMIN_ONLY_HINT} onClick={() => setCreating(true)}><Plus size={13} />新建代理</button>
        </div>
      </div>

      {note && <p className="form-note service-warn">{note}</p>}

      {!note && gostState.installed === false && (
        <p className="form-note service-warn">节点上未安装 gost —— 新建的代理只登记凭据、不会真正监听。请先到节点门户安装 gost。</p>
      )}
      {!note && gostState.installed === true && gostState.active === false && (
        <p className="form-note service-warn">节点上的 gost 服务当前未运行，代理端口不可用。新建代理会自动尝试拉起；也可在节点门户手动启动。</p>
      )}

      {services.length === 0 && !note && !loading && (
        <div className="empty-state compact"><Boxes size={18} /><strong>没有代理服务</strong><span>需要节点上已装 gost（portal 的代理服务页可安装）。</span></div>
      )}

      {services.map((service) => {
        const statePill = gostState.active === true
          ? <span className="status-pill online">运行中</span>
          : gostState.active === false
            ? <span className="status-pill offline">gost 未运行</span>
            : <span className="status-pill unknown">状态未知</span>
        const host = service.host || gostState.host
        const created = service.created_at ? new Date(service.created_at * 1000).toLocaleString() : null
        return (
          <div className="service-row" key={service.id}>
            <div className="service-identity">
              <strong>{service.type} · {host ? `${host}:` : ':'}{service.port}</strong>
              <div className="service-badges">
                {statePill}
                <span className="service-badge-muted">{service.username}:{service.password || '••••（只读账号不可见）'}</span>
                {service.note && <span className="service-badge-muted">{service.note}</span>}
                {created && <span className="service-badge-muted">创建于 {created}</span>}
              </div>
              {service.password && (service.url || (host && `${service.type}://${service.username}:${service.password}@${host}:${service.port}`)) && (
                <small className="service-error mono">{service.url || `${service.type}://${service.username}:${service.password}@${host}:${service.port}`}</small>
              )}
            </div>
            <div className="service-actions">
              <button
                className="button button-quiet button-compact"
                disabled={!service.password}
                title={service.password ? undefined : '只读账号看不到代理密码'}
                onClick={() => {
                  const text = service.format || (host ? `${host}:${service.port}:${service.username}:${service.password}` : '')
                  if (!text) { notify('暂无可复制的连接信息'); return }
                  void copyToClipboard(text).then((ok) => notify(ok ? '已复制 host:port:user:pass' : '复制失败，请手动选中复制'))
                }}
              >复制</button>
              <button className="button button-quiet button-compact" disabled={!isAdminSession()} title={isAdminSession() ? undefined : "仅管理员可删除代理服务"} onClick={() => setPendingDelete(service)}><Trash2 size={13} />删除</button>
            </div>
          </div>
        )
      })}

      {creating && (
        <CreateProxyDialog node={node} notify={notify} onClose={() => setCreating(false)} onCreated={async () => { setCreating(false); await load() }} />
      )}
      {pendingDelete && (
        <Modal title="删除代理服务" kicker="CONFIRM DELETE" onClose={() => { if (!deleting) setPendingDelete(null) }}>
          <p className="form-note">即将删除节点 <strong>{node.name}</strong> 上的代理服务 <strong>{pendingDelete.type} :{pendingDelete.port}</strong>。依赖该代理的客户端会立即断线。</p>
          <div className="modal-actions"><button className="button button-quiet" disabled={deleting} onClick={() => setPendingDelete(null)}>取消</button><button className="button button-primary" disabled={deleting} onClick={() => void confirmDelete()}>{deleting ? '删除中…' : '确认删除'}</button></div>
        </Modal>
      )}
    </>
  )
}

function CreateProxyDialog({ node, notify, onClose, onCreated }: {
  node: Hy2Node
  notify: (message: string) => void
  onClose: () => void
  onCreated: () => void | Promise<void>
}) {
  const [protocol, setProtocol] = useState<ProxyProtocol>('socks5')
  const [port, setPort] = useState('0')
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)

  async function submit() {
    const parsed = Number(port)
    if (!Number.isInteger(parsed) || parsed < 0 || parsed > 65535) {
      notify('端口需为 0（自动分配）或 1-65535')
      return
    }
    setBusy(true)
    try {
      const result = await api.createNodeProxyService(node.id, { protocol, port: parsed, note: note.trim() || undefined })
      notify(`已创建 ${result.type} 代理，端口 ${result.port}`)
      await onCreated()
    } catch (err) {
      notify(err instanceof Error ? `创建失败：${err.message}` : '创建失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal title={`新建代理服务 · ${node.name}`} kicker="CREATE PROXY SERVICE" onClose={onClose}>
      <div style={{ display: 'grid', gap: 12 }}>
        <label className="form-field"><span>协议 *</span>
          <select value={protocol} onChange={(e) => setProtocol(e.target.value as ProxyProtocol)}>
            <option value="socks5">socks5</option>
            <option value="http">http</option>
            <option value="https">https</option>
          </select>
        </label>
        <label className="form-field"><span>端口（0=自动分配）</span><input value={port} onChange={(e) => setPort(e.target.value)} inputMode="numeric" /></label>
        <label className="form-field"><span>备注</span><input value={note} onChange={(e) => setNote(e.target.value)} placeholder="可选" /></label>
        <p className="form-note">节点上的 portal 会调用 gost 落地这条代理并返回用户名/口令。若 gost 尚未安装，创建会失败。</p>
        <div className="modal-actions"><button className="button button-quiet" onClick={onClose}>取消</button><button className="button button-primary" disabled={busy} onClick={() => void submit()}>{busy ? '创建中…' : '创建代理'}</button></div>
      </div>
    </Modal>
  )
}

function NodeStatus({ node }: { node: Hy2Node }) {
  const [overview, setOverview] = useState<NodeOverview | null>(null)
  const [note, setNote] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)

  const overviewSeq = useRef(0)
  const load = useCallback(async () => {
    const seq = ++overviewSeq.current
    setLoading(true)
    try {
      const next = await api.nodeOverview(node.id)
      if (seq !== overviewSeq.current) return
      setOverview(next)
      setNote(null)
    } catch (err) {
      if (seq !== overviewSeq.current) return
      setOverview(null)
      setNote(err instanceof Error ? `读取节点概览失败：${err.message}` : '读取节点概览失败')
    } finally {
      if (seq === overviewSeq.current) setLoading(false)
    }
  }, [node.id])

  useEffect(() => { void load() }, [load])

  // Each read reports its own outcome. A failed read shows the reason in place
  // rather than a blank that could be mistaken for "nothing there".
  const failed = overview
    ? (Object.entries(overview.probes) as Array<[string, { available: boolean; error?: string }]>).filter(([, state]) => !state.available)
    : []

  const summary = overview?.summary

  return (
    <>
      <div className="node-toolbar">
        <span className="form-note">只读概览 · 来自节点 portal 的 node/meta、users/list、proxy-services/list</span>
        <div className="node-toolbar-actions">
          <button className="button button-quiet button-compact" onClick={() => void load()} disabled={loading}>
            {loading ? <Loader2 size={13} className="spin" /> : <RefreshCw size={13} />}刷新
          </button>
        </div>
      </div>

      {note && <p className="form-note service-warn"><AlertTriangle size={13} /> {note}</p>}

      {failed.length > 0 && (
        <p className="form-note service-warn">
          <AlertTriangle size={13} /> 有 {failed.length} 项读取失败：
          {failed.map(([key, state]) => `${key}（${state.error ?? '未知原因'}）`).join('；')}
        </p>
      )}

      {overview && !summary && (
        <p className="form-note service-warn">
          <AlertTriangle size={13} />
          <span>节点返回的概览结构不符合预期（缺少 summary），可能是节点上的 portal 版本过旧。用户与代理服务两个页签仍可单独查看。</span>
        </p>
      )}

      {overview && summary && (
        <>
          <div className="drawer-info-grid">
            <div><span>监听端口</span><strong>{overview.meta.listen_port || '—'}</strong></div>
            <div><span>订阅端口</span><strong>{overview.meta.subscription_port || '—'}</strong></div>
            <div><span>域名</span><strong>{overview.meta.server_name || '—'}</strong></div>
            <div><span>公网 IP</span><strong>{overview.meta.public_ip || '—'}</strong></div>
            <div><span>证书类型</span><strong>{overview.meta.cert_type || '—'}</strong></div>
            <div><span>端口跳段</span><strong>{overview.meta.hop_port_range || '—'}</strong></div>
            <div><span>混淆</span><strong>{overview.meta.has_obfs ? 'salamander' : '未启用'}</strong></div>
            <div><span>证书校验</span><strong>{overview.meta.is_insecure ? (overview.meta.has_pin ? '自签 + pin' : '自签（跳过校验）') : '公网可信'}</strong></div>
          </div>

          <div className="overview-grid">
            <div className="overview-card">
              <div className="overview-card-head">用户</div>
              <div className="overview-metric">{summary.users_count}<small>个</small></div>
              <span className="service-badge-muted">
                有效 {summary.users_active} · 不限量 {summary.users_unlimited}
              </span>
            </div>
            <div className="overview-card">
              <div className="overview-card-head">记账用量</div>
              <div className="overview-metric">{formatBytes(summary.traffic_used_bytes)}</div>
              <span className="service-badge-muted">
                {summary.traffic_limit_bytes > 0 ? `配额合计 ${formatBytes(summary.traffic_limit_bytes)}` : '无配额'}
              </span>
            </div>
            <div className="overview-card">
              <div className="overview-card-head">gost 代理</div>
              <div className="overview-metric">{summary.proxy_services_count}<small>条</small></div>
              <span className="service-badge-muted">socks5 / http / https</span>
            </div>
          </div>
        </>
      )}

      {overview && (
        <p className="form-note">{overview.unreachable}</p>
      )}
    </>
  )
}

// ---------------------------------------------------------------------------
// 节点运维 — the web-session half.
//
// Everything here goes through the portal's /{token}/ endpoints, which require
// HTTP Basic credentials (portal-access.json's username/password). That is a
// strictly larger capability set than the Bearer key: it includes actions that
// stop the service, rewrite kernel sysctls, or reboot the machine outright.
//
// Two deliberate restrictions:
//
//  - The verb set is closed. The UI cannot ask for an arbitrary endpoint; it
//    picks from a list the server also validates. Install-style verbs that run
//    remote scripts are spelled out rather than composed here.
//  - Dangerous verbs (reboot, upgrade, Reality key reset) ask for confirmation
//    before firing, and the server independently requires the
//    X-Confirm-Dangerous header — a UI bug cannot bypass it.
//
// Reads are not polled. The operator refreshes when they want a current answer;
// traffic-speed in particular is a live rate that is only meaningful at the
// moment it is asked for.

// 需要依赖组件的动作声明 requires。服务端会用同样的规则拦一次，
// 这里只是提前把「点了必然失败」的按钮显示成不可用，并说清要先装什么。
type Capability = 'warp' | 'reality' | 'awg' | 'gost'

type OpsAction = {
  verb: PortalVerb
  label: string
  hint: string
  group: string
  dangerous?: boolean
  requires?: Capability
}

const OPS_ACTIONS: OpsAction[] = [
  { verb: 'reality_toggle', label: '开关 Reality', hint: '在 xray 服务上切换启动/停止；未安装时自动生成配置', group: 'Reality', requires: 'reality' },
  { verb: 'install_xray', label: '安装 Xray', hint: 'Reality 与 VMess 共用的 Xray 内核', group: 'Reality' },
  { verb: 'vmess_enable', label: '启用 VMess', hint: '启用 WebSocket + TLS 通道，需要节点域名与有效证书', group: 'VMess', requires: 'reality' },
  { verb: 'vmess_disable', label: '停用 VMess', hint: '关闭 VMess 入站并重载 Xray，现有连接会中断，Reality 可能短暂重连', group: 'VMess', dangerous: true, requires: 'reality' },
  { verb: 'vlessenc_enable', label: '启用 VLESS Encryption', hint: 'Xray 的 VMess 替代路线（ML-KEM-768），需要节点域名与有效证书；启用后需确认公开客户端参数可读取', group: 'VLESS Encryption', requires: 'reality' },
  { verb: 'vlessenc_disable', label: '停用 VLESS Encryption', hint: '关闭 VLESS Encryption 入站并重载 Xray，现有连接会中断，Reality / VMess 可能短暂重连', group: 'VLESS Encryption', dangerous: true, requires: 'reality' },
  { verb: 'reality_reset', label: '重置 Reality 密钥', hint: '重新生成 UUID 与公私钥，已下发的链接全部失效', group: 'Reality', dangerous: true, requires: 'reality' },

  { verb: 'set_bbr', label: '设置 BBR', hint: '写入 sysctl 并尝试热加载；部分内核需重启生效', group: '网络调优' },
  { verb: 'warp_toggle', label: '开关 WARP 分流', hint: '按域名表把流量走 Cloudflare WARP 出口', group: 'WARP', requires: 'warp' },
  { verb: 'warp_reset_rules', label: '恢复 WARP 默认域名表', hint: '还原为内置的 AI 站点列表', group: 'WARP', requires: 'warp' },
  { verb: 'install_warp', label: '安装 WARP 客户端', hint: '部署 wgcf + wireproxy 及其看门狗', group: 'WARP' },

  { verb: 'install_gost', label: '安装 gost', hint: '代理服务（socks5 / http / https）的运行时', group: '组件安装' },
  { verb: 'install_awg', label: '安装 AmneziaWG', hint: '另一种 VPN 协议，可独立于 Hysteria 使用', group: '组件安装' },

  { verb: 'upgrade_core', label: '升级 Hysteria 内核', hint: '按 portal 报告的 latest 版本升级', group: '升级', dangerous: true },
  { verb: 'upgrade_portal', label: '升级 portal 自身', hint: '升级节点上的门户程序，过程中面板会短暂失联', group: '升级', dangerous: true },

  { verb: 'reboot', label: '重启服务器', hint: '整机重启，所有连接立即中断，约 1-2 分钟后恢复', group: '整机', dangerous: true },
]

const CAPABILITY_LABEL: Record<Capability, string> = {
  warp: 'Cloudflare WARP',
  reality: 'Reality（Xray）',
  awg: 'AmneziaWG',
  gost: 'gost',
}

// 概览卡归属的功能分组。选中的分段决定哪些卡留在视野里 ——
// 点「WARP」却还看到六张卡，等于没做筛选。
// 「实时速率」是全节点共享的一个指标，任何分段下都在，标 global。
const CARD_SECTION: Record<string, string> = {
  实时速率: 'global',
  版本: '升级',
  BBR: '网络调优',
  Reality: 'Reality',
  VMess: 'VMess',
  'VLESS Encryption': 'VLESS Encryption',
  WARP: 'WARP',
  AmneziaWG: '组件安装',
}

// 从七路探测结果里推断哪些组件还没装。
//
// 只在探测成功（state === 'healthy'）时才敢下结论：探测失败说明
// 「不知道」，那就不该把按钮锁上 —— 否则节点门户抖一下，整个页面
// 就变成只读，比不做判断更糟。
function missingCapabilities(probes: PortalNodeStatus['probes'] | undefined): Set<Capability> {
  const missing = new Set<Capability>()
  if (!probes) return missing
  const warp = probes.warp
  if (warp?.state === 'healthy' && warp.payload && !warp.payload.installed) missing.add('warp')
  const reality = probes.reality
  if (reality?.state === 'healthy' && reality.payload && !reality.payload.installed) missing.add('reality')
  const awg = probes.awg
  if (awg?.state === 'healthy' && awg.payload && !awg.payload.installed) missing.add('awg')
  const gost = probes.gost
  if (gost?.state === 'healthy' && gost.payload && !gost.payload.gost_installed) missing.add('gost')
  return missing
}

function NodeOps({ node, notify }: { node: Hy2Node; notify: (message: string) => void }) {
  const [status, setStatus] = useState<PortalNodeStatus | null>(null)
  const [note, setNote] = useState<string | null>(null)
  const [needBasic, setNeedBasic] = useState(false)
  const [loading, setLoading] = useState(false)
  const [busy, setBusy] = useState<string | null>(null)
  // 二级分段：'all' 或某个功能分组名。默认全部，保持原有的整页视野。
  const [section, setSection] = useState<string>('all')

  // VLESS Encryption 的「买家能不能拿到配置」不在 portal-status 里 ——
  // 那要靠 capabilities 的 delivery_error（它需要网页会话读客户端密钥）。
  // 单独取一次，失败不影响状态页本身。
  const [vlessEncDelivery, setVlessEncDelivery] = useState<{ state: 'loading' | 'unknown' | 'blocked' | 'ready'; reason: string }>({ state: 'loading', reason: '' })
  const active = useRef(true)
  const deliveryRequest = useRef(0)
  const statusRequest = useRef(0)
  useEffect(() => {
    active.current = true
    return () => { active.current = false; deliveryRequest.current += 1; statusRequest.current += 1 }
  }, [])

  const load = useCallback(async () => {
    if (!active.current) return
    const request = ++statusRequest.current
    setLoading(true)
    try {
      const result = await api.nodePortalStatus(node.id)
      if (!active.current || request !== statusRequest.current) return
      setStatus(result)
      setNote(null)
      setNeedBasic(false)
    } catch (err) {
      if (!active.current || request !== statusRequest.current) return
      setStatus(null)
      const message = err instanceof Error ? err.message : '读取节点状态失败'
      // 服务端用结构化字段区分「还没绑凭据」这种设置状态与真正的失败。
      // 早先这里靠正则嗅探错误文案，但那句话改一个词就静默失效了。
      setNeedBasic((err as ApiError | undefined)?.needBasic === true)
      setNote(message)
    } finally {
      if (active.current && request === statusRequest.current) setLoading(false)
    }
  }, [node.id])

  useEffect(() => { void load() }, [load])

  // 能力接口有 60 秒服务端缓存；手动刷新和动作完成后必须显式跳过。
  const refreshDelivery = useCallback(async (force = false) => {
    if (!active.current) return
    const request = ++deliveryRequest.current
    setVlessEncDelivery({ state: 'loading', reason: '' })
    try {
      const res = await api.nodeCapabilities(force)
      if (!active.current || request !== deliveryRequest.current) return
      const item = res.nodes.find((item) => item.node_id === node.id)
      const enc = item?.protocols?.vlessenc
      let reason = ''
      if (!item) reason = '能力响应中缺少当前节点'
      else if (!item.reachable || !item.supported) reason = item.error || '节点能力端点不可用，请检查连接或升级节点门户'
      else if (!enc || typeof enc !== 'object') reason = '节点未提供 VLESS Encryption 交付能力'
      else if (enc.state_error) reason = enc.state_error
      else if (enc.delivery_error) {
        setVlessEncDelivery({ state: 'blocked', reason: enc.delivery_error })
        return
      } else if (enc.active !== true || enc.client_encryption_present !== true) {
        reason = '尚未确认通道已启用且客户端密钥可读取'
      }
      setVlessEncDelivery({ state: reason ? 'unknown' : 'ready', reason })
    } catch (err) {
      if (!active.current || request !== deliveryRequest.current) return
      setVlessEncDelivery({ state: 'unknown', reason: err instanceof Error ? err.message : '能力查询失败' })
    }
  }, [node.id])

  useEffect(() => { void refreshDelivery() }, [refreshDelivery])

  const refresh = useCallback(async () => {
    await Promise.all([load(), refreshDelivery(true)])
  }, [load, refreshDelivery])

  const run = useCallback(async (action: OpsAction) => {
    if (action.dangerous && !await askConfirm(`确认执行「${action.label}」？\n\n${action.hint}`)) return
    if (!active.current || loadSession()?.role === "viewer") return
    setBusy(action.verb)
    try {
      if (action.verb === 'upgrade_portal') {
        const task = await api.startPortalUpdateBatch([node.id])
        if (!active.current) return
        notify(`已创建双轮更新任务 ${task.id.slice(0, 8)}；请在「批量更新门户」查看执行及核验进度`)
        return
      }
      await api.nodePortalAction(node.id, { verb: action.verb }, action.dangerous)
      if (!active.current) return
      notify(`${action.label} 已下发`)
      // A reboot or an upgrade takes the portal away for a while; refreshing
      // immediately would only produce a confusing error.
      if (action.verb !== 'reboot' && active.current) {
        await refresh()
      }
    } catch (err) {
      if (active.current) notify(err instanceof Error ? `${action.label} 失败：${err.message}` : `${action.label} 失败`)
    } finally {
      if (active.current) setBusy(null)
    }
  }, [node.id, notify, refresh])

  // set-bbr takes a version; offer all three rather than guessing one.
  const runBBR = useCallback(async (version: string) => {
    if (!active.current || loadSession()?.role === 'viewer') return
    setBusy('set_bbr')
    try {
      await api.nodePortalAction(node.id, { verb: 'set_bbr', params: { version } })
      if (!active.current) return
      notify(`BBR ${version.toUpperCase()} 已下发`)
      await load()
    } catch (err) {
      if (active.current) notify(err instanceof Error ? `设置 BBR 失败：${err.message}` : '设置 BBR 失败')
    } finally {
      if (active.current) setBusy(null)
    }
  }, [node.id, notify, load])

  if (needBasic) {
    return (
      <>
        <p className="form-note service-warn">
          <AlertTriangle size={13} /> 该节点只绑定了 API 密钥，未绑定门户访问凭据。
        </p>
        <p className="form-note">
          「节点运维」里的全部功能（版本与升级、BBR、Reality、WARP、AmneziaWG、组件安装、重启）
          都挂在门户的网页会话端点上，只有 Bearer 密钥够不到。请在节点列表里重新绑定这个节点，
          把节点上 <code>/etc/hysteria/portal-access.json</code> 的 <code>username</code> 与
          <code>password</code> 一并填入（整个文件粘一次即可），这些按钮就会解锁。
        </p>
        <p className="form-note">
          这些能力全部通过 HTTP 完成，面板不需要 SSH 登进节点。
        </p>
      </>
    )
  }

  const p = status?.probes
  const groups = Array.from(new Set(OPS_ACTIONS.map((a) => a.group)))
  // 未安装的组件对应的动作直接置灰，并说明要先装什么 ——
  // 与其让操作者点一下再收一条节点侧报错，不如一开始就说清楚。
  const missing = missingCapabilities(p)
  const blockedBy = (action: OpsAction): string | null => {
    if (action.group === 'VMess') {
      if (!p?.vmess || p.vmess.state !== 'healthy') {
        return p?.vmess?.state === 'unsupported'
          ? '节点门户尚未支持 VMess，请先升级节点门户'
          : '尚未确认 VMess 管理能力，请刷新并检查节点状态'
      }
    }
    if (action.group === 'VLESS Encryption') {
      if (!p?.vlessenc || p.vlessenc.state !== 'healthy') {
        return p?.vlessenc?.state === 'unsupported'
          ? '节点门户尚未支持 VLESS Encryption，请先升级节点门户'
          : '尚未确认 VLESS Encryption 管理能力，请刷新并检查节点状态'
      }
    }
    if (!action.requires || !missing.has(action.requires)) return null
    const label = CAPABILITY_LABEL[action.requires]
    return `该节点未安装 ${label}，请先执行「安装${action.requires === 'reality' ? ' Xray' : ' ' + label}」`
  }
  // 概览卡是否留在当前分段视野里：'global'（实时速率）永远在，
  // 其余按 CARD_SECTION 归属；未登记的卡默认保留，宁可多看不漏看。
  const showCard = (title: string): boolean => {
    if (section === 'all') return true
    const owner = CARD_SECTION[title]
    if (!owner || owner === 'global') return true
    return owner === section
  }
  // 选中的分段决定渲染哪些动作组；'all' 时全给。
  const visibleGroups = section === 'all' ? groups : groups.filter((g) => g === section)

  return (
    <>
      <div className="node-toolbar">
        <span className="form-note">节点运维 · 走门户网页会话（HTTP Basic），无需 SSH</span>
        <div className="node-toolbar-actions">
          <button className="button button-quiet button-compact" onClick={() => void refresh()} disabled={loading || vlessEncDelivery.state === 'loading' || busy !== null}>
            {loading || vlessEncDelivery.state === 'loading' ? <Loader2 size={13} className="spin" /> : <RefreshCw size={13} />}刷新
          </button>
        </div>
      </div>

      {/* 二级分段导航：一级页签只有 4 个，功能全挤在运维页里要往下滚才看得见。
          这里把每个功能分组提成一眼可选的入口，同时筛掉不相干的概览卡，
          让「点 WARP 就只看 WARP 的状态和按钮」成立。 */}
      <div className="ops-segment" role="tablist" aria-label="节点运维功能分组">
        <button
          role="tab"
          aria-selected={section === 'all'}
          className={`ops-segment-item${section === 'all' ? ' is-active' : ''}`}
          onClick={() => setSection('all')}
        >全部</button>
        {groups.map((group) => (
          <button
            key={group}
            role="tab"
            aria-selected={section === group}
            className={`ops-segment-item${section === group ? ' is-active' : ''}`}
            onClick={() => setSection(group)}
          >{group}</button>
        ))}
      </div>

      {note && <p className="form-note service-warn"><AlertTriangle size={13} /> {note}</p>}

      {p && (
        <div className="overview-grid">
          {showCard('实时速率') && (
          <div className="overview-card">
            <div className="overview-card-head"><Zap size={12} /> 实时速率</div>
            {p.speed.state === 'healthy' && p.speed.payload ? (
              <>
                <div className="overview-metric">
                  {formatRate(p.speed.payload.node_rx)}<small>下行</small>
                </div>
                <span className="service-badge-muted">
                  上行 {formatRate(p.speed.payload.node_tx)} · {Object.keys(p.speed.payload.users ?? {}).length} 个用户在册
                </span>
              </>
            ) : (
              <ProbeError state={p.speed} />
            )}
          </div>
          )}

          {showCard('版本') && (
          <div className="overview-card">
            <div className="overview-card-head">版本</div>
            {p.version.state === 'healthy' && p.version.payload ? (
              <>
                <div className="overview-metric">{p.version.payload.core_current || '—'}</div>
                <span className="service-badge-muted">
                  {p.version.payload.core_has_update
                    ? `内核可升级到 ${p.version.payload.core_latest}`
                    : '内核已是最新'}
                  {p.version.payload.portal_has_update ? ' · 门户可升级' : ''}
                </span>
              </>
            ) : (
              <ProbeError state={p.version} />
            )}
          </div>
          )}

          {showCard('BBR') && (
          <div className="overview-card">
            <div className="overview-card-head">BBR</div>
            {p.bbr.state === 'healthy' && p.bbr.payload ? (
              <>
                <div className="overview-metric">{p.bbr.payload.current || '—'}</div>
                <span className="service-badge-muted">
                  {p.bbr.payload.qdisc} · {p.bbr.payload.kernel}
                  {p.bbr.payload.need_reboot ? ' · 待重启生效' : ''}
                </span>
              </>
            ) : (
              <ProbeError state={p.bbr} />
            )}
          </div>
          )}

          {showCard('Reality') && (
          <div className="overview-card">
            <div className="overview-card-head">Reality</div>
            {p.reality.state === 'healthy' && p.reality.payload ? (
              <>
                <div className="overview-metric">
                  {p.reality.payload.active ? '运行中' : (p.reality.payload.installed ? '已停止' : '未安装')}
                </div>
                <span className="service-badge-muted">
                  {p.reality.payload.config?.sni
                    ? `SNI ${p.reality.payload.config.sni}:${p.reality.payload.config?.port ?? '—'}`
                    : '无配置'}
                </span>
              </>
            ) : (
              <ProbeError state={p.reality} />
            )}
          </div>
          )}

          {showCard('VMess') && (
          <div className="overview-card">
            <div className="overview-card-head">VMess · WebSocket · TLS</div>
            {p.vmess?.state === 'healthy' && p.vmess.payload ? (
              <>
                <div className="overview-metric">{p.vmess.payload.active ? '已启用' : '未启用'}</div>
                <span className="service-badge-muted">
                  {p.vmess.payload.config?.domain
                    ? `${p.vmess.payload.config.domain}:${p.vmess.payload.config.port ?? '—'}`
                    : '启用时需要域名与有效证书'}
                </span>
              </>
            ) : p.vmess ? <ProbeError state={p.vmess} /> : (
              <span className="service-badge-muted">尚未提供 VMess 状态，请刷新或升级节点门户</span>
            )}
          </div>
          )}

          {showCard('VLESS Encryption') && (
          <div className="overview-card">
            <div className="overview-card-head">VLESS Encryption · ML-KEM-768</div>
            {p.vlessenc?.state === 'healthy' && p.vlessenc.payload ? (
              <>
                <div className="overview-metric">{p.vlessenc.payload.active ? '已启用' : '未启用'}</div>
                <span className="service-badge-muted">
                  {p.vlessenc.payload.config?.domain
                    ? `${p.vlessenc.payload.config.domain}:${p.vlessenc.payload.config.port ?? '—'}`
                    : '启用时需要域名与有效证书'}
                </span>
                {/* 状态原因尾巴：通道「已启用」不等于「买家拿得到配置」。
                    客户端密钥不在 capabilities 里，要靠网页会话读；
                    读不到时订阅里不会出现这条通道 —— 必须在这里说清楚，
                    否则操作者会以为买家能用（铁律：不可用的操作/能力要给出原因）。 */}
                {p.vlessenc.payload.active && vlessEncDelivery.state !== 'ready' && (
                  <span className="service-badge-muted" style={{ color: 'var(--amber)' }} title={vlessEncDelivery.reason}>
                    {vlessEncDelivery.state === 'loading' ? '正在确认买家交付能力…' : vlessEncDelivery.state === 'blocked' ? '买家不可交付' : '买家交付能力未确认'}
                    {vlessEncDelivery.reason ? `：${vlessEncDelivery.reason}` : ''}
                    {vlessEncDelivery.state === 'unknown' ? '；请点击刷新重试' : ''}
                  </span>
                )}
              </>
            ) : p.vlessenc ? <ProbeError state={p.vlessenc} /> : (
              <span className="service-badge-muted">尚未提供 VLESS Encryption 状态，请刷新或升级节点门户</span>
            )}
          </div>
          )}

          {showCard('WARP') && (
          <div className="overview-card">
            <div className="overview-card-head">WARP</div>
            {p.warp.state === 'healthy' && p.warp.payload ? (
              <>
                <div className="overview-metric">
                  {p.warp.payload.connected ? '已连接' : (p.warp.payload.installed ? '未连接' : '未安装')}
                </div>
                <span className="service-badge-muted">
                  {p.warp.payload.ip || `出口分流 ${(p.warp.payload.rules ?? []).length} 条域名`}
                </span>
              </>
            ) : (
              <ProbeError state={p.warp} />
            )}
          </div>
          )}

          {showCard('AmneziaWG') && (
          <div className="overview-card">
            <div className="overview-card-head">AmneziaWG</div>
            {p.awg.state === 'healthy' && p.awg.payload ? (
              <>
                <div className="overview-metric">
                  {p.awg.payload.active ? '运行中' : (p.awg.payload.installed ? '已停止' : '未安装')}
                </div>
                <span className="service-badge-muted">
                  {p.awg.payload.peer_count > 0 ? `${p.awg.payload.peer_count} 个客户端` : '无客户端'}
                </span>
              </>
            ) : (
              <ProbeError state={p.awg} />
            )}
          </div>
          )}
        </div>
      )}

      {missing.size > 0 && (
        <p className="form-note service-warn">
          <Ban size={13} /> 该节点未安装 {Array.from(missing).map((c) => CAPABILITY_LABEL[c]).join('、')}
          ，相关按钮已置灰（服务端也会拦）。先点右上角对应的「安装」按钮即可解锁。
        </p>
      )}

      {visibleGroups.map((group) => (
        <div key={group} className="ops-group">
          <div className="section-kicker">{group}</div>
          <div className="ops-actions">
            {OPS_ACTIONS.filter((a) => a.group === group && a.verb !== 'set_bbr').map((action) => {
              const blocked = blockedBy(action)
              const adminOnly = portalActionAdminOnly(action.verb, action.dangerous) && !isAdminSession()
              return (
                <button
                  key={action.verb}
                  className={`button button-compact ${action.dangerous ? 'button-danger' : 'button-quiet'}`}
                  title={adminOnly ? ADMIN_ONLY_HINT : (blocked ?? action.hint)}
                  disabled={loadSession()?.role === "viewer" || adminOnly || busy !== null || blocked !== null}
                  onClick={() => void run(action)}
                >
                  {busy === action.verb
                    ? <Loader2 size={13} className="spin" />
                    : blocked ? <Ban size={13} />
                    : action.dangerous ? <AlertTriangle size={13} /> : <Shield size={13} />}
                  {action.label}
                </button>
              )
            })}
            {group === '网络调优' && ['v1', 'v2', 'v3'].map((version) => (
              <button
                key={version}
                className="button button-compact button-quiet"
                title={`切换拥塞控制算法到 BBR ${version.toUpperCase()}`}
                disabled={loadSession()?.role === "viewer" || busy !== null}
                onClick={() => void runBBR(version)}
              >
                {busy === 'set_bbr' ? <Loader2 size={13} className="spin" /> : <Zap size={13} />}
                BBR {version.toUpperCase()}
              </button>
            ))}
            {group === '整机' && (
              <span className="form-note" style={{ alignSelf: 'center' }}>
                <Power size={12} /> 重启走 HTTP，不需要 SSH
              </span>
            )}
          </div>
        </div>
      ))}
    </>
  )
}

function ProbeError({ state }: { state: { state: string; error?: string } }) {
  if (state.state === 'healthy') return <span className="service-badge-muted">—</span>
  return (
    <span className="service-badge-muted" title={state.error ?? ''}>
      {state.error ? state.error.slice(0, 60) : '不可读'}
    </span>
  )
}

// formatRate renders bytes-per-second, which is what portal's traffic-speed
// reports. Distinct from formatBytes because a rate of zero should read as idle
// rather than as a missing value.
function formatRate(bytesPerSecond: number): string {
  if (!bytesPerSecond || bytesPerSecond <= 0) return '0 B/s'
  return `${formatBytes(Math.round(bytesPerSecond))}/s`
}
