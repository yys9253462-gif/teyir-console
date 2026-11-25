import { useCallback, useEffect, useRef, useState } from 'react'
import { Loader2, Play, Power, RefreshCw, RotateCw, Square, Zap, ZapOff } from 'lucide-react'
import { api, isAdminSession, loadSession, ADMIN_ONLY_HINT, type Server, type ServiceOperation, type ServiceUnitStatus } from '../lib/api'

// ServiceControlPanel renders the live systemd state of a host's whitelisted
// units and lets the operator drive them.
//
// Design notes:
//  - Status is read on mount and on demand. It is NOT polled on a timer: each
//    read costs an SSH round trip to the target host, and this panel is one of
//    several things on screen. The user refreshes when they care.
//  - Write verbs are queued through the task API, exactly like every other
//    mutation, so they land in the audit log. The panel therefore cannot show
//    the new state immediately — the executor picks the task up within one
//    cycle (20s by default). That is stated in the UI instead of being hidden.
//  - Actions are disabled while a dispatch is in flight to prevent double-fires.

const ACTION_META: Record<
  Extract<ServiceOperation, 'start' | 'stop' | 'restart' | 'reload' | 'enable' | 'disable'>,
  { label: string; icon: React.ReactNode; tone: 'primary' | 'quiet'; danger: boolean }
> = {
  start: { label: '启动', icon: <Play size={14} />, tone: 'primary', danger: false },
  stop: { label: '停止', icon: <Square size={14} />, tone: 'quiet', danger: true },
  restart: { label: '重启', icon: <RotateCw size={14} />, tone: 'quiet', danger: true },
  reload: { label: '重载', icon: <RefreshCw size={14} />, tone: 'quiet', danger: false },
  enable: { label: '开机自启', icon: <Zap size={14} />, tone: 'quiet', danger: false },
  disable: { label: '取消自启', icon: <ZapOff size={14} />, tone: 'quiet', danger: true },
}

// The order the buttons appear in, grouped by intent: bring up, take down,
// re-apply, then boot policy.
const ACTION_ORDER: Array<keyof typeof ACTION_META> = ['start', 'restart', 'reload', 'stop', 'enable', 'disable']

// activeTone maps systemd's ActiveState to the existing status colour classes so
// the badge matches the rest of the console (green = healthy, amber = in flux,
// red = failed).
function activeTone(active: string): string {
  switch (active) {
    case 'active':
      return 'online'
    case 'activating':
    case 'deactivating':
    case 'reloading':
      return 'warning'
    case 'failed':
      return 'offline'
    default:
      return 'unknown'
  }
}

const ACTIVE_TEXT: Record<string, string> = {
  active: '运行中',
  inactive: '已停止',
  failed: '失败',
  activating: '启动中',
  deactivating: '停止中',
  reloading: '重载中',
  unknown: '未知',
}

const ENABLED_TEXT: Record<string, string> = {
  enabled: '自启',
  'enabled-runtime': '自启(临时)',
  disabled: '不自启',
  static: '静态',
  masked: '已屏蔽',
  indirect: '间接',
  unknown: '未知',
}

export function ServiceControlPanel({ server, notify }: {
  server: Server
  notify: (message: string) => void
}) {
  const [units, setUnits] = useState<ServiceUnitStatus[]>([])
  const [note, setNote] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [busyUnit, setBusyUnit] = useState<string | null>(null)
  // Which write verbs are awaiting an explicit confirm click, keyed by unit+verb.
  const [confirming, setConfirming] = useState<string | null>(null)

  // 2026-11-06：只采用最后一次请求的结果。切换主机时上一台主机的服务列表晚回来会显示在当前主机下，
  // 「重启 / 停止」按钮随即对当前主机下发另一台主机的服务名。
  const loadSeq = useRef(0)
  const mountedRef = useRef(true)
  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      loadSeq.current += 1
    }
  }, [])
  const load = useCallback(async () => {
    const seq = ++loadSeq.current
    setLoading(true)
    try {
      const response = await api.serverServices(server.id)
      if (!mountedRef.current || seq !== loadSeq.current) return
      setUnits(response.units ?? [])
      setNote(response.note ?? response.error ?? null)
    } catch (err) {
      if (!mountedRef.current || seq !== loadSeq.current) return
      setUnits([])
      setNote(err instanceof Error ? `读取服务状态失败：${err.message}` : '读取服务状态失败')
    } finally {
      if (mountedRef.current && seq === loadSeq.current) setLoading(false)
    }
  }, [server.id])

  useEffect(() => {
    loadSeq.current++ // 作废上一台主机还在途的请求
    setUnits([])
    setNote(null)
    setConfirming(null)
    if (server.has_credential) {
      void load()
    } else {
      setNote('该主机未配置 SSH 凭据，无法读取服务状态')
    }
    return () => { loadSeq.current++ }
  }, [server.id, server.has_credential, load])

  async function dispatch(unit: string, action: keyof typeof ACTION_META) {
    const key = `${unit}:${action}`
    const meta = ACTION_META[action]
    // Two-step confirmation for the destructive verbs. This is a UI courtesy on
    // top of the server-side gate (X-Confirm-Dangerous + allow_write_ops +
    // whitelist), not a replacement for it.
    if (meta.danger && confirming !== key) {
      setConfirming(key)
      return
    }
    setConfirming(null)
    setBusyUnit(unit)
    try {
      await api.createTask({
        server_id: server.id,
        operation_type: action,
        parameters: { source: 'service-panel', unit },
      })
      notify(`已下发「${meta.label} ${unit}」· ${server.name}（执行器约 20 秒内领取）`)
    } catch (err) {
      notify(err instanceof Error ? `下发失败：${err.message}` : '下发失败')
    } finally {
      if (mountedRef.current) setBusyUnit(null)
    }
  }

  // 写类服务控制任务由后端限制为仅管理员可执行，普通运维与只读账号只可查看状态。
  const viewer = loadSession()?.role === 'viewer'
  const admin = isAdminSession()
  const writable = server.allow_write_ops && admin

  return (
    <div className="service-panel">
      <div className="service-panel-head">
        <div>
          <div className="section-kicker">SERVICE CONTROL</div>
          <strong>服务控制</strong>
        </div>
        <button className="button button-quiet" onClick={() => void load()} disabled={loading || !server.has_credential}>
          {loading ? <Loader2 size={14} className="spin" /> : <RefreshCw size={14} />}
          刷新状态
        </button>
      </div>

      {viewer && <p className="form-note">只读账号只能查看服务状态。</p>}
      {!viewer && !admin && <p className="form-note">{ADMIN_ONLY_HINT}，当前账号仅可查看服务状态。</p>}
      {admin && !server.allow_write_ops && (
        <p className="form-note service-warn">
          <Power size={13} />
          <span>该主机未开启写操作。可以查看状态，但启动/停止/重启等操作会被执行器拒绝。请先在上方开启写操作并确认 unit 白名单。</span>
        </p>
      )}

      {note && <p className="form-note">{note}</p>}

      {units.length === 0 && !note && !loading && (
        <div className="empty-state compact"><Power size={18} /><strong>没有可管理的服务</strong><span>在写操作设置中添加 unit 后即可在此控制。</span></div>
      )}

      {units.map((unit) => (
        <div className="service-row" key={unit.unit}>
          <div className="service-identity">
            <strong>{unit.unit}</strong>
            <div className="service-badges">
              <span className={`status-pill ${activeTone(unit.active)}`}>{ACTIVE_TEXT[unit.active] ?? unit.active}</span>
              <span className="service-badge-muted">{ENABLED_TEXT[unit.enabled] ?? unit.enabled}</span>
              {unit.since && !unit.since.startsWith('n/a') && <span className="service-badge-muted" title="上次状态变更时间">{unit.since}</span>}
            </div>
            {unit.error && <small className="service-error">{unit.error}</small>}
          </div>
          <div className="service-actions">
            {ACTION_ORDER.map((action) => {
              const meta = ACTION_META[action]
              const key = `${unit.unit}:${action}`
              const busy = busyUnit === unit.unit
              const awaiting = confirming === key
              return (
                <button
                  key={action}
                  className={`button ${awaiting ? 'button-danger' : meta.tone === 'primary' ? 'button-primary' : 'button-quiet'} button-compact`}
                  disabled={busy || !writable || loading}
                  onClick={() => void dispatch(unit.unit, action)}
                  onBlur={() => { if (awaiting) setConfirming(null) }}
                  title={viewer ? '只读账号不能操作服务' : !admin ? ADMIN_ONLY_HINT : !server.allow_write_ops ? '该主机未开启写操作' : `systemctl ${action} ${unit.unit}`}
                >
                  {busy ? <Loader2 size={13} className="spin" /> : meta.icon}
                  {awaiting ? '确认?' : meta.label}
                </button>
              )
            })}
          </div>
        </div>
      ))}

      {units.length > 0 && (
        <p className="form-note">
          状态来自目标机 `systemctl show`，点「刷新状态」重新读取。下发的操作先入队，执行器领取后才会生效，因此这里不会立即变化。
        </p>
      )}
    </div>
  )
}
