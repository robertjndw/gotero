import { expect, request, type APIRequestContext, type Page } from '@playwright/test'

export const baseURL = process.env.GOTERO_URL ?? 'http://localhost:8080'

// Defaults match the root compose.yaml dev stack (`make dev`).
export const admin = {
  username: process.env.GOTERO_ADMIN_USER ?? 'admin',
  password: process.env.GOTERO_ADMIN_PASSWORD ?? 'gotero-admin',
}

// The server keeps state between runs, so every account and group a test
// makes gets a fresh name. Lowercase only: user lookup is case-insensitive
// but the assertions compare exact text.
export function unique(prefix: string) {
  const rand = Math.random().toString(36).slice(2, 7)
  return `${prefix}-${Date.now().toString(36)}${rand}`
}

export type Credentials = { username: string; password: string }

export async function login(page: Page, { username, password }: Credentials) {
  await page.goto('/login')
  await page.getByLabel('Username').fill(username)
  await page.getByLabel('Password', { exact: true }).fill(password)
  await page.getByRole('button', { name: 'Log in' }).click()
  await expect(page.getByRole('heading', { name: /^Welcome, / })).toBeVisible()
}

export async function logout(page: Page) {
  await page.getByRole('button', { name: 'User menu' }).click()
  await page.getByRole('menuitem', { name: 'Log out' }).click()
  await expect(page).toHaveURL(/\/login$/)
}

// A logged-in admin API client for test setup. Journeys under test go
// through the UI; this only creates the other side of an interaction (a
// second user to add to a group, say) so each test stays short.
export class AdminApi {
  private constructor(
    private readonly ctx: APIRequestContext,
    private readonly csrf: string,
  ) {}

  static async login(creds: Credentials = admin) {
    // Public POSTs are checked against the server's own origin, like a browser would send.
    const ctx = await request.newContext({ baseURL, extraHTTPHeaders: { Origin: baseURL } })
    const res = await ctx.post('/admin/api/login', { data: creds })
    expect(res.status(), await res.text()).toBe(200)
    const me = (await res.json()) as { csrfToken: string }
    return new AdminApi(ctx, me.csrfToken)
  }

  async createUser(prefix: string): Promise<Credentials & { id: number; displayName: string }> {
    const username = unique(prefix)
    const password = `pw-${username}`
    const displayName = `${prefix} ${username.slice(prefix.length + 1)}`
    const res = await this.ctx.post('/admin/api/admin/users', {
      headers: { 'X-CSRF-Token': this.csrf },
      data: { username, password, displayName, email: `${username}@example.org` },
    })
    expect(res.status(), await res.text()).toBe(201)
    const u = (await res.json()) as { id: number }
    return { id: u.id, username, password, displayName }
  }

  async dispose() {
    await this.ctx.dispose()
  }
}

// What GET /api/keys/current says about a raw key, from outside any browser
// session, the way a script or the Zotero client would ask.
export async function keyInfo(key: string) {
  const ctx = await request.newContext({ baseURL })
  try {
    const res = await ctx.get('/api/keys/current', {
      headers: { 'Zotero-API-Key': key, 'Zotero-API-Version': '3' },
    })
    return { status: res.status(), body: res.ok() ? await res.json() : await res.text() }
  } finally {
    await ctx.dispose()
  }
}
