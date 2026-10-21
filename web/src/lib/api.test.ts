import { describe, expect, it, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import { server } from '@/mocks/server'
import { api, ApiError, setUnauthorizedHandler } from './api'

async function login() {
  await api('/login', { method: 'POST', body: { username: 'admin', password: 'password' } })
}

describe('api client', () => {
  it('sends the CSRF token learned from the login response on mutations', async () => {
    await login()
    let seen: string | null = null
    server.use(
      http.patch('/admin/api/me', ({ request }) => {
        seen = request.headers.get('X-CSRF-Token')
        return HttpResponse.json({ ok: true })
      }),
    )
    await api('/me', { method: 'PATCH', body: { displayName: 'x' } })
    expect(seen).toBe('csrf-mock-token')
  })

  it('does not send the CSRF header on GET requests', async () => {
    await login()
    let seen: string | null = 'unset'
    server.use(
      http.get('/admin/api/me', ({ request }) => {
        seen = request.headers.get('X-CSRF-Token')
        return HttpResponse.json({})
      }),
    )
    await api('/me')
    expect(seen).toBeNull()
  })

  it('rejects mutations the server refuses for a missing token', async () => {
    await login()
    server.use(
      http.post('/admin/api/logout', () =>
        HttpResponse.json({ error: { code: 'csrf', message: 'bad token' } }, { status: 403 }),
      ),
    )
    await expect(api('/logout', { method: 'POST' })).rejects.toMatchObject({
      status: 403,
      code: 'csrf',
    })
  })

  it('maps the error envelope including field errors', async () => {
    server.use(
      http.post('/admin/api/register', () =>
        HttpResponse.json(
          { error: { code: 'validation', message: 'Nope', fields: { username: 'Taken' } } },
          { status: 422 },
        ),
      ),
    )
    const err = await api('/register', { method: 'POST', body: {} }).catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({
      status: 422,
      code: 'validation',
      message: 'Nope',
      fields: { username: 'Taken' },
    })
  })

  it('calls the unauthorized handler on 401 for protected endpoints', async () => {
    const handler = vi.fn()
    setUnauthorizedHandler(handler)
    await expect(api('/me')).rejects.toMatchObject({ status: 401 })
    expect(handler).toHaveBeenCalledTimes(1)
  })

  it('does not treat a 401 from /login as a session expiry', async () => {
    const handler = vi.fn()
    setUnauthorizedHandler(handler)
    await expect(
      api('/login', { method: 'POST', body: { username: 'admin', password: 'wrong' } }),
    ).rejects.toMatchObject({
      code: 'invalid_credentials',
    })
    expect(handler).not.toHaveBeenCalled()
  })
})
