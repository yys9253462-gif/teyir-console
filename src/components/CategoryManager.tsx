import { useCallback, useEffect, useRef, useState } from 'react'
import { Plus, Trash2, Tag, AlertTriangle, Power } from 'lucide-react'
import { api, type NodeCategory } from '../lib/api'
import { Modal } from './Modal'
import { askConfirm } from './ConfirmHost'

// CategoryManager 是「节点分类」的管理界面。
//
// 设计要点（2026-10-03 管理员需求）
// ----------------------------
// 分类不只是展示标签，它同时决定**商城商品能给买家哪些节点**：
// 商品选「AI 专用」→ 买家只拿到 AI 专用的节点。所以这个界面上：
//
// 1. **slug 单独说明**：它是商城侧的引用键（商城的商品配置里存的就是它）。
//    改名无妨，改 slug 会让商城已有的配置失配 —— 必须显式提醒，
//    而不是让操作者改完发现商城下错单才反过来查。
//
// 2. **删除要有前置检查**：被节点或订阅引用时后端会拒绝，
//    但界面上应先把「有多少节点/订阅在用」显示出来，
//    让操作者一眼知道该不该删、还是该停用。
//
// 3. **停用 ≠ 删除**：停用的语义是「不再用于新订单」，
//    已售出的订阅继续工作（后端 loadCategoryNodeIDs 不按 enabled 过滤）。
//    这一点必须在界面上写清楚，否则操作者会以为停用等于让老客户断线。
export function CategoryManager({ notify, onClose, onChanged }: {
  notify: (message: string) => void
  onClose: () => void
  onChanged: () => void
}) {
  const [list, setList] = useState<NodeCategory[]>([])
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')

  // 新建表单
  const [slug, setSlug] = useState('')
  const [name, setName] = useState('')
  const [desc, setDesc] = useState('')

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
    setErr('')
    try {
      const res = await api.listCategories()
      if (!mountedRef.current || seq !== loadSeq.current) return
      setList(res.categories ?? [])
    } catch (e) {
      if (!mountedRef.current || seq !== loadSeq.current) return
      setErr(e instanceof Error ? e.message : '读取分类失败')
    } finally {
      if (mountedRef.current && seq === loadSeq.current) setLoading(false)
    }
  }, [])
  useEffect(() => { void load() }, [load])

  async function create() {
    const s = slug.trim().toLowerCase()
    const n = name.trim()
    if (!/^[a-z0-9][a-z0-9_-]{0,31}$/.test(s)) {
      notify('slug 只能用小写字母、数字、下划线与短横线，且以字母或数字开头')
      return
    }
    if (!n) { notify('请填分类名称'); return }
    setBusy(true)
    try {
      await api.createCategory({ slug: s, name: n, description: desc.trim() })
      notify(`已创建分类：${n}`)
      if (mountedRef.current) { setSlug(''); setName(''); setDesc('') }
      await load()
      onChanged()
    } catch (e) {
      notify(e instanceof Error ? `创建失败：${e.message}` : '创建失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  async function toggleEnabled(c: NodeCategory) {
    setBusy(true)
    try {
      await api.updateCategory(c.id, { enabled: !c.enabled })
      notify(c.enabled
        ? `已停用「${c.name}」—— 商城新订单不能再选它，已售出的订阅不受影响`
        : `已启用「${c.name}」`)
      await load()
      onChanged()
    } catch (e) {
      notify(e instanceof Error ? `操作失败：${e.message}` : '操作失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  async function remove(c: NodeCategory) {
    if (c.node_count > 0) {
      notify(`不能删除「${c.name}」：还有 ${c.node_count} 个节点属于它。请先在节点上改掉，或改用「停用」。`)
      return
    }
    if (!await askConfirm(`删除分类「${c.name}」？\n\n它不能被恢复。若只是暂时不用，建议改用「停用」。`)) return
    setBusy(true)
    try {
      await api.deleteCategory(c.id)
      notify(`已删除分类：${c.name}`)
      await load()
      onChanged()
    } catch (e) {
      notify(e instanceof Error ? `删除失败：${e.message}` : '删除失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  return (
    <Modal title="节点分类" kicker="NODE CATEGORIES" onClose={onClose} wide>
      <div style={{ display: 'grid', gap: 14 }}>
        <p className="form-note">
          分类既是<b>分组</b>，也是<b>商城圈定节点范围的依据</b> ——
          商城商品选择某个分类后，买家只会拿到该分类下的节点。
        </p>

        {/* 新建 */}
        <div className="detail-section" style={{ paddingTop: 0 }}>
          <h3>新建分类</h3>
          <div className="form-row">
            <label className="form-field">
              <span>标识 slug（商城引用键）</span>
              {/* placeholder 写成 `例如 ai` 而不是裸 `ai` ——
                  裸单词会被误认为已填好的值，操作者直接点「创建」
                  会撞上"slug 已被占用"的报错，而其实是他没填。 */}
              <input value={slug} onChange={(e) => setSlug(e.target.value)}
                     placeholder="例如 ai" autoComplete="off" spellCheck={false} />
            </label>
            <label className="form-field">
              <span>显示名称</span>
              <input value={name} onChange={(e) => setName(e.target.value)} placeholder="例如 AI 专用" />
            </label>
          </div>
          <label className="form-field">
            <span>说明（可选）</span>
            <input value={desc} onChange={(e) => setDesc(e.target.value)}
                   placeholder="例如：面向 ChatGPT / Claude 等服务的节点" />
          </label>
          <p className="form-note" style={{ marginTop: 4 }}>
            slug 用小写字母/数字/下划线/短横线，<b>建议建好后不要再改</b> ——
            商城的商品配置里存的就是它，改了要同步改商城。
          </p>
          <div style={{ marginTop: 8 }}>
            <button className="button button-primary button-compact" disabled={busy} onClick={() => void create()}>
              <Plus size={14} /> 创建
            </button>
          </div>
        </div>

        {/* 列表 */}
        <div className="detail-section">
          <h3>已有分类（{list.length}）</h3>
          {loading ? (
            <p className="form-note">读取中…</p>
          ) : err ? (
            <p className="form-note service-warn"><AlertTriangle size={13} /> {err}</p>
          ) : list.length === 0 ? (
            <div className="empty-state">
              <Tag size={18} />
              <strong>还没有分类</strong>
              <span>上面建一个，然后到节点列表里给节点勾选分类</span>
            </div>
          ) : (
            <div className="category-list">
              {list.map((c) => (
                <div key={c.id} className={`category-row${c.enabled ? '' : ' is-off'}`}>
                  <div className="category-main">
                    <span className="category-name">{c.name}</span>
                    <code className="category-slug">{c.slug}</code>
                    {!c.enabled && <span className="status-pill warning">已停用</span>}
                  </div>
                  <div className="category-meta">
                    {c.description && <span className="category-desc">{c.description}</span>}
                    <span className="traffic-sub">
                      {c.node_count > 0 ? `${c.node_count} 个节点在用` : '暂无节点'}
                    </span>
                  </div>
                  <div className="row-actions">
                    <button className="row-more" disabled={busy}
                            title={c.enabled ? '停用（商城新订单不能再选，已售出的不受影响）' : '启用'}
                            aria-label={`${c.enabled ? '停用' : '启用'} ${c.name}`}
                            onClick={() => void toggleEnabled(c)}>
                      <Power size={15} />
                    </button>
                    <button className="row-more danger" disabled={busy}
                            title={c.node_count > 0 ? `还有 ${c.node_count} 个节点用它，不能删` : '删除分类'}
                            aria-label={`删除 ${c.name}`}
                            onClick={() => void remove(c)}>
                      <Trash2 size={15} />
                    </button>
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>

        <p className="form-note service-warn">
          <AlertTriangle size={13} />
          <span>
            <b>停用</b>的语义是「不再用于新订单」—— 已经卖出去的订阅<b>继续正常工作</b>，
            不会让老客户断线。要彻底不再使用再考虑删除（且必须没有被节点/订阅引用）。
          </span>
        </p>
      </div>
    </Modal>
  )
}
