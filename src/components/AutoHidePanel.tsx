import { useCallback, useEffect, useRef, useState } from 'react'
import { EyeOff } from 'lucide-react'
import { api, loadSession, type AutoHideState } from '../lib/api'
import { askConfirm } from './ConfirmHost'

// 故障节点自动下线（2026-10-16）。挂在节点监控页。
// 端口不通或疑似被墙超过 5 分钟的节点，自动从所有订阅里拿掉，恢复后自动加回。

function formatSince(iso: string): string {
  const date = new Date(iso)
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString('zh-CN', { hour12: false })
}

export function AutoHidePanel({ notify }: { notify: (m: string) => void }) {
  const admin = loadSession()?.role === 'admin'
  const [state, setState] = useState<AutoHideState | null>(null)
  const [busy, setBusy] = useState(false)
  // 2026-11-06：首次读取失败要留一行说明。原来静默吞掉、整块不渲染：
  // 「哪些节点被自动下线了」这件事从页面上消失，看起来像没有节点被下线。
  const [loadError, setLoadError] = useState('')
  const mountedRef = useRef(true)
  const loadSeq = useRef(0)

  const load = useCallback(async () => {
    const seq = ++loadSeq.current
    try {
      const next = await api.autoHide()
      if (!mountedRef.current || seq !== loadSeq.current) return
      setState(next)
      setLoadError('')
    } catch (err) {
      if (!mountedRef.current || seq !== loadSeq.current) return
      setLoadError(err instanceof Error ? err.message : '读取失败')
    }
  }, [])
  useEffect(() => {
    mountedRef.current = true
    void load()
    const t = window.setInterval(() => { if (!document.hidden) void load() }, 60_000)
    const onVisible = () => { if (!document.hidden) void load() }
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      mountedRef.current = false
      loadSeq.current += 1
      window.clearInterval(t)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [load])

  async function toggle(enabled: boolean) {
    if (busy) return
    // 2026-11-06：关闭前确认。关掉后端口不通 / 疑似被墙的节点立刻回到所有买家的订阅里，原来一点即生效。
    if (!enabled && !await askConfirm(`关闭故障节点自动下线？

端口不通或疑似被墙的节点会立刻重新下发给所有客户，客户端可能选到连不上的节点。`, { confirmText: '关闭', danger: true })) return
    if (!mountedRef.current) return
    setBusy(true)
    try {
      const next = await api.setAutoHide(enabled)
      if (!mountedRef.current) return
      setState(next)
      notify(enabled ? '已开启：故障节点会自动从订阅下线' : '已关闭自动下线：故障节点仍会下发给客户')
    } catch (err) {
      if (!mountedRef.current) return
      notify(err instanceof Error ? err.message : '设置失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  if (!state) {
    if (!loadError) return null
    return <section className="autohide" aria-label="故障节点自动下线"><div className="autohide-main"><EyeOff size={17} /><div><strong>故障节点自动下线</strong><small className="lat-bad">读取失败：{loadError}。被自动下线的节点暂时看不到，刷新页面重试。</small></div></div></section>
  }
  const hidden = state.hidden ?? []
  return (
    <section className={`autohide${hidden.length ? ' has-hidden' : ''}`} aria-label="故障节点自动下线">
      <div className="autohide-main">
        <EyeOff size={17} />
        <div>
          <strong>故障节点自动下线 {state.enabled ? <span className="status-pill online">已开启</span> : <span className="status-pill offline">已关闭</span>}</strong>
          <small>
            端口不通或疑似被墙超过 {state.after_minutes} 分钟的节点，自动从客户订阅里拿掉，恢复后自动加回。
            若全部节点同时故障则不下线（多半是探测侧问题）。
          </small>
        </div>
        {admin && <button className="button button-quiet" disabled={busy} onClick={() => void toggle(!state.enabled)}>{state.enabled ? '关闭' : '开启'}</button>}
      </div>
      {state.enabled && hidden.length > 0 && (
        <ul className="autohide-list">
          {hidden.map((h) => (
            <li key={h.node_id}><strong>{h.name}</strong> 已下线 · {h.reason} · 自 {formatSince(h.since)}</li>
          ))}
        </ul>
      )}
    </section>
  )
}
