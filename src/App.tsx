import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  Activity, AlertTriangle, Bell, Check, ChevronDown,
  ChevronRight, CircleHelp, Command, Cpu, Gauge, HardDrive, KeyRound,
  Layers, LayoutDashboard, ListFilter, Menu, Network, RefreshCw,
  Palette, Search, Server as ServerIcon, Settings2, ShieldCheck, ShoppingBag, Trash2, TrendingUp, X,
} from 'lucide-react'
import { api, clearSession, loadSession, SESSION_EXPIRED_EVENT, type Alert, type Hy2Node, type MetricSample, type OperationTask, type Server, type ServerStatus, type ServiceOperation } from './lib/api'
import { formatBytes } from './lib/format'
import { readDeliveryToken } from './lib/deliveryRoute'
import { applyAppearance, loadScale, loadTheme, saveScale, saveTheme, type ScaleId, type ThemeId } from './lib/theme'
import { LoginView } from './components/LoginView'
import { ServiceControlPanel } from './components/ServiceControlPanel'
import { AppearancePicker } from './components/AppearancePicker'
import { HostTrend } from './components/HostTools'
import { WhatsNewBanner } from './components/HostToolbox'
import { hasOpenModal, Modal } from './components/Modal'
import { ConfirmHost } from './components/ConfirmHost'
import { ErrorBoundary, ViewLoadingFallback } from './components/ErrorBoundary'

// 全部子页面与重型主机工作台（含终端依赖）均按需分包加载，显著压低首屏包体。
const NodeMonitoringPage = lazy(() => import('./components/NodeMonitoringPage').then((m) => ({ default: m.NodeMonitoringPage })))
const IPPurityPage = lazy(() => import('./components/IPPurityPage').then((m) => ({ default: m.IPPurityPage })))
const ServersPage = lazy(() => import('./components/ServersPage').then((m) => ({ default: m.ServersPage })))
const NodesPage = lazy(() => import('./components/NodesPage').then((m) => ({ default: m.NodesPage })))
const XuiPanelsPage = lazy(() => import('./components/XuiPanelsPage').then((m) => ({ default: m.XuiPanelsPage })))
const ProfitPage = lazy(() => import('./components/ProfitPage').then((m) => ({ default: m.ProfitPage })))
const SubscriptionCenter = lazy(() => import('./components/SubscriptionCenter').then((m) => ({ default: m.SubscriptionCenter })))
const TasksPage = lazy(() => import('./components/TasksPage').then((m) => ({ default: m.TasksPage })))
const AlertsPage = lazy(() => import('./components/AlertsPage').then((m) => ({ default: m.AlertsPage })))
const SettingsPage = lazy(() => import('./components/SettingsPage').then((m) => ({ default: m.SettingsPage })))
const DeliveryPage = lazy(() => import('./components/DeliveryPage').then((m) => ({ default: m.DeliveryPage })))
const HostWorkbench = lazy(() => import('./components/HostWorkbench').then((m) => ({ default: m.HostWorkbench })))

// 判断主机详情里的资源指标是否在 15 分钟新鲜窗口内；超时则显示「—」避免拿陈旧数据冒充当前负载。
function hostMetricsFresh(server: Server): boolean {
  if (!server.metrics_sampled_at) return false
  const ageMs = Date.now() - new Date(server.metrics_sampled_at).getTime()
  return Number.isFinite(ageMs) && ageMs <= 15 * 60 * 1000
}

// 后台全部主导航项（买家交付页由独立路由处理，不经过登录与主导航）。
const nav = [
  { label: '节点监控', icon: LayoutDashboard },
  { label: 'IP 纯净度', icon: ShieldCheck },
  { label: 'VPS 主机', icon: ServerIcon },
  { label: 'HY2 节点', icon: Network },
  { label: '3X-UI 面板', icon: Layers },
  { label: '订阅中心', icon: ShoppingBag },
  { label: '收益报表', icon: TrendingUp },
  { label: '运维任务', icon: Activity },
  { label: '告警中心', icon: Bell, alert: true },
]

const roleText: Record<string, string> = {
  admin: '管理员',
  operator: '运维人员',
  viewer: '只读访客',
}

function explainApiError(raw: string): { text: string; hint: string } {
  const m = raw.match(/\((\d{3})\)/)
  const code = m ? Number(m[1]) : 0
  const backendSaid = raw.replace(/\s*\(\d{3}\)\s*$/, '').trim()
  if (backendSaid && !/^请求失败/.test(backendSaid)) {
    return { text: backendSaid, hint: '' }
  }
  switch (code) {
    case 502:
    case 503:
    case 504:
      return {
        text: '面板服务暂时没有响应',
        hint: '通常是后端在重启或负载过高。等 10 秒后点右上角重试；若持续不通，到服务器检查后端容器运行状态。',
      }
    case 500:
      return {
        text: '面板内部出错了',
        hint: '这是服务端问题，重试一次；若反复出现，请查看后端容器错误日志。',
      }
    case 401:
      return { text: '登录状态已过期', hint: '请重新登录。' }
    case 403:
      return { text: '当前账号没有这个操作权限', hint: '该操作需要管理员账号。' }
    case 404:
      return { text: '请求的资源不存在', hint: '它可能已被删除，刷新页面看最新状态。' }
    case 0:
      return {
        text: '连不上面板服务',
        hint: '检查网络是否正常；如果面板刚部署完，确认前端与后端容器都已启动。',
      }
    default:
      return { text: raw, hint: '重试一次；若持续失败，请查看服务端日志。' }
  }
}

const serverStatusText: Record<ServerStatus, string> = {
  online: '运行中',
  warning: '资源告警',
  offline: '离线',
  unknown: '未知',
}
const serverStatusClass: Record<ServerStatus, string> = {
  online: 'online',
  warning: 'warning',
  offline: 'offline',
  unknown: 'offline',
}

const opTypeText: Record<ServiceOperation, string> = {
  health_check: '健康检查',
  restart: '重启服务',
  start: '启动服务',
  stop: '停止服务',
  reload: '重载配置',
  enable: '设为开机自启',
  disable: '取消开机自启',
  logs: '日志拉取',
  system_preset: '预设任务',
}

function operationLabel(operation: string): string {
  return opTypeText[operation as ServiceOperation] ?? operation
}

function formatTime(iso: string): string {
  const date = new Date(iso)
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleTimeString('zh-CN', { hour12: false })
}

type SearchCandidate = {
  id: string
  title: string
  subtitle: string
  tag: string
  keywords: string
  onSelect: () => void
}

function App() {
  const [active, setActive] = useState('节点监控')
  const [selectedSnapshot, setSelected] = useState<Server | null>(null)
  const [servers, setServers] = useState<Server[]>([])
  const selected = selectedSnapshot ? servers.find((server) => server.id === selectedSnapshot.id) ?? null : null
  const [nodes, setNodes] = useState<Hy2Node[]>([])
  const [alerts, setAlerts] = useState<Alert[]>([])
  const [alertsStale, setAlertsStale] = useState(false)
  const [helpOpen, setHelpOpen] = useState(false)
  const [hostCreateSignal, setHostCreateSignal] = useState(0)
  const [theme, setTheme] = useState<ThemeId>(() => loadTheme())
  const [scale, setScale] = useState<ScaleId>(() => loadScale())
  const [appearanceOpen, setAppearanceOpen] = useState(false)
  const [pendingHostDelete, setPendingHostDelete] = useState<Server | null>(null)
  const [tasks, setTasks] = useState<OperationTask[]>([])
  const [metrics, setMetrics] = useState<Record<string, MetricSample[]>>({})
  const [apiOnline, setApiOnline] = useState(false)
  const [toast, setToast] = useState('')
  const [toastSticky, setToastSticky] = useState(false)
  const [sidebarOpen, setSidebarOpen] = useState(false)
  const [searchOpen, setSearchOpen] = useState(false)
  const [searchText, setSearchText] = useState('')
  const [searchCursor, setSearchCursor] = useState(0)
  const [error, setError] = useState<string | null>(null)
  const [session, setSession] = useState(() => loadSession())
  const adminCan = session?.role === 'admin'
  const isViewer = session?.role === 'viewer'

  // 会话代数：登出或会话失效时递增，拦截在途旧请求的晚到响应，杜绝跨账号串数据。
  const sessionEpoch = useRef(0)
  const refreshInFlight = useRef(false)
  const refreshRerun = useRef(false)

  const resetWorkspaceData = useCallback(() => {
    sessionEpoch.current += 1
    refreshInFlight.current = false
    refreshRerun.current = false
    setSelected(null)
    setServers([])
    setNodes([])
    setAlerts([])
    setAlertsStale(false)
    setTasks([])
    setMetrics({})
    setError(null)
    setSearchOpen(false)
    setPendingHostDelete(null)
    setPendingCredDelete(null)
    setAppearanceOpen(false)
    setHelpOpen(false)
    setSidebarOpen(false)
    setKeyInput('')
    setRotatingCred(false)
  }, [])

  // 任何请求拿到 401（令牌过期 / 账号被停用）都回到登录页并清空状态。
  useEffect(() => {
    const onExpired = () => {
      resetWorkspaceData()
      setSession(null)
    }
    window.addEventListener(SESSION_EXPIRED_EVENT, onExpired)
    return () => window.removeEventListener(SESSION_EXPIRED_EVENT, onExpired)
  }, [resetWorkspaceData])

  const [keyInput, setKeyInput] = useState('')
  const [credAuth, setCredAuth] = useState<'ssh_key' | 'password'>('ssh_key')
  const [rotatingCred, setRotatingCred] = useState(false)
  const [pendingCredDelete, setPendingCredDelete] = useState<Server | null>(null)
  const [unitsInput, setUnitsInput] = useState('hysteria-server')
  const [busy, setBusy] = useState(false)
  // 交付页路径只解析一次，避免后续重渲染重复触发读取。
  const [deliveryToken] = useState(readDeliveryToken)

  const logout = useCallback(() => {
    api.logout()
    resetWorkspaceData()
    setSession(null)
  }, [resetWorkspaceData])

  // 前一条提示的定时器不清掉，会把紧跟着出现的新提示提前收掉；组件卸载时一并清理。
  const toastTimer = useRef<number | undefined>(undefined)
  useEffect(() => () => window.clearTimeout(toastTimer.current), [])

  const showToast = useCallback((message: string, tone?: 'info' | 'error') => {
    setToast(message)
    window.clearTimeout(toastTimer.current)
    const sticky = message.startsWith('⚠') || tone === 'error'
    setToastSticky(sticky)
    if (!sticky) {
      toastTimer.current = window.setTimeout(() => setToast(''), 3200)
    }
  }, [])

  const dismissToast = useCallback(() => {
    window.clearTimeout(toastTimer.current)
    setToast('')
    setToastSticky(false)
  }, [])

  // 后台定时刷新只拉取全局共享的 5 个核心接口（不再每 15 秒白扫未消费的小时流量与全主机指标），
  // 并带并发合并与会话代数守卫，避免慢网时请求堆积或登出后回填旧数据。
  const refresh = useCallback(async () => {
    if (refreshInFlight.current) {
      refreshRerun.current = true
      return
    }
    const epoch = sessionEpoch.current
    refreshInFlight.current = true
    try {
      const [health, list, taskList, nodeList, alertList] = await Promise.all([
        api.health().catch(() => ({ status: 'unavailable' as const })),
        api.servers().catch(() => null),
        api.tasks().catch(() => null),
        api.nodes().catch(() => null),
        api.alerts().catch(() => null),
      ])
      if (epoch !== sessionEpoch.current) return
      setApiOnline(health.status === 'ok')
      const failedParts: string[] = []
      if (list === null) failedParts.push('主机')
      else setServers(list)
      if (taskList === null) failedParts.push('运维任务')
      else setTasks(taskList)
      if (nodeList === null) failedParts.push('节点')
      else setNodes(nodeList)
      setAlertsStale(alertList === null)
      if (alertList === null) failedParts.push('告警')
      else setAlerts(alertList)
      setError(failedParts.length ? `${failedParts.join('、')}未更新（显示的是上次数据）` : null)
    } catch (err) {
      if (epoch !== sessionEpoch.current) return
      const message = err instanceof Error ? err.message : '请求失败'
      setError(message)
      void api.health().then((h) => {
        if (epoch === sessionEpoch.current) setApiOnline(h.status === 'ok')
      }).catch(() => {
        if (epoch === sessionEpoch.current) setApiOnline(false)
      })
      if ((err as { status?: number }).status === 401) {
        clearSession()
        resetWorkspaceData()
        setSession(null)
      }
    } finally {
      refreshInFlight.current = false
      if (refreshRerun.current && epoch === sessionEpoch.current) {
        refreshRerun.current = false
        void refresh()
      }
    }
  }, [resetWorkspaceData])

  useEffect(() => {
    if (!session) return
    setError(null)
    void refresh()
    // 标签页在后台时不轮询，切回前台立即补一次。
    const handle = window.setInterval(() => { if (!document.hidden) void refresh() }, 15_000)
    const onVisible = () => { if (!document.hidden) void refresh() }
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      window.clearInterval(handle)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [refresh, session])

  // 按需加载主机详情抽屉的 24 小时趋势指标：仅在打开具体主机时拉取该主机序列，减轻数据库负载。
  const selectedId = selected?.id
  useEffect(() => {
    if (!selectedId || !session) return
    let cancelled = false
    void api.serverMetrics(selectedId)
      .then((samples) => {
        if (!cancelled) {
          setMetrics((prev) => ({ ...prev, [selectedId]: samples }))
        }
      })
      .catch(() => { /* 趋势图拉取失败保留旧快照或空态，不打断抽屉操作 */ })
    return () => { cancelled = true }
  }, [selectedId, session])

  useEffect(() => {
    applyAppearance(theme, scale)
  }, [theme, scale])

  function chooseTheme(next: ThemeId) {
    setTheme(next)
    saveTheme(next)
  }

  function chooseScale(next: ScaleId) {
    setScale(next)
    saveScale(next)
  }

  // 全局快捷键：Ctrl/⌘+K 打开快捷搜索弹窗；Esc 关闭主机详情抽屉（当上层无更高级弹窗时）。
  useEffect(() => {
    if (!session) return
    function onKey(event: KeyboardEvent) {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') {
        event.preventDefault()
        setSearchText('')
        setSearchCursor(0)
        setSearchOpen(true)
        window.setTimeout(() => document.getElementById('server-search')?.focus(), 0)
        return
      }
      if (
        event.key === 'Escape' &&
        !event.defaultPrevented &&
        !hasOpenModal() &&
        selected &&
        !searchOpen &&
        !pendingHostDelete &&
        !pendingCredDelete &&
        !appearanceOpen &&
        !helpOpen
      ) {
        // 焦点位于交互式 SSH 终端内时，将 Esc 键保留给远端程序（如 vim / less），禁止直接关掉抽屉断开终端。
        if (event.target instanceof Element && event.target.closest('.host-terminal, .xterm')) return
        event.preventDefault()
        setSelected(null)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [session, selected, searchOpen, pendingHostDelete, pendingCredDelete, appearanceOpen, helpOpen])

  useEffect(() => {
    if (!selected) return
    setUnitsInput(selected.write_units?.length ? selected.write_units.join(', ') : 'hysteria-server')
    setRotatingCred(false)
    setKeyInput('')
  }, [selected?.id])

  // 快捷搜索候选项：涵盖全部导航页、VPS 主机、HY2 节点与常用快捷入口。
  const searchCandidates = useMemo<SearchCandidate[]>(() => {
    const items: SearchCandidate[] = [
      ...nav.map((item) => ({
        id: `nav:${item.label}`,
        title: item.label,
        subtitle: '切换到此控制台页面',
        tag: '页面',
        keywords: item.label.toLowerCase(),
        onSelect: () => { setActive(item.label); setSearchOpen(false) },
      })),
      {
        id: 'nav:系统设置',
        title: '系统设置',
        subtitle: '修改密码、账号权限、告警与外观配置',
        tag: '页面',
        keywords: '系统设置 设置 账号 密码 审计 告警 外观',
        onSelect: () => { setActive('系统设置'); setSearchOpen(false) },
      },
      ...(adminCan ? [{
        id: 'action:new-host',
        title: '登记新 VPS 主机',
        subtitle: '跳转到 VPS 主机页并打开新增主机窗口',
        tag: '快捷动作',
        keywords: '新建 添加 登记 vps 主机 server',
        onSelect: () => {
          setActive('VPS 主机')
          setHostCreateSignal((n) => n + 1)
          setSearchOpen(false)
        },
      }] : []),
      ...servers.map((server) => ({
        id: `server:${server.id}`,
        title: server.name,
        subtitle: `${server.host} · ${server.region || '未分组'} · ${serverStatusText[server.status]}`,
        tag: 'VPS 主机',
        keywords: `${server.name} ${server.host} ${server.region} ${server.provider} ${server.tags.join(' ')}`.toLowerCase(),
        onSelect: () => { setSelected(server); setSearchOpen(false) },
      })),
      ...nodes.map((node) => ({
        id: `node:${node.id}`,
        title: node.name,
        subtitle: `${node.domain || '无域名'}:${node.listen_port} · ${node.status === 'online' ? '在线' : node.status}`,
        tag: 'HY2 节点',
        keywords: `${node.name} ${node.domain} ${node.listen_port} ${node.api_url}`.toLowerCase(),
        onSelect: () => { setActive('HY2 节点'); setSearchOpen(false) },
      })),
    ]
    const q = searchText.trim().toLowerCase()
    if (!q) return items.slice(0, 12)
    return items.filter((it) => it.keywords.includes(q) || it.title.toLowerCase().includes(q) || it.subtitle.toLowerCase().includes(q)).slice(0, 20)
  }, [adminCan, servers, nodes, searchText])

  const safeSearchCursor = searchCandidates.length > 0 ? Math.min(searchCursor, searchCandidates.length - 1) : 0

  async function runAction(action: ServiceOperation, server: Server, unit = 'hysteria-server') {
    if (isViewer || busy) return
    setBusy(true)
    try {
      await api.createTask({ server_id: server.id, operation_type: action, parameters: { source: 'console', unit } })
      showToast(`已下发：${operationLabel(action)} · ${server.name}（执行器约 20 秒内领取）`)
      await refresh()
    } catch (err) {
      showToast(err instanceof Error ? `任务创建失败：${err.message}` : '任务创建失败', 'error')
    } finally {
      setBusy(false)
    }
  }

  async function attachCredential(server: Server) {
    if (keyInput.trim().length < 6) {
      showToast(credAuth === 'ssh_key' ? '请粘贴 SSH 私钥（PEM）' : 'SSH 密码至少 6 个字符')
      return
    }
    setBusy(true)
    try {
      await api.uploadCredential(server.id, credAuth, credAuth === 'ssh_key' ? `${keyInput.trimEnd()}\n` : keyInput.trim())
      setKeyInput('')
      setRotatingCred(false)
      showToast(`凭据已加密保存：${server.name}`)
      await refresh()
    } catch (err) {
      showToast(err instanceof Error ? `凭据保存失败：${err.message}` : '凭据保存失败', 'error')
    } finally {
      setBusy(false)
    }
  }

  async function removeCredential() {
    if (!pendingCredDelete) return
    const target = pendingCredDelete
    setPendingCredDelete(null)
    setBusy(true)
    try {
      await api.deleteCredential(target.id)
      setKeyInput('')
      setRotatingCred(false)
      showToast(`已删除凭据：${target.name}（转为只登记，不再采集）`)
      await refresh()
    } catch (err) {
      showToast(err instanceof Error ? `删除凭据失败：${err.message}` : '删除凭据失败', 'error')
    } finally {
      setBusy(false)
    }
  }

  async function toggleWriteOps(server: Server, allow: boolean) {
    const units = allow ? unitsInput.split(/[,\s]+/).map((unit) => unit.trim()).filter(Boolean) : []
    if (allow && units.length === 0) {
      showToast('请先填写允许重启的服务名白名单')
      return
    }
    setBusy(true)
    try {
      await api.setWriteOps(server.id, allow, units)
      showToast(allow ? `已开启写操作：${server.name}（仅限 ${units.join(', ')}）` : `已关闭写操作：${server.name}`)
      await refresh()
    } catch (err) {
      showToast(err instanceof Error ? `设置失败：${err.message}` : '设置失败', 'error')
    } finally {
      setBusy(false)
    }
  }

  async function confirmRemoveServer() {
    if (!pendingHostDelete) return
    const target = pendingHostDelete
    setPendingHostDelete(null)
    setSelected(null)
    setBusy(true)
    try {
      await api.deleteServer(target.id)
      showToast(`已删除：${target.name}`)
      await refresh()
    } catch (err) {
      showToast(err instanceof Error ? `删除失败：${err.message}` : '删除失败', 'error')
    } finally {
      setBusy(false)
    }
  }

  const realMode = servers.some((s) => s.has_credential)
  const hasNodes = nodes.length > 0
  const openAlerts = alerts.filter((alert) => alert.status === 'open').length
  const pageTitle = active

  // 买家交付页优先于管理员登录守卫，持链接令牌即可直接访问。
  if (deliveryToken) {
    return (
      <ErrorBoundary scope="订阅交付页">
        <Suspense fallback={<div className="root-loading-shell"><ViewLoadingFallback label="正在加载订阅页面…" /></div>}>
          <DeliveryPage token={deliveryToken} />
        </Suspense>
      </ErrorBoundary>
    )
  }

  if (!session) {
    return <LoginView onLogin={() => { sessionEpoch.current += 1; setSession(loadSession()) }} />
  }

  const envLabel = !apiOnline ? 'API 离线' : realMode ? '真实数据' : hasNodes ? 'API 监测' : '未接入节点'
  const envLive = apiOnline && (realMode || hasNodes)

  const pageContent = (() => {
    switch (active) {
      case '节点监控':
        return <NodeMonitoringPage notify={showToast} />
      case 'IP 纯净度':
        return <IPPurityPage notify={showToast} readOnly={session.role !== 'admin'} />
      case 'VPS 主机':
        return <ServersPage servers={servers} nodes={nodes} createSignal={hostCreateSignal} onOpen={setSelected} onChanged={refresh} notify={showToast} />
      case 'HY2 节点':
        return <NodesPage nodes={nodes} servers={servers} onChanged={refresh} notify={showToast} />
      case '3X-UI 面板':
        return <XuiPanelsPage notify={showToast} />
      case '收益报表':
        return <ProfitPage notify={showToast} readOnly={session.role !== 'admin'} />
      case '订阅中心':
        return <SubscriptionCenter notify={showToast} />
      case '运维任务':
        return <TasksPage tasks={tasks} servers={servers} onChanged={refresh} notify={showToast} />
      case '告警中心':
        return <AlertsPage alerts={alerts} stale={alertsStale} onChanged={refresh} notify={showToast} />
      case '系统设置':
        return <SettingsPage servers={servers} session={{ email: session.email, role: session.role }} onChanged={refresh} notify={showToast} appearance={<AppearancePicker theme={theme} scale={scale} onTheme={chooseTheme} onScale={chooseScale} />} />
      default:
        return null
    }
  })()

  return (
    <div className="app-shell">
      <aside className={`sidebar ${sidebarOpen ? 'sidebar-open' : ''}`}>
        <div className="brand-row">
          <div className="brand-mark"><span /><span /><span /></div>
          <div className="brand-name">Teyir<i>控制台</i></div>
          <button className="icon-button close-sidebar" aria-label="关闭导航" onClick={() => setSidebarOpen(false)}><X size={18} /></button>
        </div>
        <div className="workspace-switch">
          <div className="workspace-icon"><Command size={16} /></div>
          <div className="workspace-copy"><span>工作空间</span><strong>运维总控</strong></div>
          <ChevronDown size={15} className="subtle-icon" />
        </div>
        <div className="nav-caption">控制台</div>
        <nav className="main-nav" aria-label="主导航">
          {nav.map(({ label, icon: Icon, alert }) => (
            <button key={label} className={`nav-item ${active === label ? 'active' : ''}`} onClick={() => { setActive(label); setSidebarOpen(false) }}>
              <Icon size={17} strokeWidth={1.8} /><span>{label}</span>
              {alert && openAlerts > 0 && <span className="nav-count count-alert">{openAlerts}</span>}
            </button>
          ))}
        </nav>
        <div className="sidebar-spacer" />
        <div className="connection-card">
          <div className="connection-top"><span className={`live-dot ${apiOnline ? '' : 'dot-off'}`} />面板服务<span className={apiOnline ? 'connected' : 'connected disconnected'}>{apiOnline ? '运行中' : '连不上'}</span></div>
          <div className="connection-line" title="面板后端是否在正常应答。异常时页面上的数据会停止刷新。">
            <span>后端接口</span><strong>{apiOnline ? '正常' : '无响应'}</strong>
          </div>
          <div className="connection-line" title="「主机真实采集」表示面板已拿到主机的 SSH 凭据；「仅节点 API」表示通过节点接口监测状态。">
            <span>数据来源</span>
            <strong>{!apiOnline ? '暂停刷新' : realMode ? '主机真实采集' : hasNodes ? '仅节点 API' : '未配置凭据'}</strong>
          </div>
          <div className="connection-foot"><ShieldCheck size={13} /> {realMode ? '重启/停止等写操作需逐台授权' : '写操作未开启，仅可查看'}</div>
        </div>
        <button className={`nav-item settings-item ${active === '系统设置' ? 'active' : ''}`} onClick={() => { setActive('系统设置'); setSidebarOpen(false) }}><Settings2 size={17} /><span>系统设置</span><ChevronRight size={15} className="nav-end" /></button>
        <div className="profile-row"><div className="avatar">{(session.email[0] ?? 'O').toUpperCase()}</div><div className="profile-copy"><strong>{session.email}</strong><span>{roleText[session.role] ?? session.role}</span></div><button className="icon-button logout-button" aria-label="退出登录" onClick={logout}><span>退出</span></button></div>
      </aside>

      <main className="main-area">
        <header className="topbar">
          <div className="breadcrumbs"><button className="mobile-menu icon-button" aria-label="打开导航" onClick={() => setSidebarOpen(true)}><Menu size={19} /></button><span>控制台</span><ChevronRight size={14} /><strong>{pageTitle}</strong></div>
          <div className="top-actions">
            <div className={`environment-pill ${envLive ? 'env-live' : 'env-local'}`}><span />{envLabel}</div>
            <button
              className="icon-button search-trigger"
              aria-label="快捷搜索主机与页面"
              title="快捷搜索（Ctrl/⌘ + K）"
              onClick={() => {
                setSearchText('')
                setSearchCursor(0)
                setSearchOpen(true)
                window.setTimeout(() => document.getElementById('server-search')?.focus(), 0)
              }}
            >
              <Search size={17} />
            </button>
            <button className="icon-button" aria-label="外观设置" title="外观设置" onClick={() => setAppearanceOpen(true)}><Palette size={17} /></button>
            <button className="icon-button notification-button" aria-label="查看告警" title="查看告警" onClick={() => setActive('告警中心')}><Bell size={17} />{openAlerts > 0 && <i />}</button>
            <div className="top-divider" />
            <span className="top-time">UTC+8 <ChevronDown size={13} /></span>
          </div>
        </header>

        <div className="content-wrap">
          {session.role === 'admin' && <WhatsNewBanner onGo={(page) => setActive(page as typeof active)} />}
          {error && (() => {
            const { text, hint } = explainApiError(error)
            return (
              <div className="api-error-banner" role="alert">
                <AlertTriangle size={16} />
                <div className="api-error-copy">
                  <strong>{text}</strong>
                  {hint && <small>{hint}</small>}
                </div>
                <button className="button button-quiet api-error-retry" onClick={() => { void refresh(); showToast('已重试') }}>
                  <RefreshCw size={14} />重试
                </button>
              </div>
            )
          })()}

          <ErrorBoundary scope={pageTitle} resetKey={active}>
            <Suspense fallback={<ViewLoadingFallback label={`正在加载${pageTitle}…`} />}>
              {pageContent}
            </Suspense>
          </ErrorBoundary>

          <footer className="page-footer">
            <span><span className="footer-mark">T</span> Teyir 控制台 <i /> 基础设施运维中枢</span>
            <span>{!apiOnline ? 'API 离线 · 数据不可用' : realMode ? 'API 已连接 · SSH 真实采集' : hasNodes ? 'API 已连接 · 节点接口监测' : 'API 已连接 · 主机未配置凭据'} <i /> {realMode ? '写操作按主机授权' : '远程操作未启用'}</span>
            <button onClick={() => setHelpOpen(true)}><CircleHelp size={14} /> 帮助</button>
          </footer>
        </div>
      </main>

      {sidebarOpen && <button className="mobile-scrim" aria-label="关闭导航" onClick={() => setSidebarOpen(false)} />}
      {selected && (
        <div className="modal-backdrop" onMouseDown={(event) => event.target === event.currentTarget && setSelected(null)}>
          <section className="detail-drawer host-detail-drawer" role="dialog" aria-modal="true" aria-labelledby="drawer-title">
            <div className="drawer-head">
              <div>
                <div className="section-kicker">主机详情 <span>·</span> {selected.id.slice(0, 8).toUpperCase()}</div>
                <h2 id="drawer-title">{selected.name}</h2>
              </div>
              <button className="icon-button" aria-label="关闭详情" onClick={() => setSelected(null)}><X size={18} /></button>
            </div>
            <div className="drawer-status">
              <span className={`status-badge ${serverStatusClass[selected.status]}`}><i />{serverStatusText[selected.status]}</span>
              <span>更新时间 · {formatTime(selected.updated_at)}</span>
            </div>
            <div className="drawer-address">
              <span>公网地址</span><strong>{selected.host}</strong>
              <span className="provider-tag">{selected.provider || selected.region || '未分组'}</span>
            </div>
            <div className="drawer-metrics">
              {(() => {
                const fresh = hostMetricsFresh(selected)
                const fmt = (v: number) => fresh ? `${Math.round(v)}%` : '—'
                const bar = (v: number) => fresh ? Math.round(v) : 0
                return (
                  <>
                    <DrawerMetric icon={<Cpu size={16} />} label="CPU 使用率" value={fmt(selected.cpu_percent)} percent={bar(selected.cpu_percent)} />
                    <DrawerMetric icon={<Gauge size={16} />} label="内存使用率" value={fmt(selected.memory_percent)} percent={bar(selected.memory_percent)} />
                    <DrawerMetric icon={<HardDrive size={16} />} label="磁盘使用率" value={fmt(selected.disk_percent)} percent={bar(selected.disk_percent)} />
                  </>
                )
              })()}
            </div>
            <div className="drawer-trend">
              <span>24 小时趋势</span>
              <div className="drawer-trend-grid">
                <div className="drawer-trend-item"><small>CPU</small><Sparkline values={(metrics[selected.id] ?? []).map((sample) => Math.round(sample.cpu_percent))} color="var(--accent)" /></div>
                <div className="drawer-trend-item"><small>内存</small><Sparkline values={(metrics[selected.id] ?? []).map((sample) => Math.round(sample.memory_percent))} color="var(--green)" /></div>
                <div className="drawer-trend-item"><small>磁盘</small><Sparkline values={(metrics[selected.id] ?? []).map((sample) => Math.round(sample.disk_percent))} color="var(--muted)" /></div>
              </div>
            </div>
            <div className="drawer-info-grid">
              <div><span>区域</span><strong>{selected.region || '未设置'}</strong></div>
              <div><span>SSH 端口</span><strong>{selected.ssh_port}</strong></div>
              <div><span>SSH 用户</span><strong>{selected.ssh_user}</strong></div>
              <div><span>标签</span><strong>{selected.tags.length ? selected.tags.join(', ') : '—'}</strong></div>
              <div><span>24h 出站</span><strong>{formatBytes(selected.tx_bytes_24h, { decimals: 1 })}</strong></div>
              <div><span>24h 入站</span><strong>{formatBytes(selected.rx_bytes_24h, { decimals: 1 })}</strong></div>
            </div>
            <div className={selected.has_credential ? 'drawer-notice' : 'drawer-notice warn'}>
              {selected.has_credential ? <ShieldCheck size={17} /> : <AlertTriangle size={17} />}
              <span>
                <strong>{selected.has_credential ? '已接入 SSH 凭据，指标为真实采集' : '未配置 SSH 凭据，无法采集真实指标'}</strong>
                <small>{selected.has_credential ? `主机密钥已在首次连接时锁定（TOFU）；${selected.allow_write_ops ? '写操作已开启，重启类任务会真实执行' : '写操作关闭，仅执行只读任务'}` : '粘贴目标主机的私钥（PEM）或密码后由采集器加密存储'}</small>
              </span>
            </div>
            {(!selected.has_credential || rotatingCred) && (
              <label className="login-field" style={{ marginTop: 14 }}>
                <span>SSH 认证方式</span>
                <select value={credAuth} disabled={busy || !adminCan} onChange={(event) => setCredAuth(event.target.value as 'ssh_key' | 'password')}>
                  <option value="ssh_key">私钥（PEM，推荐）</option>
                  <option value="password">密码</option>
                </select>
              </label>
            )}
            {(!selected.has_credential || rotatingCred) && (credAuth === 'ssh_key'
              ? <label className="login-field" style={{ marginTop: 10 }}><span>SSH 私钥（仅用于本次加密存储）</span><textarea className="key-input" rows={4} spellCheck={false} disabled={busy || !adminCan} value={keyInput} onChange={(event) => setKeyInput(event.target.value)} placeholder="-----BEGIN OPENSSH PRIVATE KEY-----" /></label>
              : <label className="login-field" style={{ marginTop: 10 }}><span>SSH 密码</span><input type="password" disabled={busy || !adminCan} value={keyInput} onChange={(event) => setKeyInput(event.target.value)} autoComplete="off" /></label>)}
            {selected.has_credential && (
              <label className="login-field" style={{ marginTop: 14 }}>
                <span>允许写操作的服务白名单（逗号分隔，仅这些服务单元可被重启）</span>
                <input value={unitsInput} onChange={(event) => setUnitsInput(event.target.value)} spellCheck={false} placeholder="hysteria-server" disabled={busy || !adminCan} />
              </label>
            )}
            <ServiceControlPanel server={selected} notify={showToast} />
            <div className="drawer-actions">
              {!selected.has_credential && <button className="button button-primary" disabled={busy || !adminCan} title={adminCan ? undefined : ADMIN_HOST_HINT} onClick={() => attachCredential(selected)}><ShieldCheck size={15} />保存凭据</button>}
              {selected.has_credential && (
                <>
                  <button className="button button-quiet" disabled={busy || isViewer} title={isViewer ? '只读账号不能下发运维任务' : undefined} onClick={() => runAction('health_check', selected)}><RefreshCw size={15} />健康检查</button>
                  <button className="button button-quiet" disabled={busy || isViewer} title={isViewer ? '只读账号不能下发运维任务' : undefined} onClick={() => runAction('logs', selected)}><ListFilter size={15} />拉取日志</button>
                  <button className={`button ${selected.allow_write_ops ? 'button-primary' : 'button-quiet'}`} disabled={busy || !adminCan} title={adminCan ? undefined : ADMIN_HOST_HINT} onClick={() => toggleWriteOps(selected, !selected.allow_write_ops)}><ShieldCheck size={15} />{selected.allow_write_ops ? '关闭写操作' : '开启写操作'}</button>
                  <button className="button button-quiet" disabled={busy || !adminCan} title={adminCan ? undefined : ADMIN_HOST_HINT} onClick={() => setRotatingCred((current) => !current)}><KeyRound size={15} />{rotatingCred ? '取消轮换' : '轮换凭据'}</button>
                  {rotatingCred && <button className="button button-primary" disabled={busy || !adminCan} title={adminCan ? undefined : ADMIN_HOST_HINT} onClick={() => attachCredential(selected)}><ShieldCheck size={15} />保存新凭据</button>}
                  <button className="button button-quiet" disabled={busy || !adminCan} title={adminCan ? undefined : ADMIN_HOST_HINT} onClick={() => setPendingCredDelete(selected)}><Trash2 size={15} />删除凭据</button>
                </>
              )}
              <button className="button button-quiet" disabled={busy || !adminCan} title={adminCan ? undefined : ADMIN_HOST_HINT} onClick={() => setPendingHostDelete(selected)}><Trash2 size={15} />删除主机</button>
            </div>
            <HostTrend server={selected} />
            <ErrorBoundary scope="主机工作台" resetKey={selected.id}>
              <Suspense fallback={<ViewLoadingFallback label="正在加载主机工作台…" />}>
                <HostWorkbench key={selected.id} server={selected} role={session.role} />
              </Suspense>
            </ErrorBoundary>
          </section>
        </div>
      )}

      {searchOpen && (
        <Modal title="快捷搜索与跳转" kicker="快捷搜索" onClose={() => setSearchOpen(false)}>
          <div className="quick-search-box">
            <div className="quick-search-input-wrap">
              <Search size={16} />
              <input
                id="server-search"
                type="search"
                autoFocus
                placeholder="输入页面名、主机名、公网地址、区域或节点域名…"
                value={searchText}
                onChange={(event) => { setSearchText(event.target.value); setSearchCursor(0) }}
                onKeyDown={(event) => {
                  if (event.key === 'ArrowDown') {
                    event.preventDefault()
                    if (searchCandidates.length > 0) setSearchCursor((c) => (c + 1) % searchCandidates.length)
                  } else if (event.key === 'ArrowUp') {
                    event.preventDefault()
                    if (searchCandidates.length > 0) setSearchCursor((c) => (c - 1 + searchCandidates.length) % searchCandidates.length)
                  } else if (event.key === 'Enter' && searchCandidates[safeSearchCursor]) {
                    event.preventDefault()
                    searchCandidates[safeSearchCursor].onSelect()
                  }
                }}
              />
            </div>
            {searchCandidates.length === 0 ? (
              <p className="form-note">没有找到匹配项，试试主机 IP、地区名或导航名称。</p>
            ) : (
              <div className="quick-search-list" role="listbox" aria-label="快捷搜索结果">
                {searchCandidates.map((item, idx) => (
                  <button
                    key={item.id}
                    type="button"
                    role="option"
                    aria-selected={idx === safeSearchCursor}
                    className={`quick-search-item${idx === safeSearchCursor ? ' active' : ''}`}
                    onMouseEnter={() => setSearchCursor(idx)}
                    onClick={item.onSelect}
                  >
                    <span className="quick-search-item-main">
                      <strong>{item.title}</strong>
                      <small>{item.subtitle}</small>
                    </span>
                    <span className="quick-search-tag">{item.tag}</span>
                  </button>
                ))}
              </div>
            )}
            <div className="quick-search-foot">
              <span>方向键切换 · 回车跳转 · Esc 关闭</span>
              <span>共 {searchCandidates.length} 项</span>
            </div>
          </div>
        </Modal>
      )}

      {pendingHostDelete && (
        <Modal title="删除主机" kicker="危险操作确认" onClose={() => setPendingHostDelete(null)}>
          <p className="form-note">即将删除主机 <strong>{pendingHostDelete.name}</strong>（{pendingHostDelete.host}）。软删除后历史指标与任务仍保留；若其下仍有节点需先在 HY2 节点页删除。</p>
          <div className="modal-actions"><button className="button button-quiet" disabled={busy} onClick={() => setPendingHostDelete(null)}>取消</button><button className="button button-danger" disabled={busy} onClick={confirmRemoveServer}>确认删除</button></div>
        </Modal>
      )}

      {pendingCredDelete && (
        <Modal title="删除 SSH 凭据" kicker="危险操作确认" onClose={() => setPendingCredDelete(null)}>
          <p className="form-note">即将删除主机 <strong>{pendingCredDelete.name}</strong>（{pendingCredDelete.host}）的加密凭据。删除后采集器与执行器都无法再连接该主机，节点会显示为未知；历史指标与任务记录保留。</p>
          <div className="modal-actions"><button className="button button-quiet" disabled={busy} onClick={() => setPendingCredDelete(null)}>取消</button><button className="button button-danger" disabled={busy} onClick={removeCredential}>确认删除凭据</button></div>
        </Modal>
      )}

      {appearanceOpen && (
        <Modal title="外观设置" kicker="外观定制" onClose={() => setAppearanceOpen(false)}>
          <AppearancePicker theme={theme} scale={scale} onTheme={chooseTheme} onScale={chooseScale} />
          <div className="modal-actions"><button className="button button-primary" onClick={() => setAppearanceOpen(false)}>完成</button></div>
        </Modal>
      )}

      {helpOpen && (
        <Modal title="使用与限制说明" kicker="使用帮助" onClose={() => setHelpOpen(false)}>
          <div className="help-body">
            <h3>能做什么</h3>
            <ul>
              <li>登记主机、上传 SSH 私钥或密码（AES-256-GCM 加密入库，接口不回显），采集真实 CPU / 内存 / 磁盘 / 负载与握手延迟；详情抽屉可轮换或删除凭据。</li>
              <li>修改主机的地址、SSH 端口与用户；改动会清空已锁定的主机密钥指纹，下次连接按首次信任原则重新锁定。</li>
              <li><strong>VPS 主机是辅助/高级功能</strong>：HY2 节点的接入、状态、用户管理、代理服务与三网延迟监测<strong>全部不依赖它</strong>，节点只靠自己的门户接口工作。这里登记主机是为了额外拿到 CPU / 内存 / 磁盘这类操作系统级指标，以及下发重启、拉日志等运维动作。</li>
              <li>HY2 节点支持接入、编辑（名称 / 端口 / 版本 / 域名）与删除。接入走节点侧接口，产出的节点所有功能立即可用。</li>
              <li>下发任务：<code>健康检查</code>、<code>拉取日志</code> 为只读真实执行；<code>重启服务</code> 需该主机开启写操作且服务单元在白名单内。已结束的任务可按原参数重新入队。</li>
              <li>告警中心逐条确认可处理；主机或节点恢复后告警自动转为已解决，点「显示已解决」查看归档。</li>
              <li>系统设置内可改登录密码、查看运行时状态、审计日志，管理员可新建账号、调整角色、重置他人密码或删除账号。</li>
            </ul>
            <h3>快捷键</h3>
            <ul><li><kbd>Ctrl</kbd>/<kbd>⌘</kbd> + <kbd>K</kbd> 打开快捷搜索与页面跳转</li><li><kbd>Esc</kbd> 关闭弹层与详情抽屉</li></ul>
            <h3>当前边界</h3>
            <ul>
              <li>流量趋势来自 Hysteria 自带的 <code>trafficStats</code> 接口（按采集周期清零，天然是速率），在主机表单里填「速率采集接口 + 密钥文件路径」即开启；没开启的主机改用门户记账值，且明确标注口径，不伪装成上/下行。</li>
              <li>写操作依赖目标机具备 sudo（以 root 连接时直接执行）。</li>
              <li>执行器为单实例轮询，任务约在下一个周期（默认 20 秒）被领取。</li>
            </ul>
          </div>
        </Modal>
      )}
      <ConfirmHost />
      {toast && <div className={`toast-message${toastSticky ? ' toast-sticky' : ''}`} role="status">{toastSticky ? <AlertTriangle size={16} /> : <Check size={16} />}{toast}<button aria-label="关闭提示" onClick={dismissToast}><X size={14} /></button></div>}
    </div>
  )
}

function DrawerMetric({ icon, label, value, percent }: { icon: React.ReactNode; label: string; value: string; percent: number }) {
  const safePercent = Number.isFinite(percent) ? Math.min(100, Math.max(0, percent)) : 0
  return <div className="drawer-metric"><div><span className="drawer-metric-icon">{icon}</span><span>{label}</span><strong>{value}</strong></div><div className="drawer-meter"><i style={{ width: `${safePercent}%` }} /></div></div>
}

function Sparkline({ values, color = 'var(--green)' }: { values: number[]; color?: string }) {
  const clean = values.filter((v) => Number.isFinite(v))
  if (clean.length < 2) return <div className="sparkline-empty">—</div>
  const width = 120
  const height = 36
  const max = Math.max(...clean, 1)
  const min = Math.min(...clean, 0)
  const range = Math.max(max - min, 1)
  const points = clean.map((value, index) => {
    const x = (index / (clean.length - 1)) * width
    const y = height - ((value - min) / range) * height
    return `${x.toFixed(1)},${y.toFixed(1)}`
  })
  return (
    <svg className="sparkline" viewBox={`0 0 ${width} ${height}`} preserveAspectRatio="none" aria-hidden="true">
      <polyline points={points.join(' ')} fill="none" stroke={color} strokeWidth="1.4" strokeLinejoin="round" />
    </svg>
  )
}

export default App

const ADMIN_HOST_HINT = '仅管理员可操作（凭据、写操作授权与删除主机）'
