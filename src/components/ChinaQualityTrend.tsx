import { memo, useEffect, useMemo, useState } from 'react'
import { Loader2 } from 'lucide-react'
import { api, type ChinaQualityRange, type ChinaQualityResponse, type ChinaQualitySeries } from '../lib/api'

// 三网质量趋势（2026-11-01）。
//
// 只看「最近一轮」看不出线路是不是在变差：晚高峰丢包、某天起延迟翻倍，
// 都要把时间轴摊开才看得见。数据来自定时测速（约 15 分钟一轮、保留 30 天），
// 后端已按运营商 × 时间桶取平均，这里只负责画。
//
// 用手写 SVG 而不是引图表库：项目里其他图（流量柱、纯净度）也都是手写的，
// 为两条折线引一个几百 KB 的依赖不值。

const RANGES: { key: ChinaQualityRange; label: string }[] = [
  { key: '24h', label: '24 小时' },
  { key: '7d', label: '7 天' },
  { key: '30d', label: '30 天' },
]

const COLORS: Record<string, string> = {
  telecom: 'var(--accent)',
  unicom: 'var(--amber)',
  mobile: 'var(--green)',
}

const W = 640
const H = 120
const PAD_L = 40
const PAD_B = 18

function fmtMs(v: number | null) {
  return v === null || !Number.isFinite(v) ? '—' : `${Math.round(v)} ms`
}

function fmtLoss(v: number | null) {
  return v === null || !Number.isFinite(v) ? '—' : `${v < 10 ? v.toFixed(1) : Math.round(v)}%`
}

function axisLabel(t: number, range: ChinaQualityRange) {
  const d = new Date(t)
  const pad = (n: number) => String(n).padStart(2, '0')
  return range === '24h' ? `${pad(d.getHours())}:${pad(d.getMinutes())}` : `${d.getMonth() + 1}/${d.getDate()}`
}

const pickAvgMs = (p: ChinaQualitySeries['points'][number]) => p.avg_ms
const pickLossPct = (p: ChinaQualitySeries['points'][number]) => p.loss_pct

const LineChart = memo(function LineChart({ series, pick, max, unit, range, from, to, label }: {
  series: ChinaQualitySeries[]
  pick: (p: ChinaQualitySeries['points'][number]) => number | null
  max: number
  unit: string
  range: ChinaQualityRange
  from: number
  to: number
  label: string
}) {
  const safeMax = max > 0 ? max : 1
  const x = (t: number) => PAD_L + ((t - from) / Math.max(to - from, 1)) * (W - PAD_L - 4)
  const y = (v: number) => 4 + (1 - Math.max(0, Math.min(v, safeMax)) / safeMax) * (H - PAD_B - 4)
  const ticks = [0, 0.5, 1].map((f) => f * safeMax)
  return (
    <svg className="cq-chart" viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" role="img" aria-label={label}>
      {ticks.map((v) => (
        <g key={v}>
          <line x1={PAD_L} x2={W} y1={y(v)} y2={y(v)} className="cq-grid" />
          <text x={PAD_L - 6} y={y(v) + 3} className="cq-axis" textAnchor="end">{Math.round(v)}{unit}</text>
        </g>
      ))}
      {[from, (from + to) / 2, to].map((t, i) => (
        <text key={i} x={x(t)} y={H - 4} className="cq-axis" textAnchor={i === 0 ? 'start' : i === 2 ? 'end' : 'middle'}>{axisLabel(t, range)}</text>
      ))}
      {series.map((s) => {
        // 缺数据的桶断开折线，而不是把两端直接连起来 —— 连起来会把
        // 「这段时间没测到」画成「这段时间一切平稳」。
        const segments: string[] = []
        let current = ''
        for (const p of s.points ?? []) {
          const ts = Date.parse(p.t)
          const v = pick(p)
          if (!Number.isFinite(ts) || v === null || !Number.isFinite(v)) {
            if (current) segments.push(current)
            current = ''
            continue
          }
          current += `${current ? 'L' : 'M'}${x(ts).toFixed(1)},${y(v).toFixed(1)}`
        }
        if (current) segments.push(current)
        return segments.map((d, i) => (
          <path key={`${s.carrier}-${i}`} d={d} fill="none" stroke={COLORS[s.carrier] ?? 'var(--accent)'} strokeWidth={1.5} vectorEffect="non-scaling-stroke" />
        ))
      })}
    </svg>
  )
})

export function ChinaQualityTrend({ nodeId }: { nodeId: string }) {
  const [range, setRange] = useState<ChinaQualityRange>('24h')
  const [data, setData] = useState<ChinaQualityResponse | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    setError('')
    api.nodeChinaQuality(nodeId, range)
      .then((res) => { if (!cancelled) setData(res) })
      .catch((err: unknown) => { if (!cancelled) setError(err instanceof Error ? err.message : String(err)) })
      .finally(() => { if (!cancelled) setLoading(false) })
    return () => { cancelled = true }
  }, [nodeId, range])

  const seriesList = data?.series ?? []
  const view = useMemo(() => {
    if (!data) return null
    const all = (data.series ?? []).flatMap((s) => s.points ?? [])
    if (all.length === 0) return null
    const times = all.map((p) => Date.parse(p.t)).filter((t) => Number.isFinite(t))
    if (times.length === 0) return null
    const msValues = all.map((p) => p.avg_ms).filter((v): v is number => v !== null && Number.isFinite(v))
    const lossValues = all.map((p) => p.loss_pct).filter((v): v is number => v !== null && Number.isFinite(v))
    // 纵轴取整到好读的刻度；丢包至少给到 20%，否则 2% 的抖动会被放大成满屏起伏。
    const msMax = Math.max(100, Math.ceil(Math.max(...msValues, 0) / 100) * 100)
    const lossMax = Math.max(20, Math.ceil(Math.max(...lossValues, 0) / 10) * 10)
    return { from: Math.min(...times), to: Math.max(...times), msMax, lossMax }
  }, [data])

  return (
    <section className="detail-section">
      <div className="cq-head">
        <h3>三网质量趋势</h3>
        <div className="cq-ranges" role="tablist">
          {RANGES.map((r) => (
            <button key={r.key} role="tab" aria-selected={range === r.key} className={range === r.key ? 'active' : ''} onClick={() => setRange(r.key)}>{r.label}</button>
          ))}
        </div>
      </div>
      {error ? (
        <p className="form-note notice-error">读取失败：{error}</p>
      ) : !data && loading ? (
        <p className="form-note"><Loader2 size={13} className="spin" /> 加载中…</p>
      ) : !view || !data ? (
        <p className="form-note">这个区间还没有测速数据。测速约每 15 分钟自动跑一轮。</p>
      ) : (
        <div className={loading ? 'cq-body cq-loading' : 'cq-body'}>
          <div className="cq-summary">
            {seriesList.map((s) => (
              <div key={s.carrier}>
                <span><i style={{ background: COLORS[s.carrier] ?? 'var(--accent)' }} />{s.label}</span>
                <strong>{fmtMs(s.avg_ms)}</strong>
                <small>P95 {fmtMs(s.p95_ms)} · 丢包 {fmtLoss(s.loss_pct)}</small>
              </div>
            ))}
          </div>
          <div className="cq-label">平均延迟</div>
          <LineChart series={seriesList} pick={pickAvgMs} max={view.msMax} unit="" range={range} from={view.from} to={view.to} label="三网平均延迟趋势图" />
          <div className="cq-label">平均丢包</div>
          <LineChart series={seriesList} pick={pickLossPct} max={view.lossMax} unit="%" range={range} from={view.from} to={view.to} label="三网平均丢包趋势图" />
          <p className="form-note">
            每个点是该时段内该运营商全部靶点的平均。单个靶点偶尔全丢包属正常（禁 ping / 限速），
            持续丢包 ≥30% 超过 1 小时会进入告警并推送。
          </p>
        </div>
      )}
    </section>
  )
}
