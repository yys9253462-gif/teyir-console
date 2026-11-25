import { lazy, Suspense, useState } from 'react'
import type { Server } from '../lib/api'
import { HostDockerPanel, HostFilesPanel } from './HostReadOnlyPanels'
import './HostWorkbench.css'

const HostTerminal = lazy(() => import('./HostTerminal').then((module) => ({ default: module.HostTerminal })))

type Tab = 'docker' | 'compose' | 'files' | 'terminal'
const tabs: { id: Tab; label: string }[] = [
  { id: 'docker', label: 'Docker 容器' },
  { id: 'compose', label: 'Compose 项目' },
  { id: 'files', label: '文件浏览' },
  { id: 'terminal', label: 'SSH 终端' },
]

export function HostWorkbench({ server, role }: { server: Server; role: string }) {
  const [tab, setTab] = useState<Tab>('docker')
  const reason = role !== 'admin'
    ? '主机运维工作台仅管理员可操作，请使用管理员账号。'
    : !server.has_credential
      ? '主机未配置 SSH 凭据，请先在上方保存主机 SSH 凭据。'
      : ''
  return <section className="host-workbench" aria-label="主机运维工作台">
    <h3>主机运维工作台</h3>
    <p>Docker、Compose 与文件浏览仅只读；终端为真实交互 shell，可能修改主机。</p>
    {reason && <div className="host-ops-error" role="status">{reason}</div>}
    <div className="host-ops-tabs" role="tablist" aria-label="主机运维功能">
      {tabs.map(({ id, label }) => <button key={id} id={`host-tab-${id}`} className="button button-quiet" type="button" role="tab" aria-selected={tab === id} aria-controls="host-ops-content" tabIndex={tab === id ? 0 : -1} onKeyDown={(event) => {
        const index = tabs.findIndex((item) => item.id === tab)
        const next = event.key === 'ArrowRight' ? (index + 1) % tabs.length : event.key === 'ArrowLeft' ? (index + tabs.length - 1) % tabs.length : event.key === 'Home' ? 0 : event.key === 'End' ? tabs.length - 1 : -1
        if (next >= 0) {
          event.preventDefault()
          setTab(tabs[next].id)
          document.getElementById(`host-tab-${tabs[next].id}`)?.focus()
        }
      }} onClick={() => setTab(id)}>{label}</button>)}
    </div>
    <div id="host-ops-content" role="tabpanel" aria-labelledby={`host-tab-${tab}`}>
      {reason ? <button className="button button-quiet" disabled>{tab === 'terminal' ? '连接终端' : '刷新'}（不可用）</button>
        : tab === 'terminal' ? <Suspense fallback={<p role="status">加载终端组件…</p>}><HostTerminal key={server.id} serverId={server.id} /></Suspense>
          : tab === 'files' ? <HostFilesPanel key={server.id} serverId={server.id} />
            : <HostDockerPanel key={`${server.id}-${tab}`} serverId={server.id} kind={tab} />}
    </div>
  </section>
}
