import { Fragment, useEffect, useRef, useState } from 'react'
import { AlertTriangle, Bell, Check, CheckCircle2, ChevronDown, ChevronRight, History, PowerOff, RotateCcw, ShieldCheck } from 'lucide-react'
import { api, isAdminSession, loadSession, type Alert, type AlertEvent } from '../lib/api'
import { askConfirm } from './ConfirmHost'

const severityText: Record<Alert['severity'], string> = { info: '提示', warning: '警告', critical: '严重' }
const severityClass: Record<Alert['severity'], string> = { info: 'online', warning: 'warning', critical: 'offline critical' }

// 2026-11-08：到期账号收不回来的两类告警。机器确实已下线时，管理员可以在这里一步确认：
// 删除节点（勾选「机器已永久停用」的同一条路径），收不回的账号登记后结案，告警随后自动消除。
const revokeAlertTypes = new Set(['node_revoke_exhausted', 'node_subscription_lingering'])

// Plain-language rendering of each timeline entry. The backend stores terse
// machine states; a non-technical operator needs to know what happened and,
// where it matters, what they are expected to do about it.
const eventText: Record<AlertEvent['event_type'], { label: string; hint: string }> = {
  triggered: { label: '告警触发', hint: '这个问题第一次出现。' },
  reopened: { label: '故障复现', hint: '之前已经恢复过，现在又出现了 —— 说明根因没解决，别只重启了事。' },
  repeated: { label: '持续存在', hint: '这个问题一直没有消失。' },
  acknowledged: { label: '已确认处理', hint: '已经有人认领这条告警。' },
  resolved: { label: '已自动恢复', hint: '对象状态恢复正常，告警自动归档。' },
}

function formatDateTime(iso: string): string {
  const date = new Date(iso)
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString('zh-CN', { hour12: false })
}

export function AlertsPage({ alerts, onChanged, notify, stale = false }: {
  alerts: Alert[]
  onChanged: () => Promise<void> | void
  notify: (message: string) => void
  // 告警接口这一轮没取到：列表是上次的数据，空列表也不能说「一切正常」。
  stale?: boolean
}) {
  const [busy, setBusy] = useState<string | null>(null)
  const [showResolved, setShowResolved] = useState(false)
  const [resolved, setResolved] = useState<Alert[]>([])
  const [historyLoading, setHistoryLoading] = useState(false)
  // Which alert's timeline is expanded, plus a small cache so re-expanding the
  // same row does not re-hit the API on every click.
  const [expanded, setExpanded] = useState<string | null>(null)
  const [timelines, setTimelines] = useState<Record<string, AlertEvent[]>>({})
  const [timelineLoading, setTimelineLoading] = useState<string | null>(null)
  // 告警外发配置。只存 host（服务端不回完整 URL）。
  const [webhook, setWebhook] = useState<{ configured: boolean; host: string }>({ configured: false, host: '' })
  const [webhookDraft, setWebhookDraft] = useState('')
  // 2026-11-04：读取失败单独标记。原来读失败写成 configured:false，徽章显示「未配置」——
  // 已配好的外发看起来像没配，运维可能重填一遍或以为告警不会外发。
  const [webhookLoadError, setWebhookLoadError] = useState('')
  // 外发地址只有管理员能改（PUT /api/alerts/webhook 是 requireAdmin）。
  const canEditWebhook = isAdminSession()
  const viewer = loadSession()?.role === 'viewer'
  const resolvedLoadedRef = useRef(false)
  const mountedRef = useRef(true)
  const timelineSeq = useRef(0)

  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      timelineSeq.current += 1
    }
  }, [])

  useEffect(() => {
    let alive = true
    // 读失败不阻断页面 —— 它是增强信息，不该让告警页打不开；但要如实显示「读取失败」。
    api.alertWebhook()
      .then((res) => { if (alive) { setWebhook(res); setWebhookLoadError('') } })
      .catch((err) => { if (alive) setWebhookLoadError(err instanceof Error ? err.message : '读取失败') })
    return () => { alive = false }
  }, [])

  async function toggleTimeline(alert: Alert) {
    if (expanded === alert.id) {
      setExpanded(null)
      return
    }
    setExpanded(alert.id)
    if (timelines[alert.id]) return
    const seq = ++timelineSeq.current
    setTimelineLoading(alert.id)
    try {
      const events = await api.alertEvents(alert.id)
      if (!mountedRef.current) return
      setTimelines((current) => ({ ...current, [alert.id]: events ?? [] }))
    } catch (err) {
      if (!mountedRef.current || seq !== timelineSeq.current) return
      notify(err instanceof Error ? `时间线读取失败：${err.message}` : '时间线读取失败')
    } finally {
      if (mountedRef.current && seq === timelineSeq.current) setTimelineLoading(null)
    }
  }

  useEffect(() => {
    if (!showResolved) {
      resolvedLoadedRef.current = false
      return
    }
    let alive = true
    if (!resolvedLoadedRef.current) setHistoryLoading(true)
    api.alerts(true)
      .then((list) => {
        if (!alive) return
        resolvedLoadedRef.current = true
        setResolved((list ?? []).filter((alert) => alert.status === 'resolved'))
      })
      .catch((err) => {
        if (alive) notify(err instanceof Error ? `历史告警读取失败：${err.message}` : '历史告警读取失败')
      })
      .finally(() => {
        if (alive) setHistoryLoading(false)
      })
    return () => { alive = false }
  }, [showResolved, alerts, notify])

  async function acknowledge(alert: Alert) {
    if (busy) return
    setBusy(alert.id)
    try {
      await api.acknowledgeAlert(alert.id)
      if (!mountedRef.current) return
      // 2026-11-06：清掉这条的时间线缓存。原来缓存一直不清，确认之后再展开看不到刚写入的「已确认」事件。
      setTimelines(({ [alert.id]: _stale, ...rest }) => rest)
      notify(`已确认告警：${alert.server_name || alert.node_name}`)
      await onChanged()
    } catch (err) {
      if (!mountedRef.current) return
      notify(err instanceof Error ? `确认失败：${err.message}` : '确认失败')
    } finally {
      if (mountedRef.current) setBusy(null)
    }
  }

  async function confirmMachineOffline(alert: Alert) {
    if (!alert.node_id || busy) return
    const ok = await askConfirm(
      `确认「${alert.node_name}」所在的机器已经永久下线（关机、退租或被封且不会恢复）？\n\n` +
      '确认后会删除这个节点：能注销的账号照常注销，注销不了的登记后直接结案，不再进入待补删、不再告警。\n' +
      '这一步不可撤销。如果机器只是暂时连不上，请不要点，等它恢复后系统会自动重试回收。',
      { confirmText: '确认已下线并删除节点', danger: true },
    )
    if (!ok || !mountedRef.current) return
    setBusy(alert.id)
    try {
      const result = await api.deleteNode(alert.node_id, true)
      if (!mountedRef.current) return
      notify(result.status === 'deleted_decommissioned'
        ? `已删除节点 ${alert.node_name}，收不回的账号已登记结案`
        : `已删除节点 ${alert.node_name}`)
      await onChanged()
    } catch (err) {
      if (!mountedRef.current) return
      notify(err instanceof Error ? `操作失败：${err.message}` : '操作失败')
    } finally {
      if (mountedRef.current) setBusy(null)
    }
  }

  const open = alerts.filter((alert) => alert.status === 'open')
  const acknowledged = alerts.filter((alert) => alert.status === 'acknowledged')

  return (
    <>
      <div className="page-heading"><div><div className="eyebrow"><span className="eyebrow-line" />ALERTS / 04</div><h1>告警中心<span className="heading-period">.</span></h1><p className="page-subtitle">由主机与节点真实状态派生，可逐条确认</p></div></div>
      <section className="metrics-grid" aria-label="告警指标">
        <MetricCard label="待处理" value={open.length.toString()} unit="条" icon={<Bell size={17} />} tone="amber" foot={<span>未确认的告警</span>} />
        <MetricCard label="严重" value={alerts.filter((a) => a.severity === 'critical').length.toString()} unit="条" icon={<AlertTriangle size={17} />} tone="amber" foot={<span>主机不可达或端口未监听</span>} />
        <MetricCard label="警告" value={alerts.filter((a) => a.severity === 'warning').length.toString()} unit="条" icon={<AlertTriangle size={17} />} tone="neutral" foot={<span>资源阈值与采集异常</span>} />
        <MetricCard label="已确认" value={acknowledged.length.toString()} unit="条" icon={<Check size={17} />} tone="green" foot={<span>本轮已认领处理</span>} />
      </section>

      {/* 告警外发配置。
          为什么放在告警页而不是设置页：它是「这条告警怎么通知我」的直接延伸，
          放在告警旁边才符合心智 —— 设置页里那一堆选项没人会翻。
          ⚠️ 不回填 URL 到输入框：服务端只返回 host（URL 带 access_token，
          多管理员可见）。回填空值会导致一次误保存就清掉配置。 */}
      <section className="panel servers-panel">
        <div className="section-head servers-head">
          <div>
            <div className="section-kicker">NOTIFY <span>·</span> WEBHOOK</div>
            <h2>告警外发</h2>
          </div>
          {/* ⚠️ 不套 heading-actions —— 它带 flex-wrap，徽章在窄容器里
              会被折行并挤成竖排（截图复核时发现的）。
              直接作为 section-head 的第二个 flex 子项，配合
              justify-content:space-between 自然靠右。 */}
          <span className={`status-badge${webhookLoadError ? ' warning' : webhook.configured ? ' online' : ' offline'}`}>
            <i />{webhookLoadError ? '读取失败' : webhook.configured ? '已配置' : '未配置'}
          </span>
        </div>
        <div className="panel-body">
          <p className="form-note">
            填任意一个能收 POST JSON 的地址即可 —— 钉钉、企业微信、飞书、Telegram Bot
            的「机器人 Webhook」都适用，不需要额外配置。告警触发、恢复、被确认时都会推一条。
            同一问题的重复告警 1 小时内只推一次，不会刷屏。
          </p>
          {webhookLoadError && (
            <p className="form-error">读取外发配置失败：{webhookLoadError}。当前是否已配置未知，刷新页面重试。</p>
          )}
          {!canEditWebhook && (
            <p className="form-note">仅管理员可修改告警外发地址。</p>
          )}
          {webhook.configured && (
            <p className="form-note">
              当前指向 <code>{webhook.host}</code>（出于安全只显示主机名，不显示完整地址与令牌）。
            </p>
          )}
          <div className="webhook-form-row">
            <input
              className="text-input"
              type="url"
              placeholder={webhook.configured ? '粘贴新地址可覆盖，留空并保存则关闭外发' : 'https://oapi.dingtalk.com/robot/send?access_token=…'}
              value={webhookDraft}
              onChange={(e) => setWebhookDraft(e.target.value)}
              disabled={busy === 'webhook' || !canEditWebhook}
            />
            <button
              className="button button-primary"
              disabled={busy === 'webhook' || !canEditWebhook}
              title={canEditWebhook ? undefined : '仅管理员可修改'}
              onClick={async () => {
                if (busy === 'webhook') return
                // 留空保存 = 关闭外发，与「关闭外发」按钮同样先确认。
                if (!webhookDraft.trim() && !await askConfirm('关闭告警外发？之后告警只在面板内显示，不再推送到外部地址。', { confirmText: '关闭外发', danger: true })) return
                if (!mountedRef.current) return
                setBusy('webhook')
                try {
                  const res = await api.setAlertWebhook(webhookDraft.trim())
                  if (!mountedRef.current) return
                  setWebhook(res)
                  setWebhookLoadError('')
                  setWebhookDraft('')
                  notify(res.configured ? `告警外发已指向 ${res.host}` : '已关闭告警外发')
                } catch (error) {
                  if (!mountedRef.current) return
                  notify(error instanceof Error ? error.message : '保存失败')
                } finally {
                  if (mountedRef.current) setBusy(null)
                }
              }}
            >
              {busy === 'webhook' ? '保存中…' : '保存'}
            </button>
            {webhook.configured && (
              <button
                className="button button-quiet"
                disabled={busy === 'webhook' || !canEditWebhook}
                title={canEditWebhook ? undefined : '仅管理员可修改'}
                onClick={async () => {
                  if (busy === 'webhook') return
                  // 2026-11-04：关闭前确认。原来一点就清空外发地址，误点后所有告警都不再外发。
                  if (!await askConfirm('关闭告警外发？之后告警只在面板内显示，不再推送到外部地址。', { confirmText: '关闭外发', danger: true })) return
                  if (!mountedRef.current) return
                  setBusy('webhook')
                  try {
                    const res = await api.setAlertWebhook('')
                    if (!mountedRef.current) return
                    setWebhook(res)
                    setWebhookLoadError('')
                    notify('已关闭告警外发')
                  } catch (error) {
                    if (!mountedRef.current) return
                    notify(error instanceof Error ? error.message : '关闭失败')
                  } finally {
                    if (mountedRef.current) setBusy(null)
                  }
                }}
              >
                关闭外发
              </button>
            )}
          </div>
          <p className="form-note">
            只接受 http/https，且不能填本机回环地址（<code>127.0.0.1</code> / <code>localhost</code>）——
            那会让面板变成一个可被利用的本机请求发起器。内网地址是允许的。
          </p>
        </div>
      </section>

      <section className="panel servers-panel">
        <div className="section-head servers-head"><div><div className="section-kicker">ACTIVE ALERTS <span>·</span> {alerts.length.toString().padStart(2, '0')}</div><h2>告警列表 <span className="count-chip">{alerts.length.toString().padStart(2, '0')}</span></h2></div><div className="heading-actions"><button className={`button ${showResolved ? 'button-primary' : 'button-quiet'}`} aria-pressed={showResolved} onClick={() => setShowResolved((current) => !current)}><History size={14} />{showResolved ? '隐藏已解决' : '显示已解决'}</button></div></div>
        {alerts.length === 0 && stale ? (
          <div className="empty-state"><AlertTriangle size={20} /><strong>告警未能读取</strong><span>告警接口这次请求失败，无法确认当前是否有未处理告警；稍后会自动重试。</span></div>
        ) : alerts.length === 0 ? (
          <div className="empty-state"><CheckCircle2 size={20} /><strong>一切正常</strong><span>当前没有未处理告警。主机离线、资源超阈值或节点端口未监听都会在此出现。</span></div>
        ) : (
          <div className="server-table-wrap"><table className="server-table">
            <thead><tr><th></th><th>对象</th><th>级别</th><th>状态</th><th>描述</th><th>首次 / 最近</th><th></th></tr></thead>
            <tbody>
              {alerts.map((alert) => {
                const isOpen = expanded === alert.id
                const events = timelines[alert.id]
                const recurred = (events ?? []).some((event) => event.event_type === 'reopened')
                return (
                  <Fragment key={alert.id}>
                    <tr>
                      <td>
                        <button className="row-more" aria-label={isOpen ? '收起时间线' : '展开时间线'} aria-expanded={isOpen} onClick={() => toggleTimeline(alert)}>
                          {isOpen ? <ChevronDown size={15} /> : <ChevronRight size={15} />}
                        </button>
                      </td>
                      <td><div className="host-cell"><div className={`host-icon ${severityClass[alert.severity]}`}><AlertTriangle size={15} /></div><div><strong>{alert.node_name || alert.server_name}</strong><span>{alert.node_name ? `端口 ${alert.node_port}` : alert.host}</span></div></div></td>
                      <td><span className={`status-badge ${severityClass[alert.severity]}`}><i />{severityText[alert.severity]}</span></td>
                      <td>{alert.status === 'open' ? <span className="status-badge warning"><i />待处理</span> : <span className="status-badge online"><i />已确认</span>}</td>
                      <td><span className="traffic-sub">{alert.message}</span>{recurred && <span className="alert-recur-tag"><RotateCcw size={12} />反复出现过</span>}</td>
                      <td><span className="traffic-sub">{formatDateTime(alert.first_seen_at)}<br />{formatDateTime(alert.last_seen_at)}</span></td>
                      <td><div className="row-actions">
                        {canEditWebhook && alert.node_id && revokeAlertTypes.has(alert.type) && alert.status !== 'resolved' && (
                          <button className="button button-quiet button-compact" disabled={busy === alert.id} title="机器已永久下线时使用：删除节点并结案收不回的账号" onClick={() => confirmMachineOffline(alert)}>
                            <PowerOff size={14} />确认机器已下线
                          </button>
                        )}
                        {alert.status === 'open' && <button className="row-more" aria-label="确认告警" title={viewer ? '只读账号不能确认告警' : '确认告警'} disabled={viewer || busy === alert.id} onClick={() => acknowledge(alert)}><Check size={15} /></button>}
                      </div></td>
                    </tr>
                    {isOpen && (
                      <tr className="alert-timeline-row">
                        <td colSpan={7}>
                          <div className="alert-timeline">
                            <div className="alert-timeline-head"><History size={14} /><strong>这条告警的经过</strong>
                              <span className="alert-timeline-sub">点这里能看到它是只出过一次，还是反复发生 —— 反复出现说明上次没修到根因。</span>
                            </div>
                            {timelineLoading === alert.id
                              ? <p className="form-note">读取中…</p>
                              : (events ?? []).length === 0
                                ? <p className="form-note">暂无记录。时间线从本次告警触发开始累积。</p>
                                : (
                                  <ol className="alert-timeline-list">
                                    {events!.map((event) => {
                                      const meta = eventText[event.event_type] ?? { label: event.event_type, hint: '' }
                                      const by = typeof event.details?.by === 'string' ? event.details.by : ''
                                      return (
                                        <li key={event.id}>
                                          <span className={`timeline-dot ${event.event_type}`} />
                                          <div>
                                            <strong>{meta.label}</strong>
                                            <time>{formatDateTime(event.created_at)}</time>
                                            {meta.hint && <small>{meta.hint}</small>}
                                            {by && <small>操作人：{by}</small>}
                                          </div>
                                        </li>
                                      )
                                    })}
                                  </ol>
                                )}
                          </div>
                        </td>
                      </tr>
                    )}
                  </Fragment>
                )
              })}
            </tbody>
          </table></div>
        )}
        {showResolved && (
          <div className="alert-history">
            <div className="section-head"><div><div className="section-kicker">RESOLVED <span>·</span> {resolved.length.toString().padStart(2, '0')}</div><h2>已解决归档</h2></div></div>
            {historyLoading ? <p className="form-note">读取中…</p> : resolved.length === 0
              ? <p className="form-note">暂无已解决记录。告警在对象状态恢复后会自动归档到这里。</p>
              : (
                <div className="server-table-wrap"><table className="server-table">
                  <thead><tr><th>对象</th><th>级别</th><th>描述</th><th>首次</th><th>解决时间</th></tr></thead>
                  <tbody>{resolved.map((alert) => (
                    <tr key={alert.id}>
                      <td><div className="host-cell"><div className="host-icon online"><CheckCircle2 size={15} /></div><div><strong>{alert.node_name || alert.server_name}</strong><span>{alert.node_name ? `端口 ${alert.node_port}` : alert.host}</span></div></div></td>
                      <td><span className={`status-badge ${severityClass[alert.severity]}`}><i />{severityText[alert.severity]}</span></td>
                      <td><span className="traffic-sub">{alert.message}</span></td>
                      <td><span className="traffic-sub">{formatDateTime(alert.first_seen_at)}</span></td>
                      <td><span className="traffic-sub">{alert.resolved_at ? formatDateTime(alert.resolved_at) : '—'}</span></td>
                    </tr>
                  ))}</tbody>
                </table></div>
              )}
          </div>
        )}
        <div className="table-foot"><span>告警在状态恢复后自动转为已解决</span><span>15 秒自动刷新</span></div>
      </section>
    </>
  )
}
function MetricCard({ label, value, unit, icon, tone, foot }: { label: string; value: string; unit: string; icon: React.ReactNode; tone: string; foot: React.ReactNode }) {
  return <div className={`metric-card tone-${tone}`}><div className="metric-top"><span>{label}</span><span className="metric-icon">{icon}</span></div><div className="metric-value">{value}<small>{unit}</small></div><div className="metric-foot">{foot}</div></div>
}
