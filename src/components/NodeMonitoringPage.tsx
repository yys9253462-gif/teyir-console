import { memo, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  Activity, AlertTriangle, ArrowDown, ArrowUp, Cpu, Gauge, Globe, RefreshCw,
  Search, Server as ServerIcon, Wifi, Zap, Loader2, LayoutGrid, List, ChevronRight,
} from 'lucide-react'
import { api, loadSession, type MonitoringNode, type NodeMonitoringResponse } from '../lib/api'
import { formatBytes } from '../lib/format'
import { Modal } from './Modal'
import { BlockCheckPanel } from './BlockCheckPanel'
import { AutoHidePanel } from './AutoHidePanel'
import { ChinaQualityTrend } from './ChinaQualityTrend'
import './NodeMonitoringPage.css'

// 节点监控页 —— 替代原来的「运行总览」。
//
// 它回答一个具体问题：**每台节点现在到底好不好用**。
//
// 页面上有三个容易混淆、必须分开标注的"延迟"口径，这里刻意不合并：
//
//   1. 网络延迟（net_latency_ms）—— 面板到该主机 SSH 端口的**纯 TCP 建连**
//      耗时。这是"网络可达性与质量"，不受对方 sshd 负载影响。
//   2. SSH 握手（ssh_handshake_ms）—— 从拨号到 SSH 建连完成的总耗时。
//      比上面那个多包含了 SSH 协议协商与认证，能看出对方主机忙不忙。
//   3. 三网延迟（icmp_avg_ms）—— 节点本机 ping 大陆靶点的 ICMP 往返。
//      这是"这台 VPS 回国线路好不好"，与前两个测的东西完全不同。
//
// 三者混在一起展示会让用户得出错误结论（比如把一个 SSH 很慢但线路很好的
// 美国节点判成"差"），所以每一处都带明确的口径说明。

// 三网测速的语义边界。由后端在 speedtest_note 里也返回一份，两处一致。
const SPEEDTEST_CAVEAT =
  '测速在节点本机执行，反映「节点 ↔ 大陆三网」的线路质量；不代表你家宽带连这台节点的速度。'

type Props = {
  notify: (message: string) => void
}

// 延迟分档：用于配色。阈值按运维常识取 —— 60ms 以内是优秀，
// 150ms 以内可用，再高就到"能连但体验差"的区间。
// nodeStatusReason 把后端的状态码翻译成一段「为什么是这个状态」的人话。
//
// 这是用户能直接看懂的核心问题：
//   "为什么这台机器没数据？"
//   "为什么这台机器状态是 online 但 CPU 0%？"
//
// 三类典型情况对应三种处理动作（用户最关心的就是「下一步该干什么」）：
//
//   1. 节点门户 API 探测 → online + 节点 status_error='unauthorized'
//      处理动作：换节点门户的 API 密钥
//      视觉：warn（黄）
//
//   2. 主机 SSH 凭据失效 → online + 主机 status_error 含「SSH 凭据失效」
//      处理动作：在主机页重新填 SSH 凭据
//      视觉：warn（黄）—— 比 unauthorized 更"静默"，需要提示用户
//
//   3. 节点真的不通 → offline + 节点 status_error 含 'unreachable'
//      处理动作：去机房/控制台查机器本身
//      视觉：bad（红）—— 这就是故障
//
// 重要：不要给"正常"的节点也挂一行"状态正常"——那会污染视觉，
// 让有问题的节点反而看不清。
type ProbeReason = {
  label: string
  severity: 'warn' | 'bad' | 'info'
} | null

function nodeStatusReason(node: MonitoringNode): ProbeReason {
  // 主机 SSH 凭据失效 —— 即使节点 portal API 还能通，这台机器也没法采指标。
  // 这类问题用户最不易察觉（状态是 online 但指标是空的），
  // 所以放第一条。
  if (node.server_status_error) {
    return { label: `主机：${node.server_status_error}`, severity: 'warn' }
  }

  // 节点不通。
  if (node.node_status === 'offline') {
    const detail = node.node_status_error || '无详细原因'
    return { label: `节点不通：${detail}`, severity: 'bad' }
  }

  // 节点不稳定 —— 连续多轮探测没能得出结果（2026-10-02 接通）。
  //
  // ⚠️ 这条必须放在 unauthorized 之前判：
  // 一个既不稳定、密钥又过期的节点，用户该先看到"它在抖"
  // （那是更根本的问题：连不上就没法判断密钥对不对）。
  //
  // 为什么不显示绿色：warning 是"时好时坏"。
  // 如果按 online 渲染，用户会以为它一直好用 ——
  // 而这恰恰是最该被注意到的情况（客户端会间歇性连不上）。
  if (node.node_status === 'warning') {
    return { label: '节点不稳定：连续多轮探测未成功，时通时断', severity: 'warn' }
  }

  // 节点在线但鉴权失败 —— 实际是在线的，只是采集器看不到完整数据。
  if (node.node_status_error === 'unauthorized') {
    return { label: '节点 API 鉴权失败（密钥可能过期）', severity: 'warn' }
  }

  // 节点探到了，但从没成功过 —— 一般是刚导入还没轮到第一次探测。
  if (node.node_status === 'unknown' && !node.status_source) {
    return { label: '尚未探测', severity: 'info' }
  }

  // 其余 unknown 是"探过但没结论"（密钥解不开等）——
  // 原来这里直接落到 null，于是这类节点在卡片上**没有任何提示**，
  // 看上去和正常节点一样。补一条 info 说明，别让它静默。
  if (node.node_status === 'unknown') {
    return {
      label: node.node_status_error
        ? `无法确定状态：${node.node_status_error}`
        : '无法确定状态：最近一次探测未得出结果',
      severity: 'info',
    }
  }

  if (node.node_status_error) {
    return { label: `本轮节点探测异常：${node.node_status_error}`, severity: 'warn' }
  }

  return null
}

// 只有 SSH 通道能采到 CPU/内存/磁盘；API 通道探测的节点这些值恒为 0，
// 直接显示「0%」会被误读成"空闲"，应显示 —。
//
// ⚠️ 2026-10-30：判据从「上次探测是不是 SSH」改成「有没有新鲜的采样时刻」。
// 原判据只看 status_source：一台 SSH 停采很久、但最后一次探测仍标记为 ssh 的
// 主机，页面会把**一周前的 CPU** 当成当前值显示 —— 和刚采到的一模一样，
// 没有任何一处告诉用户这是旧数据。现在后端只返回 15 分钟内的采样并附带
// metrics_sampled_at，没有它一律按「未采集」渲染。
function hasMetrics(node: MonitoringNode): boolean {
  if (node.status_source !== 'ssh') return false
  if (!node.metrics_sampled_at) return false
  const ageMs = Date.now() - new Date(node.metrics_sampled_at).getTime()
  return Number.isFinite(ageMs) && ageMs <= 15 * 60 * 1000
}

function percentText(node: MonitoringNode, value: number): string {
  return hasMetrics(node) ? `${Math.round(value)}%` : '—'
}

const STATUS_LABELS: Record<string, string> = {
  online: '在线', warning: '不稳定', offline: '离线', unknown: '未知',
}

function latencyTone(ms: number | null): 'good' | 'ok' | 'bad' | 'none' {
  if (ms === null || ms === undefined) return 'none'
  if (ms <= 60) return 'good'
  if (ms <= 150) return 'ok'
  return 'bad'
}

function toneClass(tone: ReturnType<typeof latencyTone>): string {
  switch (tone) {
    case 'good': return 'lat-good'
    case 'ok': return 'lat-ok'
    case 'bad': return 'lat-bad'
    default: return 'lat-none'
  }
}

function formatMs(ms: number | null | undefined): string {
  if (ms === null || ms === undefined) return '未测得'
  // 0ms 要显式写成 <1 ms，不能直接显示 "0 ms"。
  //
  // 理由：面板与本机同机（或极近）时 TCP 建连的毫秒取整后确实是 0，
  // 但 "0 ms" 在界面上和 "未测得" 极难区分 —— 用户会以为延迟低到不存在，
  // 或者以为这个数字是坏的。写成 "<1 ms" 明确表达"极低但确实测到了"。
  if (ms < 1) return '<1 ms'
  return `${Math.round(ms)} ms`
}

function formatLoss(pct: number | null | undefined): string {
  if (pct === null || pct === undefined) return '—'
  return `${pct.toFixed(1)}%`
}

function formatTime(iso: string | null): string {
  if (!iso) return '—'
  const date = new Date(iso)
  const ts = date.getTime()
  if (Number.isNaN(ts)) return '—'
  const diffMs = Date.now() - ts
  const minutes = Math.floor(diffMs / 60000)
  if (minutes < 1) return '刚刚'
  if (minutes < 60) return `${minutes} 分钟前`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours} 小时前`
  return date.toLocaleDateString('zh-CN', { month: '2-digit', day: '2-digit' })
}

// 运营商的固定展示顺序：电信 / 联通 / 移动 / 公共。
// 不按 map 的键顺序渲染 —— JS 对象的键顺序在混合数字键时不可靠，
// 而"三网卡片每次刷新换个顺序"会让人以为数据错了。
//
// ⚠️ 这四组是**固定渲染**的，不是有数据才渲染。
// 原来的写法是 `.filter(c => by_carrier[c])`，只渲染有数据的组。
// 电信/联通的靶点当时全部 enabled=false（迁移 016 实测 100% 丢包），
// 于是这两组一个数据都没有 → 整组消失 → 用户看到「三网数据待测」，
// 而真实情况是"压根没启用电信/联通的靶点"。
// 两种情况在界面上长得一模一样 —— 这就是缺信息。
// 现在没数据也占位显示「未测得」，用户能分清"测了不通"和"没测"。
const CARRIER_ORDER = ['telecom', 'unicom', 'mobile', 'other'] as const

const CARRIER_LABELS: Record<string, string> = {
  telecom: '电信',
  unicom: '联通',
  mobile: '移动',
  other: '公共',
}

const CARD_CARRIERS = ['telecom', 'unicom', 'mobile'] as const

// 丢包分档：实测基线三网单轮平均 5%–14%（含偶发全丢的靶点），
// 所以 <5% 算好、<15% 是常态、≥15% 才值得注意（告警线是 30%）。
function lossClass(pct: number | null | undefined): string {
  if (pct === null || pct === undefined) return 'lat-none'
  if (pct < 5) return 'lat-good'
  if (pct < 15) return 'lat-ok'
  return 'lat-bad'
}

function carrierName(key: string): string {
  // 未知分类不隐藏也不编造，原样显示 —— 后端 carrierLabel 也是这个态度。
  return CARRIER_LABELS[key] ?? key
}

type MonitorView = 'cards' | 'table'
type MonitorSort = 'attention' | 'name' | 'traffic' | 'latency'
const VIEW_KEY = 'nodeops.monitoring.view'

function needsAttention(node: MonitoringNode): boolean {
  return node.node_status !== 'online' || Boolean(node.node_status_error || node.server_status_error)
}

function readView(): MonitorView {
  try { return localStorage.getItem(VIEW_KEY) === 'table' ? 'table' : 'cards' } catch { return 'cards' }
}

export function NodeMonitoringPage({ notify }: Props) {
  const [data, setData] = useState<NodeMonitoringResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [testing, setTesting] = useState(false)
  const [error, setError] = useState('')
  const [selected, setSelected] = useState<MonitoringNode | null>(null)
  const [nodeFilter, setNodeFilter] = useState<'all' | 'attention'>('all')
  const [nodeQuery, setNodeQuery] = useState('')
  const [view, setView] = useState<MonitorView>(readView)
  const [sort, setSort] = useState<MonitorSort>('attention')
  const [region, setRegion] = useState('')
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null)

  function changeView(next: MonitorView) {
    setView(next)
    try { localStorage.setItem(VIEW_KEY, next) } catch { /* 隐私模式仍可切换。 */ }
  }

  // 2026-11-06：30 秒定时刷新与测速等待循环会并发调用 load；晚到的旧响应原来会覆盖新数据，
  // 测速完成判断（measured_at 是否变化）随之读到旧值。只采用最后一次发起的请求结果。
  const loadSeq = useRef(0)
  // 保存最新一份数据，供「等测速真跑完」的轮询判断用。load 里直接写入：
  // 原来只靠 useEffect 在提交后同步，waitForSpeedtest 在 await load() 之后立刻读，读到的常是上一份。
  const dataRef = useRef<NodeMonitoringResponse | null>(null)
  // 离开页面后，测速等待循环与数据请求不能再写状态（2026-10-30）。
  const mounted = useRef(true)
  const waitTimers = useRef<Set<number>>(new Set())
  const load = useCallback(async (quiet = false) => {
    const seq = ++loadSeq.current
    if (!quiet) setLoading(true)
    let ok = false
    try {
      const raw = await api.nodeMonitoring()
      if (!mounted.current || seq !== loadSeq.current) return true
      const result: NodeMonitoringResponse = { ...raw, nodes: raw.nodes ?? [] }
      dataRef.current = result
      setData(result)
      setUpdatedAt(new Date())
      setError('')
      ok = true
      // 刷新后同步抽屉里的那份数据，否则抽屉会一直显示旧值。
      setSelected((current) => {
        if (!current) return null
        return result.nodes.find((n) => n.node_id === current.node_id) ?? null
      })
    } catch (err) {
      if (!mounted.current || seq !== loadSeq.current) return false
      setError(err instanceof Error ? err.message : '读取节点监控数据失败')
    } finally {
      if (mounted.current && seq === loadSeq.current) setLoading(false)
    }
    return ok
  }, [])

  useEffect(() => { void load() }, [load])

  // 定时刷新。30 秒是权衡：这个页面的数据来自后台周期采集（默认 30s）
  // 与测速（默认 15min），刷新得比采集更快没有意义。
  useEffect(() => {
    // 2026-11-06：标签页在后台时不轮询（与订阅中心等页面一致），回到前台立即补一次。
    // 原来后台也每 30 秒拉一次整份监控数据（全部节点 + 三网测速汇总），开着不看的标签页一直在压数据库。
    const timer = window.setInterval(() => { if (!document.hidden) void load(true) }, 30_000)
    const onVisible = () => { if (!document.hidden) void load(true) }
    document.addEventListener('visibilitychange', onVisible)
    return () => { window.clearInterval(timer); document.removeEventListener('visibilitychange', onVisible) }
  }, [load])

  const reloadTimer = useRef<number>()
  const viewer = loadSession()?.role === 'viewer'
  const [testingNodes, setTestingNodes] = useState<Set<string>>(() => new Set())
  const setNodeTesting = useCallback((nodeId: string, active: boolean) => {
    setTestingNodes((prev) => {
      const next = new Set(prev)
      if (active) next.add(nodeId)
      else next.delete(nodeId)
      return next
    })
  }, [])
  useEffect(() => { dataRef.current = data }, [data])
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
      loadSeq.current += 1
      window.clearTimeout(reloadTimer.current)
      for (const id of waitTimers.current) window.clearTimeout(id)
      waitTimers.current.clear()
    }
  }, [])

  function waitWithCancel(ms: number): Promise<void> {
    return new Promise((resolve) => {
      const id = window.setTimeout(() => {
        waitTimers.current.delete(id)
        resolve()
      }, ms)
      waitTimers.current.add(id)
    })
  }

  // 等这一台节点的测速**真的**产出新一轮结果（measured_at 变了），再撤掉忙碌态。
  //
  // ⚠️ 2026-10-30：原来的写法是发起后固定 20 秒就清忙碌态。
  // 但 portal 通道的测速实测可达 100 秒以上（AGENTS 里记过 38 秒才落库的样本），
  // 20 秒后按钮就恢复可点，用户看着"没反应"会再点一次 —— 重复触发同一节点测速，
  // 既浪费节点资源，又让结果互相覆盖。
  // 现在最多等 3 分钟，按 measured_at 变化判定完成；超时也一定复位，不会永久转圈。
  async function waitForSpeedtest(node: MonitoringNode, before: string) {
    for (let i = 0; i < 9; i += 1) {
      await waitWithCancel(20_000)
      if (!mounted.current) return
      await load(true)
      if (!mounted.current) return
      const after = dataRef.current?.nodes.find((n) => n.node_id === node.node_id)?.speedtest?.measured_at ?? ''
      if (after && after !== before) return
    }
  }

  // node 传了就只测这一个节点（卡片上的测速按钮）。
  async function runSpeedtest(node?: MonitoringNode) {
    if (viewer) return
    if (node) {
      if (testingNodes.has(node.node_id)) return
      setNodeTesting(node.node_id, true)
    } else {
      if (testing) return
      setTesting(true)
    }
    try {
      const before = dataRef.current?.nodes.find((n) => n.node_id === node?.node_id)?.speedtest?.measured_at ?? ''
      const result = await api.runSpeedtest(node?.node_id)
      if (!mounted.current) return
      // 后端没有启动任何测速（无 SSH 凭据也没绑节点 API）时返回 200 + started=0：
      // 原来照样轮询 9×20 秒等一个不可能出现的结果，跳过原因也没显示（2026-11-03）。
      if (result.started === 0) {
        const why = result.skipped?.length ? `：${result.skipped.join('；')}` : ''
        notify(`${result.message}${why}`)
        if (node) setNodeTesting(node.node_id, false); else setTesting(false)
        return
      }
      notify(result.message)
      // 后台跑，所以隔一会儿自动刷新看结果；同时告诉用户在等。
      window.clearTimeout(reloadTimer.current)
      reloadTimer.current = window.setTimeout(() => { if (mounted.current) void load(true) }, 20_000)
      if (node) {
        await waitForSpeedtest(node, before)
        if (mounted.current) setNodeTesting(node.node_id, false)
      } else if (mounted.current) {
        setTesting(false)
      }
    } catch (err) {
      if (!mounted.current) return
      notify(err instanceof Error ? `测速失败：${err.message}` : '测速失败')
      if (node) setNodeTesting(node.node_id, false); else setTesting(false)
    }
  }

  const runSpeedtestRef = useRef(runSpeedtest)
  runSpeedtestRef.current = runSpeedtest
  const handleOpenNode = useCallback((node: MonitoringNode) => setSelected(node), [])
  const handleNodeSpeedtest = useCallback((node: MonitoringNode) => { void runSpeedtestRef.current(node) }, [])

  const nodes = data?.nodes ?? []
  const stats = useMemo(() => {
    const online = nodes.filter((n) => n.node_status === 'online').length
    // ⚠️ warning 必须单独计数。
    // 原来的写法是 online 取 'online'、offline 取 'offline' || 'unknown'，
    // 于是 warning 节点**从两个计数里都掉了出去** ——
    // 「在线 4 / 5」既不是 4 也不是 5 的解释，用户会以为还有一台是未知。
    const unstable = nodes.filter((n) => n.node_status === 'warning').length
    const offline = nodes.filter((n) => n.node_status === 'offline').length
    const unknown = nodes.filter((n) => n.node_status === 'unknown').length
    const withLatency = nodes.filter((n) => n.net_latency_ms !== null)
    const avgLatency = withLatency.length > 0
      ? Math.round(withLatency.reduce((sum, n) => sum + (n.net_latency_ms ?? 0), 0) / withLatency.length)
      : null
    const totalTx = nodes.reduce((sum, n) => sum + n.tx_bytes_24h, 0)
    const totalRx = nodes.reduce((sum, n) => sum + n.rx_bytes_24h, 0)
    const tested = nodes.filter((n) => n.speedtest).length
    const attention = nodes.filter(needsAttention).length
    // API 通道探测的节点没有 CPU/内存（恒为 0），算进均值会把平均值拉低。
    const metered = nodes.filter(hasMetrics)
    const avgCpu = metered.length ? Math.round(metered.reduce((sum, n) => sum + n.cpu_percent, 0) / metered.length) : null
    const avgMemory = metered.length ? Math.round(metered.reduce((sum, n) => sum + n.memory_percent, 0) / metered.length) : null
    // 未测的节点名 —— 让「三网已测 3/5」能落到具体是哪几台。
    // 只在真未测时计算，正常时不额外遍历。
    const untested = tested === nodes.length
      ? []
      : nodes.filter((n) => !n.speedtest).map((n) => n.server_name || n.node_name)
    return { online, unstable, offline, unknown, attention, avgLatency, totalTx, totalRx, tested, untested, avgCpu, avgMemory, metered: metered.length }
  }, [nodes])

  // ⚠️ 2026-10-25 五轮排查第四轮：地区列表要 memo。
  // 原来每次渲染都重建数组并排序 —— 30 秒自动刷新之外，任何一次输入
  // （搜索框每敲一个字）都会重跑。节点多时这是白白的 O(n log n) × 每次按键。
  const regions = useMemo(
    () => [...new Set(nodes.map((node) => node.region).filter(Boolean))].sort((a, b) => a.localeCompare(b, 'zh-CN')),
    [nodes],
  )
  const filteredNodes = useMemo(() => nodes.filter((node) => {
    const matchesFilter = nodeFilter === 'all' || needsAttention(node)
    const name = `${node.server_name} ${node.node_name} ${node.region} ${node.provider} ${node.host}`.toLowerCase()
    return matchesFilter && (!region || node.region === region) && name.includes(nodeQuery.trim().toLowerCase())
  }).sort((a, b) => {
    if (sort === 'traffic') return (b.tx_bytes_24h + b.rx_bytes_24h) - (a.tx_bytes_24h + a.rx_bytes_24h)
    if (sort === 'latency') return (a.net_latency_ms ?? Infinity) - (b.net_latency_ms ?? Infinity)
    if (sort === 'attention') {
      const priority = (node: MonitoringNode) => node.node_status === 'offline' ? 0 : needsAttention(node) ? 1 : 2
      const difference = priority(a) - priority(b)
      if (difference) return difference
    }
    return (a.server_name || a.node_name).localeCompare(b.server_name || b.node_name, 'zh-CN')
  }), [nodes, nodeFilter, nodeQuery, region, sort])
  const maxTraffic = useMemo(
    () => Math.max(1, ...nodes.map((node) => node.tx_bytes_24h + node.rx_bytes_24h)),
    [nodes],
  )
  const trafficRanked = useMemo(
    () => nodes.slice().sort((a, b) => b.tx_bytes_24h + b.rx_bytes_24h - a.tx_bytes_24h - a.rx_bytes_24h),
    [nodes],
  )

  // 三网汇总：把所有节点的 by_carrier 合并成"全网三网质量"。
  // 这是首屏最有价值的一个数字 —— "我的节点整体回国线路怎么样"。
  //
  // 固定返回四组（含一个节点数据都没有的组），理由见 CARRIER_ORDER 的注释：
  // 全网级汇总更不能"缺一组就少一张卡" ——
  // 否则「电信整体很差」和「电信压根没测」在页面上完全无从分辨。
  // 卡片顶部的三网汇总：只算 telecom/unicom/mobile，按「节点 × 运营商」取平均。
  const fleetQuality = useMemo(() => {
    let msSum = 0, msCount = 0, lossSum = 0, lossCount = 0
    const lossy: string[] = []
    for (const node of nodes) {
      let worst = 0
      for (const carrier of CARD_CARRIERS) {
        const agg = node.speedtest?.by_carrier?.[carrier]
        if (!agg) continue
        if (agg.avg_ms !== null) { msSum += agg.avg_ms; msCount++ }
        if (agg.avg_loss_pct !== null && agg.avg_loss_pct !== undefined) {
          lossSum += agg.avg_loss_pct; lossCount++
          worst = Math.max(worst, agg.avg_loss_pct)
        }
      }
      if (worst >= 15) lossy.push(node.server_name || node.node_name)
    }
    return {
      avgMs: msCount ? Math.round(msSum / msCount) : null,
      avgLoss: lossCount ? lossSum / lossCount : null,
      lossy,
    }
  }, [nodes])

  const fleetCarriers = useMemo(() => {
    const acc: Record<string, { sum: number; count: number; worst: number; worstName: string }> = {}
    for (const node of nodes) {
      const summary = node.speedtest
      if (!summary) continue
      for (const [carrier, agg] of Object.entries(summary.by_carrier ?? {})) {
        if (agg.avg_ms === null) continue
        const entry = acc[carrier] ?? { sum: 0, count: 0, worst: 0, worstName: '' }
        entry.sum += agg.avg_ms
        entry.count += 1
        if (agg.avg_ms > entry.worst) {
          entry.worst = agg.avg_ms
          entry.worstName = node.server_name || node.node_name
        }
        acc[carrier] = entry
      }
    }
    return CARRIER_ORDER.map((carrier) => {
      const entry = acc[carrier]
      return {
        carrier,
        label: carrierName(carrier),
        // count 为 0 时 avg 必须是 null 而不是 0 ——
        // "没有节点测到电信"和"电信平均 0ms"是完全不同的意思。
        avg: entry && entry.count > 0 ? Math.round(entry.sum / entry.count) : null,
        count: entry?.count ?? 0,
        worst: entry && entry.count > 0 ? Math.round(entry.worst) : null,
        worstName: entry?.worstName ?? '',
      }
    })
  }, [nodes])

  return (
    <div className="monitoring-home">
      <div className="page-heading monitor-heading">
        <div>
          <h1>节点监控<span className="heading-period">.</span></h1>
          <p className="page-subtitle">连通状态、资源用量与回国线路，一眼掌握。</p>
        </div>
        <div className="heading-actions">
          <span className="updated-label"><span className="live-dot" />30 秒自动刷新{updatedAt && <small>更新于 {updatedAt.toLocaleTimeString('zh-CN', { hour12: false })}</small>}</span>
          <button className="button button-quiet" disabled={loading} onClick={async () => { if (await load()) notify('已刷新') }}>
            <RefreshCw size={15} className={loading ? 'spin' : ''} />刷新
          </button>
          <button className="button button-primary" onClick={() => void runSpeedtest()} disabled={testing || viewer} title={viewer ? '只读账号不能发起测速' : undefined}>
            <Zap size={16} />{testing ? '提交中…' : '立即测速'}
          </button>
        </div>
      </div>

      {error && (
        <div className="drawer-notice notice-error" style={{ marginBottom: 14 }}>
          <AlertTriangle size={17} />
          <span><strong>读取失败</strong><small>{error}</small></span>
        </div>
      )}
      {!error && data?.speedtest_error && (
        <div className="drawer-notice notice-error" style={{ marginBottom: 14 }}>
          <AlertTriangle size={17} />
          <span><strong>三网测速未加载</strong><small>{data.speedtest_error}</small></span>
        </div>
      )}

      <section className="metrics-grid monitoring-kpis" aria-label="节点概览">
        <div className="metric-card monitor-online"><div className="metric-head"><span>在线节点</span><Wifi size={17} /></div><div className="metric-body"><strong>{data ? stats.online : '—'}</strong><span className="metric-unit">/ {data ? nodes.length : '—'}</span></div><div className="metric-foot">{data ? `${stats.offline} 离线 / ${stats.unstable} 不稳定 / ${stats.unknown} 未知` : '等待监控数据'}</div></div>
        {/* 2026-11-01：原来这里是「平均网络延迟」（面板所在东京 → 主机的建连耗时），
            与客户回国体验无关；换成三家运营商的平均延迟。 */}
        <div className="metric-card">
          <div className="metric-head"><span>三网平均延迟</span><Gauge size={17} /></div>
          <div className="metric-body">
            <strong className={toneClass(latencyTone(fleetQuality.avgMs))}>{fleetQuality.avgMs === null ? '—' : fleetQuality.avgMs}</strong>
            <span className="metric-unit">{fleetQuality.avgMs === null ? '' : 'ms'}</span>
          </div>
          <div className="metric-foot">电信 / 联通 / 移动，各节点最近一轮的均值</div>
        </div>
        <div className="metric-card">
          {/* 口径必须写清：这个数有两个来源，含义不同。
              - hysteria 自带 trafficStats（每轮清零，真实上下行，分 tx/rx）
              - portal 记账值（累计计数器，只有出站，无入站）
              没开 trafficStats 的节点用后者兜底 —— 而它**没有入站数据**，
              所以入站会显示 0。不说清楚的话，"入站 0"看着像坏了。 */}
          <div className="metric-head"><span>24h 流量</span><ArrowUp size={17} /></div>
          <div className="metric-body"><strong>{data ? formatBytes(stats.totalTx) : '—'}</strong><span className="metric-unit">出站</span></div>
          <div className="metric-foot">
            {stats.totalRx > 0
              ? <><span className="metric-up"><ArrowDown size={12} /> {formatBytes(stats.totalRx)}</span> 入站</>
              : <>含未开 trafficStats 的节点（仅记账出站，无入站）</>}
          </div>
        </div>
        <div className="metric-card">
          <div className="metric-head"><span>三网已测</span><Globe size={17} /></div>
          <div className="metric-body"><strong>{data ? stats.tested : '—'}</strong><span className="metric-unit">/ {data ? nodes.length : '—'}</span></div>
          {/* 三网测速有**两条**通道，任一条可用即可采到：
                ① 节点 portal 的 /api/v1/speedtest（推荐，不需要任何 SSH）
                ② 面板 SSH 过去跑探针（需该主机的 SSH 凭据）
              所以这里**不能**再说「需绑定 SSH 凭据」—— 那是旧版只有一条通道
              时的说法。现在真正的前提是「节点已接入 portal API」，
              而没接 portal 又没 SSH 的才会落进未测名单。
              文案要给的是"是哪几台"，用户才能去处理。 */}
          <div className="metric-foot">
            {stats.untested.length > 0
              ? <>未测：{stats.untested.slice(0, 3).join('、')}{stats.untested.length > 3 ? ` 等 ${stats.untested.length} 台` : ''}；详情可查看采集状态</>
              : nodes.length ? '在节点本机测得' : '等待节点测速数据'}
          </div>
        </div>
        {/* CPU / 内存均值换成丢包：大部分节点走 API 接入、没有资源指标，两张卡常年显示「—」。
            资源数据仍在节点卡底栏和详情里（有的才显示）。 */}
        <div className="metric-card"><div className="metric-head"><span>三网平均丢包</span><Activity size={17} /></div><div className="metric-body"><strong className={lossClass(fleetQuality.avgLoss)}>{fleetQuality.avgLoss === null ? '—' : formatLoss(fleetQuality.avgLoss)}</strong></div><div className="metric-foot">偶发单靶点全丢属正常，持续 ≥30% 会告警</div></div>
        <div className="metric-card"><div className="metric-head"><span>丢包偏高</span><AlertTriangle size={17} /></div><div className="metric-body"><strong className={fleetQuality.lossy.length ? 'lat-bad' : 'lat-good'}>{data ? fleetQuality.lossy.length : '—'}</strong><span className="metric-unit">台</span></div><div className="metric-foot">{fleetQuality.lossy.length ? `≥15%：${fleetQuality.lossy.slice(0, 3).join('、')}${fleetQuality.lossy.length > 3 ? ` 等 ${fleetQuality.lossy.length} 台` : ''}` : '没有节点任一运营商丢包 ≥15%'}</div></div>
      </section>

      <section className="panel monitoring-nodes-panel">
        <div className="section-head monitoring-list-head">
          <div><h2>节点状态 <span className="monitor-count">{filteredNodes.length} / {nodes.length}</span></h2><p className="monitor-section-note">点击节点查看诊断与逐靶点明细</p></div>
          <div className="monitor-view" role="group" aria-label="节点视图">
            <button type="button" aria-pressed={view === 'cards'} className={view === 'cards' ? 'active' : ''} onClick={() => changeView('cards')}><LayoutGrid size={15} />卡片</button>
            <button type="button" aria-pressed={view === 'table'} className={view === 'table' ? 'active' : ''} onClick={() => changeView('table')}><List size={15} />列表</button>
          </div>
        </div>
        <div className="monitor-toolbar">
          <div className="monitor-filter" role="group" aria-label="节点筛选">
            <button aria-pressed={nodeFilter === 'all'} className={nodeFilter === 'all' ? 'active' : ''} onClick={() => setNodeFilter('all')}>全部 <b>{nodes.length}</b></button>
            <button aria-pressed={nodeFilter === 'attention'} className={nodeFilter === 'attention' ? 'active' : ''} onClick={() => setNodeFilter('attention')}><AlertTriangle size={13} />需关注 <b>{stats.attention}</b></button>
          </div>
          <div className="monitor-list-tools">
            <label className="monitor-search"><Search size={14} /><input value={nodeQuery} onChange={(event) => setNodeQuery(event.target.value)} placeholder="搜索节点、地区、地址" aria-label="搜索节点" /></label>
            <select aria-label="筛选地区" value={region} onChange={(event) => setRegion(event.target.value)}><option value="">全部地区</option>{regions.map((item) => <option key={item} value={item}>{item}</option>)}</select>
            <select aria-label="节点排序" value={sort} onChange={(event) => setSort(event.target.value as MonitorSort)}><option value="attention">需关注优先</option><option value="name">按名称</option><option value="traffic">24h 流量从高到低</option><option value="latency">TCP 延迟从低到高</option></select>
          </div>
        </div>
        {loading && !data ? <div className="empty-state"><RefreshCw size={20} className="spin" /><strong>加载中…</strong><span>正在读取节点状态。</span></div>
          : error && !data ? <div className="empty-state"><AlertTriangle size={20} /><strong>暂时无法读取节点</strong><span>请检查连接后刷新，读取失败不代表节点离线。</span></div>
          : nodes.length === 0 ? <div className="empty-state"><ServerIcon size={20} /><strong>还没有节点</strong><span>到「HY2 节点」页接入节点后，这里会显示状态与三网数据。</span></div>
          : filteredNodes.length === 0 ? <div className="empty-state"><Search size={20} /><strong>没有符合条件的节点</strong><button className="button button-quiet" onClick={() => { setNodeQuery(''); setRegion(''); setNodeFilter('all') }}>清除筛选</button></div>
          : view === 'cards' ? <div className="node-monitor-grid">{filteredNodes.map((node) => <NodeCard key={node.node_id} node={node} onOpen={handleOpenNode} testing={testingNodes.has(node.node_id)} readOnly={viewer} onSpeedtest={handleNodeSpeedtest} />)}</div>
          : <NodeTable nodes={filteredNodes} testingNodes={testingNodes} readOnly={viewer} onOpen={handleOpenNode} onSpeedtest={handleNodeSpeedtest} />}
      </section>

      <div className="monitor-analysis">
      <section className="panel traffic-panel">
        <div className="section-head"><div><h2>节点流量分布</h2><p className="monitor-section-note">近 24 小时出站 + 入站</p></div><span className="range-select">24 小时</span></div>
        {nodes.length ? <div className="traffic-list">{trafficRanked.map((node) => {
          const tx = node.tx_bytes_24h || 0
          const rx = node.rx_bytes_24h || 0
          const total = tx + rx
          return <button type="button" className="traffic-row" key={node.node_id} onClick={() => setSelected(node)} aria-label={`${node.server_name || node.node_name}，总流量 ${formatBytes(total)}，查看详情`}><span className="traffic-name">{node.server_name || node.node_name}</span><div className="traffic-track"><i className="traffic-total" style={{ width: `${total / maxTraffic * 100}%` }} /></div><strong>{formatBytes(total)}</strong><span className="traffic-tooltip"><b>{node.server_name || node.node_name}</b><span>出站 {formatBytes(tx)}</span><span>入站 {formatBytes(rx)}</span></span></button>
        })}</div> : <div className="monitor-empty">接入节点后显示真实流量分布</div>}
        <div className="chart-footnote">按节点上报的近 24 小时流量排序；无流量节点仍保留显示。</div>
      </section>

      {/* 全网三网质量：把各节点的三网数据合并成一眼能看的结论。
          固定四张卡（含没数据的），否则"电信没测"会整组消失 —— 见 CARRIER_ORDER 注释。 */}
      <section className="panel monitor-carrier-panel">
        <div className="section-head">
          <div>
            <h2>全网三网质量</h2>
            <p className="monitor-section-note">节点本机 → 大陆靶点</p>
          </div>
          <span className="range-select" title={SPEEDTEST_CAVEAT}>按运营商平均</span>
        </div>
        <div className="carrier-grid">
          {fleetCarriers.map((item) => (
            <div className={`carrier-card${item.avg === null ? ' untested' : ''}`} key={item.carrier}>
              <div className="carrier-name">{item.label}</div>
              {item.avg === null ? (
                <div className="carrier-value lat-none">未测得<span>·</span></div>
              ) : (
                <div className={`carrier-value ${toneClass(latencyTone(item.avg))}`}>
                  {item.avg}<span>ms</span>
                </div>
              )}
              <div className="carrier-meta">
                {item.count > 0 ? (
                  <>
                    {item.count} 台节点有数据
                    {item.worstName && <><br />最差 {item.worst}ms（{item.worstName}）</>}
                  </>
                ) : (
                  <>暂无节点有该运营商的测速数据</>
                )}
              </div>
            </div>
          ))}
        </div>
        <p className="form-note form-note-hint">
          <Globe size={13} />
          <span>{SPEEDTEST_CAVEAT}</span>
        </p>
      </section>

      </div>

      <AutoHidePanel notify={notify} />
      <BlockCheckPanel notify={notify} />

      {selected && (
        <NodeDetailModal node={selected} onClose={() => setSelected(null)} />
      )}
    </div>
  )
}

function NodeStatus({ node }: { node: MonitoringNode }) {
  return <span className={`monitor-status ${node.node_status}`}><i />{STATUS_LABELS[node.node_status] ?? node.node_status}</span>
}

function ResourceMeter({ node, value, label }: { node: MonitoringNode; value: number; label: string }) {
  const available = hasMetrics(node) && Number.isFinite(value)
  const bounded = Math.min(100, Math.max(0, value))
  // 两种「没有」要分开说：走 API 探测的节点本来就没有这些指标；
  // 而走 SSH 但采样已经过期，是采集停了 —— 后者需要去查，前者不用。
  const missingHint = node.status_source === 'ssh'
    ? `${label} 采样已过期（超过 15 分钟没有新数据，主机采集可能已中断）`
    : `${label} 未采集，节点 API 不提供此指标`
  return <span className={`monitor-resource${available ? bounded >= 90 ? ' critical' : bounded >= 75 ? ' elevated' : '' : ' unavailable'}`} title={available ? `${label} ${Math.round(value)}%` : missingHint}>
    <strong>{available ? `${Math.round(value)}%` : '—'}</strong>
    <span className="monitor-resource-track" aria-hidden="true">{available && <i style={{ width: `${bounded}%` }} />}</span>
  </span>
}

const NodeTable = memo(function NodeTable({ nodes, testingNodes, readOnly, onOpen, onSpeedtest }: { nodes: MonitoringNode[]; testingNodes: Set<string>; readOnly: boolean; onOpen: (node: MonitoringNode) => void; onSpeedtest: (node: MonitoringNode) => void }) {
  return <div className="monitor-table-scroll" tabIndex={0} role="region" aria-label="节点列表，可横向滚动">
    <table className="monitor-table">
      <thead><tr><th scope="col">节点 / 状态原因</th><th scope="col">状态</th><th scope="col">TCP 延迟</th><th scope="col">CPU / 内存</th><th scope="col">电信 / 联通 / 移动 / 公共</th><th scope="col">24h 出站 / 入站</th><th scope="col">用户</th><th scope="col">操作</th></tr></thead>
      <tbody>{nodes.map((node) => {
        const reason = nodeStatusReason(node)
        const isTesting = testingNodes.has(node.node_id)
        return <tr key={node.node_id}>
          <td><button className="monitor-node-link" onClick={() => onOpen(node)}>{node.server_name || node.node_name}<ChevronRight size={13} /></button><small className="monitor-node-meta">{[node.region, node.provider].filter(Boolean).join(' / ') || node.host}</small>{reason && <div className={`node-card-reason ${reason.severity}`}>{reason.label}</div>}</td>
          <td><NodeStatus node={node} /></td>
          <td><span title="面板 → 主机 TCP 建连">{formatMs(node.net_latency_ms)}</span></td>
          <td><div className="monitor-table-resources"><ResourceMeter node={node} value={node.cpu_percent} label="CPU" /><ResourceMeter node={node} value={node.memory_percent} label="内存" /></div></td>
          <td><div className="monitor-table-carriers">{CARRIER_ORDER.map((carrier) => <span key={carrier} title={carrierName(carrier)}><small>{carrierName(carrier)}</small>{formatMs(node.speedtest?.by_carrier?.[carrier]?.avg_ms)}</span>)}</div>{!node.speedtest && <small className="monitor-node-meta">尚无测速数据，查看详情了解采集方式</small>}</td>
          <td><div className="monitor-table-traffic"><span>↑ {formatBytes(node.tx_bytes_24h)}</span><span>↓ {formatBytes(node.rx_bytes_24h)}</span></div></td>
          <td>{node.users_count >= 0 ? node.users_count : '—'}</td>
          <td><button className="node-card-speed" disabled={readOnly || isTesting} title={readOnly ? '只读账号不能发起测速' : undefined} aria-label={`测速 ${node.server_name || node.node_name}`} onClick={() => onSpeedtest(node)}>{isTesting ? <Loader2 size={13} className="spin" /> : <Zap size={13} />}测速</button><button className="monitor-details" onClick={() => onOpen(node)} aria-label={`查看 ${node.server_name || node.node_name} 详情`}>详情</button></td>
        </tr>
      })}</tbody>
    </table>
  </div>
})

// 卡片用于巡检，列表用于横向比较；两者共用状态与资源的显示语义。
const NodeCard = memo(function NodeCard({ node, onOpen, testing, readOnly, onSpeedtest }: { node: MonitoringNode; onOpen: (node: MonitoringNode) => void; testing: boolean; readOnly: boolean; onSpeedtest: (node: MonitoringNode) => void }) {
  const unstable = node.node_status === 'warning'
  const summary = node.speedtest

  // 三网固定四行渲染，不做 filter。
  //
  // 逐靶点明细（哪个城市多少 ms）挪到详情抽屉了 —— 卡片上只留每家的平均，
  // 高度可控；需要看细节时点开卡片。
  //
  // 有数据 → ms 数 + 分档配色；没数据 → 「未测得」+ 暗色。
  // 这里**不能**像以前那样"没数据就整组不显示"：那会让用户误以为
  // 电信/联通"测不出来"，而真实情况往往是"压根没启用那两组的靶点"。
  // 2026-11-01：卡片只留三家运营商（「公共」是 DNS 靶点，不代表回国线路，
  // 放详情里），每家同时给延迟和丢包 —— 只给延迟时，200ms 不丢包与
  // 200ms 丢 30% 在卡片上看起来一模一样，而后者才是客户卡顿的原因。
  const carrierRows = CARD_CARRIERS.map((carrier) => {
    const agg = summary?.by_carrier?.[carrier]
    return {
      carrier,
      label: carrierName(carrier),
      avgMs: agg?.avg_ms ?? null,
      // 分母也要给：3 个靶点只测到 1 个时，"平均 50ms"的说服力完全不同。
      // 这个信息放在 title 里，hover 可见，不占卡片高度。
      coverage: agg && agg.total_targets > 0
        ? `${agg.measured_targets}/${agg.total_targets} 靶点`
        : '',
      loss: agg?.avg_loss_pct,
    }
  })

  // 「未测得」的原因。
  //
  // ⚠️ 这一条是截图自检才发现的：改成固定四行后，**所有**没数据的节点
  // 都会显示四行「未测得」，看上去像"这台机器三网全线不通"。
  // 实际原因往往完全不是线路问题。两者对用户的含义天差地别：
  // 一个是线路差（换节点），一个是没接好（去配置）。
  //
  // 所以这里把"为什么没数据"直接讲清楚，而不是丢一句"未测得"让用户猜。
  // 判据用后端已经返回的字段，不新增接口：
  //   - 主机层错误存在 → 直接用那句话（可能是 SSH 凭据失效，也可能是别的）
  //   - 压根没测过 → 没有 speedtest 数据，且两条通道都用不了
  //   - 探针跑了但这组全丢包 → 有 summary、只是该组 avg_ms 为空
  //
  // ⚠️ 2026-10-02 改：原来这里写死「该主机还没绑定 SSH 凭据」。
  // 三网测速现在有两条通道（节点 portal API / 面板 SSH 探针），
  // 任一条可用就能采到 —— 把原因钉死在 SSH 上会误导用户去配一把
  // 其实用不上的钥匙。改成说清"两条都不通"，并给出各自的下一步。
  // ⚠️ 判据只用**后端确实会写入**的字段。
  // 第一版这里写了 `node.node_status_error === 'no_api'` —— 那是**猜的**，
  // 实际全库 grep 只在 collector 的一条注释里出现过 'no_api' 这个词，
  // 从来不是写入值。这种"看起来很像真字段"的臆造判据最危险：
  // 它永远不命中，页面永远走到 fallback 分支，而没人会发现问题。
  // 现改为用 status_source（'api' / 'ssh' / ''，明确由采集器写入）：
  // 空值就代表这台节点从未被任何通道探到过。
  const noDataReason = !summary
    ? (node.server_status_error
        ? `未测速：${node.server_status_error}`
        : node.status_source === ''
          ? '尚无测速数据，节点也未完成首次探测；请先检查节点接入状态'
          : '尚无测速数据；可尝试测速，并在详情中检查采集状态')
    : carrierRows.every((row) => row.avgMs === null)
      ? '本轮测速已执行，但所有运营商都没测到值'
      : ''

  // 状态原因卡片头部小尾巴 —— 把「为什么是这个状态」讲清楚。
  //
  // 之前的状态点只是个绿/灰圆，用户看不出：
  //   - 主机 SSH 凭据失效（凭据失效 → online + 主机层 status_error）
  //   - 节点门户 API 鉴权失败（→ online + 节点层 status_error='unauthorized'）
  //   - 节点机器不通（→ offline + 节点层 status_error='unreachable'）
  //
  // 三类问题的处理动作完全不同（换密钥 / 改密钥 / 查机器），
  // 必须让用户**一眼看到**是哪一类。
  const probeReason = nodeStatusReason(node)

  return (
    <div className={`node-card${unstable ? ' is-warning' : node.node_status === 'offline' ? ' is-offline' : ''}`} onClick={() => onOpen(node)} role="button" tabIndex={0}
      onKeyDown={(e) => { if (e.target === e.currentTarget && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); onOpen(node) } }}>
      <div className="node-card-head">
        {/* 状态点三态：在线（绿）/ 不稳定（琥珀）/ 离线（灰）。
            原来只有 online/offline 两种，warning 会被渲染成绿点 ——
            那等于把"时通时断"显示成"一切正常"，是最坏的一种误报。 */}
        <div className="node-card-title">
          <strong>{node.server_name || node.node_name}</strong>
          <small>{node.region || node.provider || node.host}</small>
        </div>
        <button type="button" className="node-card-speed" title={readOnly ? '只读账号不能发起测速' : '只测这一个节点的三网延迟'} aria-label={`测速 ${node.server_name || node.node_name}`} disabled={readOnly || testing}
          onClick={(e) => { e.stopPropagation(); onSpeedtest(node) }}>
          {testing ? <Loader2 size={13} className="spin" /> : <Zap size={13} />}<span>{testing ? '测速中' : '测速'}</span>
        </button>
        <NodeStatus node={node} />
      </div>

      {/* 状态原因尾巴：仅在有问题时显示，避免给所有卡片加一行「正常」。 */}
      {probeReason && (
        <div className={`node-card-reason ${probeReason.severity}`}>
          {probeReason.label}
        </div>
      )}

      {/* 三网：延迟 + 丢包一行一家。没数据的显示「未测得」而不是整组消失 ——
          这样用户能分清"测了但不通"与"压根没测"。
          末尾必须给出原因：没配 SSH 凭据与线路差，处理动作完全不同。 */}
      <div className="node-card-quality">
        {carrierRows.map((row) => (
          <div className="quality-row" key={row.carrier} title={row.coverage || undefined}>
            <span className="quality-name"><i className={`chip-dot ${toneClass(latencyTone(row.avgMs))}`} />{row.label}</span>
            <span className={`quality-ms ${toneClass(latencyTone(row.avgMs))}`}>
              {row.avgMs === null ? '未测得' : formatMs(row.avgMs)}
            </span>
            <span className={`quality-loss ${lossClass(row.loss)}`}>
              {row.loss === null || row.loss === undefined ? '—' : `丢包 ${formatLoss(row.loss)}`}
            </span>
          </div>
        ))}
        {noDataReason && <span className="carrier-row-sub">{noDataReason}</span>}
      </div>

      {/* 底栏只放有值的东西：API 接入的节点没有 CPU/内存，以前显示两条空进度条和
          「—」，占了半张卡却什么也没说。面板→主机的建连延迟是东京到节点，
          与客户体验无关，挪进详情。 */}
      <div className="node-card-foot">
        {hasMetrics(node) && <span>CPU {Math.round(node.cpu_percent)}% · 内存 {Math.round(node.memory_percent)}%</span>}
        {node.tx_bytes_24h + node.rx_bytes_24h > 0 && <span>24h ↑ {formatBytes(node.tx_bytes_24h)} ↓ {formatBytes(node.rx_bytes_24h)}</span>}
        <span className="foot-users">{node.users_count >= 0 ? `${node.users_count} 用户` : '—'}</span>
      </div>
    </div>
  )
})

// NodeDetailModal 是点开节点卡后的详情弹窗：把三个延迟口径、三网逐靶点结果
// 都摊开，并明确标注各自测的是什么。
//
// ⚠️ 2026-10-02 由「页面下方内联展开」改为**弹窗**（管理员要求）。
// 改之前实测发现的问题比"位置不好"严重得多：
//   - `.drawer` 的 CSS **整个不存在**（只有 .drawer-head），于是 position 是
//     static —— 它排在文档流里，顶部位置 1158px 而视口只有 749px，
//     **点完卡片根本看不到详情，必须往下滚**；
//   - 高度 1726px（视口 749px），内容溢出一屏多；
//   - `.drawer-backdrop` 同样没有样式，高度为 0，遮罩层形同虚设。
// 所以这不是"换个展示形式"，是**原来那条路径基本不可用**。
//
// 改用项目既有的 Modal：position:fixed + max-height:86vh + overflow:auto，
// 居中、Esc 关闭、点遮罩关闭 —— 这些行为不用重写。
function NodeDetailModal({ node, onClose }: { node: MonitoringNode; onClose: () => void }) {
  const summary = node.speedtest
  return (
    <Modal
      title={node.server_name || node.node_name}
      kicker="NODE DETAIL"
      onClose={onClose}
      wide
    >
      <div className="node-detail">
          <section className="detail-section">
            <h3>基本信息</h3>
            <div className="detail-rows">
              <div><span>主机地址</span><strong>{node.host}</strong></div>
              <div><span>地区 / 商家</span><strong>{node.region || '—'} / {node.provider || '—'}</strong></div>
              <div><span>监听端口</span><strong>{node.listen_port}</strong></div>
              <div><span>节点状态</span><strong>{STATUS_LABELS[node.node_status] ?? node.node_status}</strong></div>
              <div>
                <span>状态来源</span>
                <strong>
                  {node.status_source === 'ssh' ? 'SSH 探测（有指标）'
                   : node.status_source === 'api' ? 'API 探测（无 CPU/内存/流量）'
                   : '从未探测'}
                </strong>
              </div>
              {node.node_status_error && (
                <div>
                  <span>节点错误</span>
                  <strong className="notice-error">{node.node_status_error}</strong>
                </div>
              )}
              {node.server_status_error && (
                <div>
                  <span>主机错误</span>
                  <strong className="notice-error">{node.server_status_error}</strong>
                </div>
              )}
              <div><span>用户数</span><strong>{node.users_count >= 0 ? node.users_count : '未上报'}</strong></div>
            </div>
          </section>

          <section className="detail-section">
            <h3>延迟（口径不同，别混看）</h3>
            <div className="detail-rows">
              <div>
                <span>网络延迟<small>面板 → 主机 TCP 建连</small></span>
                <strong className={toneClass(latencyTone(node.net_latency_ms))}>{formatMs(node.net_latency_ms)}</strong>
              </div>
              <div>
                <span>SSH 握手<small>含协议协商与认证</small></span>
                <strong>{formatMs(node.ssh_handshake_ms)}</strong>
              </div>
              <div>
                <span>更新时间</span>
                <strong>{formatTime(node.net_latency_checked_at)}</strong>
              </div>
            </div>
          </section>

          <ChinaQualityTrend nodeId={node.node_id} />

          <section className="detail-section">
            <h3>资源</h3>
            <div className="detail-rows">
              <div><span>CPU</span><strong>{percentText(node, node.cpu_percent)}</strong></div>
              <div><span>内存</span><strong>{percentText(node, node.memory_percent)}</strong></div>
              <div><span>磁盘</span><strong>{percentText(node, node.disk_percent)}</strong></div>
              <div><span>负载</span><strong>{node.load_average || '—'}</strong></div>
              <div><span>24h 出站</span><strong>{formatBytes(node.tx_bytes_24h)}</strong></div>
              <div><span>24h 入站</span><strong>{formatBytes(node.rx_bytes_24h)}</strong></div>
            </div>
          </section>

          <section className="detail-section">
            <h3>大陆三网测速</h3>
            {!summary ? (
              <p className="form-note">
                还没有测速数据。测速在<strong>节点本机</strong>执行，两条通道任选其一即可：
                接入该节点的 API（推荐，不需要 SSH），或为它所属主机配置 SSH 凭据。
                然后点右上角「立即测速」。
              </p>
            ) : (
              <>
                <p className="form-note" style={{ marginBottom: 10 }}>
                  测量时间 {formatTime(summary.measured_at)}
                  {!summary.probe_ok && (
                    <span className="metric-warn"> · 探针未成功：{summary.probe_error}</span>
                  )}
                </p>
                {/* 按运营商分组展示，与卡片上的四行顺序一致 ——
                    卡片给平均、这里给逐靶点明细，两处对得上才不用来回猜。
                    不分组的话 18 条靶点平铺，用户得自己在脑子里归类。 */}
                {CARRIER_ORDER.map((carrier) => {
                  const list = (summary.targets ?? []).filter((t) => t.carrier === carrier)
                  if (list.length === 0) return null
                  const agg = summary.by_carrier?.[carrier]
                  return (
                    <div key={carrier} style={{ marginBottom: 12 }}>
                      <div className="sub-link-row" style={{ marginBottom: 6 }}>
                        <strong style={{ fontSize: 'var(--fs-sm)' }}>{carrierName(carrier)}</strong>
                        {agg && (
                          <span className="status-pill">
                            平均{' '}
                            <span className={toneClass(latencyTone(agg.avg_ms))}>
                              {agg.avg_ms === null ? '未测得' : formatMs(agg.avg_ms)}
                            </span>
                            {' · '}
                            {agg.measured_targets}/{agg.total_targets} 靶点
                          </span>
                        )}
                      </div>
                      <div className="detail-rows">
                        {list.map((target) => (
                          <div key={target.key}>
                            <span>
                              {target.name}
                              <small>{target.host}</small>
                            </span>
                            <strong className={toneClass(latencyTone(target.icmp_avg_ms))}>
                              {formatMs(target.icmp_avg_ms)}
                              {target.icmp_loss_pct !== null && target.icmp_loss_pct > 0 && (
                                <small className="loss-tag">丢包 {formatLoss(target.icmp_loss_pct)}</small>
                              )}
                            </strong>
                          </div>
                        ))}
                      </div>
                    </div>
                  )
                })}
                <p className="form-note form-note-hint" style={{ marginTop: 10 }}>
                  <Activity size={13} />
                  <span>{SPEEDTEST_CAVEAT}</span>
                </p>
              </>
            )}
          </section>
      </div>
    </Modal>
  )
}
