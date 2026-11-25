import { useEffect, useMemo, useRef, useState } from 'react'
import { api, type Server, type ServerBilling, type ServerTrendPoint } from '../lib/api'
import { Modal } from './Modal'
import { askConfirm } from './ConfirmHost'

// VPS 主机页的实用工具（2026-10-15）：到期与费用、换 IP 同步、资源趋势图。

export const CURRENCY_SYMBOL: Record<string, string> = { USD: '$', CNY: '¥', EUR: '€', HKD: 'HK$', JPY: '¥' }

/** 距到期还有几天（按本地日期）；未填写或格式异常返回 null。 */
export function daysUntil(expiresOn: string | null | undefined): number | null {
  if (!expiresOn) return null
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(expiresOn.trim())
  if (!match) return null
  const y = Number(match[1])
  const m = Number(match[2])
  const d = Number(match[3])
  const endDate = new Date(y, m - 1, d)
  if (endDate.getFullYear() !== y || endDate.getMonth() !== m - 1 || endDate.getDate() !== d) return null
  const end = endDate.getTime()
  const today = new Date(); today.setHours(0, 0, 0, 0)
  return Math.round((end - today.getTime()) / 86_400_000)
}

function isValidIPv4(raw: string): boolean {
  const parts = raw.trim().split('.')
  if (parts.length !== 4) return false
  return parts.every((part) => /^(0|[1-9]\d{0,2})$/.test(part) && Number(part) <= 255)
}

export function ExpiryBadge({ billing }: { billing?: ServerBilling }) {
  const days = daysUntil(billing?.expires_on)
  if (days === null) return <span className="traffic-sub">未填写</span>
  const tone = billing?.auto_renew ? 'muted' : days < 0 ? 'bad' : days <= 7 ? 'warn' : 'ok'
  const text = days < 0 ? `已过期 ${-days} 天` : days === 0 ? '今天到期' : `${days} 天后`
  return (
    <span className={`expiry-badge tone-${tone}`} title={`到期日 ${billing?.expires_on}${billing?.auto_renew ? ' · 自动续费' : ''}`}>
      {text}{billing?.auto_renew ? ' · 自动续费' : ''}
    </span>
  )
}

export function BillingDialog({ server, billing, onClose, onSaved, notify }: {
  server: Server
  billing?: ServerBilling
  onClose: () => void
  onSaved: (b: ServerBilling) => void
  notify: (m: string) => void
}) {
  const [expires, setExpires] = useState(billing?.expires_on ?? '')
  const [cost, setCost] = useState(billing?.monthly_cost != null ? String(billing.monthly_cost) : '')
  const [currency, setCurrency] = useState(billing?.currency ?? 'USD')
  const [autoRenew, setAutoRenew] = useState(billing?.auto_renew ?? false)
  const [note, setNote] = useState(billing?.note ?? '')
  const [busy, setBusy] = useState(false)
  const mountedRef = useRef(true)
  useEffect(() => {
    mountedRef.current = true
    return () => { mountedRef.current = false }
  }, [])
  const trimmedCost = cost.trim()
  const costOk = trimmedCost === '' || (Number.isFinite(Number(trimmedCost)) && Number(trimmedCost) >= 0)

  async function save() {
    setBusy(true)
    try {
      const saved = await api.setServerBilling(server.id, {
        expires_on: expires || null, monthly_cost: trimmedCost === '' ? null : Number(trimmedCost),
        currency, auto_renew: autoRenew, note,
      })
      notify(`已保存 ${server.name} 的到期与费用`)
      onSaved({ ...saved, server_id: server.id })
      if (mountedRef.current) onClose()
    } catch (err) {
      notify(err instanceof Error ? err.message : '保存失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  return (
    <Modal title={`到期与费用 · ${server.name}`} kicker="BILLING" onClose={onClose}>
      <div style={{ display: 'grid', gap: 12 }}>
        <label className="form-field"><span>到期日（到期前 7 天在告警中心提醒）</span><input type="date" value={expires} onChange={(e) => setExpires(e.target.value)} /></label>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 110px', gap: 10 }}>
          <label className="form-field"><span>月费（年付请折算成月均）</span><input inputMode="decimal" value={cost} onChange={(e) => setCost(e.target.value)} placeholder="例如 5" aria-invalid={!costOk} /></label>
          <label className="form-field"><span>币种</span>
            <select value={currency} onChange={(e) => setCurrency(e.target.value)}>
              {Object.keys(CURRENCY_SYMBOL).map((c) => <option key={c} value={c}>{c}</option>)}
            </select>
          </label>
        </div>
        <label style={{ display: 'flex', gap: 8, alignItems: 'center' }}><input type="checkbox" checked={autoRenew} onChange={(e) => setAutoRenew(e.target.checked)} />已开自动续费（不再提醒到期）</label>
        <label className="form-field"><span>备注（可选，如商家、套餐）</span><input value={note} onChange={(e) => setNote(e.target.value)} maxLength={200} /></label>
        <div className="modal-actions">
          <button className="button button-quiet" onClick={onClose}>取消</button>
          <button className="button button-primary" disabled={busy || !costOk} onClick={() => void save()}>{busy ? '保存中…' : '保存'}</button>
        </div>
      </div>
    </Modal>
  )
}

export function ChangeIPDialog({ server, onClose, onDone, notify }: {
  server: Server
  onClose: () => void
  onDone: () => void
  notify: (m: string) => void
}) {
  const [ip, setIp] = useState('')
  const [updateDNS, setUpdateDNS] = useState(true)
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<Awaited<ReturnType<typeof api.changeServerIP>> | null>(null)
  const mountedRef = useRef(true)
  useEffect(() => {
    mountedRef.current = true
    return () => { mountedRef.current = false }
  }, [])
  const ipOk = isValidIPv4(ip)

  async function submit() {
    if (!await askConfirm(`把 ${server.name} 的地址从 ${server.host} 改为 ${ip.trim()}？`)) return
    setBusy(true)
    try {
      const r = await api.changeServerIP(server.id, ip.trim(), updateDNS)
      if (mountedRef.current) setResult(r)
      notify(r.dns_ok === false ? `${server.name} 的地址已改为 ${r.new_ip}，但有节点域名 DNS 更新失败，请查看明细` : `${server.name} 的地址已改为 ${r.new_ip}`)
      onDone()
    } catch (err) {
      notify(err instanceof Error ? err.message : '修改失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  return (
    <Modal title={`更换 IP · ${server.name}`} kicker="CHANGE IP" onClose={onClose}>
      {!result ? (
        <div style={{ display: 'grid', gap: 12 }}>
          <p className="form-note">先在云厂商控制台给这台机器换好 IP（或绑定新的弹性 IP），再在这里同步。面板会更新主机地址，并用 Cloudflare 把这台机器上各节点的域名改指向新 IP。</p>
          <label className="form-field"><span>当前地址</span><input value={server.host} disabled /></label>
          <label className="form-field"><span>新的公网 IPv4</span><input value={ip} onChange={(e) => setIp(e.target.value)} placeholder="1.2.3.4" aria-invalid={ip !== '' && !ipOk} /></label>
          <label style={{ display: 'flex', gap: 8, alignItems: 'center' }}><input type="checkbox" checked={updateDNS} onChange={(e) => setUpdateDNS(e.target.checked)} />同时更新节点域名的 Cloudflare A 记录（需在系统设置里填 Cloudflare Token）</label>
          <div className="modal-actions">
            <button className="button button-quiet" onClick={onClose}>取消</button>
            <button className="button button-primary" disabled={busy || !ipOk} onClick={() => void submit()}>{busy ? '同步中…' : '同步新 IP'}</button>
          </div>
        </div>
      ) : (
        <div style={{ display: 'grid', gap: 10 }}>
          <p>地址已更新：<code>{result.old_ip}</code> → <code>{result.new_ip}</code></p>
          {(result.dns ?? []).length > 0 && <ul className="change-ip-dns">
            {(result.dns ?? []).map((d) => <li key={d.node}><strong>{d.node}</strong> <span className="traffic-sub">{d.domain || '—'}</span> · {d.failed ? <strong>{d.result}</strong> : d.result}{d.warning && <span className="traffic-sub">（{d.warning}）</span>}</li>)}
          </ul>}
          <p className="form-note">IP 变了，IP 纯净度与原生判定会显示「已变更」，建议到 IP 纯净度页重新检测。DNS 生效通常需要 1-5 分钟。</p>
          <div className="modal-actions"><button className="button button-primary" onClick={onClose}>完成</button></div>
        </div>
      )}
    </Modal>
  )
}

const SERIES: { key: 'cpu' | 'mem' | 'disk'; label: string; color: string }[] = [
  { key: 'cpu', label: 'CPU', color: 'var(--accent)' },
  { key: 'mem', label: '内存', color: 'var(--green)' },
  { key: 'disk', label: '磁盘', color: 'var(--amber)' },
]

/** 主机详情里的资源趋势（按小时均值）。 */
export function HostTrend({ server }: { server: Server }) {
  const [days, setDays] = useState(7)
  const [points, setPoints] = useState<ServerTrendPoint[] | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    let alive = true
    setPoints(null)
    api.serverTrend(server.id, days)
      .then((r) => { if (alive) { setPoints(r.points ?? []); setError('') } })
      .catch((err) => { if (alive) setError(err instanceof Error ? err.message : '读取失败') })
    return () => { alive = false }
  }, [server.id, days])

  const W = 440, H = 120
  const paths = useMemo(() => {
    const validPoints = (points ?? []).filter((p) => Number.isFinite(Date.parse(p.t)))
    if (validPoints.length < 2) return []
    const t0 = Date.parse(validPoints[0].t)
    const t1 = Date.parse(validPoints[validPoints.length - 1].t) || t0 + 1
    return SERIES.map((s) => {
      let d = ''
      let pen = false
      for (const p of validPoints) {
        const v = p[s.key]
        if (v == null || !Number.isFinite(v)) { pen = false; continue }
        const x = ((Date.parse(p.t) - t0) / Math.max(1, t1 - t0)) * W
        const y = H - (Math.min(100, Math.max(0, v)) / 100) * H
        d += `${pen ? 'L' : 'M'}${x.toFixed(1)},${y.toFixed(1)} `
        pen = true
      }
      const vals = validPoints.map((p) => p[s.key]).filter((v): v is number => v != null && Number.isFinite(v))
      return { ...s, d, max: vals.length ? Math.max(...vals) : null, last: vals.length ? vals[vals.length - 1] : null }
    })
  }, [points])

  return (
    <div className="host-trend">
      <div className="host-trend-head">
        <strong>资源趋势</strong>
        <div className="monitor-filter" role="group" aria-label="时间范围">
          {[1, 7, 30].map((d) => <button key={d} className={days === d ? 'active' : ''} onClick={() => setDays(d)}>{d === 1 ? '24 小时' : `${d} 天`}</button>)}
        </div>
      </div>
      {error ? <p className="form-note">{error}</p>
        : !points ? <p className="form-note">读取中…</p>
          : points.length < 2 ? <p className="form-note">还没有足够的采集数据（需要这台主机配置 SSH 凭据并被采集）。</p>
            : <>
              <svg viewBox={`0 0 ${W} ${H}`} className="host-trend-chart" preserveAspectRatio="none" role="img" aria-label="CPU、内存、磁盘使用率趋势">
                {[25, 50, 75].map((g) => <line key={g} x1="0" x2={W} y1={H - (g / 100) * H} y2={H - (g / 100) * H} className="host-trend-grid" />)}
                {paths.map((p) => <path key={p.key} d={p.d} fill="none" stroke={p.color} strokeWidth="1.6" vectorEffect="non-scaling-stroke" />)}
              </svg>
              <div className="host-trend-legend">
                {paths.map((p) => <span key={p.key}><i style={{ background: p.color }} />{p.label} 当前 {p.last == null ? '—' : `${Math.round(p.last)}%`} · 峰值 {p.max == null ? '—' : `${Math.round(p.max)}%`}</span>)}
              </div>
            </>}
    </div>
  )
}
