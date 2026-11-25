import { useEffect, useRef } from 'react'
import { api, type InstallJob } from './api'

// Keep a single request in flight and retry reads without restarting the job.
export function useInstallJobPolling(job: InstallJob | null, setJob: (job: InstallJob) => void, setError: (error: string) => void, onSuccess: (job: InstallJob) => void) {
  const callbacks = useRef({ setJob, setError, onSuccess })
  callbacks.current = { setJob, setError, onSuccess }
  const id = job?.id
  const running = job?.status === 'running'
  useEffect(() => {
    if (!id || !running) return
    let cancelled = false
    let timer: number
    async function poll() {
      try {
        const next = await api.installJob(id!)
        if (cancelled) return
        callbacks.current.setError('')
        callbacks.current.setJob(next)
        if (next.status === 'success') callbacks.current.onSuccess(next)
        if (next.status !== 'running') return
      } catch (err) {
        if (cancelled) return
        // 2026-11-06：404 = 任务已不在面板上（超过保留期，或面板重启后没恢复出来），重试永远不会成功。
        // 原来照样每 3 秒重试并提示「将自动重试」，窗口一直转圈。停下来，告诉操作者去哪里看结果。
        if ((err as { status?: number }).status === 404) {
          callbacks.current.setError('面板上已找不到这个任务（可能已超过保留期或面板重启过），已停止读取进度；请到「运维任务」页的「安装与迁移任务」查看结果')
          return
        }
        callbacks.current.setError(`${err instanceof Error ? err.message : '读取进度失败'}，将自动重试`)
      }
      if (!cancelled) timer = window.setTimeout(poll, 3000)
    }
    timer = window.setTimeout(poll, 3000)
    return () => { cancelled = true; window.clearTimeout(timer) }
  }, [id, running])
}

export function pendingInstallJob(id: string, serverID: string): InstallJob {
  return { id, server_id: serverID, status: 'running', step: '任务已启动，等待读取进度…', log: [], started_at: new Date().toISOString() }
}
