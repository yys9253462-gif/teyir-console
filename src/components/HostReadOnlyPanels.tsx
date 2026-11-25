import { useEffect, useRef, useState } from 'react'
import { api, type HostContainer, type HostComposeProject, type HostFileList, type HostFilePreview } from '../lib/api'
import { parentDirectory } from '../lib/hostOpsLifecycle'
import { HostOpsError, HostOpsTable, hostOpsError } from './HostOpsView'

export function HostDockerPanel({ serverId, kind }: { serverId: string; kind: 'docker' | 'compose' }) {
  const [containers, setContainers] = useState<HostContainer[]>([])
  const [projects, setProjects] = useState<HostComposeProject[]>([])
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const [generation, refresh] = useState(0)
  useEffect(() => {
    const controller = new AbortController()
    setLoading(true); setError(''); setContainers([]); setProjects([])
    const load = async () => {
      try {
        if (kind === 'docker') {
          const data = await api.hostContainers(serverId, controller.signal)
          if (controller.signal.aborted) return
          if (!Array.isArray(data.containers)) throw new Error('容器列表响应格式无效')
          setContainers(data.containers)
        } else {
          const data = await api.hostCompose(serverId, controller.signal)
          if (controller.signal.aborted) return
          if (!Array.isArray(data.projects)) throw new Error('Compose 列表响应格式无效')
          setProjects(data.projects)
        }
      } catch (cause) { if (!controller.signal.aborted) setError(hostOpsError(cause)) }
      finally { if (!controller.signal.aborted) setLoading(false) }
    }
    void load()
    return () => controller.abort()
  }, [serverId, kind, generation])
  return <div>
    <div className="host-ops-toolbar"><button type="button" className="button button-quiet" disabled={loading} onClick={() => refresh((n) => n + 1)}>刷新{kind === 'docker' ? '容器' : '项目'}</button><span className="host-terminal-status">仅只读 · 不提供启动、停止或删除</span></div>
    <HostOpsError error={error} />
    {loading ? <p role="status">正在读取主机数据…</p> : !error && (kind === 'docker'
      ? <HostOpsTable columns={['容器', '镜像', '状态', '端口']} rows={containers.map((item) => [<span title={item.id}>{item.name || item.id}</span>, item.image, `${item.state} · ${item.status}`, item.ports || '—'])} empty="主机未返回容器" />
      : <HostOpsTable columns={['项目', '状态', '配置文件']} rows={projects.map((item) => [item.name, item.status, item.config_files || '—'])} empty="主机未返回 Compose 项目" />)}
  </div>
}

export function HostFilesPanel({ serverId }: { serverId: string }) {
  const [input, setInput] = useState('/')
  const [listing, setListing] = useState<HostFileList | null>(null)
  const [preview, setPreview] = useState<HostFilePreview | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const request = useRef<AbortController | null>(null)
  async function load(path: string, file = false) {
    request.current?.abort()
    const controller = new AbortController()
    request.current = controller
    setLoading(true); setError(''); setPreview(null)
    try {
      if (file) {
        const data = await api.hostFilePreview(serverId, path, controller.signal)
        if (!controller.signal.aborted) setPreview(data)
      }
      else {
        const data = await api.hostFiles(serverId, path, controller.signal)
        if (controller.signal.aborted) return
        if (!Array.isArray(data.entries)) throw new Error('目录响应格式无效')
        setListing(data); setInput(data.path)
      }
    } catch (cause) { if (!controller.signal.aborted) setError(hostOpsError(cause)) }
    finally { if (!controller.signal.aborted) setLoading(false) }
  }
  useEffect(() => { void load('/'); return () => request.current?.abort() }, [serverId])
  return <div>
    <p>只读目录与有限文本预览；不提供写入、上传、下载或删除。二进制文件、超限或不可读路径由服务端拒绝。</p>
    <form className="host-ops-toolbar" onSubmit={(event) => { event.preventDefault(); if (input.trim()) void load(input.trim()) }}>
      <button type="button" className="button button-quiet" disabled={loading || !listing || listing.path === '/'} onClick={() => void load(parentDirectory(listing!.path))}>上级目录</button>
      <input aria-label="目录路径" value={input} onChange={(event) => setInput(event.target.value)} spellCheck={false} placeholder="/" />
      <button type="submit" className="button button-quiet" disabled={loading || !input.trim()}>打开目录</button>
      <button type="button" className="button button-quiet" disabled={loading} onClick={() => void load(listing?.path ?? '/')}>刷新目录</button>
    </form>
    <HostOpsError error={error} />
    {loading && <p role="status">正在读取…</p>}
    {listing?.truncated && <p role="status">目录条目超过 2000，仅显示前 2000 项。</p>}
    {listing && <HostOpsTable columns={['名称', '类型', '大小', '权限', '修改时间']} rows={listing.entries.map((entry) => [
      <button type="button" className="host-file-link" disabled={loading || (entry.type !== 'directory' && entry.type !== 'file')} title={entry.type === 'symlink' || entry.type === 'other' ? '不支持预览链接或特殊文件' : undefined} onClick={() => void load(entry.path, entry.type !== 'directory')}>{entry.name}</button>, ({ directory: '目录', file: '文件', symlink: '链接', other: '特殊文件' })[entry.type], entry.type === 'directory' ? '—' : `${entry.size} B`, entry.mode, entry.modified_at,
    ])} empty="空目录" />}
    {preview && <section aria-label="文本预览"><p>{preview.path}{preview.truncated ? ' · 已截断，仅展示部分内容' : ''}</p><pre className="host-ops-preview">{preview.content.slice(0, 65536)}</pre>{preview.content.length > 65536 && <p>浏览器预览上限 64 Ki 字符，剩余内容未显示。</p>}</section>}
  </div>
}
