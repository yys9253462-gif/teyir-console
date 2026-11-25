// 交付页路由识别（从 App.tsx 移出，2026-10-30）。
//
// `/user/<token>` 与 portal 发给用户的链接一致；也接受 hash 形式（`#/user/<token>`），
// 这样只会把 `/` 重写到 index.html 的静态托管也能打开。
// 单独成模块是为了让 main.tsx 在**不加载后台代码**的情况下判断要不要只渲染交付页。
export const DELIVERY_TOKEN = /^[A-Za-z0-9_-]{20,128}$/

export function readDeliveryToken(): string {
  const pathMatch = window.location.pathname.match(/^\/user\/([^/]+)\/?$/)
  if (pathMatch && DELIVERY_TOKEN.test(pathMatch[1])) return pathMatch[1]
  const hashMatch = window.location.hash.match(/^#\/user\/([^/?]+)/)
  if (hashMatch && DELIVERY_TOKEN.test(hashMatch[1])) return hashMatch[1]
  return ''
}
