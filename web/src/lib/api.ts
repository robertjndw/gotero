const BASE = '/admin/api'

export type ApiErrorBody = {
  error?: { code?: string; message?: string; fields?: Record<string, string> }
}

export class ApiError extends Error {
  status: number
  code: string
  fields: Record<string, string>
  constructor(status: number, code: string, message: string, fields: Record<string, string> = {}) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.fields = fields
  }
}

let csrfToken: string | null = null
export const setCsrfToken = (t: string | null) => {
  csrfToken = t
}
export const getCsrfToken = () => csrfToken

// Endpoints that legitimately answer 401 for anonymous callers (bad credentials,
// public probes), so a 401 there must surface as an error instead of a redirect.
const PUBLIC_PATHS = [/^\/login$/, /^\/register$/, /^\/setup$/, /^\/invites\//]

let onUnauthorized: () => void = () => {
  const here = window.location.pathname + window.location.search
  if (window.location.pathname !== '/login') {
    window.location.assign(`/login?redirect=${encodeURIComponent(here)}`)
  }
}
export const setUnauthorizedHandler = (fn: () => void) => {
  onUnauthorized = fn
}

type Options = {
  method?: string
  body?: unknown
  query?: Record<string, string | number | undefined>
}

export async function api<T>(path: string, opts: Options = {}): Promise<T> {
  const method = opts.method ?? 'GET'
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (opts.body !== undefined) headers['Content-Type'] = 'application/json'
  if (method !== 'GET' && csrfToken) headers['X-CSRF-Token'] = csrfToken

  let url = BASE + path
  if (opts.query) {
    const qs = new URLSearchParams()
    for (const [k, v] of Object.entries(opts.query))
      if (v !== undefined && v !== '') qs.set(k, String(v))
    const s = qs.toString()
    if (s) url += `?${s}`
  }

  let res: Response
  try {
    // Absolute URL: Node's fetch (tests) cannot resolve relative ones.
    res = await fetch(window.location.origin + url, {
      method,
      headers,
      credentials: 'same-origin',
      body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
    })
  } catch {
    throw new ApiError(0, 'network_error', 'Could not reach the server. Check your connection.')
  }

  const token = res.headers.get('X-CSRF-Token')
  if (token) csrfToken = token

  if (res.status === 204) return undefined as T

  const text = await res.text()
  let data: unknown = undefined
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = undefined
    }
  }

  if (!res.ok) {
    const err = (data as ApiErrorBody | undefined)?.error
    if (res.status === 401 && !PUBLIC_PATHS.some((re) => re.test(path))) {
      csrfToken = null
      onUnauthorized()
    }
    throw new ApiError(
      res.status,
      err?.code ?? 'unknown_error',
      err?.message ?? `Request failed (${res.status})`,
      err?.fields,
    )
  }
  const me = data as { csrfToken?: string } | undefined
  if (me && typeof me === 'object' && typeof me.csrfToken === 'string') csrfToken = me.csrfToken
  return data as T
}
