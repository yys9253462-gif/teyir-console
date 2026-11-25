import { useEffect, useRef } from 'react'
import { X } from 'lucide-react'

// 维护打开中弹窗的层级栈：按 Esc 仅关闭最顶层弹窗，并在有弹窗打开时统一锁定页面底层滚动。
const modalStack: symbol[] = []
let savedBodyOverflow: string | null = null

// 供外层抽屉判断是否存在打开中的弹窗，避免按 Esc 键时同时穿透关闭下层抽屉。
export function hasOpenModal(): boolean {
  return modalStack.length > 0
}

export function Modal({ title, kicker, children, onClose, wide }: { title: string; kicker?: string; children: React.ReactNode; onClose: () => void; wide?: boolean }) {
  const onCloseRef = useRef(onClose)
  onCloseRef.current = onClose

  useEffect(() => {
    const id = Symbol('modal')
    if (modalStack.length === 0) {
      savedBodyOverflow = document.body.style.overflow
      document.body.style.overflow = 'hidden'
    }
    modalStack.push(id)

    function onKey(event: KeyboardEvent) {
      if (event.key === 'Escape' && modalStack[modalStack.length - 1] === id) {
        event.preventDefault()
        event.stopPropagation()
        onCloseRef.current()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener('keydown', onKey)
      const idx = modalStack.lastIndexOf(id)
      if (idx >= 0) modalStack.splice(idx, 1)
      if (modalStack.length === 0 && savedBodyOverflow !== null) {
        document.body.style.overflow = savedBodyOverflow
        savedBodyOverflow = null
      }
    }
  }, [])

  return (
    <div className="modal-backdrop modal-centered" onMouseDown={(event) => event.target === event.currentTarget && onClose()}>
      <section className={`modal-card${wide ? ' modal-wide' : ''}`} role="dialog" aria-modal="true" aria-label={title}>
        <div className="modal-head">
          <div>
            {kicker && <div className="section-kicker">{kicker}</div>}
            <h2>{title}</h2>
          </div>
          <button className="icon-button" aria-label="关闭" onClick={onClose}><X size={17} /></button>
        </div>
        <div className="modal-body">{children}</div>
      </section>
    </div>
  )
}
