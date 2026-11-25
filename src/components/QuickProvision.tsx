import { useCallback, useEffect, useMemo, useState } from 'react'
import { Check, ClipboardCopy, Loader2, Zap } from 'lucide-react'
import { api, loadSession } from '../lib/api'
import { copyToClipboard } from '../lib/clipboard'
import { useCopiedFlash } from '../lib/copiedFlash'
import type { Hy2Node, NodeUserCreated } from '../lib/api'
import { Modal } from './Modal'

/**
 * 总览页上的「快速开户」卡片。
 *
 * 存在的理由：原本给一个客户开户要「进 HY2 节点页 → 找到节点 → 开抽屉 →
 * 切到用户 tab → 点新建 → 填 5 个字段 → 创建 → 在结果弹窗里找链接 →
 * 再去别处拿另一种格式」，跨两个页面、十几次交互。
 *
 * 这里把它压成：选节点 → 填天数 → 点一下 → 结果弹窗里四种链接各带一个复制。
 *
 * 设计取舍：
 *   * 用户 ID 留空即由 portal 自动生成，不必每次想名字（后端支持留空）。
 *   * 没绑定 portal API 的节点不隐藏，而是置灰并写明原因 —— 静默消失会让人
 *     以为是 bug（这个项目在别处也是同一条原则：宁可不做，也不放会失败的按钮）。
 *   * 结果里的四种产物一次性给全，避免为「换一种格式」再跑一趟。
 */
export function QuickProvision({ nodes, notify, onOpenNodePage }: {
  nodes: Hy2Node[]
  notify: (message: string) => void
  onOpenNodePage: () => void
}) {
  // 只列已绑定 portal API 的节点：没有 api_key 就开不了户，列出来只会误导。
  const viewer = loadSession()?.role === 'viewer'
  const ready = useMemo(() => nodes.filter((n) => n.has_api), [nodes])
  const blocked = useMemo(() => nodes.filter((n) => !n.has_api), [nodes])

  const [nodeID, setNodeID] = useState('')
  const [days, setDays] = useState('30')
  const [trafficGB, setTrafficGB] = useState('0')
  const [ipLimit, setIPLimit] = useState('0')
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<NodeUserCreated | null>(null)
  const [error, setError] = useState('')

  // 默认选中第一个可用节点；节点列表变化时（比如刚绑完 API）自动跟上。
  useEffect(() => {
    if (ready.length === 0) {
      setNodeID('')
      return
    }
    if (!ready.some((n) => n.id === nodeID)) setNodeID(ready[0].id)
  }, [ready, nodeID])

  const current = ready.find((n) => n.id === nodeID)

  const submit = useCallback(async () => {
    if (!nodeID) return
    // 2026-11-03：原来写 `Number(x) || 默认值`，输错（如把 10 打成「1O」）时
    // 流量/IP 上限会被悄悄换成 0（不限），天数被换成 30，买家拿到的套餐和填的不一样。
    // 后端只收非负整数，这里先把不合法的输入拦下并说清楚。
    const parsed = [days, trafficGB, ipLimit].map((v) => (v.trim() === '' ? 0 : Number(v)))
    if (parsed.some((v) => !Number.isInteger(v) || v < 0)) {
      setError('有效期、流量配额与 IP 上限需为不小于 0 的整数（0 = 用节点默认 / 不限）')
      return
    }
    const [durationDays, trafficLimit, ipCap] = parsed
    setBusy(true)
    setError('')
    try {
      const created = await api.createNodeUser(nodeID, {
        // 留空 → portal 自己生成一个（如 hy2_xxxxxxxxxxxx）。
        user_id: undefined,
        duration_days: durationDays,
        traffic_gb: trafficLimit,
        ip_limit: ipCap,
        note: note.trim() || undefined,
      })
      setResult(created)
      notify(`已为 ${created.user_id} 开户`)
    } catch (err) {
      const message = err instanceof Error ? err.message : '开户失败'
      setError(message)
      notify(message)
    } finally {
      setBusy(false)
    }
  }, [nodeID, days, trafficGB, ipLimit, note, notify])

  const nothingReady = ready.length === 0

  return (
    <>
      <div className="panel provision-panel">
        <div className="section-head provision-head">
          <div>
            <div className="section-kicker">QUICK PROVISION <span>·</span> 客户开户</div>
            <h2>快速开户</h2>
          </div>
          <span className="range-select">
            {nothingReady ? '无可用节点' : `${ready.length} 个节点可开户`}
          </span>
        </div>

        {nothingReady ? (
          // 说清「为什么不能用」以及「怎么才能用」，而不是只显示一个空框。
          <div className="empty-state" style={{ minHeight: 110 }}>
            <Zap size={20} />
            <strong>还没有可开户的节点</strong>
            <span>
              开户需要节点侧 portal 的 API 密钥。请先到「HY2 节点」页登记节点并绑定 API。
            </span>
            <button className="button button-quiet button-compact" onClick={onOpenNodePage}>
              去 HY2 节点页
            </button>
          </div>
        ) : (
          <>
            <div className="provision-form">
              <label className="form-field">
                <span>节点</span>
                <select value={nodeID} onChange={(e) => setNodeID(e.target.value)}>
                  {ready.map((n) => (
                    <option key={n.id} value={n.id}>
                      {n.name} · {n.domain || n.api_url} · :{n.listen_port}
                    </option>
                  ))}
                </select>
              </label>
              <label className="form-field">
                <span>有效期（天）</span>
                <input value={days} onChange={(e) => setDays(e.target.value)} inputMode="numeric" />
              </label>
              <label className="form-field">
                <span>流量配额（GB，0=不限）</span>
                <input value={trafficGB} onChange={(e) => setTrafficGB(e.target.value)} inputMode="numeric" />
              </label>
              <label className="form-field">
                <span>IP 数限制（0=不限）</span>
                <input value={ipLimit} onChange={(e) => setIPLimit(e.target.value)} inputMode="numeric" />
              </label>
              <label className="form-field">
                <span>备注（可选）</span>
                <input
                  value={note}
                  onChange={(e) => setNote(e.target.value)}
                  placeholder="例如客户名或订单号"
                  spellCheck={false}
                />
              </label>
            </div>

            <div className="provision-actions">
              <span className="provision-hint">
                用户 ID 留空即由节点自动生成；开户后立刻给出四种订阅产物。
              </span>
              <button className="button button-primary" onClick={() => void submit()} disabled={viewer || busy || !nodeID} title={viewer ? '只读账号不能创建节点用户' : undefined}>
                {busy ? <Loader2 size={15} className="spin" /> : <Zap size={15} />}
                {busy ? '开户中…' : '一键开户'}
              </button>
            </div>

            {error && <div className="form-error">{error}</div>}

            {blocked.length > 0 && (
              // 显式列出被排除的节点及原因：静默少一个节点会被当成 bug。
              <div className="provision-blocked">
                {blocked.length} 个节点未绑定 portal API，无法开户：
                {blocked.map((n) => ` ${n.name}（未绑定 API）`).join('、')}
              </div>
            )}
          </>
        )}
      </div>

      {result && (
        <CreatedModal
          result={result}
          node={current}
          notify={notify}
          onClose={() => setResult(null)}
        />
      )}
    </>
  )
}

/** 开户结果弹窗：四种订阅产物各带一个复制按钮。 */
function CreatedModal({ result, node, notify, onClose }: {
  result: NodeUserCreated
  node?: Hy2Node
  notify: (message: string) => void
  onClose: () => void
}) {
  const [copied, flashCopied] = useCopiedFlash()

  async function copy(label: string, value: string) {
    if (!value) {
      notify(`${label} 为空，可能该节点未启用对应协议`)
      return
    }
    const ok = await copyToClipboard(value)
    if (ok) {
      flashCopied(label)
      notify(`已复制${label}`)
    } else {
      // 明文 HTTP 下浏览器不给剪贴板权限且降级失败时，给一条能自救的提示而不是静默失败。
      notify('复制失败：请在下方文本框里手动全选复制')
    }
  }

  const expires = result.expires_at
    ? new Date(result.expires_at * 1000).toLocaleString('zh-CN', { hour12: false })
    : '—'

  // 四种产物；reality_uri 只在节点真的给了内容时才出现（自签证书模式下常常没有）。
  const artifacts: Array<{ label: string; value: string; hint: string }> = [
    { label: 'Clash / mihomo', value: result.clash, hint: '导入 Clash Verge / mihomo' },
    { label: 'sing-box', value: result.sing_box, hint: '导入 sing-box / SFA / SFI' },
    { label: '通用 URI', value: result.uri, hint: 'v2rayN / v2rayNG / Shadowrocket' },
    { label: 'Reality URI', value: result.reality_uri, hint: '仅在节点启用 Reality 时有值' },
  ]

  return (
    <Modal title="开户成功" kicker="PROVISIONED" onClose={onClose} wide>
      <div className="created-summary">
        <div className="created-line">
          <span>用户 ID</span>
          <code>{result.user_id}</code>
          <button className="button button-quiet button-compact" onClick={() => void copy('用户 ID', result.user_id)}>
            {copied === '用户 ID' ? <Check size={13} /> : <ClipboardCopy size={13} />}复制
          </button>
        </div>
        <div className="created-line">
          <span>连接口令</span>
          <code>{result.password}</code>
          <button className="button button-quiet button-compact" onClick={() => void copy('口令', result.password)}>
            {copied === '口令' ? <Check size={13} /> : <ClipboardCopy size={13} />}复制
          </button>
        </div>
        <div className="created-line">
          <span>所属节点</span>
          <code>{node ? `${node.name} · ${node.domain || node.api_url} · :${node.listen_port}` : '—'}</code>
        </div>
        <div className="created-line">
          <span>到期时间</span>
          <code>{expires}</code>
        </div>
        <div className="created-line">
          <span>配额</span>
          <code>
            {result.traffic_gb > 0 ? `${result.traffic_gb} GB` : '不限流量'} ·{' '}
            {result.ip_limit > 0 ? `${result.ip_limit} 个 IP` : '不限 IP'}
          </code>
        </div>
      </div>

      <div className="created-artifacts">
        <div className="section-kicker">订阅产物 <span>·</span> 复制任意一种交给客户</div>
        {artifacts.map((a) => (
          <div className="artifact-row" key={a.label}>
            <div className="artifact-head">
              <strong>{a.label}</strong>
              <span>{a.hint}</span>
              <button
                className="button button-quiet button-compact"
                onClick={() => void copy(a.label, a.value)}
                disabled={!a.value}
              >
                {copied === a.label ? <Check size={13} /> : <ClipboardCopy size={13} />}复制
              </button>
            </div>
            <textarea readOnly value={a.value || '（该节点未返回此格式）'} rows={a.label === '通用 URI' || a.label === 'Reality URI' ? 2 : 5} />
          </div>
        ))}
      </div>

      <div className="modal-actions">
        <span className="provision-hint">
          口令与订阅链接只在这里出现一次，请先复制再关闭。
        </span>
        <button className="button button-primary" onClick={onClose}>完成</button>
      </div>
    </Modal>
  )
}
