import { useState } from 'react'
import { CalendarClock, ChevronRight, Download, Plug, Route, ShieldCheck, Shuffle, Sparkles, X } from 'lucide-react'
import type { Server } from '../lib/api'
import { Modal } from './Modal'

// VPS 主机页顶部的「主机工具」卡片（2026-10-15）。
//
// 这些功能原来藏在表格行末的小图标、一列要点开的单元格、和「批量预设任务」
// 弹窗的第二层里 —— 实测用户上线后一个都没找到。这里把它们摆成一排大卡片，
// 点卡片 → 选主机（不满足条件的主机置灰并写明原因）→ 打开对应功能。

export type ToolKey = 'install' | 'migrate' | 'test' | 'billing' | 'changeip' | 'security_audit' | 'net_route'

export type ToolDef = {
  key: ToolKey
  title: string
  desc: string
  Icon: typeof Plug
  /** 返回空串表示这台主机可用，否则是不可用的原因。 */
  blocked: (s: Server) => string
  /** 批量类工具直接打开批量预设任务，不逐台选。 */
  batch?: boolean
  tone?: 'accent'
}

export function HostToolbox({ tools, onPick }: { tools: ToolDef[]; onPick: (tool: ToolDef) => void }) {
  return (
    <section className="host-toolbox" aria-label="主机工具">
      <div className="host-toolbox-head">
        <div className="section-kicker">TOOLS</div>
        <h2>主机工具</h2>
      </div>
      <div className="host-toolbox-grid">
        {tools.map((t) => (
          <button key={t.key} className={`host-tool${t.tone === 'accent' ? ' accent' : ''}`} onClick={() => onPick(t)}>
            <span className="host-tool-icon"><t.Icon size={18} /></span>
            <span className="host-tool-text"><strong>{t.title}</strong><small>{t.desc}</small></span>
            <ChevronRight size={15} className="host-tool-go" />
          </button>
        ))}
      </div>
    </section>
  )
}

export function HostPicker({ tool, servers, onClose, onChoose }: {
  tool: ToolDef
  servers: Server[]
  onClose: () => void
  onChoose: (s: Server) => void
}) {
  const ready = servers.filter((s) => !tool.blocked(s))
  return (
    <Modal title={tool.title} kicker="CHOOSE HOST" onClose={onClose}>
      <div style={{ display: 'grid', gap: 10 }}>
        <p className="form-note">{tool.desc}。选择一台主机：</p>
        {ready.length === 0 && <p className="form-note form-note-hint"><span>当前没有满足条件的主机。看下面每台的原因；新买的 VPS 先点右上角「添加主机」并上传 SSH 凭据。</span></p>}
        <div className="host-picker">
          {servers.map((s) => {
            const reason = tool.blocked(s)
            return (
              <button key={s.id} className="host-picker-row" disabled={Boolean(reason)} onClick={() => onChoose(s)}>
                <span><strong>{s.name}</strong><small>{s.host}</small></span>
                {reason ? <small className="host-picker-reason">{reason}</small> : <ChevronRight size={15} />}
              </button>
            )
          })}
        </div>
      </div>
    </Modal>
  )
}

export const TOOL_ICONS = { Download, Plug, CalendarClock, Shuffle, ShieldCheck, Route }

// ---------------------------------------------------------------------------
// 全局「新功能」横幅：版本号变了才重新出现，关掉后记在 localStorage（只是个人便利）。

const WHATS_NEW_VERSION = '2026-10-16c'
const WHATS_NEW_KEY = 'nodeops.whatsnew.dismissed'

export function WhatsNewBanner({ onGo }: { onGo: (page: string) => void }) {
  const [hidden, setHidden] = useState(() => {
    try { return localStorage.getItem(WHATS_NEW_KEY) === WHATS_NEW_VERSION } catch { return false }
  })
  if (hidden) return null
  function dismiss() {
    setHidden(true)
    try { localStorage.setItem(WHATS_NEW_KEY, WHATS_NEW_VERSION) } catch { /* 个人便利，失败无妨 */ }
  }
  const items: { label: string; page: string }[] = [
    { label: '定时巡检', page: '系统设置' },
    { label: '登录安全提醒', page: '系统设置' },
    { label: 'Telegram 告警', page: '系统设置' },
    { label: '故障节点自动下线', page: '节点监控' },
    { label: '收益报表', page: '收益报表' },
    { label: '迁移节点', page: 'VPS 主机' },
    { label: '被墙检测', page: '节点监控' },
    { label: '一键安装节点', page: 'VPS 主机' },
    { label: '到期提醒与成本', page: 'VPS 主机' },
    { label: '安全检查 / 线路检测', page: 'VPS 主机' },
    { label: '节点出口设置', page: 'HY2 节点' },
    { label: '原生 IP / 流媒体解锁', page: 'IP 纯净度' },
    { label: 'Cloudflare 集成', page: '系统设置' },
  ]
  return (
    <div className="whats-new" role="status">
      <Sparkles size={16} className="whats-new-icon" />
      <strong>新功能</strong>
      <div className="whats-new-items">
        {items.map((it) => <button key={it.label} onClick={() => onGo(it.page)}>{it.label}</button>)}
      </div>
      <button className="whats-new-close" aria-label="关闭新功能提示" onClick={dismiss}><X size={15} /></button>
    </div>
  )
}
