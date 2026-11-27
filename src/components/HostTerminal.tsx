import { useEffect, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { api, hostTerminalURL } from '../lib/api'
import { HostOpsError, hostOpsError } from './HostOpsView'
import { terminalInputChunks } from '../lib/hostOpsLifecycle'

type Connection = { cancelled: boolean; socket: WebSocket | null; connectTimer: number | null }

const TERMINAL_CONNECT_TIMEOUT_MS = 30_000

export function decodeBase64(data: string): Uint8Array {
  const binary = atob(data.replace(/\s+/g, ''))
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i)
  return bytes
}

export function HostTerminal({ serverId }: { serverId: string }) {
  const element = useRef<HTMLDivElement>(null)
  const terminal = useRef<Terminal | null>(null)
  const fit = useRef<FitAddon | null>(null)
  const connection = useRef<Connection | null>(null)
  const [status, setStatus] = useState('未连接')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  function safeSend(socket: WebSocket | null | undefined, payload: Record<string, unknown>) {
    if (!socket || socket.readyState !== WebSocket.OPEN) return
    try {
      socket.send(JSON.stringify(payload))
    } catch {
      // 发送瞬间连接已断开时由 onclose 统一更新状态
    }
  }

  function safeFitAndResize(socketOverride?: WebSocket | null) {
    const el = element.current
    const term = terminal.current
    const addon = fit.current
    if (!el || !term || !addon || !el.clientWidth || !el.clientHeight) return
    try {
      addon.fit()
    } catch {
      return
    }
    const socket = socketOverride ?? connection.current?.socket
    safeSend(socket, { type: 'resize', cols: Math.min(500, Math.max(1, term.cols)), rows: Math.min(200, Math.max(1, term.rows)) })
  }

  function disconnect(update = true) {
    const active = connection.current
    connection.current = null
    if (active) {
      active.cancelled = true
      if (active.connectTimer !== null) {
        window.clearTimeout(active.connectTimer)
        active.connectTimer = null
      }
      try {
        active.socket?.close(1000, 'client disconnect')
      } catch {
        // 忽略重复关闭异常
      }
    }
    if (update) { setBusy(false); setStatus('已断开') }
  }

  useEffect(() => {
    setStatus('未连接')
    setBusy(false)
    setError('')
    const term = new Terminal({ cursorBlink: true, fontSize: 13, scrollback: 3000, theme: { background: '#101318' } })
    const addon = new FitAddon()
    term.loadAddon(addon)
    term.open(element.current!)
    terminal.current = term
    fit.current = addon
    let rafId: number | null = null
    const scheduleResize = () => {
      if (rafId !== null) window.cancelAnimationFrame(rafId)
      rafId = window.requestAnimationFrame(() => {
        rafId = null
        safeFitAndResize()
      })
    }
    const observer = new ResizeObserver(scheduleResize)
    observer.observe(element.current!)
    safeFitAndResize()
    const input = term.onData((data) => {
      const socket = connection.current?.socket
      if (socket?.readyState === WebSocket.OPEN) {
        for (const chunk of terminalInputChunks(data)) safeSend(socket, { type: 'input', data: chunk })
      }
    })
    return () => {
      disconnect(false)
      if (rafId !== null) window.cancelAnimationFrame(rafId)
      observer.disconnect()
      input.dispose()
      term.dispose()
      terminal.current = null
      fit.current = null
    }
  }, [serverId])

  async function connect() {
    if (connection.current) return
    const active: Connection = { cancelled: false, socket: null, connectTimer: null }
    connection.current = active
    setBusy(true)
    setError('')
    setStatus('连接中')
    try {
      await api.terminalCookie()
      if (active.cancelled) return
      const socket = new WebSocket(hostTerminalURL(serverId))
      active.socket = socket
      // WebSocket 建连超时保护：防止反代或防火墙静默丢包导致界面永久卡在「连接中」。
      active.connectTimer = window.setTimeout(() => {
        if (active.cancelled || socket.readyState === WebSocket.OPEN) return
        active.cancelled = true
        active.connectTimer = null
        connection.current = null
        try { socket.close(1000, 'connect timeout') } catch { /* 忽略 */ }
        setBusy(false)
        setStatus('连接超时')
        setError('SSH 终端建连超时，请检查主机连通性、反向代理 WebSocket 配置或稍后重试。')
      }, TERMINAL_CONNECT_TIMEOUT_MS)
      const clearConnectTimer = () => {
        if (active.connectTimer !== null) {
          window.clearTimeout(active.connectTimer)
          active.connectTimer = null
        }
      }
      socket.onopen = () => {
        clearConnectTimer()
        if (active.cancelled) { socket.close(); return }
        setStatus('已连接 · shell 可写')
        safeFitAndResize(socket)
        terminal.current?.focus()
      }
      socket.onmessage = (event) => {
        if (active.cancelled) return
        try {
          const message = JSON.parse(String(event.data)) as { type: string; data?: string; encoding?: string; error?: string; code?: number; max_session_seconds?: number }
          // 2026-11-06：后端按 base64 发送输出（hostops.go remoteTerminal：encoding="base64"）。原来原样写进终端，
          // 屏幕上是一串 base64 而不是 shell 输出。解成字节交给 xterm（它自己处理跨块截断的 UTF-8）。
          if (message.type === 'output' || message.type === 'stdout' || message.type === 'stderr') terminal.current?.write(message.encoding === 'base64' ? decodeBase64(message.data ?? '') : (message.data ?? ''))
          else if (message.type === 'ready') {
            const secs = typeof message.max_session_seconds === 'number' && message.max_session_seconds > 0 ? message.max_session_seconds : 1800
            const limitLabel = secs >= 60 ? `${Math.max(1, Math.round(secs / 60))} 分钟` : `${secs} 秒`
            setStatus(`已连接 · shell 可写 · 最长 ${limitLabel}`)
          }
          else if (message.type === 'error') setError(message.error ?? message.data ?? '终端错误')
          else if (message.type === 'exit' || message.type === 'close') {
            clearConnectTimer()
            connection.current = null
            active.cancelled = true
            socket.close(1000, 'remote shell ended')
            setBusy(false)
            setStatus(`远端已结束${message.code === undefined ? '' : ` (${message.code})`}`)
          }
        } catch { setError('终端消息格式无效') }
      }
      socket.onerror = () => {
        clearConnectTimer()
        if (!active.cancelled) setError('WebSocket 连接失败，请检查会话、SSH 凭据及后端日志。')
      }
      socket.onclose = (event) => {
        clearConnectTimer()
        if (active.cancelled) return
        connection.current = null
        setBusy(false)
        setStatus(`已关闭 (${event.code})${event.reason ? ` · ${event.reason}` : ''}`)
      }
    } catch (cause) {
      if (active.connectTimer !== null) {
        window.clearTimeout(active.connectTimer)
        active.connectTimer = null
      }
      if (active.cancelled) return
      connection.current = null
      setBusy(false)
      setStatus('连接失败')
      setError(hostOpsError(cause))
    }
  }

  return <div>
    <p>真实 SSH 交互终端具有写入能力；仅在明确需要时连接。离开此标签或关闭详情将断开连接。</p>
    <div className="host-ops-toolbar"><button type="button" className="button button-primary" onClick={connect} disabled={busy}>连接终端</button><button type="button" className="button button-quiet" onClick={() => disconnect()} disabled={!busy}>断开终端</button><span className="host-terminal-status" role="status">{status}</span></div>
    <HostOpsError error={error} />
    <div className="host-terminal"><div className="host-terminal-screen" ref={element} aria-label="SSH 终端输出" /></div>
  </div>
}
