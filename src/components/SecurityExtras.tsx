import { useCallback, useEffect, useRef, useState } from 'react'
import { Loader2, Play } from 'lucide-react'
import { api, isAdminSession, ADMIN_ONLY_HINT, type DeliveryDomainState, type LoginRecord, type ScheduleItem } from '../lib/api'
import { askConfirm } from './ConfirmHost'

// 系统设置里的三块（2026-10-16）：最近登录、定时巡检、商城接口签名。

function formatFullTime(iso: string | null | undefined): string {
  if (!iso) return '—'
  const date = new Date(iso)
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString('zh-CN', { hour12: false })
}

export function RecentLogins() {
  const [items, setItems] = useState<LoginRecord[] | null>(null)
  const [policy, setPolicy] = useState('')
  const [loadError, setLoadError] = useState('')
  useEffect(() => {
    let alive = true
    // 2026-11-06：读取失败不能显示成「还没有记录」—— 那会让人以为最近没有登录，正是排查异地登录时最不该出现的结论。
    api.myLogins().then((r) => { if (!alive) return; setItems(r.items ?? []); setPolicy(r.ban_policy); setLoadError('') })
      .catch((err) => { if (!alive) return; setItems([]); setLoadError(err instanceof Error ? err.message : '读取失败') })
    return () => { alive = false }
  }, [])
  return (
    <section className="panel settings-panel">
      <div className="section-head"><div><div className="section-kicker">SESSIONS</div><h2>最近登录</h2></div></div>
      <p className="form-note">新 IP 登录、以及{policy || '连续登录失败封禁'}时，会推送到 Telegram（需在集成密钥里配置）。</p>
      {loadError ? <p className="form-note service-warn">⚠ 登录记录读取失败（{loadError}），下面不代表没有登录，请稍后刷新。</p> : !items ? <p className="form-note">读取中…</p> : items.length === 0 ? <p className="form-note">还没有记录（这项功能上线后的登录才会记录）。</p> : (
        <div className="server-table-wrap"><table className="server-table">
          <thead><tr><th>IP</th><th>设备</th><th>首次</th><th>最近</th><th>次数</th></tr></thead>
          <tbody>{items.map((it) => (
            <tr key={it.ip}>
              <td><span className="traffic-value">{it.ip}</span>{it.current && <span className="status-pill online" style={{ marginLeft: 6 }}>当前</span>}</td>
              <td><span className="traffic-sub" title={it.user_agent}>{shortUA(it.user_agent)}</span></td>
              <td><span className="traffic-sub">{formatFullTime(it.first_seen_at)}</span></td>
              <td><span className="traffic-sub">{formatFullTime(it.last_seen_at)}</span></td>
              <td><span className="traffic-sub">{it.login_count}</span></td>
            </tr>
          ))}</tbody>
        </table></div>
      )}
    </section>
  )
}

function shortUA(ua: string): string {
  const browser = /Edg\//.test(ua) ? 'Edge' : /Chrome\//.test(ua) ? 'Chrome' : /Firefox\//.test(ua) ? 'Firefox' : /Safari\//.test(ua) ? 'Safari' : '浏览器'
  const os = /Windows/.test(ua) ? 'Windows' : /Mac OS X/.test(ua) ? 'macOS' : /Android/.test(ua) ? 'Android' : /iPhone|iPad/.test(ua) ? 'iOS' : /Linux/.test(ua) ? 'Linux' : ''
  return `${browser}${os ? ` · ${os}` : ''}`
}

export function SchedulesCard({ notify }: { notify: (m: string) => void }) {
  const admin = isAdminSession()
  const [items, setItems] = useState<ScheduleItem[]>([])
  // null = 没读到绑定状态（不是「未配置」）。
  const [tg, setTg] = useState<boolean | null>(true)
  const [busy, setBusy] = useState('')
  // 2026-11-06：读取失败要显示出来。原来静默吞掉，列表为空，看起来像「没有任何定时巡检」。
  const [loadError, setLoadError] = useState('')
  const mountedRef = useRef(true)
  const loadSeq = useRef(0)
  const refreshTimerRef = useRef<number | undefined>(undefined)
  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      loadSeq.current += 1
      window.clearTimeout(refreshTimerRef.current)
    }
  }, [])

  const load = useCallback(async () => {
    const seq = ++loadSeq.current
    try {
      const r = await api.schedules()
      if (!mountedRef.current || seq !== loadSeq.current) return
      setItems(r.items ?? []); setTg(r.telegram_configured); setLoadError('')
    } catch (err) {
      if (!mountedRef.current || seq !== loadSeq.current) return
      setLoadError(err instanceof Error ? err.message : '读取失败')
    }
  }, [])
  useEffect(() => { void load() }, [load])

  const anyRunning = items.some((it) => it.running)
  useEffect(() => {
    if (!anyRunning) return
    const timer = window.setInterval(() => {
      if (!document.hidden) void load()
    }, 5000)
    const onVisible = () => { if (!document.hidden) void load() }
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      window.clearInterval(timer)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [anyRunning, load])

  async function toggle(it: ScheduleItem) {
    if (!admin || busy) return
    setBusy(it.key)
    try {
      const r = await api.setSchedule(it.key, !it.enabled)
      if (mountedRef.current) setItems(r.items ?? [])
    } catch (err) {
      if (mountedRef.current) notify(err instanceof Error ? err.message : '设置失败')
    } finally {
      if (mountedRef.current) setBusy('')
    }
  }
  async function runNow(it: ScheduleItem) {
    if (!admin || busy) return
    setBusy(it.key)
    try {
      await api.runSchedule(it.key)
      if (!mountedRef.current) return
      notify(`已开始「${it.label}」，结果有变化会推送到 Telegram`)
      window.clearTimeout(refreshTimerRef.current)
      refreshTimerRef.current = window.setTimeout(() => { if (mountedRef.current) void load() }, 1500)
    } catch (err) {
      if (mountedRef.current) notify(err instanceof Error ? err.message : '启动失败')
    } finally {
      if (mountedRef.current) setBusy('')
    }
  }
  const fmt = (t: string | null) => {
    if (!t) return '—'
    const d = new Date(t)
    return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString('zh-CN', { hour12: false, month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
  }

  return (
    <section className="panel settings-panel">
      <div className="section-head"><div><div className="section-kicker">SCHEDULES</div><h2>定时巡检</h2></div></div>
      <p className="form-note">自动按周期检测，<strong>结果有变化才推送</strong> Telegram，平时不打扰。{tg === false && <span style={{ color: 'var(--amber)' }}> 还没配置 Telegram，检测会照常运行但收不到通知。</span>}{tg === null && <span style={{ color: 'var(--amber)' }}> Telegram 绑定状态没能读取（不代表未配置），请稍后刷新。</span>}</p>
      {!admin && <p className="form-note">{ADMIN_ONLY_HINT}（仅管理员可启停或手动触发定时巡检）。</p>}
      {loadError && <p className="form-error">读取定时巡检失败：{loadError}。刷新页面重试。</p>}
      <div className="schedule-list">
        {items.map((it) => (
          <div className="schedule-row" key={it.key}>
            <div>
              <strong>{it.label}</strong>
              <small>{it.interval} · 上次 {fmt(it.last_run)}{it.enabled ? ` · 下次 ${fmt(it.next_run)}` : ''}{it.running ? ' · 运行中…' : ''}</small>
              {it.last_error && <small className="lat-bad">⚠ {it.last_error}</small>}
              <small className="schedule-desc">{SCHEDULE_DESC[it.key]}</small>
            </div>
            <div className="schedule-actions">
              <button className="row-action" disabled={!admin || busy === it.key || it.running} title={admin ? undefined : ADMIN_ONLY_HINT} onClick={() => void runNow(it)}>{it.running ? <Loader2 size={13} className="spin" /> : <Play size={13} />}<span>立即运行</span></button>
              <label className="switch"><input type="checkbox" checked={it.enabled} disabled={!admin || busy === it.key} onChange={() => void toggle(it)} /><span>{it.enabled ? '已开启' : '已关闭'}</span></label>
            </div>
          </div>
        ))}
      </div>
    </section>
  )
}

const SCHEDULE_DESC: Record<string, string> = {
  unlock_daily: '所有节点的 Netflix / ChatGPT 等解锁情况；某项从可用变成不可用（或恢复）时通知',
  purity_weekly: 'IP 纯净度：欺诈分涨跌 ≥20、原生 / 广播变化、新出现代理 / VPN / 滥用标记时通知（会消耗第三方查询配额）',
  audit_weekly: '对已配 SSH 凭据的主机做只读安全检查，评分低于 80 时通知',
}

const PY_EXAMPLE = `import hashlib, hmac, json, time, secrets, requests
from urllib.parse import urlsplit, unquote, parse_qsl, urlencode

def call_panel(method, path, payload=None, params=None):
    key = "你的对接密钥"
    body = json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode() if payload is not None else b""
    url = requests.Request(method, "https://panel.example.com" + path, params=params).prepare().url
    parsed = urlsplit(url)
    pairs = parse_qsl(parsed.query, keep_blank_values=True)
    pairs.sort(key=lambda pair: pair[0])
    query = urlencode(pairs)
    canonical = unquote(parsed.path) + ("?" + query if query else "")
    ts, nonce = str(int(time.time())), secrets.token_hex(16)
    msg = "\\n".join([ts, method.upper(), canonical, hashlib.sha256(body).hexdigest(), nonce])
    sig = hmac.new(key.encode(), msg.encode(), hashlib.sha256).hexdigest()
    return requests.request(method, url, data=body or None, timeout=20, headers={
        "Content-Type": "application/json",
        "X-Store-Key": key, "X-Store-Timestamp": ts,
        "X-Store-Signature-Version": "2", "X-Store-Nonce": nonce,
        "X-Store-Signature": sig,
    })`

export function StoreSignatureCard({ notify }: { notify: (m: string) => void }) {
  const admin = isAdminSession()
  const [required, setRequired] = useState<boolean | null>(null)
  const [busy, setBusy] = useState(false)
  const mountedRef = useRef(true)
  useEffect(() => {
    mountedRef.current = true
    api.storeSignature()
      .then((r) => { if (mountedRef.current) setRequired(r.required) })
      .catch((err) => { if (mountedRef.current) notify(err instanceof Error ? err.message : '签名设置加载失败') })
    return () => { mountedRef.current = false }
  }, [notify])
  async function toggle() {
    if (!admin || busy || required === null) return
    if (!required && !await askConfirm('开启后，没带签名的商城请求都会被拒绝。\n请确认商城已经按下面的示例改好并测试通过，否则下单会失败。')) return
    // 2026-11-06：关闭也要确认：关掉后只凭对接密钥就能调商城接口（下单、续费、撤单），密钥泄露即可伪造请求。
    if (required && !await askConfirm('关闭强制签名？\n关闭后只凭对接密钥就能调用下单、续费、撤单接口，密钥一旦泄露即可伪造商城请求。', { danger: true })) return
    if (!mountedRef.current) return
    setBusy(true)
    try {
      const r = await api.setStoreSignature(!required)
      if (!mountedRef.current) return
      setRequired(r.required)
      notify(r.required ? '已开启强制签名' : '已关闭强制签名')
    } catch (err) {
      if (mountedRef.current) notify(err instanceof Error ? err.message : '设置失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }
  return (
    <section className="panel settings-panel">
      <div className="section-head"><div><div className="section-kicker">STORE API</div><h2>商城接口签名</h2></div></div>
      <div className="settings-form" style={{ maxWidth: 'none' }}>
        <p className="form-note">原来商城只靠固定的对接密钥，泄露后可被任意下单，截获的请求还能原样重放。签名后：每个请求带时间戳（±5 分钟）、签名v2覆盖方法、路径、查询参数、请求体和独立随机nonce；nonce跨重启防重放。</p>
        <p className="form-note"><strong>兼容</strong>：带了签名的请求一律校验；没带签名的请求，在下面开关关闭时照常放行 —— 先改商城、确认下单正常，再打开「强制签名」。</p>
        <pre className="install-log" style={{ maxHeight: 'none' }}>{PY_EXAMPLE}</pre>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <span>强制签名：{required === null ? '—' : required ? <span className="status-pill online">已开启</span> : <span className="status-pill offline">未开启</span>}</span>
          <button className="button button-quiet" disabled={!admin || busy || required === null} title={admin ? undefined : ADMIN_ONLY_HINT} onClick={() => void toggle()}>{required ? '关闭' : '开启'}强制签名</button>
        </div>
      </div>
    </section>
  )
}

function normalizeDomainDraft(raw: string): string {
  const cleaned = raw.trim().replace(/^https?:\/\//i, '').replace(/\/+$/, '').toLowerCase()
  return cleaned ? `https://${cleaned}` : ''
}

export function DeliveryDomainCard({ notify }: { notify: (m: string) => void }) {
  const admin = isAdminSession()
  const [state, setState] = useState<DeliveryDomainState | null>(null)
  const [draft, setDraft] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [nameDraft, setNameDraft] = useState('')
  const [nameError, setNameError] = useState('')
  const [loadError, setLoadError] = useState('')
  const mountedRef = useRef(true)
  useEffect(() => {
    mountedRef.current = true
    api.deliveryDomain()
      .then((r) => { if (!mountedRef.current) return; setState(r); setNameDraft(r.name); setDraft(r.value.replace(/^https:\/\//, '')); setLoadError('') })
      .catch((err) => { if (!mountedRef.current) return; const m = err instanceof Error ? err.message : '交付域名加载失败'; setLoadError(m); notify(m) })
    return () => { mountedRef.current = false }
  }, [notify])
  async function save(value: string) {
    if (!admin || busy) return
    setBusy(true); setError('')
    try {
      const r = await api.setDeliveryDomain(value)
      if (!mountedRef.current) return
      setState(r); setDraft(r.value.replace(/^https:\/\//, ''))
      notify(value ? `交付域名已改为 ${r.effective}，之后发货的链接都用它` : '已清除，恢复默认域名')
    } catch (err) {
      if (mountedRef.current) setError(err instanceof Error ? err.message : '保存失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }
  async function saveName() {
    if (!admin || busy) return
    setBusy(true); setNameError('')
    try {
      const r = await api.setSubscriptionName(nameDraft.trim())
      if (!mountedRef.current) return
      setState(r); setNameDraft(r.name)
      notify(`订阅显示名称已改为「${r.name}」，之后发货的链接导入后就叫这个`)
    } catch (err) {
      if (mountedRef.current) setNameError(err instanceof Error ? err.message : '保存失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }
  const sourceLabel = state?.source === 'setting' ? '控制台设置' : state?.source === 'env' ? '环境变量' : '面板访问域名（默认）'
  const normalizedDraft = normalizeDomainDraft(draft)
  return (
    <section className="panel settings-panel">
      <div className="section-head"><div><div className="section-kicker">DELIVERY DOMAIN</div><h2>交付域名</h2></div></div>
      <div className="settings-form" style={{ maxWidth: 'none' }}>
        <p className="form-note">商城发货给买家的<strong>订阅链接</strong>和<strong>交付页</strong>使用这个域名，和面板登录域名分开，买家看不到你的面板地址。</p>
        {/* 2026-11-06：读取失败时不让保存：输入框是空的，照样保存名称会把已有的订阅显示名清空。 */}
        {loadError && <p className="form-note service-warn">⚠ 当前设置读取失败（{loadError}），刷新页面后再修改。</p>}
        <p className="form-note">当前生效：<code>{state?.effective || '—'}</code>（来源：{sourceLabel}）</p>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
          <label className="form-field" style={{ flex: '1 1 260px', margin: 0 }}>
            <input value={draft} onChange={(e) => { setDraft(e.target.value); setError('') }} placeholder="sub.example.com" spellCheck={false} disabled={!admin || busy}
              onKeyDown={(e) => { if (e.key === 'Enter' && admin && state && draft.trim() && normalizedDraft !== state.value) void save(draft.trim()) }} />
          </label>
          <button className="button button-primary" disabled={!admin || busy || !state || !normalizedDraft || normalizedDraft === state?.value} title={admin ? undefined : ADMIN_ONLY_HINT} onClick={() => void save(draft.trim())}>{busy ? <Loader2 size={14} className="spin" /> : null}{busy ? '检查中…' : '保存'}</button>
          {state?.value && <button className="button button-quiet" disabled={!admin || busy} title={admin ? undefined : ADMIN_ONLY_HINT} onClick={() => void save('')}>恢复默认</button>}
        </div>
        {error && <p role="alert" style={{ color: 'var(--red)' }}>{error}</p>}
        <div style={{ borderTop: '1px solid var(--line-soft)', paddingTop: 12, marginTop: 4 }}>
          <p className="form-note"><strong>订阅显示名称</strong>：买家把链接导入 v2rayN、Clash Verge、Stash 等客户端后，订阅分组 / 配置文件显示的名字（不设置时 v2rayN 会显示 import_sub）。</p>
          <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
            <label className="form-field" style={{ flex: '1 1 260px', margin: 0 }}>
              <input value={nameDraft} maxLength={32} onChange={(e) => { setNameDraft(e.target.value); setNameError('') }} placeholder="Teyir" disabled={!admin || busy}
                onKeyDown={(e) => { if (e.key === 'Enter' && admin && state && nameDraft.trim() !== (state?.name ?? '')) void saveName() }} />
            </label>
            <button className="button button-primary" disabled={!admin || busy || !state || nameDraft.trim() === (state?.name ?? '')} title={admin ? undefined : ADMIN_ONLY_HINT} onClick={() => void saveName()}>保存名称</button>
          </div>
          {nameError && <p role="alert" style={{ color: 'var(--red)' }}>{nameError}</p>}
          <p className="form-note">只对之后发出的链接生效；已经导入的买家可以在客户端里自己改名，或重新导入新链接。</p>
        </div>
        <p className="form-note">保存前会自动检查：这个域名已解析到面板服务器、HTTPS 证书可用、并且转发到了本面板。新域名需要先在 DNS 和反向代理（Caddy）里配好。已发出去的旧链接不受影响，仍然可用。</p>
      </div>
    </section>
  )
}
