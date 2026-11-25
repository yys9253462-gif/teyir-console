import { useCallback, useEffect, useState, useRef } from 'react'
import { AlertTriangle, Check, Copy, ExternalLink, Loader2, Power, RefreshCw, ShieldAlert } from 'lucide-react'
import { askConfirm } from './ConfirmHost'
import { api, isAdminSession, type SubscriptionFormat, type SubscriptionState } from '../lib/api'
import { copyToClipboard } from '../lib/clipboard'
import { useCopiedFlash } from '../lib/copiedFlash'
import { safeHttpUrl } from '../lib/safeUrl'

// SubscriptionPanel exposes the merged subscription: one link carrying every
// registered node, so a client imports it once and gets the whole fleet.
//
// Two things about it are deliberately loud rather than buried, because they are
// consequences of the chosen credential model:
//
//  1. The link carries each node's *master* password. Anyone holding it has full
//     access to every node, so it is treated as a credential: rotatable, and
//     disabled-able, with the warning shown before the link rather than after.
//  2. Clients importing it share one identity per node, so per-user accounting on
//     a node merges across them.

const FORMAT_META: Record<SubscriptionFormat, { label: string; hint: string }> = {
  clash: { label: 'Clash / mihomo', hint: '含 PROXY 手动选择组与 AUTO 自动测速组' },
  singbox: { label: 'sing-box', hint: 'outbounds：selector + urltest，可直接导入' },
  uri: { label: '通用（base64）', hint: 'v2rayN / v2rayNG 等认的 URI 列表' },
}

const FORMAT_ORDER: SubscriptionFormat[] = ['clash', 'singbox', 'uri']

export function SubscriptionPanel({ notify }: { notify: (message: string) => void }) {
  const [state, setState] = useState<SubscriptionState | null>(null)
  const [note, setNote] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [busy, setBusy] = useState(false)
  const [rotating, setRotating] = useState(false)
  const [copied, flashCopied] = useCopiedFlash()
  const [format, setFormat] = useState<SubscriptionFormat>('clash')
  const [preview, setPreview] = useState('')
  const previewRequest = useRef(0)
  const [previewLoading, setPreviewLoading] = useState(false)

  const loadSeq = useRef(0)
  const mountedRef = useRef(true)
  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      loadSeq.current += 1
    }
  }, [])

  const load = useCallback(async () => {
    const seq = ++loadSeq.current
    setLoading(true)
    try {
      const next = await api.subscriptionState()
      if (!mountedRef.current || seq !== loadSeq.current) return
      setState(next)
      setNote(null)
    } catch (err) {
      if (!mountedRef.current || seq !== loadSeq.current) return
      setState(null)
      setNote(err instanceof Error ? `读取订阅失败：${err.message}` : '读取订阅失败')
    } finally {
      if (mountedRef.current && seq === loadSeq.current) setLoading(false)
    }
  }, [])

  useEffect(() => { void load() }, [load])

  // 2026-11-06：预览失败不能写进 preview —— 那个字段会被「复制内容」按钮当成配置复制走，
  // 于是操作者复制到的是「生成预览失败：需要管理员角色 (403)」，还 toast「已复制配置内容」。
  // 另外预览接口是 requireAdmin，非管理员根本不该发这个请求。
  const [previewError, setPreviewError] = useState('')
  const canPreview = isAdminSession()
  const loadPreview = useCallback(async (target: SubscriptionFormat) => {
    if (!isAdminSession()) {
      setPreview('')
      setPreviewError('')
      return
    }
    const requestId = ++previewRequest.current
    setPreviewLoading(true)
    setPreviewError('')
    try {
      const value = await api.subscriptionPreview(target)
      if (requestId === previewRequest.current) { setPreview(value); setPreviewError('') }
    } catch (err) {
      if (requestId === previewRequest.current) {
        setPreview('')
        setPreviewError(err instanceof Error ? err.message : '生成预览失败')
      }
    } finally {
      if (requestId === previewRequest.current) setPreviewLoading(false)
    }
  }, [])

  useEffect(() => { void loadPreview(format); return () => { previewRequest.current++ } }, [format, loadPreview])

  async function copy(label: string, value: string) {
    if (!value) return
    const ok = await copyToClipboard(value)
    if (ok) {
      flashCopied(label)
      notify(`已复制${label}`)
    } else {
      notify('复制失败，请手动选中复制')
    }
  }

  async function rotate() {
    setBusy(true)
    try {
      await api.rotateSubscription()
      // 2026-11-12：换新只换链接令牌，各节点上的账号口令不变。原文案「旧链接立即失效」会让人以为外流的配置也失效了。
      notify('已换新订阅链接，旧链接不能再拉取；已导入的配置仍可连接')
      if (mountedRef.current) setRotating(false)
      await load()
      // 2026-11-06：预览框里还是旧 token 生成的配置（引用旧链接）。不刷新的话，
      // 运维点「复制内容」复制走的是刚失效的那条链接 —— 客户导入后 404。
      if (mountedRef.current) await loadPreview(format)
    } catch (err) {
      notify(err instanceof Error ? `换新失败：${err.message}` : '换新失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  async function toggleEnabled() {
    if (!state) return
    // 2026-11-06：停用先确认。停用会在所有节点上停掉这条合并订阅的账号，已连接的客户端立刻断开；原来一点即生效。
    if (state.enabled && !await askConfirm(`停用合并订阅？

所有节点上的这条订阅账号会被停用，已连接的客户端立刻断开，客户端也拉不到新配置。`, { confirmText: '停用', danger: true })) return
    setBusy(true)
    try {
      const next = !state.enabled
      const res = await api.updateSubscription({ enabled: next })
      // 停用/启用会同步到各节点账号；有节点没切成功时，那台节点上的客户端仍可用，必须说出来。
      if (res.node_state_ok === false && res.node_state_message) {
        notify(`⚠ ${next ? '启用' : '停用'}不完整 —— ${res.node_state_message}`)
      } else {
        notify(next ? '订阅已启用' : '订阅已停用，客户端将拉不到配置、已连接的客户端会断开')
      }
      await load()
    } catch (err) {
      notify(err instanceof Error ? `操作失败：${err.message}` : '操作失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  return (
    <div className="service-panel">
      <div className="service-panel-head">
        <div>
          <div className="section-kicker">MERGED SUBSCRIPTION</div>
          <strong>合并订阅</strong>
        </div>
        <button className="button button-quiet" onClick={() => void load()} disabled={loading}>
          {loading ? <Loader2 size={14} className="spin" /> : <RefreshCw size={14} />}
          刷新
        </button>
      </div>

      {note && <p className="form-note service-warn"><AlertTriangle size={13} /><span>{note}</span></p>}

      {state && (
        <>
          {/* 正文必须包在一个 span 里：.service-warn 是 flex 容器，若正文中夹着
              <strong> 这类行内元素，每一段连续文本都会各自变成一个 flex item，
              结果是几段文字并排错位、互相叠字。 */}
          <p className="form-note service-warn">
            <ShieldAlert size={13} />
            <span>
              这条链接内含各节点的<strong>主口令</strong>，等同于全部节点的通行证：谁拿到谁就能用。
              只发给你自己或信任的人。「换新链接」只阻止再次拉取，已导入的配置仍可连接；要让外流的配置连不上，请停用合并订阅。另外，导入它的客户端在每台节点上算<strong>同一个账号</strong>，
              节点侧的按用户流量记账会合并到一起。
            </span>
          </p>

          <div className="sub-link-row">
            <input className="sub-link-input" value={state.url} readOnly onFocus={(event) => event.currentTarget.select()} spellCheck={false} />
            <button className="button button-primary button-compact" onClick={() => void copy('订阅链接', state.url)}>
              {copied === '订阅链接' ? <Check size={13} /> : <Copy size={13} />}复制
            </button>
            {safeHttpUrl(state.url) && (
              <a className="button button-quiet button-compact" href={safeHttpUrl(state.url)} target="_blank" rel="noreferrer noopener">
                <ExternalLink size={13} />打开
              </a>
            )}
          </div>

          <div className="sub-meta-row">
            <span className={`status-pill ${state.enabled ? 'online' : 'offline'}`}>{state.enabled ? '已启用' : '已停用'}</span>
            <span className="service-badge-muted">客户端拉取 {state.access_count} 次</span>
            <span className="service-badge-muted">
              最近 {state.last_accessed_at ? new Date(state.last_accessed_at).toLocaleString('zh-CN', { hour12: false }) : '从未'}
            </span>
            <span className="sub-meta-actions">
              <button className="button button-quiet button-compact" disabled={busy || !isAdminSession()} title={isAdminSession() ? undefined : '仅管理员可停用或启用'} onClick={() => void toggleEnabled()}>
                <Power size={13} />{state.enabled ? '停用' : '启用'}
              </button>
              <button className={`button button-compact ${rotating ? 'button-danger' : 'button-quiet'}`} disabled={busy || !isAdminSession()} title={isAdminSession() ? undefined : '仅管理员可换新链接'} onClick={() => (rotating ? void rotate() : setRotating(true))} onBlur={() => setRotating(false)}>
                <RefreshCw size={13} />{rotating ? '确认换新?' : '换新链接'}
              </button>
            </span>
          </div>

          <div className="sub-counts">
            <span className="status-pill online">已包含 {(state.included ?? []).length} 个节点</span>
            {(state.excluded ?? []).length > 0 && <span className="status-pill warning">排除 {(state.excluded ?? []).length} 个</span>}
          </div>

          {(state.included ?? []).length > 0 && (
            <div className="sub-node-list">
              {(state.included ?? []).map((node) => (
                <span className="sub-node-chip" key={node.id} title={`${node.host}:${node.port} · SNI ${node.sni}`}>
                  {node.name}
                  <small>{node.host}:{node.port}</small>
                </span>
              ))}
            </div>
          )}

          {(state.excluded ?? []).length > 0 && (
            <div className="sub-excluded">
              <strong>未纳入的节点</strong>
              {(state.excluded ?? []).map((node) => (
                <div className="sub-excluded-row" key={node.node_id}>
                  <span>{node.name}</span>
                  <small className="service-error">{node.reason}</small>
                </div>
              ))}
            </div>
          )}

          <div className="sub-format-tabs">
            {FORMAT_ORDER.map((item) => (
              <button
                key={item}
                className={`button button-compact ${format === item ? 'button-primary' : 'button-quiet'}`}
                onClick={() => setFormat(item)}
              >
                {FORMAT_META[item].label}
              </button>
            ))}
            <button className="button button-quiet button-compact" onClick={() => void loadPreview(format)} disabled={previewLoading || !canPreview}
              title={canPreview ? undefined : '预览内容含各节点主口令，仅管理员可生成'}>
              {previewLoading ? <Loader2 size={13} className="spin" /> : <RefreshCw size={13} />}重新生成
            </button>
            <button className="button button-quiet button-compact" disabled={!preview || Boolean(previewError)}
              title={previewError ? '预览没有生成成功，没有可复制的内容' : undefined}
              onClick={() => void copy('配置内容', preview)}>{copied === '配置内容' ? '已复制' : '复制内容'}</button>
          </div>

          <p className="form-note">{FORMAT_META[format].hint}。客户端一般按 User-Agent 自动匹配格式；也可以加 <code>?format=clash|singbox|uri</code> 强制指定。</p>
          {previewError
            ? <div className="drawer-notice warn"><AlertTriangle size={17} /><span><strong>预览没有生成</strong><small>{previewError}</small></span></div>
            : <pre className="sub-preview">{previewLoading ? '生成中…' : preview || (canPreview ? '' : '预览含各节点主口令，仅管理员可查看。')}</pre>}
        </>
      )}
    </div>
  )
}
