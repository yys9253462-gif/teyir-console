import React from 'react'
import { createRoot } from 'react-dom/client'
import {
  createPreviewFetch, previewSession, scenarioLabels, scenarios,
  type PreviewScenario,
} from './fixture'
import '../src/styles.css'

const params = new URLSearchParams(window.location.search)
const requested = params.get('scenario')
const scenario: PreviewScenario = scenarios.find((item) => item === requested) ?? 'abnormal'

// App 读取 nodeops.session；整份 localStorage 都隔离，避免覆盖同源真实开发登录态。
// 主题/字号仅写预览命名空间，刷新切场景仍保留。示例会话只存在内存。
function installPreviewStorage() {
  const nativeStorage = window.localStorage
  const namespace = 'nodeops.design-preview.preferences'
  const values = new Map<string, string>()
  try {
    const saved: unknown = JSON.parse(nativeStorage.getItem(namespace) ?? '{}')
    if (saved && typeof saved === 'object') {
      for (const [key, value] of Object.entries(saved)) {
        if (typeof value === 'string' && key !== 'nodeops.session') values.set(key, value)
      }
    }
    for (const key of ['nodeops.theme', 'nodeops.fontscale']) {
      const current = nativeStorage.getItem(key)
      if (!values.has(key) && current) values.set(key, current)
    }
  } catch { /* 存储不可读时继续使用内存默认值。 */ }
  if (params.get('login') !== '1') values.set('nodeops.session', JSON.stringify(previewSession))
  function persist() {
    try {
      nativeStorage.setItem(namespace, JSON.stringify(Object.fromEntries(
        [...values].filter(([key]) => key !== 'nodeops.session'),
      )))
    } catch { /* 不影响本次预览。 */ }
  }
  const storage: Storage = {
    get length() { return values.size },
    clear() { values.clear(); persist() },
    getItem(key) { return values.get(String(key)) ?? null },
    key(index) { return [...values.keys()][index] ?? null },
    removeItem(key) { values.delete(String(key)); persist() },
    setItem(key, value) { values.set(String(key), String(value)); persist() },
  }
  Object.defineProperty(window, 'localStorage', { configurable: true, value: storage })
}

function installMocks() {
  installPreviewStorage()
  window.fetch = createPreviewFetch(scenario, ({ method, path, message }) => {
    const output = document.getElementById('preview-write-status')!
    output.textContent = `${message} · ${method} ${path}`
  })
  // 真实导航仍可点击。即使打开主机工具，也不能旁路 fetch 建立 SSH 连接。
  const NativeWebSocket = window.WebSocket
  window.WebSocket = class PreviewWebSocket extends NativeWebSocket {
    constructor(url: string | URL, protocols?: string | string[]) {
      const target = new URL(String(url), window.location.href)
      const list = typeof protocols === 'string' ? [protocols] : protocols ?? []
      const viteHMR = list.includes('vite-hmr') && target.host === window.location.host
      if (!viteHMR) throw new Error('本地预览已阻止 WebSocket；不会连接节点或生产服务')
      super(url, protocols)
    }
  }
  window.XMLHttpRequest = class PreviewXMLHttpRequest extends XMLHttpRequest {
    override open(): void { throw new Error('本地预览仅允许 mock fetch，已阻止 XMLHttpRequest') }
  }
  Object.defineProperty(navigator, 'sendBeacon', { configurable: true, value: () => false })
}

function installControls() {
  document.body.dataset.designPreview = scenario
  const controls = document.getElementById('preview-controls')!
  const measureControls = () => document.documentElement.style.setProperty(
    '--preview-bar-height', `${Math.ceil(controls.getBoundingClientRect().height)}px`,
  )
  new ResizeObserver(measureControls).observe(controls)
  measureControls()
  const nav = document.getElementById('preview-scenarios')!
  for (const item of scenarios) {
    const link = document.createElement('a')
    const url = new URL(window.location.href)
    url.searchParams.set('scenario', item)
    link.href = `${url.pathname}${url.search}${url.hash}`
    link.textContent = scenarioLabels[item]
    if (item === scenario) link.setAttribute('aria-current', 'page')
    nav.append(link)
  }
  const loginButton = document.getElementById('preview-login') as HTMLButtonElement
  loginButton.textContent = params.get('login') === '1' ? '进入示例控制台' : '查看登录页'
  loginButton.addEventListener('click', () => {
    const url = new URL(window.location.href)
    if (params.get('login') === '1') url.searchParams.delete('login')
    else url.searchParams.set('login', '1')
    window.location.assign(`${url.pathname}${url.search}`)
  })
}

async function bootstrap() {
  // 必须在动态加载 App 之前完成，避免任何初始化请求落到 Vite 的 /api 代理。
  installMocks()
  installControls()
  const { default: App } = await import('../src/App')
  createRoot(document.getElementById('root')!).render(<React.StrictMode><App /></React.StrictMode>)
}

void bootstrap().catch((error: unknown) => {
  const message = document.createElement('pre')
  message.id = 'preview-fatal'
  message.setAttribute('role', 'alert')
  message.textContent = `本地预览启动失败：${error instanceof Error ? error.message : String(error)}`
  document.getElementById('root')!.replaceChildren(message)
})
