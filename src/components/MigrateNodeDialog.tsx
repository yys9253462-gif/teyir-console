import { useEffect, useRef, useState } from 'react'
import { AlertTriangle, CheckCircle2, Loader2, XCircle } from 'lucide-react'
import { api, type Hy2Node, type InstallJob, type Server } from '../lib/api'
import { pendingInstallJob, useInstallJobPolling } from '../lib/useInstallJobPolling'
import { Modal } from './Modal'
import { askConfirm } from './ConfirmHost'
import { loadAcmeEmail, saveAcmeEmail } from './InstallHy2Dialog'

// 节点一键迁移到新 VPS（2026-10-15）。
// 把 /etc/hysteria 原样搬到新机器、域名改指新 IP、沿用门户重装 —— 客户订阅链接不变。

export function MigrateNodeDialog({ nodes, servers, onClose, onDone, notify }: {
  nodes: Hy2Node[]
  servers: Server[]
  onClose: () => void
  onDone: () => void
  notify: (m: string) => void
}) {
  const hostsWithNode = new Set(nodes.map((n) => n.server_id))
  const [nodeId, setNodeId] = useState('')
  const [targetId, setTargetId] = useState('')
  const [autoDNS, setAutoDNS] = useState(true)
  const [email, setEmail] = useState(() => loadAcmeEmail())
  const [starting, setStarting] = useState(false)
  const [error, setError] = useState('')
  const [job, setJob] = useState<InstallJob | null>(null)
  const logRef = useRef<HTMLPreElement>(null)

  const node = nodes.find((n) => n.id === nodeId)
  const oldHost = servers.find((s) => s.id === node?.server_id)
  const target = servers.find((s) => s.id === targetId)
  // 2026-11-06：与后端同一套前置条件。原来只看域名：旧主机没有 SSH 凭据时，选好目标、填完邮箱、确认后才收到 409；
  // 节点没接入 API 时任务开起来，跑到「读取旧节点」一步才失败。
  const nodeBlocked = (n: Hy2Node) => {
    if (!n.domain || /^\d+\.\d+\.\d+\.\d+$/.test(n.domain)) return '没有域名'
    if (!n.has_api) return '未接入节点 API'
    if (!servers.find((s) => s.id === n.server_id)?.has_credential) return '旧主机无 SSH 凭据'
    return ''
  }
  const targetBlocked = (s: Server) => {
    if (hostsWithNode.has(s.id)) return '已有节点'
    if (!s.has_credential) return '无 SSH 凭据'
    if (!s.allow_write_ops) return '未开写操作'
    return ''
  }

  useInstallJobPolling(job, setJob, setError, (next) => {
    notify(`迁移完成：${next.node_name}`)
    onDone()
  })
  useEffect(() => { logRef.current?.scrollTo(0, logRef.current.scrollHeight) }, [job?.log.length])

  async function start() {
    if (!node || !target) return
    if (!await askConfirm(`把「${node.name}」从 ${oldHost?.name ?? '旧主机'}（${oldHost?.host ?? ''}）迁移到 ${target.name}（${target.host}）？\n\n域名 ${node.domain} 会改指新 IP，期间节点会中断几分钟；旧机器上的服务不会被删除。`)) return
    setStarting(true)
    setError('')
    try {
      const { job_id } = await api.migrateNode(node.id, { target_server_id: target.id, auto_dns: autoDNS, email: email.trim() })
      saveAcmeEmail(email.trim())
      setJob(pendingInstallJob(job_id, target.id))
    } catch (err) {
      setError(err instanceof Error ? err.message : '启动迁移失败')
    } finally {
      setStarting(false)
    }
  }

  return (
    <Modal title="迁移节点到新 VPS" kicker="MIGRATE NODE" wide onClose={onClose}>
      <div style={{ display: 'grid', gap: 12 }}>
        {!job && <>
          <p className="form-note">把节点的全部数据（用户、门户凭据、订阅端口、Reality 密钥）搬到一台新机器，域名改指新 IP 后沿用门户重装。<strong>客户手里的订阅链接不变</strong>。旧机器上的服务不会被动，确认新节点正常后再自行停用。</p>
          <label className="form-field"><span>要迁移的节点</span>
            <select value={nodeId} onChange={(e) => setNodeId(e.target.value)}>
              <option value="">— 选择节点 —</option>
              {nodes.map((n) => <option key={n.id} value={n.id} disabled={Boolean(nodeBlocked(n))}>{n.name}（{n.domain || '无域名'}）{nodeBlocked(n) ? ` · ${nodeBlocked(n)}` : ''}</option>)}
            </select>
          </label>
          <label className="form-field"><span>迁移到（空机器：已配 SSH 凭据、开启写操作、还没有节点）</span>
            <select value={targetId} onChange={(e) => setTargetId(e.target.value)}>
              <option value="">— 选择目标主机 —</option>
              {servers.filter((s) => s.id !== node?.server_id).map((s) => <option key={s.id} value={s.id} disabled={Boolean(targetBlocked(s))}>{s.name}（{s.host}）{targetBlocked(s) ? ` · ${targetBlocked(s)}` : ''}</option>)}
            </select>
          </label>
          <label className="checkbox-row"><input type="checkbox" checked={autoDNS} onChange={(e) => setAutoDNS(e.target.checked)} /><span>用 Cloudflare 自动把域名改指新 IP（不勾则需你手工改 DNS，任务会等最多 10 分钟）</span></label>
          <label className="form-field"><span>证书通知邮箱 *（新主机申请证书必须）</span><input type="email" value={email} onChange={(e) => setEmail(e.target.value)} placeholder="me@example.com" /></label>
          {node && target && <p className="form-note">{node.name}：{oldHost?.host} → <strong>{target.host}</strong>，域名 <code>{node.domain}</code></p>}
          {error && <p role="alert" style={{ color: 'var(--red)' }}>{error}</p>}
          <div className="modal-actions">
            <button className="button button-quiet" onClick={onClose}>取消</button>
            <button className="button button-primary" disabled={starting || !node || !target || !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email.trim())} onClick={() => void start()}>{starting ? '启动中…' : '开始迁移'}</button>
          </div>
        </>}
        {job && <>
          <div className="install-status">
            {job.status === 'running' && <Loader2 size={18} className="spin" />}
            {job.status === 'success' && <CheckCircle2 size={18} style={{ color: 'var(--green)' }} />}
            {job.status === 'failed' && <XCircle size={18} style={{ color: 'var(--red)' }} />}
            {/* 2026-11-06：interrupted（远端可能仍在跑、主机互斥保留）原来没有图标和标题，只剩步骤文字，看着像还在进行。 */}
            {job.status === 'interrupted' && <AlertTriangle size={18} style={{ color: 'var(--amber)' }} />}
            <strong>{job.status === 'success' ? '迁移完成，客户订阅不变' : job.status === 'failed' ? '迁移失败' : job.status === 'interrupted' ? '已中断，需要核验远端是否已停止' : job.step}</strong>
          </div>
          {error && <p role="alert" style={{ color: 'var(--amber)' }}>{error}</p>}
          {job.error && <p role="alert" style={{ color: 'var(--red)', overflowWrap: 'anywhere' }}>{job.error}</p>}
          <pre className="install-log" ref={logRef}>{job.log.join('\n') || '等待输出…'}</pre>
          <div className="modal-actions">
            {job.status === 'running' ? <span className="form-note">迁移在后台进行，关掉窗口不影响。</span> : <button className="button button-primary" onClick={onClose}>关闭</button>}
          </div>
        </>}
      </div>
    </Modal>
  )
}
