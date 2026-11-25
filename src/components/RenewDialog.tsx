import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Check, CheckCircle2, Copy, ExternalLink, Loader2 } from 'lucide-react'
import { copyToClipboard } from '../lib/clipboard'
import { useCopiedFlash } from '../lib/copiedFlash'
import { loadTurnstile } from '../lib/turnstile'
import { safeSameOriginPath } from '../lib/safeUrl'
import { Modal } from './Modal'

// RenewDialog —— 交付页内弹窗续费（2026-10-30）。
//
// 原来「前往续费」依赖独立商城域名，若该域名不可达时客户无法续费。
// 这里支持在交付页同域调用外部商城续费接口，边缘反代可按需仅放行以下路径：
//   GET  /store-api/renew-info  —— 用订阅页 token 认出订单、商品与买家邮箱（不需要会员登录）
//   GET  /store-api/catalog     —— 面额与支付方式（金额由商城算，前端只展示）
//   GET  /store-api/turnstile   —— 人机验证 site key
//   POST /shop/checkout         —— 下单；商城原有的全部校验不变（订单/邮箱/状态/人机验证/限流）
//   GET  /store-api/order       —— 轮询付款与续期结果
// 二维码图片（/qr/、/alipay/qr/、/wechat/qr/）同样同域取。
//
// 不支持：会员余额（需要商城登录态）。PayPal 必须在 PayPal 自己的页面付款，新标签打开。
//
// 两种模式：
//   商城订单（order 非空）：按原订单续费（renewal_of），邮箱沿用原订单。
//   手工开通（order 为空）：/store-api/adopt-info 确认可接入，买家选节点商品、自己填邮箱，
//     以 adopt_sub 下单；付款后商城把订单挂到这个订阅上并延期，之后就是普通商城订阅。

export type RenewTarget = { order: string; sub: string }

type PayMethod = { id: string; label: string; amount: string; currency: string }
type Denomination = { id: string; label: string; price_cny: string; methods: PayMethod[]; voucher_available: boolean; voucher_amount: string }
type CatalogProduct = { id: string; name: string; denominations: Denomination[] }

type Checkout = {
  order_id: string
  token: string
  method: string
  method_label?: string
  amount: string
  currency: string
  completed?: boolean
  qr_url?: string
  wallet?: string
  network?: string
  approval_url?: string
}

// window.turnstile 的类型声明在 LoginView.tsx（全局 TurnstileApi），这里共用。

// 订阅页给的 renew_url 形如 https://shop.example.com/?renew=prdord_xxx&sub=<token>；
// 不带这两个参数（手工开通、非商城订单）就没有可续的订单，调用方退回原来的外链。
export function renewTargetFrom(renewURL: string | undefined | null): RenewTarget | null {
  if (!renewURL) return null
  try {
    const url = new URL(renewURL)
    const order = url.searchParams.get('renew') ?? ''
    const sub = url.searchParams.get('sub') ?? ''
    if (/^prdord_[A-Za-z0-9_-]{8,128}$/.test(order) && /^[A-Za-z0-9_-]{32,128}$/.test(sub)) return { order, sub }
  } catch {
    // 非法 URL：当作不可页内续费
  }
  return null
}

const STORE_REQUEST_TIMEOUT_MS = 30_000
const STORE_UNAVAILABLE_MSG = '当前站点尚未配置在线自助续费接口，请通过外部商城页面续费或联系客服/管理员处理。'

async function getJSON<T>(path: string, externalSignal?: AbortSignal): Promise<T> {
  const controller = new AbortController()
  let timedOut = false
  const timer = window.setTimeout(() => { timedOut = true; controller.abort() }, STORE_REQUEST_TIMEOUT_MS)
  const onAbort = () => controller.abort()
  externalSignal?.addEventListener('abort', onAbort, { once: true })
  try {
    const response = await fetch(path, {
      headers: { Accept: 'application/json' },
      cache: 'no-store',
      signal: controller.signal,
    })
    const contentType = response.headers.get('content-type') ?? ''
    const isJSON = contentType.toLowerCase().includes('json')
    if (response.status === 404 || response.status === 502 || response.status === 503) {
      const body = isJSON ? await response.json().catch(() => ({})) : {}
      const backendErr = (body as { error?: string }).error
      throw new Error(backendErr || STORE_UNAVAILABLE_MSG)
    }
    if (!isJSON) {
      // 未配置外部商城反代时，静态托管/反向代理可能把 /store-api/* 回退到前端 HTML。
      throw new Error(STORE_UNAVAILABLE_MSG)
    }
    const body = await response.json().catch(() => null)
    if (!body || typeof body !== 'object') throw new Error('续费接口响应格式异常，请稍后重试')
    if (!response.ok) throw new Error((body as { error?: string }).error || `请求失败（${response.status}）`)
    return body as T
  } catch (err) {
    if (timedOut) throw new Error('续费接口响应超时，请检查网络或稍后重试')
    throw err
  } finally {
    window.clearTimeout(timer)
    externalSignal?.removeEventListener('abort', onAbort)
  }
}

// 只允许同域相对路径的图片，拦截 //、/\ 反斜杠与控制字符并复核 URL origin，防止接口响应被篡改指到外站。
function sameOriginPath(path: string | undefined): string | undefined {
  return safeSameOriginPath(path)
}

// 支付跳转链接只放行绝对 http/https 且拒绝控制字符，防止后端返回异常协议注入恶意脚本链接。
function safeExternalUrl(value: string | undefined): string | undefined {
  if (!value || /[\u0000-\u001f\u007f]/.test(value)) return undefined
  try {
    const parsed = new URL(value.trim())
    return parsed.protocol === 'https:' || parsed.protocol === 'http:' ? parsed.href : undefined
  } catch {
    return undefined
  }
}

function maskEmail(email: string): string {
  const [name, domain] = email.split('@')
  if (!domain) return email
  return (name.length <= 2 ? name[0] + '*' : name.slice(0, 2) + '***') + '@' + domain
}

const POLL_MS = 4000
const POLL_LIMIT_MS = 30 * 60 * 1000

export function RenewDialog({ target, fallbackUrl, onClose, onRenewed }: {
  target: RenewTarget
  fallbackUrl?: string
  onClose: () => void
  onRenewed: () => void
}) {
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [email, setEmail] = useState('')
  const [productID, setProductID] = useState('')
  const [productName, setProductName] = useState('')
  const [products, setProducts] = useState<CatalogProduct[]>([])
  const adopt = !target.order
  const [denominations, setDenominations] = useState<Denomination[]>([])
  const [denomID, setDenomID] = useState('')
  const [method, setMethod] = useState('')
  const [voucher, setVoucher] = useState('')
  const [siteKey, setSiteKey] = useState('')
  const [captcha, setCaptcha] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [checkout, setCheckout] = useState<Checkout | null>(null)
  const [status, setStatus] = useState('')
  const [done, setDone] = useState(false)
  // 2026-11-06：轮询已停止（订单取消或超过 30 分钟）。原来停了之后转圈和「付款后会自动确认」一直留着，
  // 买家以为还在等，实际页面已经不会再更新。
  const [pollStopped, setPollStopped] = useState(false)
  const [copiedKey, flashCopied] = useCopiedFlash()
  const widgetEl = useRef<HTMLDivElement | null>(null)
  const widgetID = useRef<string | null>(null)
  const mountedRef = useRef(true)
  const onRenewedRef = useRef(onRenewed)
  onRenewedRef.current = onRenewed
  useEffect(() => {
    mountedRef.current = true
    return () => { mountedRef.current = false }
  }, [])

  useEffect(() => {
    let cancelled = false
    const controller = new AbortController()
    void (async () => {
      try {
        const [catalog, ts] = await Promise.all([
          getJSON<{ products?: CatalogProduct[] }>('/store-api/catalog', controller.signal),
          getJSON<{ enabled?: boolean; site_key?: string }>('/store-api/turnstile', controller.signal),
        ])
        if (!Array.isArray(catalog?.products)) throw new Error(STORE_UNAVAILABLE_MSG)
        let candidates: CatalogProduct[]
        let resolvedEmail = ''
        if (adopt) {
          const info = await getJSON<{ product_ids?: string[] }>(`/store-api/adopt-info?sub=${encodeURIComponent(target.sub)}`, controller.signal)
          const allowedIDs = Array.isArray(info?.product_ids) ? info.product_ids : []
          candidates = catalog.products.filter((p) => allowedIDs.includes(p.id) && Array.isArray(p.denominations) && p.denominations.length > 0)
        } else {
          const q = `order=${encodeURIComponent(target.order)}&sub=${encodeURIComponent(target.sub)}`
          const info = await getJSON<{ order_id?: string; product_id?: string; email?: string }>(`/store-api/renew-info?${q}`, controller.signal)
          candidates = catalog.products.filter((p) => p.id === info.product_id && Array.isArray(p.denominations) && p.denominations.length > 0)
          resolvedEmail = info.email ?? ''
        }
        if (cancelled) return
        if (candidates.length === 0) throw new Error('该套餐已下架或暂未开放自助续费，请联系客服续费')
        if (!adopt) setEmail(resolvedEmail)
        setProducts(candidates)
        setProductID(candidates[0].id)
        setProductName(candidates[0].name)
        setDenominations(candidates[0].denominations)
        setDenomID(candidates[0].denominations[0].id)
        setSiteKey(ts.enabled && ts.site_key ? ts.site_key : '')
      } catch (err) {
        if (!cancelled) setError(err instanceof Error ? err.message : '读取续费信息失败')
      } finally {
        if (!cancelled) setLoading(false)
      }
    })()
    return () => { cancelled = true; controller.abort() }
  }, [target.order, target.sub, adopt])

  const pickProduct = (p: CatalogProduct) => {
    setProductID(p.id); setProductName(p.name); setDenominations(p.denominations); setDenomID(p.denominations[0].id)
  }

  // 只有续费信息本身读取失败才算加载失败；下单前的校验错误不影响人机验证框。
  const loadFailed = !loading && denominations.length === 0
  const denom = denominations.find((d) => d.id === denomID) ?? denominations[0]
  // 会员余额需要商城登录态，这里拿不到，不提供；用 useMemo 稳定引用避免频繁重跑选中校验。
  const methods = useMemo<PayMethod[]>(() => (denom
    ? [...denom.methods.filter((m) => m.id !== 'BALANCE'),
       ...(denom.voucher_available ? [{ id: 'VOUCHER', label: '兑换码', amount: denom.voucher_amount, currency: 'CNY' }] : [])]
    : []), [denom])
  useEffect(() => {
    if (methods.length > 0 && !methods.some((m) => m.id === method)) setMethod(methods[0].id)
  }, [methods, method])

  // 人机验证：选好方式、还没下单时渲染；下单后移除，失败时重置以便重试。
  useEffect(() => {
    if (!siteKey || checkout || loading || loadFailed) return
    let disposed = false
    void loadTurnstile().then((ts) => {
      if (disposed || !widgetEl.current || widgetID.current) return
      widgetID.current = ts.render(widgetEl.current, {
        sitekey: siteKey,
        language: 'zh-cn',
        callback: (token: string) => { if (!disposed) setCaptcha(token) },
        'expired-callback': () => { if (!disposed) setCaptcha('') },
        'error-callback': () => { if (!disposed) setCaptcha('') },
      })
    }).catch((err: Error) => { if (!disposed) setError(err.message) })
    return () => {
      disposed = true
      if (widgetID.current && window.turnstile) window.turnstile.remove?.(widgetID.current)
      widgetID.current = null
    }
  }, [siteKey, checkout, loading, loadFailed])

  const submit = useCallback(async () => {
    if (!denom || !method) return
    if (siteKey && !captcha) { setError('请先完成人机验证'); return }
    if (method === 'VOUCHER' && !voucher.trim()) { setError('请输入兑换码'); return }
    if (adopt && !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email.trim())) { setError('请填写有效的邮箱，用于接收订单与以后续费'); return }
    setSubmitting(true)
    setError('')
    const form = new URLSearchParams({
      product_id: productID,
      buyer_email: email.trim(),
      method,
      price_option_id: denom.id,
      'cf-turnstile-response': captcha,
    })
    if (adopt) form.set('adopt_sub', target.sub)
    else form.set('renewal_of', target.order)
    if (method === 'VOUCHER') form.set('voucher_code', voucher.trim())
    const controller = new AbortController()
    let timedOut = false
    const timer = window.setTimeout(() => { timedOut = true; controller.abort() }, STORE_REQUEST_TIMEOUT_MS)
    try {
      const response = await fetch('/shop/checkout', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json', 'X-Requested-With': 'XMLHttpRequest' },
        body: form.toString(),
        signal: controller.signal,
      })
      const contentType = response.headers.get('content-type') ?? ''
      const body = contentType.toLowerCase().includes('json') ? await response.json().catch(() => ({})) : {}
      if (!response.ok) throw new Error((body as { error?: string }).error || `下单失败（${response.status}）`)
      const result = body as Checkout
      if (!result?.order_id) throw new Error(STORE_UNAVAILABLE_MSG)
      if (!mountedRef.current) return
      setCheckout(result)
      setStatus(result.completed ? '已付款，正在续期…' : '等待付款')
      const approval = safeExternalUrl(result.approval_url)
      if (approval) window.open(approval, '_blank', 'noopener,noreferrer')
    } catch (err) {
      if (!mountedRef.current) return
      setError(timedOut ? '下单请求超时，请检查网络或稍后重试' : (err instanceof Error ? err.message : '下单失败'))
      setCaptcha('')
      if (widgetID.current && window.turnstile) window.turnstile.reset(widgetID.current)
    } finally {
      window.clearTimeout(timer)
      if (mountedRef.current) setSubmitting(false)
    }
  }, [denom, method, siteKey, captcha, voucher, productID, email, target.order, target.sub, adopt])

  // 轮询订单：fulfilled = 续期已写入订阅；cancelled = 超时未付款。
  useEffect(() => {
    if (!checkout || done) return
    const started = Date.now()
    let stopped = false
    let timer = 0
    const controller = new AbortController()
    const tick = async () => {
      try {
        const order = await getJSON<{ status: string; status_label: string }>(
          `/store-api/order?id=${encodeURIComponent(checkout.order_id)}&token=${encodeURIComponent(checkout.token)}`,
          controller.signal,
        )
        if (stopped) return
        if (order.status === 'fulfilled') {
          setDone(true)
          setStatus('续费成功')
          onRenewedRef.current()
          return
        }
        if (order.status === 'cancelled') { setError('订单已取消（超时未付款），请重新下单'); setPollStopped(true); return }
        setStatus(order.status_label || order.status)
      } catch {
        // 网络抖动：下一轮再查
      }
      if (stopped) return
      if (Date.now() - started < POLL_LIMIT_MS) timer = window.setTimeout(() => void tick(), POLL_MS)
      else setPollStopped(true)
    }
    timer = window.setTimeout(() => void tick(), 1500)
    return () => { stopped = true; controller.abort(); window.clearTimeout(timer) }
  }, [checkout, done])

  const selected = methods.find((m) => m.id === method)
  const qr = sameOriginPath(checkout?.qr_url)
  const approvalUrl = safeExternalUrl(checkout?.approval_url)
  const safeFallbackUrl = safeExternalUrl(fallbackUrl)

  async function copyPayField(key: string, value: string) {
    if (await copyToClipboard(value)) flashCopied(key)
  }

  return (
    <Modal title="续费订阅" kicker="RENEW" onClose={onClose}>
      {loading ? (
        <div className="delivery-centered"><Loader2 size={20} className="spin" /><p className="form-note">正在读取续费信息…</p></div>
      ) : done ? (
        <div className="delivery-centered">
          <CheckCircle2 size={28} />
          <p><strong>续费成功</strong></p>
          <p className="form-note">订阅已恢复，本页链接不变，客户端无需改动。</p>
          <div className="modal-actions"><button className="button button-primary" onClick={onClose}>完成</button></div>
        </div>
      ) : checkout ? (
        <div className="renew-pay">
          <p className="form-note">订单 {checkout.order_id} · {checkout.method_label || checkout.method}</p>
          <div className="renew-wallet-row">
            <strong>应付 {checkout.amount} {checkout.currency}</strong>
            {checkout.network && <span>（{checkout.network} 网络）</span>}
            {checkout.wallet && (
              <button type="button" className="button button-quiet button-compact" onClick={() => void copyPayField('amount', checkout.amount)}>
                {copiedKey === 'amount' ? <Check size={12} /> : <Copy size={12} />}{copiedKey === 'amount' ? '已复制金额' : '复制金额'}
              </button>
            )}
          </div>
          {qr && <img className="renew-qr" src={qr} alt="付款二维码" width={200} height={200} />}
          {checkout.wallet && (
            <div className="form-note">
              <div className="renew-wallet-row">
                <span>收款地址：</span>
                <code className="renew-wallet">{checkout.wallet}</code>
                <button type="button" className="button button-quiet button-compact" onClick={() => void copyPayField('wallet', checkout.wallet ?? '')}>
                  {copiedKey === 'wallet' ? <Check size={12} /> : <Copy size={12} />}{copiedKey === 'wallet' ? '已复制地址' : '复制地址'}
                </button>
              </div>
              请转账<strong>精确金额</strong>，多转少转都无法自动到账。
            </div>
          )}
          {checkout.method === 'ALIPAY' && <p className="form-note">请用支付宝扫码付款。</p>}
          {checkout.method === 'WECHAT' && <p className="form-note">请用微信扫码付款。</p>}
          {approvalUrl && (
            <p className="form-note">已在新标签页打开 PayPal；没有弹出的话 <a href={approvalUrl} target="_blank" rel="noreferrer noopener">点这里付款 <ExternalLink size={12} /></a>。付完回到本页即可。</p>
          )}
          {pollStopped
            ? <p className="form-note">已停止自动查询付款状态（{status || '未确认'}）。如果已经付款，刷新本页即可看到新的到期时间；没付款请关闭后重新下单。</p>
            : <p className="form-note"><Loader2 size={13} className="spin" /> {status}（付款后会自动确认，请勿关闭本窗口）</p>}
          {error && <p className="form-error">{error}</p>}
        </div>
      ) : error && denominations.length === 0 ? (
        <div>
          <p className="form-error">{error}</p>
          <div className="modal-actions">
            <button className="button button-quiet" onClick={onClose}>关闭</button>
            {safeFallbackUrl && (
              <a className="button button-primary" href={safeFallbackUrl} target="_blank" rel="noreferrer noopener">
                前往商城页面续费 <ExternalLink size={13} />
              </a>
            )}
          </div>
        </div>
      ) : (
        <div className="renew-form">
          {adopt ? (
            <>
              <p className="form-note">续费后本页链接与配置都不变，只延长到期时间。</p>
              {products.length > 1 && (
                <div className="sub-format-tabs">
                  {products.map((p) => (
                    <button key={p.id} className={`button button-compact ${p.id === productID ? 'button-primary' : 'button-quiet'}`} onClick={() => pickProduct(p)}>{p.name}</button>
                  ))}
                </div>
              )}
              <label className="renew-field">
                <span>邮箱</span>
                <input className="renew-input" type="email" placeholder="name@example.com" value={email} onChange={(e) => { setEmail(e.target.value); setError('') }} autoComplete="email" />
                <small>用于接收订单通知，以后续费也按这个邮箱识别</small>
              </label>
              <span className="renew-field-title">支付方式</span>
            </>
          ) : (
            <p className="form-note">{productName} · 续费到 {maskEmail(email)} 的订单，链接与配置都不变。</p>
          )}
          {denominations.length > 1 && (
            <div className="sub-format-tabs">
              {denominations.map((d) => (
                <button key={d.id} className={`button button-compact ${d.id === denom?.id ? 'button-primary' : 'button-quiet'}`} onClick={() => setDenomID(d.id)}>
                  {d.label} · ¥{d.price_cny}
                </button>
              ))}
            </div>
          )}
          <div className="renew-methods">
            {methods.map((m) => (
              <label key={m.id} className={`renew-method${m.id === method ? ' active' : ''}`}>
                <input type="radio" name="renew-method" checked={m.id === method} onChange={() => setMethod(m.id)} />
                <span>{m.label}</span>
                <strong>{m.amount} {m.currency}</strong>
              </label>
            ))}
          </div>
          {method === 'VOUCHER' && (
            <label className="renew-field">
              <span>兑换码</span>
              <input className="renew-input" placeholder="输入兑换码" value={voucher} onChange={(e) => { setVoucher(e.target.value); setError('') }} autoComplete="off" />
            </label>
          )}
          {method === 'PAYPAL' && <p className="form-note">PayPal 需要在它自己的页面付款，下单后会在新标签页打开。</p>}
          {siteKey && <div ref={widgetEl} className="renew-captcha" />}
          {error && <p className="form-error">{error}</p>}
          <div className="modal-actions">
            <button className="button button-quiet" onClick={onClose}>取消</button>
            <button className="button button-primary" disabled={submitting || !selected || (Boolean(siteKey) && !captcha)} onClick={() => void submit()}>
              {submitting ? <Loader2 size={14} className="spin" /> : null}下单并付款{selected ? ` ${selected.amount} ${selected.currency}` : ''}
            </button>
          </div>
        </div>
      )}
    </Modal>
  )
}
