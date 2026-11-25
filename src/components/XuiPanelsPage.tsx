import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  Activity, AlertTriangle, ChevronRight, Layers, Plus, RefreshCw, Search, Sparkles, Trash2,
  Users, Wifi, WifiOff, X,
} from 'lucide-react'
import {
  api, loadSession, type XuiClientStatus, type XuiInbound, type XuiPanel, type XuiPreview,
  isAdminSession,
} from '../lib/api'
import { formatBytes } from '../lib/format'
import { hasOpenModal, Modal } from './Modal'
import { askConfirm } from './ConfirmHost'

// 3X-UI 面板管理页。
//
// 与「HY2 节点」页的关键差别：那边的节点是自建、靠 portal 凭据管理；
// 这边的面板是**外部**的第三方服务，我们只持有一对登录凭据。
// 所以本页的第一要务是把「这台面板现在能不能连上、里面的东西长什么样」
// 讲清楚，而不是像节点页那样堆大量运维动作。

const statusText: Record<string, string> = { ok: '已连接', error: '连接失败', unknown: '未探测' }
const statusClass: Record<string, string> = { ok: 'online', error: 'offline', unknown: 'offline' }

// GeoTag 展示自动识别出来的地区与商家。
//
// 加这个标签不是为了好看，是为了**让「这是谁定的」可见**：
// 运维者需要一眼看出「Tokyo/AWS」是系统猜的还是自己填的 ——
// 因为前者会在下次识别时被刷新，后者永远不会被动。
// 手动填过的用实心样式、自动识别的带虚线边框，hover 各自说明来源。
function GeoTag({ region, provider, source }: { region: string; provider: string; source: string }) {
  const text = region && provider ? `${region} · ${provider}` : (region || provider)
  if (!text) return null
  const manual = source === 'manual'
  return (
    <span
      className={`geo-tag ${manual ? 'geo-tag-manual' : 'geo-tag-auto'}`}
      title={manual ? '人工填写，自动识别不会覆盖' : '由 IP 自动识别，可手工修改'}
    >
      {text}
    </span>
  )
}

// 3X-UI 的 expiryTime 是毫秒时间戳；0 表示永不过期。
// 2026-11-06：负数不是「永不过期」—— 3X-UI 用 -(天数 × 86400000) 表示「首次使用后 N 天到期」。
// 原来一律显示「永不过期」，这类客户端既看不出有期限，用起来以后也不会出现「剩 N 天」预警。
function formatExpiry(ms: number): string {
  if (!ms) return '永不过期'
  if (ms < 0) return `首次使用后 ${Math.round(-ms / 86_400_000)} 天到期`
  const d = new Date(ms)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleDateString('zh-CN', { year: 'numeric', month: '2-digit', day: '2-digit' })
}

function formatCheckedAt(iso: string | null | undefined): string {
  if (!iso) return '尚未探测'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '探测时间未知' : `探测于 ${d.toLocaleString('zh-CN', { hour12: false })}`
}

function expiryRemaining(ms: number): { text: string; warn: boolean } {
  if (!ms || ms < 0) return { text: '', warn: false }
  const days = Math.ceil((ms - Date.now()) / 86_400_000)
  if (days < 0) return { text: `已过期 ${-days} 天`, warn: true }
  if (days <= 7) return { text: `剩 ${days} 天`, warn: true }
  return { text: `剩 ${days} 天`, warn: false }
}

// 客户端用量占比。total 为 0 表示不限量。
function usageRatio(used: number, total: number): number | null {
  if (!total || total <= 0) return null
  return Math.min(100, Math.round((used / total) * 100))
}

type Draft = { name: string; base_url: string; username: string; password: string }
const emptyDraft: Draft = { name: '', base_url: '', username: '', password: '' }

export function XuiPanelsPage({ notify }: { notify: (message: string) => void }) {
  const [panels, setPanels] = useState<XuiPanel[]>([])
  const [loading, setLoading] = useState(true)
  const [selected, setSelected] = useState<XuiPanel | null>(null)
  const [adding, setAdding] = useState(false)
  const [draft, setDraft] = useState<Draft>(emptyDraft)
  const [preview, setPreview] = useState<XuiPreview | null>(null)
  const [busy, setBusy] = useState(false)
  const [refreshing, setRefreshing] = useState('')
  const [pendingDelete, setPendingDelete] = useState<XuiPanel | null>(null)
  // 2026-11-06：列表读取失败单独标记，原来失败时显示「还没有登记 3X-UI 面板」。
  const [loadError, setLoadError] = useState('')
  const viewer = loadSession()?.role === 'viewer'
  const mountedRef = useRef(true)
  const loadSeq = useRef(0)

  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      loadSeq.current += 1
    }
  }, [])

  const load = useCallback(async (): Promise<boolean> => {
    const seq = ++loadSeq.current
    try {
      const list = await api.xuiPanels()
      if (!mountedRef.current || seq !== loadSeq.current) return false
      setPanels(list ?? [])
      setLoadError('')
      return true
    } catch (err) {
      if (!mountedRef.current || seq !== loadSeq.current) return false
      const message = err instanceof Error ? err.message : '读取失败'
      setLoadError(message)
      notify(`读取 3X-UI 面板失败：${message}`)
      return false
    } finally {
      if (mountedRef.current && seq === loadSeq.current) setLoading(false)
    }
  }, [notify])

  useEffect(() => { void load() }, [load])

  function openAdd() {
    setDraft(emptyDraft)
    setPreview(null)
    setAdding(true)
  }

  async function runPreview() {
    if (busy) return
    if (!draft.base_url.trim() || !draft.username.trim() || !draft.password) {
      notify('请填写面板地址、用户名与密码')
      return
    }
    setBusy(true)
    try {
      const result = await api.xuiPreviewPanel({
        name: draft.name.trim() || draft.base_url.trim(),
        base_url: draft.base_url.trim(),
        username: draft.username.trim(),
        password: draft.password,
      })
      if (!mountedRef.current) return
      setPreview(result)
      notify(`连通成功：${result.inbounds_count} 个入站 / ${result.clients_count} 个客户端`)
    } catch (err) {
      if (!mountedRef.current) return
      setPreview(null)
      notify(err instanceof Error ? `连接失败：${err.message}` : '连接失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  async function submitAdd() {
    if (busy) return
    // 名称**允许留空**：服务端会按面板地址里的公网 IP 自动识别地区与商家
    // 来命名。但留空且识别不出来时，后端会因为没有名字而存成空串 ——
    // 所以这里不能不做任何检查就放行。取一个"至少能看"的兜底：
    // 没填名字就用地址本身，与后端 probeXUIPanel 的占位逻辑一致。
    if (!draft.name.trim() && !draft.base_url.trim()) {
      notify('请填写面板地址（名称可留空，会自动识别）')
      return
    }
    if (!preview) {
      notify('请先点「测试连接」确认凭据可用')
      return
    }
    setBusy(true)
    try {
      await api.xuiCreatePanel({
        name: draft.name.trim(),
        base_url: draft.base_url.trim(),
        username: draft.username.trim(),
        password: draft.password,
      })
      if (!mountedRef.current) return
      notify(`已添加 3X-UI 面板：${draft.name.trim() || draft.base_url.trim()}`)
      setAdding(false)
      setDraft(emptyDraft)
      setPreview(null)
      await load()
    } catch (err) {
      if (mountedRef.current) notify(err instanceof Error ? `添加失败：${err.message}` : '添加失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  async function confirmDelete() {
    if (!pendingDelete || busy) return
    const target = pendingDelete
    setPendingDelete(null)
    setBusy(true)
    try {
      await api.xuiDeletePanel(target.id)
      if (!mountedRef.current) return
      notify(`已删除 3X-UI 面板：${target.name}`)
      if (selected?.id === target.id) setSelected(null)
      await load()
    } catch (err) {
      if (mountedRef.current) notify(err instanceof Error ? `删除失败：${err.message}` : '删除失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  async function refresh(panel: XuiPanel) {
    if (viewer || refreshing) return
    setRefreshing(panel.id)
    try {
      const result = await api.xuiRefreshPanel(panel.id)
      if (!mountedRef.current) return
      notify(`已刷新 ${panel.name}：${result.inbounds_count} 个入站 / ${result.online_count === null ? '在线人数未取到' : `${result.online_count} 人在线`}`)
      await load()
    } catch (err) {
      if (!mountedRef.current) return
      notify(err instanceof Error ? `刷新失败：${err.message}` : '刷新失败')
      await load()
    } finally {
      if (mountedRef.current) setRefreshing('')
    }
  }

  const totalInbounds = panels.reduce((sum, panel) => sum + panel.inbounds_count, 0)
  const totalClients = panels.reduce((sum, panel) => sum + panel.clients_count, 0)
  const onlineToday = panels.reduce((sum, panel) => sum + panel.online_count, 0)
  const healthy = panels.filter((panel) => panel.last_status === 'ok').length

  return (
    <>
      <div className="page-heading">
        <div>
          <div className="eyebrow"><span className="eyebrow-line" />XRAY / 3X-UI</div>
          <h1>3X-UI 面板<span className="heading-period">.</span></h1>
          <p className="page-subtitle">登记外部 3X-UI，用其管理员凭据直连 API 做统一管理</p>
        </div>
        <div className="heading-actions">
          <span className="updated-label"><span className="live-dot" />{panels.length} 个面板</span>
          <button className="button button-quiet" onClick={async () => { if (await load()) notify('已刷新') }}><RefreshCw size={15} />刷新</button>
          {/* 2026-11-04：添加面板是 requireAdmin 接口；原来运维账号能填完表单、测试连接通过，点「确认添加」才收到 403。 */}
          <button className="button button-primary" onClick={openAdd} disabled={!isAdminSession()} title={isAdminSession() ? undefined : '仅管理员可添加面板'}><Plus size={16} />添加面板</button>
        </div>
      </div>

      <section className="metrics-grid" aria-label="3X-UI 概览">
        <MetricCard label="面板可用" value={`${healthy}`} unit={`/ ${panels.length}`} icon={<Activity size={17} />} tone={healthy === panels.length && panels.length > 0 ? 'green' : 'amber'} foot={<>最近一次探测结果</>} />
        <MetricCard label="入站总数" value={`${totalInbounds}`} unit="个" icon={<Layers size={17} />} tone="neutral" foot={<>全部面板汇总</>} />
        <MetricCard label="客户端" value={`${totalClients}`} unit="个" icon={<Users size={17} />} tone="neutral" foot={<><span className="metric-up">{onlineToday} 在线</span> 当前连接</>} />
        <MetricCard label="凭据安全" value="加密" unit="存储" icon={<Activity size={17} />} tone="neutral" foot={<>密码 AES-256-GCM 落库，接口不回显</>} />
      </section>

      <section className="panel" style={{ padding: '15px 16px 0', animation: 'rise .6s .12s var(--ease) both' }}>
        <div className="section-head">
          <div>
            <div className="section-kicker">PANELS <span>·</span> {panels.length.toString().padStart(2, '0')} REGISTERED</div>
            <h2>已登记面板</h2>
          </div>
        </div>

        {loading ? (
          <div className="empty-state"><RefreshCw size={20} /><strong>加载中…</strong><span>正在读取已登记的 3X-UI 面板。</span></div>
        ) : loadError && panels.length === 0 ? (
          <div className="empty-state">
            <Layers size={20} />
            <strong>读取 3X-UI 面板失败</strong>
            <span>{loadError}</span>
            <button onClick={() => { void load() }}>重试</button>
          </div>
        ) : panels.length === 0 ? (
          <div className="empty-state">
            <Layers size={20} />
            <strong>还没有登记 3X-UI 面板</strong>
            <span>点右上角「添加面板」，填入 3X-UI 的地址与管理员账号即可接入。</span>
            <button onClick={openAdd} disabled={!isAdminSession()} title={isAdminSession() ? undefined : '仅管理员可添加面板'}>添加第一个面板</button>
          </div>
        ) : (
          <div className="server-table-wrap" style={{ marginTop: 12 }}>
            <table className="server-table">
              <thead>
                <tr><th>面板</th><th>状态</th><th>入站</th><th>客户端</th><th>在线</th><th></th></tr>
              </thead>
              <tbody>
                {panels.map((panel) => (
                  <tr key={panel.id} onClick={() => setSelected(panel)} tabIndex={0} onKeyDown={(event) => event.key === 'Enter' && setSelected(panel)}>
                    <td>
                      <div className="host-cell">
                        <div className={`host-icon ${statusClass[panel.last_status] ?? 'offline'}`}><Layers size={16} /></div>
                        <div>
                          <strong>{panel.name}</strong>
                          <span>
                            {(panel.region || panel.provider) && (
                              <>
                                <GeoTag region={panel.region} provider={panel.provider} source={panel.region_source} />
                                {' '}<i />
                              </>
                            )}
                            {panel.base_url} <i /> {panel.username}
                          </span>
                        </div>
                      </div>
                    </td>
                    <td>
                      <span className={`status-badge ${statusClass[panel.last_status] ?? 'offline'}`} title={panel.last_error || undefined}>
                        <i />{statusText[panel.last_status] ?? panel.last_status}
                      </span>
                    </td>
                    <td><span className="traffic-value">{panel.inbounds_count}</span></td>
                    <td><span className="traffic-value">{panel.clients_count}</span></td>
                    <td><span className="traffic-value">{panel.online_count > 0 ? `● ${panel.online_count}` : '—'}</span></td>
                    <td>
                      <div style={{ display: 'flex', gap: 6 }}>
                        <button
                          className="icon-button compact"
                          aria-label={`刷新 ${panel.name}`}
                          disabled={viewer || refreshing === panel.id}
                          title={viewer ? '只读账号不能主动刷新外部面板状态' : '重新连接并刷新面板统计'}
                          onClick={(event) => { event.stopPropagation(); void refresh(panel) }}
                        >
                          <RefreshCw size={15} className={refreshing === panel.id ? 'spin' : undefined} />
                        </button>
                        <button className="row-more" aria-label={`查看 ${panel.name}`} onClick={(event) => { event.stopPropagation(); setSelected(panel) }}>
                          <ChevronRight size={17} />
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <div className="table-foot">
          <span>共 <strong>{panels.length}</strong> 个面板</span>
          <button onClick={load}><RefreshCw size={13} />重新读取</button>
        </div>
      </section>

      {adding && (
        <Modal title="添加 3X-UI 面板" kicker="REGISTER PANEL" onClose={() => setAdding(false)}>
          <p className="form-note">
            填入 3X-UI 面板的访问地址与管理员账号。地址可用 IP:端口，也可用反代域名（<code>https://面板域名/子路径</code>）。
            凭据仅用于登录其 API，AES-256-GCM 加密落库、任何接口都不回显。
          </p>
          <p className="form-note form-note-hint">
            <Sparkles size={13} />
            <span>
              地址里填的是<strong>公网 IP</strong> 时，名称留空即可自动识别地区与商家并命名（如 <code>Tokyo-AWS</code>）。
              用域名的话识别不了 —— 反解域名得到的是 CDN 节点、不是这台机器真实所在，所以那种情况请自己填名字。
            </span>
          </p>
          <label className="login-field" style={{ marginTop: 12 }}>
            <span>面板名称<em className="field-optional">留空＝自动识别</em></span>
            <input value={draft.name} onChange={(event) => setDraft({ ...draft, name: event.target.value })} placeholder="留空自动识别，或自己起名（如 香港-3XUI-01）" disabled={busy} />
          </label>
          <label className="login-field" style={{ marginTop: 10 }}>
            <span>面板地址</span>
            <input value={draft.base_url} onChange={(event) => { setDraft({ ...draft, base_url: event.target.value }); setPreview(null) }} placeholder="https://1.2.3.4:2053 或 https://xui.example.com" spellCheck={false} disabled={busy} />
          </label>
          <label className="login-field" style={{ marginTop: 10 }}>
            <span>登录用户名</span>
            <input value={draft.username} onChange={(event) => { setDraft({ ...draft, username: event.target.value }); setPreview(null) }} placeholder="admin" autoComplete="off" disabled={busy} />
          </label>
          <label className="login-field" style={{ marginTop: 10 }}>
            <span>登录密码</span>
            <input type="password" value={draft.password} onChange={(event) => { setDraft({ ...draft, password: event.target.value }); setPreview(null) }} autoComplete="off" disabled={busy} />
          </label>
          {preview && (
            <div className="drawer-notice" style={{ marginTop: 14 }}>
              <Activity size={17} />
              <span>
                <strong>连接成功</strong>
                <small>{preview.inbounds_count} 个入站 · {preview.clients_count} 个客户端 · {preview.online_count} 人在线（{preview.base_url}）</small>
              </span>
            </div>
          )}
          <div className="modal-actions">
            <button className="button button-quiet" onClick={() => setAdding(false)} disabled={busy}>取消</button>
            <button className="button button-quiet" onClick={runPreview} disabled={busy}>测试连接</button>
            <button className="button button-primary" onClick={submitAdd} disabled={busy || !preview || !isAdminSession()}>确认添加</button>
          </div>
        </Modal>
      )}

      {selected && (
        <XuiPanelDrawer
          // 2026-11-06：用列表里的最新一份。原来传的是打开那一刻的快照：抽屉里刷新成功后，
          // 头部仍显示旧的「连接失败」和旧的探测时间。
          panel={panels.find((p) => p.id === selected.id) ?? selected}
          notify={notify}
          onClose={() => setSelected(null)}
          onRequestDelete={() => setPendingDelete(selected)}
          onChanged={async () => { await load() }}
        />
      )}

      {pendingDelete && (
        <Modal title="删除 3X-UI 面板" kicker="CONFIRM DELETE" onClose={() => setPendingDelete(null)}>
          <p className="form-note">
            即将从控制台移除 <strong>{pendingDelete.name}</strong>（{pendingDelete.base_url}）。
            <strong>这只是解除登记</strong> —— 3X-UI 面板本身与它的节点、客户端都不受任何影响，
            随时可以重新添加。已保存的加密凭据会一并删除。
          </p>
          <div className="modal-actions">
            <button className="button button-quiet" onClick={() => setPendingDelete(null)}>取消</button>
            <button className="button button-danger" disabled={busy} onClick={confirmDelete}>{busy ? '移除中…' : '确认移除登记'}</button>
          </div>
        </Modal>
      )}
    </>
  )
}

// ---- 抽屉：单个面板的入站与客户端 ----

function XuiPanelDrawer({ panel, notify, onClose, onRequestDelete, onChanged }: {
  panel: XuiPanel
  notify: (message: string) => void
  onClose: () => void
  onRequestDelete: () => void
  onChanged: () => Promise<void> | void
}) {
  const viewer = loadSession()?.role === 'viewer'
  const [inbounds, setInbounds] = useState<XuiInbound[]>([])
  const [clients, setClients] = useState<XuiClientStatus[]>([])
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState('')
  const [query, setQuery] = useState('')
  const [activeInbound, setActiveInbound] = useState<number | 'all'>('all')
  const [creating, setCreating] = useState(false)
  const [pendingClientDelete, setPendingClientDelete] = useState<XuiClientStatus | null>(null)
  const [busy, setBusy] = useState(false)
  const [onlinesError, setOnlinesError] = useState('')
  const loadSeq = useRef(0)
  const mountedRef = useRef(true)

  const load = useCallback(async () => {
    const seq = ++loadSeq.current
    setLoading(true)
    try {
      const result = await api.xuiInbounds(panel.id)
      if (!mountedRef.current || seq !== loadSeq.current) return
      setInbounds(result.inbounds ?? [])
      setClients(result.clients ?? [])
      setOnlinesError(result.onlines_error ?? '')
      setLoadError('')
    } catch (err) {
      if (!mountedRef.current || seq !== loadSeq.current) return
      setLoadError(err instanceof Error ? err.message : '读取失败')
      setInbounds([])
      setClients([])
    } finally {
      if (mountedRef.current && seq === loadSeq.current) setLoading(false)
    }
  }, [panel.id])

  useEffect(() => {
    mountedRef.current = true
    void load()
    return () => {
      mountedRef.current = false
      loadSeq.current += 1
    }
  }, [load])

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      // 子弹窗或全局确认弹窗打开时交由 Modal 栈处理 Esc，不把整个抽屉关掉。
      if (event.key === 'Escape' && !event.defaultPrevented && !creating && !pendingClientDelete && !hasOpenModal()) {
        onClose()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose, creating, pendingClientDelete])

  const inboundClientCounts = useMemo(() => {
    const map = new Map<number, number>()
    for (const client of clients) {
      map.set(client.inbound_id, (map.get(client.inbound_id) ?? 0) + 1)
    }
    return map
  }, [clients])

  const filtered = useMemo(() => clients.filter((client) => {
    const matchesQuery = !query || `${client.email} ${client.inbound_name}`.toLowerCase().includes(query.toLowerCase())
    const matchesInbound = activeInbound === 'all' || client.inbound_id === activeInbound
    return matchesQuery && matchesInbound
  }), [clients, query, activeInbound])

  const onlineCount = clients.filter((client) => client.online).length

  async function resetTraffic(client: XuiClientStatus) {
    if (busy) return
    // 2026-11-06：重置流量不可撤销（已用量清零、配额重新放开），原来一点即执行，误点就把这个客户的用量记录抹掉了。
    if (!await askConfirm(`重置 ${client.email} 的已用流量？

已用量会清零，配额重新放开，无法撤销。`, { confirmText: '重置', danger: true })) return
    if (!mountedRef.current) return
    setBusy(true)
    try {
      await api.xuiResetClientTraffic(panel.id, client.inbound_id, client.email)
      if (!mountedRef.current) return
      notify(`已重置流量：${client.email}`)
      await load()
    } catch (err) {
      if (mountedRef.current) notify(err instanceof Error ? `重置失败：${err.message}` : '重置失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  async function confirmDeleteClient() {
    if (!pendingClientDelete || busy) return
    const target = pendingClientDelete
    setPendingClientDelete(null)
    setBusy(true)
    try {
      await api.xuiDeleteClient(panel.id, target.inbound_id, target.client_id)
      if (!mountedRef.current) return
      notify(`已删除客户端：${target.email}`)
      await load()
      await onChanged()
    } catch (err) {
      if (mountedRef.current) notify(err instanceof Error ? `删除失败：${err.message}` : '删除失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  return (
    <div className="modal-backdrop" onMouseDown={(event) => event.target === event.currentTarget && onClose()}>
      <section className="detail-drawer" role="dialog" aria-modal="true" aria-labelledby="xui-drawer-title">
        <div className="drawer-head">
          <div>
            <div className="section-kicker">3X-UI PANEL <span>·</span> {panel.id.slice(0, 8).toUpperCase()}</div>
            <h2 id="xui-drawer-title">{panel.name}</h2>
          </div>
          <button className="icon-button" aria-label="关闭详情" onClick={onClose}><X size={18} /></button>
        </div>

        <div className="drawer-status">
          <span className={`status-badge ${statusClass[panel.last_status] ?? 'offline'}`}><i />{statusText[panel.last_status] ?? panel.last_status}</span>
          <span>{formatCheckedAt(panel.last_checked_at)}</span>
        </div>

        <div className="drawer-address">
          <span>面板地址</span>
          <strong style={{ fontSize: 'var(--fs-xs)', wordBreak: 'break-all' }}>{panel.base_url}</strong>
        </div>

        {panel.last_status === 'error' && panel.last_error && (
          <div className="drawer-notice warn" style={{ marginTop: 12 }}>
            <AlertTriangle size={17} />
            <span><strong>上次连接失败</strong><small>{panel.last_error}</small></span>
          </div>
        )}

        <div className="drawer-info-grid">
          <div><span>登录用户</span><strong>{panel.username}</strong></div>
          <div><span>凭据状态</span><strong>已加密保存</strong></div>
          <div><span>入站数量</span><strong>{inbounds.length || panel.inbounds_count}</strong></div>
          <div><span>客户端</span><strong>{clients.length || panel.clients_count}（{!loadError && onlinesError ? '在线未知' : `${onlineCount} 在线`}）</strong></div>
        </div>

        <div className="drawer-actions" style={{ marginBottom: 14 }}>
          <button className="button button-quiet" onClick={() => void load()} disabled={loading}><RefreshCw size={15} />{loading ? '读取中…' : '重新读取'}</button>
          <button className="button button-primary" onClick={() => setCreating(true)} disabled={viewer || inbounds.length === 0} title={viewer ? '只读账号不能新增客户端' : undefined}><Plus size={15} />新增客户端</button>
        </div>

        {loadError && (
          <div className="drawer-notice warn" style={{ marginBottom: 14 }}>
            <AlertTriangle size={17} />
            <span><strong>无法读取面板数据</strong><small>{loadError}</small></span>
          </div>
        )}
        {!loadError && onlinesError && (
          <div className="drawer-notice warn" style={{ marginBottom: 14 }}>
            <AlertTriangle size={17} />
            <span><strong>在线状态未知</strong><small>{onlinesError}；下方客户端都显示为未在线，并不代表真的离线。</small></span>
          </div>
        )}

        {inbounds.length > 0 && (
          <div style={{ marginBottom: 12 }}>
            <div className="section-kicker" style={{ marginBottom: 7 }}>INBOUNDS <span>·</span> 按入站筛选</div>
            <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
              <button
                className={`button ${activeInbound === 'all' ? 'button-primary' : 'button-quiet'}`}
                style={{ height: 30, padding: '0 9px', fontSize: 'var(--fs-2xs)' }}
                onClick={() => setActiveInbound('all')}
              >全部 {clients.length}</button>
              {inbounds.map((inbound) => {
                const count = inboundClientCounts.get(inbound.id) ?? 0
                return (
                  <button
                    key={inbound.id}
                    className={`button ${activeInbound === inbound.id ? 'button-primary' : 'button-quiet'}`}
                    style={{ height: 30, padding: '0 9px', fontSize: 'var(--fs-2xs)' }}
                    onClick={() => setActiveInbound(inbound.id)}
                    title={`${inbound.protocol} · 端口 ${inbound.port}`}
                  >
                    {inbound.remark || `#${inbound.id}`}（{inbound.protocol}）{count}
                    {!inbound.enable && ' · 已停用'}
                  </button>
                )
              })}
            </div>
          </div>
        )}

        <label className="search-field" style={{ maxWidth: '100%', marginBottom: 12 }}>
          <Search size={14} />
          <input placeholder="搜索客户端邮箱或入站备注…" value={query} onChange={(event) => setQuery(event.target.value)} />
        </label>

        <div className="section-kicker" style={{ marginBottom: 8 }}>CLIENTS <span>·</span> {filtered.length} 条</div>

        {loading ? (
          <div className="empty-state" style={{ minHeight: 120 }}><RefreshCw size={20} /><strong>读取中…</strong><span>正在从 3X-UI 拉取入站与客户端。</span></div>
        ) : loadError && clients.length === 0 ? (
          // 2026-11-06：读取失败时不能再显示「这个面板还没有客户端」—— 与上方的错误提示互相矛盾。
          <div className="empty-state" style={{ minHeight: 120 }}>
            <Users size={20} />
            <strong>读取客户端失败</strong>
            <span>{loadError}</span>
          </div>
        ) : filtered.length === 0 ? (
          <div className="empty-state" style={{ minHeight: 120 }}>
            <Users size={20} />
            <strong>{clients.length === 0 ? '这个面板还没有客户端' : '没有匹配的客户端'}</strong>
            <span>{clients.length === 0 ? '点上方「新增客户端」为某个入站开户。' : '试试其他邮箱或切换入站筛选。'}</span>
          </div>
        ) : (
          <div style={{ display: 'grid', gap: 8 }}>
            {filtered.map((client) => {
              const ratio = usageRatio(client.up + client.down, client.total)
              const expiry = expiryRemaining(client.expiry_time)
              return (
                <div key={`${client.inbound_id}-${client.client_id}`} className={`xui-client-card ${client.online ? 'xui-client-online' : ''}`}>
                  <div className="xui-client-head">
                    {client.online ? <Wifi size={15} /> : <WifiOff size={15} />}
                    <strong title={client.email}>{client.email}</strong>
                    {client.online && <span className="xui-online-pill">在线</span>}
                  </div>
                  <div className="xui-client-meta">
                    <span className="xui-meta-chip">{client.protocol}</span>
                    <span>{client.inbound_name || `入站 #${client.inbound_id}`}</span>
                  </div>
                  <div className="xui-client-usage">
                    <span className="xui-usage-label">用量</span>
                    <strong>{formatBytes(client.up + client.down)}</strong>
                    {client.total > 0 ? <span className="xui-usage-total">/ {formatBytes(client.total)}</span> : <span className="xui-usage-total">不限量</span>}
                    <span className="xui-usage-date">
                      {formatExpiry(client.expiry_time)}
                      {expiry.text && expiry.warn ? <em>（{expiry.text}）</em> : null}
                    </span>
                  </div>
                  {ratio !== null && (
                    <div className="xui-usage-bar"><i style={{ width: `${ratio}%` }} className={ratio >= 90 ? 'bar-hot' : undefined} /></div>
                  )}
                  <div className="xui-client-actions">
                    <button className="button button-quiet" disabled={viewer || busy} title={viewer ? '只读账号不能重置流量' : undefined} onClick={() => resetTraffic(client)}>重置流量</button>
                    <button className="button button-quiet" disabled={busy || !isAdminSession()} title={isAdminSession() ? undefined : '仅管理员可删除客户端'} onClick={() => setPendingClientDelete(client)}><Trash2 size={12} />删除</button>
                  </div>
                </div>
              )
            })}
          </div>
        )}

        <div className="drawer-actions" style={{ marginTop: 20 }}>
          <button className="button button-quiet" disabled={busy || !isAdminSession()} onClick={onRequestDelete}><Trash2 size={15} />移除登记</button>
        </div>

        {creating && (
          <AddClientModal
            panel={panel}
            inbounds={inbounds}
            defaultInbound={typeof activeInbound === 'number' ? activeInbound : undefined}
            onClose={() => setCreating(false)}
            onDone={async () => { setCreating(false); await load(); await onChanged() }}
            notify={notify}
          />
        )}
      </section>

      {pendingClientDelete && (
        <Modal title="删除客户端" kicker="CONFIRM DELETE" onClose={() => setPendingClientDelete(null)}>
          <p className="form-note">
            即将从 3X-UI 面板 <strong>{panel.name}</strong> 删除客户端 <strong>{pendingClientDelete.email}</strong>
            （入站 {pendingClientDelete.inbound_name || `#${pendingClientDelete.inbound_id}`}）。
            <strong>该客户端的订阅会立即失效</strong>，其流量记录一并清除。此操作不可撤销。
          </p>
          <div className="modal-actions">
            <button className="button button-quiet" onClick={() => setPendingClientDelete(null)}>取消</button>
            <button className="button button-danger" disabled={busy} onClick={confirmDeleteClient}>{busy ? '删除中…' : '确认删除'}</button>
          </div>
        </Modal>
      )}
    </div>
  )
}

// ---- 新增客户端 ----

function AddClientModal({ panel, inbounds, defaultInbound, onClose, onDone, notify }: {
  panel: XuiPanel
  inbounds: XuiInbound[]
  defaultInbound?: number
  onClose: () => void
  onDone: () => Promise<void> | void
  notify: (message: string) => void
}) {
  const [inboundID, setInboundID] = useState<number>(defaultInbound ?? inbounds[0]?.id ?? 0)
  const [email, setEmail] = useState('')
  const [expireDays, setExpireDays] = useState('30')
  const [limitGB, setLimitGB] = useState('0')
  const [busy, setBusy] = useState(false)
  const mountedRef = useRef(true)
  useEffect(() => {
    mountedRef.current = true
    return () => { mountedRef.current = false }
  }, [])

  const selected = inbounds.find((inbound) => inbound.id === inboundID)

  async function submit() {
    if (busy) return
    const trimmedEmail = email.trim()
    // 与服务端同一套白名单，前端先挡一道，避免白跑一次请求。
    if (!/^[A-Za-z0-9._@+-]{1,64}$/.test(trimmedEmail)) {
      notify('邮箱只能包含字母、数字与 . _ @ + -，长度 1-64')
      return
    }
    const days = Number(expireDays)
    const gb = Number(limitGB)
    if (!Number.isInteger(days) || days < 0 || days > 3650) {
      notify('有效期天数需为 0-3650 的整数（0 表示永不过期）')
      return
    }
    if (!Number.isFinite(gb) || gb < 0) {
      notify('流量上限需 >= 0（0 表示不限量）')
      return
    }
    setBusy(true)
    try {
      await api.xuiAddClient(panel.id, {
        inbound_id: inboundID,
        email: trimmedEmail,
        // 面板按字节存配额，UI 按 GB 填，这里换算。
        limit_total_bytes: Math.round(gb * 1024 * 1024 * 1024),
        expire_days: days,
      })
      if (!mountedRef.current) return
      notify(`已新增客户端：${trimmedEmail}`)
      await onDone()
    } catch (err) {
      if (mountedRef.current) notify(err instanceof Error ? `新增失败：${err.message}` : '新增失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  return (
    <Modal title="新增客户端" kicker={`${panel.name} · ADD CLIENT`} onClose={onClose}>
      <p className="form-note">
        在选定的入站上创建一个客户端账号。UUID / 密码等协议字段由 3X-UI 自行生成，
        只需给出一个唯一邮箱作为账号标识。
      </p>
      <label className="login-field" style={{ marginTop: 12 }}>
        <span>所属入站</span>
        <select value={inboundID} onChange={(event) => setInboundID(Number(event.target.value))} disabled={busy}>
          {inbounds.map((inbound) => (
            <option key={inbound.id} value={inbound.id}>
              {inbound.remark || `入站 #${inbound.id}`} · {inbound.protocol} · 端口 {inbound.port}
            </option>
          ))}
        </select>
      </label>
      {selected && (
        <p className="form-note" style={{ marginTop: 8 }}>
          当前入站已有 {(selected.clients ?? []).length} 个客户端，端口 {selected.port}，协议 {selected.protocol}。
        </p>
      )}
      <label className="login-field" style={{ marginTop: 10 }}>
        <span>客户端邮箱（唯一标识）</span>
        <input value={email} onChange={(event) => setEmail(event.target.value)} placeholder="user001@example.com" spellCheck={false} disabled={busy} />
      </label>
      <label className="login-field" style={{ marginTop: 10 }}>
        <span>有效期（天，0 = 永不过期）</span>
        <input value={expireDays} onChange={(event) => setExpireDays(event.target.value)} inputMode="numeric" disabled={busy} />
      </label>
      <label className="login-field" style={{ marginTop: 10 }}>
        <span>流量上限（GB，0 = 不限量）</span>
        <input value={limitGB} onChange={(event) => setLimitGB(event.target.value)} inputMode="decimal" disabled={busy} />
      </label>
      <div className="modal-actions">
        <button className="button button-quiet" onClick={onClose} disabled={busy}>取消</button>
        <button className="button button-primary" onClick={submit} disabled={busy || !inboundID}>创建客户端</button>
      </div>
    </Modal>
  )
}

function MetricCard({ label, value, unit, icon, tone, foot }: { label: string; value: string; unit: string; icon: React.ReactNode; tone: string; foot: React.ReactNode }) {
  return <div className={`metric-card tone-${tone}`}><div className="metric-top"><span>{label}</span><span className="metric-icon">{icon}</span></div><div className="metric-value">{value}<small>{unit}</small></div><div className="metric-foot">{foot}</div></div>
}
