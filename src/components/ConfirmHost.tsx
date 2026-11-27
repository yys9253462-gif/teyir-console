import { useEffect, useRef, useState } from 'react'
import { AlertTriangle, HelpCircle } from 'lucide-react'

// 面板统一的确认弹窗，替代浏览器原生 window.confirm（2026-10-16）。
// 用法：if (!(await askConfirm('标题\n\n说明', { confirmText: '开启并安装' }))) return
// 文本里第一个空行之前是标题，之后是说明；含「删除 / 清除 / 停止」等字样自动用危险样式。

type ConfirmOptions = { title?: string; confirmText?: string; cancelText?: string; danger?: boolean }
type Pending = ConfirmOptions & { message: string; resolve: (ok: boolean) => void }

let push: ((p: Pending) => void) | null = null

export function askConfirm(message: string, options: ConfirmOptions = {}): Promise<boolean> {
  if (!push) return Promise.resolve(window.confirm(message))
  return new Promise((resolve) => push!({ ...options, message, resolve }))
}

const DANGER = /删除|清除|停止|移除|关闭后|不能被恢复|重写|迁移|轮换/

export function ConfirmHost() {
  const [queue, setQueue] = useState<Pending[]>([])
  const queueRef = useRef<Pending[]>([])
  queueRef.current = queue
  const okRef = useRef<HTMLButtonElement>(null)
  const cancelRef = useRef<HTMLButtonElement>(null)
  const settledRef = useRef<WeakSet<Pending>>(new WeakSet())
  const settle = (target: Pending, ok: boolean) => {
    if (settledRef.current.has(target)) return
    settledRef.current.add(target)
    target.resolve(ok)
    setQueue((q) => q.filter((item) => item !== target))
  }
  useEffect(() => {
    push = (p) => setQueue((q) => [...q, p])
    return () => {
      push = null
      // 组件卸载时结算全部未决确认，避免调用方 await askConfirm 永久挂起。
      for (const pending of queueRef.current) {
        if (!settledRef.current.has(pending)) {
          settledRef.current.add(pending)
          pending.resolve(false)
        }
      }
    }
  }, [])
  const current = queue[0]
  // 2026-11-06：危险操作默认聚焦「取消」。原来一律聚焦确认键：按回车提交表单、或连按一次回车，
  // 就直接确认了删除 / 卸载 / 关闭这类不可逆操作。普通确认仍聚焦确认键，回车即可继续。
  useEffect(() => {
    if (!current) return
    const isDanger = current.danger ?? DANGER.test(current.message)
    ;(isDanger ? cancelRef : okRef).current?.focus()
  }, [current])
  useEffect(() => {
    if (!current) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopImmediatePropagation()
        e.stopPropagation()
        settle(current, false)
      }
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [current])
  if (!current) return null

  function done(ok: boolean) {
    settle(current, ok)
  }
  const text = current.message.trim()
  const split = text.indexOf('\n\n')
  const title = current.title ?? (split > 0 ? text.slice(0, split) : text)
  const body = current.title ? text : split > 0 ? text.slice(split + 2) : ''
  const danger = current.danger ?? DANGER.test(text)

  return (
    <div className="modal-backdrop modal-centered confirm-backdrop" onMouseDown={(e) => e.target === e.currentTarget && done(false)}>
      <section className={`modal-card confirm-card${danger ? ' is-danger' : ''}`} role="alertdialog" aria-modal="true" aria-label={title}>
        <div className="confirm-icon">{danger ? <AlertTriangle size={20} /> : <HelpCircle size={20} />}</div>
        <div className="confirm-content">
          <h2>{title}</h2>
          {body && <p>{body}</p>}
          <div className="confirm-actions">
            <button ref={cancelRef} className="button button-quiet" onClick={() => done(false)}>{current.cancelText ?? '取消'}</button>
            <button ref={okRef} className={`button ${danger ? 'button-danger' : 'button-primary'}`} onClick={() => done(true)}>{current.confirmText ?? '确定'}</button>
          </div>
        </div>
      </section>
    </div>
  )
}
