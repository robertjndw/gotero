import { describe, expect, it } from 'vitest'
import { formatBytes, relativeTime, streamUrl } from './format'

describe('format', () => {
  it('formats bytes in binary units', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(1536)).toBe('1.5 KB')
    expect(formatBytes(5 * 1024 ** 3)).toBe('5.0 GB')
  })

  it('formats relative times and null as never', () => {
    const now = Date.parse('2026-01-10T12:00:00Z')
    expect(relativeTime(null)).toBe('never')
    expect(relativeTime('2026-01-10T11:59:50Z', now)).toBe('just now')
    expect(relativeTime('2026-01-10T09:00:00Z', now)).toBe('3 hours ago')
    expect(relativeTime('2026-01-12T12:00:00Z', now)).toBe('in 2 days')
  })

  it('derives the streaming url from the public url scheme', () => {
    expect(streamUrl('https://z.example.org')).toBe('wss://z.example.org/stream')
    expect(streamUrl('http://localhost:8080')).toBe('ws://localhost:8080/stream')
  })
})
