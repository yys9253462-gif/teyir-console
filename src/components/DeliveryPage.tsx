import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  AlertTriangle, CalendarClock, Check, Copy, Download, ExternalLink, Loader2,
  MonitorSmartphone, Network, QrCode as QrCodeIcon, RefreshCw, ShieldCheck, ShoppingCart,
} from 'lucide-react'
import { api, type DeliveryView, type SubscriptionIncludedNode } from '../lib/api'
import { copyToClipboard } from '../lib/clipboard'
import { useCopiedFlash } from '../lib/copiedFlash'
import { safeHttpUrl } from '../lib/safeUrl'
import qrcode from 'qrcode-generator'
import { RenewDialog, renewTargetFrom } from './RenewDialog'

// DeliveryPage is what a *customer* sees after buying from the shop. It is
// addressed by token alone (`/user/<token>`) and renders without any session —
// the token is the credential.
//
// Two rules follow from that and are enforced structurally, not by convention:
//
//  1. Nothing here is a capacity we invented. Every number shown comes from the
//     single `/api/sub/view/{token}` response; a value the server did not send
//     (e.g. a node list that failed to build) renders as an explicit "—"/empty
//     state rather than a plausible-looking zero.
//  2. When `expired` is true the server omits `subscription_url` / `clash_url` /
//     `singbox_url` / `uri_url` entirely. The page therefore must not render a
//     link row at all in that state, and must not fall back to a cached URL.
//     An expired customer gets a banner and the renewal button, nothing else.

const FORMATS = [
  { key: 'clash', label: 'Clash / mihomo', file: 'clash.yaml', hint: '导入 Clash Verge、mihomo、Stash 等' },
  { key: 'singbox', label: 'sing-box', file: 'singbox.json', hint: '导入 sing-box / SFA / SFI' },
  { key: 'uri', label: '通用订阅', file: 'subscription.txt', hint: 'v2rayN、v2rayNG、Shadowrocket 等' },
] as const

type FormatKey = (typeof FORMATS)[number]['key']

// 回退续费地址：优先使用后端下发的 renew_url；仅当部署时显式提供 VITE_RENEW_URL 才作为兜底外链。
const RENEW_URL = safeHttpUrl(import.meta.env.VITE_RENEW_URL?.trim())

function formatDateTime(epochSeconds: number): string {
  if (!epochSeconds || epochSeconds <= 0) return '永久有效'
  return new Date(epochSeconds * 1000).toLocaleString('zh-CN', { hour12: false })
}

// 二维码在本地生成 SVG（不把订阅链接发给第三方图片服务）。
// 2026-10-16：原来手写的编码器生成的码无法被解码（OpenCV 实测失败），
// 换成成熟的 qrcode-generator；并按规范留出 4 格白边（quiet zone），
// 否则放在有底色的卡片上，手机扫码器找不到定位角。
function qrMatrix(text: string): boolean[][] | null {
  try {
    const qr = qrcode(0, 'M')
    qr.addData(text, 'Byte')
    qr.make()
    const n = qr.getModuleCount()
    return Array.from({ length: n }, (_, r) => Array.from({ length: n }, (_, c) => qr.isDark(r, c)))
  } catch {
    return null
  }
}

function QrCode({ value, size = 168 }: { value: string; size?: number }) {
  // 每次渲染都重新编码二维码很费 CPU（链接不变时完全没必要）。
  const matrix = useMemo(() => qrMatrix(value), [value])
  if (!matrix) {
    // Never silently render a broken image: say so.
    return <p className="form-note">二维码生成失败，请直接复制上方链接。</p>
  }
  // 以「格」为单位画，viewBox 里留 4 格白边；整数坐标 + crispEdges 避免缩放后格子之间出现细缝。
  const quiet = 4
  const count = matrix.length
  const total = count + quiet * 2
  const path: string[] = []
  for (let r = 0; r < count; r += 1) {
    for (let c = 0; c < count; c += 1) {
      if (matrix[r][c]) path.push(`M${c + quiet} ${r + quiet}h1v1h-1z`)
    }
  }
  return (
    <svg width={size} height={size} viewBox={`0 0 ${total} ${total}`} shapeRendering="crispEdges" role="img" aria-label="订阅链接二维码">
      <rect width={total} height={total} fill="#ffffff" />
      <path d={path.join('')} fill="#000000" />
    </svg>
  )
}

export function DeliveryPage({ token }: { token: string }) {
  const [view, setView] = useState<DeliveryView | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [copied, flashCopied] = useCopiedFlash()
  const [format, setFormat] = useState<FormatKey>('clash')

  const [gone, setGone] = useState(false)
  const [refreshing, setRefreshing] = useState(false)
  const [copyTip, setCopyTip] = useState('')
  const loaded = useRef(false)
  const loadSeq = useRef(0)
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => { mounted.current = false }
  }, [])

  const load = useCallback(async () => {
    const seq = ++loadSeq.current
    // 只有第一次加载用整页转圈。刷新失败（网络抖动、后端重启）时保留已显示的内容，
    // 不能把付费客户的页面换成「订阅已被回收」——只有后端明确 404 才这样说。
    if (!loaded.current) setLoading(true)
    else setRefreshing(true)
    try {
      const next = await api.deliveryView(token)
      if (!mounted.current || seq !== loadSeq.current) return
      setView(next)
      loaded.current = true
      setError(null)
      setGone(false)
    } catch (err) {
      if (!mounted.current || seq !== loadSeq.current) return
      const status = (err as { status?: number }).status
      if (status === 404) { setView(null); setGone(true) }
      setError(err instanceof Error ? err.message : '读取订阅失败')
    } finally {
      if (mounted.current && seq === loadSeq.current) {
        setLoading(false)
        setRefreshing(false)
      }
    }
  }, [token])

  useEffect(() => {
    loaded.current = false
    setView(null)
    setError(null)
    setGone(false)
    setRenewOpen(false)
    void load()
  }, [load])
  // 2026-11-06：暂时读不到（503：服务重启、节点开户中）时每 60 秒自动重试一次，直到读到或确认不存在。
  // 客户多半不会想到去点「重试」，而开户通常一两分钟内就完成。
  const [retryTick, setRetryTick] = useState(0)
  useEffect(() => {
    if (view || gone || !error) return
    const t = window.setTimeout(() => {
      if (!document.hidden) void load()
      setRetryTick((n) => n + 1) // 无论这一轮是否发请求，都排下一轮（标签页在后台时只是跳过）
    }, 60_000)
    return () => window.clearTimeout(t)
  }, [view, gone, error, load, retryTick])

  async function copy(label: string, value: string) {
    if (!value) return
    const ok = await copyToClipboard(value)
    if (ok) {
      setCopyTip('')
      flashCopied(label)
    } else {
      // 禁用原生弹窗阻塞，改为页内温和提示，方便买家长按文本框手动复制。
      setCopyTip(`当前浏览器限制自动复制「${label}」，请点击下方链接输入框全选手动复制。`)
    }
  }

  const links: Record<FormatKey, string | undefined> = {
    clash: view?.clash_url,
    singbox: view?.singbox_url,
    uri: view?.uri_url,
  }

  // 二维码线路：扫码导入后客户端会一直用这个地址更新，所以默认放最抗封的线路。
  // 2026-10-28 起交付域名本身就是走 CDN 的域名，主线路排第一并默认选中，备用线路排在后面。
  // 备用链接只给了通用地址，这里把 uri_url 的协议与域名同步替换为备用地址得到同一令牌的通用订阅地址。
  const qrLines = useMemo(() => {
    const lines: { label: string; url: string }[] = []
    const main = view?.uri_url
    if (!main) return lines
    lines.push({ label: '主线路', url: main })
    ;(view?.backup_subscription_urls ?? []).forEach((backup, index) => {
      try {
        const url = new URL(main)
        const parsedBackup = new URL(backup)
        url.protocol = parsedBackup.protocol
        url.host = parsedBackup.host
        lines.push({ label: `备用线路${index + 1}`, url: url.href })
      } catch { /* 格式不对的备用地址直接跳过 */ }
    })
    return lines
  }, [view?.uri_url, view?.backup_subscription_urls])
  const [qrIndex, setQrIndex] = useState(0)
  // 页内弹窗续费（见 RenewDialog.tsx）；非商城订单拿不到订单号，仍用原来的外链。
  const [renewOpen, setRenewOpen] = useState(false)
  // 外部续费地址（后端 STORE_RENEW_BASE_URL 下发的 renew_url 或构建期 VITE_RENEW_URL）；
  // 两者皆空说明当前站点未接入外部商城续费，禁止强行构造页内续费目标导致点击报 404/解析异常。
  const fallbackRenewUrl = safeHttpUrl(view?.renew_url) ?? RENEW_URL
  // 商城订单按原订单续；手工开通且站点已配置外部商城续费入口（非永久）时，以订阅令牌接入页内续费。
  const renewTarget = useMemo(
    () => renewTargetFrom(view?.renew_url) ?? (fallbackRenewUrl && (view?.expires_at ?? 0) > 0 ? { order: '', sub: token } : null),
    [view?.renew_url, view?.expires_at, fallbackRenewUrl, token],
  )
  const handleRenewed = useCallback(() => { void load() }, [load])
  // ⚠️ 2026-10-30：线路数会变（后台改了 SUBSCRIPTION_BACKUP_BASE_URLS 后刷新，
  // 或订阅被暂停后 view 变空），而 qrIndex 是上一次选择的残留值。
  // 越界时 `qrLines[qrIndex]` 是 undefined：二维码变成空白、且没有任何标签
  // 处于选中态 —— 页面上看不出"为什么空"，客户只会以为码坏了。
  // 这里把越界值钳回第一条，而不是依赖调用方每次都重置。
  const safeQrIndex = qrIndex < qrLines.length ? qrIndex : 0

  // Go 把空切片序列化成 null；直接 .length 会让客户看到白屏。
  const nodes = view?.nodes ?? []
  const excluded = view?.excluded ?? []

  if (loading) {
    return (
      <div className="delivery-shell"><div className="delivery-card delivery-centered">
        <Loader2 size={22} className="spin" /><p className="form-note">正在读取订阅…</p>
      </div></div>
    )
  }

  if (!view) {
    return (
      <div className="delivery-shell"><div className="delivery-card delivery-centered">
        <AlertTriangle size={26} className="delivery-error-icon" />
        <h1 className="delivery-title">无法打开这个订阅</h1>
        <p className="form-note">{gone ? '订阅不存在或已被删除。' : (error ?? '暂时读取不到订阅信息。')}</p>
        {gone ? (
          <p className="form-note">
            最可能的原因有两个：一是这个订阅已到期较久、已按约自动回收；
            二是链接被聊天工具截断或不完整。请先确认链接是完整复制的一整条。
          </p>
        ) : (
          <p className="form-note">可能是网络不稳定或服务正在重启，订阅本身不受影响，请稍后点「重试」。</p>
        )}
        <div className="delivery-actions">
          <button className={gone ? 'button button-quiet' : 'button button-primary'} onClick={() => void load()}><RefreshCw size={14} />重试</button>
          {/* 2026-11-06：只有订阅确实不存在时才给「重新购买」。暂时读不到（服务重启、节点开户中）时摆着它，
              付费客户会以为订阅没了要重新买。 */}
          {gone && RENEW_URL && <a className="button button-primary" href={RENEW_URL} target="_blank" rel="noreferrer noopener">
            <ShoppingCart size={14} />重新购买
          </a>}
        </div>
      </div></div>
    )
  }

  const deviceText = view.ip_limit > 0
    ? `${view.active_ip_count} / ${view.ip_limit} 个网络`
    : `${view.active_ip_count} 个网络（不限）`

  return (
    <div className="delivery-shell">
      <div className="delivery-card">
        <header className="delivery-head">
          <div className="delivery-brand">
            <div className="brand-mark"><span /><span /><span /></div>
            <div>
              <div className="section-kicker">SUBSCRIPTION DELIVERY</div>
              <strong>{view.label || '我的订阅'}</strong>
            </div>
          </div>
          <div className="delivery-head-actions">
            {renewTarget && (
              <button className="button button-primary button-compact" onClick={() => setRenewOpen(true)}><ShoppingCart size={13} />续费</button>
            )}
            <button className="button button-quiet button-compact" disabled={refreshing} onClick={() => void load()}>
              <RefreshCw size={13} className={refreshing ? 'spin' : undefined} />{refreshing ? '刷新中…' : '刷新'}
            </button>
          </div>
        </header>

        {copyTip && (
          <div className="delivery-banner delivery-banner-warn" role="status">
            <AlertTriangle size={16} />
            <span><strong>请手动复制</strong>{copyTip}</span>
          </div>
        )}

        {/* 2026-11-04：已有内容时刷新失败原来什么都不显示（error 只在整页失败分支里用）——
            客户点「我已续费，刷新」碰上后端重启或限流，页面安静地停在「已到期」，以为续费没生效。 */}
        {error && (
          <div className="delivery-banner delivery-banner-warn">
            <AlertTriangle size={16} />
            <span><strong>刷新失败</strong>页面显示的是上次读取到的内容：{error}。请稍后再点「刷新」。</span>
          </div>
        )}
        {view.expired && (
          <div className="delivery-banner delivery-banner-danger">
            <AlertTriangle size={16} />
            <span>
              <strong>订阅已到期</strong>
              链接已停止服务（到期时间 {formatDateTime(view.expires_at)}）。
              续费后本页链接不变、配置自动恢复，无需重新导入。
            </span>
          </div>
        )}
        {!view.expired && view.enabled && view.days_left >= 0 && view.days_left <= 3 && (
          <div className="delivery-banner delivery-banner-warn">
            <CalendarClock size={16} />
            <span><strong>即将到期</strong>还剩 {view.days_left} 天（{formatDateTime(view.expires_at)}），建议提前续费。</span>
          </div>
        )}
        {!view.expired && view.enabled && (view.device_limit_blocked ?? 0) > 0 && (
          <div className="delivery-banner delivery-banner-warn">
            <AlertTriangle size={16} />
            <span>
              <strong>检测到超出设备数</strong>
              同时在线的网络超过 {view.ip_limit} 个，多出的 {view.device_limit_blocked} 个网络已被暂时拦截。
              断开不用的设备，约 3 分钟后名额会自动释放。
            </span>
          </div>
        )}
        {!view.expired && !view.enabled && (
          <div className="delivery-banner delivery-banner-warn">
            <AlertTriangle size={16} />
            <span><strong>订阅已被暂停</strong>请联系客服或续费后恢复。</span>
          </div>
        )}

        <div className="delivery-facts">
          <div className="delivery-fact">
            <span>状态</span>
            <strong className={`status-pill ${view.expired || !view.enabled ? 'offline' : 'online'}`}>
              {view.expired ? '已到期' : view.enabled ? '正常' : '已暂停'}
            </strong>
          </div>
          <div className="delivery-fact">
            <span>到期时间</span>
            <strong>{formatDateTime(view.expires_at)}</strong>
            {!view.expired && view.days_left >= 0 && <small>剩余 {view.days_left} 天</small>}
          </div>
          <div className="delivery-fact">
            <span>同时在线网络</span>
            <strong>{deviceText}</strong>
            <small>{view.ip_window_hours} 小时内活跃地址</small>
          </div>
          <div className="delivery-fact">
            <span>可用节点</span>
            <strong>{view.node_count} 个</strong>
            <small>合并订阅，一次导入全部</small>
          </div>
        </div>

        {view.expired || !view.enabled ? (
          // `!view.enabled` must be handled here too, not just `expired`.
          // A paused subscription is served without any URL on purpose (the
          // server omits subscription_url / clash_url / singbox_url / uri_url
          // when enabled is false). Rendering the normal block anyway produced
          // an empty QR code, empty link inputs and buttons pointing at
          // `undefined`, and it also leaked the node list — a page that says
          // "paused" at the top while showing a full delivery panel below.
          <div className="delivery-locked">
            <AlertTriangle size={18} />
            <div>
              <strong>订阅链接已锁定</strong>
              <p className="form-note">
                {view.expired
                  ? '到期后服务器不再下发配置，因此本页也不显示链接 —— 这是正常的，不是出错。完成续费后刷新本页即可看到全部链接。'
                  : '订阅被暂停期间服务器不下发配置，因此本页也不显示链接 —— 这是正常的，不是出错。恢复订阅后刷新本页即可看到全部链接。'}
              </p>
            </div>
          </div>
        ) : (
          <>
            <section className="delivery-section">
              <div className="delivery-section-head">
                <QrCodeIcon size={15} />
                <strong>扫码导入</strong>
                <span className="form-note">手机端用客户端扫这个二维码即可</span>
              </div>
              <div className="delivery-qr">
                {qrLines.length > 1 && (
                  <div className="sub-format-tabs">
                    {qrLines.map((line, index) => (
                      <button key={line.url} className={`button button-compact ${safeQrIndex === index ? 'button-primary' : 'button-quiet'}`} onClick={() => setQrIndex(index)}>
                        {line.label}
                      </button>
                    ))}
                  </div>
                )}
                {qrLines.length > 0 ? (
                  <QrCode value={qrLines[safeQrIndex]?.url ?? ''} />
                ) : (
                  <p className="form-note">当前暂无可生成二维码的订阅地址，请刷新或联系管理员。</p>
                )}
                <p className="form-note">
                  二维码内容为「通用订阅」地址，任何支持订阅的客户端都能扫。
                  {qrLines.length > 1 && '默认用主线路；它更新不了时，再换下面的备用线路重新扫码。'}
                </p>
              </div>
            </section>

            <section className="delivery-section">
              <div className="delivery-section-head">
                <Network size={15} />
                <strong>订阅链接</strong>
              </div>
              <div className="sub-link-row">
                <input className="sub-link-input" value={view.subscription_url ?? ''} readOnly onFocus={(event) => event.currentTarget.select()} spellCheck={false} />
                <button className="button button-primary button-compact" disabled={!view.subscription_url} onClick={() => void copy('订阅链接', view.subscription_url ?? '')}>
                  {copied === '订阅链接' ? <Check size={13} /> : <Copy size={13} />}复制
                </button>
                {safeHttpUrl(view.subscription_url) && (
                  <a className="button button-quiet button-compact" href={safeHttpUrl(view.subscription_url)} target="_blank" rel="noreferrer noopener"><ExternalLink size={13} />打开</a>
                )}
              </div>
              <p className="form-note">这一条就是全部：把它粘进客户端即可，之后节点有变动会自动同步，不用重新导入。</p>
              {(view.backup_subscription_urls ?? []).length > 0 && (
                <>
                  <p className="form-note">更新订阅失败（主域名被墙）时，换用下面任意一条备用链接，内容完全相同：</p>
                  {(view.backup_subscription_urls ?? []).map((url, index) => {
                    const key = `备用链接${index + 1}`
                    return (
                      <div className="sub-link-row" key={url}>
                        <input className="sub-link-input" value={url} readOnly onFocus={(event) => event.currentTarget.select()} spellCheck={false} aria-label={key} />
                        <button className="button button-quiet button-compact" onClick={() => void copy(key, url)}>
                          {copied === key ? <Check size={13} /> : <Copy size={13} />}复制备用{index + 1}
                        </button>
                      </div>
                    )
                  })}
                </>
              )}
            </section>

            <section className="delivery-section">
              <div className="delivery-section-head">
                <Download size={15} />
                <strong>按客户端下载</strong>
              </div>
              <div className="sub-format-tabs">
                {FORMATS.map((item) => (
                  <button key={item.key} className={`button button-compact ${format === item.key ? 'button-primary' : 'button-quiet'}`} onClick={() => setFormat(item.key)}>
                    {item.label}
                  </button>
                ))}
              </div>
              <div className="sub-link-row">
                <input className="sub-link-input" value={links[format] ?? ''} readOnly onFocus={(event) => event.currentTarget.select()} spellCheck={false} />
                <button className="button button-primary button-compact" disabled={!links[format]} onClick={() => void copy(FORMATS.find((item) => item.key === format)?.label ?? '', links[format] ?? '')}>
                  {copied === FORMATS.find((item) => item.key === format)?.label ? <Check size={13} /> : <Copy size={13} />}复制
                </button>
                {safeHttpUrl(links[format]) && (
                  <a className="button button-quiet button-compact" href={safeHttpUrl(links[format])} download={FORMATS.find((item) => item.key === format)?.file}>
                    <Download size={13} />下载
                  </a>
                )}
              </div>
              <p className="form-note">{FORMATS.find((item) => item.key === format)?.hint}。同一个地址按 <code>?format=</code> 区分格式，客户端一般会按 UA 自动匹配。</p>
            </section>

            <section className="delivery-section">
              <div className="delivery-section-head">
                <Network size={15} />
                <strong>包含的节点</strong>
                <span className="form-note">共 {view.node_count} 个</span>
              </div>
              {nodes.length === 0 ? (
                <p className="form-note">当前没有可用节点，通常是所有节点同时离线。请稍后刷新，或联系客服。</p>
              ) : (
                <div className="sub-node-list">
                  {nodes.map((node: SubscriptionIncludedNode) => (
                    <span className="sub-node-chip" key={node.id} title={`${node.host}:${node.port}`}>
                      {node.name}<small>{node.host}:{node.port}</small>
                    </span>
                  ))}
                </div>
              )}
              {excluded.length > 0 && (
                <p className="form-note">另有 {excluded.length} 个节点暂时不可用，已在配置中自动跳过；它们恢复后会自动出现在你的订阅里。</p>
              )}
            </section>
          </>
        )}

        <section className="delivery-section">
          <div className="delivery-section-head">
            <ShoppingCart size={15} />
            <strong>续费</strong>
          </div>
          <p className="form-note">
            {renewTarget || fallbackRenewUrl
              ? '续费后本页链接保持不变，客户端无需任何改动。'
              : '当前站点未配置在线自助续费入口，如需续费请联系客服或管理员；续费完成后点击下方刷新即可，链接与配置保持不变。'}
          </p>
          <div className="delivery-actions">
            {renewTarget ? (
              <button className="button button-primary" onClick={() => setRenewOpen(true)}><ShoppingCart size={15} />前往续费</button>
            ) : fallbackRenewUrl ? (
              <a className="button button-primary" href={fallbackRenewUrl} target="_blank" rel="noreferrer noopener"><ShoppingCart size={15} />前往续费</a>
            ) : null}
            <button className="button button-quiet" disabled={refreshing} onClick={() => void load()}>
              <RefreshCw size={15} className={refreshing ? 'spin' : undefined} />{refreshing ? '正在刷新…' : '我已续费，刷新'}
            </button>
          </div>
          {renewOpen && renewTarget && (
            <RenewDialog target={renewTarget} fallbackUrl={fallbackRenewUrl} onClose={() => setRenewOpen(false)} onRenewed={handleRenewed} />
          )}
        </section>

        <section className="delivery-section">
          <div className="delivery-section-head">
            <MonitorSmartphone size={15} />
            <strong>使用提示</strong>
          </div>
          <ul className="delivery-tips">
            <li><ShieldCheck size={13} />本页链接等同于账号凭证，请勿分享给他人；一旦外流，任何人都能用你的流量与在线名额。</li>
            <li><ShieldCheck size={13} />名额按网络（公网 IP）计算：同一 Wi-Fi 或宽带下的手机、电脑、平板只占 1 个，设备数不限。</li>
            <li><ShieldCheck size={13} />按 {view.ip_window_hours} 小时内出现过的不同网络统计，所以临时换网络不会立刻占满额度。</li>
            <li><ShieldCheck size={13} />机场节点可能随时上下线，客户端里看到的节点列表会自动跟着变。</li>
          </ul>
        </section>

        <footer className="delivery-foot">
          <span>订阅编号 <code>{token.slice(0, 8)}…</code></span>
          <span>本页可安全收藏，随时回来复制链接或下载配置</span>
        </footer>
      </div>
    </div>
  )
}
