// Shared plumbing for the contract tests: server config, reachability probe,
// test-user provisioning through the admin API, and small helpers.
import { randomBytes } from 'node:crypto';
import { loadRefs } from '../fetch.mjs';
import { loadClient } from '../shim/zotero.mjs';

export const SERVER = (process.env.GOTERO_URL || 'http://localhost:8080').replace(/\/+$/, '');
export const ADMIN_USER = process.env.GOTERO_ADMIN_USER || 'admin';
export const ADMIN_PASSWORD = process.env.GOTERO_ADMIN_PASSWORD || 'admin-dev-password';
export const BASE_URL = `${SERVER}/api/`;

const origin = new URL(SERVER).origin;

export async function pinnedRefs() {
  const cfg = await loadRefs();
  const all = Object.keys(cfg.refs);
  const only = process.env.ZOTERO_REF;
  if (!only) return all;
  if (!all.includes(only)) throw new Error(`ZOTERO_REF=${only} is not pinned in zotero-ref.json`);
  return [only];
}

// Resolves to a skip reason (string) or false. Passed straight to node:test's
// `skip` option so a missing server shows up as skipped, not failed.
export async function skipReason() {
  try {
    const res = await fetch(`${SERVER}/healthz`, { signal: AbortSignal.timeout(5000) });
    if (!res.ok) return `gotero at ${SERVER} answered /healthz with ${res.status}`;
    return false;
  } catch (e) {
    return `gotero is not reachable at ${SERVER} (${e.cause?.code || e.message}); set GOTERO_URL or start the stack`;
  }
}

// Minimal cookie + CSRF aware client for /admin/api.
class AdminSession {
  cookie = '';
  csrf = '';

  async call(method, path, body) {
    const headers = { Origin: origin };
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    if (this.cookie) headers.Cookie = this.cookie;
    if (method !== 'GET' && this.csrf) headers['X-CSRF-Token'] = this.csrf;
    const res = await fetch(`${SERVER}/admin/api${path}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const set = res.headers.getSetCookie?.() || [];
    if (set.length) this.cookie = set.map((c) => c.split(';')[0]).join('; ');
    const token = res.headers.get('x-csrf-token');
    if (token) this.csrf = token;
    const text = await res.text();
    const json = text ? JSON.parse(text) : null;
    if (!res.ok) throw new Error(`admin ${method} ${path} -> ${res.status} ${text}`);
    return json;
  }

  async login(username, password) {
    return this.call('POST', '/login', { username, password });
  }
}

const rand = (n = 6) => randomBytes(n).toString('hex');
export const FULL_ACCESS = { library: true, notes: true, write: true, files: true, groups: 'all' };

let adminPromise;
async function admin() {
  adminPromise ||= (async () => {
    const s = new AdminSession();
    await s.login(ADMIN_USER, ADMIN_PASSWORD);
    return s;
  })();
  return adminPromise;
}

// Creates a throwaway user with a full-access API key. Every test file gets
// its own so library versions start at zero and nothing leaks between files.
export async function provisionUser(label = 'contract') {
  const adm = await admin();
  const username = `${label}-${rand()}`.slice(0, 30);
  const password = `pw-${rand(12)}`;
  const user = await adm.call('POST', '/admin/users', {
    username,
    password,
    email: `${username}@example.test`,
    displayName: username,
    isAdmin: false,
    quotaBytes: null,
  });
  const session = new AdminSession();
  await session.login(username, password);
  const created = await session.call('POST', '/me/keys', { name: 'contract tests', access: FULL_ACCESS });
  if (!created.key) throw new Error('POST /me/keys did not return a key');
  return {
    userID: user.id,
    username,
    password,
    key: created.key,
    session,
    async cleanup() {
      await adm.call('DELETE', `/admin/users/${user.id}`).catch(() => {});
    },
  };
}

export async function createGroup(user, name) {
  return user.session.call('POST', '/groups', {
    name,
    description: 'contract test group',
    libraryReading: 'all',
    libraryEditing: 'members',
    fileEditing: 'members',
  });
}

// Objects built by the VM live in another realm, so deepStrictEqual would
// reject them on prototype alone. Round-trip through JSON to compare data.
export const plain = (v) => (v === undefined ? v : JSON.parse(JSON.stringify(v)));

const ALPHABET = '23456789ABCDEFGHIJKLMNPQRSTUVWXYZ';
export function objectKey() {
  const b = randomBytes(8);
  return [...b].map((x) => ALPHABET[x % ALPHABET.length]).join('');
}

export const noteItem = (extra = {}) => ({
  key: objectKey(),
  version: 0,
  itemType: 'note',
  note: '<p>contract</p>',
  tags: [],
  collections: [],
  relations: {},
  ...extra,
});

export const collection = (name, extra = {}) => ({ key: objectKey(), version: 0, name, ...extra });

// Loads the client for a ref and binds an API client to a user.
export async function clientFor(ref, user) {
  const c = await loadClient(ref);
  const api = c.makeApiClient({ baseURL: BASE_URL, apiKey: user.key });
  return { ...c, api, libType: 'user', libID: user.userID };
}

// Escape hatch for pointing the suite at a gotero build without file sync:
// GOTERO_ZFS_TODO=1 reports ZFS failures as `todo` instead of failing the run.
export const zfsTodo = process.env.GOTERO_ZFS_TODO === '1' ? 'gotero file sync (ZFS) is not available on this server' : false;
