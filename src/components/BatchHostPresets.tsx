import { useEffect, useRef, useState } from 'react'
import { api, isAdminSession, loadSession, ADMIN_ONLY_HINT, type OperationTask, type Server } from '../lib/api'
import { Modal } from './Modal'
import { askConfirm } from './ConfirmHost'

// 主机批量预设任务（参考 kejilion/sh 的「集群控制 · 批量执行任务」）。
//
// 每台主机创建一条 system_preset 任务，交给后台执行器跑 —— 进度与完整日志
// 都在「运维任务」页，这里只轮询本次创建的任务、给出每台的结果摘要。
// 脚本全部写死在服务端（executor/presets.go），这里只能选名字。

const PRESETS: { key: string; label: string; hint: string; readOnly?: boolean; danger?: boolean }[] = [
  { key: 'update', label: '更新系统', hint: 'apt / dnf / yum 升级全部软件包；保留现有配置文件。可能耗时几分钟' },
  { key: 'kernel_reboot', label: '更新内核并重启', hint: '升级软件包（含新内核）；只有装上了比当前更新的内核才重启，内核已是最新则不重启。重启约 1-2 分钟，期间节点用户断线；面板所在主机只升级不重启' },
  { key: 'clean', label: '清理系统', hint: '移除无用依赖、清包缓存、日志只留 7 天 / 200MB、删 7 天前的临时文件' },
  { key: 'swap_1g', label: '设置 1G 虚拟内存', hint: '没有 swap 时创建 /swapfile 并写入 fstab；已有 swap 的主机跳过' },
  { key: 'timezone_shanghai', label: '时区设为上海', hint: '设为 Asia/Shanghai；已是该时区的主机跳过' },
  { key: 'install_fail2ban', label: '安装 fail2ban', hint: 'SSH 防爆破：5 次失败封禁 1 小时；已运行的主机跳过' },
  { key: 'security_audit', label: '安全检查', hint: '只读：检查 SSH 密码登录、root 登录、fail2ban、防火墙、待装安全更新等，给出 100 分制评分；不改动任何东西', readOnly: true },
  { key: 'net_route', label: '回程线路检测', hint: '用 nexttrace 测到电信 / 联通 / 移动的回程，识别 CN2、9929、CMI 等线路；首次会安装 nexttrace，耗时约 3-6 分钟' },
  { key: 'uninstall_hy2', label: '卸载 Hysteria 2', hint: '危险：用官方脚本卸载 Hysteria 2 及其配置，节点上的所有用户会立即断开；面板里的节点需另行删除', danger: true },
]

const STATUS_TEXT: Record<string, string> = {
  queued: '排队中', running: '执行中', success: '完成', failed: '失败', cancelled: '已取消',
}
const STATUS_COLOR: Record<string, string> = {
  queued: 'var(--muted)', running: 'var(--text-soft)', success: 'var(--green)', failed: 'var(--red)', cancelled: 'var(--muted)',
}

function blockedReason(server: Server, readOnly = false): string {
  if (!server.has_credential) return '未配置 SSH 凭据'
  if (!readOnly && !server.allow_write_ops) return '未开启写操作授权（编辑主机可开启）'
  return ''
}

export function BatchHostPresets({ servers, onClose, notify, initialPreset }: {
  servers: Server[]; onClose: () => void; notify: (message: string) => void
  /** 从「主机工具」卡片打开时直接定位到对应预设。 */
  initialPreset?: string
}) {
  const readOnly = loadSession()?.role === 'viewer'
  const admin = isAdminSession()
  const [preset, setPreset] = useState(PRESETS.some((p) => p.key === initialPreset) ? initialPreset! : PRESETS[0].key)
  const [selected, setSelected] = useState<string[]>([])
  const [submitting, setSubmitting] = useState(false)
  // server_id → 本次创建的任务 id
  const [created, setCreated] = useState<Record<string, string>>({})
  const [createErrors, setCreateErrors] = useState<Record<string, string>>({})
  const [tasks, setTasks] = useState<Record<string, OperationTask>>({})
  const [pollError, setPollError] = useState('')
  const mountedRef = useRef(true)
  useEffect(() => {
    mountedRef.current = true
    return () => { mountedRef.current = false }
  }, [])

  const current = PRESETS.find((p) => p.key === preset)!
  const eligible = servers.filter((s) => !blockedReason(s, current.readOnly))
  const taskIds = Object.values(created)
  const pending = taskIds.some((id) => !tasks[id] || ['queued', 'running'].includes(tasks[id].status))

  // 串行定时轮询本次创建的任务：首轮立即拉取、前一次未完成不重叠、标签页隐藏时暂停并在切回前台立刻补一次。
  // 连续失败 3 次即停止并提示原因，避免界面永远卡在「执行中」。
  useEffect(() => {
    if (!taskIds.length || !pending) return
    let alive = true
    let fails = 0
    let timer: number | undefined

    const poll = async () => {
      if (!alive) return
      if (document.hidden) {
        timer = window.setTimeout(() => { void poll() }, 3000)
        return
      }
      try {
        const list = await api.tasks(taskIds)
        if (!alive) return
        fails = 0
        setPollError('')
        setTasks(Object.fromEntries(list.filter((t) => taskIds.includes(t.id)).map((t) => [t.id, t])))
        timer = window.setTimeout(() => { void poll() }, 3000)
      } catch (err) {
        if (!alive) return
        fails++
        if (fails >= 3) {
          setPollError(err instanceof Error ? `进度查询失败：${err.message}（已停止刷新，结果请到「运维任务」页查看）` : '进度查询连续失败，已停止刷新')
          return
        }
        timer = window.setTimeout(() => { void poll() }, 3000)
      }
    }

    const onVisible = () => {
      if (!document.hidden && alive && fails < 3) {
        window.clearTimeout(timer)
        void poll()
      }
    }
    document.addEventListener('visibilitychange', onVisible)
    void poll()
    return () => {
      alive = false
      window.clearTimeout(timer)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [taskIds.join(','), pending])

  async function start() {
    if (readOnly || !admin || submitting || !selected.length) return
    if (!await askConfirm(`确认对 ${selected.length} 台主机执行「${current.label}」？\n\n${current.hint}`)) return
    if (current.danger && window.prompt(`这是不可逆操作。输入「卸载」确认对 ${selected.length} 台主机执行：`) !== '卸载') return
    setSubmitting(true)
    setCreated({}); setCreateErrors({}); setTasks({})
    const ok: Record<string, string> = {}
    const bad: Record<string, string> = {}
    for (const id of selected) {
      try {
        const task = await api.createTask({ server_id: id, operation_type: 'system_preset', parameters: { preset } })
        ok[id] = task.id
      } catch (err) {
        bad[id] = err instanceof Error ? err.message : String(err)
      }
    }
    if (mountedRef.current) {
      setCreated(ok); setCreateErrors(bad)
      setSubmitting(false)
    }
    notify(`已创建 ${Object.keys(ok).length} 个「${current.label}」任务${Object.keys(bad).length ? `，${Object.keys(bad).length} 台创建失败` : ''}`)
  }

  const finished = taskIds.map((id) => tasks[id]).filter(Boolean)
  const count = (status: string) => finished.filter((t) => t.status === status).length

  return <Modal title="批量预设任务" kicker="HOST PRESETS" wide onClose={onClose}>
    <div style={{ color: 'var(--text)', fontSize: 'var(--fs-sm)', lineHeight: 1.65 }}>
      <p style={{ marginBottom: 10 }}>对多台主机执行固定的系统维护脚本。每项都可安全重复执行；任务在后台运行，关闭窗口不影响，完整日志在「运维任务」页。</p>

      <div className="monitor-filter" role="group" aria-label="预设" style={{ display: 'inline-flex', flexWrap: 'wrap', marginBottom: 8 }}>
        {PRESETS.map((p) => <button key={p.key} className={preset === p.key ? 'active' : ''} disabled={submitting || pending} onClick={() => { setPreset(p.key); setSelected([]); setCreated({}); setTasks({}); setCreateErrors({}); setPollError('') }}>{p.label}</button>)}
      </div>
      <p className="form-note" style={{ marginBottom: 10 }}>{current.hint}</p>

      <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap', marginBottom: 12 }}>
        <button className="button button-quiet" disabled={readOnly || !admin || submitting || pending || !eligible.length} onClick={() => setSelected(eligible.map((s) => s.id))}>全选可执行（{eligible.length}）</button>
        <button className="button button-quiet" disabled={readOnly || !admin || submitting || pending || !selected.length} onClick={() => setSelected([])}>清空选择</button>
        <button className="button button-primary" disabled={readOnly || !admin || submitting || pending || !selected.length} title={readOnly ? '只读账号不能执行预设任务' : !admin ? ADMIN_ONLY_HINT : undefined} onClick={() => void start()}>
          {submitting ? '创建任务…' : pending ? '执行中…' : `开始执行（${selected.length}）`}
        </button>
      </div>
      {readOnly && <p>只读账号只能查看，不能执行。</p>}
      {!readOnly && !admin && <p className="form-note">{ADMIN_ONLY_HINT}（系统预设任务需管理员权限）。</p>}
      {eligible.length < servers.length && <p className="form-note" style={{ marginBottom: 8 }}>
        {current.readOnly
          ? '灰色的主机不能执行：该只读检查需要主机已配置 SSH 凭据。'
          : '灰色的主机不能执行：预设任务会改动系统，需要该主机已配置 SSH 凭据并开启写操作授权。'}
      </p>}

      {pollError && <p role="alert" style={{ margin: '0 0 8px', color: 'var(--amber-text)' }}>⚠ {pollError}</p>}
      {taskIds.length > 0 && <p style={{ margin: '0 0 8px' }}>
        完成 {count('success')}
        {count('failed') > 0 && <> · <span style={{ color: 'var(--red)' }}>失败 {count('failed')}</span></>}
        {pending && <> · 进行中 {taskIds.length - count('success') - count('failed') - count('cancelled')}</>}
      </p>}

      <div style={{ display: 'grid', gap: 6 }}>
        {servers.map((server) => {
          const reason = blockedReason(server, current.readOnly)
          const task = created[server.id] ? tasks[created[server.id]] : undefined
          const createError = createErrors[server.id]
          return <label key={server.id} style={{ display: 'flex', alignItems: 'baseline', gap: 8, flexWrap: 'wrap', padding: '4px 0', borderBottom: '1px solid var(--line-soft)', opacity: reason ? 0.6 : 1 }}>
            <input type="checkbox" checked={selected.includes(server.id)} disabled={readOnly || !admin || Boolean(reason) || submitting || pending}
              onChange={(e) => setSelected((ids) => e.target.checked ? [...ids, server.id] : ids.filter((id) => id !== server.id))} />
            <span style={{ flexShrink: 0 }}>{server.name}</span>
            <small style={{ color: 'var(--faint)' }}>{server.host}</small>
            {reason && <small style={{ color: 'var(--muted)' }}>{reason}</small>}
            {createError && <small style={{ color: 'var(--red)' }}>创建失败 · {createError}</small>}
            {created[server.id] && <small style={{ color: STATUS_COLOR[task?.status ?? 'queued'], overflowWrap: 'anywhere' }}>
              {STATUS_TEXT[task?.status ?? 'queued'] ?? task?.status}{task?.result_summary ? ` · ${task.result_summary}` : ''}
            </small>}
          </label>
        })}
        {!servers.length && <p>还没有主机。</p>}
      </div>
    </div>
  </Modal>
}
