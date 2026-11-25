import { useRef, useState } from 'react'
import { Activity, Ban, Clock3, RotateCcw, Send, Server as ServerIcon } from 'lucide-react'
import { api, isAdminSession, loadSession, ADMIN_ONLY_HINT, type OperationTask, type Server, type ServiceOperation, type TaskLog } from '../lib/api'
import { Modal } from './Modal'
import { OrphanAccounts } from './OrphanAccounts'
import { HostJobHistory } from './HostJobHistory'
import { askConfirm } from './ConfirmHost'

// Every verb the executor implements and that mutates the host. Read-only
// operations (health_check, logs) are absent on purpose.
const WRITE_OPS = new Set<string>(['restart', 'start', 'stop', 'reload', 'enable', 'disable', 'system_preset'])
// 与后端 supportedOperations 一致。历史行可能是执行器从未实现的类型（deploy 等），
// 后端拒绝重试它们，这里也不给重试按钮（2026-11-03）。
const RETRYABLE_OPS = new Set<string>(['health_check', 'logs', ...WRITE_OPS])

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

// Task rows may be historical, so labels resolve defensively.
function operationLabel(operation: string): string {
  return opTypeText[operation as ServiceOperation] ?? operation
}

// The verbs this page offers as dispatch buttons, in the order they appear.
const DISPATCH_ACTIONS: ServiceOperation[] = ['health_check', 'logs', 'restart', 'start', 'stop', 'reload']

const taskStatusText: Record<OperationTask['status'], string> = {
  queued: '排队中',
  running: '执行中',
  success: '已完成',
  failed: '失败',
  cancelled: '已取消',
}

const taskStatusClass: Record<OperationTask['status'], string> = {
  queued: 'offline',
  running: 'warning',
  success: 'online',
  failed: 'offline',
  cancelled: 'offline',
}

function formatDateTime(iso: string): string {
  return new Date(iso).toLocaleString('zh-CN', { hour12: false })
}

const VIEWER_HINT = '只读账号不能下发、取消或重试任务'

export function TasksPage({ tasks, servers, onChanged, notify }: {
  tasks: OperationTask[]
  servers: Server[]
  onChanged: () => Promise<void> | void
  notify: (message: string) => void
}) {
  const [openTask, setOpenTask] = useState<OperationTask | null>(null)
  const [logs, setLogs] = useState<TaskLog[]>([])
  const [logError, setLogError] = useState<string | null>(null)
  const [logsLoading, setLogsLoading] = useState(false)
  // 2026-11-06：只读账号的所有 POST（含只读任务的下发、取消、重试）后端一律 403；写类任务仅限管理员。
  const viewer = loadSession()?.role === 'viewer'
  const admin = isAdminSession()
  const [statusFilter, setStatusFilter] = useState('全部')
  const [dispatching, setDispatching] = useState(false)
  const [cancelingId, setCancelingId] = useState('')
  const [retrying, setRetrying] = useState(false)
  const [targetId, setTargetId] = useState('')
  const [unit, setUnit] = useState('hysteria-server')
  const [pendingRetry, setPendingRetry] = useState<OperationTask | null>(null)

  const target = servers.find((server) => server.id === targetId) ?? servers.find((server) => server.has_credential) ?? servers[0]

  async function cancel(task: OperationTask) {
    if (cancelingId) return
    setCancelingId(task.id)
    try {
      await api.cancelTask(task.id)
      notify(`已取消任务 ${task.id.slice(0, 8)}`)
      await onChanged()
    } catch (err) {
      notify(err instanceof Error ? `取消失败：${err.message}` : '取消失败')
    } finally {
      setCancelingId('')
    }
  }

  async function runRetry(task: OperationTask, dangerous: boolean) {
    setRetrying(true)
    try {
      const created = await api.retryTask(task.id, dangerous)
      notify(`已重新入队任务 ${created.id.slice(0, 8)}（${operationLabel(task.operation_type)}）`)
      await onChanged()
    } catch (err) {
      notify(err instanceof Error ? `重试失败：${err.message}` : '重试失败')
    } finally {
      setRetrying(false)
    }
  }

  async function confirmRetry() {
    if (!pendingRetry || retrying) return
    const task = pendingRetry
    setPendingRetry(null)
    await runRetry(task, WRITE_OPS.has(task.operation_type))
  }

  async function dispatch(operation: ServiceOperation) {
    if (!target) {
      notify('请先添加一台主机')
      return
    }
    // 2026-11-04：会改动主机的动作（停止 / 重启 / 启动 / 重载）先确认。原来一点即下发，
    // api 层会自动带上 X-Confirm-Dangerous —— 后端要求的「人确认过」形同虚设，
    // 选错主机点一下「停止服务」，这台机器上的买家就全部断线。重试路径早就有确认弹窗，这里补上。
    const serviceUnit = unit.trim() || 'hysteria-server'
    if (WRITE_OPS.has(operation) && !await askConfirm(`确认对 ${target.name} 执行「${operationLabel(operation)}」？

服务：${serviceUnit}。正在使用这台主机的客户端可能会断线。`, { danger: operation === 'stop' })) return
    setDispatching(true)
    try {
      await api.createTask({ server_id: target.id, operation_type: operation, parameters: { source: 'console', unit: serviceUnit } })
      notify(`已下发 ${operationLabel(operation)} · ${target.name}（约 20 秒内被领取）`)
      await onChanged()
    } catch (err) {
      notify(err instanceof Error ? `下发失败：${err.message}` : '下发失败')
    } finally {
      setDispatching(false)
    }
  }

  // 当前展开的任务 id：慢响应回来时据此丢弃过期结果（2026-11-04）。原来先点 A 再点 B，
  // A 的日志晚到就会显示在 B 的标题下 —— 排查「为什么重启失败」时读到的是另一条任务的输出。
  const logsFor = useRef<string | null>(null)

  async function toggleLogs(task: OperationTask) {
    // 2026-11-06：任务日志是 root 命令输出，后端现在只对管理员 / 运维开放；只读账号点开只会拿 403，
    // 直接在这里说清楚，而不是让用户看到一句「日志读取失败：任务日志含主机内部信息…」。
    if (viewer) {
      notify('只读账号不能查看任务日志（内容是主机上的 root 命令输出）')
      return
    }
    setLogError(null)
    if (openTask?.id === task.id) {
      logsFor.current = null
      setOpenTask(null)
      setLogs([])
      setLogsLoading(false)
      return
    }
    logsFor.current = task.id
    setOpenTask(task)
    setLogs([])
    setLogsLoading(true)
    try {
      const rows = await api.taskLogs(task.id)
      if (logsFor.current === task.id) setLogs(rows)
    } catch (err) {
      if (logsFor.current === task.id) setLogError(err instanceof Error ? err.message : '日志读取失败')
    } finally {
      if (logsFor.current === task.id) setLogsLoading(false)
    }
  }

  const counts = {
    queued: tasks.filter((t) => t.status === 'queued').length,
    running: tasks.filter((t) => t.status === 'running').length,
    success: tasks.filter((t) => t.status === 'success').length,
    failed: tasks.filter((t) => t.status === 'failed').length,
  }
  const visibleTasks = tasks.filter((task) => statusFilter === '全部' || taskStatusText[task.status] === statusFilter)
  return (
    <>
      <div className="page-heading"><div><div className="eyebrow"><span className="eyebrow-line" />AUTOMATION / 03</div><h1>运维任务<span className="heading-period">.</span></h1><p className="page-subtitle">任务由执行器经 SSH 真实执行；只读操作直接运行，写操作需按主机授权</p></div></div>
      <section className="metrics-grid" aria-label="任务指标">
        <MetricCard label="任务总数" value={tasks.length.toString()} unit="条" icon={<Activity size={17} />} tone="green" foot={<span>最近 100 条记录</span>} />
        <MetricCard label="排队中" value={counts.queued.toString()} unit="条" icon={<Clock3 size={17} />} tone="neutral" foot={<span>等待执行</span>} />
        <MetricCard label="已完成" value={counts.success.toString()} unit="条" icon={<Activity size={17} />} tone="green" foot={<span>执行成功</span>} />
        <MetricCard label="失败" value={counts.failed.toString()} unit="条" icon={<Activity size={17} />} tone="amber" foot={<span>需要排查</span>} />
      </section>
      <section className="panel servers-panel">
        <div className="section-head servers-head"><div><div className="section-kicker">TASK QUEUE <span>·</span> {tasks.length.toString().padStart(2, '0')} ITEMS</div><h2>任务记录 <span className="count-chip">{tasks.length.toString().padStart(2, '0')}</span></h2></div>
          <div className="heading-actions dispatch-tools">
            <label className="filter-select"><ServerIcon size={14} /><select value={target?.id ?? ''} onChange={(event) => setTargetId(event.target.value)} aria-label="目标主机">{servers.length === 0 && <option value="">无主机</option>}{servers.map((server) => <option key={server.id} value={server.id}>{server.name}{server.has_credential ? '' : '（无凭据）'}</option>)}</select></label>
            <label className="filter-select unit-field"><span className="unit-label">unit</span><input value={unit} onChange={(event) => setUnit(event.target.value)} spellCheck={false} aria-label="systemd 服务名" placeholder="hysteria-server" /></label>
            {DISPATCH_ACTIONS.map((action) => {
              const isWrite = WRITE_OPS.has(action)
              const blocked = viewer || (isWrite && !admin)
              return (
              <button
                key={action}
                className="button button-quiet"
                disabled={dispatching || !target || blocked}
                // Write verbs need admin role and host write ops enabled; warn before
                // sending rather than letting the backend reject after confirmation.
                title={viewer ? VIEWER_HINT : isWrite && !admin ? ADMIN_ONLY_HINT : isWrite ? '写操作：目标主机需已开启写操作，且 unit 在白名单内' : action === 'logs' ? '只读操作：只能查看 hysteria-server 或该主机白名单内服务的日志' : '只读操作'}
                onClick={() => dispatch(action)}
              >
                <Send size={14} />下发{operationLabel(action)}
              </button>
              )
            })}
          </div>
        </div>
        <div className="table-tools"><label className="filter-select"><Clock3 size={14} /><select value={statusFilter} onChange={(event) => setStatusFilter(event.target.value)} aria-label="按任务状态筛选"><option>全部</option><option>排队中</option><option>执行中</option><option>已完成</option><option>失败</option><option>已取消</option></select></label><span className="traffic-sub">点击任务行展开执行日志</span></div>
        {visibleTasks.length === 0 ? (
          <div className="empty-state"><Activity size={20} /><strong>{tasks.length === 0 ? '暂无任务' : '没有符合筛选的任务'}</strong><span>可在主机详情或上方按钮下发只读任务。</span></div>
        ) : (
          <div className="server-table-wrap"><table className="server-table">
            <thead><tr><th>任务 / 主机</th><th>操作类型</th><th>状态</th><th>创建时间</th><th>耗时</th><th>结果</th><th></th></tr></thead>
            <tbody>
              {visibleTasks.map((task) => (
                <tr key={task.id} onClick={() => toggleLogs(task)} tabIndex={0} onKeyDown={(event) => event.key === 'Enter' && toggleLogs(task)} style={{ cursor: 'pointer' }}>
                  <td><div className="host-cell"><div className={`task-icon ${task.status}`}><Activity size={13} /></div><div><strong>{task.id.slice(0, 8)}</strong><span>{task.server_name}</span></div></div></td>
                  <td><span className="traffic-value">{operationLabel(task.operation_type)}</span></td>
                  <td><span className={`status-badge ${taskStatusClass[task.status]}`}><i />{taskStatusText[task.status]}</span></td>
                  <td><span className="traffic-sub">{formatDateTime(task.created_at)}</span></td>
                  <td><span className="traffic-sub">{task.duration_ms ? `${(task.duration_ms / 1000).toFixed(1)}s` : '—'}</span></td>
                  <td><span className="traffic-sub">{task.result_summary || task.error_code || '点击查看日志'}</span></td>
                  <td><div className="row-actions">
                    {task.status === 'queued' && <button className="row-more" aria-label={`取消任务 ${task.id.slice(0, 8)}`} disabled={viewer || cancelingId === task.id} title={viewer ? VIEWER_HINT : '取消排队中的任务'} onClick={(event) => { event.stopPropagation(); void cancel(task) }}><Ban size={15} /></button>}
                    {(task.status === 'failed' || task.status === 'success' || task.status === 'cancelled') && RETRYABLE_OPS.has(task.operation_type) && (() => {
                      const isWrite = WRITE_OPS.has(task.operation_type)
                      const retryBlocked = viewer || (isWrite && !admin)
                      return <button className="row-more" aria-label={`重试任务 ${task.id.slice(0, 8)}`} disabled={retryBlocked || retrying} title={viewer ? VIEWER_HINT : isWrite && !admin ? ADMIN_ONLY_HINT : isWrite ? '重新入队（写操作，需确认）' : '按相同参数重新入队'} onClick={(event) => { event.stopPropagation(); setPendingRetry(task) }}><RotateCcw size={15} /></button>
                    })()}
                  </div></td>
                </tr>
              ))}
            </tbody>
          </table></div>
        )}
        {openTask && (
          <div className="task-log-panel">
            <div className="task-log-head"><span>任务 {openTask.id.slice(0, 8)} · {operationLabel(openTask.operation_type)} · {openTask.server_name}</span><button className="row-more" aria-label="关闭日志" onClick={() => { logsFor.current = null; setOpenTask(null); setLogs([]); setLogsLoading(false) }}>×</button></div>
            {openTask.result_summary && <p className="task-log-summary">{openTask.result_summary}</p>}
            {logError && <p className="task-log-summary">{logError}</p>}
            {/* 2026-11-06：读取中不能说「没有日志」—— 网络慢时运维会以为执行器没留日志。 */}
            {logsLoading && <p className="task-log-summary">日志读取中…</p>}
            {!logsLoading && !logError && logs.length === 0 && <p className="task-log-summary">该任务没有日志记录。</p>}
            <ul className="task-log-list">
              {logs.map((entry) => <li key={entry.sequence} className={`log-${entry.level}`}><span>{new Date(entry.logged_at).toLocaleTimeString('zh-CN', { hour12: false })}</span><code>{entry.message}</code></li>)}
            </ul>
          </div>
        )}
        <div className="table-foot"><span>显示最近 {tasks.length} 条任务</span><span>点击任务行查看执行日志</span></div>
      </section>

      {/* 孤儿账号：删订阅/节点时没能从节点侧注销的账号。
          放在任务页而不是订阅页 —— 它本质是「有活儿没干完」，
          与任务队列同一心智；而且它是运维低频查看的东西，
          放进订阅页会让那个高频页面变重。 */}
      <HostJobHistory notify={notify} />
      <OrphanAccounts notify={notify} />

      {pendingRetry && (
        <Modal title="重新下发任务" kicker="RETRY TASK" onClose={() => setPendingRetry(null)}>
          <p className="form-note">将以相同参数重新入队 <strong>{operationLabel(pendingRetry.operation_type)}</strong>（主机 {pendingRetry.server_name}，原任务 {pendingRetry.id.slice(0, 8)}）。{WRITE_OPS.has(pendingRetry.operation_type) ? '这是写操作：目标主机需已开启写操作，且 unit 在其白名单内，否则会直接失败。' : '这是只读操作，不会改动目标主机上的服务。'}</p>
          <div className="modal-actions"><button className="button button-quiet" onClick={() => setPendingRetry(null)}>取消</button><button className="button button-primary" onClick={confirmRetry}>确认重新下发</button></div>
        </Modal>
      )}
    </>
  )
}

function MetricCard({ label, value, unit, icon, tone, foot }: { label: string; value: string; unit: string; icon: React.ReactNode; tone: string; foot: React.ReactNode }) {
  return <div className={`metric-card tone-${tone}`}><div className="metric-top"><span>{label}</span><span className="metric-icon">{icon}</span></div><div className="metric-value">{value}<small>{unit}</small></div><div className="metric-foot">{foot}</div></div>
}
