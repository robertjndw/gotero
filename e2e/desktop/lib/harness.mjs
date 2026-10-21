// Glue between node:test scenarios, the gotero server and the Zotero clients.
//
//   import { suite, assert, eventually, userLibrary } from '../lib/harness.mjs';
//   suite('my scenario', { clients: 2 }, (s) => {
//     s.test('does a thing', async ({ A, B, user }) => { ... });
//   });
//
// Environment:
//   GOTERO_URL             server base URL as the host sees it (default http://localhost:8080)
//   GOTERO_CLIENT_URL      the same server as seen from inside the containers
//                          (default: GOTERO_URL with localhost -> host.docker.internal)
//   GOTERO_PUBLIC_URL      value for the Origin header on admin writes (default GOTERO_URL)
//   GOTERO_ADMIN_USER / GOTERO_ADMIN_PASSWORD
//   GOTERO_LOG_CMD         shell command whose output is saved as gotero.log on failure
//                          (e.g. "docker compose -f deploy/compose.yaml logs --no-color gotero")
//   GOTERO_LOG_PATH        file to copy into the artifacts instead
//   ZOTERO_VERSION         which Zotero build to run (default: current stable)
//   E2E_ARTIFACTS_DIR      where failure artifacts go

import { describe, before, after, test as nodeTest } from 'node:test';
import assert from 'node:assert/strict';
import { execSync } from 'node:child_process';
import { cpSync, mkdirSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { GoteroServer, serverReachable } from './gotero.mjs';
import { ZoteroClient } from './client.mjs';
import { fetchZotero } from '../fetch-zotero.mjs';
import { resolve as resolveVersions } from '../versions.mjs';

export { assert };

const here = path.dirname(fileURLToPath(import.meta.url));
const ARTIFACTS = process.env.E2E_ARTIFACTS_DIR || path.join(here, '..', '.cache', 'artifacts');

const serverURL = (process.env.GOTERO_URL || 'http://localhost:8080').replace(/\/+$/, '');
const clientURL = (process.env.GOTERO_CLIENT_URL
  || serverURL.replace(/\/\/(localhost|127\.0\.0\.1)/, '//host.docker.internal')).replace(/\/+$/, '');

const adminUser = process.env.GOTERO_ADMIN_USER;
const adminPassword = process.env.GOTERO_ADMIN_PASSWORD;

// Decided once at import time (top-level await) because node:test needs the
// skip flag when the suite is declared.
let skipReason = false;
if (!adminUser || !adminPassword) skipReason = 'GOTERO_ADMIN_USER / GOTERO_ADMIN_PASSWORD not set';
else if (!(await serverReachable(serverURL))) skipReason = `no gotero server reachable at ${serverURL} (set GOTERO_URL)`;

export const zoteroVersion = process.env.ZOTERO_VERSION || (skipReason ? '' : (await resolveVersions()).include[0].version);
export const isLegacy = zoteroVersion.startsWith('7.');

export const eventually = async (fn, { timeoutMs = 15000, intervalMs = 500, message = 'condition' } = {}) => {
  const deadline = Date.now() + timeoutMs;
  let last;
  while (Date.now() < deadline) {
    try {
      const v = await fn();
      if (v) return v;
    }
    catch (e) { last = e; }
    await new Promise((r) => setTimeout(r, intervalMs));
  }
  throw new Error(`timed out waiting for ${message}${last ? `: ${last.message}` : ''}`);
};

export const userLibrary = (dump) => dump.libraries.find((l) => l.type === 'user');
export const groupLibrary = (dump, groupID) => dump.libraries.find((l) => l.type === 'group' && l.groupID === groupID);
export const itemByKey = (lib, key) => lib.items.find((i) => i.key === key);

/**
 * Comparable view of a library: what two synced clients must agree on.
 * `synced` is a local flag, and the library version can differ by the number
 * of writes each client did, so neither takes part.
 */
export function normalise(lib, { ignoreItemKeys = [] } = {}) {
  const strip = (o) => {
    const c = { ...o };
    delete c.synced;
    for (const k of ignoreItemKeys) delete c[k];
    return c;
  };
  return {
    items: lib.items.map(strip),
    collections: lib.collections,
    tags: lib.tags,
  };
}

export function assertSameLibrary(a, b, opts) {
  assert.deepEqual(normalise(b, opts), normalise(a, opts));
}

async function collectGoteroLogs(dir) {
  const safe = (fn) => { try { fn(); } catch { /* best effort */ } };
  if (process.env.GOTERO_LOG_CMD) {
    safe(() => writeFileSync(path.join(dir, 'gotero.log'), execSync(process.env.GOTERO_LOG_CMD, { encoding: 'utf8', maxBuffer: 64 << 20 })));
  }
  if (process.env.GOTERO_LOG_PATH) safe(() => cpSync(process.env.GOTERO_LOG_PATH, path.join(dir, 'gotero-server-log'), { recursive: true }));
}

/**
 * Declares a scenario suite.
 * @param {string} name
 * @param {object} [opts]
 * @param {number} [opts.clients=2]   how many Zotero clients to start
 * @param {boolean} [opts.sameUser=true]  all clients log in as one user (otherwise one user each)
 * @param {number|null} [opts.quotaBytes]
 * @param {(v:string)=>boolean} [opts.onlyVersions] e.g. (v) => v.startsWith('7.')
 * @param {boolean} [opts.login=true]  false: leave clients without an API key
 * @param {(s)=>void} body
 */
export function suite(name, opts, body) {
  if (typeof opts === 'function') { body = opts; opts = {}; }
  const { clients = 2, sameUser = true, quotaBytes = null, onlyVersions, login = true, extraPrefs } = opts;
  let skip = skipReason;
  if (!skip && onlyVersions && !onlyVersions(zoteroVersion)) skip = `not applicable to Zotero ${zoteroVersion}`;

  describe(name, { skip }, () => {
    const ctx = { clients: [], users: [], server: null, zotero: null };
    const slug = name.replace(/[^a-z0-9]+/gi, '-').toLowerCase();

    before(async () => {
      ctx.zotero = await fetchZotero(zoteroVersion);
      ctx.server = new GoteroServer({
        url: serverURL, publicURL: process.env.GOTERO_PUBLIC_URL || serverURL, adminUser, adminPassword,
      });
      const names = 'ABCDEFGH';
      for (let i = 0; i < clients; i++) {
        if (i === 0 || !sameUser) ctx.users.push(await ctx.server.createUser({ prefix: slug.slice(0, 12), quotaBytes }));
      }
      ctx.user = ctx.users[0];
      for (let i = 0; i < clients; i++) {
        const c = new ZoteroClient({
          name: names[i], index: i, zotero: ctx.zotero, extraPrefs, scope: slug,
          apiURL: `${clientURL}/api/`, streamURL: `${clientURL.replace(/^http/, 'ws')}/stream`,
        });
        ctx.clients.push(c);
        ctx[names[i]] = c;
      }
      await Promise.all(ctx.clients.map((c) => c.start()));
      if (login) {
        await Promise.all(ctx.clients.map((c, i) => {
          const u = sameUser ? ctx.user : ctx.users[i];
          return c.setAPIKey({ key: u.key, userID: u.id, username: u.username });
        }));
      }
    });

    after(async () => {
      await Promise.all(ctx.clients.map((c) => c.stop()));
    });

    // Wraps a test so a failure leaves logs, dumps and server output behind.
    ctx.test = (testName, fn, testOpts) => nodeTest(testName, testOpts, async () => {
      try {
        await fn(ctx);
      }
      catch (e) {
        const dir = path.join(ARTIFACTS, `${zoteroVersion}`, slug, testName.replace(/[^a-z0-9]+/gi, '-').toLowerCase());
        mkdirSync(dir, { recursive: true });
        await Promise.all(ctx.clients.map((c) => c.collectArtifacts(dir)));
        await collectGoteroLogs(dir);
        writeFileSync(path.join(dir, 'error.txt'), String(e?.stack ?? e));
        console.error(`artifacts for failed test saved to ${dir}`);
        throw e;
      }
    });

    body(ctx);
  });
}
