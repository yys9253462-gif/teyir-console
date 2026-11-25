import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { AlertTriangle, RefreshCw, ShieldCheck, TrendingDown, TrendingUp, Users } from 'lucide-react'
import { api, isAdminSession, type ProfitReport, type SharingItem } from '../lib/api'
import { formatBytes } from '../lib/format'
import { CURRENCY_SYMBOL } from './HostTools'

// 收益报表（2026-10-15）：单节点利润 + 订阅月收入维护 + 账号共享检测。
//
// 收入按每个订阅在各节点的实测流量占比分摊；主机月费在同主机的节点间平均分摊。
// 各币种分别汇总，不做汇率换算。

function money(m: Record<string, number> | undefined): string {
  if (!m) return '—'
  const parts = Object.entries(m).filter(([, v]) => Math.abs(v) >= 0.005)
  if (!parts.length) return '—'
  return parts.map(([c, v]) => `${v < 0 ? '-' : ''}${CURRENCY_SYMBOL[c] ?? ''}${Math.abs(v).toFixed(2)}${parts.length > 1 || c !== 'CNY' ? ` ${c}` : ''}`).join(' + ')
}

// 2026-11-06：跨币种不能压成一个结论。后端把利润按币种分开累加，而本项目的常见姿势是
// 「VPS 月费填 USD、订阅收入填 CNY」，于是 {CNY:+120, USD:-10} 会被原来的 any-negative 判成亏损 ——
// 一台在 CNY 口径下明显赚钱的节点被标成「在亏钱」。只有所有币种同向时才下结论。
function sign(m: Record<string, number> | undefined): number {
  if (!m) return 0
  const vals = Object.values(m).filter((v) => Math.abs(v) >= 0.005)
  if (!vals.length) return 0
  if (vals.every((v) => v < 0)) return -1
  if (vals.every((v) => v > 0)) return 1
  return 0
}

export function ProfitPage({ notify, readOnly }: { notify: (m: string) => void; readOnly: boolean }) {
  // 2026-11-06：收益报表含主机成本与订阅收入，后端是 requireAdmin。原来对 operator / viewer 也渲染整页：
  // 他们只能看到「读取失败 需要管理员角色 (403)」加一堆空卡片，这个 readOnly prop 也就形同虚设。
  // 现在直接说清为什么看不到（不扩大读取权限，那属于产品决策）。
  const admin = isAdminSession()
  const [report, setReport] = useState<ProfitReport | null>(null)
  const [sharing, setSharing] = useState<SharingItem[]>([])
  // 2026-11-04：共享检测单独记错误。原来失败被吞成空列表，KPI 显示「疑似共享 0」、
  // 区块写「没有发现疑似共享的订阅」—— 读不到被说成了查过没有。
  const [sharingError, setSharingError] = useState('')
  const [error, setError] = useState('')
  const [drafts, setDrafts] = useState<Record<string, { v: string; c: string }>>({})
  const [saving, setSaving] = useState('')
  const mountedRef = useRef(true)
  const loadSeq = useRef(0)

  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      loadSeq.current += 1
    }
  }, [])

  const load = useCallback(async () => {
    // 非管理员不必发这个请求：接口是 requireAdmin，发了只会拿一个 403（页面本身已显示说明）。
    if (!isAdminSession()) return
    const seq = ++loadSeq.current
    try {
      let sharingErr = ''
      const [r, s] = await Promise.all([api.profitReport(), api.sharing().catch((err) => {
        sharingErr = err instanceof Error ? err.message : '读取失败'
        return null
      })])
      if (!mountedRef.current || seq !== loadSeq.current) return
      setReport({ ...r, nodes: r.nodes ?? [], subscriptions: r.subscriptions ?? [] })
      if (s) setSharing(s.items ?? [])
      setSharingError(sharingErr)
      setError('')
    } catch (err) {
      if (!mountedRef.current || seq !== loadSeq.current) return
      setError(err instanceof Error ? err.message : '读取失败')
    }
  }, [])

  useEffect(() => { void load() }, [load])

  async function saveRevenue(id: string) {
    if (saving) return
    const d = drafts[id]
    if (!d) return
    const v = d.v.trim() === '' ? null : Number(d.v)
    if (v !== null && (!Number.isFinite(v) || v < 0)) { notify('月收入需为有限非负数'); return }
    setSaving(id)
    try {
      await api.setSubscriptionRevenue(id, v, d.c)
      if (!mountedRef.current) return
      setDrafts((m) => { const n = { ...m }; delete n[id]; return n })
      notify('月收入已保存')
      await load()
    } catch (err) {
      if (!mountedRef.current) return
      notify(err instanceof Error ? err.message : '保存失败')
    } finally {
      if (mountedRef.current) setSaving('')
    }
  }

  // 不同币种不能直接相加（10 美元 ≠ 10 元）。没有汇率时按币种逐个比较：
  // 先比 CNY，相同再比 USD，再比其它币种；这样排序不会被币种混算带偏。
  const ranked = useMemo(() => {
    const currencies = Array.from(new Set((report?.nodes ?? []).flatMap((n) => Object.keys(n.profit))))
      .sort((x, y) => (x === 'CNY' ? -1 : y === 'CNY' ? 1 : x === 'USD' ? -1 : y === 'USD' ? 1 : x.localeCompare(y)))
    return [...(report?.nodes ?? [])].sort((a, b) => {
      for (const c of currencies) {
        const diff = (b.profit[c] ?? 0) - (a.profit[c] ?? 0)
        if (Math.abs(diff) > 0.005) return diff
      }
      return 0
    })
  }, [report])
  const losing = ranked.filter((n) => sign(n.profit) < 0)

  if (!admin) {
    return (
      <section className="panel settings-panel">
        <div className="empty-state">
          <ShieldCheck size={20} />
          <strong>收益报表仅管理员可见</strong>
          <span>这一页包含各主机的月成本与每条订阅的月收入（属于经营数据）。如需查看，请让管理员在「系统设置 → 账号」里把你的角色改成 admin。</span>
        </div>
      </section>
    )
  }

  return (
    <>
      <div className="page-heading">
        <div>
          <div className="eyebrow"><span className="eyebrow-line" />REVENUE / 06</div>
          <h1>收益报表<span className="heading-period">.</span></h1>
          <p className="page-subtitle">每台节点每月赚多少、哪台在亏钱，以及疑似共享的账号</p>
        </div>
        <div className="heading-actions"><button className="button button-quiet" onClick={() => void load()}><RefreshCw size={15} />刷新</button></div>
      </div>

      {error && <div className="drawer-notice notice-error" style={{ marginBottom: 14 }}><AlertTriangle size={17} /><span><strong>读取失败</strong><small>{error}</small></span></div>}

      {report && <>
        <section className="metrics-grid settings-kpis" aria-label="收益概览">
          <div className="metric-card"><div className="metric-head"><span>月收入</span><TrendingUp size={17} /></div><div className="metric-body"><strong className="metric-money">{money(report.totals.revenue)}</strong></div><div className="metric-foot">{report.subscriptions_with_revenue}/{report.subscriptions_total} 个订阅已填收入{money(report.unallocated_revenue) !== '—' && <span className="metric-warn" title={`以下订阅没有可分摊的在役节点（开户待补或节点已移除），收入已计入合计、不计入任何节点：${(report.unallocated_subscription_ids ?? []).map((id) => id.slice(0, 8)).join('、')}`}> · 其中 {money(report.unallocated_revenue)} 未分摊到节点</span>}</div></div>
          <div className="metric-card"><div className="metric-head"><span>月成本</span><TrendingDown size={17} /></div><div className="metric-body"><strong className="metric-money">{money(report.totals.cost)}</strong></div><div className="metric-foot">来自 VPS 主机页的月费{money(report.unallocated_cost) !== '—' && <span className="metric-warn" title="这些主机上暂时没有在役节点，月费已计入合计、不计入任何节点"> · 其中 {money(report.unallocated_cost)} 来自无节点的主机</span>}</div></div>
          <div className="metric-card"><div className="metric-head"><span>月利润</span><TrendingUp size={17} /></div><div className="metric-body"><strong className={`metric-money ${sign(report.totals.profit) < 0 ? 'lat-bad' : 'lat-good'}`}>{money(report.totals.profit)}</strong></div><div className="metric-foot">{losing.length ? <span className="metric-warn">{losing.length} 台节点在亏钱</span> : '没有亏钱的节点'}</div></div>
          <div className="metric-card"><div className="metric-head"><span>疑似共享</span><Users size={17} /></div><div className="metric-body"><strong className={sharing.length && !sharingError ? 'lat-bad' : ''}>{sharingError ? '—' : sharing.length}</strong><span className="metric-unit">个订阅</span></div><div className="metric-foot">{sharingError ? '共享检测读取失败' : '按同时在线网段数判断'}</div></div>
        </section>

        {report.subscriptions_with_revenue === 0 && (
          <p className="form-note form-note-hint" style={{ marginBottom: 14 }}><AlertTriangle size={13} /><span>还没有任何订阅填写收入，所以收入与利润都是 0。在下方「订阅月收入」逐条填写；或让商城下单 / 续费时在请求里带上 <code>amount</code> 与 <code>currency</code>，面板会按订单时长自动折算成月收入。</span></p>
        )}

        <section className="panel settings-panel" style={{ marginBottom: 14 }}>
          <div className="section-head"><div><div className="section-kicker">NODES <span>·</span> {ranked.length}</div><h2>单节点利润</h2></div></div>
          <div className="server-table-wrap"><table className="server-table profit-table">
            <thead><tr><th>节点</th><th>月收入</th><th>月成本</th><th>月利润</th><th>付费订阅</th><th>累计流量</th></tr></thead>
            <tbody>{ranked.map((n) => (
              <tr key={n.node_id}>
                <td><div className="host-cell"><div><strong>{n.node_name}</strong><span>{n.server_name}{n.cost_shared_by > 1 ? ` · 与 ${n.cost_shared_by - 1} 个节点分摊主机费` : ''}</span></div></div></td>
                <td><span className="traffic-value">{money(n.revenue)}</span></td>
                <td><span className="traffic-value">{Object.keys(n.cost).length ? money(n.cost) : <span className="traffic-sub">未填月费</span>}</span></td>
                <td><span className={`traffic-value ${sign(n.profit) < 0 ? 'lat-bad' : sign(n.profit) > 0 ? 'lat-good' : ''}`}>{money(n.profit)}</span></td>
                <td><span className="traffic-sub">{n.subscribers}</span></td>
                <td><span className="traffic-sub">{formatBytes(n.traffic_bytes, { decimals: 1, zero: '—' })}</span></td>
              </tr>
            ))}</tbody>
          </table></div>
          <p className="form-note">月收入按每个订阅在当前节点的累计实测流量占比分摊（没有流量数据时平均分摊）；各币种分别汇总，不做汇率换算。</p>
        </section>

        <section className="panel settings-panel" style={{ marginBottom: 14 }}>
          <div className="section-head"><div><div className="section-kicker">SHARING <span>·</span> {sharing.length}</div><h2>账号共享检测</h2></div></div>
          {sharingError ? <p className="form-error">共享检测读取失败：{sharingError}。刷新页面重试。</p> : sharing.length === 0 ? <p className="form-note">没有发现疑似共享的订阅。判定：同一时间从 ≥3 个不同网段在线（很可能共享），或最近 2 小时内出现在 ≥4 个不同网段（疑似共享）。</p> : (
            <div className="server-table-wrap"><table className="server-table">
              <thead><tr><th>订阅</th><th>判定</th><th>同时在线网段</th><th>近 2 小时网段</th><th>在线网络上限</th><th>当前在线 IP</th></tr></thead>
              <tbody>{sharing.map((s) => (
                <tr key={s.subscription_id}>
                  <td><div className="host-cell"><div><strong>{s.label || s.order_id || s.subscription_id.slice(0, 8)}</strong><span>{s.order_id}</span></div></div></td>
                  <td><span className={`expiry-badge tone-${s.level === 'likely' ? 'bad' : 'warn'}`}>{s.level === 'likely' ? '很可能共享' : '疑似共享'}</span><small className="traffic-sub">{s.reason}</small></td>
                  <td><span className="traffic-value">{s.live_networks}</span></td>
                  <td><span className="traffic-value">{s.day_networks}</span></td>
                  <td><span className="traffic-sub">{s.ip_limit > 0 ? s.ip_limit : '不限'}</span></td>
                  <td><span className="traffic-sub mono">{s.live_ips?.join(', ') || '—'}</span></td>
                </tr>
              ))}</tbody>
            </table></div>
          )}
        </section>

        <section className="panel settings-panel">
          <div className="section-head"><div><div className="section-kicker">SUBSCRIPTIONS <span>·</span> {report.subscriptions.length}</div><h2>订阅月收入</h2></div></div>
          <div className="server-table-wrap"><table className="server-table">
            <thead><tr><th>订阅 / 订单</th><th>到期</th><th>月收入</th><th aria-label="操作" /></tr></thead>
            <tbody>{report.subscriptions.map((sub) => {
              const d = drafts[sub.id] ?? { v: sub.revenue_monthly != null ? String(sub.revenue_monthly) : '', c: sub.revenue_currency || 'CNY' }
              const dirty = Boolean(drafts[sub.id])
              return (
                <tr key={sub.id}>
                  <td><div className="host-cell"><div><strong>{sub.label || sub.order_id || sub.id.slice(0, 8)}</strong><span>{sub.order_id}</span></div></div></td>
                  <td><span className="traffic-sub">{sub.expires_at ? (() => { const dt = new Date(sub.expires_at); return Number.isNaN(dt.getTime()) ? '—' : dt.toLocaleDateString('zh-CN') })() : '不过期'}</span></td>
                  <td>
                    <div className="revenue-input">
                      <input inputMode="decimal" value={d.v} disabled={readOnly} placeholder="未填写" aria-label="月收入"
                        onChange={(e) => setDrafts((m) => ({ ...m, [sub.id]: { ...d, v: e.target.value } }))} />
                      <select value={d.c} disabled={readOnly} aria-label="币种" onChange={(e) => setDrafts((m) => ({ ...m, [sub.id]: { ...d, c: e.target.value } }))}>
                        {Object.keys(CURRENCY_SYMBOL).map((c) => <option key={c} value={c}>{c}</option>)}
                      </select>
                    </div>
                  </td>
                  <td>{dirty && !readOnly && <button className="row-action" disabled={saving === sub.id} onClick={() => void saveRevenue(sub.id)}>{saving === sub.id ? '保存中' : '保存'}</button>}</td>
                </tr>
              )
            })}</tbody>
          </table></div>
        </section>
      </>}
    </>
  )
}
