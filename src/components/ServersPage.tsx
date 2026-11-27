import { useEffect, useMemo, useRef, useState } from 'react'
import { Cloud, Download, ListChecks, Pencil, Plug, Shuffle, Plus, Server as ServerIcon, Sparkles, Trash2 } from 'lucide-react'
import { api, loadSession, type ApiError, type Hy2Node, type Server, type ServerBilling, type ServerStatus, isAdminSession } from '../lib/api'
import { Modal } from './Modal'
import { BatchHostPresets } from './BatchHostPresets'
import { InstallHy2Dialog, installBlockedReason } from './InstallHy2Dialog'
import { BillingDialog, ChangeIPDialog, CURRENCY_SYMBOL, ExpiryBadge, daysUntil } from './HostTools'
import { HostPicker, HostToolbox, TOOL_ICONS, type ToolDef } from './HostToolbox'
import { MigrateNodeDialog } from './MigrateNodeDialog'
import { askConfirm } from './ConfirmHost'

const statusText: Record<ServerStatus, string> = { online: '运行中', warning: '资源告警', offline: '离线', unknown: '未知' }
const statusClass: Record<ServerStatus, string> = { online: 'online', warning: 'warning', offline: 'offline', unknown: 'offline' }

type AuthKind = 'none' | 'ssh_key' | 'password'
type Draft = { name: string; host: string; ssh_port: string; ssh_user: string; region: string; provider: string; tags: string; auth: AuthKind; secret: string; trafficUrl: string; trafficFile: string }

const emptyDraft: Draft = { name: '', host: '', ssh_port: '22', ssh_user: 'root', region: '', provider: '', tags: '', auth: 'ssh_key', secret: '', trafficUrl: '', trafficFile: '' }

export function ServersPage({ servers, nodes = [], createSignal, onOpen, onChanged, notify }: {
  servers: Server[]
  nodes?: Hy2Node[]
  createSignal: number
  onOpen: (server: Server) => void
  onChanged: () => Promise<void> | void
  notify: (message: string) => void
}) {
  const [creating, setCreating] = useState(false)
  const [presets, setPresets] = useState(false)
  const [installing, setInstalling] = useState<Server | null>(null)
  const [billing, setBilling] = useState<Record<string, ServerBilling>>({})
  // 2026-11-06：到期 / 费用读取失败单独记下。原来静默吞掉：表格每台都显示「未填写」，
  // 汇总写「月成本 未填写 · 7 天内到期 0」；此时打开「到期与费用」弹窗是空表单，
  // 一保存就把库里原有的到期日、月费、备注覆盖成空值。
  const [billingError, setBillingError] = useState('')
  const [billingFor, setBillingFor] = useState<Server | null>(null)
  const [changingIP, setChangingIP] = useState<Server | null>(null)
  const [testing, setTesting] = useState('')
  const [picking, setPicking] = useState<ToolDef | null>(null)
  const [migrating, setMigrating] = useState(false)
  const [presetInitial, setPresetInitial] = useState<string | undefined>()
  const admin = isAdminSession()
  const viewer = loadSession()?.role === 'viewer'
  const mountedRef = useRef(true)
  useEffect(() => {
    mountedRef.current = true
    return () => { mountedRef.current = false }
  }, [])

  const tools: ToolDef[] = [
    { key: 'install', title: '一键安装节点', desc: 'SSH 装好 Hysteria 2 并自动接入控制台', Icon: TOOL_ICONS.Download, tone: 'accent',
      blocked: (s) => (hostsWithNode.has(s.id) ? '已有节点' : installBlockedReason(s, false)) },
    { key: 'migrate', title: '迁移节点', desc: '被墙 / 换机：数据搬到新 VPS，客户订阅不变', Icon: TOOL_ICONS.Shuffle, batch: true, blocked: () => '' },
    { key: 'test', title: '测试连接', desc: '检查 SSH 并登记主机指纹', Icon: TOOL_ICONS.Plug,
      blocked: (s) => (s.has_credential ? '' : '未配置 SSH 凭据') },
    { key: 'billing', title: '到期与费用', desc: '到期前 7 天提醒，汇总月成本', Icon: TOOL_ICONS.CalendarClock, blocked: () => '' },
    { key: 'changeip', title: '换 IP 同步', desc: '更新主机地址与节点域名 DNS', Icon: TOOL_ICONS.Shuffle, blocked: () => '' },
    { key: 'security_audit', title: '安全检查', desc: '只读体检，100 分制评分', Icon: TOOL_ICONS.ShieldCheck, batch: true, blocked: () => '' },
    { key: 'net_route', title: '回程线路检测', desc: '识别 CN2 / 9929 / CMI 等线路', Icon: TOOL_ICONS.Route, batch: true, blocked: () => '' },
  ]

  function pickTool(tool: ToolDef) {
    // 2026-11-04：到期与费用也是管理员接口（PUT /api/servers/{id}/billing 是 requireAdmin）。
    // 原来特意对非管理员放行：填完表单点保存才收到「需要管理员角色」，输入白填。
    if (!admin) {
      notify('只读 / 运维账号不能执行这项操作')
      return
    }
    if (tool.key === 'migrate') {
      setMigrating(true)
      return
    }
    if (tool.batch) {
      setPresetInitial(tool.key)
      setPresets(true)
      return
    }
    setPicking(tool)
  }

  function chooseHost(tool: ToolDef, server: Server) {
    setPicking(null)
    if (tool.key === 'install') setInstalling(server)
    else if (tool.key === 'test') void testSSH(server)
    else if (tool.key === 'billing') openBilling(server)
    else if (tool.key === 'changeip') setChangingIP(server)
  }

  // 按主机 ID 集合变化重新拉取计费信息，避免增删主机后数量碰巧不变导致计费不刷新。
  const serverIdsKey = useMemo(() => servers.map((s) => s.id).sort().join(','), [servers])
  useEffect(() => {
    let cancelled = false
    api.serverBilling()
      .then((list) => {
        if (cancelled) return
        setBilling(Object.fromEntries(list.map((b) => [b.server_id, b])))
        setBillingError('')
      })
      .catch((err) => {
        if (!cancelled) setBillingError(err instanceof Error ? err.message : '读取失败')
      })
    return () => { cancelled = true }
  }, [serverIdsKey])

  // 没读到现有的到期与费用时不打开编辑：空表单一保存就会覆盖库里的记录。
  function openBilling(server: Server) {
    if (billingError) {
      notify(`到期与费用读取失败（${billingError}），为避免覆盖已有记录，请刷新页面后再编辑`)
      return
    }
    setBillingFor(server)
  }

  // 连接测试：首次连接会登记 SSH 指纹（一键安装等写操作需要它）。
  async function testSSH(server: Server) {
    setTesting(server.id)
    try {
      const r = await api.testServerSSH(server.id)
      notify(`${server.name} 连接正常（${r.latency_ms} ms）· ${r.os || '未知系统'}${r.enrolled ? ' · 已登记 SSH 指纹' : ''}${r.switched_from ? ` · 主机禁止 ${r.switched_from} 执行命令，已自动改用 ${r.ssh_user}` : ''}${r.is_root ? '' : r.passwordless_sudo ? ' · 非 root，已具备免密 sudo' : ' · 非 root 且没有免密 sudo：一键安装与写操作会失败'}`)
      if (r.switched_from) void onChanged()
    } catch (err) {
      const body = (err as ApiError | undefined)?.body
      if (body?.fingerprint_mismatch) {
        const oldFp = String(body.expected ?? '')
        const newFp = String(body.got ?? '')
        const ok = await askConfirm(
          `${server.name} 的 SSH 主机指纹变了：\n\n登记的：${oldFp}\n现在的：${newFp}\n\n` +
          '如果你刚重装过系统 / 重建过实例，这是正常的，点「确定」登记新指纹。\n' +
          '如果你没做过这些操作，可能存在中间人攻击 —— 请点「取消」并检查网络。')
        if (ok) {
          try {
            await api.acceptFingerprint(server.id, oldFp, newFp)
            notify(`已登记 ${server.name} 的新指纹，正在重新测试连接…`)
            setTesting('')
            await testSSH(server)
            return
          } catch (e) {
            notify(e instanceof Error ? e.message : '登记新指纹失败')
          }
        }
      } else {
        notify(err instanceof Error ? `${server.name} 连接失败：${err.message}` : '连接失败')
      }
    } finally {
      if (mountedRef.current) setTesting('')
    }
  }

  // 月成本按币种分别求和（不做汇率换算，避免给出一个看似精确实则过时的总数）。
  const costByCurrency = Object.values(billing).reduce<Record<string, number>>((acc, b) => {
    if (b.monthly_cost != null && servers.some((s) => s.id === b.server_id)) acc[b.currency] = (acc[b.currency] ?? 0) + b.monthly_cost
    return acc
  }, {})
  const expiringSoon = servers.filter((s) => {
    const b = billing[s.id]
    const d = daysUntil(b?.expires_on)
    return d !== null && d <= 7 && !b?.auto_renew
  })
  const hostsWithNode = new Set(nodes.map((n) => n.server_id))
  const [editing, setEditing] = useState<Server | null>(null)
  const [draft, setDraft] = useState<Draft>(emptyDraft)
  const [naming, setNaming] = useState(false)
  const [busy, setBusy] = useState(false)

  const [pendingDelete, setPendingDelete] = useState<Server | null>(null)

  useEffect(() => {
    if (createSignal > 0 && admin) {
      setDraft(emptyDraft)
      setCreating(true)
    }
  }, [createSignal, admin])

  function startEdit(server: Server) {
    setEditing(server)
    setDraft({
      name: server.name, host: server.host, ssh_port: String(server.ssh_port), ssh_user: server.ssh_user,
      region: server.region, provider: server.provider, tags: (server.tags ?? []).join(', '), auth: 'none', secret: '',
      trafficUrl: server.traffic_api_url || '', trafficFile: server.traffic_secret_file || '',
    })
  }

  function trafficDraftOK(requireCheck = true): boolean {
    if (!requireCheck) return true
    if (!draft.trafficUrl && !draft.trafficFile) return true
    if (!draft.trafficUrl || !draft.trafficFile) {
      notify('速率采集接口与密钥文件路径需同时填写，或同时留空')
      return false
    }
    const urlMatch = /^http:\/\/(127\.0\.0\.1|localhost):([0-9]{1,5})$/.exec(draft.trafficUrl)
    const trafficPort = urlMatch ? Number(urlMatch[2]) : 0
    if (!urlMatch || !Number.isInteger(trafficPort) || trafficPort < 1 || trafficPort > 65535) {
      notify('采集接口地址只能是回环 http 且端口在 1-65535，例如 http://127.0.0.1:29862')
      return false
    }
    if (!/^\/[A-Za-z0-9._/-]+$/.test(draft.trafficFile)) {
      notify('密钥路径需为目标机上的绝对路径')
      return false
    }
    return true
  }

  // 主机地址填完自动命名：名称 / 区域 / 服务商只填空着的，手写的不覆盖。
  async function autoName(d: Draft): Promise<Draft> {
    const host = d.host.trim()
    if (!host || (d.name.trim() && d.region.trim() && d.provider.trim())) return d
    setNaming(true)
    try {
      const r = await api.suggestServerName(host)
      return { ...d, name: d.name.trim() || r.name || '', region: d.region.trim() || r.region || '', provider: d.provider.trim() || r.provider || '' }
    } catch { return d } finally { if (mountedRef.current) setNaming(false) }
  }

  async function onHostBlur() {
    if (editing) return
    const next = await autoName(draft)
    if (!mountedRef.current) return
    setDraft((cur) => (cur.host === next.host ? { ...cur, name: cur.name || next.name, region: cur.region || next.region, provider: cur.provider || next.provider } : cur))
  }

  // 未开写操作授权时：确认后直接开启（白名单 hysteria-server）并进入安装，不必再去编辑主机。
  async function unlockAndInstall(server: Server, blocked: string) {
    if (server.allow_write_ops || !server.has_credential) { notify(`无法安装：${blocked}`); return }
    if (!(await askConfirm(`一键安装会以 sudo 执行环境变量 HY2_INSTALL_SCRIPT_URL 指定的节点安装脚本，需要先开启这台主机的写操作授权。

白名单：hysteria-server（允许面板重启这个服务）
之后可随时在「编辑主机」里关闭。`, { title: `为「${server.name}」开启写操作并安装？`, confirmText: '开启并继续安装', danger: false }))) return
    try {
      const r = await api.setWriteOps(server.id, true, (server.write_units ?? []).length ? server.write_units : ['hysteria-server'])
      const updated = { ...server, allow_write_ops: r.allow_write_ops, write_units: r.write_units }
      void onChanged()
      if (mountedRef.current) setInstalling(updated)
    } catch (err) { notify(`开启写操作授权失败：${err instanceof Error ? err.message : err}`) }
  }

  async function loadKeyFile(file: File | undefined) {
    if (!file) return
    if (file.size > 64 * 1024) { notify('文件过大，私钥一般只有几 KB'); return }
    const text = (await file.text()).split(String.fromCharCode(13)).join("")
    if (/PuTTY-User-Key-File/.test(text)) { notify('这是 PuTTY 的 .ppk 格式，请先用 PuTTYgen「Conversions → Export OpenSSH key」导出后再上传'); return }
    if (!/-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----/.test(text)) { notify('没识别到私钥（需要以 -----BEGIN ... PRIVATE KEY----- 开头）；注意别选成 .pub 公钥'); return }
    if (/ENCRYPTED/.test(text)) notify('这个私钥看起来带口令，暂不支持，请先去掉口令')
    setDraft((cur) => ({ ...cur, secret: text.trim() }))
    notify(`已读取私钥文件：${file.name}`)
  }

  async function submitCreate() {
    if (!draft.name.trim() && draft.host.trim()) {
      const next = await autoName(draft)
      if (!next.name) { notify('无法自动识别名称，请手动填写'); return }
      setDraft(next)
      return createWith(next)
    }
    return createWith(draft)
  }

  async function createWith(draft: Draft) {
    const port = Number(draft.ssh_port)
    if (!draft.name.trim() || !draft.host.trim() || !draft.ssh_user.trim() || !Number.isInteger(port) || port < 1 || port > 65535) {
      notify('主机地址、SSH 用户必填，端口需为 1-65535')
      return
    }
    if (!trafficDraftOK()) return
    if (draft.auth !== 'none' && draft.secret.trim().length < 6) {
      notify(draft.auth === 'ssh_key' ? '请粘贴 SSH 私钥（PEM），或把认证方式改为「暂不配置」' : 'SSH 密码至少 6 个字符')
      return
    }
    setBusy(true)
    // 2026-11-04：主机行与凭据分两步保存。原来共用一个 try：主机建好、凭据被拒（.pub 公钥、带口令的私钥）时
    // 只提示「添加失败」，对话框还开着同一份草稿 —— 再点「创建主机」只会撞上「已登记过同地址同端口」409，
    // 怎么重试都失败，而列表里已经多出一台没有凭据的主机。
    let createdID = ''
    try {
      const created = await api.createServer({
        name: draft.name.trim(), host: draft.host.trim(), ssh_port: port, ssh_user: draft.ssh_user.trim(),
        region: draft.region.trim(), provider: draft.provider.trim(),
        tags: draft.tags.split(',').map((tag) => tag.trim()).filter(Boolean),
        traffic_api_url: draft.trafficUrl, traffic_secret_file: draft.trafficFile,
      })
      createdID = created.id
      if (draft.auth !== 'none') {
        try {
          await api.uploadCredential(created.id, draft.auth, draft.auth === 'ssh_key' ? `${draft.secret.trimEnd()}\n` : draft.secret.trim())
        } catch (err) {
          notify(`⚠ 主机「${draft.name.trim()}」已添加，但凭据保存失败：${err instanceof Error ? err.message : '未知错误'}。请在主机列表里编辑这台主机重新填写凭据。`)
          setCreating(false)
          setDraft(emptyDraft)
          await onChanged()
          return
        }
        // 保存凭据后立即测一次连接：顺带登记 SSH 指纹，之后一键安装等功能才能直接用。
        void api.testServerSSH(created.id)
          .then((r) => { notify(`${draft.name.trim()} SSH 连接正常 · ${r.os || '未知系统'}${r.enrolled ? ' · 已登记指纹' : ''}${r.switched_from ? ` · 主机禁止 ${r.switched_from} 执行命令，已自动改用 ${r.ssh_user}` : ''}`); void onChanged() })
          .catch((err) => notify(`${draft.name.trim()} SSH 连接测试失败：${err instanceof Error ? err.message : err}`))
      }
      notify(draft.auth === 'none' ? `已添加主机：${draft.name.trim()}（未配置凭据，暂不会采集）` : `已添加主机并加密保存凭据：${draft.name.trim()}`)
      setCreating(false)
      setDraft(emptyDraft)
      await onChanged()
    } catch (err) {
      notify(createdID
        ? `⚠ 主机「${draft.name.trim()}」已添加，但后续步骤失败：${err instanceof Error ? err.message : '未知错误'}`
        : (err instanceof Error ? `添加失败：${err.message}` : '添加失败'))
      if (createdID) {
        setCreating(false)
        setDraft(emptyDraft)
      }
      await onChanged()
    } finally {
      setBusy(false)
    }
  }

  async function submitEdit() {
    if (!editing) return
    const port = Number(draft.ssh_port)
    if (!draft.name.trim() || !draft.host.trim() || !draft.ssh_user.trim() || !Number.isInteger(port) || port < 1 || port > 65535) {
      notify('主机地址、SSH 用户必填，端口需为 1-65535')
      return
    }
    const endpointChanged = draft.host.trim() !== editing.host || port !== editing.ssh_port || draft.ssh_user.trim() !== editing.ssh_user
    const trafficChanged = draft.trafficUrl !== (editing.traffic_api_url || '') || draft.trafficFile !== (editing.traffic_secret_file || '')
    if (!trafficDraftOK(trafficChanged)) return
    if (draft.auth !== 'none' && draft.secret.trim().length < 6) {
      notify(draft.auth === 'ssh_key' ? '请粘贴新的 SSH 私钥，或把认证方式改回「不更换凭据」' : 'SSH 密码至少 6 个字符')
      return
    }
    setBusy(true)
    try {
      await api.updateServer(editing.id, {
        name: draft.name.trim(), region: draft.region.trim(), provider: draft.provider.trim(),
        tags: draft.tags.split(',').map((tag) => tag.trim()).filter(Boolean),
        // 只在真的改了时才发 SSH 端点与流量采集字段：后端将这五项全部归为管理员专属（manage.go touchesEndpoint），
        // 原样回传会导致运维账号修改名称/地区/标签时触发 403，且会无谓清空已锁定的主机指纹。
        ...(endpointChanged ? { host: draft.host.trim(), ssh_port: port, ssh_user: draft.ssh_user.trim() } : {}),
        ...(trafficChanged ? { traffic_api_url: draft.trafficUrl, traffic_secret_file: draft.trafficFile } : {}),
      })
      if (draft.auth !== 'none') {
        try {
          await api.uploadCredential(editing.id, draft.auth, draft.auth === 'ssh_key' ? `${draft.secret.trimEnd()}\n` : draft.secret.trim())
        } catch (err) {
          // 主机字段已经保存成功：不能说成「更新失败」。对话框保持打开，只需重新填凭据（2026-11-04）。
          notify(`⚠ 「${draft.name.trim()}」的主机信息已保存，但凭据未能更新：${err instanceof Error ? err.message : '未知错误'}`)
          await onChanged()
          return
        }
      }
      notify(draft.auth !== 'none'
        ? `已更新并轮换凭据：${draft.name.trim()}`
        : (endpointChanged
          ? `已更新：${draft.name.trim()}，主机密钥已重置，下次连接按 TOFU 重新锁定`
          : `已更新：${draft.name.trim()}`))
      setEditing(null)
      await onChanged()
    } catch (err) {
      notify(err instanceof Error ? `更新失败：${err.message}` : '更新失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  async function confirmDelete() {
    if (!pendingDelete) return
    const target = pendingDelete
    setPendingDelete(null)
    setBusy(true)
    try {
      await api.deleteServer(target.id)
      notify(`已删除：${target.name}`)
      await onChanged()
    } catch (err) {
      notify(err instanceof Error ? `删除失败：${err.message}` : '删除失败')
    } finally {
      if (mountedRef.current) setBusy(false)
    }
  }

  const endpointLocked = Boolean(editing && !admin)

  const fields = (
    <>
      <label className="form-field"><span>名称{editing ? ' *' : '（留空自动命名）'}</span><input value={draft.name} onChange={(event) => setDraft({ ...draft, name: event.target.value })} placeholder={naming ? "识别中…" : editing ? "edge-01" : "填完主机地址自动生成，如 美国-哥伦布"} /></label>
      <label className="form-field"><span>主机地址 *{endpointLocked ? '（仅管理员可修改）' : ''}</span><input value={draft.host} disabled={endpointLocked} onChange={(event) => setDraft({ ...draft, host: event.target.value })} onBlur={() => void onHostBlur()} placeholder="203.0.113.10" /></label>
      <div className="form-row">
        <label className="form-field"><span>SSH 端口</span><input value={draft.ssh_port} disabled={endpointLocked} onChange={(event) => setDraft({ ...draft, ssh_port: event.target.value })} inputMode="numeric" /></label>
        <label className="form-field"><span>SSH 用户</span><input value={draft.ssh_user} disabled={endpointLocked} onChange={(event) => setDraft({ ...draft, ssh_user: event.target.value })} /></label>
      </div>
      <p className="form-note form-note-hint">
        <Sparkles size={13} />
        <span>
          名称留空时，会按主机地址自动识别并命名为 <code>国家-城市</code>（如 <code>美国-哥伦布</code>，重名自动加序号），同时填好区域与服务商。
          这里手工填的字段<strong>永远不会被自动识别覆盖</strong>。
        </span>
      </p>
      <div className="form-row">
        <label className="form-field"><span>区域</span><input value={draft.region} onChange={(event) => setDraft({ ...draft, region: event.target.value })} placeholder="Tokyo（可留空，导入时自动识别）" /></label>
        <label className="form-field"><span>服务商</span><input value={draft.provider} onChange={(event) => setDraft({ ...draft, provider: event.target.value })} placeholder="AWS（可留空，导入时自动识别）" /></label>
      </div>
      <label className="form-field"><span>标签（逗号分隔）</span><input value={draft.tags} onChange={(event) => setDraft({ ...draft, tags: event.target.value })} placeholder="hy2, edge" /></label>
      <div className="form-row">
        <label className="form-field"><span>速率采集接口（可留空）{endpointLocked ? '（仅管理员可修改）' : ''}</span><input value={draft.trafficUrl} disabled={endpointLocked} onChange={(event) => setDraft({ ...draft, trafficUrl: event.target.value.trim() })} placeholder="http://127.0.0.1:29862" spellCheck={false} /></label>
        <label className="form-field"><span>密钥文件路径（目标机）{endpointLocked ? '（仅管理员可修改）' : ''}</span><input value={draft.trafficFile} disabled={endpointLocked} onChange={(event) => setDraft({ ...draft, trafficFile: event.target.value.trim() })} placeholder="/etc/hysteria/traffic_stats_secret" spellCheck={false} /></label>
      </div>
      <p className="form-note">填了这两项，采集器每个周期会在目标机本地读取 hysteria 自带的 trafficStats 接口（<code>?clear=true</code> 取走即清零），把这一周期的字节数按上/下行分别入库，总览的「流量趋势」就会出现真实曲线。地址仅允许回环 http，密钥不入库、只记文件路径。</p>
      <label className="form-field"><span>{editing ? '更换 SSH 凭据（可选）' : 'SSH 认证方式'}{endpointLocked ? '（仅管理员可更换）' : ''}</span>
        <select value={draft.auth} disabled={endpointLocked} onChange={(event) => setDraft({ ...draft, auth: event.target.value as AuthKind })}>
          <option value="ssh_key">私钥（PEM，推荐）</option>
          <option value="password">密码</option>
          <option value="none">{editing ? '不更换凭据' : '暂不配置（只登记，不采集）'}</option>
        </select>
      </label>
      {draft.auth === 'ssh_key' && (
        <label className="form-field"><span>SSH 私钥</span>
          <textarea className="key-input" rows={4} spellCheck={false} value={draft.secret} onChange={(event) => setDraft({ ...draft, secret: event.target.value })} placeholder="-----BEGIN OPENSSH PRIVATE KEY-----（粘贴，或点下方选择文件）" />
          <span className="key-file-row">
            <label className="button button-quiet key-file-btn">选择本地私钥文件
              <input type="file" accept=".pem,.key,.txt,application/x-pem-file,*/*" hidden onChange={(event) => { void loadKeyFile(event.target.files?.[0]); event.target.value = '' }} />
            </label>
            <small>id_rsa / id_ed25519 / .pem，仅在浏览器读取后随表单加密上传</small>
          </span>
        </label>
      )}
      {draft.auth === 'password' && (
        <label className="form-field"><span>SSH 密码</span>
          <input type="password" value={draft.secret} onChange={(event) => setDraft({ ...draft, secret: event.target.value })} autoComplete="off" />
        </label>
      )}
      <p className="form-note">{editing
        ? '修改地址、端口或 SSH 用户会清空已锁定的主机密钥指纹，下一次连接按 TOFU 重新锁定；若目标机换过 host key，这正是要做的。凭据仍以 AES-256-GCM 加密入库，接口不回显。'
        : '凭据以 AES-256-GCM 加密入库，接口不回显；带口令的私钥暂不支持，请先转成无口令密钥或改用密码方式。采集与执行只使用只读命令，重启类操作仍需按主机开启白名单。'}</p>
    </>
  )

  return (
    <>
      <div className="page-heading"><div><div className="eyebrow"><span className="eyebrow-line" />OPS <span>·</span> ADVANCED</div><h1>VPS 主机<span className="heading-period">.</span></h1><p className="page-subtitle">主机的 SSH 凭据与写操作授权 —— 可选的高级功能，节点功能不依赖它</p></div><div className="heading-actions"><button className="button button-quiet" disabled={!admin} title={admin ? undefined : '仅管理员可执行批量预设任务'} onClick={() => setPresets(true)}><ListChecks size={16} />批量预设任务</button><button className="button button-primary" disabled={!admin} title={admin ? undefined : '仅管理员可添加主机'} onClick={() => { setDraft(emptyDraft); setCreating(true) }}><Plus size={16} />添加主机</button></div></div>
      {billingFor && <BillingDialog server={billingFor} billing={billing[billingFor.id]} notify={notify} onClose={() => setBillingFor(null)}
        onSaved={(b) => setBilling((m) => ({ ...m, [b.server_id]: b }))} />}
      {changingIP && <ChangeIPDialog server={changingIP} notify={notify} onClose={() => setChangingIP(null)} onDone={() => { void onChanged() }} />}
      {installing && <InstallHy2Dialog server={installing} notify={notify} onClose={() => setInstalling(null)} onDone={() => { void onChanged() }} />}
      {presets && <BatchHostPresets servers={servers} notify={notify} initialPreset={presetInitial} onClose={() => { setPresets(false); setPresetInitial(undefined) }} />}
      {migrating && <MigrateNodeDialog nodes={nodes} servers={servers} notify={notify} onClose={() => setMigrating(false)} onDone={() => { void onChanged() }} />}
      {picking && <HostPicker tool={picking} servers={servers} onClose={() => setPicking(null)} onChoose={(s) => chooseHost(picking, s)} />}
      {/* 说清这一页的定位，避免用户误以为"不登记主机就用不了节点"。
          实际依赖关系：HY2 节点 → 节点侧 portal API（与主机无关）；
          本页 → 额外的 OS 级指标（CPU/内存/磁盘）与重启、拉日志等写操作。 */}
      <HostToolbox tools={tools} onPick={pickTool} />
      <p className="form-note form-note-hint" style={{ marginBottom: 12 }}>
        <ServerIcon size={13} />
        <span>
          这一页是<strong>可选</strong>的。节点的接入、状态、用户管理、代理服务与三网延迟
          全部由节点自己的 API 提供，<strong>不经过这里</strong>；
          登记主机只是为了额外拿到 CPU / 内存 / 磁盘等操作系统级指标，
          以及下发重启、拉日志这类 SSH 运维动作。
        </span>
      </p>
      <section className="panel servers-panel">
        <div className="section-head servers-head"><div><div className="section-kicker">HOSTS <span>·</span> {servers.length.toString().padStart(2, '0')}</div><h2>主机清单 <span className="count-chip">{servers.length.toString().padStart(2, '0')}</span></h2></div></div>
        {servers.length === 0 ? (
          <div className="empty-state"><ServerIcon size={20} /><strong>暂无主机</strong><span>点击右上角「添加主机」登记第一台机器。</span></div>
        ) : (<>
          <div className="host-summary">
            {billingError && <span className="warn">到期与费用读取失败：{billingError}</span>}
            <span>月成本 <strong>{billingError ? '—' : Object.keys(costByCurrency).length ? Object.entries(costByCurrency).map(([c, v]) => `${CURRENCY_SYMBOL[c] ?? ''}${v.toFixed(2)} ${c}`).join(' + ') : '未填写'}</strong></span>
            <span className={expiringSoon.length ? 'warn' : ''}>7 天内到期 <strong>{billingError ? '—' : expiringSoon.length}</strong>{expiringSoon.length ? `（${expiringSoon.map((x) => x.name).join('、')}）` : ''}</span>
            <span>已配置凭据 <strong>{servers.filter((x) => x.has_credential).length}/{servers.length}</strong></span>
          </div>
          <div className="server-table-wrap"><table className="server-table">
            <thead><tr><th>主机 / 地址</th><th>状态</th><th>SSH</th><th>凭据</th><th>写操作</th><th>到期 / 月费</th><th>区域 / 标签</th><th></th></tr></thead>
            <tbody>
              {servers.map((server) => (
                <tr key={server.id} onClick={() => onOpen(server)} tabIndex={0} onKeyDown={(event) => event.key === 'Enter' && onOpen(server)}>
                  <td><div className="host-cell"><div className={`host-icon ${statusClass[server.status]}`}><Cloud size={16} /></div><div><strong>{server.name}</strong><span>{server.host}</span></div></div></td>
                  <td><span className={`status-badge ${statusClass[server.status]}`}><i />{statusText[server.status]}</span></td>
                  <td><span className="traffic-sub">{server.ssh_user} · 端口 {server.ssh_port}</span></td>
                  <td>{server.has_credential ? <span className="status-badge online"><i />已配置</span> : <span className="status-badge offline"><i />未配置</span>}</td>
                  <td>{server.allow_write_ops ? <span className="status-badge warning"><i />{(server.write_units ?? []).join(', ') || '无白名单'}</span> : <span className="traffic-sub">只读</span>}</td>
                  <td>
                    <button className="billing-cell" disabled={!admin} title={admin ? '设置到期日与月费' : '仅管理员可设置到期日与月费'} onClick={(event) => { event.stopPropagation(); openBilling(server) }}>
                      {billingError ? <span className="traffic-sub">读取失败</span> : <ExpiryBadge billing={billing[server.id]} />}
                      <small>{billingError ? '刷新重试' : billing[server.id]?.monthly_cost != null ? `${CURRENCY_SYMBOL[billing[server.id].currency] ?? ''}${billing[server.id].monthly_cost}/月` : '点此填写'}</small>
                    </button>
                  </td>
                  <td><span className="traffic-sub">{server.region || '—'}{(server.tags ?? []).length ? ` · ${(server.tags ?? []).join('/')}` : ''}</span></td>
                  <td>
                    <div className="row-actions">
                      {isAdminSession() && !hostsWithNode.has(server.id) && (() => {
                        const blocked = installBlockedReason(server, false)
                        return <button className="row-action" aria-label={`在 ${server.name} 上一键安装 HY2`} title={blocked || '一键安装 HY2 并自动接入'}
                          onClick={(event) => { event.stopPropagation(); if (blocked) { void unlockAndInstall(server, blocked) } else { setInstalling(server) } }}>
                          <Download size={14} /><span>安装</span>
                        </button>
                      })()}
                      {isAdminSession() && server.has_credential && <button className="row-action" aria-label={`测试 ${server.name} 的 SSH 连接`} title="测试 SSH 连接（首次会登记主机指纹）" disabled={testing === server.id}
                        onClick={(event) => { event.stopPropagation(); void testSSH(server) }}><Plug size={14} /><span>{testing === server.id ? '测试中' : '测连接'}</span></button>}
                      {isAdminSession() && <button className="row-action" aria-label={`为 ${server.name} 同步新 IP`} title="换 IP 后同步（主机地址 + 节点域名 DNS）"
                        onClick={(event) => { event.stopPropagation(); setChangingIP(server) }}><Shuffle size={14} /><span>换 IP</span></button>}
                      <button className="row-more" aria-label={`编辑 ${server.name}`} disabled={viewer} title={viewer ? '只读账号不能编辑主机' : '编辑主机'} onClick={(event) => { event.stopPropagation(); startEdit(server) }}><Pencil size={15} /></button>
                      <button className="row-more" aria-label={`删除 ${server.name}`} disabled={busy || !isAdminSession()} title={isAdminSession() ? undefined : "仅管理员可删除服务器"} onClick={(event) => { event.stopPropagation(); setPendingDelete(server) }}><Trash2 size={15} /></button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table></div>
        </>)}
        <div className="table-foot"><span>共 {servers.filter((s) => s.has_credential).length} 台已配置凭据</span><span>点击行查看详情与操作</span></div>
      </section>

      {creating && (
        <Modal title="添加主机" kicker="CREATE HOST" onClose={() => setCreating(false)}>
          {fields}
          <div className="modal-actions"><button className="button button-quiet" onClick={() => setCreating(false)}>取消</button><button className="button button-primary" disabled={busy} onClick={submitCreate}>{busy ? '提交中…' : '创建主机'}</button></div>
        </Modal>
      )}

      {pendingDelete && (
        <Modal title="删除主机" kicker="CONFIRM DELETE" onClose={() => setPendingDelete(null)}>
          <p className="form-note">即将删除主机 <strong>{pendingDelete.name}</strong>（{pendingDelete.host}）。这是软删除，历史指标与任务仍保留在数据库中；若该主机下仍有节点需先删除节点。</p>
          <div className="modal-actions"><button className="button button-quiet" disabled={busy} onClick={() => setPendingDelete(null)}>取消</button><button className="button button-danger" disabled={busy} onClick={confirmDelete}>确认删除</button></div>
        </Modal>
      )}

      {editing && (
        <Modal title={`编辑 ${editing.name}`} kicker="UPDATE HOST" onClose={() => setEditing(null)}>
          {fields}
          <div className="modal-actions"><button className="button button-quiet" onClick={() => setEditing(null)}>取消</button><button className="button button-primary" disabled={busy} onClick={submitEdit}>{busy ? '保存中…' : '保存修改'}</button></div>
        </Modal>
      )}
    </>
  )
}
