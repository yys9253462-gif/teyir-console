import { Check, Palette, Type } from 'lucide-react'
import { SCALES, THEMES, type ScaleId, type ThemeId } from '../lib/theme'

export function AppearancePicker({ theme, scale, onTheme, onScale }: {
  theme: ThemeId
  scale: ScaleId
  onTheme: (theme: ThemeId) => void
  onScale: (scale: ScaleId) => void
}) {
  return (
    <div className="appearance">
      <div className="appearance-caption"><Palette size={14} /><span>配色方案</span></div>
      <div className="theme-grid">
        {THEMES.map((option) => (
          <button key={option.id} className={`theme-card ${theme === option.id ? 'theme-active' : ''}`} onClick={() => onTheme(option.id)} aria-pressed={theme === option.id}>
            <span className="theme-swatch">
              <i style={{ background: option.swatch[0] }} /><i style={{ background: option.swatch[1] }} /><i style={{ background: option.swatch[2] }} />
            </span>
            <span className="theme-copy"><strong>{option.label}</strong><small>{option.hint}</small></span>
            {theme === option.id && <Check size={15} className="theme-check" />}
          </button>
        ))}
      </div>
      <div className="appearance-caption"><Type size={14} /><span>字号档位</span></div>
      <div className="scale-row">
        {SCALES.map((option) => (
          <button key={option.id} className={`scale-chip ${scale === option.id ? 'scale-active' : ''}`} onClick={() => onScale(option.id)} aria-pressed={scale === option.id}>
            <strong>{option.label}</strong><small>{option.hint}</small>
          </button>
        ))}
      </div>
      <p className="form-note">选择保存在本机浏览器（localStorage），不会同步给其他设备；切换即时生效，无需重新登录。</p>
    </div>
  )
}
