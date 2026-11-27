import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// 开发态代理目标。
//
// 生产环境前端走同源相对路径 /api/*，由 web 容器里的 Caddy 转发到 api:8080，
// 因此这里的目标只在 `npm run dev` 本地开发时生效。
//
// 默认 127.0.0.1:8080 与 docker-compose 里 API 的默认发布端口保持一致；
// 如果你把本地 Go 进程跑在别的端口，用 VITE_API_PROXY 覆盖即可，例如：
//   VITE_API_PROXY=http://127.0.0.1:18080 npm run dev
const apiProxyTarget = process.env.VITE_API_PROXY ?? 'http://127.0.0.1:8080'

export default defineConfig({
  plugins: [react()],
  build: { rollupOptions: { output: { manualChunks(id) {
    if (id.includes('node_modules')) {
      if (id.includes('@xterm')) return 'terminal-vendor'
      if (id.includes('lucide-react')) return 'icons'
      if (/[/\\](react|react-dom|scheduler)[/\\]/.test(id)) return 'react-vendor'
    }
  } } } },
  server: {
    proxy: {
      '/api': {
        target: apiProxyTarget,
        // SSH 终端走同源 WebSocket；保留浏览器 Host 头以便后端与 Origin 核验而不轻信转发头。
        changeOrigin: false,
        ws: true,
      },
    },
  },
})
