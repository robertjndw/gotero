// Thin client for the gotero admin API (docs/dev/admin-api.md), used to create
// users, API keys and groups for a test run. Cookie + CSRF handling is done by
// hand because there are no dependencies.

import { randomBytes } from 'node:crypto';

export class AdminAPI {
  constructor(baseURL, { publicURL } = {}) {
    this.base = baseURL.replace(/\/+$/, '');
    // The server checks Origin against its configured public URL on writes
    this.origin = new URL(publicURL || baseURL).origin;
    this.cookie = null;
    this.csrf = null;
  }

  async request(method, path, body) {
    const headers = { accept: 'application/json', origin: this.origin };
    if (this.cookie) headers.cookie = this.cookie;
    if (method !== 'GET' && this.csrf) headers['x-csrf-token'] = this.csrf;
    if (body !== undefined) headers['content-type'] = 'application/json';
    const res = await fetch(`${this.base}/admin/api${path}`, {
      method, headers, body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(15000),
    });
    const setCookie = res.headers.getSetCookie?.() ?? [];
    for (const c of setCookie) {
      if (c.startsWith('gotero_session=')) this.cookie = c.split(';')[0];
    }
    const csrf = res.headers.get('x-csrf-token');
    if (csrf) this.csrf = csrf;
    const text = await res.text();
    let json = null;
    try { json = text ? JSON.parse(text) : null; } catch { /* not json */ }
    if (!res.ok) {
      const msg = json?.error?.message ?? text.slice(0, 200);
      const err = new Error(`${method} /admin/api${path} -> ${res.status}: ${msg}`);
      err.status = res.status;
      err.code = json?.error?.code;
      throw err;
    }
    return json;
  }

  async login(username, password) {
    const me = await this.request('POST', '/login', { username, password });
    this.csrf = me?.csrfToken || this.csrf;
    this.me = me;
    return me;
  }
}

export async function serverReachable(baseURL) {
  for (const p of ['/admin/api/setup', '/healthz']) {
    try {
      const res = await fetch(`${baseURL.replace(/\/+$/, '')}${p}`, { signal: AbortSignal.timeout(3000) });
      if (res.status < 500) return true;
    }
    catch { /* try next */ }
  }
  return false;
}

const fullAccess = { library: true, notes: true, write: true, files: true, groups: 'all' };

export class GoteroServer {
  constructor({ url, publicURL, adminUser, adminPassword }) {
    this.url = url.replace(/\/+$/, '');
    this.publicURL = publicURL;
    this.adminUser = adminUser;
    this.adminPassword = adminPassword;
    this.admin = null;
  }

  async adminSession() {
    if (!this.admin) {
      this.admin = new AdminAPI(this.url, { publicURL: this.publicURL });
      await this.admin.login(this.adminUser, this.adminPassword);
    }
    return this.admin;
  }

  /** Creates a fresh non-admin user and a full-access API key for it. */
  async createUser({ prefix = 'e2e', quotaBytes = null } = {}) {
    const admin = await this.adminSession();
    const suffix = randomBytes(4).toString('hex');
    const username = `${prefix}-${suffix}`;
    const password = `pw-${randomBytes(9).toString('base64url')}`;
    const user = await admin.request('POST', '/admin/users', {
      username, password, email: `${username}@example.invalid`, displayName: username, isAdmin: false, quotaBytes,
    });
    const session = new AdminAPI(this.url, { publicURL: this.publicURL });
    await session.login(username, password);
    const created = await session.request('POST', '/me/keys', { name: 'desktop-e2e', access: fullAccess });
    return {
      id: user.id, username, password, key: created.key, keyID: created.id, session,
      setQuota: (bytes) => admin.request('PATCH', `/admin/users/${user.id}`, { quotaBytes: bytes }),
      revokeKey: () => session.request('DELETE', `/me/keys/${created.id}`),
      remove: () => admin.request('DELETE', `/admin/users/${user.id}`),
    };
  }

  /** Creates a group owned by the admin and adds `members` (user objects). */
  async createGroup(members, o = {}) {
    const admin = await this.adminSession();
    const group = await admin.request('POST', '/groups', {
      name: o.name || `e2e-group-${randomBytes(3).toString('hex')}`,
      description: '',
      libraryReading: 'all',
      libraryEditing: o.libraryEditing || 'members',
      fileEditing: o.fileEditing || 'members',
    });
    for (const m of members) {
      await admin.request('PUT', `/groups/${group.id}/members/${m.id}`, { role: 'member' });
    }
    return { ...group, remove: () => admin.request('DELETE', `/groups/${group.id}`) };
  }
}
