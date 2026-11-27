import { useCallback, useEffect, useRef, useState } from 'react'
import { api, isAdminSession, type Hy2Node, type PortalUpdateBatch, type PortalUpdateStatus } from '../lib/api'
import { Modal } from './Modal'
import { askConfirm } from './ConfirmHost'

const statusText: Record<string, string> = {
  queued: '等待', running: '执行中', success: '已核验', warning: '已更新 · 计量告警',
  failed: '失败', skipped: '未下发', cancelled: '已取消',
}
const batchStatusText: Record<string, string> = {
  running: '执行中', completed: '已完成', interrupted: '已中断', cancelled: '已取消', queued: '排队中',
}
const doneOK = new Set(['success'])

// 批量更新门户（2026-10-15 改版）：
//   - 打开即检查每台节点的门户是否已是 GitHub 最新（逐文件比对指纹，见
//     portalupdatestatus.go），「选择需要更新的节点」只勾过时的，
//     不再把已是最新的节点也重启一遍。
//   - 结果区默认只列有问题的节点；全部成功时一行总结即可。
export function PortalBatchUpdate({ nodes, onClose, notify }: {
  nodes: Hy2Node[]; onClose: () => void; notify: (message: string) => void
}) {
  const eligible = nodes.filter(n => n.has_basic)
  const [selected, setSelected] = useState<string[]>([])
  const [batch, setBatch] = useState<PortalUpdateBatch | null>(null)
  // 2026-11-06：发起 / 停止批量更新后端是 requireAdmin；原来只挡 viewer，operator 点了才吃 403。
  const readOnly = !isAdminSession()
  const [history, setHistory] = useState<Pick<PortalUpdateBatch, 'id' | 'status' | 'created_at' | 'updated_at'>[]>([])
  const [starting, setStarting] = useState(false)
  const [canceling, setCanceling] = useState(false)
  const [error, setError] = useState('')
  const [status, setStatus] = useState<PortalUpdateStatus | null>(null)
  const [checking, setChecking] = useState(false)
  const [showAllResults, setShowAllResults] = useState(false)
  const running = batch?.status === 'running'
  const statusSeq = useRef(0)
  const batchSelectSeq = useRef(0)
  const mountedRef = useRef(true)
  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      statusSeq.current++
      batchSelectSeq.current++
    }
  }, [])

  const loadStatus = useCallback(async (refresh: boolean) => {
    const seq = ++statusSeq.current
    setChecking(true)
    try {
      const next = await api.portalUpdateStatus(refresh)
      if (!mountedRef.current || seq !== statusSeq.current) return
      setStatus(next)
    } catch (err) {
      if (!mountedRef.current || seq !== statusSeq.current) return
      setStatus({ error: `检查失败：${err instanceof Error ? err.message : String(err)}`, nodes: [], checked_at: '' })
    } finally {
      if (mountedRef.current && seq === statusSeq.current) setChecking(false)
    }
  }, [])

  useEffect(() => {
    let alive = true
    void loadStatus(false)
    api.portalUpdateBatchHistory().then(value => { if (alive) setHistory(value) }).catch(() => {})
    api.portalUpdateBatch('latest').then(value => { if (alive) setBatch(value) }).catch(err => {
      if (alive && err.status !== 404) setError(`读取最近任务失败：${err.message}`)
    })
    return () => { alive = false }
  }, [loadStatus])

  useEffect(() => {
    if (!batch || !running) return
    let alive = true
    let timer: ReturnType<typeof setTimeout> | undefined
    const id = batch.id
    async function poll() {
      if (!alive || document.hidden) return
      try {
        const value = await api.portalUpdateBatch(id)
        if (!alive) return
        setBatch(value); setError('')
        if (value.status !== 'running') return
      } catch (err) {
        if (!alive) return
        setError(`进度读取失败，后台任务仍可能在执行：${err instanceof Error ? err.message : String(err)}`)
      }
      if (alive && !document.hidden) timer = setTimeout(poll, 3000)
    }
    const onVisible = () => {
      if (!document.hidden && alive) {
        clearTimeout(timer)
        void poll()
      }
    }
    document.addEventListener('visibilitychange', onVisible)
    if (!document.hidden) timer = setTimeout(poll, 1000)
    return () => {
      alive = false
      clearTimeout(timer)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [batch?.id, running])

  // 任务从「执行中」变为结束时，重新检查一遍各节点是否已是最新。
  const wasRunning = useRef(false)
  useEffect(() => {
    if (wasRunning.current && !running) {
      void loadStatus(true)
      api.portalUpdateBatchHistory().then((value) => { if (mountedRef.current) setHistory(value) }).catch(() => {})
    }
    wasRunning.current = running
  }, [running, loadStatus])

  const statusById = new Map((status?.nodes ?? []).map(n => [n.node_id, n]))
  const outdated = eligible.filter(n => statusById.get(n.id)?.up_to_date === false)
  const upToDate = eligible.filter(n => statusById.get(n.id)?.up_to_date === true)

  async function start() {
    if (readOnly || !selected.length || selected.length > 20 || running || starting) return
    if (!await askConfirm(`确认更新 ${selected.length} 个节点的门户？\n\n逐台进行，每台门户会重启几秒，期间不影响用户上网。`)) return
    setStarting(true); setError('')
    try {
      const nextBatch = await api.startPortalUpdateBatch(selected)
      if (!mountedRef.current) return
      setBatch(nextBatch)
      setSelected([])
      setShowAllResults(false)
      api.portalUpdateBatchHistory().then((value) => { if (mountedRef.current) setHistory(value) }).catch(() => {})
      notify('已开始更新；可以关闭这个窗口，更新会在后台继续')
    } catch (err) {
      if (!mountedRef.current) return
      setError(err instanceof Error ? err.message : String(err))
      // 若已有批次，展示它而不是诱导用户重复点击。
      api.portalUpdateBatch('latest').then((value) => { if (mountedRef.current) setBatch(value) }).catch(() => {})
    } finally { if (mountedRef.current) setStarting(false) }
  }

  const counts = (batch?.results ?? []).reduce<Record<string, number>>((out, item) => {
    out[item.status] = (out[item.status] || 0) + 1; return out
  }, {})
  const problemResults = (batch?.results ?? []).filter(item => !doneOK.has(item.status))
  const visibleResults = showAllResults || running ? (batch?.results ?? []) : problemResults
  const retryable = (batch?.results ?? []).filter(item => ['failed', 'skipped', 'warning', 'running', 'queued'].includes(item.status) && eligible.some(n => n.id === item.node_id))

  function badge(node: Hy2Node) {
    if (!node.has_basic) return <small style={{ color: 'var(--muted)' }}>未绑定门户管理账号，无法更新（请重新接入完整凭据）</small>
    const st = statusById.get(node.id)
    if (!st) return <small style={{ color: 'var(--muted)' }}>{checking ? '检查中…' : '—'}</small>
    const version = st.portal_version ? `门户 ${st.portal_version}` : ''
    if (st.error) return <small style={{ color: 'var(--muted)' }} title={st.error}>{version} · 无法检查</small>
    if (st.up_to_date === true) return <small style={{ color: 'var(--green)' }}>{version} · 已是最新</small>
    if (st.up_to_date === false) return <small style={{ color: 'var(--amber-text)' }} title={`与最新版不同的文件：${(st.outdated_files ?? []).join('、')}`}>{version} · 需要更新</small>
    return <small style={{ color: 'var(--muted)' }}>{version}</small>
  }

  const repoUnconfigured = Boolean(status?.error?.includes('HY2_INSTALLER_REPO'))

  return <Modal title="批量更新门户" kicker="PORTAL BATCH UPDATE" wide onClose={onClose}>
    <div style={{ color: 'var(--text)', fontFamily: 'var(--sans)', fontSize: 'var(--fs-sm)', lineHeight: 1.65 }}>
    <p>把节点上的管理门户更新到环境变量 <code>HY2_INSTALLER_REPO</code> 对应仓库的最新版本。逐台进行，每台门户重启几秒，<strong>不影响用户上网</strong>，也不会升级 Hysteria 内核或改动用户。关闭窗口后更新会在后台继续。</p>

    <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap', alignItems: 'center', margin: '4px 0 10px' }}>
      <span>
        {checking ? '正在检查各节点版本…'
          : status?.error ? <span style={{ color: 'var(--amber-text)' }}>{status.error}</span>
          : status ? <>需要更新 <strong>{outdated.length}</strong> 台 · 已是最新 {upToDate.length} 台</>
          : null}
      </span>
      <button className="button button-quiet" disabled={checking || running} onClick={() => void loadStatus(true)}>重新检查</button>
    </div>
    {repoUnconfigured && (
      <p className="form-note service-warn" style={{ marginBottom: 12 }}>
        <span>开源自托管版本默认不绑定特定上游仓库。请在服务端环境变量中设置 <code>HY2_INSTALLER_REPO=owner/repo</code> 并重启后端，即可启用版本比对与批量更新。</span>
      </p>
    )}

    <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap', marginBottom: 12 }}>
      <button className="button button-quiet" disabled={readOnly || running || starting || !outdated.length} onClick={() => setSelected(outdated.slice(0, 20).map(n => n.id))}>选择需要更新的（{outdated.length}）</button>
      {batch && !running && retryable.length > 0 && <button className="button button-quiet" disabled={readOnly || starting} onClick={() => setSelected(retryable.slice(0, 20).map(item => item.node_id))}>选择上次未成功的（{retryable.length}）</button>}
      <button className="button button-quiet" disabled={readOnly || running || starting || !selected.length} onClick={() => setSelected([])}>清空选择</button>
      <button className="button button-primary" disabled={readOnly || running || starting || !selected.length || selected.length > 20} onClick={() => void start()}>{starting ? '创建任务…' : running ? '后台更新中…' : `开始更新（${selected.length}）`}</button>
      {running && !readOnly && <button className="button button-quiet" disabled={batch?.cancel_requested || canceling} onClick={async () => {
        if (!batch || canceling || !await askConfirm('当前这台完成后，停止更新剩余节点？')) return
        setCanceling(true)
        try {
          const result = await api.cancelPortalUpdateBatch(batch.id)
          notify(result.message)
          const latest = await api.portalUpdateBatch(batch.id)
          if (mountedRef.current) setBatch(latest)
        } catch (err) {
          if (mountedRef.current) setError(err instanceof Error ? err.message : String(err))
        } finally {
          if (mountedRef.current) setCanceling(false)
        }
      }}>{canceling ? '正在请求停止…' : batch?.cancel_requested ? '已请求停止' : '停止剩余节点'}</button>}
    </div>
    {readOnly && <p>仅管理员可以发起或停止批量更新，当前账号只能查看进度。</p>}
    {selected.length > 20 && <p style={{ color: 'var(--amber-text)' }}>每批最多 20 台。</p>}

    <div style={{ display: 'grid', gap: 6, marginBottom: 18 }}>
      {nodes.map(node => <label key={node.id} style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
        <input type="checkbox" checked={selected.includes(node.id)} disabled={readOnly || !node.has_basic || running || starting}
          onChange={e => setSelected(ids => e.target.checked ? [...ids, node.id] : ids.filter(id => id !== node.id))} />
        <span style={{ flexShrink: 0 }}>{node.name}</span>
        {badge(node)}
      </label>)}
      {!nodes.length && <p>还没有节点，请先接入节点。</p>}
    </div>

    {error && <p role="alert" style={{ color: 'var(--amber-text)' }}>{error}</p>}

    {batch && <section aria-label="更新任务进度" style={{ borderTop: '1px solid var(--line)', paddingTop: 12 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
        <h3 style={{ margin: 0 }}>{running ? '正在更新' : batch.status === 'interrupted' ? '上次更新被中断' : '上次更新结果'}</h3>
        {history.length > 1 && <select value={batch.id} onChange={async e => {
          const seq = ++batchSelectSeq.current
          try {
            const next = await api.portalUpdateBatch(e.target.value)
            if (!mountedRef.current || seq !== batchSelectSeq.current) return
            setBatch(next)
            setShowAllResults(false)
          } catch (err) {
            if (!mountedRef.current || seq !== batchSelectSeq.current) return
            setError(err instanceof Error ? err.message : String(err))
          }
        }}>
          {history.map(item => <option key={item.id} value={item.id}>{new Date(item.created_at).toLocaleString('zh-CN', { hour12: false })} · {batchStatusText[item.status] || item.status}</option>)}
        </select>}
      </div>
      <p style={{ margin: '6px 0' }}>
        成功 {counts.success || 0}
        {(counts.warning || 0) > 0 && <> · <span style={{ color: 'var(--amber-text)' }}>计量告警 {counts.warning}</span></>}
        {(counts.failed || 0) > 0 && <> · <span style={{ color: 'var(--red)' }}>失败 {counts.failed}</span></>}
        {(counts.cancelled || 0) > 0 && <> · 已取消 {counts.cancelled}</>}
        {(counts.skipped || 0) > 0 && <> · 未下发 {counts.skipped}</>}
        {running && <> · 进行中 {(counts.running || 0) + (counts.queued || 0)}</>}
        <small style={{ color: 'var(--muted)' }}> · {new Date(batch.updated_at).toLocaleString('zh-CN', { hour12: false })}</small>
      </p>
      {batch.status === 'interrupted' && <p role="alert">没跑完的节点不能算更新成功，请选中它们重新更新（重复更新是安全的）。</p>}
      {!running && problemResults.length === 0 && <p style={{ color: 'var(--green)' }}>全部 {(batch.results ?? []).length} 台都已更新并核验通过。</p>}
      <div style={{ display: 'grid', gap: 8, marginTop: 8 }}>
        {visibleResults.map(item => <div key={item.node_id} style={{ border: '1px solid var(--line)', borderRadius: 8, padding: '8px 12px' }}>
          <strong>{item.name}</strong> · {batch.status === 'interrupted' && ['running', 'queued'].includes(item.status) ? '中断 / 未核验' : statusText[item.status] || item.status}
          <div style={{ color: 'var(--muted)' }}>{item.stage}</div>
          {item.error && <div role="alert" style={{ color: 'var(--amber-text)', overflowWrap: 'anywhere' }}>{item.error}</div>}
        </div>)}
      </div>
      {!running && (batch.results ?? []).length > problemResults.length && <button className="button button-quiet" style={{ marginTop: 8 }} onClick={() => setShowAllResults(v => !v)}>
        {showAllResults ? '只看有问题的' : `显示全部 ${(batch.results ?? []).length} 台的详情`}
      </button>}
    </section>}
    </div>
  </Modal>
}
