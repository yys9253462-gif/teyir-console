// 统一字节量格式化工具：默认保留两位小数（>=100 或单位为 B 时取整），支持自定义零值占位与小数位数。
const BYTE_UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'] as const

export function formatBytes(
  bytes: number,
  options?: { decimals?: number; zero?: string },
): string {
  const zeroText = options?.zero ?? '0 B'
  if (!Number.isFinite(bytes) || bytes <= 0) return zeroText
  const decimals = options?.decimals ?? 2
  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < BYTE_UNITS.length - 1) {
    value /= 1024
    unit += 1
  }
  return `${value.toFixed(value >= 100 || unit === 0 ? 0 : decimals)} ${BYTE_UNITS[unit]}`
}
