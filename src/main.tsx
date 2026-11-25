import React, { lazy, Suspense } from 'react'
import ReactDOM from 'react-dom/client'
import { readDeliveryToken } from './lib/deliveryRoute'
import { applyAppearance, loadScale, loadTheme } from './lib/theme'
import { ErrorBoundary, ViewLoadingFallback } from './components/ErrorBoundary'
import './styles.css'

// 买家打开交付页（/user/<token>）时只加载交付页自己的代码，不下载整个后台
// （2026-10-30：原来交付页要拉 475KB 的后台主包，也把后台界面代码暴露给了每个买家）。
// 后台照旧加载 App；App 内部仍保留交付页分支，hash 路由等边界行为不变。
const deliveryToken = readDeliveryToken()

const Root = deliveryToken
  ? lazy(() => import('./components/DeliveryPage').then((m) => ({
    default: function DeliveryRoot() { return <m.DeliveryPage token={deliveryToken} /> },
  })))
  : lazy(() => import('./App'))

// 首屏渲染前即同步应用已保存的主题与字号，消除后台暗色/日间主题切换闪烁。
applyAppearance(loadTheme(), loadScale())

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <ErrorBoundary scope={deliveryToken ? '订阅交付页' : '控制台'}>
      <Suspense fallback={<div className="root-loading-shell"><ViewLoadingFallback label={deliveryToken ? '正在加载订阅页面…' : '正在加载控制台…'} /></div>}>
        <Root />
      </Suspense>
    </ErrorBoundary>
  </React.StrictMode>,
)
