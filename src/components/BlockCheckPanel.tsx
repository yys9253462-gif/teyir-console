import { useCallback, useEffect, useRef, useState } from 'react'
import { Loader2, Play, ShieldAlert } from 'lucide-react'
import { api, loadSession, type BlockChecks, type Server } from '../lib/api'

// 被墙检测（2026-10-15）。挂在节点监控页底部。
//
// 面板在海外测不出「国内连不上」—— 需要一台在中国大陆、能 SSH 的主机当探测点。
// 选好后每 30 分钟自动从它探测所有节点（TCP + ping），被墙会进告警中心。

function formatCheckedAt(iso: string | null | undefined): string {
  if (!iso) return '—'
  const date = new Date(iso)
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString('zh-CN', { hour12: false })
}

export function BlockCheckPanel({ notify }: { notify: (m: string) => void }) {
  const admin = loadSession()?.role === 'admin'
  const [data, setData] = useState<BlockChecks | null>(null)
  const [servers, setServers] = useState<Server[]>([])
  const [running, setRunning] = useState(false)
  const [savingProbe, setSavingProbe] = useState(false)
  // 用 null 表示未手动修改，空字符串 '' 表示用户主动选择「— 不使用 —」，从而支持取消已配置的国内探测点。
  const [choosing, setChoosing] = useState<string | null>(null)
  const [loadError, setLoadError] = useState('')
  const mountedRef = useRef(true)
  const loadSeq = useRef(0)

  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      loadSeq.current += 1
    }
  }, [])

  const load = useCallback(async () => {
    const seq = ++loadSeq.current
    try {
      const [d, s] = await Promise.all([api.blockChecks(), api.servers()])
      if (!mountedRef.current || seq !== loadSeq.current) return
      setData(d)
      setServers(s ?? [])
      setLoadError('')
    } catch (err) {
      if (!mountedRef.current || seq !== loadSeq.current) return
      // 不阻断节点监控页主体，但要留痕：原来静默吞掉，这块面板直接消失，看不出是读失败。
      setLoadError(err instanceof Error ? err.message : '读取失败')
    }
  }, [])
  useEffect(() => { void load() }, [load])

  async function run() {
    if (running) return
    setRunning(true)
    try {
      const r = await api.runBlockChecks()
      if (!mountedRef.current) return
      notify(`已从国内探测 ${r.checked} 个节点`)
      await load()
    } catch (err) {
      if (!mountedRef.current) return
      notify(err instanceof Error ? err.message : '检测失败')
    } finally {
      if (mountedRef.current) setRunning(false)
    }
  }

  async function setProbe(id: string) {
    if (savingProbe) return
    setSavingProbe(true)
    try {
      await api.setCNProbe(id)
      if (!mountedRef.current) return
      notify(id ? '已设置国内探测点' : '已取消国内探测点')
      setChoosing(null)
      await load()
    } catch (err) {
      if (!mountedRef.current) return
      notify(err instanceof Error ? err.message : '设置失败')
    } finally {
      if (mountedRef.current) setSavingProbe(false)
    }
  }

  if (!data) {
    if (!loadError) return null
    return (
      <section className="panel settings-panel" style={{ marginTop: 14 }}>
        <div className="section-head"><div><div className="section-kicker">GFW <span>·</span> CN REACHABILITY</div><h2>被墙检测</h2></div></div>
        <p className="form-error">读取被墙检测结果失败：{loadError}。刷新页面重试。</p>
      </section>
    )
  }
  const candidates = servers.filter((s) => s.has_credential)
  const items = data.items ?? []
  const blocked = items.filter((i) => i.blocked)
  const currentProbeId = data.probe?.server_id ?? ''
  const selectedProbeId = choosing ?? currentProbeId

  return (
    <section className="panel settings-panel" style={{ marginTop: 14 }}>
      <div className="section-head">
        <div><div className="section-kicker">GFW <span>·</span> CN REACHABILITY</div><h2>被墙检测</h2></div>
        {data.probe && admin && <button className="button button-quiet" disabled={running} onClick={() => void run()}>{running ? <Loader2 size={15} className="spin" /> : <Play size={15} />}{running ? '检测中…' : '立即检测'}</button>}
      </div>

      <div className="block-probe">
        <span>国内探测点：{data.probe ? <strong>{data.probe.name}（{data.probe.host}）</strong> : <strong className="lat-bad">{data.probe_missing ? '已配置的探测点主机已被删除，请重新选择' : '未设置'}</strong>}</span>
        {admin && (
          <span className="block-probe-pick">
            <select value={selectedProbeId} disabled={savingProbe} onChange={(e) => setChoosing(e.target.value)} aria-label="选择国内探测点">
              <option value="">— 不使用 —</option>
              {candidates.map((s) => <option key={s.id} value={s.id}>{s.name}（{s.host}）</option>)}
            </select>
            {choosing !== null && choosing !== currentProbeId && (
              <button className="row-action" disabled={savingProbe} onClick={() => void setProbe(choosing)}>{savingProbe ? '保存中…' : '保存'}</button>
            )}
          </span>
        )}
      </div>

      {!data.probe ? (
        <p className="form-note form-note-hint"><ShieldAlert size={13} /><span>
          面板在海外，测不出「国内连不上」；公开拨测服务在大陆也没有节点。需要一台<strong>位于中国大陆、能 SSH 的机器</strong>当探测点
          （便宜的国内 VPS、家里的软路由 / 树莓派都行）：先在「VPS 主机」添加它并上传 SSH 凭据，再在上面选中。之后每 30 分钟自动检测，被墙会进告警中心。
        </span></p>
      ) : (
        <>
          {blocked.length > 0 && <p className="form-note" style={{ color: 'var(--red)' }}>⚠️ {blocked.length} 个节点疑似被墙：{blocked.map((b) => b.node_name).join('、')}。可在「VPS 主机 → 主机工具 → 迁移节点」换一台机器，客户订阅不变。</p>}
          <div className="server-table-wrap"><table className="server-table">
            <thead><tr><th>节点</th><th>国内可达性</th><th>TCP</th><th>Ping</th><th>说明</th><th>检测时间</th></tr></thead>
            <tbody>{items.map((i) => (
              <tr key={i.node_id}>
                <td><div className="host-cell"><div><strong>{i.node_name}</strong><span>{i.target_ip || '—'}{i.tcp_port ? `:${i.tcp_port}` : ''}</span></div></div></td>
                <td>{i.blocked == null ? <span className="traffic-sub">未检测</span> : i.blocked ? <span className="expiry-badge tone-bad">疑似被墙</span> : <span className="expiry-badge tone-ok">国内可达</span>}</td>
                <td><span className={i.tcp_ok ? 'lat-good' : i.tcp_ok === false ? 'lat-bad' : 'traffic-sub'}>{i.tcp_ok == null ? '—' : i.tcp_ok ? '通' : '不通'}</span></td>
                <td><span className={i.ping_ok ? 'lat-good' : i.ping_ok === false ? 'lat-bad' : 'traffic-sub'}>{i.ping_ok == null ? '—' : i.ping_ok ? '通' : '不通'}</span></td>
                <td><span className="traffic-sub">{i.detail || '—'}</span></td>
                <td><span className="traffic-sub">{formatCheckedAt(i.checked_at)}</span></td>
              </tr>
            ))}</tbody>
          </table></div>
        </>
      )}
    </section>
  )
}
