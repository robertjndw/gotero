const UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']

export function formatBytes(bytes: number, digits = 1): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B'
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), UNITS.length - 1)
  const v = bytes / 1024 ** i
  return `${i === 0 || v >= 100 ? Math.round(v) : v.toFixed(digits)} ${UNITS[i]}`
}

const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' })
const STEPS: [Intl.RelativeTimeFormatUnit, number][] = [
  ['year', 31536000],
  ['month', 2592000],
  ['week', 604800],
  ['day', 86400],
  ['hour', 3600],
  ['minute', 60],
]

export function relativeTime(iso: string | null | undefined, now = Date.now()): string {
  if (!iso) return 'never'
  const diff = Math.round((new Date(iso).getTime() - now) / 1000)
  const abs = Math.abs(diff)
  if (abs < 45) return 'just now'
  for (const [unit, secs] of STEPS) {
    if (abs >= secs) return rtf.format(Math.round(diff / secs), unit)
  }
  return rtf.format(Math.round(diff / 60), 'minute')
}

export function formatDate(iso: string): string {
  return new Date(iso).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
}

export const MB = 1024 ** 2
export const GB = 1024 ** 3

export function initials(name: string): string {
  return (
    name
      .split(/\s+/)
      .filter(Boolean)
      .slice(0, 2)
      .map((p) => p[0]?.toUpperCase())
      .join('') || '?'
  )
}

export function streamUrl(publicUrl: string): string {
  const u = new URL(publicUrl)
  return `${u.protocol === 'https:' ? 'wss' : 'ws'}://${u.host}/stream`
}
