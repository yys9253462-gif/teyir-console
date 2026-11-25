import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  Activity, Clock, Cpu, Database, Eye, KeyRound, LayoutDashboard, Lock, Palette, Plus,
  Search, Server as ServerIcon, ShieldCheck, Trash2, UserCog, Users, Send,
} from 'lucide-react'
import { api, type AuditEntry, type RuntimeStatus, type Server, type UserAccount } from '../lib/api'
import { Modal } from './Modal'
import { askConfirm } from './ConfirmHost'
import { PurityKeysCard } from './PurityKeysCard'
import { TelegramCard } from './TelegramCard'
import { DeliveryDomainCard, RecentLogins, SchedulesCard, StoreSignatureCard } from './SecurityExtras'

// 系统设置（2026-10-15 改版）。
//
// 原来是两列堆叠的七张卡片：信息权重一样、找东西要上下翻、审计里显示的是
// `set_purity_keys` 这种原始动作码。现在改成「左侧分区导航 + 右侧单一面板」：
//   概览 / 账号安全 / 用户与角色（管理员）/ 集成密钥（管理员）/ 操作审计（管理员）/ 外观。
// 当前分区记在 localStorage —— 只是个人便利，读写失败时退回「概览」。

type SectionKey = 'overview' | 'security' | 'users' | 'keys' | 'telegram' | 'schedules' | 'audit' | 'appearance'

const SECTION_STORE = 'settings.section'

const ROLE_META: Record<UserAccount['role'], { label: string; desc: string }> = {
  admin: { label: '管理员', desc: '全部权限：主机、节点、账号与系统设置' },
  operator: { label: '运维', desc: '可下发任务与节点操作，不能管理账号' },
  viewer: { label: '只读', desc: '只能查看，任何写操作都会被拒绝' },
}

// 审计动作码 → 中文。未收录的原样显示，不猜。
const AUDIT_LABELS: Record<string, string> = {
  acknowledge_alert: '确认告警', bind_node_api: '绑定节点 API', unbind_node_api: '解绑节点 API',
  cancel_portal_batch: '取消门户批量更新', portal_batch_update: '批量更新门户',
  cancel_task: '取消任务', retry_task: '重试任务', create_task: '创建任务',
  change_password: '修改密码', reset_password: '重置他人密码',
  create_user: '新建账号', update_user: '修改角色', delete_user: '删除账号',
  create_node: '接入节点', update_node: '修改节点', delete_node: '删除节点',
  create_node_category: '新建分类', update_node_category: '修改分类', delete_node_category: '删除分类',
  set_node_categories: '设置节点分类',
  create_node_proxy_service: '创建代理服务', delete_node_proxy_service: '删除代理服务',
  create_node_user: '创建节点用户', delete_node_user: '删除节点用户', set_node_user_status: '启停节点用户',
  renew_node_user: '续期节点用户', renew_node_user_absolute: '续期节点用户',
  create_server: '添加主机', update_server: '修改主机', delete_server: '删除主机',
  upload_credential: '上传 SSH 凭据', delete_credential: '删除 SSH 凭据', set_write_ops: '设置写操作授权',
  create_subscription: '创建订阅', update_subscription: '修改订阅', delete_subscription: '删除订阅',
  reconcile_subscription: '对账订阅', reconcile_all_subscriptions: '全量对账订阅',
  rotate_subscription_token: '轮换订阅令牌', rotate_subscription_record: '轮换订阅记录',
  update_subscription_record: '修改订阅记录',
  host_directory_read: '浏览主机目录', host_file_preview: '预览主机文件',
  host_terminal_connect: '打开主机终端', host_terminal_disconnect: '关闭主机终端',
  refresh_all_ip_purity: '批量检测 IP 纯净度', refresh_node_ip_purity: '检测 IP 纯净度',
  refresh_node_unlock: '检测流媒体解锁', run_speedtest: '三网测速',
  set_alert_webhook: '设置告警 Webhook', set_purity_keys: '设置 IP 检测密钥',
  set_node_traffic_limit: '设置流量限额', resolve_orphan: '处理孤儿账号', retry_orphan: '重试孤儿账号',
  rotate_store_key: '轮换商城密钥', store_create_order: '商城下单', store_renew_order: '商城续费',
  store_revoke_order: '商城撤单', store_revoke_order_failed: '商城撤单失败',
  create_xui_panel: '添加 3X-UI 面板', delete_xui_panel: '删除 3X-UI 面板',
  xui_add_client: '3X-UI 添加客户端', xui_delete_client: '3X-UI 删除客户端', xui_reset_client_traffic: '3X-UI 重置流量',
  // 2026-11-06：补齐原来没收录的动作码（原来原样显示 change_server_ip 之类）。
  accept_server_fingerprint: '确认新 SSH 指纹', host_key_first_trust: '首次信任 SSH 主机密钥', test_server_ssh: '测试 SSH 连接',
  change_server_ip: '更换主机 IP', migrate_node: '迁移节点', resolve_interrupted_host_job: '结案中断的安装/迁移',
  set_server_billing: '设置到期与费用', set_subscription_revenue: '设置订阅月收入',
  close_expired_subscription: '结案到期订阅', close_expired_subscriptions_batch: '批量结案到期订阅',
  delete_subscription_incomplete: '删除订阅（回收不完整）', store_adopt_subscription: '商城接管手工订阅',
  reconcile_node_accounts: '巡检节点残留账号', probe_ip: '检测指定 IP',
  // 2026-11-06 之后新增的三类事件，不收录的话审计表里只能看到英文动作码。
  change_password_failed: '改密失败（当前密码错）', store_revoke_order_gone: '商城撤单（订阅已不存在）',
  renewal_revenue_skipped: '续费金额未记账',
  run_block_checks: '被墙检测', set_cn_probe: '设置国内探测点', set_auto_hide_nodes: '设置故障节点自动下线',
  run_schedule: '立即运行巡检', set_schedule: '设置定时巡检',
  set_delivery_domain: '设置交付域名', set_subscription_name: '设置订阅显示名称', set_store_signature: '设置商城签名',
  set_telegram_chat: '设置 Telegram Chat ID', telegram_control: 'Telegram 机器人操作',
  login_failed: '登录失败', forbidden_admin_only: '越权（需管理员）', forbidden_viewer_write: '越权（只读账号写操作）',
  forbidden_invalid_role: '越权（角色无效）',
  install_hy2: '一键安装节点', host_containers_read: '查看主机容器', host_projects_read: '查看主机 Compose 项目',
}

// portal_<动作>：节点门户操作（portalaction.go 按动词拼出的动作码）。
function auditLabel(action: string): string {
  if (AUDIT_LABELS[action]) return AUDIT_LABELS[action]
  if (action.startsWith('portal_')) return `节点操作 · ${action.slice('portal_'.length)}`
  return action
}

function auditTone(action: string): string {
  // forbidden_* 是越权被拒、login_failed 是登录失败：都应醒目（原来按 normal 显示，混在日常操作里）。
  if (/^(delete|revoke|store_revoke|forbidden_)|_failed$|_incomplete$/.test(action)) return 'bad'
  // 续费金额没记进报表、订阅已不存在还在撤单：都不是故障，但必须被看见。
  if (/skipped|_gone$/.test(action)) return 'warn'
  if (/password|credential|key|token|write_ops|terminal|fingerprint|first_trust/.test(action)) return 'warn'
  return 'normal'
}

function formatDateTime(iso: string | null): string {
  if (!iso) return '—'
  const date = new Date(iso)
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString('zh-CN', { hour12: false })
}

function formatRelative(iso: string | null): string {
  if (!iso) return '从未'
  const date = new Date(iso)
  const ts = date.getTime()
  if (Number.isNaN(ts)) return '—'
  const diff = Date.now() - ts
  const m = Math.floor(diff / 60000)
  if (m < 1) return '刚刚'
  if (m < 60) return `${m} 分钟前`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h} 小时前`
  const d = Math.floor(h / 24)
  if (d < 30) return `${d} 天前`
  return date.toLocaleDateString('zh-CN')
}

function loadSection(): SectionKey {
  try {
    return (localStorage.getItem(SECTION_STORE) as SectionKey) || 'overview'
  } catch {
    return 'overview'
  }
}

export function SettingsPage({ servers, session, onChanged, notify, appearance }: {
  servers: Server[]
  session: { email: string; role: string }
  onChanged: () => Promise<void> | void
  notify: (message: string) => void
  appearance?: React.ReactNode
}) {
  const isAdmin = session.role === 'admin'
  const [section, setSectionState] = useState<SectionKey>(loadSection)
  const [runtime, setRuntime] = useState<RuntimeStatus | null>(null)
  const [users, setUsers] = useState<UserAccount[]>([])
  const [audit, setAudit] = useState<AuditEntry[]>([])
  // 读取失败的列表名（2026-11-04）。原来失败一律当空列表：数据库抖一下，审计页显示「暂无审计记录」、
  // 用户页显示 0 个账号 —— 排查事故时会被读成「什么都没发生」。现在保留上次的数据并说明没读到。
  const [loadFailed, setLoadFailed] = useState<{ users?: string; audit?: string }>({})
  const [auditLimit, setAuditLimit] = useState(50)
  const [auditQuery, setAuditQuery] = useState('')
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirmNext, setConfirmNext] = useState('')
  const [busy, setBusy] = useState(false)
  const [addingUser, setAddingUser] = useState(false)
  const [draftUser, setDraftUser] = useState({ email: '', password: '', role: 'operator' as UserAccount['role'] })
  const [pwTarget, setPwTarget] = useState<UserAccount | null>(null)
  const [pwValue, setPwValue] = useState('')
  const [deleteTarget, setDeleteTarget] = useState<UserAccount | null>(null)
  const [savingRole, setSavingRole] = useState('')
  const mountedRef = useRef(true)
  const loadSeq = useRef(0)

  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      loadSeq.current += 1
    }
  }, [])

  const sections = useMemo(() => ([
    { key: 'overview', label: '概览', Icon: LayoutDashboard, show: true },
    { key: 'security', label: '账号安全', Icon: Lock, show: true },
    { key: 'users', label: '用户与角色', Icon: Users, show: isAdmin },
    { key: 'keys', label: '集成密钥', Icon: KeyRound, show: isAdmin },
    { key: 'telegram', label: 'Telegram', Icon: Send, show: isAdmin },
    { key: 'schedules', label: '定时巡检', Icon: Clock, show: isAdmin },
    { key: 'audit', label: '操作审计', Icon: ShieldCheck, show: isAdmin },
    { key: 'appearance', label: '外观', Icon: Palette, show: Boolean(appearance) },
  ] as { key: SectionKey; label: string; Icon: typeof Lock; show: boolean }[]).filter((s) => s.show), [isAdmin, appearance])

  // 存下来的分区对当前角色不可见（如 viewer 打开了 users）时退回概览。
  const active: SectionKey = sections.some((s) => s.key === section) ? section : 'overview'

  function setSection(key: SectionKey) {
    setSectionState(key)
    try { localStorage.setItem(SECTION_STORE, key) } catch { /* 个人便利，失败无妨 */ }
  }

  const load = useCallback(async () => {
    const seq = ++loadSeq.current
    const failed: { users?: string; audit?: string } = {}
    const why = (err: unknown) => (err instanceof Error ? err.message : '读取失败')
    const [status, accountList, auditList] = await Promise.all([
      api.runtime().catch(() => null),
      isAdmin ? api.users().catch((err) => { failed.users = why(err); return null }) : Promise.resolve([]),
      isAdmin ? api.auditLogs(auditLimit).catch((err) => { failed.audit = why(err); return null }) : Promise.resolve([]),
    ])
    if (!mountedRef.current || seq !== loadSeq.current) return
    setRuntime(status)
    if (accountList) setUsers(accountList)
    if (auditList) setAudit(auditList)
    setLoadFailed(failed)
  }, [isAdmin, auditLimit])

  useEffect(() => { void load() }, [load])

  async function changePassword() {
    if (busy) return
    if (next.length < 12) return notify('新密码至少 12 个字符')
    if (next !== confirmNext) return notify('两次输入的新密码不一致')
    setBusy(true)
    try {
      const res = await api.changePassword(current, next)
      if (!mountedRef.current) return
      setCurrent(''); setNext(''); setConfirmNext('')
      notify(res.token ? '密码已更新，其它设备上的登录已失效' : '密码已更新，请重新登录')
      await load()
    } catch (err) {
      if (mountedRef.current) notify(err instanceof Error ? `修改失败：${err.message}` : '修改失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  async function changeRole(user: UserAccount, role: UserAccount['role']) {
    if (role === user.role || savingRole) return
    // 2026-11-06：设为管理员先确认。下拉框一改即生效：误选一次就把账号、密钥、商城对接、主机凭据的全部权限交了出去。
    if (role === 'admin' && !await askConfirm(`把 ${user.email} 设为管理员？

管理员可以管理账号、集成密钥、商城对接与主机凭据，并能删除节点和订阅。`, { confirmText: '设为管理员', danger: true })) return
    if (!mountedRef.current) return
    setSavingRole(user.id)
    try {
      await api.updateUser(user.id, role)
      if (!mountedRef.current) return
      notify(`已更新角色：${user.email} → ${ROLE_META[role].label}`)
      await Promise.all([load(), onChanged()])
    } catch (err) {
      if (mountedRef.current) notify(err instanceof Error ? `角色更新失败：${err.message}` : '角色更新失败')
    } finally {
      if (mountedRef.current) setSavingRole('')
    }
  }

  async function resetPassword() {
    if (!pwTarget || busy) return
    if (pwValue.length < 12) return notify('新密码至少 12 个字符')
    setBusy(true)
    try {
      await api.resetUserPassword(pwTarget.id, pwValue)
      if (!mountedRef.current) return
      notify(`已重置密码：${pwTarget.email}`)
      setPwTarget(null); setPwValue('')
      await load()
    } catch (err) {
      if (mountedRef.current) notify(err instanceof Error ? `重置失败：${err.message}` : '重置失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  async function removeUser() {
    if (!deleteTarget || busy) return
    const target = deleteTarget
    setDeleteTarget(null)
    setBusy(true)
    try {
      await api.deleteUser(target.id)
      if (!mountedRef.current) return
      notify(`已删除账号：${target.email}`)
      await Promise.all([load(), onChanged()])
    } catch (err) {
      if (mountedRef.current) notify(err instanceof Error ? `删除失败：${err.message}` : '删除失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  async function createUser() {
    if (busy) return
    const email = draftUser.email.trim()
    if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) return notify('请输入有效的邮箱地址')
    if (draftUser.password.length < 12) return notify('密码至少 12 个字符')
    setBusy(true)
    try {
      await api.createUser({ ...draftUser, email })
      if (!mountedRef.current) return
      notify(`已创建账号：${email}`)
      setAddingUser(false)
      setDraftUser({ email: '', password: '', role: 'operator' })
      await Promise.all([load(), onChanged()])
    } catch (err) {
      if (mountedRef.current) notify(err instanceof Error ? `创建失败：${err.message}` : '创建失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  const filteredAudit = useMemo(() => {
    const q = auditQuery.trim().toLowerCase()
    if (!q) return audit
    return audit.filter((e) => [e.action, auditLabel(e.action), e.actor, e.resource_type, e.resource_id, e.ip]
      .some((v) => (v ?? '').toLowerCase().includes(q)))
  }, [audit, auditQuery])

  const credentialed = runtime?.servers_with_credential ?? servers.filter((s) => s.has_credential).length
  const sampleAgeMin = runtime?.last_sample_at ? (Date.now() - new Date(runtime.last_sample_at).getTime()) / 60000 : null
  const sampleStale = sampleAgeMin !== null && runtime ? sampleAgeMin > Math.max(3, (runtime.collector_interval * 4) / 60) : false
  const passwordMismatch = confirmNext.length > 0 && next !== confirmNext

  return (
    <>
      <div className="page-heading">
        <div>
          <div className="eyebrow"><span className="eyebrow-line" />SETTINGS / 05</div>
          <h1>系统设置<span className="heading-period">.</span></h1>
          <p className="page-subtitle">账号与权限、集成密钥、运行状态与操作审计</p>
        </div>
        <div className="heading-actions">
          <button className="button button-quiet" onClick={() => { void load(); notify('已刷新') }}><Activity size={15} />刷新</button>
        </div>
      </div>

      <div className="settings-layout">
        <nav className="settings-nav" aria-label="设置分区">
          <div className="settings-me">
            <div className="settings-avatar">{session.email.slice(0, 1).toUpperCase()}</div>
            <div>
              <strong title={session.email}>{session.email}</strong>
              <span>{ROLE_META[session.role as UserAccount['role']]?.label ?? session.role}</span>
            </div>
          </div>
          {sections.map(({ key, label, Icon }) => (
            <button key={key} className={`settings-nav-item${active === key ? ' active' : ''}`} onClick={() => setSection(key)} aria-current={active === key ? 'page' : undefined}>
              <Icon size={16} /><span>{label}</span>
            </button>
          ))}
        </nav>

        <div className="settings-content">
          {active === 'overview' && (
            <>
              <section className="metrics-grid settings-kpis" aria-label="运行概览">
                <div className="metric-card"><div className="metric-head"><span>主机 / 节点</span><ServerIcon size={17} /></div><div className="metric-body"><strong>{runtime?.servers ?? '—'}</strong><span className="metric-unit">/ {runtime?.nodes ?? '—'} 节点</span></div><div className="metric-foot">{credentialed} 台主机已配置 SSH 凭据</div></div>
                <div className="metric-card"><div className="metric-head"><span>排队中任务</span><Cpu size={17} /></div><div className="metric-body"><strong>{runtime?.queued_tasks ?? '—'}</strong></div><div className="metric-foot">执行器每 {runtime?.executor_interval ?? '—'} 秒取一次任务</div></div>
                <div className="metric-card"><div className="metric-head"><span>最近采样</span><Clock size={17} /></div><div className="metric-body"><strong className={sampleStale ? 'lat-bad' : ''}>{formatRelative(runtime?.last_sample_at ?? null)}</strong></div><div className="metric-foot">{sampleStale ? <span className="metric-warn">采样滞后，检查采集器或主机连通性</span> : `采集间隔 ${runtime?.collector_interval ?? '—'} 秒`}</div></div>
                <div className="metric-card"><div className="metric-head"><span>会话有效期</span><Lock size={17} /></div><div className="metric-body"><strong>{runtime?.jwt_ttl_minutes ?? '—'}</strong><span className="metric-unit">分钟</span></div><div className="metric-foot">凭据与密钥以 AES-256-GCM 加密存储</div></div>
              </section>

              <section className="panel settings-panel">
                <div className="section-head"><div><div className="section-kicker">DATA SOURCE</div><h2>数据来源与授权</h2></div></div>
                <div className="settings-notes">
                  <div className="drawer-notice"><Database size={17} /><span><strong>{servers.some((s) => s.has_credential) ? 'SSH 真实采集' : '尚未接入真实主机'}</strong><small>指标来自目标主机 /proc 与 df 的真实读数；未配置凭据的主机显示未知，不会编造数值。</small></span></div>
                  <div className="drawer-notice"><KeyRound size={17} /><span><strong>写操作按主机授权</strong><small>需同时开启 allow_write_ops 且给出 unit 白名单；越界请求会被拒绝且不执行任何命令。</small></span></div>
                  <div className="drawer-notice"><Eye size={17} /><span><strong>敏感值从不回显</strong><small>SSH 凭据、API 密钥、Webhook 令牌保存后只显示是否已配置，接口不会返回明文。</small></span></div>
                </div>
              </section>
            </>
          )}

          {active === 'security' && (
            <section className="panel settings-panel">
              <div className="section-head"><div><div className="section-kicker">SECURITY</div><h2>修改登录密码</h2></div></div>
              <div className="settings-form">
                <label className="form-field"><span>当前密码</span><input type="password" value={current} onChange={(e) => setCurrent(e.target.value)} autoComplete="current-password" /></label>
                <label className="form-field"><span>新密码（至少 12 个字符）</span><input type="password" value={next} onChange={(e) => setNext(e.target.value)} autoComplete="new-password" /></label>
                <label className="form-field"><span>再次输入新密码</span><input type="password" value={confirmNext} onChange={(e) => setConfirmNext(e.target.value)} autoComplete="new-password" aria-invalid={passwordMismatch} /></label>
                <div className="settings-pw-hints">
                  <span className={next.length >= 12 ? 'ok' : ''}>✓ 至少 12 个字符</span>
                  <span className={confirmNext.length > 0 && !passwordMismatch ? 'ok' : passwordMismatch ? 'bad' : ''}>{passwordMismatch ? '✕ 两次输入不一致' : '✓ 两次输入一致'}</span>
                </div>
                <div><button className="button button-primary" disabled={busy || !current || next.length < 12 || next !== confirmNext} onClick={changePassword}><KeyRound size={15} />更新密码</button></div>
                <p className="form-note">种子管理员密码只在账号首次创建时写入，重启服务不会覆盖你改过的密码。</p>
              </div>
            </section>
          )}
          {active === 'security' && <RecentLogins />}

          {active === 'users' && isAdmin && (
            <section className="panel settings-panel">
              <div className="section-head">
                <div><div className="section-kicker">ACCOUNTS <span>·</span> {users.length}</div><h2>用户与角色</h2></div>
                <button className="button button-primary" onClick={() => setAddingUser(true)}><Plus size={15} />新建账号</button>
              </div>
              {loadFailed.users && <p className="form-note lat-bad" role="alert">⚠ 账号列表读取失败（{loadFailed.users}），下面可能不是最新情况。</p>}
              <div className="settings-roles">
                {(Object.keys(ROLE_META) as UserAccount['role'][]).map((role) => (
                  <div key={role} className={`settings-role role-${role}`}>
                    <strong>{ROLE_META[role].label}<small> · {users.filter((u) => u.role === role).length}</small></strong>
                    <span>{ROLE_META[role].desc}</span>
                  </div>
                ))}
              </div>
              <div className="server-table-wrap"><table className="server-table">
                <thead><tr><th>账号</th><th>角色</th><th>最近登录</th><th aria-label="操作" /></tr></thead>
                <tbody>{users.map((user) => {
                  const isSelf = user.email === session.email
                  return (
                    <tr key={user.id}>
                      <td><div className="host-cell"><div className="host-icon"><UserCog size={15} /></div><div><strong>{user.email}</strong>{isSelf && <span>当前登录账号</span>}</div></div></td>
                      <td>
                        <select className="role-select" value={user.role} disabled={savingRole === user.id || isSelf} title={isSelf ? '不能修改自己的角色' : undefined} onChange={(e) => changeRole(user, e.target.value as UserAccount['role'])} aria-label={`设置 ${user.email} 的角色`}>
                          {(Object.keys(ROLE_META) as UserAccount['role'][]).map((r) => <option key={r} value={r}>{ROLE_META[r].label}</option>)}
                        </select>
                      </td>
                      <td><span className="traffic-sub" title={formatDateTime(user.last_login_at)}>{formatRelative(user.last_login_at)}</span></td>
                      <td><div className="row-actions">
                        <button className="row-more" aria-label={`重置 ${user.email} 的密码`} title={isSelf ? '修改自己的密码请用上方「修改密码」' : '重置登录密码'} disabled={isSelf} onClick={() => { setPwValue(''); setPwTarget(user) }}><KeyRound size={15} /></button>
                        <button className="row-more" aria-label={`删除账号 ${user.email}`} title={isSelf ? '不能删除当前登录账号' : '删除账号'} disabled={isSelf} onClick={() => setDeleteTarget(user)}><Trash2 size={15} /></button>
                      </div></td>
                    </tr>
                  )
                })}</tbody>
              </table></div>
            </section>
          )}

          {active === 'keys' && isAdmin && <><PurityKeysCard notify={notify} /><DeliveryDomainCard notify={notify} /><StoreSignatureCard notify={notify} /></>}
          {active === 'telegram' && isAdmin && <TelegramCard notify={notify} />}
          {active === 'schedules' && isAdmin && <SchedulesCard notify={notify} />}

          {active === 'audit' && isAdmin && (
            <section className="panel settings-panel">
              <div className="section-head">
                <div><div className="section-kicker">AUDIT <span>·</span> {audit.length}</div><h2>操作审计</h2></div>
                <label className="monitor-search"><Search size={14} /><input value={auditQuery} onChange={(e) => setAuditQuery(e.target.value)} placeholder="搜索操作 / 操作者 / IP…" aria-label="搜索审计" /></label>
              </div>
              {loadFailed.audit && <p className="form-note lat-bad" role="alert">⚠ 审计记录读取失败（{loadFailed.audit}），下面可能不是最新情况。</p>}
              {/* 2026-11-06：搜索是纯前端过滤，列表只加载了最近 auditLimit 条。已加载满额时不能说「没有匹配」——
                  那会被读成「查无此事」，排查「谁删了这台节点」正好查到第 51 条以外就断了。 */}
              {filteredAudit.length === 0 && loadFailed.audit && audit.length === 0 ? null : filteredAudit.length === 0 ? (
                <div className="empty-state"><ShieldCheck size={20} /><strong>{audit.length ? '已加载的记录里没有匹配' : '暂无审计记录'}</strong><span>{audit.length >= auditLimit ? `只搜索了最近 ${audit.length} 条；更早的记录尚未加载，点下方「加载更多」继续。` : '写操作成功后会在此留痕。'}</span></div>
              ) : (
                <ol className="settings-audit">
                  {filteredAudit.map((entry) => (
                    <li key={entry.id} className={`tone-${auditTone(entry.action)}`}>
                      <i className="settings-audit-dot" />
                      <div className="settings-audit-main">
                        <strong title={entry.action}>{auditLabel(entry.action)}</strong>
                        <span>{entry.actor || '系统'}{entry.ip ? ` · ${entry.ip}` : ''}{entry.resource_type ? ` · ${entry.resource_type}${entry.resource_id ? ` ${entry.resource_id.slice(0, 8)}` : ''}` : ''}</span>
                      </div>
                      <time dateTime={entry.created_at} title={formatDateTime(entry.created_at)}>{formatRelative(entry.created_at)}</time>
                    </li>
                  ))}
                </ol>
              )}
              {audit.length >= auditLimit && auditLimit < 200 && (
                <div><button className="button button-quiet" onClick={() => setAuditLimit((n) => Math.min(200, n + 50))}>加载更多</button></div>
              )}
            </section>
          )}

          {active === 'appearance' && appearance && (
            <section className="panel settings-panel">
              <div className="section-head"><div><div className="section-kicker">APPEARANCE</div><h2>外观设置</h2></div></div>
              <div className="settings-form">{appearance}</div>
            </section>
          )}
        </div>
      </div>

      {addingUser && (
        <Modal title="新建账号" kicker="CREATE USER" onClose={() => setAddingUser(false)}>
          <div style={{ display: 'grid', gap: 12 }}>
            <label className="form-field"><span>邮箱</span><input value={draftUser.email} onChange={(e) => setDraftUser({ ...draftUser, email: e.target.value })} placeholder="operator@example.com" /></label>
            <label className="form-field"><span>密码（至少 12 个字符）</span><input type="password" value={draftUser.password} onChange={(e) => setDraftUser({ ...draftUser, password: e.target.value })} autoComplete="new-password" /></label>
            <label className="form-field"><span>角色</span>
              <select value={draftUser.role} onChange={(e) => setDraftUser({ ...draftUser, role: e.target.value as UserAccount['role'] })}>
                {(Object.keys(ROLE_META) as UserAccount['role'][]).map((r) => <option key={r} value={r}>{ROLE_META[r].label} — {ROLE_META[r].desc}</option>)}
              </select>
            </label>
            <div className="modal-actions"><button className="button button-quiet" onClick={() => setAddingUser(false)}>取消</button><button className="button button-primary" disabled={busy} onClick={createUser}>创建</button></div>
          </div>
        </Modal>
      )}

      {pwTarget && (
        <Modal title={`重置密码 · ${pwTarget.email}`} kicker="RESET PASSWORD" onClose={() => setPwTarget(null)}>
          <div style={{ display: 'grid', gap: 12 }}>
            <label className="form-field"><span>新密码（至少 12 个字符）</span><input type="password" value={pwValue} onChange={(e) => setPwValue(e.target.value)} autoComplete="new-password" /></label>
            {/* 2026-11-04：后端重置密码会 token_version+1，所有已登录会话立即失效；原文案说的正相反。 */}
            <p className="form-note">该账号的旧密码与所有已登录的会话立即失效，需要用新密码重新登录。</p>
            <div className="modal-actions"><button className="button button-quiet" onClick={() => setPwTarget(null)}>取消</button><button className="button button-primary" disabled={busy || pwValue.length < 12} onClick={resetPassword}>重置密码</button></div>
          </div>
        </Modal>
      )}

      {deleteTarget && (
        <Modal title="删除账号" kicker="CONFIRM DELETE" onClose={() => setDeleteTarget(null)}>
          <p className="form-note">即将删除账号 <strong>{deleteTarget.email}</strong>（{ROLE_META[deleteTarget.role]?.label ?? deleteTarget.role}）。删除后该账号无法登录，其历史审计记录保留。</p>
          <div className="modal-actions"><button className="button button-quiet" onClick={() => setDeleteTarget(null)}>取消</button><button className="button button-danger" disabled={busy} onClick={removeUser}>{busy ? '删除中…' : '确认删除'}</button></div>
        </Modal>
      )}
    </>
  )
}
