// 统一剪贴板复制工具：优先走标准异步剪贴板接口，在非加密连接或内置浏览器受限时自动降级为临时文本框选中复制。
export async function copyToClipboard(text: string): Promise<boolean> {
  if (!text) return false
  try {
    if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    // 部分内置浏览器或非 HTTPS 页面会拒绝异步剪贴板，继续走降级方案。
  }
  let area: HTMLTextAreaElement | null = null
  try {
    area = document.createElement('textarea')
    area.value = text
    area.setAttribute('readonly', '')
    area.style.position = 'fixed'
    area.style.top = '-9999px'
    area.style.left = '-9999px'
    area.style.opacity = '0'
    document.body.appendChild(area)
    area.focus()
    area.select()
    area.setSelectionRange(0, area.value.length)
    return document.execCommand('copy')
  } catch {
    return false
  } finally {
    area?.remove()
  }
}
