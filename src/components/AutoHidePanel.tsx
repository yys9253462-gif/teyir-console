import { useCallback, useEffect, useState } from 'react'
import { EyeOff } from 'lucide-react'
import { api, loadSession, type AutoHideState } from '../lib/api'
import { askConfirm } from './ConfirmHost'

// 故障节点自动下线（2026-10-16）。挂在节点监控页。
// 端口不通或疑似被墙超过 5 分钟的节点，自动从所有订阅里拿掉，恢复后自动加回。

export function AutoHidePanel({ notify }: { notify: (m: string) => void }) {
  const admin = loadSession()?.role === 'admin'
  const [state, setState] = useState<AutoHideState | null>(null)
  const [busy, setBusy] = useState(false)
  // 2026-11-06：首次读取失败要留一行说明。原来静默吞掉、整块不渲染：
  // 「哪些节点被自动下线了」这件事从页面上消失，看起来像没有节点被下线。
  const [loadError, setLoadError] = useState('')

  const load = useCallback(async () => {
    try { setState(await api.autoHide()); setLoadError('') } catch (err) { setLoadError(err instanceof Error ? err.message : '读取失败') }
  }, [])
  useEffect(() => {
    void load()
    const t = window.setInterval(() => { if (!document.hidden) void load() }, 60_000)
    return () => window.clearInterval(t)
  }, [load])

  async function toggle(enabled: boolean) {
    // 2026-11-06：关闭前确认。关掉后端口不通 / 疑似被墙的节点立刻回到所有买家的订阅里，原来一点即生效。
    if (!enabled && !await askConfirm(`关闭故障节点自动下线？

端口不通或疑似被墙的节点会立刻重新下发给所有客户，客户端可能选到连不上的节点。`, { confirmText: '关闭', danger: true })) return
    setBusy(true)
    try {
      setState(await api.setAutoHide(enabled))
      notify(enabled ? '已开启：故障节点会自动从订阅下线' : '已关闭自动下线：故障节点仍会下发给客户')
    } catch (err) {
      notify(err instanceof Error ? err.message : '设置失败')
    } finally {
      setBusy(false)
    }
  }

  if (!state) {
    if (!loadError) return null
    return <section className="autohide" aria-label="故障节点自动下线"><div className="autohide-main"><EyeOff size={17} /><div><strong>故障节点自动下线</strong><small className="lat-bad">读取失败：{loadError}。被自动下线的节点暂时看不到，刷新页面重试。</small></div></div></section>
  }
  return (
    <section className={`autohide${state.hidden.length ? ' has-hidden' : ''}`} aria-label="故障节点自动下线">
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
      {state.enabled && state.hidden.length > 0 && (
        <ul className="autohide-list">
          {state.hidden.map((h) => (
            <li key={h.node_id}><strong>{h.name}</strong> 已下线 · {h.reason} · 自 {new Date(h.since).toLocaleString('zh-CN', { hour12: false })}</li>
          ))}
        </ul>
      )}
    </section>
  )
}
