import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
// Plus 与 ServerIcon 已随「登记节点」入口和「所属主机」列一起移除（2026-10-02）。
import { Activity, AlertTriangle, Download, Layers, Gauge, Link2, Network, Pencil, RefreshCw, SlidersHorizontal, Tag, Trash2, Unplug } from 'lucide-react'
import { api, loadSession, type Hy2Node, type NodeCapabilities, type NodeCategory, type PortalPreview, type Server, isAdminSession } from '../lib/api'
import { formatBytes } from '../lib/format'
import { Modal } from './Modal'
import { askConfirm } from './ConfirmHost'
import { CategoryManager } from './CategoryManager'
import { NodeControlPanel } from './NodeControlPanel'
import { IPPurityBadge } from './IPPurityBadge'
import { SubscriptionPanel } from './SubscriptionPanel'
import { PortalBatchUpdate } from './PortalBatchUpdate'
import { BatchNodeOps } from './BatchNodeOps'

const nodeStatusText: Record<Hy2Node['status'], string> = { online: '在线', warning: '不稳定', offline: '离线', unknown: '未知' }
const nodeStatusClass: Record<Hy2Node['status'], string> = { online: 'online', warning: 'warning', offline: 'offline', unknown: 'offline' }

// 失效节点排在最前，方便一眼看到要排查的；同类之间保持服务端顺序（sort 是稳定的）。
const isNodeFailed = (node: Hy2Node) => node.health_ok === false
const byHealth = (a: Hy2Node, b: Hy2Node) => Number(isNodeFailed(b)) - Number(isNodeFailed(a))

// 操作能力缺口的原因文案。
//
// 为什么需要它：没绑 api_key 的节点上，用户管理/代理服务/刷新读取全不可用。
// 灰键只让用户知道"点不动"，却会让人以为"这功能本来就没有"——
// 实测 2026-10-02 就是这样，洛杉矶那台缺凭据，探测键一直灰着没人知道原因。
//
// 按项目级铁律「没有这一栏，状态码只是数字」的同一思路：**能力也要有原因尾巴**。
// 这里只在有缺口时显示一行，正常节点保持安静 —— 给每个节点都加提示是噪音。
//
// ⚠️ 2026-10-02 起这里**不再提主机 / SSH 凭据**。
// 原来有第二条「主机未配置 SSH 凭据：无法下发端口探测（去 VPS 主机页为它保存凭据）」，
// 它把「VPS 主机」变成了节点的必要前置，与「节点是节点、主机是主机」的产品定位
// 直接冲突；而实测它也早已不是硬前提 —— portal 侧的 /api/v1/node/meta 与
// /api/v1/speedtest 都能在没有主机 SSH 的情况下给出真实状态与三网延迟
// （洛杉矶-NTT 就是这么测出 18/18 的）。
// 端口探测那条 SSH 路径是**可选增强**，不是节点可用的条件；它没配好时
// 由「节点监控」页的 status_source 说明，不该在这里当成缺口喊。
function actionGaps(node: Hy2Node): { tone: 'warn'; text: string }[] {
  const gaps: { tone: 'warn'; text: string }[] = []
  if (!node.has_api) {
    gaps.push({ tone: 'warn', text: '未接入节点 API：用户管理、代理服务与状态读取都不可用（用「接入节点」绑定一次即可全部可用）' })
  }
  return gaps
}

type Draft = { server_id: string; name: string; listen_port: string; version: string; domain: string; in_store: boolean }
const emptyDraft: Draft = { server_id: '', name: '', listen_port: '443', version: '', domain: '', in_store: true }

// 本月流量单元格：已用 / 限额 + 进度条。没有整机计量数据时说明原因，不显示假数字。
function TrafficCycleCell({ node }: { node: Hy2Node }) {
  if (!node.traffic_sampled_at) {
    // 2026-11-06：没绑节点 API 的节点永远不会有流量数据，原来一直显示「采集中…」—— 看起来像在等，
    // 实际是不会来。整机流量计量只走节点 API，这类节点需要先接入 API 才有这一列。
    if (!node.has_api) {
      return <span className="traffic-sub" title="整机流量计量走节点 API；这台节点还没接入，所以没有流量数据">无 API 无法计量</span>
    }
    return <span className="traffic-sub" title={node.traffic_error || '等待第一次采集'}>{node.traffic_error ? '未计量' : '采集中…'}</span>
  }
  const used = node.traffic_cycle_bytes ?? 0
  const limit = node.traffic_limit_bytes ?? 0
  const pct = limit > 0 ? Math.min(100, Math.round((used / limit) * 100)) : 0
  const tone = node.traffic_suspended || pct >= 100 ? 'over' : pct >= 80 ? 'near' : ''
  return (
    <div className={`node-traffic ${tone}`} title={node.traffic_error || `每月 ${node.traffic_reset_day ?? 1} 号重置（北京时间），上传+下载合计`}>
      <span className="traffic-value">{formatBytes(used)}{limit > 0 ? ` / ${formatBytes(limit)}` : ''}</span>
      {limit > 0 && <span className="node-traffic-bar"><i style={{ width: `${pct}%` }} /></span>}
      <span className="traffic-sub">{node.traffic_suspended ? '已超额停用' : limit > 0 ? `${pct}% · ${node.traffic_reset_day ?? 1} 号重置` : `不限额 · ${node.traffic_reset_day ?? 1} 号重置`}</span>
    </div>
  )
}

export function NodesPage({ nodes, servers, onChanged, notify }: {
  nodes: Hy2Node[]
  servers: Server[]
  onChanged: () => Promise<void> | void
  notify: (message: string) => void
}) {
  const [editing, setEditing] = useState<Hy2Node | null>(null)
  const [limiting, setLimiting] = useState<Hy2Node | null>(null)
  const [limitGB, setLimitGB] = useState('')
  const [resetDay, setResetDay] = useState('1')
  const [savingLimit, setSavingLimit] = useState(false)

  function openLimit(node: Hy2Node) {
    setLimiting(node)
    setLimitGB(node.traffic_limit_bytes ? String(Math.round((node.traffic_limit_bytes / 1024 ** 3) * 100) / 100) : '')
    setResetDay(String(node.traffic_reset_day ?? 1))
  }

  async function saveLimit() {
    if (!limiting) return
    const gb = limitGB.trim() === '' ? null : Number(limitGB)
    const day = Number(resetDay)
    if (gb !== null && (!Number.isFinite(gb) || gb < 0)) { notify('限额需为不小于 0 的数字（GB），留空表示不限额'); return }
    if (!Number.isInteger(day) || day < 1 || day > 31) { notify('重置日需为 1-31'); return }
    setSavingLimit(true)
    try {
      await api.setNodeTrafficLimit(limiting.id, gb && gb > 0 ? gb : null, day)
      notify(gb && gb > 0 ? `已设置 ${limiting.name} 每月限额 ${gb} GB，${day} 号重置` : `已取消 ${limiting.name} 的流量限额`)
      setLimiting(null)
      await onChanged()
    } catch (err) {
      notify(err instanceof Error ? `设置失败：${err.message}` : '设置失败')
    } finally {
      setSavingLimit(false)
    }
  }
  const [importing, setImporting] = useState(false)
  const readOnly = loadSession()?.role === "viewer"
  const [controlling, setControlling] = useState<Hy2Node | null>(null)
  const [batchUpdating, setBatchUpdating] = useState(false)
  const [batchOps, setBatchOps] = useState(false)
  const [unbinding, setUnbinding] = useState('')
  const [subscribing, setSubscribing] = useState(false)
  const [busy, setBusy] = useState(false)
  const [probing, setProbing] = useState('')
  const [refreshing, setRefreshing] = useState('')
  // draft 现在只服务「编辑」（改名称/域名/端口/商城开关）。
  // 初始 server_id 留空 —— 用户不再需要选主机；编辑时会被节点自身的
  // server_id 覆盖，界面上也不暴露这个字段。
  const [draft, setDraft] = useState<Draft>(emptyDraft)
  // 编辑弹窗里勾选的分类 id。与 draft 分开是因为它不参与 updateNode，
  // 而走独立的 setNodeCategories（覆盖式设置）。
  const [draftCategoryIDs, setDraftCategoryIDs] = useState<string[]>([])
  // 全部分类（含停用的）。停用的也要列出来 —— 否则编辑一个挂了
  // 已停用分类的节点时，那次保存会把这个分类静默丢掉。
  const [categories, setCategories] = useState<NodeCategory[]>([])
  // 2026-11-06：分类读取失败单独标记。原来失败时清成空数组，编辑弹窗写「还没有分类，先去建几个」——
  // 运维会去重复建分类；节点已有的分类也看不到、改不了。
  const [categoriesError, setCategoriesError] = useState('')
  const [managingTags, setManagingTags] = useState(false)
  // 节点能力总览（协议/扩展/门户版本）。
  //
  // 为什么单独一次请求而不是并进 /api/nodes：
  //   能力要从每台节点实时拉，慢；而 /api/nodes 是高频列表接口。
  //   混在一起会让"刷新节点列表"也被节点超时拖住。
  // 读失败**不报错** —— 能力是增强信息，不该让节点页打不开。
  const [caps, setCaps] = useState<Map<string, NodeCapabilities>>(new Map())
  // 能力聚合接口**整体**失败时的原因。
  // ⚠️ 2026-10-30：原来 catch 里只 setCaps(new Map())，页面表现为
  // 「所有节点都没有任何协议标签」—— 和「这些节点确实什么都没装」
  // 长得一模一样。运维会据此判断节点能力缺失，而真实原因只是这一次读取失败。
  // 现在把原因留在页面上，并提供重试。
  const [capsError, setCapsError] = useState('')
  const [deleting, setDeleting] = useState(false)
  const mountedRef = useRef(true)
  useEffect(() => {
    mountedRef.current = true
    return () => { mountedRef.current = false }
  }, [])
  const serverById = useMemo(() => new Map(servers.map((server) => [server.id, server])), [servers])
  const sortedNodes = useMemo(() => [...nodes].sort(byHealth), [nodes])
  const activeControlling = useMemo(
    () => (controlling ? (nodes.find((n) => n.id === controlling.id) ?? controlling) : null),
    [controlling, nodes],
  )

  const loadCapabilities = useCallback(async () => {
    try {
      const res = await api.nodeCapabilities()
      if (!mountedRef.current) return
      setCaps(new Map((res.nodes ?? []).map((c) => [c.node_id, c])))
      setCapsError('')
    } catch (err) {
      if (!mountedRef.current) return
      // ⚠️ 2026-10-30：失败时不能清空已有能力标签。
      // 上一轮成功拿到的 Map 是"最近一次已知能力"；把它清空会让每个节点
      // 都显示成"无协议/无扩展"，把一次瞬时网络失败谎报成"节点坏了"。
      // 保留旧 Map，只记错误原因；标签仍显示上次成功的值（数据真实陈旧，
      // 但比"没有"诚实），并在错误尾巴里说明本次读取失败。
      setCapsError(err instanceof Error ? err.message : '读取节点能力失败')
    }
  }, [])
  useEffect(() => { void loadCapabilities() }, [loadCapabilities])

  const loadCategories = useCallback(async () => {
    try {
      const res = await api.listCategories()
      if (!mountedRef.current) return
      setCategories(res.categories ?? [])
      setCategoriesError('')
    } catch (err) {
      if (!mountedRef.current) return
      // 读不到分类不该让整个节点页失败 —— 分类是增强信息；但要说清是读失败，不是没有分类。
      setCategoriesError(err instanceof Error ? err.message : '读取失败')
    }
  }, [])
  useEffect(() => { void loadCategories() }, [loadCategories])

  function openEdit(node: Hy2Node) {
    setDraft({ server_id: node.server_id, name: node.name, listen_port: String(node.listen_port), version: node.version, domain: node.domain, in_store: node.in_store !== false })
    setDraftCategoryIDs((node.categories ?? []).map((c) => c.id))
    setEditing(node)
  }

  // 校验草稿。
  //
  // ⚠️ `server_id` 仍然必填，但**它不再由用户选择** ——
  // 2026-10-02 起新增节点只走「接入节点」（portal API），那条路径会自动
  // 按公网 IP 匹配主机（见 bindNodeAPI 的 preview）。这个字段仅保留给
  // 「编辑」使用：数据库层 hysteria_nodes.server_id 仍是 NOT NULL 外键，
  // 真正的解耦（改可空 + 视图）不在本轮范围内。
  //
  // 为什么删掉「登记节点」这个入口：它要求手填名称/端口/域名且必须先选主机，
  // 产出的节点**天然没有 api_key**，于是用户管理、代理服务、状态读取、
  // 三网测速全都不可用 —— 与「添加节点必须所有功能都能用」直接冲突。
  // 实测也印证了这条路径的价值为零：五台节点全部经 API 接入。
  function validated(): { name: string; server_id: string; listen_port: number; version: string; domain: string; in_store: boolean } | null {
    const port = Number(draft.listen_port)
    if (!draft.server_id || !draft.name.trim() || !Number.isInteger(port) || port < 1 || port > 65535) {
      notify('请填写节点名称，端口需为 1-65535 的整数')
      return null
    }
    return { server_id: draft.server_id, name: draft.name.trim(), listen_port: port, version: draft.version.trim(), domain: draft.domain.trim(), in_store: draft.in_store }
  }

  async function submitEdit() {
    if (!editing) return
    const payload = validated()
    if (!payload) return
    setBusy(true)
    try {
      const result = await api.updateNode(editing.id, payload)
      // 把节点移出商城会触发买家账号注销；注销不干净时后端返回 207，
      // 它落在 2xx 里，必须显式判断（否则就是对「账号还能用」弹「已更新」）。
      //
      // 后端的返回是**联合类型**：正常时是节点本体、失败时是
      // {status:'updated_with_revoke_failures', ...}。这里先用 in 收窄，
      // 不能直接读 status —— 节点本体上也有 status（online/offline），
      // 直接比字符串虽然能跑，但语义上会把"节点在线"误当成"更新成功"。
      const revokeFailed = 'status' in result && result.status === 'updated_with_revoke_failures'
      // 2026-11-03：所有警告攒到一起最后一次性显示。toast 只有一条，原来注销失败的警告
      // 会被紧接着的分类结果覆盖 —— 「N 个买家账号还能连」这句话操作者根本看不到。
      const warnings: string[] = []
      if (revokeFailed) {
        const r = result as { failed_count?: number; error?: string }
        // 后端在「读不全订阅账号清单」时只回 error、不回 failed_count：
        // 这时不知道有几个没注销，不能写成「0 个」——那读起来像「没有遗留」（2026-11-03）。
        const what = r.failed_count === undefined ? '无法确认买家账号是否已从节点侧注销' : `${r.failed_count} 个买家账号未能从节点侧注销`
        warnings.push(`${what}（映射已保留，可重新关闭再开启商城开关以重试）：${r.error ?? ''}`)
      }

      // 分类单独保存（后端是覆盖式接口）。
      //
      // ⚠️ 两件事都要做对：
      //  1) **必须 diff 后再决定要不要调** —— 分类没变时不该发请求，
      //     否则每次改个名字都会顺带重写分类。
      //  2) 分类保存失败**不能只说「已更新节点」** —— 那会让操作者
      //     以为分类也改了，而商城圈的还是旧分类（铁律①：不能谎报）。
      const before = (editing.categories ?? []).map((c) => c.id).sort().join(',')
      const after = [...draftCategoryIDs].sort().join(',')
      let catNote = ''
      if (before !== after) {
        try {
          const catRes = await api.setNodeCategories(editing.id, draftCategoryIDs)
          // 207 落在 2xx 里：移出分类后有账号没能从节点注销时必须说出来，
          // 否则那些买家用已导入的配置照样能连这台节点。
          if (catRes.ok === false && catRes.error) {
            warnings.push(`分类已更新，但：${catRes.error}`)
          } else {
            catNote = draftCategoryIDs.length === 0
              ? '，已清空分类'
              : `，分类已更新为 ${draftCategoryIDs.length} 个`
            if (catRes.revoked_count) catNote += `，已回收 ${catRes.revoked_count} 个移出分类的订阅账号`
          }
        } catch (err) {
          warnings.push(
            `**分类没能保存**：${err instanceof Error ? err.message : '未知错误'}。` +
            '商城的商品如果按分类圈定节点，当前仍是旧分类。',
          )
        }
      }

      if (warnings.length > 0) {
        notify(`⚠ 已更新节点 ${payload.name}，但：${warnings.join('；另外，')}`)
      } else {
        notify(`已更新节点：${payload.name}${catNote}，状态将在下次探测后刷新`)
      }
      setEditing(null)
      await onChanged()
      await loadCategories()
    } catch (err) {
      notify(err instanceof Error ? `更新失败：${err.message}` : '更新失败')
    } finally {
      setBusy(false)
    }
  }

  const [pendingDelete, setPendingDelete] = useState<Hy2Node | null>(null)
  const [decommissioned, setDecommissioned] = useState(false)

  async function confirmDelete() {
    if (!pendingDelete) return
    if (!pendingDelete || deleting) return
    const target = pendingDelete
    const retired = decommissioned
    setDeleting(true)
    try {
      const result = await api.deleteNode(target.id, retired)
      setPendingDelete(null)
      setDecommissioned(false)
      // 节点是删掉了，但买家账号可能没能从节点侧注销掉。
      // 后端对此返回 207 —— 落在 2xx 里，必须显式判断，否则会对
      // 「账号还在节点上能用」的结果弹一句「已删除」。
      if (result.status === 'deleted_decommissioned') {
        notify(`已删除节点 ${target.name}；${result.failed_count ?? 0} 个买家账号因机器已停用直接结案，可在「节点回收」的已处理里查看`)
      } else if (result.status === 'deleted_with_revoke_failures') {
        const what = result.failed_count === undefined ? '无法确认买家账号是否已自动注销' : `${result.failed_count} 个买家账号未能自动注销`
        // 后端的 error 已写明下一步（可在「孤儿账号」补删，或只能上机手工删），不再在这里另说一套。
        notify(`⚠ 已删除节点 ${target.name}，但${what}：${result.error ?? ''}`)
      } else {
        notify(`已删除节点：${target.name}`)
      }
      await onChanged()
    } catch (err) {
      notify(err instanceof Error ? `删除失败：${err.message}` : '删除失败')
    } finally {
      setDeleting(false)
    }
  }

  async function probe(node: Hy2Node) {
    setProbing(node.id)
    try {
      await api.createTask({
        server_id: node.server_id,
        node_id: node.id,
        operation_type: 'health_check',
        parameters: { source: 'console', unit: 'hysteria-server' },
      })
      notify(`已下发探测任务：${node.name}，稍后在任务页查看日志`)
      await onChanged()
    } catch (err) {
      notify(err instanceof Error ? `探测失败：${err.message}` : '探测失败')
    } finally {
      setProbing('')
    }
  }

  async function refreshAPI(node: Hy2Node) {
    setRefreshing(node.id)
    try {
      const result = await api.refreshNodeAPI(node.id)
      notify(`${node.name}：portal 返回 ${result.users_count} 个用户，端口 ${result.listen_port}`)
      await onChanged()
    } catch (err) {
      notify(err instanceof Error ? `读取节点 API 失败：${err.message}` : '读取节点 API 失败')
    } finally {
      setRefreshing('')
    }
  }

  // 2026-11-04：解绑前确认、进行中禁用按钮。原来行内图标一点就执行（请求本身固定带 X-Confirm-Dangerous），
  // 误点就清掉 api_url / api_key / 门户凭据，节点管理全部变灰，要重新粘 portal_access.json；
  // 双击还会发第二次请求，在成功提示之后再弹一条「解绑失败」。
  async function unbindAPI(node: Hy2Node) {
    if (unbinding) return
    if (!await askConfirm(`解绑节点「${node.name}」的节点侧 API？\n\n解绑后用户管理、代理服务、状态读取、三网测速都会停用，需要重新粘贴 portal_access.json 才能恢复。`, { confirmText: '解绑', danger: true })) return
    setUnbinding(node.id)
    try {
      await api.unbindNodeAPI(node.id)
      notify(`已解绑节点侧 API：${node.name}`)
      await onChanged()
    } catch (err) {
      notify(err instanceof Error ? `解绑失败：${err.message}` : '解绑失败')
    } finally {
      setUnbinding('')
    }
  }

  // 编辑表单。
  //
  // 「所属主机」下拉框已移除（2026-10-02）：它把 VPS 主机摆成了节点的必填前置，
  // 而实际接入路径是「接入节点」按公网 IP 自动匹配，用户不需要、也不该管这件事。
  // 节点跑在哪台机器仍以一行次要文字展示在列表里，溯源能力没丢。
  const formFields = (label: string) => (
    <div style={{ display: 'grid', gap: 12 }}>
      <label className="form-field"><span>节点名称 *</span><input value={draft.name} onChange={(event) => setDraft({ ...draft, name: event.target.value })} placeholder="hy2-tokyo-01" /></label>
      <div className="form-row">
        <label className="form-field"><span>监听端口 *</span><input value={draft.listen_port} onChange={(event) => setDraft({ ...draft, listen_port: event.target.value })} inputMode="numeric" /></label>
        <label className="form-field"><span>版本</span><input value={draft.version} onChange={(event) => setDraft({ ...draft, version: event.target.value })} placeholder="2.6.1" /></label>
      </div>
      <label className="form-field"><span>域名</span><input value={draft.domain} onChange={(event) => setDraft({ ...draft, domain: event.target.value })} placeholder="hy2.example.com" /></label>
      <label className="checkbox-row" style={{ display: 'flex', alignItems: 'center', gap: 8, marginTop: 4, cursor: 'pointer' }}>
        <input type="checkbox" checked={draft.in_store} onChange={(event) => setDraft({ ...draft, in_store: event.target.checked })} />
        <span style={{ fontWeight: 600 }}>加入商城（推送给已购用户）</span>
      </label>
      <p className="form-note" style={{ marginTop: -4 }}>
        {draft.in_store
          ? '开启后，该节点会自动包含在所有已购买家的订阅中。'
          : '关闭后，买家订阅不会包含该节点，且会即刻在底层注销所有已购买家的专属账号，买家客户端已导入的配置将立刻断网。手工开的自用订阅不受影响，照常可用。'}
      </p>

      {/* 节点分类：既是分组，也决定商城商品能圈到哪些节点。
          一个节点可以属于多个分类 —— AI 专用与跨境专用并不互斥。 */}
      <div className="form-field">
        <span>节点分类（可多选）</span>
        {categoriesError && categories.length === 0 ? (
          <p className="form-error" style={{ marginTop: 4 }}>
            分类读取失败：{categoriesError}。本次保存不会改动这台节点的分类，刷新页面后再改。
          </p>
        ) : categories.length === 0 ? (
          <p className="form-note" style={{ marginTop: 4 }}>
            还没有分类。点右上角「分类管理」先建几个（例如 AI 专用 / 跨境专用）。
          </p>
        ) : (
          <div className="cat-picker" style={{ marginTop: 4 }}>
            {categories.map((c) => (
              <label key={c.id} title={c.description || c.name}>
                <input type="checkbox" checked={draftCategoryIDs.includes(c.id)} disabled={!isAdminSession()}
                       onChange={(e) => setDraftCategoryIDs((prev) =>
                         e.target.checked ? [...prev, c.id] : prev.filter((x) => x !== c.id))} />
                <span>{c.name}</span>
                {!c.enabled && <span className="cat-picker-slug">已停用</span>}
              </label>
            ))}
          </div>
        )}
        <p className="form-note" style={{ marginTop: 4 }}>
          {!isAdminSession() && '仅管理员可修改节点分类。'}商城商品选择某个分类后，买家只会拿到该分类下的节点。
          不勾任何分类的节点<b>仍会</b>出现在「未按分类圈定」的订阅里。
        </p>
      </div>

      <p className="form-note">{label}</p>
    </div>
  )

  return (
    <>
      <div className="page-heading"><div><div className="eyebrow"><span className="eyebrow-line" />PROXY NODES / 02</div><h1>HY2 节点<span className="heading-period">.</span></h1><p className="page-subtitle">Hysteria2 节点清单，状态与用量均由节点侧 API 直接读取</p></div><div className="heading-actions">
        {/* 分类管理放在这里而不是单独一个导航页：分类是节点的属性，
            离开节点页去管理分类会割裂上下文。 */}
        {/* 2026-11-06：分类的增删改都是 requireAdmin；原来运维账号也能打开分类管理，提交时才收到 403。 */}
        <button className="button button-quiet" disabled={!isAdminSession()} title={isAdminSession() ? undefined : '仅管理员可管理分类'} onClick={() => setManagingTags(true)}><Tag size={16} />分类管理{categories.length > 0 ? `（${categories.length}）` : ''}</button>
        {/* 2026-11-03：批量升级门户在节点上以 root 覆盖 portal，后端仅限管理员；按钮禁用并说明原因，不隐藏。 */}
        <button className="button button-quiet" disabled={!isAdminSession()} title={isAdminSession() ? undefined : '仅管理员可批量更新门户'} onClick={() => setBatchUpdating(true)}><RefreshCw size={16} />批量更新门户</button>
        <button className="button button-quiet" onClick={() => setBatchOps(true)}><Layers size={16} />批量运维</button>
        <button className="button button-quiet" onClick={() => setSubscribing(true)}><Link2 size={16} />合并订阅</button>
        {/* 「接入节点」是**唯一**的新增入口（原「登记节点」已移除）。
            它粘 portal-access.json 或填 api_url + api_key 后自动探测并创建，
            产出的节点天然带 api_key —— 用户管理、代理服务、状态读取、
            三网测速全部可用。手工登记做不到这一点，故不再提供。 */}
        <button className="button button-primary" disabled={readOnly || !isAdminSession()} title={readOnly ? "只读账号不能接入节点" : !isAdminSession() ? "仅管理员可接入节点（绑定会保存节点凭据）" : undefined} onClick={() => setImporting(true)}><Download size={16} />接入节点</button>
      </div></div>

      {batchOps && <BatchNodeOps nodes={nodes} notify={notify} onClose={() => setBatchOps(false)} />}
      {batchUpdating && <PortalBatchUpdate nodes={nodes} notify={notify} onClose={() => setBatchUpdating(false)} />}

      <section className="metrics-grid" aria-label="节点指标">
        <MetricCard label="节点总数" value={nodes.length.toString()} unit="个" icon={<Network size={17} />} tone="green" foot={<span>已登记的 HY2 节点</span>} />
        <MetricCard label="在线节点" value={nodes.filter((node) => node.status === 'online').length.toString()} unit="个" icon={<Network size={17} />} tone="green" foot={<span>端口正在监听</span>} />
        <MetricCard label="不稳定" value={nodes.filter((node) => node.status === 'warning').length.toString()} unit="个" icon={<Network size={17} />} tone="amber" foot={<span>探测异常</span>} />
        <MetricCard label="失效节点" value={nodes.filter(isNodeFailed).length.toString()} unit="个" icon={<Network size={17} />} tone="amber" foot={<span>离线、不稳定或长时间未检测成功</span>} />
      </section>

      <section className="panel servers-panel">
        <div className="section-head servers-head"><div><div className="section-kicker">HYSTERIA2 <span>·</span> {nodes.length.toString().padStart(2, '0')} NODES</div><h2>节点列表 <span className="count-chip">{nodes.length.toString().padStart(2, '0')}</span></h2></div></div>
        {nodes.length === 0 ? (
          <div className="empty-state"><Network size={20} /><strong>暂无节点</strong><span>点右上角「接入节点」，把节点上的 portal-access.json 粘进来即可 —— 接入后所有功能立即可用，不需要 SSH 凭据。</span></div>
        ) : (
          <div className="server-table-wrap node-table-wrap"><table className="server-table node-table">
            {/* 列构成原则（2026-10-02）：这张表回答的是「这台节点本身怎么样」，
                所以**只列节点自己的属性**。「所属主机」原是独立一列，那是把
                「VPS 主机」抬成了节点的一等公民 —— 现在降级为节点单元格里
                的一行次要信息（连同 IP），保留溯源能力但不占据主视线。
                端口/用户/版本/用量/探测全部走 portal API，不依赖主机 SSH。 */}
            <thead><tr><th>节点 / 域名</th><th>状态</th><th>端口</th><th>用户</th><th>版本</th><th>24h 用量</th><th>本月流量</th><th>最近探测</th><th></th></tr></thead>
            <tbody>
              {sortedNodes.map((node) => {
                const server = serverById.get(node.server_id)
                const gaps = actionGaps(node)
                return (
                  <tr key={node.id} className={isNodeFailed(node) ? 'node-row-failed' : undefined}>
                    <td><div className="host-cell"><div className={`host-icon ${nodeStatusClass[node.status]}`}><Network size={16} /></div><div><div style={{ display: 'flex', alignItems: 'center', gap: 6 }}><strong>{node.name}</strong><span className={`store-badge${node.in_store ? ' in-store' : ''}`}>{node.in_store ? '商城' : '自用'}</span></div><span>{node.domain || '未绑定域名'}{node.has_basic ? ' · 全功能 API' : node.has_api ? ' · 仅 API' : ''}</span>
                      {/* 分类标签：挂在节点名下方。
                          没有分类时给一句淡提示 —— 空着会让人以为是加载失败，
                          而这里"没有分类"本身是有意义的信息（商城按分类圈定时它不会被选中）。 */}
                      <div className="node-cat-list">
                        {(node.categories ?? []).length === 0 ? (
                          <span className="node-cat-none">未分类</span>
                        ) : (
                          (node.categories ?? []).map((c) => (
                            <span key={c.id} className={`node-cat${c.enabled ? '' : ' is-off'}`}
                                  title={c.enabled ? c.name : `${c.name}（该分类已停用）`}>
                              {c.name}
                            </span>
                          ))
                        )}
                      </div>
                      {/* 能力标签：这台节点实际支持哪些协议 / 扩展。
                          数据来自节点侧 /api/v1/capabilities（Bearer 通道）。
                          ⚠️ 三种「没有」要区分开（见 NodeCapabilities 注释）：
                            门户过旧 → 提示升级；取不到 → 提示查节点；就是没装 → 不显示。
                          这里只显示**实际可用的**，避免把"没装"渲染成一片灰标签。 */}
                      {(() => {
                        const cap = caps.get(node.id)
                        if (!cap) {
                          // 整体失败与"这一台没有能力"必须分开显示。
                          return capsError ? (
                            <div className="node-caps">
                              <span className="node-cap is-bad" title={capsError}>
                                能力读取失败
                                <button type="button" className="text-button" style={{ marginLeft: 6 }} onClick={() => void loadCapabilities()}>重试</button>
                              </span>
                            </div>
                          ) : null
                        }
                        if (cap.available === false) {
                          return (
                            <div className="node-caps">
                              <span className="node-cap is-bad" title={cap.error || '无法解密节点 API 密钥'}>
                                面板凭据不可用
                              </span>
                            </div>
                          )
                        }
                        if (!cap.reachable) {
                          return (
                            <div className="node-caps">
                              <span className="node-cap is-bad"
                                    title={cap.error || '取不到节点能力'}>
                                {cap.supported ? '能力读取失败' : '门户待升级'}
                              </span>
                            </div>
                          )
                        }
                        const tags: string[] = []
                        const p = cap.protocols ?? {}
                        const protocolNames: Record<string, string> = { hysteria2: 'Hy2', vless_reality: 'Reality', vmess: 'VMess · WS · TLS', vlessenc: 'VLESS Encryption', anytls: 'AnyTLS' }
                        for (const [key, protocol] of Object.entries(p)) {
                          // 动态 map：值可能不是对象（老门户/未来协议），先做 null 安全。
                          if (!protocol || typeof protocol !== 'object') continue
                          if (!protocol.active) continue
                          // VLESS Encryption 已启用但拿不到公开客户端参数 ⇒ 不可交付。
                          // 不能显示成「已启用」，否则操作者会以为买家能用。
                          if (key === 'vlessenc' && protocol.delivery_error) continue
                          tags.push(protocolNames[key] ?? `${key}（暂不支持下发）`)
                        }
                        const e = cap.extras ?? {}
                        const on = (k: string, field = 'active') => Boolean((e[k] ?? {})[field])
                        if (on('gost')) tags.push('GOST')
                        if (on('amneziawg')) tags.push('AWG')
                        if (on('warp', 'enabled')) tags.push('WARP')
                        if (on('bbr', 'enabled')) tags.push('BBR')
                        return (
                          <div className="node-caps">
                            {Object.entries(p).filter(([, protocol]) => protocol && typeof protocol === 'object' && protocol.state_error).map(([key, protocol]) => (
                              <span key={`error-${key}`} className="node-cap is-bad" title={protocol.state_error}>
                                {protocolNames[key] ?? key} 状态未确认：{protocol.state_error}
                              </span>
                            ))}
                            {/* 已启用但拿不到公开客户端密钥：这不是「可用」，必须单独说清。 */}
                            {Object.entries(p).filter(([, protocol]) => protocol && typeof protocol === 'object' && protocol.delivery_error).map(([key, protocol]) => (
                              <span key={`delivery-${key}`} className="node-cap is-warn" title={protocol.delivery_error}>
                                {protocolNames[key] ?? key} 不可交付
                              </span>
                            ))}
                            {tags.length === 0
                              ? <span className="node-cap is-off">无可用扩展</span>
                              : tags.map((t) => (
                                  <span key={t} className="node-cap" title={`${t} 已启用`}>{t}</span>
                                ))}
                            {cap.portal_version && (
                              <span className="node-cap is-ver"
                                    title={`节点门户版本 ${cap.portal_version}`}>
                                v{cap.portal_version}
                              </span>
                            )}
                          </div>
                        )
                      })()}
                      {/* 机器信息降级为次要行：仍然能看出这台节点跑在哪，但不抢主视线。 */}
                      {server && <span className="node-host-line">{server.name} · {server.host}</span>}                      {/* 列被容器查询隐藏时，把它们收进这里 —— 信息不丢，只是换个位置。
                          CSS 负责在够宽时 display:none，所以窄屏才看得到。 */}
                      <div className="node-purity-line"><IPPurityBadge nodeId={node.id} nodeName={node.name} readOnly={readOnly} notify={notify} /></div>
                      <div className="node-extra"><span>用户 <b>{node.users_count >= 0 ? node.users_count : '—'}</b></span><span>版本 <b>{node.version || '—'}</b></span><span>24h <b>{node.used_bytes_24h ? formatBytes(node.used_bytes_24h) : '—'}</b></span><span>本月 <b>{node.traffic_sampled_at ? formatBytes(node.traffic_cycle_bytes ?? 0) + (node.traffic_limit_bytes ? ` / ${formatBytes(node.traffic_limit_bytes)}` : '') : '—'}</b></span></div>
                      {gaps.length > 0 && <div className="action-gaps">{gaps.map((gap) => <span key={gap.text} className="action-gap" title={gap.text}><AlertTriangle size={11} />{gap.text}</span>)}</div>}</div></div></td>
                    <td>{isNodeFailed(node)
                      ? <div className="node-failed-cell"><span className="status-badge node-failed" title={node.health_reason}><i />失效</span><span className="node-failed-reason" title={node.health_reason}>{node.health_reason}</span></div>
                      : <span className={`status-badge ${nodeStatusClass[node.status]}`}><i />{nodeStatusText[node.status]}</span>}</td>
                    <td><span className="traffic-value">{node.listen_port}</span></td>
                    <td><span className="traffic-value">{node.users_count >= 0 ? node.users_count : '—'}</span></td>
                    <td><span className="traffic-sub">{node.version || '—'}</span></td>
                    <td><span className="traffic-value">{node.used_bytes_24h ? formatBytes(node.used_bytes_24h) : '—'}</span></td>
                    <td><TrafficCycleCell node={node} /></td>
                    <td><span className="traffic-sub">{node.last_checked_at ? new Date(node.last_checked_at).toLocaleTimeString('zh-CN', { hour12: false }) : '未探测'}</span></td>
                    <td><div className="row-actions">
                      <button className="row-more" aria-label={`管理 ${node.name} 的节点侧资源`} title={node.has_api ? '节点控制：用户、代理服务与状态' : '未接入节点 API，无法管理 —— 请先接入'} disabled={!node.has_api} onClick={() => setControlling(node)}><SlidersHorizontal size={15} /></button>
                      <button className="row-more" aria-label={`读取 ${node.name} 的节点 API`} title={readOnly ? '只读账号不能刷新节点 API（这个读取会写回节点信息）' : `重新读取 portal（上次 ${(node.api_checked_at && new Date(node.api_checked_at).toLocaleTimeString('zh-CN', { hour12: false })) || '尚未'}）`} disabled={readOnly || !node.has_api || refreshing === node.id} onClick={() => refreshAPI(node)}><RefreshCw size={15} /></button>
                      <button className="row-more" aria-label={`探测 ${node.name}`} title={readOnly ? '只读账号不能下发探测任务' : !serverById.get(node.server_id)?.has_credential ? '探测经 SSH 在主机上执行健康检查：该主机没有 SSH 凭据（节点状态由采集器经节点 API 自动更新）' : '经 SSH 在主机上执行健康检查（结果在运维任务页）'} disabled={readOnly || probing === node.id || !serverById.get(node.server_id)?.has_credential} onClick={() => probe(node)}><Activity size={15} /></button>
                      <button className="row-more" aria-label={`设置 ${node.name} 的流量限额`} title={isAdminSession() ? '月度流量限额：超额自动停止节点服务' : '仅管理员可设置流量限额'} disabled={readOnly || !isAdminSession() || !node.has_api} onClick={() => openLimit(node)}><Gauge size={15} /></button>
                      <button className="row-more" aria-label={`编辑 ${node.name}`} title="编辑节点" disabled={readOnly} onClick={() => openEdit(node)}><Pencil size={15} /></button>
                      <button className="row-more" aria-label={`解绑 ${node.name} 的节点 API`} title={node.has_api ? '解绑节点侧 API' : '未接入节点 API，无需解绑'} disabled={!node.has_api || !isAdminSession() || unbinding === node.id} onClick={() => unbindAPI(node)}><Unplug size={15} /></button>
                      <button className="row-more" aria-label={`删除 ${node.name}`} title={isAdminSession() ? "删除节点" : "仅管理员可删除节点"} disabled={readOnly || !isAdminSession()} onClick={() => setPendingDelete(node)}><Trash2 size={15} /></button>
                    </div></td>
                  </tr>
                )
              })}
            </tbody>
          </table></div>
        )}
        <div className="table-foot"><span>24h 用量取 hysteria 自带 trafficStats 的真实上/下行合计；累计用量是 portal 记账值，仅在客户端重新认证时前进</span><span>节点能力全部经节点侧 API 取得，不依赖「VPS 主机」页的 SSH 凭据</span></div>
      </section>

      {pendingDelete && (
        <Modal title="删除节点" kicker="CONFIRM DELETE" onClose={() => { if (!deleting) { setPendingDelete(null); setDecommissioned(false) } }}>
          <p className="form-note">即将删除节点 <strong>{pendingDelete.name}</strong>（端口 {pendingDelete.listen_port}）。删除只移除登记，不会改动目标主机上的服务；历史采样与任务记录保留，该节点未处理的告警会一并归档。</p>
          <label className="form-note" style={{ display: 'flex', gap: 8, alignItems: 'flex-start', cursor: 'pointer' }}>
            <input type="checkbox" checked={decommissioned} disabled={deleting} onChange={(event) => setDecommissioned(event.target.checked)} style={{ marginTop: 3 }} />
            <span><strong>这台机器已永久停用</strong>（已关机或退租）。删除时连不上节点而没能注销的买家账号将直接结案，不再进入「节点回收」待补删。机器若还在运行请勿勾选 —— 那些账号仍可连接。</span>
          </label>
          <div className="modal-actions"><button className="button button-quiet" disabled={deleting} onClick={() => { setPendingDelete(null); setDecommissioned(false) }}>取消</button><button className="button button-primary" disabled={deleting} onClick={() => void confirmDelete()}>{deleting ? '删除中…' : '确认删除'}</button></div>
        </Modal>
      )}

      {limiting && (
        <Modal title="流量限额" kicker="TRAFFIC LIMIT" onClose={() => setLimiting(null)}>
          <p className="form-note">节点 <strong>{limiting.name}</strong> 每月的整机流量上限（上传 + 下载合计，按网卡统计，与 VPS 商家计费口径一致）。<strong>超过限额会自动停止节点上的 Hysteria / Reality / GOST / AWG 服务</strong>，该节点上所有用户都将无法连接；到重置日或调高限额后自动恢复。</p>
          <label className="login-field"><span>每月限额（GB，留空 = 不限额）</span><input type="number" min="0" step="1" value={limitGB} onChange={(event) => setLimitGB(event.target.value)} placeholder="例如 200" /></label>
          <label className="login-field" style={{ marginTop: 10 }}><span>每月重置日（1-31，北京时间 0 点；当月没有该日时取月末）</span><input type="number" min="1" max="31" value={resetDay} onChange={(event) => setResetDay(event.target.value)} /></label>
          {limiting.traffic_sampled_at && <p className="form-note" style={{ marginTop: 10 }}>本周期已用 <strong>{formatBytes(limiting.traffic_cycle_bytes ?? 0)}</strong>。</p>}
          {limiting.traffic_error && <p className="form-note" style={{ marginTop: 10, color: 'var(--red)' }}>{limiting.traffic_error}</p>}
          <div className="modal-actions"><button className="button button-quiet" onClick={() => setLimiting(null)}>取消</button><button className="button button-primary" disabled={savingLimit} onClick={() => void saveLimit()}>{savingLimit ? '保存中…' : '保存'}</button></div>
        </Modal>
      )}

      {managingTags && (
        <CategoryManager
          notify={notify}
          onClose={() => setManagingTags(false)}
          onChanged={() => { void loadCategories(); void onChanged() }}
        />
      )}

      {editing && (
        <Modal title={`编辑节点 · ${editing.name}`} kicker="EDIT NODE" onClose={() => setEditing(null)}>
          {formFields(`修改端口后，节点状态会重置为「未知」，等待下一次从节点侧 API 读到的真实结果。当前状态：${nodeStatusText[editing.status]}。`)}
          <div className="modal-actions"><button className="button button-quiet" onClick={() => setEditing(null)}>取消</button><button className="button button-primary" disabled={busy} onClick={submitEdit}>{busy ? '保存中…' : '保存修改'}</button></div>
        </Modal>
      )}

      {importing && <ImportDialog servers={servers} onClose={() => setImporting(false)} notify={notify} onChanged={onChanged} />}

      {activeControlling && (
        <Modal title={`节点控制 · ${activeControlling.name}`} kicker="NODE CONTROL" onClose={() => setControlling(null)} wide>
          {/* Modal 头部已经写了标题，面板再画一次就是同一句话重复两行。 */}
          <NodeControlPanel node={activeControlling} notify={notify} showTitle={false} />
        </Modal>
      )}

      {subscribing && (
        <Modal title="合并订阅" kicker="MERGED SUBSCRIPTION" onClose={() => setSubscribing(false)} wide>
          <SubscriptionPanel notify={notify} />
        </Modal>
      )}
    </>
  )
}

function ImportDialog({ servers, onClose, notify, onChanged }: {
  servers: Server[]
  onClose: () => void
  notify: (message: string) => void
  onChanged: () => Promise<void> | void
}) {
  const [url, setUrl] = useState('')
  const [key, setKey] = useState('')
  const [basicUser, setBasicUser] = useState('')
  const [basicPassword, setBasicPassword] = useState('')
  const [accessJSON, setAccessJSON] = useState('')
  const [preview, setPreview] = useState<PortalPreview | null>(null)
  const [serverID, setServerID] = useState('')
  const [name, setName] = useState('')
  const [inStore, setInStore] = useState(true)
  const [busy, setBusy] = useState(false)

  // Pasting the whole portal-access.json is the recommended path: it carries the
  // url (with the {token} segment), the api_key, and the Basic credential pair,
  // so one paste is all the panel needs. It is parsed here only to decide whether
  // the manual fields are still required — the server does the authoritative
  // parsing and never trusts this.
  function onAccessJSONChange(value: string) {
    setAccessJSON(value)
    setPreview(null)
    const trimmed = value.trim()
    if (!trimmed) return
    try {
      const parsed = JSON.parse(trimmed) as { url?: string; username?: string; password?: string; api_key?: string }
      if (parsed.url) setUrl(parsed.url)
      if (parsed.api_key) setKey(parsed.api_key)
      if (parsed.username) setBasicUser(parsed.username)
      if (parsed.password) setBasicPassword(parsed.password)
    } catch {
      // Mid-paste JSON is routinely invalid; stay quiet until the operator is done.
    }
  }

  async function read() {
    setBusy(true)
    setPreview(null)
    try {
      const result = await api.previewNodeAPI({
        url: url.trim(),
        api_key: key.trim(),
        basic_user: basicUser.trim(),
        basic_password: basicPassword,
        access_json: accessJSON.trim(),
      })
      setPreview(result)
      setServerID(result.server_id || servers.find((server) => server.host === result.public_ip)?.id || '')
      setName(result.suggested_name)
      notify(`已读取节点信息：端口 ${result.listen_port}，${result.users_count} 个用户`)
    } catch (err) {
      notify(err instanceof Error ? `读取失败：${err.message}` : '读取失败')
    } finally {
      setBusy(false)
    }
  }

  async function submit() {
    if (!preview) return
    if (!serverID && !preview.auto_host) {
      notify('这台节点还没匹配到已登记的主机，请手动选一台，或留空让面板自动登记')
      return
    }
    if (!name.trim()) {
      notify('请填写节点名称')
      return
    }
    setBusy(true)
    try {
      // server_id is deliberately omitted when auto_host is set: the server
      // records a host from the node's own public IP, so importing stays a
      // two-field operation instead of a detour through the host form.
      const createdNode = await api.createNode({
        server_id: serverID,
        name: name.trim(),
        listen_port: preview.listen_port,
        version: '',
        domain: preview.domain,
        api_url: preview.api_url,
        api_key: key.trim(),
        // 2026-11-06：门户凭据没验证通过时不上架。买家订阅要经门户网页会话读每个账号的客户端参数，
        // 没有它的节点永远进不了任何买家的配置（后端改上架时同样拒绝）；原来照样建成「商城」节点。
        in_store: inStore && preview.basic_ok,
      })
      // The node row now exists; bind the web-session credential onto it. This is
      // best-effort: the api_key alone already makes the node usable (users, gost
      // proxies, overview), so a Basic failure must not lose the node itself.
      //
      // 2026-11-03：直接用 createNode 返回的 id。原来再拉一遍 api.nodes() 按名字 + 地址去找：
      // 那次请求一失败就落进外层 catch 报「登记失败」，而节点其实已经建好了 ——
      // 操作者重试只会得到「该主机上已存在同端口节点」，门户凭据也没绑上。
      if (createdNode.id && (basicUser.trim() || accessJSON.trim())) {
        try {
          await api.bindNodeAPI(createdNode.id, {
            url: url.trim(),
            api_key: key.trim(),
            basic_user: basicUser.trim(),
            basic_password: basicPassword,
            access_json: accessJSON.trim(),
          })
        } catch (err) {
          // 绑定失败时把刚建的节点撤下商城（它进不了任何买家的配置）；撤不下也照样提示。
          if (inStore && preview.basic_ok) {
            try { await api.updateNode(createdNode.id, { in_store: false }) } catch { /* 下面的提示已说明需要处理 */ }
          }
          notify(`节点已登记为自用，但门户凭据绑定失败：${err instanceof Error ? err.message : '未知原因'}。重新绑定后才能启用「节点运维」与上架商城（买家订阅里生成不出没有门户凭据的节点）。`)
          onClose()
          await onChanged()
          return
        }
      }
      // 服务端可能把「上架」降级为「自用」（没有 api_key 的节点开不出买家账号）。
      // 这条提示必须**取代**那句"登记成功"，否则会被后一条覆盖掉，操作者看不到。
      notify(createdNode?.warning ? `已登记：${createdNode.warning}` : `已通过 API 登记节点：${name.trim()}`)
      onClose()
      await onChanged()
    } catch (err) {
      notify(err instanceof Error ? `登记失败：${err.message}` : '登记失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal title="接入节点" kicker="CONNECT NODE" onClose={onClose}>
      <div style={{ display: 'grid', gap: 12 }}>
        <label className="form-field">
          <span>整份 portal-access.json（推荐）</span>
          <textarea
            value={accessJSON}
            onChange={(event) => onAccessJSONChange(event.target.value)}
            rows={4}
            spellCheck={false}
            placeholder='{"url": "https://hy2.example.com:8443/<token>/", "username": "...", "password": "...", "api_key": "..."}'
            style={{ fontFamily: 'monospace', fontSize: 12 }}
          />
        </label>
        <p className="form-note">
          在节点上执行 <code>cat /etc/hysteria/portal-access.json</code>，把整份内容粘进来即可。
          接入后这台节点<strong>所有功能立即可用</strong>：用户管理、代理服务、版本升级、BBR、Reality、
          WARP、AmneziaWG、组件安装、重启，以及状态与三网延迟监测 ——
          <strong>全程不需要 SSH 密钥或密码</strong>。
        </p>

        <details>
          <summary className="form-note" style={{ cursor: 'pointer' }}>或手动填写（只填 url 与 api_key 也可用，但「节点运维」页签不可用）</summary>
          <div style={{ display: 'grid', gap: 12, marginTop: 10 }}>
            <label className="form-field"><span>节点 API 地址 *</span><input value={url} onChange={(event) => { setUrl(event.target.value); setPreview(null) }} placeholder="https://hy2.example.com:8443" spellCheck={false} /></label>
            <label className="form-field"><span>portal api_key *</span><input type="password" value={key} onChange={(event) => { setKey(event.target.value); setPreview(null) }} autoComplete="off" placeholder="portal-access.json 里的 api_key" /></label>
            <label className="form-field"><span>portal username（可选）</span><input value={basicUser} onChange={(event) => { setBasicUser(event.target.value); setPreview(null) }} autoComplete="off" spellCheck={false} /></label>
            <label className="form-field"><span>portal password（可选）</span><input type="password" value={basicPassword} onChange={(event) => { setBasicPassword(event.target.value); setPreview(null) }} autoComplete="off" /></label>
          </div>
        </details>

        <p className="form-note">
          api_key 与这组用户名密码来自同一份 <code>portal-access.json</code>，两者用途不同：
          api_key 走 Bearer，够读节点概要、测三网延迟、管理用户与代理；username+password 走 HTTP Basic，
          才够得着门户网页端那一整片运维功能。三样都加密入库，接口永不回显。
        </p>

        <div><button className="button button-quiet" disabled={busy || !url.trim() || key.trim().length < 16} onClick={read}>{busy ? '读取中…' : '读取节点信息'}</button></div>

        {preview && (
          <div className="import-preview">
            <div className="drawer-info-grid">
              <div><span>监听端口</span><strong>{preview.listen_port}</strong></div>
              <div><span>域名</span><strong>{preview.domain || '—'}</strong></div>
              <div><span>公网 IP</span><strong>{preview.public_ip || '—'}</strong></div>
              <div><span>端口跳段</span><strong>{preview.hop_port_range || '—'}</strong></div>
              <div><span>证书类型</span><strong>{preview.cert_type || '—'}</strong></div>
              <div><span>portal 用户数</span><strong>{preview.users_count}</strong></div>
            </div>
            {/* 归属主机：数据库层 hysteria_nodes.server_id 仍是 NOT NULL 外键，
                所以技术上必须给一个值。但面板会按公网 IP 自动匹配，
                留空即自动登记一台「仅 API」主机 —— 用户不需要为这件事操心，
                节点功能也**不依赖**主机是否配了 SSH。
                文案也据此改写：原来那句「面板仍需要该主机的 SSH 凭据才能采
                CPU/内存/磁盘」把主机说成了硬前提，与实际不符 ——
                实测没配 SSH 的节点照样有状态、有三网延迟、有全部管理功能。 */}
            <label className="form-field"><span>归属主机 {preview.auto_host ? '（可留空，自动登记）' : ''}</span>
              <select value={serverID} onChange={(event) => setServerID(event.target.value)}>
                <option value="">{preview.auto_host ? `留空 → 自动登记为「${preview.public_ip}」` : '未匹配到同 IP 主机，请手动选择…'}</option>
                {servers.map((server) => <option key={server.id} value={server.id}>{server.name} · {server.host}</option>)}
              </select>
            </label>
            <label className="form-field"><span>节点名称 *</span><input value={name} onChange={(event) => setName(event.target.value)} /></label>
            <label className="checkbox-row">
              <input type="checkbox" checked={inStore && preview.basic_ok} disabled={!preview.basic_ok} onChange={(event) => setInStore(event.target.checked)} />
              <span><strong>加入商城（推送给已购用户）</strong></span>
            </label>
            <p className="form-note">
              {!preview.basic_ok
                ? '门户账号密码未验证通过：买家订阅里生成不出这台节点的配置，暂时只能登记为自用；补填门户凭据后再上架。'
                : inStore
                  ? '开启后，该节点会自动包含在所有已购买家的订阅中。'
                  : '关闭后，买家订阅不会包含该节点，仅限管理员自用。'}
            </p>
            {preview.basic_ok ? (
              <p className="form-note">
                <strong>门户凭据已验证</strong> —— 这台节点登记后即可在面板里直接管理全部功能，
                不需要 SSH 密钥或密码。
              </p>
            ) : preview.basic_error ? (
              <p className="form-note service-warn">
                <AlertTriangle size={13} />
                <span>门户凭据校验未通过：{preview.basic_error}　仍可登记节点，但「节点运维」页签会不可用。</span>
              </p>
            ) : (
              <p className="form-note">
                未提供 portal username / password，登记后只能管理用户与代理服务；
                想解锁「节点运维」（版本、BBR、Reality、WARP、组件安装、重启），
                请把 <code>portal-access.json</code> 整份粘到上面的输入框。
              </p>
            )}
            <p className="form-note">{preview.server_id
              ? `已按公网 IP 自动匹配到主机「${preview.server_name}」。`
              : preview.auto_host
                ? `留空即可：面板会把 ${preview.public_ip} 记为「仅 API」主机，节点立刻可用全部功能。主机仅用于记录「跑在哪台机器」，与节点能力无关。`
                : '未能按公网 IP 匹配到已有主机，请手动选择一台，或点「留空」自动登记。'}</p>
          </div>
        )}

        <div className="modal-actions"><button className="button button-quiet" onClick={onClose}>取消</button><button className="button button-primary" disabled={busy || !preview} onClick={submit}>{busy ? '提交中…' : '接入节点'}</button></div>      </div>
    </Modal>
  )
}

function MetricCard({ label, value, unit, icon, tone, foot }: { label: string; value: string; unit: string; icon: ReactNode; tone: string; foot: ReactNode }) {
  return <div className={`metric-card tone-${tone}`}><div className="metric-top"><span>{label}</span><span className="metric-icon">{icon}</span></div><div className="metric-value">{value}<small>{unit}</small></div><div className="metric-foot">{foot}</div></div>
}
