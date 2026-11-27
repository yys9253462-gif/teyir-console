import { useEffect, useRef, useState } from 'react'
import { api, loadSession, type Hy2Node, type PortalNodeStatus, type PortalVerb, isAdminSession, ADMIN_ONLY_HINT } from '../lib/api'
import { Modal } from './Modal'
import { askConfirm } from './ConfirmHost'

// 批量运维：把单节点「运维」页里最常用的操作一次下发到多台节点。
//
// 不新增后端接口，逐台复用 /portal-status + /portal-action：
// 服务端的动词白名单、组件门禁（Requires）、审计都照常生效。
//
// 每个操作都先读状态再决定做不做，保证**重复执行是安全的**：
//   - reality_toggle 是「切换」—— 不先看状态就批量下发，已开启的节点会被关掉；
//   - install_* 会真的跑安装脚本，已装好的节点没必要再装一遍。

// 2026-10-30：补齐各协议的批量开关 —— Reality / VMess / VLESS Encryption / AnyTLS
// 各有「开启」「关闭」。关闭会断开该协议上的现有连接，执行前单独确认；
// 节点门户太旧、没有该协议端点（状态 unsupported）时如实标「无需操作」，不算失败。
type ProtoKey = 'vmess' | 'vlessenc' | 'anytls'
type OpKey = 'reality' | 'reality_off' | 'vmess_on' | 'vmess_off' | 'vlessenc_on' | 'vlessenc_off' | 'anytls_on' | 'anytls_off' | 'bbr' | 'gost'

// adminOnly：会用到后端只允许管理员的动词（停用协议是危险动词；gost 与 Reality 首次开启需要 install_*）。
const ADMIN_ONLY_OPS = new Set<OpKey>(['reality', 'vmess_off', 'vlessenc_off', 'anytls_off', 'gost'])

const OPS: { key: OpKey; label: string; hint: string; group: string; off?: boolean }[] = [
  { key: 'reality', label: '安装并开启 Reality', hint: '未装 Xray 先安装；已在运行的节点跳过', group: '协议' },
  { key: 'reality_off', label: '关闭 Reality', hint: '只停 Reality 入站，不影响同一 Xray 里的 VMess / VLESS Encryption；未在运行的节点跳过', group: '协议', off: true },
  { key: 'vmess_on', label: '开启 VMess', hint: '需要 Xray（先「安装并开启 Reality」）以及节点域名与有效证书；已开启的节点跳过', group: '协议' },
  { key: 'vmess_off', label: '关闭 VMess', hint: '关闭 VMess 入站并重载 Xray；未开启的节点跳过', group: '协议', off: true },
  { key: 'vlessenc_on', label: '开启 VLESS Encryption', hint: '需要 Xray 以及节点域名与有效证书；已开启的节点跳过', group: '协议' },
  { key: 'vlessenc_off', label: '关闭 VLESS Encryption', hint: '关闭 VLESS Encryption 入站并重载 Xray；未开启的节点跳过', group: '协议', off: true },
  { key: 'anytls_on', label: '开启 AnyTLS', hint: '由独立的 sing-box 承载，需要节点域名与有效证书；门户版本过旧的节点跳过', group: '协议' },
  { key: 'anytls_off', label: '关闭 AnyTLS', hint: '停用 sing-box 上的 AnyTLS；未开启的节点跳过', group: '协议', off: true },
  { key: 'bbr', label: '开启 BBR', hint: '写入 sysctl 并热加载；部分内核需重启后生效', group: '系统' },
  { key: 'gost', label: '安装 gost', hint: '代理服务运行时；已安装的节点跳过', group: '系统' },
]

const PROTOCOLS: Record<ProtoKey, { name: string; on: PortalVerb; off: PortalVerb }> = {
  vmess: { name: 'VMess', on: 'vmess_enable', off: 'vmess_disable' },
  vlessenc: { name: 'VLESS Encryption', on: 'vlessenc_enable', off: 'vlessenc_disable' },
  anytls: { name: 'AnyTLS', on: 'anytls_enable', off: 'anytls_disable' },
}

type RowState = { status: 'queued' | 'running' | 'success' | 'skipped' | 'failed'; message: string }

const STATUS_TEXT: Record<RowState['status'], string> = {
  queued: '等待', running: '执行中', success: '完成', skipped: '无需操作', failed: '失败',
}
const STATUS_COLOR: Record<RowState['status'], string> = {
  queued: 'var(--muted)', running: 'var(--text-soft)', success: 'var(--green)', skipped: 'var(--muted)', failed: 'var(--red)',
}

// 同时进行的节点数。install 脚本单台可能跑一两分钟，
// 全并发会让面板同时挂着十几条 120s 的长请求；3 台是体感和负载的折中。
const CONCURRENCY = 3

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err)
}

type ProtoProbe = { state: string; error?: string; payload?: { installed: boolean; active: boolean; enabled?: boolean } }

function protoProbe(status: PortalNodeStatus | null, key: ProtoKey): ProtoProbe | undefined {
  return status ? (status.probes[key] as ProtoProbe | undefined) : undefined
}

// VMess / VLESS Encryption / AnyTLS：先读状态，只在需要时下发 enable / disable，再复查。
async function switchProtocol(node: Hy2Node, key: ProtoKey, want: boolean, step: (m: string) => void): Promise<RowState> {
  const proto = PROTOCOLS[key]
  step('读取节点状态…')
  const before = protoProbe(await api.nodePortalStatus(node.id), key)
  if (!before || before.state === 'unsupported') {
    return { status: 'skipped', message: `节点门户版本不支持 ${proto.name}（先「批量更新门户」）` }
  }
  if (before.state !== 'healthy' || !before.payload) {
    throw new Error(`读不到 ${proto.name} 状态（${before.error || before.state}），未下发`)
  }
  const on = before.payload.active || Boolean(before.payload.enabled)
  if (want && before.payload.active) return { status: 'skipped', message: `${proto.name} 已在运行` }
  if (!want && !on) return { status: 'skipped', message: `${proto.name} 本来就没开` }
  step(want ? `开启 ${proto.name}…` : `关闭 ${proto.name}…`)
  await api.nodePortalAction(node.id, { verb: want ? proto.on : proto.off }, !want)
  const after = protoProbe(await api.nodePortalStatus(node.id).catch(() => null), key)
  if (!after?.payload) return { status: 'success', message: want ? '已下发开启（未能复查）' : '已下发关闭（未能复查）' }
  if (want && !after.payload.active) return { status: 'failed', message: `已下发开启，但复查时 ${proto.name} 仍未运行` }
  if (!want && after.payload.active) return { status: 'failed', message: `已下发关闭，但复查时 ${proto.name} 仍在运行` }
  return { status: 'success', message: want ? `${proto.name} 已运行` : `${proto.name} 已关闭` }
}

async function runOne(node: Hy2Node, op: OpKey, bbrVersion: string, step: (message: string) => void): Promise<RowState> {
  if (op === 'bbr') {
    step('下发 BBR 设置…')
    await api.nodePortalAction(node.id, { verb: 'set_bbr', params: { version: bbrVersion } })
    const status = await api.nodePortalStatus(node.id).catch(() => null)
    const bbr = status?.probes?.bbr?.payload
    if (!bbr) return { status: 'success', message: `已下发 BBR ${bbrVersion}` }
    return {
      status: 'success',
      message: `当前 ${bbr.current || '—'} / ${bbr.qdisc || '—'}${bbr.need_reboot ? ' · 需重启服务器后生效' : ''}`,
    }
  }
  if (op === 'vmess_on' || op === 'vmess_off') return switchProtocol(node, 'vmess', op === 'vmess_on', step)
  if (op === 'vlessenc_on' || op === 'vlessenc_off') return switchProtocol(node, 'vlessenc', op === 'vlessenc_on', step)
  if (op === 'anytls_on' || op === 'anytls_off') return switchProtocol(node, 'anytls', op === 'anytls_on', step)

  step('读取节点状态…')
  const status: PortalNodeStatus = await api.nodePortalStatus(node.id)

  if (op === 'gost') {
    const gost = status.probes?.gost
    if (gost?.state === 'healthy' && gost.payload?.gost_installed) {
      return { status: 'skipped', message: gost.payload.gost_active ? '已安装且在运行' : '已安装（服务未运行）' }
    }
    step('安装 gost…')
    await api.nodePortalAction(node.id, { verb: 'install_gost' })
    return { status: 'success', message: '已安装 gost' }
  }

  // reality / reality_off —— 节点只提供 toggle：必须先读状态再决定，
  // 否则开启会把已开的关掉、关闭会把已关的打开。
  const reality = status.probes?.reality
  if (!reality || reality.state !== 'healthy' || !reality.payload) {
    throw new Error(`读不到 Reality 状态（${reality?.error || reality?.state || '未知'}），为避免误切未下发`)
  }
  if (op === 'reality_off') {
    if (!reality.payload.installed || !reality.payload.active) return { status: 'skipped', message: 'Reality 本来就没开' }
    step('关闭 Reality…')
    await api.nodePortalAction(node.id, { verb: 'reality_toggle' })
    const after = await api.nodePortalStatus(node.id).catch(() => null)
    const payload = after?.probes.reality.payload
    if (payload && payload.active) return { status: 'failed', message: '已下发关闭，但复查时 Reality 仍在运行' }
    return { status: 'success', message: payload ? 'Reality 已关闭' : '已下发关闭（未能复查）' }
  }
  if (reality.payload.installed && reality.payload.active) {
    return { status: 'skipped', message: 'Reality 已在运行' }
  }
  if (!reality.payload.installed) {
    step('安装 Xray…')
    await api.nodePortalAction(node.id, { verb: 'install_xray' })
    // install-xray 自己就会生成配置并启动 xray。装完若直接 toggle，
    // 会把刚启动的 Reality 又关掉 —— 必须重读一次状态再决定。
    step('复查 Reality 状态…')
    const installed = await api.nodePortalStatus(node.id)
    const payload = installed.probes.reality.payload
    if (installed.probes.reality.state !== 'healthy' || !payload) {
      throw new Error('Xray 已安装，但读不到 Reality 状态，为避免误关未继续 toggle')
    }
    if (payload.active) return { status: 'success', message: '已安装 Xray，Reality 已运行' }
  }
  step('开启 Reality…')
  await api.nodePortalAction(node.id, { verb: 'reality_toggle' })
  const after = await api.nodePortalStatus(node.id).catch(() => null)
  const payload = after?.probes.reality.payload
  if (payload && !payload.active) {
    return { status: 'failed', message: '已下发开启，但复查时 Reality 仍未运行' }
  }
  return { status: 'success', message: payload ? 'Reality 已运行' : '已下发开启（未能复查）' }
}

export function BatchNodeOps({ nodes, onClose, notify }: {
  nodes: Hy2Node[]; onClose: () => void; notify: (message: string) => void
}) {
  const readOnly = loadSession()?.role === 'viewer'
  const eligible = nodes.filter((n) => n.has_basic)
  const eligibleIds = new Set(eligible.map((n) => n.id))
  // 非管理员默认选一个自己能执行的操作（Reality 首次开启需要 install_xray，仅限管理员）。
  const [op, setOp] = useState<OpKey>(isAdminSession() ? 'reality' : 'bbr')
  const [bbrVersion, setBbrVersion] = useState('v1')
  const [selected, setSelected] = useState<string[]>([])
  const activeSelected = selected.filter((id) => eligibleIds.has(id))
  const [rows, setRows] = useState<Record<string, RowState>>({})
  const [running, setRunning] = useState(false)
  const cancelled = useRef(false)
  const mountedRef = useRef(true)
  useEffect(() => {
    mountedRef.current = true
    return () => { mountedRef.current = false }
  }, [])
  const current = OPS.find((o) => o.key === op)!
  const cannotRun = readOnly || (!isAdminSession() && ADMIN_ONLY_OPS.has(op))

  async function start() {
    if (cannotRun || running || !activeSelected.length) return
    const label = op === 'bbr' ? `开启 BBR ${bbrVersion}` : current.label
    const warn = current.off ? '\n\n⚠️ 关闭会断开该协议上的现有连接，用这个协议的客户需要换其它协议或重新导入订阅。' : ''
    if (!await askConfirm(`确认对 ${activeSelected.length} 台节点执行「${label}」？\n\n${current.hint}${warn}`)) return
    cancelled.current = false
    setRunning(true)
    const targets = eligible.filter((n) => activeSelected.includes(n.id))
    setRows(Object.fromEntries(targets.map((n) => [n.id, { status: 'queued', message: '' } as RowState])))

    const queue = [...targets]
    // 2026-11-06：结束提示要带上失败数。原来不论成败都说「批量执行完毕」，有节点失败时操作者以为全部成功。
    const outcome = { failed: 0, done: 0, unchanged: 0 }
    const worker = async () => {
      for (let node = queue.shift(); node; node = queue.shift()) {
        if (cancelled.current) {
          if (mountedRef.current) setRows((r) => ({ ...r, [node!.id]: { status: 'skipped', message: '已停止，未下发' } }))
          continue
        }
        const id = node.id
        const step = (message: string) => { if (mountedRef.current) setRows((r) => ({ ...r, [id]: { status: 'running', message } })) }
        step('准备中…')
        let result: RowState
        try { result = await runOne(node, op, bbrVersion, step) }
        catch (err) { result = { status: 'failed', message: errText(err) } }
        if (result.status === 'failed') outcome.failed++
        else if (result.status === 'skipped') outcome.unchanged++
        else outcome.done++
        if (mountedRef.current) setRows((r) => ({ ...r, [id]: result }))
      }
    }
    await Promise.all(Array.from({ length: Math.min(CONCURRENCY, targets.length) }, worker))
    if (mountedRef.current) setRunning(false)
    const parts = [`${outcome.done} 台完成`]
    if (outcome.unchanged > 0) parts.push(`${outcome.unchanged} 台跳过（无需改动、门户不支持或已停止）`)
    notify(outcome.failed > 0
      ? `⚠ 「${label}」批量执行结束：${outcome.failed} 台失败、${parts.join('、')}，失败原因见列表`
      : `「${label}」批量执行完毕：${parts.join('、')}`)
  }

  const results = Object.values(rows)
  const count = (s: RowState['status']) => results.filter((r) => r.status === s).length
  const failedIds = Object.entries(rows).filter(([id, r]) => r.status === 'failed' && eligibleIds.has(id)).map(([id]) => id)

  return <Modal title="批量运维" kicker="BATCH NODE OPS" wide onClose={async () => { if (!running || await askConfirm('仍有节点在执行，关闭后进行中的节点会继续跑完，但看不到结果。确认关闭？')) onClose() }}>
    <div style={{ color: 'var(--text)', fontSize: 'var(--fs-sm)', lineHeight: 1.65 }}>
      <p style={{ marginBottom: 10 }}>对多台节点同时下发常用运维操作。每台先读状态再决定是否执行，<strong>重复执行是安全的</strong>；同时最多处理 {CONCURRENCY} 台。</p>

      {['协议', '系统'].map((group) => (
        <div key={group} style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 8, flexWrap: 'wrap' }}>
          <small style={{ color: 'var(--muted)', minWidth: 28 }}>{group}</small>
          <div className="monitor-filter" role="group" aria-label={group} style={{ display: 'inline-flex', flexWrap: 'wrap' }}>
            {OPS.filter((o) => o.group === group).map((o) => <button key={o.key} className={op === o.key ? 'active' : ''} disabled={running || (!isAdminSession() && ADMIN_ONLY_OPS.has(o.key))} title={!isAdminSession() && ADMIN_ONLY_OPS.has(o.key) ? ADMIN_ONLY_HINT : undefined} onClick={() => { setOp(o.key); setRows({}) }}>{o.label}</button>)}
          </div>
        </div>
      ))}
      <p className="form-note" style={{ marginBottom: 10 }}>{current.hint}</p>

      {op === 'bbr' && <label style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 10 }}>
        BBR 版本
        <select value={bbrVersion} disabled={running} onChange={(e) => setBbrVersion(e.target.value)}>
          <option value="v1">v1（内核自带，最稳）</option>
          <option value="v2">v2</option>
          <option value="v3">v3</option>
        </select>
      </label>}

      <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap', marginBottom: 12 }}>
        <button className="button button-quiet" disabled={cannotRun || running || !eligible.length} onClick={() => setSelected(eligible.map((n) => n.id))}>全选可操作（{eligible.length}）</button>
        {!running && failedIds.length > 0 && <button className="button button-quiet" disabled={cannotRun} onClick={() => setSelected(failedIds)}>只选失败的（{failedIds.length}）</button>}
        <button className="button button-quiet" disabled={cannotRun || running || !activeSelected.length} onClick={() => setSelected([])}>清空选择</button>
        <button className="button button-primary" disabled={cannotRun || running || !activeSelected.length} onClick={() => void start()}>
          {running ? '执行中…' : `开始执行（${activeSelected.length}）`}
        </button>
        {running && <button className="button button-quiet" onClick={() => { cancelled.current = true }}>停止剩余节点</button>}
      </div>
      {readOnly && <p>只读账号只能查看，不能执行。</p>}

      {results.length > 0 && <p style={{ margin: '0 0 8px' }}>
        完成 {count('success')} · 无需操作 {count('skipped')}
        {count('failed') > 0 && <> · <span style={{ color: 'var(--red)' }}>失败 {count('failed')}</span></>}
        {running && <> · 进行中 {count('running') + count('queued')}</>}
      </p>}

      <div style={{ display: 'grid', gap: 6 }}>
        {nodes.map((node) => {
          const row = rows[node.id]
          return <label key={node.id} style={{ display: 'flex', alignItems: 'baseline', gap: 8, flexWrap: 'wrap', padding: '4px 0', borderBottom: '1px solid var(--line-soft)' }}>
            <input type="checkbox" checked={selected.includes(node.id)} disabled={cannotRun || !node.has_basic || running}
              onChange={(e) => setSelected((ids) => e.target.checked ? [...ids, node.id] : ids.filter((id) => id !== node.id))} />
            <span style={{ flexShrink: 0 }}>{node.name}</span>
            {!node.has_basic
              ? <small style={{ color: 'var(--muted)' }}>未绑定门户管理账号，无法下发</small>
              : row && <small style={{ color: STATUS_COLOR[row.status], overflowWrap: 'anywhere' }}>{STATUS_TEXT[row.status]}{row.message ? ` · ${row.message}` : ''}</small>}
          </label>
        })}
        {!nodes.length && <p>还没有节点，请先接入节点。</p>}
      </div>
    </div>
  </Modal>
}
