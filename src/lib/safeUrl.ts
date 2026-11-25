// 控制字符（ASCII 0x00–0x1F 与 0x7F），拦截制表符/换行等会被 URL 解析器剥离从而触发跨站绕过的字符。
const CONTROL_CHARS = /[\u0000-\u001f\u007f]/

function currentBaseUrl(): URL {
  const raw = typeof window !== 'undefined' && window.location?.href
    ? window.location.href
    : 'https://example.invalid/'
  return new URL(raw)
}

// 仅允许同源站内相对路径：严拦双斜杠 //、反斜杠（如 /\evil.com）与控制字符，
// 并经 URL 解析后强制复核解析源站（origin）与当前站点严格一致，防止 WHATWG URL 反斜杠归一引发跨站重定向。
export function safeSameOriginPath(value: string | undefined | null): string | undefined {
  if (!value || CONTROL_CHARS.test(value)) return undefined
  const trimmed = value.trim()
  if (!trimmed || !trimmed.startsWith('/')) return undefined
  if (trimmed.startsWith('//') || trimmed.startsWith('/\\') || trimmed.includes('\\')) return undefined
  try {
    const baseUrl = currentBaseUrl()
    const parsed = new URL(trimmed, baseUrl)
    if (parsed.protocol !== 'https:' && parsed.protocol !== 'http:') return undefined
    if (parsed.origin !== baseUrl.origin) return undefined
    return `${parsed.pathname}${parsed.search}${parsed.hash}`
  } catch {
    return undefined
  }
}

// 统一链接协议安全校验：仅允许绝对 http/https 或经同源复核的站内相对路径，
// 彻底拦截 javascript:/data:/vbscript:/协议相对地址（//evil）、反斜杠跨站绕过（/\evil）与控制字符。
export function safeHttpUrl(value: string | undefined | null): string | undefined {
  if (!value || CONTROL_CHARS.test(value)) return undefined
  const trimmed = value.trim()
  if (!trimmed) return undefined
  try {
    if (trimmed.startsWith('/')) {
      const safePath = safeSameOriginPath(trimmed)
      if (!safePath) return undefined
      const baseUrl = currentBaseUrl()
      const parsed = new URL(safePath, baseUrl)
      if (parsed.protocol !== 'https:' && parsed.protocol !== 'http:') return undefined
      if (parsed.origin !== baseUrl.origin) return undefined
      return parsed.href
    }
    const parsed = new URL(trimmed)
    return parsed.protocol === 'https:' || parsed.protocol === 'http:' ? parsed.href : undefined
  } catch {
    return undefined
  }
}
