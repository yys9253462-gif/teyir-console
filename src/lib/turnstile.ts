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

let turnstileLoadPromise: Promise<TurnstileApi> | null = null

export function loadTurnstile(): Promise<TurnstileApi> {
  if (window.turnstile) return Promise.resolve(window.turnstile)
  if (turnstileLoadPromise) return turnstileLoadPromise
  turnstileLoadPromise = new Promise<TurnstileApi>((resolve, reject) => {
    const existing = document.querySelector<HTMLScriptElement>(`script[src="${TURNSTILE_SRC}"]`)
    const script = existing ?? document.createElement('script')
    const onLoad = () => {
      if (window.turnstile) {
        resolve(window.turnstile)
      } else {
        turnstileLoadPromise = null
        reject(new Error('人机验证脚本已加载但未初始化，请重试'))
      }
    }
    const onError = () => {
      turnstileLoadPromise = null
      reject(new Error('人机验证加载失败，请检查网络后重试'))
    }
    script.addEventListener('load', onLoad, { once: true })
    script.addEventListener('error', onError, { once: true })
    if (!existing) {
      script.src = TURNSTILE_SRC
      script.async = true
      document.head.appendChild(script)
    }
  })
  return turnstileLoadPromise
}
