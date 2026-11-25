import { useCallback, useEffect, useRef, useState } from 'react'
import { Globe, Loader2, Plus, RefreshCw, Route, Trash2 } from 'lucide-react'
import { api, loadSession, type Hy2Node, type NodeOutboundState, type NodeOutbound, isAdminSession } from '../lib/api'
import { Modal } from './Modal'
import { askConfirm } from './ConfirmHost'

// 节点出口设置（2026-10-15）。
//
// 节点 portal 的「自定义出站」一直存在，面板没接。这里提供：
//   - 出站列表（socks5 / http / direct）的新增、删除与「测出口 IP」；
//   - 模式切换：全局出口（所有流量走第一个自定义出站）/ 按域名分流；
//   - 域名分流规则：某个域名走哪个出站。
// 写操作经 portal-action 的 manage_outbounds（服务端白名单 + 校验 + 审计）。
// 改动由节点异步应用（会重启 Hysteria，在线用户短暂断线重连），页面轮询「应用状态」。

const TYPE_LABEL: Record<string, string> = { socks5: 'SOCKS5', http: 'HTTP', direct: '直连' }

function describe(o: NodeOutbound): string {
  if (o.type === 'socks5') return `${o.addr}${o.username ? ` · 用户 ${o.username}` : ''}${o.has_password ? ' · 已设密码' : ''}`
  if (o.type === 'http') return `${o.url}${o.insecure ? ' · 跳过证书校验' : ''}`
  return o.mode ? `IP 优先级 ${o.mode}` : '本机网络'
}

export function NodeEgress({ node, notify }: { node: Hy2Node; notify: (message: string) => void }) {
  // 出口设置的写操作走 manage_outbounds，后端只允许管理员（2026-11-03）。
  const readOnly = loadSession()?.role === 'viewer' || !isAdminSession()
  const [state, setState] = useState<NodeOutboundState | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [probes, setProbes] = useState<Record<string, string>>({})
  const [adding, setAdding] = useState(false)
  const [ruleDomain, setRuleDomain] = useState('')
  const [ruleTarget, setRuleTarget] = useState('')

  // 2026-11-06：只采用最后一次请求的结果。切到另一台节点（或应用中的 3 秒轮询与手动刷新交错）时，
  // 原来晚回来的旧请求会把上一台节点的出口设置显示在当前节点下，接着的操作就基于错的状态。
  const loadSeq = useRef(0)
  const load = useCallback(async () => {
    const seq = ++loadSeq.current
    try {
      const next = await api.nodeOutbounds(node.id)
      if (seq !== loadSeq.current) return
      setState(next)
      setError('')
    } catch (err) {
      if (seq !== loadSeq.current) return
      setError(err instanceof Error ? err.message : '读取出口设置失败')
    }
  }, [node.id])

  useEffect(() => {
    void load()
    return () => { loadSeq.current++ }
  }, [load])

  // 节点在后台应用配置时每 3 秒刷新一次，直到应用完成；标签页隐藏时暂停并在切回前台补拉一次。
  useEffect(() => {
    if (!state?.applying) return
    const t = window.setTimeout(() => { if (!document.hidden) void load() }, 3000)
    const onVisible = () => { if (!document.hidden) void load() }
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      window.clearTimeout(t)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [state, load])

  async function act(params: Record<string, string>, done: string) {
    setBusy(true)
    try {
      await api.nodePortalAction(node.id, { verb: 'manage_outbounds', params })
      notify(`${done}，节点正在应用（Hysteria 会重启，在线用户短暂断线重连）`)
      await load()
      return true
    } catch (err) {
      notify(err instanceof Error ? err.message : '操作失败')
      return false
    } finally {
      setBusy(false)
    }
  }

  async function probe(name: string) {
    setProbes((p) => ({ ...p, [name]: '检测中…' }))
    try {
      const r = await api.nodeOutboundProbe(node.id, name)
      setProbes((p) => ({ ...p, [name]: r.ip || '未能取得出口 IP（出站可能不可用）' }))
    } catch (err) {
      setProbes((p) => ({ ...p, [name]: err instanceof Error ? err.message : '检测失败' }))
    }
  }

  if (error) {
    return <div className="empty-state compact"><Globe size={18} /><strong>无法读取出口设置</strong><span>{error}</span><button onClick={() => void load()}>重试</button></div>
  }
  if (!state) {
    return <div className="empty-state compact"><Loader2 size={18} className="spin" /><strong>读取中…</strong></div>
  }

  const outbounds = state.outbounds ?? []
  const rules = state.rules ?? []
  const builtin = state.builtin ?? []
  const targets = ['warp_socks', ...outbounds.map((o) => o.name)].filter((n) => n !== 'warp_socks' || builtin.includes('warp_socks') || builtin.includes('warp'))
  const globalTarget = outbounds[0]?.name

  return (
    <div className="egress">
      <div className="egress-status">
        {state.applying
          ? <span className="status-pill"><Loader2 size={12} className="spin" /> 正在应用到节点…</span>
          : state.apply_error
            ? <span className="status-pill offline" title={state.apply_error}>上次应用失败：{state.apply_error}</span>
            : state.applied
              ? <span className="status-pill online">配置已生效</span>
              : <span className="status-pill">配置未生效</span>}
        <button className="text-button" onClick={() => void load()}><RefreshCw size={13} />刷新</button>
      </div>

      <section className="egress-block">
        <div className="egress-head">
          <div><strong>出口模式</strong><span>决定节点上用户流量从哪里出去</span></div>
        </div>
        <div className="monitor-filter" role="group" aria-label="出口模式">
          <button className={state.mode !== 'global' ? 'active' : ''} disabled={busy || readOnly || state.mode !== 'global'}
            onClick={() => void act({ action: 'mode', mode: 'rules' }, '已切换为按域名分流')}>按域名分流</button>
          {/* 没有出站时不禁用：禁用按钮点了毫无反应，用户只会以为坏了。
              改为点击后说明原因并直接打开「新增出站」。 */}
          <button className={state.mode === 'global' ? 'active' : ''} disabled={busy || readOnly || state.mode === 'global'}
            onClick={async () => {
              if (!outbounds.length) {
                notify('全局出口会让所有流量走一个自定义出站，这台节点还没有出站 —— 先新增一个')
                setAdding(true)
                return
              }
              if (await askConfirm(`切换为全局出口后，这台节点的所有流量都会走「${globalTarget}」。\n出站不可用时用户将无法上网，确认切换？`)) {
                void act({ action: 'mode', mode: 'global' }, '已切换为全局出口')
              }
            }}>全局出口</button>
        </div>
        {!outbounds.length && state.mode !== 'global' && (
          <p className="form-note">要用全局出口，先在下方「出站」里新增一个 SOCKS5 / HTTP 出站。</p>
        )}
        <p className="form-note">
          {state.mode === 'global'
            ? <>当前：<strong>所有流量</strong>都走出站「{globalTarget}」。</>
            : <>当前：默认<strong>本机直连</strong>，只有下方规则里的域名走指定出站。</>}
        </p>
      </section>

      <section className="egress-block">
        <div className="egress-head">
          <div><strong>出站</strong><span>{outbounds.length} 个自定义出站</span></div>
          <button className="button button-compact button-quiet" disabled={readOnly || busy} onClick={() => setAdding(true)}><Plus size={13} />新增出站</button>
        </div>
        <div className="egress-list">
          <div className="egress-row">
            <div><strong>direct</strong><span className="egress-type">本机</span><small>节点自己的公网出口</small></div>
            <div className="egress-actions">
              {probes.direct && <code>{probes.direct}</code>}
              <button className="text-button" onClick={() => void probe('direct')}>测出口 IP</button>
            </div>
          </div>
          {outbounds.map((o) => (
            <div className="egress-row" key={o.name}>
              <div><strong>{o.name}</strong><span className="egress-type">{TYPE_LABEL[o.type] ?? o.type}</span><small>{describe(o)}</small></div>
              <div className="egress-actions">
                {probes[o.name] && <code>{probes[o.name]}</code>}
                <button className="text-button" onClick={() => void probe(o.name)}>测出口 IP</button>
                <button className="row-more" disabled={readOnly || busy} title="删除出站（同时删除引用它的分流规则）" aria-label={`删除出站 ${o.name}`}
                  onClick={async () => {
                    if (await askConfirm(`删除出站「${o.name}」？引用它的分流规则会一并删除。${state.mode === 'global' && o.name === globalTarget ? '\n它是当前的全局出口。' : ''}`)) {
                      void act({ action: 'delete', name: o.name }, `出站「${o.name}」已删除`)
                    }
                  }}><Trash2 size={14} /></button>
              </div>
            </div>
          ))}
        </div>
      </section>

      <section className="egress-block">
        <div className="egress-head">
          <div><strong>域名分流规则</strong><span>{rules.length} 条{state.mode === 'global' ? ' · 全局出口模式下这些规则不生效' : ''}</span></div>
        </div>
        <div className="egress-list">
          {rules.map((rule) => (
            <div className="egress-row" key={rule.domain}>
              <div><strong className="mono">{rule.domain}</strong><small><Route size={11} /> {rule.outbound}</small></div>
              <div className="egress-actions">
                <button className="row-more" disabled={readOnly || busy} aria-label={`删除规则 ${rule.domain}`}
                  onClick={() => void act({ action: 'rule-del', domain: rule.domain }, `规则「${rule.domain}」已删除`)}><Trash2 size={14} /></button>
              </div>
            </div>
          ))}
          {!rules.length && <p className="form-note">还没有分流规则。</p>}
        </div>
        {!readOnly && (
          <div className="egress-rule-form">
            <input value={ruleDomain} onChange={(e) => setRuleDomain(e.target.value)} placeholder="域名，如 openai.com" aria-label="域名" />
            <select value={ruleTarget || targets[0] || ''} onChange={(e) => setRuleTarget(e.target.value)} aria-label="走哪个出站">
              {targets.map((t) => <option key={t} value={t}>{t === 'warp_socks' ? 'WARP' : t}</option>)}
            </select>
            <button className="button button-compact button-primary" disabled={busy || !ruleDomain.trim() || !targets.length}
              onClick={() => void act({ action: 'rule-add', domain: ruleDomain.trim(), outbound: ruleTarget || targets[0] }, `规则「${ruleDomain.trim()}」已添加`).then((saved) => { if (saved) setRuleDomain('') })}>
              添加规则
            </button>
          </div>
        )}
      </section>

      {adding && <AddOutboundDialog onClose={() => setAdding(false)} onSave={async (params) => {
        if (await act({ action: 'save', ...params }, `出站「${params.name}」已保存`)) setAdding(false)
      }} busy={busy} />}
    </div>
  )
}

function AddOutboundDialog({ onClose, onSave, busy }: {
  onClose: () => void
  onSave: (params: Record<string, string>) => Promise<void>
  busy: boolean
}) {
  const [type, setType] = useState<'socks5' | 'http' | 'direct'>('socks5')
  const [name, setName] = useState('')
  const [addr, setAddr] = useState('')
  const [url, setUrl] = useState('')
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [insecure, setInsecure] = useState(false)
  const [mode, setMode] = useState('')

  const nameOk = /^[A-Za-z_][A-Za-z0-9_]{0,31}$/.test(name)
  const socksMatch = /^([^\s:]+):(\d{1,5})$/.exec(addr.trim())
  const socksPort = socksMatch ? Number(socksMatch[2]) : 0
  const socksOk = Boolean(socksMatch) && Number.isInteger(socksPort) && socksPort >= 1 && socksPort <= 65535
  const httpOk = (() => {
    try {
      const parsed = new URL(url.trim())
      return (parsed.protocol === 'http:' || parsed.protocol === 'https:') && Boolean(parsed.hostname)
    } catch {
      return false
    }
  })()
  const valid = nameOk && (type === 'socks5' ? socksOk : type === 'http' ? httpOk : true)

  function submit() {
    const params: Record<string, string> = { name, type }
    if (type === 'socks5') Object.assign(params, { addr, ...(username && { username }), ...(password && { password }) })
    if (type === 'http') Object.assign(params, { url, insecure: insecure ? 'true' : '' })
    if (type === 'direct' && mode) params.mode = mode
    void onSave(params)
  }

  return (
    <Modal title="新增出站" kicker="ADD OUTBOUND" onClose={onClose}>
      <div style={{ display: 'grid', gap: 12 }}>
        <div className="monitor-filter" role="group" aria-label="出站类型" style={{ justifySelf: 'start' }}>
          {(['socks5', 'http', 'direct'] as const).map((t) => <button key={t} className={type === t ? 'active' : ''} onClick={() => setType(t)}>{TYPE_LABEL[t]}</button>)}
        </div>
        <label className="form-field"><span>名称（字母、数字、下划线，如 us_exit）</span>
          <input value={name} onChange={(e) => setName(e.target.value)} placeholder="us_exit" aria-invalid={name !== '' && !nameOk} />
        </label>
        {type === 'socks5' && <>
          <label className="form-field"><span>地址 host:port</span><input value={addr} onChange={(e) => setAddr(e.target.value)} placeholder="1.2.3.4:1080" /></label>
          <label className="form-field"><span>用户名（可选）</span><input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="off" /></label>
          <label className="form-field"><span>密码（可选）</span><input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" /></label>
        </>}
        {type === 'http' && <>
          <label className="form-field"><span>代理地址</span><input value={url} onChange={(e) => setUrl(e.target.value)} placeholder="http://user:pass@5.6.7.8:3128" /></label>
          <label style={{ display: 'flex', gap: 8, alignItems: 'center' }}><input type="checkbox" checked={insecure} onChange={(e) => setInsecure(e.target.checked)} />跳过 HTTPS 证书校验</label>
          <p className="form-note">HTTP 出站不支持 UDP，走它的 UDP 流量（如 QUIC、部分游戏）会被拒绝；需要 UDP 请用 SOCKS5。</p>
        </>}
        {type === 'direct' && (
          <label className="form-field"><span>IP 优先级</span>
            <select value={mode} onChange={(e) => setMode(e.target.value)}>
              <option value="">默认（auto）</option><option value="4">仅 IPv4</option><option value="6">仅 IPv6</option>
              <option value="46">IPv4 优先</option><option value="64">IPv6 优先</option>
            </select>
          </label>
        )}
        <div className="modal-actions">
          <button className="button button-quiet" onClick={onClose}>取消</button>
          <button className="button button-primary" disabled={busy || !valid} onClick={submit}>{busy ? '保存中…' : '保存出站'}</button>
        </div>
      </div>
    </Modal>
  )
}
