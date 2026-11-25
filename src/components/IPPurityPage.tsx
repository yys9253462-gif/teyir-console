import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  ShieldCheck, ShieldAlert, ShieldQuestion, Loader2, RefreshCw, Play,
  Globe, Home, Building2, Smartphone, Cable, Search, ChevronDown, MapPin, Network, ScanSearch, X, Info,
} from 'lucide-react'
import { api, isAdminSession, type IPProbeResult, type IPPurity, type IPPurityListItem, type NodeUnlock, type UnlockResult } from '../lib/api'
import { copyToClipboard } from '../lib/clipboard'
import { Modal } from './Modal'

// IPPurityPage —— 专用「IP 纯净度」页面。
//
// 一张表列出所有在役节点的 IP 质量：原生 / 广播、连接类型、双 ISP、
// 风险标记、ASN 滥用率；下方是流媒体 / AI 解锁检测。
//
// 铁律：
//   1. **读缓存不联网**：进页面只读库，检测永远由用户点按钮触发 ——
//      否则刷一次页就把 ipapi.is 的免费配额刷爆。
//   2. **null ≠ false**：所有判定字段都是 boolean|null，null 显示灰色
//      「未判定」，绝不画成「否」。
//   3. **失败不掩盖成功**：error 与 checked_at 独立展示。
//
// 2026-10-15 改版：
//   - 新增「原生 IP」（注册国 = 所在国），这是买家最常问的一项，放在第二列；
//   - 代理 / VPN / 滥用三列合并成一列「风险标记」—— 三列几乎永远是「无」，
//     占了大半张表却没信息量；有标记时才显示对应的红色标签；
//   - 「评分」改名「ASN 滥用率」：ipapi.is 给的 abuser_score 是整个 ASN 的
//     滥用比例，同一家云厂商的节点分数几乎一样，叫「评分」会误导成单 IP 欺诈分；
//   - 页头与其他页面统一（eyebrow + 大标题 + 右侧操作）。

type Props = {
  notify: (message: string, tone?: 'info' | 'error') => void
  readOnly?: boolean
}

// 常见国家的中文名 —— 只用于展示，认不出的原样显示 ISO 码。
const COUNTRY_ZH: Record<string, string> = {
  US: '美国', CA: '加拿大', MX: '墨西哥', BR: '巴西', GB: '英国', IE: '爱尔兰', FR: '法国',
  DE: '德国', NL: '荷兰', BE: '比利时', CH: '瑞士', AT: '奥地利', IT: '意大利', ES: '西班牙',
  SE: '瑞典', NO: '挪威', DK: '丹麦', FI: '芬兰', PL: '波兰', RU: '俄罗斯', TR: '土耳其',
  AE: '阿联酋', IL: '以色列', IN: '印度', JP: '日本', KR: '韩国', CN: '中国', HK: '香港',
  TW: '台湾', SG: '新加坡', MY: '马来西亚', TH: '泰国', VN: '越南', PH: '菲律宾', ID: '印尼',
  AU: '澳大利亚', NZ: '新西兰', ZA: '南非',
}

function countryName(code: string): string {
  return COUNTRY_ZH[code] ?? code
}

function connectionMeta(type: IPPurity['connection_type']): {
  label: string; tone: string; Icon: typeof Home
} {
  switch (type) {
    case 'residential': return { label: '家宽', tone: 'good', Icon: Home }
    case 'datacenter': return { label: '机房', tone: 'info', Icon: Building2 }
    case 'mobile': return { label: '移动', tone: 'good', Icon: Smartphone }
    case 'business': return { label: '专线', tone: 'info', Icon: Cable }
    default: return { label: '未知', tone: 'muted', Icon: Globe }
  }
}

// ASN 滥用率（0-100 换算值）→ 色调。
function scoreTone(score: number | null): string {
  if (score === null) return 'muted'
  if (score >= 70) return 'bad'
  if (score >= 30) return 'warn'
  return 'good'
}

// 风险标记：有任何 true → 列出；全部明确 false → 无风险；否则未判定。
function riskFlags(p: IPPurity): { flags: string[]; state: 'risk' | 'clean' | 'unknown' } {
  const flags: string[] = []
  if (p.is_proxy) flags.push('代理')
  if (p.is_vpn) flags.push('VPN')
  if (p.is_abuser) flags.push('滥用')
  if (flags.length) return { flags, state: 'risk' }
  if (p.is_proxy === false && p.is_vpn === false && p.is_abuser === false) return { flags, state: 'clean' }
  return { flags, state: 'unknown' }
}

type FilterKey = 'all' | 'native' | 'broadcast' | 'dualsp' | 'risk' | 'unchecked'
type SortKey = 'server' | 'native' | 'score'

export function IPPurityPage({ notify, readOnly = false }: Props) {
  const [probing, setProbing] = useState(false)
  const [items, setItems] = useState<IPPurityListItem[]>([])
  const [loading, setLoading] = useState(false)
  const [bulkRunning, setBulkRunning] = useState(false)
  // 用集合记录单行刷新状态，避免同时刷新多个节点时彼此覆盖加载指示。
  const [refreshingIds, setRefreshingIds] = useState<Set<string>>(() => new Set())
  const [filter, setFilter] = useState<FilterKey>('all')
  const [sort, setSort] = useState<SortKey>('server')
  const [query, setQuery] = useState('')
  // 2026-11-06：列表读取失败单独记下。原来只弹一条 toast，items 仍是 []：
  // 表格写「还没有节点」，KPI 显示「未检测 0/0 · 全部节点都已检测」「风险标记 0 · 未发现代理…」，
  // 与真实的干净结果一模一样。
  const [loadError, setLoadError] = useState('')
  const loadSeq = useRef(0)
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => { mounted.current = false }
  }, [])

  const load = useCallback(async (silent = false) => {
    const seq = ++loadSeq.current
    if (!silent) setLoading(true)
    try {
      const result = await api.ipPurityList()
      if (!mounted.current || seq !== loadSeq.current) return
      setItems(result.items)
      setLoadError('')
    } catch (error) {
      if (!mounted.current || seq !== loadSeq.current) return
      const message = error instanceof Error ? error.message : '读取 IP 纯净度失败'
      setLoadError(message)
      notify(message, 'error')
    } finally {
      if (mounted.current && seq === loadSeq.current) setLoading(false)
    }
  }, [notify])

  useEffect(() => { void load() }, [load])

  async function refreshOne(nodeId: string, nodeName: string) {
    setRefreshingIds((prev) => {
      const next = new Set(prev)
      next.add(nodeId)
      return next
    })
    try {
      await api.refreshIPPurity(nodeId, true)
      notify(`${nodeName} 的 IP 纯净度已更新`, 'info')
    } catch (error) {
      notify(error instanceof Error ? error.message : 'IP 纯净度检测失败', 'error')
    } finally {
      await load(true)
      if (mounted.current) {
        setRefreshingIds((prev) => {
          const next = new Set(prev)
          next.delete(nodeId)
          return next
        })
      }
    }
  }

  async function refreshAll() {
    if (bulkRunning) return
    setBulkRunning(true)
    try {
      const r = await api.refreshAllIPPurity()
      notify(`批量检测完成：成功 ${r.ok}，跳过 ${r.skipped}${r.failed ? `，失败 ${r.failed}` : ''}`, r.failed ? 'error' : 'info')
    } catch (error) {
      notify(error instanceof Error ? error.message : '批量检测失败', 'error')
    } finally {
      await load(true)
      setBulkRunning(false)
    }
  }

  const stats = useMemo(() => {
    let native = 0, broadcast = 0, dual = 0, risk = 0, unchecked = 0
    for (const it of items) {
      const p = it.purity
      if (!it.checked || !p) { unchecked++; continue }
      if (p.is_native === true) native++
      if (p.is_native === false) broadcast++
      if (p.is_dual_isp === true) dual++
      if (riskFlags(p).state === 'risk') risk++
    }
    return { total: items.length, native, broadcast, dual, risk, unchecked }
  }, [items])

  const view = useMemo(() => {
    const q = query.trim().toLowerCase()
    let rows = items.filter((it) => {
      const p = it.purity
      switch (filter) {
        case 'native': return p?.is_native === true
        case 'broadcast': return p?.is_native === false
        case 'dualsp': return p?.is_dual_isp === true
        case 'risk': return !!p && riskFlags(p).state === 'risk'
        case 'unchecked': return !it.checked
        default: return true
      }
    })
    if (q) {
      rows = rows.filter((it) =>
        it.node_name.toLowerCase().includes(q) ||
        it.server_name.toLowerCase().includes(q) ||
        it.host.toLowerCase().includes(q) ||
        (it.purity?.asn_org ?? '').toLowerCase().includes(q))
    }
    const sorted = [...rows]
    sorted.sort((a, b) => {
      if (sort === 'score') {
        const sa = a.purity?.fraud_score
        const sb = b.purity?.fraud_score
        if (sa == null && sb == null) return 0
        if (sa == null) return 1
        if (sb == null) return -1
        return sb - sa
      }
      if (sort === 'native') {
        const rank = (p?: IPPurity) => (p?.is_native === true ? 0 : p?.is_native === false ? 1 : 2)
        return rank(a.purity) - rank(b.purity)
      }
      return a.server_name.localeCompare(b.server_name, 'zh-CN') || a.node_name.localeCompare(b.node_name, 'zh-CN')
    })
    return sorted
  }, [items, filter, query, sort])

  // 任何一行的评分来自 IPQS，就把列名换成「欺诈分」（逐格的 title 仍会说明来源）。
  const usesIPQS = items.some((it) => it.purity?.score_source === 'ipqs')

  const filterCount: Record<FilterKey, number> = {
    all: stats.total, native: stats.native, broadcast: stats.broadcast,
    dualsp: stats.dual, risk: stats.risk, unchecked: stats.unchecked,
  }

  return (
    <>
      <div className="page-heading">
        <div>
          <div className="eyebrow"><span className="eyebrow-line" />IP QUALITY / 03</div>
          <h1>IP 纯净度<span className="heading-period">.</span></h1>
          <p className="page-subtitle">原生 / 广播、连接类型、双 ISP、风险标记，以及节点本机测得的流媒体与 AI 解锁情况</p>
        </div>
        <div className="heading-actions">
          {/* 2026-11-06：POST /api/ip-probe 是 requireAdmin；原来运维账号也能打开弹窗、填完点检测才收到 403。 */}
          <button className="button button-quiet" disabled={readOnly || !isAdminSession()} onClick={() => setProbing(true)}
            title={readOnly ? '只读账号无法执行检测' : !isAdminSession() ? '仅管理员可检测任意 IP（会消耗第三方查询配额）' : '输入任意 IP 或域名，检测纯净度与流媒体解锁'}>
            <ScanSearch size={16} />检测指定 IP
          </button>
          <button
            className="button button-primary"
            disabled={readOnly || bulkRunning || loading}
            onClick={() => void refreshAll()}
            title={readOnly ? '只读账号无法执行检测' : '对所有节点执行一次深度检测（走第三方 API）'}
          >
            {bulkRunning ? <Loader2 size={16} className="spin" /> : <Play size={16} />}
            {bulkRunning ? '检测中…' : '批量检测全部'}
          </button>
        </div>
      </div>

      {probing && <IPProbeDialog notify={notify} onClose={() => setProbing(false)} />}

      <section className="metrics-grid ipp-kpis" aria-label="IP 纯净度概览">
        <KpiCard failed={Boolean(loadError) && items.length === 0} label="原生 IP" value={stats.native} total={stats.total} Icon={MapPin} tone="good"
          foot={stats.broadcast ? `${stats.broadcast} 台为广播 IP` : '注册国与所在国一致'} />
        <KpiCard failed={Boolean(loadError) && items.length === 0} label="双 ISP" value={stats.dual} total={stats.total} Icon={Network} tone="info"
          foot="ASN 与网段两个口径都是 ISP" />
        <KpiCard failed={Boolean(loadError) && items.length === 0} label="风险标记" value={stats.risk} total={stats.total} Icon={ShieldAlert} tone={stats.risk ? 'bad' : 'good'}
          foot={stats.risk ? '被标代理 / VPN / 滥用' : '未发现代理 / VPN / 滥用标记'} />
        <KpiCard failed={Boolean(loadError) && items.length === 0} label="未检测" value={stats.unchecked} total={stats.total} Icon={ShieldQuestion} tone="muted"
          foot={stats.unchecked ? '点右上角「批量检测全部」' : '全部节点都已检测'} />
      </section>

      <section className="panel ipp-panel">
        <div className="section-head ipp-panel-head">
          <div>
            <div className="section-kicker">IP <span>·</span> {String(stats.total).padStart(2, '0')} NODES</div>
            <h2>节点 IP 质量</h2>
          </div>
          <div className="ipp-toolbar">
            <label className="monitor-search">
              <Search size={14} />
              <input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="搜索节点 / IP / ASN 组织…" aria-label="搜索" />
            </label>
            <label className="ipp-sort">
              <ChevronDown size={14} />
              <select value={sort} onChange={(e) => setSort(e.target.value as SortKey)} aria-label="排序">
                <option value="server">按主机</option>
                <option value="native">原生优先</option>
                <option value="score">按评分（高→低）</option>
              </select>
            </label>
          </div>
        </div>

        <div className="monitor-filter ipp-filter" role="group" aria-label="筛选">
          {([
            ['all', '全部'], ['native', '原生'], ['broadcast', '广播'],
            ['dualsp', '双 ISP'], ['risk', '有风险'], ['unchecked', '未检测'],
          ] as [FilterKey, string][]).map(([k, label]) => (
            <button key={k} className={filter === k ? 'active' : ''} onClick={() => setFilter(k)}>
              {label} {filterCount[k]}
            </button>
          ))}
        </div>

        <div className="ipp-table-wrap">
          <table className="ipp-table">
            <thead>
              <tr>
                <th>节点 / IP</th>
                <th>原生 IP</th>
                <th>连接类型</th>
                <th>双 ISP</th>
                <th>风险标记</th>
                <th title={usesIPQS ? 'IPQualityScore 给出的单 IP 欺诈分，0-100，越高越脏' : 'ipapi.is 给出的整个 ASN 的滥用比例，不是单个 IP 的欺诈分'}>{usesIPQS ? '欺诈分' : 'ASN 滥用率'}</th>
                <th>ASN / 归属</th>
                <th>检测时间</th>
                <th className="ipp-col-act" aria-label="操作" />
              </tr>
            </thead>
            <tbody>
              {view.map((it) => (
                <IPPurityRow
                  key={it.node_id}
                  item={it}
                  busy={refreshingIds.has(it.node_id)}
                  readOnly={readOnly}
                  onRefresh={() => void refreshOne(it.node_id, it.node_name)}
                />
              ))}
              {view.length === 0 && (
                <tr><td colSpan={9} className="ipp-empty">
                  {loading ? '加载中…' : loadError && items.length === 0 ? `读取失败：${loadError}` : items.length === 0 ? '还没有节点' : '没有符合筛选条件的节点'}
                </td></tr>
              )}
            </tbody>
          </table>
        </div>

        <p className="form-note form-note-hint">
          <ShieldCheck size={13} />
          <span>
            <strong>原生 IP</strong>：IP 段的注册国（RDAP）与实际所在国一致；不一致为广播 IP，流媒体常按注册地判地区。
            <strong> 双 ISP</strong>：ASN 与网段两个口径都标为运营商。「未判定」表示数据源没给该字段，不等同于「否」。
          </span>
        </p>
      </section>

      <UnlockSection items={items} notify={notify} readOnly={readOnly} />
    </>
  )
}

function KpiCard({ label, value, total, Icon, tone, foot, failed = false }: {
  label: string; value: number; total: number; Icon: typeof Home; tone: string; foot: string; failed?: boolean
}) {
  // failed：列表没读到，数字未知 —— 显示「—」与「读取失败」，不拿空数组算出 0。
  return (
    <div className={`metric-card ipp-kpi tone-${failed ? 'muted' : tone}`}>
      <div className="metric-head"><span>{label}</span><Icon size={17} /></div>
      <div className="metric-body"><strong>{failed ? '—' : value}</strong>{!failed && <span className="metric-unit">/ {total}</span>}</div>
      <div className="metric-foot">{failed ? '读取失败' : foot}</div>
    </div>
  )
}

function NativeBadge({ p }: { p: IPPurity }) {
  const reg = p.reg_country
  const geo = p.geo_country
  if (p.is_native === true) {
    return <span className="ipp-pill tone-good" title={`注册 ${countryName(reg)} · 使用 ${countryName(geo)}${p.reg_registry ? ` · ${p.reg_registry}` : ''}`}>
      <MapPin size={12} /> 原生 · {countryName(geo)}
    </span>
  }
  if (p.is_native === false) {
    return <span className="ipp-native-broadcast" title={`注册地与所在地不一致${p.reg_registry ? ` · ${p.reg_registry}` : ''}`}>
      <span className="ipp-pill tone-warn">广播</span>
      <small>注册 {countryName(reg)} → 使用 {countryName(geo)}</small>
    </span>
  }
  const missing = !reg && !geo ? '未检测原生' : !reg ? '注册国未查到' : '所在国未查到'
  return <span className="ipp-tri tone-muted" title="重新检测会补查">{p.checked_at ? missing : '未判定'}</span>
}

function IPPurityRow({ item, busy, readOnly, onRefresh }: {
  item: IPPurityListItem
  busy: boolean
  readOnly: boolean
  onRefresh: () => void
}) {
  const p = item.purity
  const checked = item.checked && !!p
  const action = (
    <td className="ipp-col-act">
      <button
        className="ipp-act"
        disabled={readOnly || busy || !item.host}
        title={readOnly ? '只读账号无法执行检测' : !item.host ? '该节点没有可检测的公网 IP' : '重新检测（走第三方 API）'}
        onClick={onRefresh}
      >
        {busy ? <Loader2 size={13} className="spin" /> : <RefreshCw size={13} />}
      </button>
    </td>
  )
  const nodeCell = (ip: string, stale = false) => (
    <td>
      <div className="ipp-node">
        <span className="ipp-node-name">{item.node_name}</span>
        <span className="ipp-node-ip">
          {ip || '—'}
          {stale && <span className="ipp-stale" title="存下来的 IP 与当前节点 IP 不一致，建议重测">已变更</span>}
        </span>
      </div>
    </td>
  )

  if (!checked) {
    return (
      <tr className="ipp-row is-unchecked">
        {nodeCell(item.host)}
        <td colSpan={7} className="ipp-reason">
          <span className="tone-muted"><ShieldQuestion size={13} /> {item.reason || '尚未检测'}</span>
        </td>
        {action}
      </tr>
    )
  }

  const conn = connectionMeta(p.connection_type)
  const ConnIcon = conn.Icon
  const risk = riskFlags(p)
  const tone = scoreTone(p.fraud_score)

  return (
    <tr className="ipp-row">
      {nodeCell(p.ip, p.stale)}
      <td><NativeBadge p={p} /></td>
      <td><span className={`ipp-pill tone-${conn.tone}`}><ConnIcon size={12} /> {conn.label}</span></td>
      <td>
        {p.is_dual_isp === null
          ? <span className="ipp-tri tone-muted">未判定</span>
          : p.is_dual_isp
            ? <span className="ipp-pill tone-good">双 ISP</span>
            : <span className="ipp-tri tone-muted">否</span>}
      </td>
      <td>
        {risk.state === 'risk'
          ? <span className="ipp-flags">{risk.flags.map((f) => <span key={f} className="ipp-pill tone-bad">{f}</span>)}</span>
          : risk.state === 'clean'
            ? <span className="ipp-tri tone-good">无风险</span>
            : <span className="ipp-tri tone-muted">未判定</span>}
      </td>
      <td>
        {p.score_source === 'ipqs'
          ? <span className={`ipp-score tone-${tone}`} title="IPQualityScore 单 IP 欺诈分（0-100，≥75 高风险）">
              {p.fraud_score === null ? '—' : p.fraud_score}
            </span>
          : <span className={`ipp-score tone-${tone}`} title="ipapi.is：整个 ASN 的滥用比例（0-100 换算），不是单 IP 欺诈分">
              {p.fraud_score === null ? '—' : `${p.fraud_score}%`}
            </span>}
      </td>
      <td>
        {/* flex 放在内层 div：td 自己设 display:flex 会脱离表格布局，多出一列空白 */}
        <div className="ipp-asn">
          <span className="ipp-asn-code">{p.asn || '—'}</span>
          {p.asn_org && <span className="ipp-asn-org" title={p.asn_org}>{p.asn_org}</span>}
        </div>
      </td>
      <td className="ipp-time">
        {p.checked_at ? new Date(p.checked_at).toLocaleString('zh-CN', { hour12: false, month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }) : '从未成功'}
        {p.error && <span className="ipp-err" title={p.error}>上次失败</span>}
      </td>
      {action}
    </tr>
  )
}

// ---------------------------------------------------------------------------
// 流媒体 / AI 解锁（2026-10-15）
//
// 检测在**节点本机**跑（portal 2.8 的 /api/v1/unlock-check）—— 解锁看的是
// 节点出口 IP，面板自己测没有意义。与上面的纯净度一样：进页面只读库，
// 检测必须由用户点按钮触发（频繁访问 Netflix / OpenAI 本身就可能让 IP 被标记）。
//
// 批量由前端逐台驱动（3 路并发）：单台约 10-25 秒，一次性的后端全量请求
// 会超过反代 60 秒读超时。

const UNLOCK_COLUMNS: { key: string; label: string }[] = [
  { key: 'netflix', label: 'Netflix' },
  { key: 'disney', label: 'Disney+' },
  { key: 'youtube', label: 'YouTube' },
  { key: 'chatgpt', label: 'ChatGPT' },
  { key: 'claude', label: 'Claude' },
  { key: 'gemini', label: 'Gemini' },
]

const UNLOCK_META: Record<UnlockResult['status'], { text: string; tone: string }> = {
  yes: { text: '解锁', tone: 'good' },
  partial: { text: '部分', tone: 'warn' },
  no: { text: '不解锁', tone: 'bad' },
  unknown: { text: '检测失败', tone: 'muted' },
}

function UnlockCell({ result }: { result?: UnlockResult }) {
  if (!result) return <span className="ipp-tri tone-muted">—</span>
  const meta = UNLOCK_META[result.status] ?? UNLOCK_META.unknown
  const title = [meta.text, result.region && `地区 ${result.region}`, result.note].filter(Boolean).join(' · ')
  return (
    <span className={`ipp-tri tone-${meta.tone}`} title={title}>
      {meta.text}{result.region && result.status !== 'no' ? ` · ${result.region}` : ''}
    </span>
  )
}

function UnlockSection({ items, notify, readOnly }: {
  items: IPPurityListItem[]
  notify: (message: string, tone?: 'info' | 'error') => void
  readOnly: boolean
}) {
  const [data, setData] = useState<Record<string, NodeUnlock>>({})
  const [running, setRunning] = useState<Record<string, boolean>>({})
  const [bulk, setBulk] = useState(false)
  // 同上：读取失败时 data 为空，每行都会显示「尚未检测 / 从未成功」，而库里其实有结果。
  const [loadError, setLoadError] = useState('')

  const load = useCallback(async () => {
    try {
      const result = await api.unlockList()
      setData(Object.fromEntries(result.items.map((it) => [it.node_id, it])))
      setLoadError('')
    } catch (error) {
      const message = error instanceof Error ? error.message : '读取解锁检测结果失败'
      setLoadError(message)
      notify(message, 'error')
    }
  }, [notify])

  useEffect(() => { void load() }, [load])

  async function checkOne(nodeId: string): Promise<boolean> {
    setRunning((r) => ({ ...r, [nodeId]: true }))
    try {
      await api.refreshUnlock(nodeId)
      return true
    } catch (error) {
      setData((d) => ({ ...d, [nodeId]: { ...(d[nodeId] ?? { node_id: nodeId, results: [], checked_at: null }), error: error instanceof Error ? error.message : String(error) } }))
      return false
    } finally {
      setRunning((r) => ({ ...r, [nodeId]: false }))
    }
  }

  async function checkSingle(nodeId: string, name: string) {
    const ok = await checkOne(nodeId)
    notify(ok ? `${name} 解锁检测完成` : `${name} 解锁检测失败`, ok ? 'info' : 'error')
    await load()
  }

  async function checkAll() {
    if (bulk) return
    setBulk(true)
    const queue = items.map((it) => it.node_id)
    let ok = 0
    let fail = 0
    const worker = async () => {
      for (let id = queue.shift(); id; id = queue.shift()) {
        if (await checkOne(id)) ok++
        else fail++
        void load()
      }
    }
    await Promise.all(Array.from({ length: Math.min(3, queue.length) }, worker))
    setBulk(false)
    await load()
    notify(`解锁检测完成：成功 ${ok}${fail ? `，失败 ${fail}` : ''}`, fail ? 'error' : 'info')
  }

  return (
    <section className="panel ipp-panel">
      <div className="section-head ipp-panel-head">
        <div>
          <div className="section-kicker">STREAMING <span>·</span> AI UNLOCK</div>
          <h2>流媒体 / AI 解锁</h2>
        </div>
        <button
          className="button button-quiet"
          disabled={readOnly || bulk || items.length === 0}
          onClick={() => void checkAll()}
          title={readOnly ? '只读账号无法执行检测' : '逐台检测全部节点（3 台并发）'}
        >
          {bulk ? <Loader2 size={15} className="spin" /> : <Play size={15} />}
          {bulk ? '检测中…' : '检测全部解锁'}
        </button>
      </div>
      <div className="ipp-table-wrap">
        <table className="ipp-table">
          <thead>
            <tr>
              <th>节点</th>
              {UNLOCK_COLUMNS.map((c) => <th key={c.key}>{c.label}</th>)}
              <th>检测时间</th>
              <th className="ipp-col-act">操作</th>
            </tr>
          </thead>
          <tbody>
            {items.map((it) => {
              const row = data[it.node_id]
              const byKey = Object.fromEntries((row?.results ?? []).map((r) => [r.key, r]))
              const busy = Boolean(running[it.node_id])
              const never = !row?.checked_at
              return (
                <tr key={it.node_id} className={`ipp-row${never ? ' is-unchecked' : ''}`}>
                  <td>
                    <div className="ipp-node"><span className="ipp-node-name">{it.node_name}</span><span className="ipp-node-host">{it.server_name}</span></div>
                  </td>
                  {never ? (
                    <td colSpan={UNLOCK_COLUMNS.length} className="ipp-reason">
                      <span className={row?.error ? 'tone-warn' : 'tone-muted'} title={row?.error}>
                        <ShieldQuestion size={13} /> {busy ? '检测中…' : row?.error || (loadError ? '读取失败' : '尚未检测')}
                      </span>
                    </td>
                  ) : UNLOCK_COLUMNS.map((c) => <td key={c.key}><UnlockCell result={byKey[c.key]} /></td>)}
                  <td className="ipp-time">
                    {row?.checked_at ? new Date(row.checked_at).toLocaleString('zh-CN', { hour12: false, month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }) : loadError ? '—' : '从未成功'}
                    {row?.error && !never && <span className="ipp-err" title={row.error}>上次失败</span>}
                  </td>
                  <td className="ipp-col-act">
                    <button
                      className="ipp-act"
                      disabled={readOnly || busy || bulk}
                      title={readOnly ? '只读账号无法执行检测' : '在节点本机重新检测解锁'}
                      onClick={() => void checkSingle(it.node_id, it.node_name)}
                    >
                      {busy ? <Loader2 size={13} className="spin" /> : <RefreshCw size={13} />}
                    </button>
                  </td>
                </tr>
              )
            })}
            {items.length === 0 && (
              <tr><td colSpan={UNLOCK_COLUMNS.length + 3} className="ipp-empty">还没有节点</td></tr>
            )}
          </tbody>
        </table>
      </div>
      <p className="form-note form-note-hint">
        <Globe size={13} />
        <span>在节点本机检测（需节点门户 2.8+），反映的是节点出口 IP 的解锁情况。悬停单元格查看地区与说明。</span>
      </p>
    </section>
  )
}

// ---------------------------------------------------------------- 检测指定 IP / 域名

const PROBE_HISTORY_KEY = 'ipp.probeHistory'
function loadProbeHistory(): string[] {
  try {
    const raw = JSON.parse(window.localStorage.getItem(PROBE_HISTORY_KEY) ?? '[]')
    return Array.isArray(raw) ? raw.filter((x) => typeof x === 'string').slice(0, 6) : []
  } catch { return [] }
}
function saveProbeHistory(list: string[]) {
  try { window.localStorage.setItem(PROBE_HISTORY_KEY, JSON.stringify(list.slice(0, 6))) } catch { /* 隐私模式等 */ }
}

function IPProbeDialog({ notify, onClose }: { notify: Props['notify']; onClose: () => void }) {
  const [query, setQuery] = useState('')
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<IPProbeResult | null>(null)
  const [error, setError] = useState('')
  const [history, setHistory] = useState<string[]>(() => loadProbeHistory())

  async function run(raw?: string) {
    const q = (raw ?? query).trim()
    if (!q || busy) return
    setQuery(q)
    setBusy(true); setError(''); setResult(null)
    try {
      const r = await api.probeIP(q)
      setResult(r)
      const next = [q, ...history.filter((h) => h !== q)]
      setHistory(next); saveProbeHistory(next)
    } catch (err) {
      setError(err instanceof Error ? err.message : '检测失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal title="检测指定 IP / 域名" kicker="IP PROBE" wide onClose={onClose}>
      <div className="probe">
        <form className="probe-search" onSubmit={(e) => { e.preventDefault(); void run() }}>
          <Search size={18} className="probe-search-icon" />
          <input value={query} onChange={(e) => setQuery(e.target.value)} autoFocus spellCheck={false} disabled={busy}
            placeholder="输入 IP 或域名，例如 1.2.3.4 或 hy2.example.com" aria-label="IP 或域名" />
          {query && !busy && <button type="button" className="probe-clear" aria-label="清空" onClick={() => setQuery('')}><X size={15} /></button>}
          <button type="submit" className="button button-primary" disabled={busy || !query.trim()}>
            {busy ? <Loader2 size={16} className="spin" /> : <ScanSearch size={16} />}{busy ? '检测中…' : '检测'}
          </button>
        </form>
        {history.length > 0 && !busy && (
          <div className="probe-history">
            <span>最近：</span>
            {history.map((h) => <button key={h} type="button" className="probe-chip" onClick={() => void run(h)}>{h}</button>)}
          </div>
        )}

        {busy && (
          <div className="probe-loading">
            <Loader2 size={22} className="spin" />
            <div><strong>正在检测 {query}</strong><small>解析地址 · 查询纯净度 · 判断原生 IP · 检测解锁，通常 5–20 秒</small></div>
          </div>
        )}
        {error && <div className="probe-error" role="alert">{error}</div>}
        {!busy && !result && !error && (
          <div className="probe-empty">
            <ScanSearch size={34} />
            <p>支持 IP、域名，也可以直接粘贴整段地址（会自动提取主机名）。</p>
            <p>输入的是你的节点时，会顺带让节点现场检测 Netflix / ChatGPT 等解锁情况。</p>
          </div>
        )}
        {result && <ProbeResultView r={result} notify={notify} />}
      </div>
    </Modal>
  )
}

function ScoreRing({ score, label }: { score: number | null; label: string }) {
  const tone = scoreTone(score)
  const pct = score === null ? 0 : Math.max(0, Math.min(100, score))
  const r = 34
  const c = 2 * Math.PI * r
  return (
    <div className={`probe-ring tone-${tone}`} title={label}>
      <svg viewBox="0 0 80 80" width="88" height="88" aria-hidden="true">
        <circle cx="40" cy="40" r={r} className="probe-ring-track" />
        <circle cx="40" cy="40" r={r} className="probe-ring-bar" strokeDasharray={`${(pct / 100) * c} ${c}`} transform="rotate(-90 40 40)" />
      </svg>
      <div className="probe-ring-text"><strong>{score === null ? '—' : score}</strong><small>{label}</small></div>
    </div>
  )
}

// risky=true 表示「有问题」那一侧（显示 bad 文字、红色）；null 表示未判定。
function TriFact({ label, risky, good, bad }: { label: string; risky: boolean | null | undefined; good: string; bad: string }) {
  const unknown = risky === null || risky === undefined
  const text = unknown ? '未判定' : risky ? bad : good
  const tone = unknown ? 'muted' : risky ? 'bad' : 'good'
  return <div className="probe-fact"><span>{label}</span><b className={`tone-${tone}`}>{text}</b></div>
}

function ProbeResultView({ r, notify }: { r: IPProbeResult; notify: Props['notify'] }) {
  const p = r.purity
  const conn = connectionMeta((p?.connection_type ?? 'unknown') as IPPurity['connection_type'])
  const scoreLabel = p?.score_source === 'ipqs' ? '欺诈风险分' : 'ASN 滥用率'
  const verdict = (() => {
    if (!p) return { text: '纯净度未知', tone: 'muted' }
    if (p.is_proxy || p.is_vpn || p.is_abuser || (p.fraud_score ?? 0) >= 70) return { text: '有风险标记', tone: 'bad' }
    if ((p.fraud_score ?? 0) >= 30) return { text: '一般', tone: 'warn' }
    return { text: r.native ? '干净 · 原生' : '干净', tone: 'good' }
  })()
  const byKey: Record<string, UnlockResult> = Object.fromEntries((r.unlock ?? []).map((u) => [u.key, u]))
  const dualRisky = p?.is_dual_isp === null || p?.is_dual_isp === undefined ? null : !p.is_dual_isp

  async function copyIP() {
    const ok = await copyToClipboard(r.ip)
    notify(ok ? `已复制 ${r.ip}` : '复制失败，请手动选中复制', ok ? 'info' : 'error')
  }

  return (
    <div className="probe-result">
      <div className="probe-hero">
        <div className="probe-hero-main">
          <div className="probe-ip">
            <button type="button" onClick={() => void copyIP()} title="点击复制 IP">{r.ip}</button>
            <span className={`probe-verdict tone-${verdict.tone}`}>{verdict.text}</span>
          </div>
          <div className="probe-meta">
            {r.location && <span><MapPin size={13} />{r.location}</span>}
            {p?.asn && <span><Network size={13} />{p.asn}{p.asn_org ? ` · ${p.asn_org}` : ''}</span>}
            {r.query !== r.ip && <span><Globe size={13} />{r.query}{r.resolved_ips && r.resolved_ips.length > 1 ? `（解析到 ${r.resolved_ips.length} 个地址，检测其中的 IPv4）` : ''}</span>}
          </div>
          {r.node && <div className="probe-node"><ShieldCheck size={14} />这是你的节点：<b>{r.node.name}</b></div>}
        </div>
        <ScoreRing score={p?.fraud_score ?? null} label={scoreLabel} />
      </div>

      {r.purity_error && <div className="probe-error">纯净度查询失败：{r.purity_error}</div>}

      <div className="probe-facts">
        <div className="probe-fact"><span>连接类型</span><b className={`tone-${conn.tone}`}><conn.Icon size={13} />{conn.label}</b></div>
        <div className="probe-fact"><span>原生 IP</span>
          <b className={`tone-${r.native === null ? 'muted' : r.native ? 'good' : 'warn'}`}>{r.native === null ? '未判定' : r.native ? '原生' : '广播'}</b>
          {r.reg_country && r.geo_country && <small>注册 {countryName(r.reg_country)} → 使用 {countryName(r.geo_country)}</small>}
        </div>
        <TriFact label="双 ISP" risky={dualRisky} good="是" bad="否" />
        <TriFact label="代理标记" risky={p?.is_proxy} good="无" bad="有" />
        <TriFact label="VPN 标记" risky={p?.is_vpn} good="无" bad="有" />
        <TriFact label="滥用记录" risky={p?.is_abuser} good="无" bad="有" />
      </div>

      <div className="probe-unlock">
        <div className="probe-section-title">流媒体与 AI 解锁</div>
        {r.unlock && r.unlock.length > 0 ? (
          <div className="probe-unlock-grid">
            {UNLOCK_COLUMNS.map((c) => {
              const u = byKey[c.key]
              const meta = u ? (UNLOCK_META[u.status] ?? UNLOCK_META.unknown) : { text: '—', tone: 'muted' }
              return (
                <div key={c.key} className={`probe-tile tone-${meta.tone}`} title={u?.note || undefined}>
                  <span>{c.label}</span>
                  <b>{meta.text}</b>
                  {u?.region && u.status !== 'no' && <small>{u.region}</small>}
                </div>
              )
            })}
          </div>
        ) : (
          <div className="probe-note"><Info size={15} /><span>{r.unlock_error ? `节点检测失败：${r.unlock_error}` : r.unlock_note || r.node_lookup_error}</span></div>
        )}
      </div>
    </div>
  )
}
