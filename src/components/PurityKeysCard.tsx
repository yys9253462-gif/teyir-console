import { useEffect, useRef, useState } from 'react'
import { KeyRound } from 'lucide-react'
import { api, type PurityKeyName, type PurityKeysState } from '../lib/api'
import { askConfirm } from './ConfirmHost'

// 系统设置 → IP 检测密钥（2026-10-15）。
//
// 密钥加密存库，保存后立即生效、无需重启。页面永远不回显明文，
// 只显示「已配置 · 来源 · 末四位」。输入框留空 = 不改动；点「清除」才删除。

const KEYS: { name: PurityKeyName; label: string; hint: string; link: string }[] = [
  {
    name: 'ipapi_is', label: 'ipapi.is',
    hint: '必填：连接类型（家宽 / 机房）、ASN、双 ISP。免费档每天 1000 次。',
    link: 'https://ipapi.is/',
  },
  {
    name: 'ipqs', label: 'IPQualityScore',
    hint: '可选：单个 IP 的欺诈分与代理 / VPN / 近期滥用标记。配置后「ASN 滥用率」换成「欺诈分」。',
    link: 'https://www.ipqualityscore.com/',
  },
  {
    name: 'cloudflare', label: 'Cloudflare API Token',
    hint: '可选：一键安装时自动建 DNS 记录、换 IP 后自动改记录。需要「Zone · DNS · Edit」权限。',
    link: 'https://dash.cloudflare.com/profile/api-tokens',
  },
]

const SOURCE_TEXT: Record<string, string> = { web: '面板设置', env: '环境变量' }

export function PurityKeysCard({ notify }: { notify: (message: string) => void }) {
  const [state, setState] = useState<PurityKeysState | null>(null)
  const [drafts, setDrafts] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  // 2026-11-06：读取失败单独显示。原来静默 setState(null)，每个密钥都显示「未配置」——
  // 已经配好的密钥看起来像没配，运维可能以为纯净度检测、Cloudflare、Telegram 都不可用。
  const [loadError, setLoadError] = useState('')
  const mountedRef = useRef(true)

  useEffect(() => {
    mountedRef.current = true
    api.purityKeys()
      .then((res) => { if (mountedRef.current) { setState(res); setLoadError('') } })
      .catch((err) => { if (mountedRef.current) setLoadError(err instanceof Error ? err.message : '读取失败') })
    return () => { mountedRef.current = false }
  }, [])

  async function save(payload: Partial<Record<PurityKeyName, string>>, message: string) {
    if (busy) return
    setBusy(true)
    try {
      const next = await api.setPurityKeys(payload)
      if (!mountedRef.current) return
      setState(next)
      setDrafts({})
      setLoadError('')
      notify(message)
    } catch (error) {
      if (!mountedRef.current) return
      notify(error instanceof Error ? error.message : '保存失败')
      // 失败后重读一次，界面以库里的真实状态为准。
      api.purityKeys()
        .then((res) => { if (mountedRef.current) { setState(res); setLoadError('') } })
        .catch(() => {})
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  const dirty = Object.entries(drafts).filter(([, v]) => v.trim() !== '')

  return (
    <section className="panel settings-panel">
      <div className="section-head"><div><div className="section-kicker">INTEGRATIONS</div><h2>集成密钥</h2></div></div>
      <div className="settings-form">
        {loadError && !state && <p className="form-error">读取密钥配置失败：{loadError}。是否已配置未知，刷新页面重试。</p>}
        {KEYS.map((k) => {
          const entry = state?.[k.name]
          return (
            <div key={k.name} style={{ display: 'grid', gap: 6 }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
                <strong style={{ fontSize: 'var(--fs-sm)' }}>{k.label}</strong>
                {entry?.configured
                  ? <span className="status-pill online">已配置 · {SOURCE_TEXT[entry.source] ?? entry.source} · {entry.masked}</span>
                  : !state && loadError
                    ? <span className="status-pill warning">读取失败</span>
                    : <span className="status-pill offline">未配置</span>}
                <a className="text-button" href={k.link} target="_blank" rel="noreferrer noopener">获取密钥 ↗</a>
              </div>
              <label className="form-field">
                <span>{k.hint}</span>
                <input
                  type="password"
                  autoComplete="off"
                  placeholder={entry?.configured ? '留空则不修改' : '粘贴密钥'}
                  value={drafts[k.name] ?? ''}
                  onChange={(e) => setDrafts((d) => ({ ...d, [k.name]: e.target.value }))}
                />
              </label>
              {entry?.source === 'web' && (
                <div>
                  <button className="text-button" disabled={busy} onClick={async () => {
                    if (await askConfirm(`清除面板里设置的 ${k.label} 密钥？${k.name === 'ipapi_is' ? '\n若环境变量里也没有，纯净度检测将不可用。' : ''}`)) {
                      void save({ [k.name]: '' }, `已清除 ${k.label} 密钥`)
                    }
                  }}>清除面板里的密钥</button>
                </div>
              )}
            </div>
          )
        })}
        <div>
          <button
            className="button button-primary"
            disabled={busy || dirty.length === 0}
            onClick={() => void save(Object.fromEntries(dirty.map(([n, v]) => [n, v.trim()])), '密钥已保存，立即生效')}
          >
            <KeyRound size={15} />{busy ? '保存中…' : '保存密钥'}
          </button>
        </div>
        <p className="form-note">密钥用与 SSH 凭据相同的 AES-256-GCM 加密存库；面板设置的值优先于环境变量。保存后到「IP 纯净度」页重新检测即可用上。</p>
      </div>
    </section>
  )
}
