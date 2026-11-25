import { useCallback, useEffect, useState } from 'react'
import { CheckCircle2, RefreshCw, Wrench } from 'lucide-react'
import { api, isAdminSession, type InstallJob } from '../lib/api'
import { askConfirm } from './ConfirmHost'

const jobStatusBadge: Record<InstallJob['status'], string> = {
  running: 'warning',
  success: 'online',
  failed: 'offline critical',
  interrupted: 'warning',
}

const jobStatusLabel: Record<InstallJob['status'], string> = {
  running: '运行中',
  success: '完成',
  failed: '失败',
  interrupted: '已中断，需核验',
}

export function HostJobHistory({ notify }: { notify: (message: string) => void }) {
  const [jobs, setJobs] = useState<InstallJob[]>([])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState('')
  const allowed = isAdminSession()
  const load = useCallback(async () => {
    if (!allowed) return
    try {
      setJobs((await api.installJobs()).jobs)
      setError('')
    } catch (e) {
      setError(e instanceof Error ? e.message : '任务读取失败')
    }
  }, [allowed])

  useEffect(() => {
    if (!allowed) return
    void load()
    const timer = window.setInterval(() => {
      if (!document.hidden) void load()
    }, 15000)
    return () => window.clearInterval(timer)
  }, [allowed, load])

  if (!allowed) return null

  async function resolve(job: InstallJob) {
    if (!await askConfirm('必须先通过SSH或服务商控制台确认远端安装/迁移进程已停止。仅解除面板互斥不会停止远端进程。确认已经核验？')) return
    setBusy(job.id)
    try {
      await api.resolveInstallJob(job.id)
      notify('中断任务已结案，可重新操作')
      await load()
    } catch (e) {
      notify(e instanceof Error ? e.message : '结案失败')
    } finally {
      setBusy('')
    }
  }

  return (
    <section className="panel servers-panel">
      <div className="section-head servers-head">
        <div>
          <div className="section-kicker">HOST JOB JOURNAL <span>·</span> {jobs.length.toString().padStart(2, '0')}</div>
          <h2>安装与迁移任务 <span className="count-chip">{jobs.length.toString().padStart(2, '0')}</span></h2>
        </div>
        <div className="heading-actions">
          <button className="button button-quiet" onClick={() => void load()}>
            <RefreshCw size={14} />刷新
          </button>
        </div>
      </div>
      <div className="panel-note-row">
        <p className="form-note">任务持久保存。进程中断后保留主机互斥；只有确认远端任务停止后才能结案，不会自动重新安装。</p>
        {error && <p className="form-error" role="alert">{error}</p>}
      </div>
      {jobs.length === 0 && !error ? (
        <div className="empty-state compact">
          <CheckCircle2 size={18} />
          <strong>暂无安装或迁移任务</strong>
          <span>通过「一键安装 HY2」或「迁移节点」发起的主机级长任务会在此持久记录。</span>
        </div>
      ) : (
        <div className="server-table-wrap table-wrap">
          <table className="server-table">
            <thead>
              <tr><th>任务</th><th>状态 / 进度</th><th>异常</th><th>处理</th></tr>
            </thead>
            <tbody>
              {jobs.map((j) => (
                <tr key={j.id}>
                  <td>
                    <div className="host-cell">
                      <div className={`host-icon ${jobStatusBadge[j.status]}`}><Wrench size={14} /></div>
                      <div>
                        <strong>{j.node_name || '主机操作'}</strong>
                        <span className="mono">{j.id}</span>
                      </div>
                    </div>
                  </td>
                  <td>
                    <span className={`status-badge ${jobStatusBadge[j.status]}`}><i />{jobStatusLabel[j.status]}</span>
                    {j.step && <span className="traffic-sub">{j.step}</span>}
                  </td>
                  <td><span className="traffic-sub">{j.error || '—'}</span></td>
                  <td>
                    {j.status === 'interrupted' ? (
                      <button className="button button-quiet button-compact" disabled={busy === j.id} onClick={() => void resolve(j)}>
                        已核验远端停止，结案
                      </button>
                    ) : <span className="traffic-sub">—</span>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
