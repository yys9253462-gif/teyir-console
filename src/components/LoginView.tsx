import { useEffect, useRef, useState } from 'react'
import { api } from '../lib/api'
import { loadTurnstile } from '../lib/turnstile'
import { ShieldCheck } from 'lucide-react'

// Cloudflare Turnstile：后端配置了 site key 才渲染；令牌一次性，
// 每次提交后（无论成败）都要重置获取新令牌。

export function LoginView({ onLogin }: { onLogin: () => void }) {
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [siteKey, setSiteKey] = useState<string | null>(null)
  const [token, setToken] = useState('')
  const widgetRef = useRef<HTMLDivElement>(null)
  const widgetId = useRef<string | undefined>(undefined)
  const [configAttempt, setConfigAttempt] = useState(0)
  const [configLoading, setConfigLoading] = useState(true)
  const [verificationError, setVerificationError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    setConfigLoading(true)
    setSiteKey(null)
    setToken('')
    setVerificationError(null)
    api.loginConfig()
      .then((cfg) => {
        if (cancelled) return
        if (typeof cfg?.turnstile_site_key !== 'string') throw new Error('登录配置响应缺少人机验证设置')
        setSiteKey(cfg.turnstile_site_key)
      })
      .catch((err) => {
        if (cancelled) return
        setVerificationError(err instanceof Error ? `人机验证配置加载失败：${err.message}` : '人机验证配置加载失败')
      })
      .finally(() => { if (!cancelled) setConfigLoading(false) })
    return () => { cancelled = true }
  }, [configAttempt])

  useEffect(() => {
    if (!siteKey || !widgetRef.current) return
    let cancelled = false
    loadTurnstile()
      .then((ts) => {
        if (cancelled || !widgetRef.current || widgetId.current !== undefined) return
        widgetId.current = ts.render(widgetRef.current, {
          sitekey: siteKey,
          language: 'zh-cn',
          callback: (t: string) => { if (!cancelled) { setToken(t); setVerificationError(null) } },
          'expired-callback': () => { if (!cancelled) setToken('') },
          'error-callback': () => { if (!cancelled) { setToken(''); setVerificationError('人机验证失败，请重试') } },
        })
      })
      .catch((err) => { if (!cancelled) setVerificationError(err instanceof Error ? err.message : '人机验证加载失败') })
    return () => {
      cancelled = true
      if (widgetId.current !== undefined) {
        window.turnstile?.remove?.(widgetId.current)
        widgetId.current = undefined
      }
    }
  }, [siteKey, configAttempt])

  const needsCaptcha = siteKey !== null && siteKey !== ''
  const captchaReady = siteKey !== null && (!needsCaptcha || token !== '')

  async function submit(event: React.FormEvent) {
    event.preventDefault()
    if (pending) return
    if (!captchaReady) {
      setError(siteKey === null ? '请先重新加载人机验证配置' : '请先完成人机验证')
      return
    }
    setPending(true)
    setError(null)
    try {
      await api.login(email, password, token)
      onLogin()
    } catch (err) {
      setError(err instanceof Error ? err.message : '登录失败')
      if (needsCaptcha) {
        setToken('')
        window.turnstile?.reset(widgetId.current)
      }
    } finally {
      setPending(false)
    }
  }

  return (
    <div className="login-shell">
      <form className="login-card" onSubmit={submit}>
        <div className="login-mark">
          <ShieldCheck size={28} />
        </div>
        <h1>Teyir <span>控制台</span></h1>
        <p className="login-subtitle">运维管理员专用 · 节点与主机管理</p>
        <p className="login-audience-hint">
          订阅用户请直接访问交付通知中的<b>完整</b>订阅链接（形如 …/user/一长串字符，
          若链接被截断或仅输入域名会落到本页，无法在此补全）。
          本登录入口仅供运维管理员管理节点与主机。
        </p>
        <label className="login-field">
          <span>邮箱</span>
          <input type="email" value={email} onChange={(event) => setEmail(event.target.value)} required autoComplete="username" />
        </label>
        <label className="login-field">
          <span>密码<span className="field-hint">至少 12 位</span></span>
          <input type="password" value={password} onChange={(event) => setPassword(event.target.value)} required autoComplete="current-password" minLength={12} />
        </label>
        {needsCaptcha && <div ref={widgetRef} className="login-captcha" />}
        {configLoading && <p className="form-note" role="status">正在加载人机验证配置…</p>}
        {verificationError && <div className="login-error" role="alert">
          {verificationError}。尚未确认验证配置，请重试。
          <button className="button button-quiet button-compact" type="button" disabled={configLoading || pending} onClick={() => { setError(null); setConfigAttempt((attempt) => attempt + 1) }}>重新加载人机验证</button>
        </div>}
        {error && <div className="login-error" role="alert">{error}</div>}
        <button className="login-submit" type="submit" disabled={pending || siteKey === null || !captchaReady}>{pending ? '登录中…' : '登录'}</button>
      </form>
    </div>
  )
}
