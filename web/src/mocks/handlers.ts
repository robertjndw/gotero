import { http, HttpResponse, type HttpHandler } from 'msw'
import { db, persistSession } from './db'
import type { ApiKeyAccess, Group, GroupInput, GroupRole, ImportJob, User } from '@/lib/types'

const B = '/admin/api'

function respond(body: unknown, status = 200) {
  const headers: Record<string, string> = {}
  if (db.sessionUserId) headers['X-CSRF-Token'] = db.csrf
  return body === undefined
    ? new HttpResponse(null, { status, headers })
    : HttpResponse.json(body as object, { status, headers })
}
const fail = (status: number, code: string, message: string, fields?: Record<string, string>) =>
  respond({ error: { code, message, ...(fields ? { fields } : {}) } }, status)

const current = () => db.users.find((u) => u.id === db.sessionUserId) ?? null
const me = (u: User) => ({ ...u, csrfToken: db.csrf, identities: db.identities[u.id] ?? [] })

function removeIdentity(user: User, provider: string) {
  db.identities[user.id] = (db.identities[user.id] ?? []).filter((i) => i.provider !== provider)
  user.providers = user.providers.filter((p) => p !== provider)
}

type Ctx = { request: Request; params: Record<string, string | readonly string[] | undefined> }
type Auth = { user: User }
type Handler = (ctx: Ctx, auth: Auth) => Response | Promise<Response>

// Mirrors the server contract: 401 when anonymous, 403 on a missing or wrong CSRF header.
function secured(opts: { admin?: boolean }, fn: Handler) {
  return (ctx: Ctx) => {
    const user = current()
    if (!user) return fail(401, 'unauthenticated', 'Please log in.')
    if (ctx.request.method !== 'GET' && ctx.request.headers.get('X-CSRF-Token') !== db.csrf) {
      return fail(403, 'csrf', 'Invalid or missing CSRF token.')
    }
    if (opts.admin && !user.isAdmin) return fail(403, 'forbidden', 'Administrator access required.')
    return fn(ctx, { user })
  }
}
const authed = (fn: Handler) => secured({}, fn)
const admin = (fn: Handler) => secured({ admin: true }, fn)

const num = (v: string | readonly string[] | undefined) => Number(Array.isArray(v) ? v[0] : v)
const str = (v: string | readonly string[] | undefined) => String(Array.isArray(v) ? v[0] : v)
const body = async <T>(r: Request) => (await r.json()) as T
const page = <T>(items: T[]) => ({ items, nextCursor: null })

function recomputeGroup(g: Group) {
  g.memberCount = db.members[g.id]?.length ?? 0
  g.version += 1
}

function canManage(user: User, g: Group) {
  const role = db.members[g.id]?.find((m) => m.userId === user.id)?.role
  return user.isAdmin || role === 'owner' || role === 'admin'
}

function advanceImport(job: ImportJob) {
  if (job.status === 'queued') job.status = 'running'
  else if (job.status === 'running') {
    const p = job.progress
    p.objects = Math.min(p.objectsTotal, p.objects + Math.ceil(p.objectsTotal / 4))
    p.files = Math.min(p.filesTotal, p.files + Math.ceil(p.filesTotal / 4))
    p.bytes += 38 * 1024 ** 2
    job.libraries.forEach((l, i) => {
      l.done = p.objects >= p.objectsTotal || (i === 0 && p.objects > p.objectsTotal / 2)
    })
    if (p.objects >= p.objectsTotal && p.files >= p.filesTotal) {
      job.status = 'completed'
      job.finishedAt = new Date().toISOString()
    }
  }
}

export const handlers: HttpHandler[] = [
  http.get(`${B}/setup`, () =>
    respond({
      openRegistration: db.settings.openRegistration,
      publicUrl: db.publicUrl,
      version: '0.1.0-mock',
      localLogin: db.localLogin,
      providers: db.providers,
    }),
  ),

  http.post(`${B}/login`, async ({ request }) => {
    const { username, password } = await body<{ username: string; password: string }>(request)
    const u = db.users.find((x) => x.username === username)
    if (!u || db.passwords[u.id] !== password)
      return fail(401, 'invalid_credentials', 'Invalid username or password.')
    if (u.disabled) return fail(403, 'disabled', 'This account is disabled.')
    db.sessionUserId = u.id
    persistSession()
    return respond(me(u))
  }),
  http.post(`${B}/logout`, () => {
    db.sessionUserId = null
    persistSession()
    return respond(undefined, 204)
  }),
  http.get(
    `${B}/me`,
    authed((_c, { user }) => respond(me(user))),
  ),
  http.patch(
    `${B}/me`,
    authed(async ({ request }, { user }) => {
      Object.assign(user, await body<Partial<User>>(request))
      return respond(me(user))
    }),
  ),
  http.post(
    `${B}/me/password`,
    authed(async ({ request }, { user }) => {
      const b = await body<{ currentPassword?: string; newPassword: string }>(request)
      if (!user.isAdmin && !db.localLogin) {
        return fail(403, 'local_login_disabled', 'Password sign-in is disabled.')
      }
      // Mirrors the contract: the current password is only checked when one exists.
      if (user.hasPassword && db.passwords[user.id] !== b.currentPassword) {
        return fail(422, 'validation', 'Validation failed.', {
          currentPassword: 'Current password is incorrect.',
        })
      }
      db.passwords[user.id] = b.newPassword
      user.hasPassword = true
      return respond(undefined, 204)
    }),
  ),
  http.delete(
    `${B}/me/identities/:provider`,
    authed(({ params }, { user }) => {
      const provider = str(params.provider)
      const list = db.identities[user.id] ?? []
      if (!list.some((i) => i.provider === provider)) {
        return fail(404, 'not_found', 'Identity not found.')
      }
      if (!user.hasPassword && list.length <= 1) {
        return fail(409, 'last_login_method', 'You cannot remove your only way to sign in.')
      }
      removeIdentity(user, provider)
      return respond(undefined, 204)
    }),
  ),

  http.post(`${B}/register`, async ({ request }) => {
    const b = await body<{
      username: string
      password: string
      email: string
      displayName: string
      inviteToken?: string
    }>(request)
    const invite = db.invites.find((i) => i.url.endsWith(`invite=${b.inviteToken}`) && !i.usedBy)
    if (!db.settings.openRegistration && !invite)
      return fail(403, 'registration_closed', 'Registration is by invitation only.')
    if (db.users.some((u) => u.username === b.username)) {
      return fail(409, 'conflict', 'That username is already taken.', {
        username: 'Already taken.',
      })
    }
    const u: User = {
      id: db.nextId++,
      username: b.username,
      displayName: b.displayName,
      email: b.email,
      isAdmin: invite?.isAdmin ?? false,
      disabled: false,
      quotaBytes: db.settings.defaultQuotaBytes,
      usedBytes: 0,
      createdAt: new Date().toISOString(),
      lastSyncAt: null,
      hasPassword: true,
      providers: [],
    }
    db.users.push(u)
    db.passwords[u.id] = b.password
    db.keys[u.id] = []
    if (invite) invite.usedBy = u.username
    db.sessionUserId = u.id
    persistSession()
    return respond(me(u), 201)
  }),
  http.get(`${B}/invites/:token`, ({ params }) => {
    const i = db.invites.find((x) => x.url.endsWith(`invite=${str(params.token)}`))
    const valid = !!i && !i.usedBy && new Date(i.expiresAt) > new Date()
    return respond({ valid, email: valid ? i.email : null })
  }),

  http.get(
    `${B}/me/keys`,
    authed((_c, { user }) => respond({ items: db.keys[user.id] ?? [] })),
  ),
  http.post(
    `${B}/me/keys`,
    authed(async ({ request }, { user }) => {
      const b = await body<{ name: string; access: ApiKeyAccess }>(request)
      const alphabet = 'abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789'
      const key = Array.from(
        { length: 24 },
        () => alphabet[Math.floor(Math.random() * alphabet.length)],
      ).join('')
      const k = {
        id: db.nextId++,
        name: b.name,
        prefix: key.slice(0, 4),
        access: b.access,
        createdAt: new Date().toISOString(),
        lastUsedAt: null,
      }
      ;(db.keys[user.id] ??= []).push(k)
      return respond({ ...k, key }, 201)
    }),
  ),
  http.delete(
    `${B}/me/keys/:id`,
    authed(({ params }, { user }) => {
      db.keys[user.id] = (db.keys[user.id] ?? []).filter((k) => k.id !== num(params.id))
      return respond(undefined, 204)
    }),
  ),
  http.get(
    `${B}/me/groups`,
    authed((_c, { user }) => {
      const items = db.groups.flatMap((g) => {
        const m = db.members[g.id]?.find((x) => x.userId === user.id)
        return m ? [{ ...g, role: m.role }] : []
      })
      return respond({ items })
    }),
  ),

  http.get(`${B}/login-sessions/:token`, ({ params }) => {
    const s = db.loginSessions.find((x) => x.token === str(params.token))
    if (!s) return fail(404, 'not_found', 'Unknown login session.')
    if (s.status === 'pending' && new Date(s.expiresAt) < new Date()) s.status = 'expired'
    return respond(s)
  }),
  http.post(
    `${B}/login-sessions/:token/approve`,
    authed(({ params }, { user }) => {
      const s = db.loginSessions.find((x) => x.token === str(params.token))
      if (!s) return fail(404, 'not_found', 'Unknown login session.')
      // Same order as the server: a finished session is reported as such
      // before the account check.
      if (s.status === 'expired') return fail(410, 'expired', 'This request has expired.')
      if (s.status !== 'pending') return fail(409, 'conflict', `This request is ${s.status}.`)
      if (s.requiredUserId && s.requiredUserId !== user.id) {
        return fail(403, 'user_mismatch', 'This Zotero client is linked to a different account.')
      }
      s.status = 'completed'
      return respond(s)
    }),
  ),
  http.post(
    `${B}/login-sessions/:token/cancel`,
    authed(({ params }) => {
      const s = db.loginSessions.find((x) => x.token === str(params.token))
      if (!s) return fail(404, 'not_found', 'Unknown login session.')
      if (s.status === 'pending') s.status = 'cancelled'
      return respond(s)
    }),
  ),

  http.get(
    `${B}/admin/stats`,
    admin(() =>
      respond({
        users: db.users.length,
        groups: db.groups.length,
        items: 18420,
        attachments: 6310,
        storage: {
          backend: 'local',
          usedBytes: 3.9 * 1024 ** 3,
          blobCount: 5980,
          orphanBytes: 84 * 1024 ** 2,
        },
        version: '0.1.0-mock',
      }),
    ),
  ),
  http.get(
    `${B}/admin/users`,
    admin(({ request }) => {
      const q = (new URL(request.url).searchParams.get('q') ?? '').toLowerCase()
      return respond(
        page(
          db.users.filter(
            (u) => !q || `${u.username} ${u.displayName} ${u.email}`.toLowerCase().includes(q),
          ),
        ),
      )
    }),
  ),
  http.post(
    `${B}/admin/users`,
    admin(async ({ request }) => {
      const b = await body<Partial<User> & { password: string }>(request)
      if (db.users.some((u) => u.username === b.username)) {
        return fail(409, 'conflict', 'That username is already taken.', {
          username: 'Already taken.',
        })
      }
      const u: User = {
        id: db.nextId++,
        username: b.username!,
        displayName: b.displayName ?? '',
        email: b.email ?? '',
        isAdmin: !!b.isAdmin,
        disabled: false,
        quotaBytes: b.quotaBytes ?? null,
        usedBytes: 0,
        createdAt: new Date().toISOString(),
        lastSyncAt: null,
        hasPassword: true,
        providers: [],
      }
      db.users.push(u)
      db.passwords[u.id] = b.password
      db.keys[u.id] = []
      return respond(u, 201)
    }),
  ),
  http.patch(
    `${B}/admin/users/:id`,
    admin(async ({ request, params }) => {
      const u = db.users.find((x) => x.id === num(params.id))
      if (!u) return fail(404, 'not_found', 'User not found.')
      const { password, ...rest } = await body<Partial<User> & { password?: string }>(request)
      Object.assign(u, rest)
      if (password) db.passwords[u.id] = password
      return respond(u)
    }),
  ),
  http.delete(
    `${B}/admin/users/:id/identities/:provider`,
    admin(({ params }) => {
      const u = db.users.find((x) => x.id === num(params.id))
      if (!u) return fail(404, 'not_found', 'User not found.')
      removeIdentity(u, str(params.provider))
      return respond(undefined, 204)
    }),
  ),
  http.delete(
    `${B}/admin/users/:id`,
    admin(({ params }, { user }) => {
      const id = num(params.id)
      if (id === user.id)
        return fail(409, 'self_modification', 'You cannot delete your own account.')
      db.users = db.users.filter((u) => u.id !== id)
      return respond(undefined, 204)
    }),
  ),
  http.get(
    `${B}/admin/invites`,
    admin(() => respond(page(db.invites))),
  ),
  http.post(
    `${B}/admin/invites`,
    admin(async ({ request }, { user }) => {
      const b = await body<{ email?: string; isAdmin: boolean; expiresInHours: number }>(request)
      const id = db.nextId++
      const inv = {
        id,
        url: `${db.publicUrl}/register?invite=inv_${id}${Math.random().toString(36).slice(2, 8)}`,
        email: b.email || null,
        isAdmin: b.isAdmin,
        createdBy: user.username,
        createdAt: new Date().toISOString(),
        expiresAt: new Date(Date.now() + b.expiresInHours * 3600_000).toISOString(),
        usedBy: null,
      }
      db.invites.unshift(inv)
      return respond(inv, 201)
    }),
  ),
  http.delete(
    `${B}/admin/invites/:id`,
    admin(({ params }) => {
      db.invites = db.invites.filter((i) => i.id !== num(params.id))
      return respond(undefined, 204)
    }),
  ),
  http.get(
    `${B}/admin/settings`,
    admin(() => respond(db.settings)),
  ),
  http.patch(
    `${B}/admin/settings`,
    admin(async ({ request }) => {
      Object.assign(db.settings, await body<object>(request))
      return respond(db.settings)
    }),
  ),
  http.post(
    `${B}/admin/gc`,
    admin(async ({ request }) => {
      const { dryRun } = await body<{ dryRun: boolean }>(request)
      return respond({ orphanBlobs: 17, orphanBytes: 84 * 1024 ** 2, deleted: !dryRun })
    }),
  ),

  http.get(
    `${B}/groups`,
    authed((_c, { user }) => {
      const items = user.isAdmin
        ? db.groups
        : db.groups.filter((g) => db.members[g.id]?.some((m) => m.userId === user.id))
      return respond(page(items))
    }),
  ),
  http.post(
    `${B}/groups`,
    authed(async ({ request }, { user }) => {
      const b = await body<GroupInput>(request)
      const g: Group = {
        id: db.nextId++,
        ...b,
        owner: { id: user.id, username: user.username },
        memberCount: 1,
        usedBytes: 0,
        version: 1,
        createdAt: new Date().toISOString(),
      }
      db.groups.push(g)
      db.members[g.id] = [
        { userId: user.id, username: user.username, displayName: user.displayName, role: 'owner' },
      ]
      return respond(g, 201)
    }),
  ),
  http.get(
    `${B}/groups/:id`,
    authed(({ params }) => {
      const g = db.groups.find((x) => x.id === num(params.id))
      return g ? respond(g) : fail(404, 'not_found', 'Group not found.')
    }),
  ),
  http.patch(
    `${B}/groups/:id`,
    authed(async ({ request, params }, { user }) => {
      const g = db.groups.find((x) => x.id === num(params.id))
      if (!g) return fail(404, 'not_found', 'Group not found.')
      if (!canManage(user, g)) return fail(403, 'forbidden', 'You cannot manage this group.')
      Object.assign(g, await body<Partial<GroupInput>>(request))
      g.version += 1
      return respond(g)
    }),
  ),
  http.delete(
    `${B}/groups/:id`,
    authed(({ params }, { user }) => {
      const id = num(params.id)
      const g = db.groups.find((x) => x.id === id)
      if (!g) return fail(404, 'not_found', 'Group not found.')
      if (!user.isAdmin && g.owner.id !== user.id)
        return fail(403, 'forbidden', 'Only the owner can delete a group.')
      db.groups = db.groups.filter((x) => x.id !== id)
      delete db.members[id]
      return respond(undefined, 204)
    }),
  ),
  http.get(
    `${B}/groups/:id/members`,
    authed(({ params }) => respond({ items: db.members[num(params.id)] ?? [] })),
  ),
  http.put(
    `${B}/groups/:id/members/:userId`,
    authed(async ({ request, params }) => {
      const gid = num(params.id)
      const g = db.groups.find((x) => x.id === gid)
      const u = db.users.find((x) => x.id === num(params.userId))
      if (!g || !u) return fail(404, 'not_found', 'Not found.')
      const { role } = await body<{ role: GroupRole }>(request)
      const list = (db.members[gid] ??= [])
      const existing = list.find((m) => m.userId === u.id)
      if (existing) existing.role = role
      else list.push({ userId: u.id, username: u.username, displayName: u.displayName, role })
      recomputeGroup(g)
      return respond(list.find((m) => m.userId === u.id))
    }),
  ),
  http.delete(
    `${B}/groups/:id/members/:userId`,
    authed(({ params }) => {
      const gid = num(params.id)
      const g = db.groups.find((x) => x.id === gid)
      if (!g) return fail(404, 'not_found', 'Group not found.')
      db.members[gid] = (db.members[gid] ?? []).filter((m) => m.userId !== num(params.userId))
      recomputeGroup(g)
      return respond(undefined, 204)
    }),
  ),
  http.get(
    `${B}/users/lookup`,
    authed(({ request }) => {
      const q = (new URL(request.url).searchParams.get('q') ?? '').toLowerCase()
      if (q.length < 2) return fail(422, 'validation', 'Query too short.')
      const items = db.users
        .filter((u) => `${u.username} ${u.displayName}`.toLowerCase().includes(q))
        .slice(0, 10)
        .map(({ id, username, displayName }) => ({ id, username, displayName }))
      return respond({ items })
    }),
  ),

  http.get(
    `${B}/imports`,
    authed(() => {
      db.imports.forEach(advanceImport)
      return respond({ items: [...db.imports].reverse() })
    }),
  ),
  http.post(
    `${B}/imports`,
    authed(async ({ request }) => {
      const b = await body<{ apiKey: string; includeGroups: boolean; includeFiles: boolean }>(
        request,
      )
      if (!/^[A-Za-z0-9]{16,64}$/.test(b.apiKey))
        return fail(422, 'invalid_key', 'zotero.org rejected this API key.', {
          apiKey: 'zotero.org rejected this API key.',
        })
      const job: ImportJob = {
        id: db.nextId++,
        status: 'queued',
        source: 'zotero.org',
        libraries: [
          { name: 'My Library', items: 1240, files: b.includeFiles ? 310 : 0, done: false },
          ...(b.includeGroups
            ? [{ name: 'Lab Group', items: 380, files: b.includeFiles ? 52 : 0, done: false }]
            : []),
        ],
        progress: {
          objects: 0,
          objectsTotal: b.includeGroups ? 1620 : 1240,
          files: 0,
          filesTotal: b.includeFiles ? (b.includeGroups ? 362 : 310) : 0,
          bytes: 0,
        },
        error: null,
        createdAt: new Date().toISOString(),
        finishedAt: null,
      }
      db.imports.push(job)
      return respond(job, 201)
    }),
  ),
  http.get(
    `${B}/imports/:id`,
    authed(({ params }) => {
      const j = db.imports.find((x) => x.id === num(params.id))
      return j ? respond(j) : fail(404, 'not_found', 'Import not found.')
    }),
  ),
  http.post(
    `${B}/imports/:id/cancel`,
    authed(({ params }) => {
      const j = db.imports.find((x) => x.id === num(params.id))
      if (!j) return fail(404, 'not_found', 'Import not found.')
      if (j.status === 'queued' || j.status === 'running') {
        j.status = 'cancelled'
        j.finishedAt = new Date().toISOString()
      }
      return respond(j)
    }),
  ),
]
