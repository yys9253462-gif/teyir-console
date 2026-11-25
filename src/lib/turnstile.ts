// Cloudflare Turnstile 统一脚本加载器：登录页与交付页续费弹窗共用同一实例，避免重复插脚本与竞态。
export type TurnstileApi = {
  render: (el: HTMLElement, opts: Record<string, unknown>) => string
  reset: (id?: string) => void
  remove?: (id?: string) => void
}

declare global {
  interface Window {
    turnstile?: TurnstileApi
  }
}

const TURNSTILE_SRC = 'https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit'
const TURNSTILE_LOAD_TIMEOUT_MS = 20_000

let turnstileLoadPromise: Promise<TurnstileApi> | null = null

export function loadTurnstile(): Promise<TurnstileApi> {
  if (window.turnstile) return Promise.resolve(window.turnstile)
  if (turnstileLoadPromise) return turnstileLoadPromise
  turnstileLoadPromise = new Promise<TurnstileApi>((resolve, reject) => {
    const existing = document.querySelector<HTMLScriptElement>(`script[src="${TURNSTILE_SRC}"]`)
    // 如果旧脚本标签残留但 window.turnstile 未就绪，先移除后重建，避免已失败/已结束的 script 永不再次触发 load/error 事件。
    if (existing && existing.dataset.turnstileState !== 'loading') {
      existing.remove()
    }
    const active = document.querySelector<HTMLScriptElement>(`script[src="${TURNSTILE_SRC}"]`)
    const script = active ?? document.createElement('script')
    let settled = false
    const cleanup = () => {
      window.clearTimeout(timer)
      script.removeEventListener('load', onLoad)
      script.removeEventListener('error', onError)
    }
    const fail = (message: string) => {
      if (settled) return
      settled = true
      cleanup()
      turnstileLoadPromise = null
      script.remove()
      reject(new Error(message))
    }
    const onLoad = () => {
      if (settled) return
      if (window.turnstile) {
        settled = true
        cleanup()
        script.dataset.turnstileState = 'ready'
        resolve(window.turnstile)
      } else {
        fail('人机验证脚本已加载但未初始化，请重试')
      }
    }
    const onError = () => {
      fail('人机验证加载失败，请检查网络后重试')
    }
    const timer = window.setTimeout(() => {
      fail('人机验证脚本加载超时，请检查网络后重试')
    }, TURNSTILE_LOAD_TIMEOUT_MS)
    script.addEventListener('load', onLoad, { once: true })
    script.addEventListener('error', onError, { once: true })
    if (!active) {
      script.src = TURNSTILE_SRC
      script.async = true
      script.dataset.turnstileState = 'loading'
      document.head.appendChild(script)
    }
  })
  return turnstileLoadPromise
}
