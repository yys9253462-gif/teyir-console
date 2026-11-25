import { useCallback, useEffect, useRef, useState } from 'react'
import { AlertTriangle, CheckCircle2, PowerOff, RefreshCw, Trash2, Wrench } from 'lucide-react'
import { api, isAdminSession, type OrphanAccount } from '../lib/api'
import { askConfirm } from './ConfirmHost'

// 孤儿账号（2026-10-04 加）。
//
// 背景：删订阅或删节点时，若某个买家账号没能从节点侧注销，
// 它就会留在节点上继续可用 —— 面板侧却已经认为「这个人没了」。
// 后端本来就有完整的登记与补删能力（orphan.go），
// 但前端一直没有入口，运维只能手工 SSH 上去清（我上一轮就是这么干的，
// 四台节点 560 个账号全靠脚本）。
//
// 展示上刻意**按「能不能一键补删」分成两组**：
//   has_credential = true  → 有凭据快照，点一下就删掉
//   has_credential = false → 凭据没了，**物理上无法重试**，只能人工上机
// 混在一起展示会让运维对着"只能人工"的条目反复点重试。
export function OrphanAccounts({ notify }: { notify: (message: string) => void }) {
  const [orphans, setOrphans] = useState<OrphanAccount[]>([])
  const [showResolved, setShowResolved] = useState(false)
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState<string | null>(null)
  // 后端附带的解释文案。直接展示它而不是前端自己造一段 ——
  // 那是"这个功能为什么存在"的最准说法，也随实现一起维护。
  const [backendNote, setBackendNote] = useState('')
  // 读取失败的原因。有它就不能显示「没有孤儿账号」：那句话的意思是「账号都清干净了」，
  // 而读失败时我们根本不知道（2026-11-04）。
  const [loadError, setLoadError] = useState('')

  // 2026-11-06：只采用最后一次请求的结果。切换「显示已解决」后，前一次较慢的响应原来会晚到并覆盖列表 ——
  // 勾着「显示已解决」却只看到未解决的，或反过来。
  const requestSeq = useRef(0)
  const mountedRef = useRef(true)
  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      requestSeq.current += 1
    }
  }, [])
  const load = useCallback(async (withResolved: boolean) => {
    const seq = ++requestSeq.current
    setLoading(true)
    try {
      const res = await api.orphans(withResolved)
      if (!mountedRef.current || seq !== requestSeq.current) return
      // ⚠️ 后端返回的是对象 {orphans, count, pending, note, how_it_happens}，
      // 不是数组。第一版按数组写会得到 undefined → 界面永远显示
      // "没有孤儿账号"（最糟的那种错：看着正常，其实把数据全吞了）。
      setOrphans(res?.orphans ?? [])
      setBackendNote(res?.note ?? '')
      setLoadError('')
    } catch (error) {
      if (!mountedRef.current || seq !== requestSeq.current) return
      // 读不到不该让任务页崩 —— 但也绝不能显示成「清理干净了」（限流 429 / 数据库抖动时
      // 待补删的买家账号仍可在节点上使用）。保留上一次读到的列表，并说明这次没读到。
      setLoadError(error instanceof Error ? error.message : '读取失败')
    } finally {
      if (mountedRef.current && seq === requestSeq.current) setLoading(false)
    }
  }, [])

  useEffect(() => { void load(showResolved) }, [load, showResolved])

  const pending = orphans.filter((o) => !o.resolved_at)
  const retryable = pending.filter((o) => o.has_credential)
  // 2026-11-08：已删除节点按节点汇总。机器确认下线后可以整台一次结案，不用逐条点。
  const deletedNodeGroups = Object.values(pending.filter((o) => o.node_deleted).reduce<Record<string, { nodeId: string; nodeName: string; count: number }>>((acc, o) => {
    const g = acc[o.node_id] ?? { nodeId: o.node_id, nodeName: o.node_name, count: 0 }
    g.count += 1
    acc[o.node_id] = g
    return acc
  }, {})).sort((a, b) => b.count - a.count)

  async function resolveMachineOffline(group: { nodeId: string; nodeName: string; count: number }) {
    const ok = await askConfirm(
      `确认「${group.nodeName}」所在的机器已经永久下线？\n\n` +
      `这台节点名下 ${group.count} 条待补删记录会一次结案。这些账号没有从机器上删除，` +
      '只是因为机器已不存在而放弃回收；结案后不会再出现在这里。\n这一步不可撤销。',
      { confirmText: `确认下线并结案 ${group.count} 条`, danger: true },
    )
    if (!ok) return
    setBusy(group.nodeId)
    try {
      const res = await api.resolveOrphansMachineOffline(group.nodeId)
      notify(`已结案 ${res.resolved} 条（${res.node_name}）`)
      await load(showResolved)
    } catch (error) {
      notify(error instanceof Error ? error.message : '结案失败')
    } finally {
      if (mountedRef.current) setBusy(null)
    }
  }
  const manual = pending.filter((o) => !o.has_credential)

  // 机器已永久停用时用：不补删，直接结案。节点连不上也永远不会再连上，
  // 挂在待办里只会淹没真正需要处理的条目。
  async function resolve(item: OrphanAccount) {
    if (!await askConfirm(`确认把 ${item.node_name} 上的 ${item.user_id} 标记为已解决？
只在这台机器已永久停用时这样做 —— 如果机器还在运行，这个账号仍然可用。`)) return
    setBusy(item.id)
    try {
      await api.resolveOrphan(item.id)
      notify(`已标记为已解决：${item.user_id}`)
    } catch (error) {
      notify(error instanceof Error ? error.message : '标记失败')
    } finally {
      if (mountedRef.current) {
        await load(showResolved)
        if (mountedRef.current) setBusy(null)
      }
    }
  }

  async function retry(item: OrphanAccount) {
    setBusy(item.id)
    try {
      const res = await api.retryOrphan(item.id)
      notify(res.warning ? `⚠ ${res.warning}` : res.detail ? `${item.user_id}：${res.detail}` : `已补删 ${item.user_id}`)
      await load(showResolved)
    } catch (error) {
      notify(error instanceof Error ? error.message : '补删失败')
      await load(showResolved)
    } finally {
      if (mountedRef.current) setBusy(null)
    }
  }

  return (
    <section className="panel servers-panel">
      <div className="section-head servers-head">
        <div>
          <div className="section-kicker">ORPHAN ACCOUNTS <span>·</span> {pending.length.toString().padStart(2, '0')}</div>
          <h2>孤儿账号 <span className="count-chip">{pending.length.toString().padStart(2, '0')}</span></h2>
        </div>
        <div className="heading-actions" style={{ flexWrap: 'nowrap' }}>
          <button className="button button-quiet" disabled={loading}
                  onClick={() => void load(showResolved)}>
            <RefreshCw size={14} />刷新
          </button>
          <button className={`button ${showResolved ? 'button-primary' : 'button-quiet'}`}
                  aria-pressed={showResolved}
                  onClick={() => setShowResolved((v) => !v)}>
            {showResolved ? '隐藏已处理' : '显示已处理'}
          </button>
        </div>
      </div>

      <p className="form-note" style={{ padding: '0 16px 10px' }}>
        {backendNote || '删除订阅或节点时，若有账号没能从节点侧注销，会登记在这里。这些账号在节点上仍然可用。'}
      </p>

      {loadError && (
        <p className="form-note" role="alert" style={{ padding: '0 16px 10px' }}>⚠ 待补删列表读取失败（{loadError}），下面显示的可能不是最新情况，请稍后刷新。</p>
      )}
      {loading ? (
        <p className="form-note" style={{ padding: '0 16px 16px' }}>读取中…</p>
      ) : orphans.length === 0 && loadError ? null : orphans.length === 0 ? (
        <div className="empty-state">
          <CheckCircle2 size={20} />
          <strong>没有孤儿账号</strong>
          <span>所有已删除的订阅与节点，账号都已从节点侧清理干净。</span>
        </div>
      ) : (
        <>
          {isAdminSession() && deletedNodeGroups.length > 0 && (
            <>
              <div className="section-head servers-head" style={{ paddingTop: 4 }}>
                <div><h2 style={{ fontSize: 'var(--fs-sm)' }}>节点已删除 <span className="count-chip">{deletedNodeGroups.length}</span></h2></div>
              </div>
              <p className="form-note" style={{ padding: '0 16px 8px' }}>
                这些节点已经从控制台删除。如果机器也确实不在了，账号不可能再收回，可以整台一次结案。
              </p>
              <div className="server-table-wrap"><table className="server-table">
                <thead><tr><th>节点</th><th>待补删</th><th></th></tr></thead>
                <tbody>
                  {deletedNodeGroups.map((g) => (
                    <tr key={g.nodeId}>
                      <td><div className="host-cell"><div className="host-icon offline"><PowerOff size={15} /></div><div><strong>{g.nodeName || g.nodeId}</strong><span>节点已删除</span></div></div></td>
                      <td><span className="traffic-sub">{g.count} 条</span></td>
                      <td><div className="row-actions">
                        <button className="button button-quiet button-compact" disabled={busy === g.nodeId} onClick={() => void resolveMachineOffline(g)}>
                          <PowerOff size={14} />确认机器已下线，整台结案
                        </button>
                      </div></td>
                    </tr>
                  ))}
                </tbody>
              </table></div>
            </>
          )}
          {retryable.length > 0 && (
            <>
              <div className="section-head servers-head" style={{ paddingTop: 4 }}>
                <div><h2 style={{ fontSize: 'var(--fs-sm)' }}>可一键补删 <span className="count-chip">{retryable.length}</span></h2></div>
              </div>
              <div className="server-table-wrap"><table className="server-table">
                <thead><tr><th>节点</th><th>账号</th><th>原因</th><th>尝试</th><th>登记时间</th><th></th></tr></thead>
                <tbody>
                  {retryable.map((o) => (
                    <tr key={o.id}>
                      <td><div className="host-cell"><div className="host-icon offline"><AlertTriangle size={15} /></div><div><strong>{o.node_name}</strong><span>{o.node_api_url || '—'}</span></div></div></td>
                      <td><span className="traffic-sub">{o.user_id}</span></td>
                      <td><span className="traffic-sub">{o.reason || '—'}</span></td>
                      <td><span className="traffic-sub">{o.attempts}{o.last_attempt_at ? ` · ${new Date(o.last_attempt_at).toLocaleString('zh-CN', { hour12: false })}` : ''}</span></td>
                      <td><span className="traffic-sub">{new Date(o.created_at).toLocaleString('zh-CN', { hour12: false })}</span></td>
                      <td>
                        <button className="button button-primary" disabled={busy === o.id || !isAdminSession()}
                                title={isAdminSession() ? '用留存的凭据在节点上删除这个账号' : '仅管理员可补删节点账号'}
                                onClick={() => void retry(o)}>
                          <Trash2 size={14} />{busy === o.id ? '删除中…' : '补删'}
                        </button>
                        <button className="button button-quiet" disabled={busy === o.id || !isAdminSession()} style={{ marginLeft: 6 }}
                                title={isAdminSession() ? '机器已永久停用：不补删，直接结案' : '仅管理员可结案'}
                                onClick={() => void resolve(o)}>
                          <CheckCircle2 size={14} />标记已解决
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table></div>
            </>
          )}

          {manual.length > 0 && (
            <>
              <div className="section-head servers-head" style={{ paddingTop: 10 }}>
                <div>
                  <h2 style={{ fontSize: 'var(--fs-sm)' }}>需人工上机处理 <span className="count-chip">{manual.length}</span></h2>
                  <p className="form-note" style={{ marginTop: 2 }}>
                    这些条目<b>没有留存凭据</b>，无法重试 —— 必须按下面的地址登机器手动删除。
                  </p>
                </div>
              </div>
              <div className="server-table-wrap"><table className="server-table">
                <thead><tr><th>节点</th><th>门户地址</th><th>账号</th><th>原因</th><th>登记时间</th><th></th></tr></thead>
                <tbody>
                  {manual.map((o) => (
                    <tr key={o.id}>
                      <td><div className="host-cell"><div className="host-icon offline"><Wrench size={15} /></div><div><strong>{o.node_name}</strong><span>无凭据</span></div></div></td>
                      <td><span className="traffic-sub">{o.node_api_url || '—'}</span></td>
                      <td><span className="traffic-sub">{o.user_id}</span></td>
                      <td><span className="traffic-sub">{o.reason || '—'}</span></td>
                      <td><span className="traffic-sub">{new Date(o.created_at).toLocaleString('zh-CN', { hour12: false })}</span></td>
                      <td>
                        <button className="button button-quiet" disabled={busy === o.id || !isAdminSession()}
                                title={isAdminSession() ? '机器已永久停用：不补删，直接结案' : '仅管理员可结案'}
                                onClick={() => void resolve(o)}>
                          <CheckCircle2 size={14} />标记已解决
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table></div>
            </>
          )}

          {pending.length === 0 && orphans.length > 0 && (
            <p className="form-note" style={{ padding: '0 16px 16px' }}>
              当前没有待处理条目，下面是已完成的历史。
            </p>
          )}
        </>
      )}
    </section>
  )
}
