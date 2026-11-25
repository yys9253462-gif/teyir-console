import { useCallback, useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import {
  AlertTriangle, CalendarClock, Check, ChevronLeft, ChevronRight, Copy, ExternalLink,
  KeyRound, Loader2, MonitorSmartphone, MoreHorizontal, Network, Plus, Power, RefreshCw,
  RotateCcw, Search, ShoppingBag, Store, Trash2, Wrench,
} from 'lucide-react'
import { api, loadSession, type Hy2Node, type ManagedSubscription, type NodeCategory, type StoreIntegration, type SubscriptionCenter as CenterData, isAdminSession } from '../lib/api'
import { copyToClipboard } from '../lib/clipboard'
import { useCopiedFlash } from '../lib/copiedFlash'
import { formatBytes } from '../lib/format'
import { safeHttpUrl } from '../lib/safeUrl'
import { Modal } from './Modal'
import { askConfirm } from './ConfirmHost'

// pageNumbers 生成页码按钮序列：首页、末页、当前页 ±2，其余用 'gap' 占位。
//
// 为什么不全列出来：300 条 = 12 页，12 个按钮又是一屏；而且真正会用的场景
// 是「第 3 页」「第 11 页」——中间的 4~8 页很少点到。
function pageNumbers(total: number, current: number): (number | 'gap')[] {
  if (total <= 7) return Array.from({ length: total }, (_, i) => i + 1)
  const out: (number | 'gap')[] = [1]
  const from = Math.max(2, current - 2)
  const to = Math.min(total - 1, current + 2)
  if (from > 2) out.push('gap')
  for (let i = from; i <= to; i += 1) out.push(i)
  if (to < total - 1) out.push('gap')
  out.push(total)
  return out
}

// SubscriptionCenter is the operator's storefront view: every subscription the
// panel has minted for a shop order, plus the shared secret and endpoint
// contract the shop calls.
//
// Design notes:
//  - Expiry and device quota are enforced server-side at *read* time (see
//    enforceSubscriptionAccess), so this page never has to be authoritative.
//    It is a control surface, not a gate: anything the operator does here is
//    re-checked on the customer's next fetch.
//  - Creating a subscription is not the same as selling one. The shop creates
//    orders through POST /api/store/orders; the form here exists so the
//    operator can mint a link by hand (comp accounts, support cases, testing).
//  - The store key is shown once at rotation time. `key_hint` is a masked
//    fingerprint that is safe to display all the time.

const PRESET_DAYS = [7, 30, 90, 180, 365]

function formatDateTime(epochSeconds: number): string {
  if (!epochSeconds || epochSeconds <= 0) return '永久有效'
  return new Date(epochSeconds * 1000).toLocaleString('zh-CN', { hour12: false })
}

// parseLocalDate 把 `<input type="date">` 的 YYYY-MM-DD 解析成本地时区当天 23:59:59。
//
// ⚠️ 不能用 `new Date('2026-11-15')` —— 那按 ECMAScript 规范被当作 **UTC 午夜**，
// 在东八区会变成 11 月 15 日 08:00。对"到期日"这种"用户心里的那一天的末刻"
// 的语义，必须显式按本地时区构造。
//
// 返回 null 表示输入不合法（交给调用方提示，不静默取默认值）。
function parseLocalDate(value: string): Date | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value.trim())
  if (!m) return null
  const year = Number(m[1]); const month = Number(m[2]); const day = Number(m[3])
  if (month < 1 || month > 12 || day < 1 || day > 31) return null
  const d = new Date(year, month - 1, day, 23, 59, 59, 0)
  // 反查一遍，排除"2026-02-30"这类被 Date 静默滚到下个月的日期。
  if (d.getFullYear() !== year || d.getMonth() !== month - 1 || d.getDate() !== day) return null
  return d
}

// formatBytes 把字节数格式化成人读的单位。
//
// 与其它页面的同名函数保持同一套口径（B/KB/MB/GB/TB、1024 进制）——
// 同一份数据在不同页面显示成不同单位会让人怀疑哪个是对的。
function formatDate(iso: string | null): string {
  if (!iso) return '从未'
  return new Date(iso).toLocaleString('zh-CN', { hour12: false })
}

// humanLabel 把「商城订阅 · R5_TEST_ROUND_1_1790932912」压成「商城订阅 · R5_TEST_ROUND_1」。
//
// 为什么要剥掉尾部那串数字：它是订单号的一部分，而订单号紧接着就在副行里
// 完整显示一次。同一串字符连出两遍既挤掉列宽、又让人以为它们是两个不同的东西
// （实测：11 行里每一行都长这样，整列看起来像乱码墙）。
//
// 只剥**看起来很像订单号**的尾部（下划线 + 10 位以上数字结尾），
// 不认识的形态原样返回 —— 猜错标签比不猜更糟。
function humanLabel(label: string): string {
  const raw = label || '未命名订阅'
  const stripped = raw.replace(/[_\-\s]*\d{10,}$/, '').trim()
  return stripped || raw
}

// RowMenu 是行内操作的「更多」菜单。
//
// 目的：把**低频且不可逆**的动作从平铺的图标堆里挪出来。
// 原来一行 7 个图标，删除与换新紧挨着启停 ——
// 这类布局里误点一次就是「客户立即断网」或「数据不可恢复」。
// 现在外面只留日常操作（复制 / 启停 / 编辑），这四项收进菜单，
// 且每项都带一行说明讲清后果。
function RowMenu({ item, busy, readOnlyHint, onCopySubscription, onResetIPs, onReconcile, onRotate, onDelete }: {
  item: ManagedSubscription
  busy: boolean
  readOnlyHint?: string
  onCopySubscription: () => void
  onResetIPs: () => void
  /** 账号对账：把确定性密码重新下发到各节点。幂等、无副作用。 */
  onReconcile: () => void
  onRotate: () => void
  onDelete: () => void
}) {
  const [open, setOpen] = useState(false)
  // 菜单坐标。
  //
  // ⚠️ 为什么不用 CSS 的 absolute + right:0 定位：
  // 表格容器为了不出竖向滚动条设了 `overflow-y:hidden`（见 styles.css），
  // 而 absolute 定位的浮层**会被这个裁剪上下文切掉** —— 实测菜单
  // 高 217px、容器底边只到 507px，下面两项直接看不见。
  // 改 fixed 定位即可脱离裁剪：fixed 相对于视口，不受祖先 overflow 影响。
  //
  // 代价是坐标要自己算（fixed 不跟滚动走），所以展开时读一次按钮位置。
  // 「更多」是低频点击，这点开销无所谓；而**看不见菜单项**是不可接受的。
  const [pos, setPos] = useState<{ top: number; left: number } | null>(null)
  const btnRef = useRef<HTMLButtonElement | null>(null)

  useEffect(() => {
    if (!open) return
    const close = () => setOpen(false)
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault()
        event.stopPropagation()
        setOpen(false)
      }
    }
    window.addEventListener('keydown', onKey)
    window.addEventListener('resize', close)
    window.addEventListener('scroll', close, true)
    return () => {
      window.removeEventListener('keydown', onKey)
      window.removeEventListener('resize', close)
      window.removeEventListener('scroll', close, true)
    }
  }, [open])

  function toggleMenu() {
    if (open) { setOpen(false); return }
    const r = btnRef.current?.getBoundingClientRect()
    if (!r) return
    // 菜单宽 280（与 CSS 一致）。右对齐按钮，但不越出视口左缘。
    const MENU_W = 280
    const left = Math.max(8, Math.min(r.right - MENU_W, window.innerWidth - MENU_W - 8))
    // 优先向下展开；下方空间不足时向上翻（避免菜单下半截在屏幕外）。
    // ⚠️ 菜单高度随条目增减：加了「账号对账」后从 232 调到 290。
    // 这个常量偏小的后果是菜单被翻到上方、首个条目贴住视口顶 ——
    // 不致命但很难看，所以改菜单项时要一起改这里。
    const MENU_H = 290
    const below = window.innerHeight - r.bottom
    const top = below >= MENU_H + 12 ? r.bottom + 6 : Math.max(8, r.top - MENU_H - 6)
    setPos({ top, left })
    setOpen(true)
  }

  return (
    <div className="row-menu">
      <button
        ref={btnRef}
        className="row-more"
        title="更多操作"
        aria-label={`${item.label} 的更多操作`}
        aria-expanded={open}
        onClick={toggleMenu}
      >
        <MoreHorizontal size={15} />
      </button>
      {/* ⚠️ 必须用 createPortal 把菜单挂到 body 上，不能就地渲染。
          原因（实测，2026-10-02）：
            fixed 定位的包含块会被**任何**带 transform / filter /
            will-change / contain / animation 的祖先改变（CSS 规范）。
            而订阅表所在的 `section.panel.servers-panel` 带着
            `animation: rise`（keyframes 里有 transform），
            于是菜单虽然内联写了 left:1254px，实际渲染却偏移到 1557px ——
            正好等于该 section 的 left(302) + 354。
          这个偏移在 DOM 里完全看不出来（内联 style 是对的），
          只有量 getBoundingClientRect 才会发现。
          createPortal 让菜单脱离整条祖先链，从根上消除这类耦合 ——
          比"记得别给祖先加 transform"可靠得多。 */}
      {open && pos && createPortal(
        <>
          {/* 点空白处关闭：用一个覆盖层接住事件，避免菜单点开后无法收起。 */}
          <div className="row-menu-backdrop" onClick={() => setOpen(false)} />
          <div
            className="row-menu-pop"
            role="menu"
            style={{ position: 'fixed', top: pos.top, left: pos.left, right: 'auto' }}
          >
            <button role="menuitem" onClick={() => { setOpen(false); onCopySubscription() }}>
              <Copy size={14} /><span><strong>复制订阅链接</strong><small>给客户端导入用，与交付页地址不同</small></span>
            </button>
            <button
              role="menuitem"
              // ⚠️ 2026-10-30：active_ip_count 为 0 有两种完全不同的含义：
              // ① 真的没有在线设备（这时点一下也无害，结果是空重置）；
              // ② 采集失败、后端读不到（这时点不动是「能力消失」，违反铁律）。
              // 两种都不能把按钮变成无解释的灰。当前采集失败时给一个明确的提示，
              // 仍可点击；真的 0 台时允许点（后端会照常清空并记账）。
              disabled={busy}
              title={readOnlyHint ?? (item.active_ip_count === 0
                ? (item.current_online_ips_complete === false
                  ? '当前未读到在线 IP（采集异常/未知），仍可清空历史记录'
                  : '当前没有记录到在线 IP；清空会重置计数，客户无需重装')
                : '只重置计数，客户无需重装')}
              onClick={() => { setOpen(false); onResetIPs() }}
            >
              <MonitorSmartphone size={14} /><span><strong>清空网络记录</strong><small>只重置计数，客户无需重装</small></span>
            </button>
            <button role="menuitem" disabled={busy} title={readOnlyHint} onClick={() => { setOpen(false); onReconcile() }}>
              <Wrench size={14} /><span><strong>账号对账</strong><small>把客户的密码重新下发到各节点；客户连不上时用</small></span>
            </button>
            <button role="menuitem" disabled={busy || !isAdminSession()} title={readOnlyHint} onClick={() => { setOpen(false); onRotate() }}>
              <RotateCcw size={14} /><span><strong>换新链接</strong><small className="danger-note">旧链接立即失效，客户需重新导入</small></span>
            </button>
            <button role="menuitem" disabled={busy || !isAdminSession()} title={readOnlyHint} onClick={() => { setOpen(false); onDelete() }}>
              <Trash2 size={14} /><span><strong>删除订阅</strong><small className="danger-note">不可恢复，且会注销节点上的账号</small></span>
            </button>
          </div>
        </>,
        document.body,
      )}
    </div>
  )
}

export function SubscriptionCenter({ notify }: { notify: (message: string) => void }) {  const [data, setData] = useState<CenterData | null>(null)
  const [note, setNote] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [copied, flashCopied] = useCopiedFlash()
  // 2026-11-06：订阅的增删改、对账在后端都是 requireAdmin。原来只拦只读账号：
  // 运维账号能打开开通 / 编辑弹窗、点启停与对账，填完才收到 403。这里统一按「非管理员」禁用。
  const readOnly = !isAdminSession()
  const readOnlyHint = loadSession()?.role === 'viewer' ? '只读账号不能修改订阅' : '仅管理员可修改订阅'
  const [creating, setCreating] = useState(false)
  const [editing, setEditing] = useState<ManagedSubscription | null>(null)
  const [pendingDelete, setPendingDelete] = useState<ManagedSubscription | null>(null)
  const [rotating, setRotating] = useState<ManagedSubscription | null>(null)
  const [rotated, setRotated] = useState<{ label: string; url: string } | null>(null)
  const [rotatingKey, setRotatingKey] = useState(false)
  const [freshKey, setFreshKey] = useState('')
  const [busy, setBusy] = useState(false)
  // 分页（2026-10-03）：测试期间开出过 300+ 条订阅，页面一次性渲染全部
  // 表格行 —— 浏览器要建 300 个 DOM 行、滚动卡顿、想找一条记录得拖很久。
  // 这里做**前端分页**：不新增后端接口（数据本来就一次性返回），
  // 切片渲染即可，改动面最小。
  const PAGE_SIZE = 25
  const [page, setPage] = useState(1)
  // 顺带一个搜索框：300 条记录里找一条，没有搜索框基本没法用。
  // 匹配 label / order_id / user_email 三列。
  const [query, setQuery] = useState('')

  const inFlight = useRef(false)
  // 操作后的刷新如果撞上正在进行的 15 秒轮询，直接 return 会让界面停在操作前的数据
  // （停用后仍显示「生效中」，运营再点一次就又启用了）。记下来，等当前请求结束再拉一次。
  const rerun = useRef(false)
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => { mounted.current = false }
  }, [])

  const load = useCallback(async (silent = false): Promise<void> => {
    if (inFlight.current) { rerun.current = true; return }
    inFlight.current = true
    if (!silent) setLoading(true)
    try {
      const next = await api.subscriptionCenter()
      if (!mounted.current) return
      setData(next)
      setNote(null)
    } catch (err) {
      if (!mounted.current) return
      if (!silent) setData(null)
      setNote(err instanceof Error ? `读取订阅中心失败：${err.message}` : '读取订阅中心失败')
    } finally {
      inFlight.current = false
      if (mounted.current && !silent) setLoading(false)
    }
    if (rerun.current && mounted.current) { rerun.current = false; await load(true) }
  }, [])

  useEffect(() => {
    void load()
    const timer = window.setInterval(() => { if (!document.hidden) void load(true) }, 15000)
    const onVisible = () => { if (!document.hidden) void load(true) }
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      window.clearInterval(timer)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [load])

  async function copy(label: string, value: string) {
    if (!value) return
    if (await copyToClipboard(value)) {
      flashCopied(label)
      notify(`已复制${label}`)
    } else {
      notify('复制失败，请手动选中复制')
    }
  }

  async function toggle(item: ManagedSubscription) {
    const turningOn = !item.enabled
    // 2026-11-06：停用先确认：会在所有节点上停掉这个客户的账号，客户端立刻断开。原来行内图标一点即生效。
    if (!turningOn && !await askConfirm(`停用订阅「${item.label || '未命名订阅'}」？

所有节点上这个客户的账号会被停用，已连接的客户端立刻断开；之后可以再启用恢复。`, { confirmText: '停用', danger: true })) return
    setBusy(true)
    try {
      const res = await api.updateSubscriptionRecord(item.id, { enabled: turningOn })
      // ⚠️ 必须看后端返回的联动结果，不能无条件说「已停用」。
      //
      // 「停用」现在会同时把各节点上的账号置为 disabled —— 客户端**立刻断开**。
      // 而节点可能不听话：老节点没有 set_status 端点、节点暂时不可达、限流…
      // 那些节点上的客户端**仍然可用**。
      //
      // 原来这里写死 `已停用：${label}`，等于把后端的警告吞掉了 ——
      // 操作者会以为客户已经断开，实际还有人在用（铁律①：不能谎报）。
      const detail = res?.node_state_message
      const partial = res?.node_state_ok === false
      if (partial && detail) {
        // 失败信息本身就写清了哪些节点、什么原因，原样透出。
        // notify 的签名只收一个字符串，所以用「⚠ 停用不完整」作前缀
        // 让操作者一眼看出这不是成功提示。
        notify(`⚠ ${turningOn ? '启用' : '停用'}不完整 —— ${detail}`)
      } else if (detail) {
        notify(`${turningOn ? '已启用' : '已停用'}：${item.label}（${detail}）`)
      } else {
        notify(`${turningOn ? '已启用' : '已停用'}：${item.label}`)
      }
      await load()
    } catch (err) {
      notify(err instanceof Error ? `操作失败：${err.message}` : '操作失败')
    } finally {
      if (mounted.current) setBusy(false)
    }
  }

  async function resetIPs(item: ManagedSubscription) {
    setBusy(true)
    try {
      await api.updateSubscriptionRecord(item.id, { reset_ips: true })
      notify(`已清空网络记录：${item.label}（下一次请求重新计数）`)
      await load()
    } catch (err) {
      notify(err instanceof Error ? `重置失败：${err.message}` : '重置失败')
    } finally {
      if (mounted.current) setBusy(false)
    }
  }

  // 账号对账（把确定性密码重新下发到各节点）。
  //
  // 什么时候用：客户反馈「导入订阅后连不上」，而节点上的账号还在。
  // 典型成因是账号在早期版本里被节点随机生成了密码，
  // 与库里记的对不上 —— 对账会把它刷成一致。
  //
  // 为什么不做二次确认：这个操作**幂等且无副作用**（密码是确定性推导的，
  // 重复下发不会换密码、不会断线）。给无副作用的操作弹确认框，
  // 只会训练用户无脑点「确定」。
  //
  // ⚠️ 但必须**如实报告失败节点** —— 部分成功时若只说"完成"，
  // 运维会以为全好了，而某个节点上的客户仍然连不上。
  async function reconcile(item: ManagedSubscription) {
    setBusy(true)
    try {
      const res = await api.reconcileSubscription(item.id)
      if (res.failed_nodes > 0) {
        const names = (res.failed ?? []).map((f) => `${f.node}（${f.error}）`).join('；')
        // 2026-11-03：以 ⚠ 开头才会常显；原来 3.2 秒就消失，失败节点名单来不及看。
        notify(`⚠ 对账部分完成：成功 ${res.ok_nodes} 个节点，失败 ${res.failed_nodes} 个 —— ${names}`)
      } else {
        notify(`对账完成：${item.label} 的账号已重新下发到 ${res.ok_nodes} 个节点`)
      }
      await load()
    } catch (err) {
      notify(err instanceof Error ? `对账失败：${err.message}` : '对账失败')
    } finally {
      if (mounted.current) setBusy(false)
    }
  }

  // 全量对账。
  //
  // 与单条对账同一个后端能力，区别是它**会遍历全部订阅**：
  // 同步逐个处理，最坏耗时 = 订阅数 × 节点数 × 10s。
  // 服务端有 50 条上限闸，超了返回 409 并说明原因 ——
  // 这里把那句话原样透传给用户，不要自己另编一句
  // （服务端的说明带着「为什么」和「怎么办」，比前端转述准确）。
  async function reconcileAll() {
    setBusy(true)
    try {
      const res = await api.reconcileAllSubscriptions()
      if (res.failed_nodes > 0) {
        const failedSubs = (res.details ?? []).filter((d) => d.failed_nodes > 0).map((d) => `${d.label}（${d.failed_nodes}）`).join('；')
        notify(`⚠ 全量对账部分完成：${res.subscriptions} 条订阅，成功 ${res.ok_nodes} 个节点，失败 ${res.failed_nodes} 个${failedSubs ? ` —— ${failedSubs}` : ''}`)
      } else {
        notify(`全量对账完成：${res.subscriptions} 条订阅，共 ${res.ok_nodes} 个节点账号已重新下发`)
      }
      await load()
    } catch (err) {
      notify(err instanceof Error ? err.message : '全量对账失败')
    } finally {
      if (mounted.current) setBusy(false)
    }
  }

  async function confirmRotate() {
    if (!rotating) return
    const target = rotating
    setRotating(null)
    setBusy(true)
    try {
      const result = await api.rotateSubscriptionRecord(target.id)
      notify(`已换新链接：${target.label}，旧链接立即失效，请把新链接发给客户`)
      if (mounted.current) {
        setFreshKey('')
        // 网络往返之后浏览器常常不再允许写剪贴板，失败提示会盖掉「已换新」；
        // 所以把新链接放进弹窗，由运营自己点复制。
        setRotated({ label: target.label, url: result.subscription_url })
      }
      // The rotate response intentionally omits `enabled`, so patch nothing and
      // re-read the list instead of guessing at the new state.
      await load()
    } catch (err) {
      notify(err instanceof Error ? `换新失败：${err.message}` : '换新失败')
    } finally {
      if (mounted.current) setBusy(false)
    }
  }

  async function confirmDelete() {
    if (!pendingDelete) return
    const target = pendingDelete
    setPendingDelete(null)
    setBusy(true)
    try {
      await api.deleteSubscriptionRecord(target.id)
      notify(`已删除订阅：${target.label}`)
      await load()
    } catch (err) {
      notify(err instanceof Error ? `删除失败：${err.message}` : '删除失败')
      // 节点回收不完整时后端已把订阅停用，刷新让列表反映真实状态。
      await load()
    } finally {
      if (mounted.current) setBusy(false)
    }
  }

  async function rotateKey() {
    if (!rotatingKey || readOnly) return
    setBusy(true)
    try {
      const result = await api.rotateStoreKey()
      if (mounted.current) {
        setFreshKey(result.key)
        setRotatingKey(false)
      }
      notify('已生成新的对接密钥，旧密钥立即失效，请同步更新商城配置')
      await load()
    } catch (err) {
      notify(err instanceof Error ? `生成失败：${err.message}` : '生成失败')
    } finally {
      if (mounted.current) setBusy(false)
    }
  }

  const subscriptions = data?.subscriptions ?? []
  const store: StoreIntegration | null = data?.store ?? null
  const fleet = data?.fleet ?? { node_count: 0, node_in_store: 0 }
  const active = subscriptions.filter((item) => item.enabled && !item.expired).length
  const expired = subscriptions.filter((item) => item.expired).length
  // 已停用 = 没到期但被手工关掉的。原来它被混进「订阅总数」，用户看不出
  // "11 条里 3 条其实是关着的"。这类"总数里藏着异常"的指标等于没说。
  const disabled = subscriptions.length - active - expired

  // ---- 分页 + 搜索（派生值，不另存 state）----
  // ⚠️ 分页必须作用在**过滤之后**：先按 query 过滤再切片，
  // 否则"搜到 3 条却停在第 5 页"这种空页会让人以为搜错了。
  const q = query.trim().toLowerCase()
  const filtered = q
    ? subscriptions.filter((item) =>
        (item.label || '').toLowerCase().includes(q) ||
        (item.order_id || '').toLowerCase().includes(q) ||
        (item.user_email || '').toLowerCase().includes(q))
    : subscriptions
  const totalPages = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE))
  // 页码夹到合法范围：删记录 / 改搜索词后 current 可能超出上限。
  const currentPage = Math.min(page, totalPages)
  const pageItems = filtered.slice((currentPage - 1) * PAGE_SIZE, currentPage * PAGE_SIZE)

  useEffect(() => {
    // 数据或搜索词变化后，把页码拉回第 1 页。
    // 不这样做的后果：删掉最后一页的最后一条后停在空页上。
    setPage(1)
  }, [query])

  return (
    <>
      <div className="page-heading">
        <div>
          <div className="eyebrow"><span className="eyebrow-line" />STOREFRONT / 03</div>
          <h1>订阅中心<span className="heading-period">.</span></h1>
          <p className="page-subtitle">把在役节点整合成一条订阅，交给商城统一出售</p>
        </div>
        <div className="heading-actions">
          <button className="button button-quiet" onClick={() => void load()} disabled={loading}>
            {loading ? <Loader2 size={15} className="spin" /> : <RefreshCw size={15} />}刷新
          </button>
          <button className="button button-primary" disabled={readOnly} title={readOnly ? readOnlyHint : undefined} onClick={() => { setCreating(true); setFreshKey('') }}><Plus size={16} />手工开订阅</button>
        </div>
      </div>

      {note && <div className="api-error-banner" role="alert"><AlertTriangle size={16} />{note}</div>}

      {/* 搜索 + 分页控件放在表格上方（不是下方）—— 300 条记录时，
          翻到第 12 页想找东西，控件在屏幕外根本看不见；若列表缩回一页内但搜索词仍在，仍保持可见以便清空。 */}
      {(subscriptions.length > PAGE_SIZE || query !== '') && (
        <div className="sub-toolbar">
          <label className="sub-search">
            <Search size={14} />
            <input
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="搜索订阅名 / 订单号 / 邮箱"
              aria-label="搜索订阅" />
          </label>
          <span className="sub-count">
            {q ? <>匹配 <b>{filtered.length}</b> / {subscriptions.length} 条</> : <>共 <b>{subscriptions.length}</b> 条</>}
          </span>
        </div>
      )}

      {/* 指标卡构成原则：
          ① 每张卡的值必须是**后端真能给的数**。原「覆盖节点」卡的值恒为 `—`
             （后端不返回节点数），脚注却承诺"包含全部可用节点" ——
             占着黄金位却零信息。现已补上 fleet 字段，卡片显示真实节点池。
          ② 三态要拆开：生效中 / 已到期 / 已停用。「已停用」原来混在「订阅总数」里，
             用户看不出"11 条里其实有 3 条是关着的"。 */}
      <section className="metrics-grid" aria-label="订阅指标">
        <MetricCard label="生效中" value={active.toString()} unit="条" icon={<Check size={17} />} tone="green" foot={disabled > 0 ? <span className="metric-warn">另有 {disabled} 条已停用</span> : <span>未到期且已启用</span>} />
        <MetricCard label="订阅总数" value={subscriptions.length.toString()} unit="条" icon={<ShoppingBag size={17} />} tone="neutral" foot={<span>含已到期与已停用</span>} />
        <MetricCard label="已到期" value={expired.toString()} unit="条" icon={<CalendarClock size={17} />} tone={expired > 0 ? 'amber' : 'neutral'} foot={<span>开启自动删除的到期后会清理</span>} />
        <MetricCard
          label="节点池"
          value={fleet.error ? '—' : fleet.node_count.toString()}
          unit="个"
          icon={<Network size={17} />}
          tone={fleet.error ? 'amber' : 'neutral'}
          foot={fleet.error ? <span className="metric-warn">{fleet.error}，请稍后刷新（不代表没有节点）</span> : <span>订阅按节点实时生成{fleet.node_in_store < fleet.node_count ? <>，其中 {fleet.node_in_store} 个已上架商城；商品按分类圈定时买家只拿「该分类 ∩ 已上架」</> : '，全部已上架商城；商品按分类圈定时买家只拿「该分类 ∩ 已上架」'}</span>}
        />
      </section>

      <section className="panel servers-panel">
        <div className="section-head servers-head">
          <div>
            <div className="section-kicker">SUBSCRIPTIONS <span>·</span> {subscriptions.length.toString().padStart(2, '0')}</div>
            <h2>已开出的订阅 <span className="count-chip">{subscriptions.length.toString().padStart(2, '0')}</span></h2>
          </div>
          {/* 全量对账：只在订阅数不多时才有意义。
              服务端有 50 条上限闸（超过会 409 并要求分批）——
              这里也按同一阈值禁用按钮，让用户在点之前就知道不能全量跑，
              而不是点完收到一个错误。 */}
          <button
            className="button button-quiet"
            disabled={readOnly || busy || subscriptions.length === 0 || subscriptions.length > 50}
            title={readOnly ? readOnlyHint : subscriptions.length > 50
              ? `订阅数为 ${subscriptions.length}，超过全量对账上限 50 —— 请用行内「账号对账」逐条处理`
              : '把全部订阅的确定性密码重新下发到各节点'}
            onClick={() => void reconcileAll()}
          >
            <Wrench size={14} />全量对账
          </button>
        </div>

        {filtered.length === 0 ? (
          <div className="empty-state">
            <ShoppingBag size={20} />
            {q ? (
              <>
                <strong>没有匹配的订阅</strong>
                <span>搜索「<b>{query}</b>」没有命中任何记录，换个关键词或清空搜索框。</span>
                <button type="button" className="button button-quiet button-compact" onClick={() => setQuery('')}>清空搜索</button>
              </>
            ) : (
              <>
                <strong>还没有任何订阅</strong>
                <span>商城通过 <code>POST /api/store/orders</code> 下单时自动开通；也可以点右上角「手工开订阅」补偿或测试。</span>
              </>
            )}
          </div>
        ) : (
          <div className="server-table-wrap"><table className="server-table sub-table">
            {/* 列构成（2026-10-02 重排）：
                - 删掉「节点」列：它 11 行全是硬编码的「全部可用节点」，零差异 ——
                  而这件事已经由页头的「节点池」卡说清了，不必每行重复。
                - 「拉取」原来的值是「3 次 2026/10/2 19:14:56」，两个数之间只靠一个
                  <i/> 分隔，读起来像一句话。拆成「次数」+「最近」两个对齐的量。 */}
            <thead>
              <tr>
                <th>订阅 / 订单</th><th>状态</th><th>到期</th><th>在线网络</th>
                <th>当前在线 IP</th><th>拉取</th><th>最近拉取</th><th>用量</th><th></th>
              </tr>
            </thead>
            <tbody>
              {/* ⚠️ 用 ?? []兜底：后端若返回 null（Go 的 nil slice 会序列化成null），
                  `.length` 会直接抛异常 —— 而"没有在线 IP"是最常见的正常状态，
                  不能让它变成整页白屏。 */}
              {pageItems.map((item) => {
                const onlineIPs = item.current_online_ips ?? []
                const uniqueIPs = new Set(onlineIPs.map(entry => entry.ip)).size
                return (
                <tr key={item.id}>
                  <td>
                    <div className="host-cell">
                      <div className={`host-icon ${item.expired ? 'offline' : item.enabled ? 'online' : 'warning'}`}><ShoppingBag size={15} /></div>
                      <div>
                        {/* 标题与副行不再重复同一件事。
                            原来 label 形如「商城订阅 · R5_TEST_ROUND_1_1790932912」，
                            副行又写一遍「订单 R5_TEST_ROUND_1_1790932912」——
                            同一串字符连出两遍，既挤掉宽度又没增加信息。
                            现在：长订单号从标题里剥掉（标题只留人类可读部分），
                            完整订单号放副行（可选中复制，运维要用它去商城对账）。 */}
                        <strong title={item.label || '未命名订阅'}>{humanLabel(item.label)}</strong>
                        <span className="sub-order" title={item.order_id || undefined}>
                          {item.order_id || '手工创建'}{item.user_email ? ` · ${item.user_email}` : ''}
                        </span>
                        {/* 分类圈定：这条订阅只含哪个分类的节点。
                            不圈定时也显式写出来 —— 否则运维看到空着会以为
                            "没设置"，而它其实是有意义的默认值（含全部节点）。 */}
                        <span className="sub-cat">
                          {item.node_ids && item.node_ids.length > 0
                            ? <>自选 <b>{item.node_ids.length}</b> 个节点</>
                            : item.category_id
                              ? <>分类 <b>{item.category_name || '未知分类'}</b></>
                              : <>不按分类圈定（全部已上架节点）</>}
                        </span>
                      </div>
                    </div>
                  </td>
                  <td>
                    <span className={`status-badge ${item.expired ? 'offline' : item.enabled ? 'online' : 'warning'}`}>
                      <i />{item.expired ? '已到期' : item.enabled ? '生效中' : '已停用'}
                    </span>
                  </td>
                  <td>
                    <span className="traffic-value">{formatDateTime(item.expires_at)}</span>
                    {!item.expired && item.days_left >= 0 && (
                      <span className="traffic-sub">剩 {item.days_left} 天</span>
                    )}
                  </td>
                  <td>
                    <span className="traffic-value">{item.ip_limit > 0 ? `${item.active_ip_count} / ${item.ip_limit}` : `${item.active_ip_count} / 不限`}</span>
                    {item.ip_limit > 0 && <span className="traffic-sub">{item.active_ip_count >= item.ip_limit ? '已占满' : `还可加 ${item.ip_limit - item.active_ip_count} 个网络`}</span>}
                  </td>
                  <td>
                    <span className="traffic-value">
                      {item.current_online_ips_readable ? `${uniqueIPs} 个已确认在线 IP` : '当前在线 IP 未知'}
                    </span>
                    <span className="traffic-sub" title={(item.current_online_errors ?? []).join('；')}>
                      Hy2覆盖 {item.current_online_hy2_nodes ?? 0}/{item.current_online_expected_nodes ?? 0} · Reality覆盖 {item.current_online_reality_nodes ?? 0}/{item.current_online_expected_nodes ?? 0}
                    </span>
                    {onlineIPs.length > 0 && <div className="sub-ip-list">
                      {onlineIPs.slice(0, 3).map(entry => <span key={`${entry.node_id}-${entry.protocol}-${entry.ip}`} className="sub-ip" title={`${entry.node_name} · ${entry.protocol}`}>
                        {entry.ip} <small>{entry.node_name} · {entry.protocol}</small>
                      </span>)}
                      {onlineIPs.length > 3 && <details><summary>展开其余 {onlineIPs.length - 3} 条连接地址</summary>
                        {onlineIPs.slice(3).map(entry => <div key={`${entry.node_id}-${entry.protocol}-${entry.ip}`}>{entry.ip} · {entry.node_name} · {entry.protocol}</div>)}
                      </details>}
                    </div>}
                    {!item.current_online_ips_complete && <span className="traffic-sub" title={(item.current_online_errors ?? []).join('；')}>
                      覆盖不完整：未采到的节点或协议不能判定无人在线
                    </span>}
                    {item.current_online_ips_complete && onlineIPs.length === 0 && <span className="traffic-sub">当前无在线连接地址</span>}
                    <span className="traffic-sub">Hy2核心：{item.hysteria_online_devices ?? '未知'} 个连接实例 · 每15秒刷新</span>
                    {item.current_online_sampled_at && <span className="traffic-sub">采样：{formatDate(item.current_online_sampled_at)}</span>}
                    {(item.online_ips ?? []).length > 0 && <details><summary>最近鉴权记录（非当前在线）</summary>
                      {(item.online_ips ?? []).map(entry => <div key={`${entry.node_id}-${entry.ip}`}>{entry.ip} · {entry.node_name}</div>)}
                    </details>}
                    {(item.active_ips ?? []).length > 0 && <span className="traffic-sub" title={(item.active_ips ?? []).map(entry => entry.ip).join('、')}>拉取过订阅：{(item.active_ips ?? []).length} 个地址（非当前在线）</span>}
                  </td>
                  <td><span className="traffic-value">{item.access_count}</span></td>
                  <td><span className="traffic-sub">{item.last_accessed_at ? formatDate(item.last_accessed_at) : '从未'}</span></td>
                  {/* 用量只展示真实计量；旧口径估算单列说明、不参与合计。
                      ⚠️ used_bytes 为 null = **从未采到过**，与"确实是 0"不同 ——
                      显示 0 B 会让人以为客户没在用，而实际是没采到。
                      两段必须相加：只显示计量段会让老订阅显示 0 B（历史全丢），
                      只显示旧口径段则是那个更糟的老问题（只在重新认证时前进）。*/}
                  <td>
                    {item.used_bytes === null ? (
                      <span className="traffic-sub" title={item.usage_reason || '真实计量尚未初始化'}>未开始计量{item.usage_legacy_bytes > 0 ? ` · 旧口径估算 ${formatBytes(item.usage_legacy_bytes)}（不计入总量）` : ''}</span>
                    ) : (
                      <>
                        <span className={`traffic-value${item.usage_ok ? '' : ' sub-usage-stale'}`}
                              title={`计量段 ${formatBytes(item.usage_metered_bytes)}${item.usage_legacy_bytes > 0 ? `；另有旧口径估算 ${formatBytes(item.usage_legacy_bytes)}（不计入总量）` : ''}。${item.usage_reason || ''}`}>
                          {formatBytes(item.used_bytes)}
                        </span>
                        <span className="traffic-sub">
                          {`计量覆盖 ${item.usage_measured_nodes ?? 0}/${item.usage_expected_nodes ?? 0} 节点`}
                          {!item.usage_current_ok ? ' · 当前采集异常/未知' : item.usage_history_gaps ? ' · 历史有缺口' : ''}
                        </span>
                        {item.usage_legacy_bytes > 0 ? (
                          <span className="traffic-sub" title="旧鉴权记账值未经真实流量验证，单独保留，不计入真实累计">
                            {`另有旧口径估算（不计入总量） ${formatBytes(item.usage_legacy_bytes)}`}
                          </span>
                        ) : (
                          <span className="traffic-sub" title="各节点从各自计量基线起累计；基线前旧记账值单独保留，不计入真实累计">
                            {item.usage_started_at
                              ? `计量起于 ${new Date(item.usage_started_at).toLocaleString()}`
                              : '统计起点未知'}
                            {item.usage_started_at_latest && item.usage_started_at_latest !== item.usage_started_at && ' · 节点起点不同'}
                          </span>
                        )}
                      </>
                    )}
                  </td>
                  <td>
                    {/* 操作分层：
                        日常高频（复制链接、启停）留在外面；
                        低频且**不可逆**的（换新链接、清空网络记录、删除）收进「更多」菜单。
                        原来 7 个图标平铺，删除和换新紧挨着启停 ——
                        这类布局下误点一次就是客户断网或数据不可恢复。 */}
                    <div className="row-actions">
                      <button className="row-more" title="复制交付页链接（发给客户）" aria-label={`复制 ${item.label} 的交付页链接`} onClick={() => void copy('交付页链接', item.delivery_url)}><ExternalLink size={15} /></button>
                      <button className="row-more" title={item.expired ? '已到期，请先续期（启用不会让到期订阅恢复）' : item.enabled ? '停用' : '启用'} aria-label={`${item.enabled ? '停用' : '启用'} ${item.label}`} disabled={readOnly || busy || item.expired} onClick={() => void toggle(item)}><Power size={15} /></button>
                      <button className="row-more" title={readOnly ? readOnlyHint : '编辑到期与在线网络上限'} aria-label={`编辑 ${item.label}`} disabled={readOnly} onClick={() => setEditing(item)}><CalendarClock size={15} /></button>
                      <RowMenu
                        item={item}
                        busy={readOnly || busy}
                        readOnlyHint={readOnly ? readOnlyHint : undefined}
                        onCopySubscription={() => void copy('订阅链接', item.subscription_url)}
                        onResetIPs={() => void resetIPs(item)}
                        onReconcile={() => void reconcile(item)}
                        onRotate={() => setRotating(item)}
                        onDelete={() => setPendingDelete(item)}
                      />
                    </div>
                  </td>
                </tr>
              )
              })}
            </tbody>
          </table></div>
        )}

        {/* 分页器：只在真的需要分页时出现。
            单页时不占位置 —— 记录少的时候多一排按钮纯属噪音。 */}
        {totalPages > 1 && (
          <div className="pager" role="navigation" aria-label="订阅分页">
            <button
              className="pager-btn"
              disabled={currentPage <= 1}
              onClick={() => setPage(currentPage - 1)}
              aria-label="上一页">
              <ChevronLeft size={15} />
            </button>
            {/* 页码按钮：当前页附近 ±2，其余用省略号。
                300 条 = 12 页，全列出来又是一屏；直接给「第 N 页」输入更快。 */}
            {pageNumbers(totalPages, currentPage).map((n, i) =>
              n === 'gap' ? (
                <span key={`gap-${i}`} className="pager-gap">…</span>
              ) : (
                <button
                  key={n}
                  className={`pager-btn${n === currentPage ? ' is-current' : ''}`}
                  onClick={() => setPage(n as number)}
                  aria-current={n === currentPage ? 'page' : undefined}>
                  {n}
                </button>
              ))}
            <button
              className="pager-btn"
              disabled={currentPage >= totalPages}
              onClick={() => setPage(currentPage + 1)}
              aria-label="下一页">
              <ChevronRight size={15} />
            </button>
            <span className="pager-jump">
              第 <b>{currentPage}</b> / {totalPages} 页
            </span>
          </div>
        )}

        <div className="table-foot">
          <span>到期与在线网络数在客户每次拉取时实时判定，不依赖定时任务；开启自动删除的订阅在到期且超过宽限期后清理</span>
          <span>{store ? `IP 统计窗口 ${store.ip_window_hours}h · 到期宽限 ${store.expiry_grace_hours}h` : ''}</span>
        </div>
      </section>

      {/* 商城对接：**默认折叠**。
          理由（管理员 2026-10-02 反馈"页面太长、主次不分"）：
          这一整块是"配置一次、之后几个月不碰"的东西（密钥 + 4 个接口契约），
          却和"每天都要看的订阅列表"平铺在同一屏里，
          把真正高频的内容挤到需要滚动才能看全。
          折叠而不是移除 —— 能力一点没少，只是不再抢视觉重心。
          `open` 由 store.enabled 决定初始态：没配密钥时展开（需要引导），
          配好了就收起（不需要天天看）。 */}
      {store && (
        <details className="sub-collapse" open={!store.enabled}>
          <summary>
            <span className="sub-collapse-title">
              <Store size={15} />
              商城对接
              <span className={`status-pill ${store.enabled ? 'online' : 'warning'}`}>
                {store.enabled ? '已配置密钥' : '未生成密钥'}
              </span>
            </span>
            <span className="sub-collapse-hint">
              {store.enabled ? '密钥与 4 个接口契约（点击展开）' : '需要生成对接密钥后才能接商城'}
            </span>
          </summary>
          <section className="panel servers-panel" style={{ marginTop: 12 }}>
            <div className="section-head servers-head">
              <div>
                <div className="section-kicker">MALL INTEGRATION <span>·</span> {store.enabled ? 'READY' : 'NOT CONFIGURED'}</div>
                <h2>商城对接 <span className={`status-pill ${store.enabled ? 'online' : 'warning'}`}>{store.enabled ? '已配置密钥' : '未生成密钥'}</span></h2>
              </div>
              <button
                className={`button ${rotatingKey ? 'button-danger' : 'button-quiet'}`}
                disabled={readOnly || busy}
                title={readOnly ? readOnlyHint : undefined}
                onClick={() => (rotatingKey ? void rotateKey() : setRotatingKey(true))}
                onBlur={() => setRotatingKey(false)}
              >
                <KeyRound size={15} />{rotatingKey ? '确认生成？旧密钥立即失效' : store.enabled ? '重新生成密钥' : '生成对接密钥'}
              </button>
            </div>

          <div style={{ padding: '0 16px 16px', display: 'grid', gap: 12 }}>
            <p className="form-note service-warn">
              <Store size={13} />
              <span>
                商城用这把密钥调用下面四个接口；密钥只在此处显示指纹，明文仅生成时出现一次。
                重新生成会让商城<strong>立刻</strong>无法下单，请与商城配置同批次更新。
              </span>
            </p>

            {freshKey && (
              <div className="delivery-banner delivery-banner-warn">
                <KeyRound size={16} />
                <span>
                  <strong>请立即复制，本页刷新后不再显示</strong>
                  <code className="store-key-reveal">{freshKey}</code>
                  请求头格式：<code>{store.auth_header}</code>
                </span>
              </div>
            )}

            <div className="cred-row">
              <div className="cred-label">当前密钥</div>
              <div className="cred-value mono" title={store.key_hint}>{store.key_hint || '尚未生成'}</div>
              <button className="button button-quiet button-compact" disabled={!store.key_hint} onClick={() => void copy('密钥指纹', store.key_hint)}>
                {copied === '密钥指纹' ? '已复制' : '复制'}
              </button>
            </div>
            <div className="cred-row">
              <div className="cred-label">鉴权请求头</div>
              <div className="cred-value mono">{store.auth_header}</div>
              <button className="button button-quiet button-compact" onClick={() => void copy('鉴权格式', store.auth_header)}>{copied === '鉴权格式' ? '已复制' : '复制'}</button>
            </div>

            <div className="overview-grid">
              <div className="overview-card">
                <div className="overview-card-head">开通</div>
                <div className="overview-json">{store.create_endpoint}</div>
                <p className="form-note">传 <code>order_id</code>（幂等键）、<code>duration_days</code> 或 <code>expires_at</code>、<code>ip_limit</code>、<code>user_email</code>、<code>label</code>。同一 <code>order_id</code> 重复调用只返回同一条订阅。</p>
              </div>
              <div className="overview-card">
                <div className="overview-card-head">续费</div>
                <div className="overview-json">{store.renew_endpoint}</div>
                <p className="form-note">传 <code>order_id</code> + <code>duration_days</code>。未到期时从当前到期日往后顺延，已到期则从此刻起算。</p>
              </div>
              <div className="overview-card">
                <div className="overview-card-head">撤销</div>
                <div className="overview-json">{store.revoke_endpoint}</div>
                <p className="form-note">传 <code>order_id</code>，默认停用（客户可续费恢复）；加 <code>delete: true</code> 才是彻底删除。建议退款场景用停用。</p>
              </div>
              <div className="overview-card">
                <div className="overview-card-head">查询</div>
                <div className="overview-json">{store.query_endpoint}</div>
                <p className="form-note">按订单号查当前状态，返回到期时间、剩余天数、网络占用与交付页地址，适合商城做对账或订单详情页。</p>
              </div>
            </div>

            <p className="form-note">
              交付页地址格式为 <code>https://你的面板域名/user/&lt;token&gt;</code>，客户打开即可看到状态、二维码与三种格式的下载；
              也可以在商城订单详情里做一次 302 跳到这个地址，客户体验与你 hysteria2 面板给的那种链接一致。
            </p>
          </div>
        </section>
        </details>
      )}

      {creating && (
        <CreateSubscriptionDialog
          notify={notify}
          onClose={() => setCreating(false)}
          // ⚠️ 这里**不能** setCreating(false)。
          //
          // 此前是 `onCreated={async () => { setCreating(false); await load() }}`
          // —— 提交成功后立刻把对话框卸载，于是 CreateSubscriptionDialog
          // 内部那个 `if (created)` 的**交付弹窗根本没机会显示**：
          // 客户链接拿不到，开户结果（开了几个节点、哪些失败）也看不到。
          //
          // 而"看不到开户结果"正是「手工建订阅不会开户」那个真 BUG
          // 能潜伏那么久的原因之一 —— 见 commit f41a936 / ca8ffbc。
          //
          // 正确行为：**留在交付弹窗上**，让操作者复制完链接、
          // 看清开户结果后，自己点「完成」关闭（那个按钮会调 onClose）。
          onCreated={async () => { await load() }}
        />
      )}

      {editing && (
        <EditSubscriptionDialog item={editing} notify={notify} onClose={() => setEditing(null)} onSaved={async () => { setEditing(null); await load() }} />
      )}

      {rotated && (
        <Modal title="新订阅链接" kicker="ROTATED" onClose={() => setRotated(null)}>
          <p className="form-note"><strong>{rotated.label || '未命名订阅'}</strong> 的旧链接已失效，请把下面的新链接发给客户。</p>
          <code className="store-key-reveal">{rotated.url}</code>
          <div className="modal-actions">
            <button className="button button-quiet" onClick={() => setRotated(null)}>关闭</button>
            <button className="button button-primary" onClick={() => void copy('新订阅链接', rotated.url)}><Copy size={14} />复制新链接</button>
          </div>
        </Modal>
      )}

      {rotating && (
        <Modal title="换新订阅链接" kicker="CONFIRM ROTATE" onClose={() => setRotating(null)}>
          <p className="form-note">
            即将为 <strong>{rotating.label || '未命名订阅'}</strong> 生成全新的链接。旧链接<strong>立即无法重新拉取</strong>，
            已导入的客户需要重新导入新链接。网络记录与拉取次数会一并清零。
          </p>
          <p className="form-note">换新不改变节点密码，已导入的代理配置仍可能可用。泄露配置需要另行轮换节点账号凭据；清空网络记录只释放拉取名额。</p>
          <div className="modal-actions">
            <button className="button button-quiet" onClick={() => setRotating(null)}>取消</button>
            <button className="button button-danger" disabled={busy} onClick={() => void confirmRotate()}>确认换新</button>
          </div>
        </Modal>
      )}

      {pendingDelete && (
        <Modal title="删除订阅" kicker="CONFIRM DELETE" onClose={() => setPendingDelete(null)}>
          <p className="form-note">
            即将彻底删除 <strong>{pendingDelete.label || '未命名订阅'}</strong>
            {pendingDelete.order_id ? `（订单 ${pendingDelete.order_id}）` : ''}。链接立即失效且<strong>无法恢复</strong>，
            客户后续用同一订单号下单会重新开一条新订阅。
          </p>
          <p className="form-note">若只是客户退款或到期，建议改用「停用」：链接保留，客户续费即可恢复。</p>
          <div className="modal-actions">
            <button className="button button-quiet" onClick={() => setPendingDelete(null)}>取消</button>
            <button className="button button-danger" disabled={busy} onClick={() => void confirmDelete()}>确认删除</button>
          </div>
        </Modal>
      )}
    </>
  )
}

function CreateSubscriptionDialog({ notify, onClose, onCreated }: {
  notify: (message: string) => void
  onClose: () => void
  onCreated: () => Promise<void> | void
}) {
  const [label, setLabel] = useState('')
  const [days, setDays] = useState('30')
  const [ipLimit, setIPLimit] = useState('3')
  const [email, setEmail] = useState('')
  const [orderID, setOrderID] = useState('')
  const [autoDelete, setAutoDelete] = useState(true)
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  const [created, setCreated] = useState<{
    delivery_url: string
    subscription_url: string
    expires_at_text: string
    // 开户结果。可选是因为老后端不返回它们 ——
    // 但**只要返回了就必须显示**（见交付弹窗里的说明）。
    provisioned_nodes?: number
    excluded?: { node_id: string; node_name: string; reason: string }[]
    // 开户结果的**可信度**。unverified 表示后端读不到节点范围、无法确认，
    // 此时既不能报"全部开好"，也不能报"一个都没成" —— 必须单独显示。
    provision_status?: 'complete' | 'partial' | 'unverified'
    provision_note?: string
    // 这条订阅范围内应开户的节点数。0 = 范围里根本没有节点（空分类 / 全未上架），
    // 那不是「开户失败」，不能让运维去查节点可达性（2026-11-04）。
    expected_nodes?: number
  } | null>(null)
  const [copied, flashCopied] = useCopiedFlash()
  // 分类圈定：选了之后这条订阅只包含该分类下的节点。
  // 空 = 不圈定（全部分类），与加分类功能之前的行为一致。
  const [categoryID, setCategoryID] = useState('')
  const [categories, setCategories] = useState<NodeCategory[]>([])
  // 节点范围：全部 / 按分类 / 自选节点（多选）。
  const [scopeMode, setScopeMode] = useState<'all' | 'category' | 'nodes'>('all')
  const [allNodes, setAllNodes] = useState<Hy2Node[]>([])
  const [pickedNodes, setPickedNodes] = useState<Set<string>>(new Set())
  const [nodeFilter, setNodeFilter] = useState('')
  // 2026-11-06：节点 / 分类读取失败要说出来。原来失败时清成空数组：「自选节点」写「还没有节点」，
  // 分类下拉只剩「请选择分类」—— 看起来像没节点、没分类，而不是读取失败。
  const [scopeLoadError, setScopeLoadError] = useState('')
  useEffect(() => {
    let alive = true
    api.nodes().then((list) => { if (alive) setAllNodes(list) }).catch((err) => {
      if (alive) setScopeLoadError(`节点列表读取失败：${err instanceof Error ? err.message : '未知错误'}`)
    })
    return () => { alive = false }
  }, [])

  useEffect(() => {
    let alive = true
    void (async () => {
      try {
        const res = await api.listCategories()
        // 只列**启用中**的分类：停用的分类不该再被用于新订阅。
        // 已存在的老订阅仍能用（后端解析时不看 enabled），但新建时不给选。
        if (alive) setCategories((res.categories ?? []).filter((c) => c.enabled))
      } catch (err) {
        if (alive) setScopeLoadError((prev) => prev || `分类读取失败：${err instanceof Error ? err.message : '未知错误'}`)
      }
    })()
    return () => { alive = false }
  }, [])

  async function copy(label: string, value: string) {
    const ok = await copyToClipboard(value)
    if (ok) {
      flashCopied(label)
      notify(`已复制${label}`)
    } else {
      notify('复制失败，请手动选中复制')
    }
  }

  async function submit() {
    const value = Number(days)
    const ips = Number(ipLimit)
    if (!Number.isFinite(value) || value < 0 || value > 3650) {
      notify('有效天数需为 0-3650 之间的数字（0 表示永久）')
      return
    }
    if (!Number.isInteger(ips) || ips < 0 || ips > 1000) {
      notify('在线网络数需为 0-1000 之间的整数（0 表示不限）')
      return
    }
    if (note.length > 256) {
      notify('备注最多 256 个字符')
      return
    }
    if (scopeMode === 'nodes' && pickedNodes.size === 0) {
      notify('请至少选择一个节点，或把节点范围改为「全部节点」')
      return
    }
    if (scopeMode === 'category' && !categoryID) {
      notify('请选择一个分类，或把节点范围改为「全部节点」')
      return
    }
    setBusy(true)
    try {
      const result = await api.createSubscription({
        label: label.trim() || undefined,
        duration_days: value,
        ip_limit: ips,
        user_email: email.trim() || undefined,
        auto_delete: autoDelete,
        note: note.trim() || undefined,
        category_id: scopeMode === 'category' ? categoryID || undefined : undefined,
        node_ids: scopeMode === 'nodes' ? [...pickedNodes] : undefined,
      })
      // 2026-11-04：按实际提交的范围描述。原来只看 categoryID：自选节点时（后端会忽略分类）
      // 提示总是「含全部节点」，先选了分类再切到自选节点时还会报出一个根本没生效的分类名。
      const scopeText = scopeMode === 'nodes'
        ? `（仅含所选 ${pickedNodes.size} 个节点）`
        : scopeMode === 'category'
          ? `（仅含「${categories.find((c) => c.id === categoryID)?.name ?? '所选分类'}」分类的节点）`
          : '（不按分类圈定，含全部节点）'
      // 提示语里带上开户结果 —— 部分失败时不能只说"已开通"。
      const prov = result.provisioned_nodes
      const gaps = result.excluded?.length ?? 0
      // unverified 必须单独说：后端读不到节点范围时，"成功 0 个"（见下方弹窗
      // 的红色结论）会把一次其实可能正常的交付报成"客户必定连不上"。
      const provNote = result.provision_status === 'unverified'
        ? `；⚠️ 开户结果未确认：${result.provision_note ?? '请用「账号对账」复查后再交付'}`
        : result.expected_nodes === 0
          ? '；⚠️ 这个范围里目前没有可用节点，客户导入后是空的'
        : typeof prov !== 'number'
          ? ''
          : gaps > 0 || prov === 0
            ? `；⚠️ 开户：成功 ${prov} 个节点，${gaps} 个未成功`
            : `；已在 ${prov} 个节点开户`
      notify(`已开通订阅：${result.label}，到期 ${result.expires_at_text}` +
        scopeText +
        provNote)
      setCreated({
        delivery_url: result.delivery_url,
        subscription_url: result.subscription_url,
        expires_at_text: result.expires_at_text,
        provisioned_nodes: result.provisioned_nodes,
        excluded: result.excluded,
        provision_status: result.provision_status,
        provision_note: result.provision_note,
        expected_nodes: result.expected_nodes,
      })
      await onCreated()
    } catch (err) {
      notify(err instanceof Error ? `开通失败：${err.message}` : '开通失败')
    } finally {
      setBusy(false)
    }
  }

  // Step 2: the link is the deliverable. Showing it before closing means the
  // operator never has to hunt for it in the table afterwards.
  if (created) {
    return (
      <Modal title="订阅已开通" kicker="DELIVER THIS LINK" onClose={onClose}>
        {/* ⚠️ minWidth:0 是必需的，不是可选优化。
            grid 子项的默认 min-width 是 auto —— 它**会被内容撑宽**，
            于是里面 cred-value 的 min-width:0 完全不起作用，
            最终把 modal-card（overflow:auto）顶出横向滚动条。
            实测撑宽源：长 URL（cred-row）与长说明段（service-warn）。
            加这一条后容器不再被内容撑开，省略号才能正常生效。 */}
        {/* ⚠️ gridTemplateColumns:'minmax(0,1fr)' 是必需的，不是可选优化。
            默认的 grid 列宽是 **auto** —— 它会按内容的最大宽度铺开，
            于是里面 cred-value 的 min-width:0 完全不起作用，
            长 URL 把整列撑宽，最终顶出 modal-card 的横向滚动条。

            量出来的证据（tests/ui/measure-modal.cjs）：
              grid 容器  cw=538  sw=667   ← 被内容撑宽了 129px
              cred-value cw=478  sw=490   ← 理论上该省略，实际没省略
            改成 minmax(0,1fr) 后列宽不再随内容增长，
            cred-value 的 overflow:hidden + ellipsis 才能生效。 */}
        <div style={{ display: 'grid', gap: 10, minWidth: 0, gridTemplateColumns: 'minmax(0, 1fr)' }}>
          <p className="form-note service-warn">
            <ExternalLink size={13} />
            <span>把<strong>交付页链接</strong>发给客户即可：客户打开后能看到状态、二维码与三种格式的下载，不需要登录面板。</span>
          </p>
          <div className="cred-row">
            <div className="cred-label">交付页链接</div>
            <div className="cred-value mono" title={created.delivery_url}>{created.delivery_url}</div>
            <button className="button button-primary button-compact" onClick={() => void copy('交付页链接', created.delivery_url)}>{copied === '交付页链接' ? '已复制' : '复制'}</button>
          </div>
          <div className="cred-row">
            <div className="cred-label">订阅链接</div>
            <div className="cred-value mono" title={created.subscription_url}>{created.subscription_url}</div>
            <button className="button button-quiet button-compact" onClick={() => void copy('订阅链接', created.subscription_url)}>{copied === '订阅链接' ? '已复制' : '复制'}</button>
          </div>
          <p className="form-note">到期时间：{created.expires_at_text}。这两条链接都等同于凭证，只发给买家本人。</p>

          {/* 开户结果（2026-10-04 加）。
              ⚠️ 这一块是**必需的**，不是装饰：
              此前手工建订阅只插一行 DB 记录就返回、完全没在节点上开户，
              而界面只说「已开通订阅」—— 操作者以为好了，客户却连不上。
              那个 BUG 能潜伏那么久，正是因为**界面从不报告开户结果**。

              三种状态分开显示，因为处理动作完全不同：
                全部成功 → 绿色，可以放心交付
                一个都没成 → 红色，**别交付**，先查节点
                部分成功 → 黄色，链接能用但覆盖不全，告知缺哪台 */}
          {created.provision_status === 'unverified' && (
            <p className="form-note service-warn">
              <AlertTriangle size={13} />
              <span>
                <strong>开户结果未确认：</strong>
                {created.provision_note ?? '后端读不到节点范围，无法判断是否已开户。'}
              </span>
            </p>
          )}
          {created.provision_status !== 'unverified' && typeof created.provisioned_nodes === 'number' && (
            (() => {
              const n = created.provisioned_nodes
              const gaps = created.excluded ?? []
              if (gaps.length === 0 && n > 0) {
                return (
                  <p className="form-note service-ok">
                    <Check size={13} />
                    <span>已在 <strong>{n}</strong> 个节点上开户完成，客户导入后可直接使用。</span>
                  </p>
                )
              }
              if (n === 0 && created.expected_nodes === 0) {
                return (
                  <p className="form-note service-warn">
                    <AlertTriangle size={13} />
                    <span>
                      <strong>注意：这条订阅的范围里没有节点。</strong>
                      所选分类下没有节点，或这些节点都未上架 / 未接入 API。给分类加上节点（或改选范围）后，
                      系统会自动补开户；在那之前客户导入会是空的。
                    </span>
                  </p>
                )
              }
              if (n === 0) {
                return (
                  <p className="form-note service-warn">
                    <AlertTriangle size={13} />
                    <span>
                      <strong>注意：没有在任何节点上开成账号。</strong>
                      客户导入这条链接会<b>无法连接</b>。请检查节点是否可达、
                      API 密钥是否有效，然后到订阅列表用「账号对账」重试。
                    </span>
                  </p>
                )
              }
              return (
                <div className="form-note service-warn" style={{ display: 'grid', gap: 4 }}>
                  <span style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                    <AlertTriangle size={13} />
                    <span>已在 <strong>{n}</strong> 个节点开户，但有 <strong>{gaps.length}</strong> 个节点未成功 —— 这些节点上的客户会连不上。</span>
                  </span>
                  {/* ⚠️ 每一行都要能换行。
                      第一版没设 min-width:0 / overflow-wrap，长节点名+长原因
                      把 modal-card 撑宽，底部冒出**横向滚动条**（截图才发现）。
                      .modal-card 是 overflow:auto，任何一行超宽都会触发它。 */}
                  <ul style={{ margin: '3px 0 0 0', padding: '0 0 0 20px', fontSize: 'var(--fs-2xs)', display: 'grid', gap: 2 }}>
                    {gaps.map((g) => (
                      <li key={g.node_id} style={{ minWidth: 0, overflowWrap: 'anywhere', lineHeight: 1.5 }}>
                        <strong>{g.node_name}</strong>：{g.reason}
                      </li>
                    ))}
                  </ul>
                </div>
              )
            })()
          )}

          <div className="modal-actions">
            {safeHttpUrl(created.delivery_url) && (
              <a className="button button-quiet" href={safeHttpUrl(created.delivery_url)} target="_blank" rel="noreferrer noopener"><ExternalLink size={14} />预览交付页</a>
            )}
            <button className="button button-primary" onClick={onClose}>完成</button>
          </div>
        </div>
      </Modal>
    )
  }

  return (
    <Modal title="手工开通订阅" kicker="CREATE SUBSCRIPTION" onClose={onClose}>
      <div style={{ display: 'grid', gap: 12 }}>
        <label className="form-field"><span>名称</span><input value={label} onChange={(event) => setLabel(event.target.value)} placeholder="例如：补偿-A0001 / 内测账号" /></label>
        <div className="form-row">
          <label className="form-field"><span>有效天数（0=永久）</span><input value={days} onChange={(event) => setDays(event.target.value)} inputMode="numeric" /></label>
          <label className="form-field"><span>在线网络数（按公网 IP，0=不限）</span><input value={ipLimit} onChange={(event) => setIPLimit(event.target.value)} inputMode="numeric" /></label>
        </div>
        <div className="sub-format-tabs">
          {PRESET_DAYS.map((preset) => (
            <button key={preset} className={`button button-compact ${days === String(preset) ? 'button-primary' : 'button-quiet'}`} onClick={() => setDays(String(preset))}>{preset} 天</button>
          ))}
        </div>
        <div className="form-row">
          <label className="form-field"><span>客户邮箱</span><input value={email} onChange={(event) => setEmail(event.target.value)} placeholder="可选，便于对账" /></label>
          <label className="form-field"><span>订单号</span><input value={orderID} onChange={(event) => setOrderID(event.target.value)} placeholder="留空则视为手工单" disabled /></label>
        </div>
        <label className="form-field" style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <input type="checkbox" checked={autoDelete} onChange={(event) => setAutoDelete(event.target.checked)} style={{ width: 16, height: 16 }} />
          <span>到期后自动删除（超出宽限期即清理，不可恢复；取消勾选则只停用、保留数据）</span>
        </label>
        <label className="form-field"><span>备注</span><input value={note} onChange={(event) => setNote(event.target.value)} placeholder="最多 256 字" /></label>

        {/* 节点范围：这条订阅包含哪些节点。全部 / 按分类 / 自选（多选）。 */}
        <div className="form-field">
          <span>节点范围</span>
          <div className="scope-tabs" role="tablist">
            {([['all', '全部节点'], ['category', '按分类'], ['nodes', '自选节点']] as const).map(([key, text]) => (
              <button key={key} type="button" role="tab" aria-selected={scopeMode === key} className={`scope-tab${scopeMode === key ? ' is-active' : ''}`} onClick={() => setScopeMode(key)}>{text}</button>
            ))}
          </div>
        </div>
        {scopeLoadError && scopeMode !== 'all' && <p className="form-error" style={{ marginTop: -6 }}>{scopeLoadError}，刷新页面重试。</p>}
        {scopeMode === 'all' && <p className="form-note" style={{ marginTop: -6 }}>包含全部节点；以后新加的节点也会自动出现在这条订阅里。</p>}
        {scopeMode === 'category' && (
          <>
            <label className="form-field">
              <span>节点分类</span>
              <select value={categoryID} onChange={(event) => setCategoryID(event.target.value)}>
                <option value="">请选择分类</option>
                {categories.map((c) => (
                  <option key={c.id} value={c.id}>
                    {c.name}{c.node_count > 0 ? `（${c.node_count} 个节点）` : '（暂无节点）'}
                  </option>
                ))}
              </select>
            </label>
            {categoryID && (() => {
              const c = categories.find((x) => x.id === categoryID)
              if (!c) return null
              return (
                <p className="form-note" style={{ marginTop: -6 }}>
                  {c.node_count > 0
                    ? <>这条订阅只会包含「{c.name}」分类下的 <b>{c.node_count}</b> 个节点{c.description ? ` —— ${c.description}` : ''}；分类里以后加的节点也会自动包含。</>
                    : <>⚠ 「{c.name}」分类下<b>目前没有节点</b>，开通后订阅会是空的。请先到节点页给节点勾选这个分类。</>}
                </p>
              )
            })()}
          </>
        )}
        {scopeMode === 'nodes' && (() => {
          const q = nodeFilter.trim().toLowerCase()
          const visible = allNodes.filter((n) => !q || `${n.name} ${n.domain ?? ''}`.toLowerCase().includes(q))
          const allVisiblePicked = visible.length > 0 && visible.every((n) => pickedNodes.has(n.id))
          return (
            <div className="node-picker">
              <div className="node-picker-head">
                <input value={nodeFilter} onChange={(e) => setNodeFilter(e.target.value)} placeholder="搜索节点名 / 域名" />
                <button type="button" className="text-button" onClick={() => setPickedNodes((cur) => {
                  const next = new Set(cur)
                  visible.forEach((n) => (allVisiblePicked ? next.delete(n.id) : next.add(n.id)))
                  return next
                })}>{allVisiblePicked ? '取消全选' : '全选'}</button>
                <span className="node-picker-count">已选 <b>{pickedNodes.size}</b> / {allNodes.length}</span>
              </div>
              <div className="node-picker-list">
                {visible.length === 0 && <p className="form-note" style={{ padding: 10 }}>{allNodes.length === 0 ? (scopeLoadError ? '读取失败' : '还没有节点') : '没有匹配的节点'}</p>}
                {visible.map((n) => (
                  <label key={n.id} className={`node-picker-item${pickedNodes.has(n.id) ? ' is-picked' : ''}`}>
                    <input type="checkbox" checked={pickedNodes.has(n.id)} onChange={() => setPickedNodes((cur) => {
                      const next = new Set(cur)
                      if (next.has(n.id)) next.delete(n.id); else next.add(n.id)
                      return next
                    })} />
                    <span className="node-picker-name">{n.name}</span>
                    <small>{n.domain || '—'}</small>
                    {!n.in_store && <span className="node-picker-tag">未上架</span>}
                  </label>
                ))}
              </div>
              <p className="form-note">只下发勾选的节点；以后新加的节点<b>不会</b>自动加入这条订阅。「未上架」的节点手工订阅也能用（自用），商城订单不会下发。</p>
            </div>
          )
        })()}

        <p className="form-note">手工创建的订阅不带订单号，因此商城无法通过订单号续费或撤销它；测试与补偿够用，正式售卖请走商城。</p>
        <div className="modal-actions">
          <button className="button button-quiet" onClick={onClose}>取消</button>
          <button className="button button-primary" disabled={busy} onClick={() => void submit()}>{busy ? '开通中…' : '开通订阅'}</button>
        </div>
      </div>
    </Modal>
  )
}

function EditSubscriptionDialog({ item, notify, onClose, onSaved }: {
  item: ManagedSubscription
  notify: (message: string) => void
  onClose: () => void
  onSaved: () => Promise<void> | void
}) {
  const [label, setLabel] = useState(item.label)
  const [days, setDays] = useState('30')
  const [ipLimit, setIPLimit] = useState(String(item.ip_limit))
  const [email, setEmail] = useState(item.user_email)
  const [note, setNote] = useState(item.note)
  const [autoDelete, setAutoDelete] = useState(item.auto_delete)
  const [resetIPs, setResetIPs] = useState(false)
  const [busy, setBusy] = useState(false)
  // 具体到期日期（YYYY-MM-DD）。
  //
  // 原来只能「+N 天」或「永久」—— 想改到"11 月 15 号"做不到，
  // 只能心算天数再点 +N。管理员 2026-10-02 明确要求支持指定日期。
  //
  // 后端本来就接受绝对时间戳（PUT 里的 expires_at），
  // 所以这一项**纯前端改动** —— 把日期转成时间戳发过去即可。
  const [expiryDate, setExpiryDate] = useState(() => {
    // 初值取当前到期日的日期部分，便于"往后挪几天"这种微调。
    if (!item.expires_at || item.expires_at <= 0) return ''
    const d = new Date(item.expires_at * 1000)
    const pad = (n: number) => String(n).padStart(2, '0')
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
  })

  // The server treats ip_limit === 0 as unlimited and an expires_at <= 0 as
  // perpetual. Both are reachable from here, so the labels say so explicitly
  // instead of leaving the operator to guess.
  async function submit(patch: { extend?: number; clearExpiry?: boolean; setDate?: boolean }) {
    const dateOnly = patch.clearExpiry || patch.setDate || patch.extend !== undefined
    const ips = Number(ipLimit)
    if (!dateOnly && (!Number.isInteger(ips) || ips < 0 || ips > 1000)) {
      notify('在线网络数需为 0-1000 之间的整数（0 表示不限）')
      return
    }
    const payload: Parameters<typeof api.updateSubscriptionRecord>[1] = dateOnly ? {} : {
      label: label.trim(),
      ip_limit: ips,
      user_email: email.trim(),
      note: note.trim(),
      auto_delete: autoDelete,
      reset_ips: resetIPs,
    }
    if (patch.clearExpiry) {
      payload.expires_at = 0
    } else if (patch.setDate) {
      // 指定到期日：把日期转成当天 23:59:59 的时间戳。
      //
      // 为什么用当天**末刻**而不是 00:00：运维说"到 11 月 15 号"时，
      // 意思是"15 号这天还能用"。用 00:00 会让客户在 15 号零点就断线 ——
      // 那不是他要的。
      const parsed = parseLocalDate(expiryDate)
      if (!parsed) {
        notify('请选择有效的到期日期')
        return
      }
      if (parsed.getTime() <= Date.now()) {
        notify('到期日必须在未来；若要让订阅失效，请改用「停用」')
        return
      }
      payload.expires_at = Math.floor(parsed.getTime() / 1000)
    } else if (patch.extend !== undefined && patch.extend > 0) {
      payload.duration_days = patch.extend
    }
    setBusy(true)
    try {
      const res = await api.updateSubscriptionRecord(item.id, payload)
      // 改到期或设备上限会同步到各节点；有节点没改成功时必须说出来，
      // 否则那台节点上的买家照旧按旧到期/旧上限使用（与 toggle 同一规则）。
      if (res?.node_state_ok === false && res.node_state_message) {
        notify(`⚠ 保存不完整：${label.trim() || item.label} —— ${res.node_state_message}`)
      } else if (res?.notice) {
        // 2026-11-12：保存成功但有必须看到的提示，例如续期后订阅仍是停用（买家还不能用）、
        // 或重新生效后节点账号要等后台补开。原来一律只显示「已保存」。
        notify(`已保存：${label.trim() || item.label} —— ${res.notice}`)
      } else {
        notify(`已保存：${label.trim() || item.label}`)
      }
      await onSaved()
    } catch (err) {
      notify(err instanceof Error ? `保存失败：${err.message}` : '保存失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal title={`编辑订阅 · ${item.label || '未命名'}`} kicker="EDIT SUBSCRIPTION" onClose={onClose}>
      <div style={{ display: 'grid', gap: 12 }}>
        <div className="drawer-info-grid">
          <div><span>当前到期</span><strong>{formatDateTime(item.expires_at)}</strong></div>
          <div><span>剩余</span><strong>{item.expired ? '已到期' : item.days_left < 0 ? '永久有效' : `${item.days_left} 天`}</strong></div>
          <div><span>网络占用</span><strong>{item.active_ip_count}{item.ip_limit > 0 ? ` / ${item.ip_limit}` : ' / 不限'}</strong></div>
          <div><span>拉取次数</span><strong>{item.access_count}</strong></div>
        </div>

        <label className="form-field"><span>名称</span><input value={label} onChange={(event) => setLabel(event.target.value)} /></label>
        <div className="form-row">
          <label className="form-field"><span>在线网络数（按公网 IP，0=不限）</span><input value={ipLimit} onChange={(event) => setIPLimit(event.target.value)} inputMode="numeric" /></label>
          <label className="form-field"><span>客户邮箱</span><input value={email} onChange={(event) => setEmail(event.target.value)} /></label>
        </div>
        <label className="form-field"><span>备注</span><input value={note} onChange={(event) => setNote(event.target.value)} placeholder="最多 256 字" /></label>
        <label className="form-field" style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <input type="checkbox" checked={autoDelete} onChange={(event) => setAutoDelete(event.target.checked)} style={{ width: 16, height: 16 }} />
          <span>到期后自动删除</span>
        </label>
        <label className="form-field" style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <input type="checkbox" checked={resetIPs} onChange={(event) => setResetIPs(event.target.checked)} style={{ width: 16, height: 16 }} />
          <span>清空网络记录（客户换网络后想立刻释放名额时勾选）</span>
        </label>

        <p className="form-note service-warn">
          <CalendarClock size={13} />
          <span>上面这些改动点「保存」提交。下面的日期操作<strong>点了就会立即生效</strong>。</span>
        </p>

        {/* 指定到期日：原来只能 +N 天或永久，改不到具体日期。
            这里给 <input type="date"> + 一个明确的「设为该日」按钮 ——
            不做"改了输入框就自动提交"，因为到期日是**会立刻影响客户能否使用**的
            字段，必须让操作者显式确认。 */}
        <div className="form-row">
          <label className="form-field">
            <span>指定到期日</span>
            <input
              type="date"
              value={expiryDate}
              onChange={(event) => setExpiryDate(event.target.value)}
            />
          </label>
          <div className="form-field" style={{ alignSelf: 'end' }}>
            <button
              className="button button-quiet"
              disabled={busy || !expiryDate}
              onClick={() => void submit({ setDate: true })}
            >
              设为该日 23:59 到期
            </button>
          </div>
        </div>
        <p className="form-note" style={{ marginTop: -4 }}>
          按<strong>当天末刻</strong>算（选 11-15 表示 15 号这天仍可使用）。
        </p>

        <div className="sub-format-tabs">
          {PRESET_DAYS.map((preset) => (
            <button key={preset} className="button button-quiet button-compact" disabled={busy} onClick={() => void submit({ extend: preset })}>+{preset} 天</button>
          ))}
          <button className="button button-quiet button-compact" disabled={busy} onClick={() => void submit({ clearExpiry: true })}>改为永久有效</button>
        </div>
        <p className="form-note">「+N 天」在未到期时从当前到期日顺延，已到期则从此刻起算。「改为永久有效」会清除到期限制。</p>

        <div className="modal-actions">
          <button className="button button-quiet" onClick={onClose}>取消</button>
          <button className="button button-primary" disabled={busy} onClick={() => void submit({})}>{busy ? '保存中…' : '保存'}</button>
        </div>
      </div>
    </Modal>
  )
}

function MetricCard({ label, value, unit, icon, tone, foot }: {
  label: string; value: string; unit: string; icon: React.ReactNode; tone: string; foot: React.ReactNode
}) {
  return (
    <div className={`metric-card tone-${tone}`}>
      <div className="metric-top"><span>{label}</span><span className="metric-icon">{icon}</span></div>
      <div className="metric-value">{value}<small>{unit}</small></div>
      <div className="metric-foot">{foot}</div>
    </div>
  )
}
