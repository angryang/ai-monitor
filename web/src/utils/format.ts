/**
 * Tiny formatting helpers. Inline here so the bundle stays one file for the
 * utilities set; if the project grows we can split per feature.
 */

export function fmtSeconds(s: number, digits = 2): string {
  if (!Number.isFinite(s)) return '–'
  if (s < 0.001) return `${(s * 1_000_000).toFixed(0)}µs`
  if (s < 1) return `${(s * 1000).toFixed(digits)}ms`
  if (s < 60) return `${s.toFixed(digits)}s`
  return `${(s / 60).toFixed(1)}m`
}

export function fmtPercent(v: number, digits = 1): string {
  if (!Number.isFinite(v)) return '–'
  return `${(v * 100).toFixed(digits)}%`
}

export function fmtCount(v: number): string {
  if (!Number.isFinite(v)) return '–'
  if (Math.abs(v) >= 1_000_000) return `${(v / 1_000_000).toFixed(1)}M`
  if (Math.abs(v) >= 1_000) return `${(v / 1_000).toFixed(1)}k`
  return v.toString()
}