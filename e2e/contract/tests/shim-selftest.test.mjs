// Sanity check for our own shim: runs the real ZFS code against a tiny
// in-process fake of the file endpoints written from docs/dev/protocol-notes.md.
// If gotero's ZFS tests fail while this passes, the server is at fault; if this
// fails, the shim is. It needs no gotero instance.
import { createServer } from 'node:http';
import { createHash, randomBytes } from 'node:crypto';
import { readFile, rm, stat } from 'node:fs/promises';
import { after, before, describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { loadClient } from '../shim/zotero.mjs';
import { pinnedRefs } from './_support.mjs';

const md5 = (b) => createHash('md5').update(b).digest('hex');
const KEY = 'ABCD2345';
const MTIME = 1_700_000_000_000;

function fakeFileServer() {
  const item = { version: 1, md5: null, mtime: null };
  const blobs = new Map();
  const pending = new Map();
  let lv = 1;
  const server = createServer(async (req, res) => {
    const chunks = [];
    for await (const ch of req) chunks.push(ch);
    const body = Buffer.concat(chunks);
    const url = new URL(req.url, 'http://x');
    const origin = `http://${req.headers.host}`;
    const send = (status, headers = {}, payload = '') => {
      res.writeHead(status, { 'Last-Modified-Version': String(lv), ...headers });
      res.end(payload);
    };
    if (url.pathname === `/api/users/1/items/${KEY}/file` && req.method === 'POST') {
      const form = new URLSearchParams(body.toString());
      if (form.get('upload')) {
        const p = pending.get(form.get('upload'));
        blobs.set(p.md5, p.bytes);
        Object.assign(item, { md5: p.md5, mtime: p.mtime, version: ++lv });
        return send(204);
      }
      const sum = form.get('md5');
      if (blobs.has(sum)) {
        Object.assign(item, { md5: sum, mtime: Number(form.get('mtime')), version: ++lv });
        return send(200, { 'Content-Type': 'application/json' }, JSON.stringify({ exists: 1 }));
      }
      pending.set('upkey', { md5: sum, mtime: Number(form.get('mtime')) });
      return send(200, { 'Content-Type': 'application/json' }, JSON.stringify({
        url: `${origin}/upload`, contentType: 'application/octet-stream', prefix: '', suffix: '', uploadKey: 'upkey',
      }));
    }
    if (url.pathname === '/upload' && req.method === 'POST') {
      pending.get('upkey').bytes = body;
      return send(201);
    }
    if (url.pathname === `/api/users/1/items/${KEY}/file` && req.method === 'GET') {
      if (!item.md5) return send(404);
      return send(302, {
        Location: `${origin}/blob/${item.md5}`,
        'Zotero-File-MD5': item.md5,
        'Zotero-File-Modification-Time': String(item.mtime),
        'Zotero-File-Compressed': 'No',
      });
    }
    if (url.pathname.startsWith('/blob/')) {
      assert.equal(req.headers['zotero-api-key'], undefined, 'redirect target must be fetched without credentials');
      return send(200, {}, blobs.get(url.pathname.slice(6)));
    }
    if (url.pathname === '/api/users/1/items') {
      return send(200, { 'Content-Type': 'application/json' }, JSON.stringify([
        { key: KEY, version: item.version, data: { key: KEY, version: item.version, md5: item.md5, mtime: item.mtime } },
      ]));
    }
    send(404);
  });
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve({ server, port: server.address().port, item })));
}

for (const ref of await pinnedRefs()) {
  describe(`[${ref}] shim self-test against a fake ZFS server`, () => {
    let fake, c, zfs;
    const bytes = randomBytes(5000);
    const req = (item) => ({ name: item.key, item, onProgress() {}, isFinished: () => false, setChannel() {}, engine: null });

    before(async () => {
      fake = await fakeFileServer();
      c = await loadClient(ref);
      c.harness.addLibrary(1, 'user', 1);
      const api = c.makeApiClient({ baseURL: `http://127.0.0.1:${fake.port}/api/`, apiKey: 'k'.repeat(24) });
      zfs = new c.Zotero.Sync.Storage.Mode.ZFS({ apiClient: api });
    });
    after(async () => {
      fake?.server.close();
      await c?.harness.cleanup();
    });

    it('uploads through authorize, upload and register', async () => {
      const item = await c.harness.newAttachment({ libraryID: 1, key: KEY, version: 1 }, bytes, MTIME);
      const r = await zfs.uploadFile(req(item));
      assert.equal(r.remoteChanges, true);
      assert.equal(item.attachmentSyncedHash, md5(bytes));
      assert.equal(String(item.version), '2');
      assert.equal(fake.item.md5, md5(bytes));
    });

    it('downloads through the 302 and verifies md5 and mtime', async () => {
      const item = await c.harness.newAttachment({ libraryID: 1, key: KEY, version: 2 }, Buffer.alloc(1), 0);
      await rm(item.filePath);
      const r = await zfs.downloadFile(req(item));
      assert.equal(r.localChanges, true);
      assert.deepEqual(await readFile(item.filePath), bytes);
      assert.equal(Math.floor((await stat(item.filePath)).mtimeMs), MTIME);
    });

    it('skips the download when mtime matches', async () => {
      const n = c.harness.calls.processed.length;
      const item = await c.harness.newAttachment({ libraryID: 1, key: KEY, version: 2 }, randomBytes(9), MTIME);
      const r = await zfs.downloadFile(req(item));
      assert.equal(r.localChanges, true);
      assert.equal(c.harness.calls.processed.length, n);
    });
  });
}
