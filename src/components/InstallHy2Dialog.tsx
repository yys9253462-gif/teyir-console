import { useEffect, useRef, useState } from 'react'
import { AlertTriangle, CheckCircle2, Loader2, XCircle } from 'lucide-react'
import { api, type InstallJob, type Server } from '../lib/api'
import { pendingInstallJob, useInstallJobPolling } from '../lib/useInstallJobPolling'
import { copyToClipboard } from '../lib/clipboard'
import { safeHttpUrl } from '../lib/safeUrl'
import { Modal } from './Modal'
import { askConfirm } from './ConfirmHost'

// 一键安装 hysteria2-installer 并自动接入控制台（2026-10-15）。
//
// 服务端会：检查域名解析 → SSH 执行官方安装脚本（Agent 模式 + Let's Encrypt）→
// 读 portal-access.json → 走「接入节点」同一套逻辑创建并绑定节点。
// 这里只收集参数、展示进度。任务在后台跑，关掉窗口不影响。

export function installBlockedReason(server: Server, hasNode: boolean): string {
  if (!server.has_credential) return '先在「编辑主机」里上传 SSH 私钥或密码'
  if (!server.allow_write_ops) return '先在「编辑主机」里开启写操作授权'
  if (hasNode) return '这台主机在面板里已经有节点了'
  return ''
}

export function InstallHy2Dialog({ server, onClose, onDone, notify }: {
  server: Server
  onClose: () => void
  onDone: () => void
  notify: (message: string) => void
}) {
  const [domain, setDomain] = useState('')
  const [email, setEmail] = useState(() => loadAcmeEmail())
  // 配了 Cloudflare Token：只填前缀 + 选域名，自动 DNS 默认打开。
  const [zones, setZones] = useState<string[] | null>(null)
  const [zonesError, setZonesError] = useState('')
  const [prefix, setPrefix] = useState('')
  const [zone, setZone] = useState('')
  const [manualDomain, setManualDomain] = useState(false)
  const [port, setPort] = useState('')
  const [name, setName] = useState(server.name)
  const [inStore, setInStore] = useState(true)
  const [autoDNS, setAutoDNS] = useState(false)
  const [starting, setStarting] = useState(false)
  const [error, setError] = useState('')
  const [job, setJob] = useState<InstallJob | null>(null)
  const logRef = useRef<HTMLPreElement>(null)
  const mountedRef = useRef(true)
  useEffect(() => {
    mountedRef.current = true
    return () => { mountedRef.current = false }
  }, [])

  useEffect(() => {
    let alive = true
    api.cloudflareZones().then((r) => {
      if (!alive) return
      setZones(r.zones)
      setZonesError(r.error ?? '')
      if (r.zones.length > 0) {
        setZone(r.zones.includes(loadLastZone()) ? loadLastZone() : r.zones[0])
        setAutoDNS(true)
      }
    }).catch((err) => {
      if (!alive) return
      // 2026-11-06：请求本身失败也要说出来。原来只吞掉成空列表，界面直接退回手填域名，
      // 不提示 Cloudflare 读取失败，也不再自动改 DNS —— 操作者以为没配 Cloudflare。
      setZones([])
      setZonesError(err instanceof Error ? err.message : '请求失败')
    })
    return () => { alive = false }
  }, [])

  const useZones = !manualDomain && !!zones && zones.length > 0
  const fullDomain = useZones ? (prefix.trim() ? `${prefix.trim().toLowerCase()}.${zone}` : '') : domain.trim()
  const running = job?.status === 'running'
  const prefixOk = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$/i.test(prefix.trim())
  const domainOk = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$/i.test(fullDomain) && (!useZones || prefixOk)
  const emailOk = /^[^\s@'"`$\;&|<>]+@[^\s@'"`$\;&|<>]+\.[^\s@'"`$\;&|<>]+$/.test(email.trim())
  const portOk = port === '' || (/^\d+$/.test(port) && Number(port) >= 1 && Number(port) <= 65535)

  useInstallJobPolling(job, setJob, setError, (next) => {
    notify(`安装完成，已接入节点：${next.node_name}`)
    onDone()
  })

  useEffect(() => {
    logRef.current?.scrollTo(0, logRef.current.scrollHeight)
  }, [(job?.log ?? []).length])

  async function start() {
    if (!await askConfirm(`在 ${server.name}（${server.host}）上安装 Hysteria 2？

域名：${fullDomain}\n\n会执行环境变量 HY2_INSTALL_SCRIPT_URL 指定的节点安装脚本并重写 /etc/hysteria 下的配置；若这台机器已装过，门户凭据与用户会被保留。`)) return
    setStarting(true)
    setError('')
    try {
      const { job_id } = await api.installHy2(server.id, {
        domain: fullDomain, email: email.trim(),
        port: port ? Number(port) : undefined, name: name.trim() || server.name, in_store: inStore, auto_dns: autoDNS,
      })
      saveAcmeEmail(email.trim())
      if (useZones) saveLastZone(zone)
      if (mountedRef.current) setJob(pendingInstallJob(job_id, server.id))
    } catch (err) {
      if (mountedRef.current) setError(err instanceof Error ? err.message : '启动安装失败')
    } finally {
      if (mountedRef.current) setStarting(false)
    }
  }

  return (
    <Modal title={`一键安装 HY2 · ${server.name}`} kicker="INSTALL HYSTERIA 2" wide onClose={onClose}>
      <div style={{ display: 'grid', gap: 12 }}>
        {!job && <>
          <p className="form-note">
            通过 SSH 在 <strong>{server.host}</strong> 上执行由环境变量 <code>HY2_INSTALL_SCRIPT_URL</code> 指定的节点安装脚本
            （Agent 模式，Let's Encrypt 证书），装完自动接入控制台，无需再复制粘贴。
            {server.ssh_user !== 'root' && <> 当前 SSH 用户是 <strong>{server.ssh_user}</strong>，需要它有免密 sudo（多数云厂商默认镜像已具备，安装前会自动检查）。</>}
          </p>
          {useZones ? (
            <div className="form-field">
              <span>节点域名 *（从 Cloudflare 账号里识别到 {zones!.length} 个域名，只填前缀即可，会自动把解析指向 {server.host}）</span>
              <div className="domain-compose">
                <input value={prefix} onChange={(e) => setPrefix(e.target.value.replace(/\s/g, ''))} placeholder="如 us6" aria-label="域名前缀" aria-invalid={prefix !== '' && !prefixOk} autoFocus />
                <span className="domain-dot">.</span>
                <select value={zone} onChange={(e) => setZone(e.target.value)} aria-label="主域名">
                  {zones!.map((z) => <option key={z} value={z}>{z}</option>)}
                </select>
              </div>
              <small className="form-note">{fullDomain ? <>完整域名：<code>{fullDomain}</code></> : '填写前缀后显示完整域名'} · <button type="button" className="text-button" style={{ display: 'inline' }} onClick={() => { setManualDomain(true); setDomain(fullDomain) }}>改为手动输入完整域名</button></small>
            </div>
          ) : (
            <label className="form-field"><span>域名 *（不勾下方自动 DNS 时，A 记录需已指向 {server.host}，且不要开 CDN 代理）</span>
              <input value={domain} onChange={(e) => setDomain(e.target.value)} placeholder="hy2.example.com" aria-invalid={domain !== '' && !domainOk} />
              {zones && zones.length > 0 && <small className="form-note"><button type="button" className="text-button" style={{ display: 'inline' }} onClick={() => setManualDomain(false)}>改回「前缀 + 选择域名」</button></small>}
              {zones && zones.length === 0 && zonesError && <small className="form-note" style={{ color: 'var(--amber)' }}>读取 Cloudflare 域名失败：{zonesError}</small>}
            </label>
          )}
          <div className="form-row">
            <label className="form-field"><span>证书通知邮箱 *（申请 Let's Encrypt 证书必须，会记住上次填的）</span><input type="email" value={email} onChange={(e) => setEmail(e.target.value)} placeholder="me@example.com" aria-invalid={email !== '' && !emailOk} /></label>
            <label className="form-field"><span>监听端口（可选，留空自动选）</span><input value={port} onChange={(e) => setPort(e.target.value)} placeholder="自动" aria-invalid={!portOk} /></label>
          </div>
          <label className="form-field"><span>节点名称</span><input value={name} onChange={(e) => setName(e.target.value)} /></label>
          <label className="checkbox-row"><input type="checkbox" checked={autoDNS} onChange={(e) => setAutoDNS(e.target.checked)} /><span>用 Cloudflare 自动把域名指向这台主机{zones && zones.length > 0 ? '（推荐，已识别到 Token）' : '（需在系统设置 → 集成密钥里填 Token）'}</span></label>
          <label className="checkbox-row"><input type="checkbox" checked={inStore} onChange={(e) => setInStore(e.target.checked)} /><span>接入后上架商城</span></label>
          {error && <p role="alert" style={{ color: 'var(--red)', overflowWrap: 'anywhere' }}>{error}</p>}
          {error.includes('HY2_INSTALL_SCRIPT_URL') && (
            <p className="form-note service-warn">
              <AlertTriangle size={13} />
              <span>
                当前服务端尚未配置节点安装脚本地址。请在服务端环境变量（如 <code>.env</code> 或容器编排文件）中设置可信的
                <code>HY2_INSTALL_SCRIPT_URL=https://…</code> 并重启后端；也可以先在目标主机手动部署节点，再到「HY2 节点 → 接入节点」粘贴 <code>portal-access.json</code> 导入。
              </span>
            </p>
          )}
          <div className="modal-actions">
            <button className="button button-quiet" onClick={onClose}>取消</button>
            <button className="button button-primary" disabled={starting || !domainOk || !portOk || !emailOk} onClick={() => void start()}>
              {starting ? '启动中…' : '开始安装'}
            </button>
          </div>
        </>}

        {job && <>
          <div className="install-status">
            {job.status === 'running' && <Loader2 size={18} className="spin" />}
            {job.status === 'success' && <CheckCircle2 size={18} style={{ color: 'var(--green)' }} />}
            {job.status === 'failed' && <XCircle size={18} style={{ color: 'var(--red)' }} />}
            {/* 2026-11-06：interrupted（远端可能仍在跑、主机互斥保留）原来没有图标和标题，只剩步骤文字，看着像还在进行。 */}
            {job.status === 'interrupted' && <AlertTriangle size={18} style={{ color: 'var(--amber)' }} />}
            <strong>{job.status === 'success' ? `完成：已接入节点「${job.node_name}」` : job.status === 'failed' ? '安装失败' : job.status === 'interrupted' ? '已中断，需要核验远端是否已停止' : job.step}</strong>
          </div>
          {job.error && <p role="alert" style={{ color: 'var(--red)', overflowWrap: 'anywhere' }}>{job.error}</p>}
          {error && <p role="alert" style={{ color: 'var(--amber)' }}>{error}</p>}
          {job.access && (
            <div className="access-card">
              <div className="access-card-head"><strong>节点后台登录信息</strong><small>只在这里显示一次，请现在保存</small></div>
              {([['后台地址', job.access.url], ['用户名', job.access.username], ['密码', job.access.password]] as const).filter(([, v]) => v).map(([label, value]) => {
                const safeAccessUrl = label === '后台地址' ? safeHttpUrl(value) : undefined
                return (
                <div className="access-row" key={label}>
                  <span>{label}</span>
                  {safeAccessUrl ? <a href={safeAccessUrl} target="_blank" rel="noreferrer noopener">{value}</a> : <code>{value}</code>}
                  <button className="row-action" onClick={() => { void copyToClipboard(value).then((ok) => notify(ok ? `已复制${label}` : '复制失败，请手动选中复制')) }}>复制</button>
                </div>
                )
              })}
              <button className="button button-quiet" style={{ marginTop: 8 }} onClick={() => {
                const a = job.access!
                void copyToClipboard(`后台地址：${a.url}\n用户名：${a.username}\n密码：${a.password}`).then((ok) => notify(ok ? '已复制全部登录信息' : '复制失败，请手动选中复制'))
              }}>复制全部</button>
            </div>
          )}
          <pre className="install-log" ref={logRef}>{(job.log ?? []).join('\n') || '等待输出…'}</pre>
          <div className="modal-actions">
            {running
              ? <span className="form-note">安装在后台进行，关掉窗口不影响；完成后节点会出现在「HY2 节点」。</span>
              : <button className="button button-primary" onClick={onClose}>关闭</button>}
          </div>
        </>}
      </div>
    </Modal>
  )
}

// 证书邮箱与上次选的主域名记在本机浏览器里，下次安装 / 迁移自动带出。
const ACME_EMAIL_KEY = 'hy2.acmeEmail'
const LAST_ZONE_KEY = 'hy2.lastZone'
export function loadAcmeEmail(): string {
  try { return window.localStorage.getItem(ACME_EMAIL_KEY) ?? '' } catch { return '' }
}
export function saveAcmeEmail(v: string) {
  try { if (v) window.localStorage.setItem(ACME_EMAIL_KEY, v) } catch { /* 隐私模式等 */ }
}
function loadLastZone(): string {
  try { return window.localStorage.getItem(LAST_ZONE_KEY) ?? '' } catch { return '' }
}
function saveLastZone(v: string) {
  try { window.localStorage.setItem(LAST_ZONE_KEY, v) } catch { /* 忽略 */ }
}
