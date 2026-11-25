import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { Bot, CheckCircle2, Circle, KeyRound, MessageSquare, Send } from 'lucide-react'
import { api, type PurityKeysState, type TelegramSettings } from '../lib/api'
import { askConfirm } from './ConfirmHost'

function ProgressStep({ done, children }: { done: boolean; children: ReactNode }) {
  return (
    <li className={done ? 'done' : ''}>{done ? <CheckCircle2 size={15} /> : <Circle size={15} />}<span>{children}</span></li>
  )
}

// 系统设置 →「Telegram」独立分区（2026-10-16）。
//
// Bot Token、Chat ID、测试消息集中在一页。Token 与其他集成密钥共用同一套
// 加密存储（system_settings + AES-256-GCM，保存即生效），这里只是单独给它一个入口，
// 页面永远不回显明文，只显示末四位。

export function TelegramCard({ notify }: { notify: (m: string) => void }) {
  const [keys, setKeys] = useState<PurityKeysState | null>(null)
  const [tokenDraft, setTokenDraft] = useState('')
  const [chat, setChat] = useState('')
  const [savedChat, setSavedChat] = useState('')
  const [busy, setBusy] = useState(false)
  const [controlRoutes, setControlRoutes] = useState(0)
  const [bot, setBot] = useState<TelegramSettings['bot']>()
  // 2026-11-06：读取失败单独标记。原来只弹 toast，页头随即显示「未完成配置」、Token 显示「未配置」——
  // 已经在工作的机器人看起来像没配。
  const [loadError, setLoadError] = useState('')
  const mountedRef = useRef(true)
  useEffect(() => {
    mountedRef.current = true
    return () => { mountedRef.current = false }
  }, [])

  const load = useCallback(async () => {
    try {
      const [k, t] = await Promise.all([api.purityKeys(), api.telegramSettings()])
      if (!mountedRef.current) return
      setKeys(k)
      setChat(t.chat_id)
      setSavedChat(t.chat_id)
      setBot(t.bot)
      setControlRoutes(t.control_routes ?? 0)
      setLoadError('')
    } catch (err) {
      if (!mountedRef.current) return
      const message = err instanceof Error ? err.message : 'Telegram设置读取失败，请刷新重试'
      setLoadError(message)
      notify(message)
    }
  }, [notify])
  useEffect(() => { void load() }, [load])

  const token = keys?.telegram
  const tokenOK = Boolean(token?.configured)
  const chatOK = savedChat !== ''

  // 仅在已配置 Bot Token 且尚未绑定 Chat ID 时每 5 秒轮询一次，收到 /start 后立刻显示检测到的 Chat ID。
  useEffect(() => {
    if (!tokenOK || savedChat !== '') return
    let alive = true
    const t = window.setInterval(() => {
      if (!document.hidden) {
        void api.telegramSettings().then((s) => { if (alive) setBot(s.bot) }).catch(() => {})
      }
    }, 5000)
    return () => { alive = false; window.clearInterval(t) }
  }, [tokenOK, savedChat])

  async function saveToken(value: string) {
    setBusy(true)
    try {
      setKeys(await api.setPurityKeys({ telegram: value }))
      setTokenDraft('')
      notify(value ? 'Bot Token 已保存，机器人约 1 分钟内上线' : '已清除 Bot Token')
    } catch (err) {
      notify(err instanceof Error ? err.message : '保存失败')
    } finally {
      setBusy(false)
    }
  }

  async function saveChat() {
    setBusy(true)
    try {
      const s = await api.setTelegramChat(chat.trim())
      setSavedChat(s.chat_id)
      notify('Chat ID 已保存')
    } catch (err) {
      notify(err instanceof Error ? err.message : '保存失败')
    } finally {
      setBusy(false)
    }
  }

  async function test() {
    setBusy(true)
    try {
      await api.testTelegram()
      notify('测试消息已发送，去 Telegram 看一下')
    } catch (err) {
      notify(err instanceof Error ? err.message : '发送失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <section className="panel settings-panel">
        <div className="section-head">
          <div><div className="section-kicker">TELEGRAM</div><h2>Telegram 告警与机器人</h2></div>
          {loadError ? <span className="status-pill warning">读取失败</span> : tokenOK && chatOK ? <span className="status-pill online">已启用</span> : <span className="status-pill offline">未完成配置</span>}
        </div>
        {loadError && <p className="form-error">读取 Telegram 设置失败：{loadError}。当前是否已配置未知，刷新页面重试。</p>}
        {tokenOK && bot && (
          <div className="tg-botstatus">
            <span>菜单控制：{controlRoutes} 个管理接口 · 仅绑定私聊本人</span>
            <span>机器人：{bot.bot_username ? <strong>@{bot.bot_username}</strong> : '—'}</span>
            <span>{bot.last_poll_ok ? <span className="lat-good">● 在线（{new Date(bot.last_poll_ok).toLocaleTimeString('zh-CN', { hour12: false })} 收过消息）</span> : <span className="lat-bad">● 尚未连上 Telegram</span>}</span>
            {bot.last_error && <span className="lat-bad" title={bot.last_error_at ?? ''}>最近错误：{bot.last_error}</span>}
            {bot.last_reply_err && <span className="lat-bad">回复失败：{bot.last_reply_err}</span>}
          </div>
        )}
        <ol className="tg-progress">
          <ProgressStep done={tokenOK}>填写 Bot Token</ProgressStep>
          <ProgressStep done={chatOK}>填写 Chat ID</ProgressStep>
          <ProgressStep done={tokenOK && chatOK}>发送测试消息确认能收到</ProgressStep>
        </ol>
      </section>

      <section className="panel settings-panel">
        <div className="section-head"><div><div className="section-kicker">STEP 1</div><h2><Bot size={16} className="tg-h-icon" />Bot Token</h2></div></div>
        <div className="settings-form">
          <p className="form-note">在 Telegram 找 <a className="text-button" style={{ display: 'inline' }} href="https://t.me/BotFather" target="_blank" rel="noreferrer noopener">@BotFather</a>，发 <code>/newbot</code>，按提示起名后它会给你一串 Token（形如 <code>123456789:ABC-def…</code>）。</p>
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
            {tokenOK
              ? <span className="status-pill online">已配置 · {token?.source === 'env' ? '环境变量' : '面板设置'} · {token?.masked}</span>
              : loadError ? <span className="status-pill warning">读取失败</span> : <span className="status-pill offline">未配置</span>}
          </div>
          <label className="form-field"><span>{tokenOK ? '更换 Token（留空不修改）' : '粘贴 Bot Token'}</span>
            <input type="password" autoComplete="off" value={tokenDraft} onChange={(e) => setTokenDraft(e.target.value)} placeholder="123456789:ABC-def…" />
          </label>
          <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
            <button className="button button-primary" disabled={busy || tokenDraft.trim().length < 8} onClick={() => void saveToken(tokenDraft.trim())}><KeyRound size={15} />保存 Token</button>
            {token?.source === 'web' && <button className="button button-quiet" disabled={busy} onClick={async () => { if (await askConfirm('清除 Bot Token？机器人将停止工作，告警不再推送到 Telegram。')) void saveToken('') }}>清除</button>}
          </div>
          <p className="form-note">Token 用 AES-256-GCM 加密存库，页面只显示末四位。</p>
        </div>
      </section>

      <section className="panel settings-panel">
        <div className="section-head"><div><div className="section-kicker">STEP 2</div><h2><MessageSquare size={16} className="tg-h-icon" />Chat ID</h2></div></div>
        <div className="settings-form">
          <p className="form-note">{tokenOK ? <>给你的机器人发一句 <code>/start</code>，它会回复你的 Chat ID（约 1 分钟内）。想发到群里：把机器人拉进群，在群里发 <code>/start</code>，群的 Chat ID 以 <code>-</code> 开头。</> : '先完成上一步保存 Token，机器人上线后才能回复你的 Chat ID。'}</p>
          {bot?.last_from_chat && bot.last_from_chat !== savedChat && (
            <div className="tg-detected">
              <span>刚收到来自 Chat ID <code>{bot.last_from_chat}</code> 的消息{bot.last_from_at ? `（${new Date(bot.last_from_at).toLocaleTimeString('zh-CN', { hour12: false })}）` : ''}</span>
              <button className="button button-primary" disabled={busy} onClick={() => setChat(bot.last_from_chat)}>使用这个 Chat ID</button>
            </div>
          )}
          <label className="form-field"><span>Chat ID</span>
            <input value={chat} onChange={(e) => setChat(e.target.value)} placeholder="123456789" />
          </label>
          <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
            <button className="button button-primary" disabled={busy || chat.trim() === savedChat} onClick={() => void saveChat()}>保存 Chat ID</button>
            <button className="button button-quiet" disabled={busy || !tokenOK || !chatOK} onClick={() => void test()}><Send size={15} />发送测试消息</button>
          </div>
        </div>
      </section>

      <section className="panel settings-panel">
        <div className="section-head"><div><div className="section-kicker">WHAT YOU GET</div><h2>会推送什么</h2></div></div>
        <div className="tg-features">
          <div><strong>告警</strong><span>节点离线、疑似被墙、主机到期、超流量、订阅滞留等；恢复时也会通知。同一告警 10 分钟内不重复。</span></div>
          <div><strong>登录安全</strong><span>面板从新 IP 登录、连续登录失败被封禁。</span></div>
          <div><strong>定时巡检</strong><span>解锁状态变化、IP 纯净度变化、安全评分偏低（只在有变化时）。</span></div>
          <div><strong>机器人命令</strong><span><code>/menu</code> 全部功能菜单 · <code>/status</code> 总览 · <code>/nodes</code> 节点 · <code>/servers</code> 主机 · <code>/subscriptions</code> 订阅 · <code>/alerts</code> 告警。输入框底部常驻中文菜单，点名称选择节点、主机或订阅；查询结果使用中文摘要，不再发送原始接口数据。</span></div>
          <div><strong>控制权限</strong><span>只有上面绑定的私聊 Chat ID 本人可执行管理操作。群聊保留只读查询。所有写操作与可能改变状态的 GET 都需要两分钟内单次确认；继续执行面板现有主机授权、管理员检查、限流和审计。</span></div>
          <div><strong>操作示例</strong><span>发送 /menu 展开输入框底部菜单。点击分类 → 点击名称 → 选择操作；更改操作会要求确认，/cancel 可取消。终端流、文件上传下载仍使用网页工作台。</span></div>
        </div>
      </section>
    </>
  )
}
