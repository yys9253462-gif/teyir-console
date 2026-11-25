export type ThemeId = 'moss' | 'graphite' | 'ocean' | 'violet' | 'daylight'
export type ScaleId = 'standard' | 'comfort' | 'large'

export const THEMES: { id: ThemeId; label: string; hint: string; swatch: [string, string, string] }[] = [
  { id: 'moss', label: '极光翡翠', hint: '默认 · 黑曜石底 + 翡翠绿强调', swatch: ['#0b111e', '#10b981', '#34d399'] },
  { id: 'graphite', label: '极客碳黑', hint: '纯粹暗黑底 + 电光青强调', swatch: ['#09090b', '#06b6d4', '#22d3ee'] },
  { id: 'ocean', label: '深空靛蓝', hint: '深海夜空底 + 云原生蓝强调', swatch: ['#0b1120', '#3b82f6', '#60a5fa'] },
  { id: 'violet', label: '星云紫电', hint: '深邃暗紫底 + 极光紫强调', swatch: ['#0e0b1a', '#8b5cf6', '#a78bfa'] },
  { id: 'daylight', label: '白昼云白', hint: '清爽云白底 + 皇家蓝强调', swatch: ['#f6f8fa', '#2563eb', '#10b981'] },
]

export const SCALES: { id: ScaleId; label: string; hint: string }[] = [
  { id: 'standard', label: '标准', hint: '正文 14px' },
  { id: 'comfort', label: '舒适', hint: '正文 15.5px' },
  { id: 'large', label: '大字', hint: '正文 17.5px' },
]

const THEME_KEY = 'nodeops.theme'
const SCALE_KEY = 'nodeops.fontscale'

function read(key: string, fallback: string, valid: string[]) {
  try {
    const value = window.localStorage.getItem(key)
    return value && valid.includes(value) ? value : fallback
  } catch {
    return fallback
  }
}

export function loadTheme(): ThemeId {
  return read(THEME_KEY, 'moss', THEMES.map((theme) => theme.id)) as ThemeId
}

export function loadScale(): ScaleId {
  return read(SCALE_KEY, 'standard', SCALES.map((scale) => scale.id)) as ScaleId
}

export function applyAppearance(theme: ThemeId, scale: ScaleId) {
  const root = document.documentElement
  root.dataset.theme = theme
  root.dataset.fontscale = scale
  const meta = document.querySelector('meta[name="theme-color"]')
  if (meta) meta.setAttribute('content', THEMES.find((option) => option.id === theme)?.swatch[0] ?? '#0b111e')
}

export function saveTheme(theme: ThemeId) {
  try {
    window.localStorage.setItem(THEME_KEY, theme)
  } catch {
    // 浏览器禁用本地存储时仅在当前会话内存中生效
  }
}

export function saveScale(scale: ScaleId) {
  try {
    window.localStorage.setItem(SCALE_KEY, scale)
  } catch {
    // 同上
  }
}
