import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { ShieldCheck, ShieldAlert, ShieldQuestion, Loader2, RefreshCw } from 'lucide-react'
import { api, type IPPurityState, type IPPurity } from '../lib/api'

// IPPurityBadge —— 节点列表里的「IP 纯净度」徽标。
//
// 设计要点
// --------
// 1. **懒加载**：每个徽标挂载时才拉自己的 /api/nodes/{id}/ip-purity。
//    不把纯净度塞进 GET /api/nodes —— 那个接口是列表主路径，几十个节点各带
//    一份 raw JSON 会把它撑肥；而且纯净度更新频率远低于节点状态。
//    拉的是**缓存**（服务端不联网），所以刷页也不会打爆第三方配额。
//
// 2. **三态显示**："未检测 / 检测中 / 有结果"，外加"检测失败但仍显示旧结果"。
//    与项目铁律一致：宁可显示"未判定"，也不给假的绿色。
//
// 3. **null 与 false 分开**：is_dual_isp / is_proxy 等是 boolean|null，
//    null 显示灰色"未判定"，绝不画成红色的"否"。
//
// 4. 检测是显式动作（点徽标里的刷新），不自动出网。

type Props = {
  nodeId: string
  nodeName: string
  // readOnly（viewer）时禁用刷新按钮。
  readOnly: boolean
  notify: (message: string, tone?: 'info' | 'error') => void
}

// 连接类型 → 展示标签与色调。
// 家宽=绿（买家最喜欢的）、机房=蓝（中性，很多节点就是机房）、
// 移动=青、专线=紫、未知=灰。
function connectionMeta(type: IPPurity['connection_type']): { label: string; tone: string } {
  switch (type) {
    case 'residential':
      return { label: '家宽', tone: 'good' }
    case 'datacenter':
      return { label: '机房', tone: 'info' }
    case 'mobile':
      return { label: '移动', tone: 'info' }
    case 'business':
      return { label: '专线', tone: 'info' }
    default:
      return { label: '未知', tone: 'muted' }
  }
}

// 纯净度评分 → 色调。0-29 干净，30-69 中性，70+ 脏。
//
// 阈值说明：ipapi.is 的 abuser_score 换算到 0-100 后，机房 IP 常见 0-20，
// 被滥用过的段常 70+。分三档是为了让运维一眼扫过去能定位问题节点。
function scoreTone(score: number | null): string {
  if (score === null) return 'muted'
  if (score >= 70) return 'bad'
  if (score >= 30) return 'warn'
  return 'good'
}

// triLabel 把负面风险标记（boolean|null）渲染成三段文案：null 一律"未判定"，true 为风险红，false 为干净绿。
function triLabel(value: boolean | null, yes: string, no: string): { text: string; tone: string } {
  if (value === null) return { text: '未判定', tone: 'muted' }
  return value ? { text: yes, tone: 'bad' } : { text: no, tone: 'good' }
}

// 双 ISP 为正面加分特征：true 呈绿色，false 呈中性灰，null 呈未判定（与 IPPurityPage 保持一致）。
function dualIspLabel(value: boolean | null): { text: string; tone: string } {
  if (value === null) return { text: '未判定', tone: 'muted' }
  return value ? { text: '是', tone: 'good' } : { text: '否', tone: 'muted' }
}

// 节点列表里每个节点都有一个徽标。原来每个徽标各发一次 GET /api/nodes/{id}/ip-purity，
// 10 台节点就是 10 个请求（N+1）。现在同一时刻挂载的徽标共用一次 GET /api/ip-purity：
// 第一个徽标发起请求，其余等同一个 Promise；结果只缓存 3 秒，之后的刷新会重新拉取。
let sharedList: Promise<Map<string, IPPurityState>> | null = null

function loadPurityState(nodeId: string): Promise<IPPurityState | null> {
  if (!sharedList) {
    const current = api.ipPurityList().then((list) => new Map((list.items ?? []).map((it): [string, IPPurityState] => [
      it.node_id,
      it.checked && it.purity
        ? { checked: true, purity: it.purity }
        : { checked: false, ip: it.host, reason: it.reason ?? '' },
    ])))
    sharedList = current
    // 成功结果缓存 3 秒防同屏 N+1；若本次请求失败则立即失效，避免 3 秒内后续重试连环复用失败 Promise。
    current.then(
      () => { window.setTimeout(() => { if (sharedList === current) sharedList = null }, 3000) },
      () => { if (sharedList === current) sharedList = null },
    )
  }
  return sharedList.then((m) => m.get(nodeId) ?? null)
}

function formatCheckedAt(iso: string | null | undefined): string {
  if (!iso) return '从未成功检测'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '检测时间未知' : `检测于 ${d.toLocaleString('zh-CN', { hour12: false })}`
}

export function IPPurityBadge({ nodeId, nodeName, readOnly, notify }: Props) {
  const [state, setState] = useState<IPPurityState | null>(null)
  const [loading, setLoading] = useState(false)
  const [refreshing, setRefreshing] = useState(false)
  const [open, setOpen] = useState(false)
  // 弹层挂到 body 上用 fixed 定位：放在表格里会被滚动容器的 overflow 裁掉（最后一行尤其明显）。
  const anchorRef = useRef<HTMLButtonElement>(null)
  const popRef = useRef<HTMLDivElement>(null)
  const [pos, setPos] = useState<{ top: number; left: number } | null>(null)

  useLayoutEffect(() => {
    if (!open) { setPos(null); return }
    const place = () => {
      const a = anchorRef.current?.getBoundingClientRect()
      const pop = popRef.current
      if (!a || !pop) return
      const h = pop.offsetHeight, w = pop.offsetWidth, gap = 6, m = 8
      const below = window.innerHeight - a.bottom - gap
      const top = below >= h + m || a.top < h + m ? Math.min(a.bottom + gap, window.innerHeight - h - m) : a.top - gap - h
      const left = Math.max(m, Math.min(a.left, window.innerWidth - w - m))
      setPos({ top: Math.max(m, top), left })
    }
    place()
    const onDown = (e: MouseEvent) => {
      const t = e.target as Node
      if (!popRef.current?.contains(t) && !anchorRef.current?.contains(t)) setOpen(false)
    }
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false) }
    const onScroll = (e: Event) => { if (!popRef.current?.contains(e.target as Node)) setOpen(false) }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    window.addEventListener('scroll', onScroll, true)
    window.addEventListener('resize', place)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
      window.removeEventListener('scroll', onScroll, true)
      window.removeEventListener('resize', place)
    }
  }, [open])

  const mountedRef = useRef(true)
  const loadSeq = useRef(0)

  const load = useCallback(async () => {
    const seq = ++loadSeq.current
    setLoading(true)
    try {
      // 列表接口里没有这个节点（刚接入等）时退回单节点接口。
      const next = (await loadPurityState(nodeId)) ?? await api.ipPurity(nodeId)
      if (!mountedRef.current || seq !== loadSeq.current) return
      setState(next)
    } catch {
      if (!mountedRef.current || seq !== loadSeq.current) return
      // 读取失败不打扰用户（列表里几十个徽标，弹几十个错误是噪音），
      // 显示成"未知"即可；真正要看原因时点开详情会触发一次刷新。
      setState(null)
    } finally {
      if (mountedRef.current && seq === loadSeq.current) setLoading(false)
    }
  }, [nodeId])

  useEffect(() => {
    mountedRef.current = true
    void load()
    return () => {
      mountedRef.current = false
      loadSeq.current += 1
    }
  }, [load])

  async function refresh(forceApi: boolean) {
    if (refreshing) return
    setRefreshing(true)
    try {
      const result = await api.refreshIPPurity(nodeId, forceApi)
      if (!mountedRef.current) return
      setState({ checked: true, purity: result.purity })
      notify(`${nodeName} 的 IP 纯净度已更新`, 'info')
    } catch (error) {
      if (!mountedRef.current) return
      notify(error instanceof Error ? error.message : 'IP 纯净度检测失败', 'error')
    } finally {
      if (mountedRef.current) setRefreshing(false)
    }
  }

  // 未检测：给一个可点的入口，而不是空白。
  if (!state || !state.checked) {
    const reason = state && !state.checked ? state.reason : ''
    return (
      <button
        className="ip-purity-badge is-empty"
        title={reason || '尚未检测该节点的 IP 纯净度，点击检测'}
        disabled={readOnly || refreshing || !state || !state.ip}
        onClick={() => void refresh(true)}
      >
        {refreshing ? <Loader2 size={11} className="spin" /> : <ShieldQuestion size={11} />}
        <span>IP 纯净度</span>
        {reason && <span className="ip-purity-reason">· {reason}</span>}
      </button>
    )
  }

  const p = state.purity
  const conn = connectionMeta(p.connection_type)
  const tone = scoreTone(p.fraud_score)

  return (
    <span className="ip-purity-wrap">
      <button
        ref={anchorRef}
        aria-expanded={open}
        className={`ip-purity-badge tone-${tone}${open ? ' is-open' : ''}`}
        title="点击查看 IP 纯净度详情"
        onClick={() => setOpen((v) => !v)}
      >
        {loading ? <Loader2 size={11} className="spin" />
          : tone === 'good' ? <ShieldCheck size={11} />
          : tone === 'bad' ? <ShieldAlert size={11} />
          : <ShieldQuestion size={11} />}
        <span className={`ip-purity-conn tone-${conn.tone}`}>{conn.label}</span>
        {p.fraud_score !== null && <span className={`ip-purity-score tone-${tone}`}>{p.fraud_score}</span>}
        {p.stale && <span className="ip-purity-stale" title={`存下来的 IP（${p.ip}）与当前节点 IP 不一致，建议重测`}>IP 已变更</span>}
      </button>

      {open && createPortal(
        <div className="ip-purity-pop" role="dialog" ref={popRef} style={pos ? { top: pos.top, left: pos.left } : { top: -9999, left: -9999 }}>
          <div className="ip-purity-pop-head">
            <strong>IP 纯净度</strong>
            <button
              className="row-more"
              title="重新检测（走第三方 API 拿评分）"
              disabled={readOnly || refreshing}
              onClick={() => void refresh(true)}
            >
              {refreshing ? <Loader2 size={13} className="spin" /> : <RefreshCw size={13} />}
            </button>
          </div>

          <dl className="ip-purity-grid">
            <div><dt>被检测 IP</dt><dd>{p.ip || '—'}</dd></div>
            <div><dt>连接类型</dt><dd><span className={`tone-${conn.tone}`}>{conn.label}（{p.connection_type}）</span></dd></div>
            <div><dt>双 ISP</dt><dd className={`tone-${dualIspLabel(p.is_dual_isp).tone}`}>{dualIspLabel(p.is_dual_isp).text}</dd></div>
            <div><dt>纯净度评分</dt><dd className={`tone-${tone}`}>{p.fraud_score === null ? '未判定' : `${p.fraud_score} / 100`}</dd></div>
            <div><dt>代理标记</dt><dd className={`tone-${triLabel(p.is_proxy, '有', '无').tone}`}>{triLabel(p.is_proxy, '有', '无').text}</dd></div>
            <div><dt>VPN 标记</dt><dd className={`tone-${triLabel(p.is_vpn, '有', '无').tone}`}>{triLabel(p.is_vpn, '有', '无').text}</dd></div>
            <div><dt>滥用记录</dt><dd className={`tone-${triLabel(p.is_abuser, '有', '无').tone}`}>{triLabel(p.is_abuser, '有', '无').text}</dd></div>
            <div><dt>ASN</dt><dd>{p.asn || '—'}{p.asn_org ? ` · ${p.asn_org}` : ''}</dd></div>
          </dl>

          <div className="ip-purity-foot">
            <span>来源 {p.source === 'offline' ? '离线判定' : p.source === 'api' ? '第三方 API' : p.source || '—'}</span>
            <span>{formatCheckedAt(p.checked_at)}</span>
          </div>
          {p.error && <p className="ip-purity-error">上次检测失败：{p.error}（下面显示的是更早一次成功的结果）</p>}
          <p className="ip-purity-note">「未判定」表示数据源没有给出该字段，不等同于「否」。</p>
        </div>,
        document.body,
      )}
    </span>
  )
}
