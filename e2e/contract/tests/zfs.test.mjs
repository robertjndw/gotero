// Drives the real Zotero.Sync.Storage.Mode.ZFS upload and download methods.
//
// The desktop's item, DB and file layers are replaced by the small fake in
// shim/storage.mjs, but everything that talks to the server (authorization
// request, S3-style upload, registration, 302 handling, 412 resolution) is
// Zotero's own code running unmodified.
import { createHash, randomBytes } from 'node:crypto';
import { readFile, rm, stat, utimes, writeFile } from 'node:fs/promises';
import { after, before, describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { BASE_URL, clientFor, objectKey, pinnedRefs, plain, provisionUser, skipReason, zfsTodo } from './_support.mjs';

const skip = await skipReason();
const md5 = (buf) => createHash('md5').update(buf).digest('hex');
const MTIME = 1_700_000_000_000;

const attachmentJSON = (key, filename = 'paper.pdf') => ({
  key,
  version: 0,
  itemType: 'attachment',
  linkMode: 'imported_file',
  title: filename,
  contentType: 'application/pdf',
  filename,
  tags: [],
  collections: [],
  relations: {},
});

for (const ref of await pinnedRefs()) {
  describe(`[${ref}] ZFS file sync`, { skip }, () => {
    let user, c, zfs, libVersion;
    const bytes = randomBytes(40_000);

    const request = (item) => ({
      name: `1/${item.key}`,
      item,
      onProgress() {},
      isFinished: () => false,
      setChannel() {},
      engine: null,
    });

    // Registers an attachment on the server and returns a fake local item for it.
    async function newAttachment(content, mtime = MTIME) {
      const key = objectKey();
      // File registration bumps the library version too, so re-read it.
      libVersion = (await c.api.getSettings('user', user.userID)).libraryVersion;
      const r = await c.api.uploadObjects('user', user.userID, 'POST', libVersion, 'item', [attachmentJSON(key)]);
      const saved = plain(r.results.successful[0]);
      const item = await c.harness.newAttachment({ libraryID: 1, key, version: saved.version }, content, mtime);
      return item;
    }

    const serverItem = async (key) => {
      const [p] = c.api.downloadObjects('user', user.userID, 'item', [key]);
      return plain((await p).json[0]);
    };

    before(async () => {
      user = await provisionUser('zfs');
      c = await clientFor(ref, user);
      c.harness.addLibrary(1, 'user', user.userID);
      libVersion = 0;
      zfs = new c.Zotero.Sync.Storage.Mode.ZFS({ apiClient: c.api });
    });
    after(async () => {
      await user?.cleanup();
      await c?.harness.cleanup();
    });

    describe('upload', { todo: zfsTodo }, () => {
      let first;

      it('authorizes, uploads, registers and records md5/mtime on the attachment', async () => {
        first = await newAttachment(bytes);
        const before = Number(first.version);
        const result = await zfs.uploadFile(request(first));
        assert.equal(result.localChanges, true);
        assert.equal(result.remoteChanges, true);
        assert.equal(first.attachmentSyncState, 'in_sync');
        assert.equal(first.attachmentSyncedHash, md5(bytes));
        assert.equal(first.attachmentSyncedModificationTime, MTIME);
        assert.ok(Number(first.version) > before, 'registration must bump the item version');

        const remote = await serverItem(first.key);
        assert.equal(remote.data.md5, md5(bytes));
        assert.equal(remote.data.mtime, MTIME);
        assert.equal(String(remote.version), String(first.version));
      });

      it('answers {exists:1} when the same content is already stored in the library', async () => {
        const second = await newAttachment(bytes, MTIME + 5000);
        const result = await zfs.uploadFile(request(second));
        assert.equal(result.localChanges, true);
        assert.equal(second.attachmentSyncedHash, md5(bytes));
        const remote = await serverItem(second.key);
        assert.equal(remote.data.md5, md5(bytes));
        assert.equal(remote.data.mtime, MTIME + 5000);
      });

      it('reuploads changed content with If-Match on the stored hash', async () => {
        const changed = randomBytes(2048);
        await writeFile(first.filePath, changed);
        await utimes(first.filePath, new Date(MTIME + 9000), new Date(MTIME + 9000));
        const result = await zfs.uploadFile(request(first));
        assert.equal(result.remoteChanges, true);
        assert.equal(first.attachmentSyncedHash, md5(changed));
        const remote = await serverItem(first.key);
        assert.equal(remote.data.md5, md5(changed));
      });

      it('resolves a 412 by comparing hashes and flags a conflict when they differ', async () => {
        const base = await newAttachment(randomBytes(1000));
        await zfs.uploadFile(request(base));
        // Same server item, but this "device" thinks a different hash was synced.
        const other = await c.harness.newAttachment(
          { libraryID: 1, key: base.key, version: base.version },
          randomBytes(1500),
          MTIME + 20000,
        );
        other.attachmentSyncedHash = md5(Buffer.from('stale'));
        const result = await zfs.uploadFile(request(other));
        assert.equal(result.fileSyncRequired, true);
        assert.equal(other.attachmentSyncState, 'in_conflict');
      });

      it('marks the item for item sync when the server does not know it (404)', async () => {
        const ghost = await c.harness.newAttachment({ libraryID: 1, key: objectKey(), version: 1 }, randomBytes(100), MTIME);
        const result = await zfs.uploadFile(request(ghost));
        assert.equal(result.syncRequired, true);
        assert.ok(c.harness.calls.markUnsynced.includes(ghost.key));
      });
    });

    describe('download', { todo: zfsTodo }, () => {
      let source;
      // Fake locals for the same key share one path on disk, so keep the
      // original bytes here instead of re-reading the file later.
      const sourceBytes = randomBytes(30_000);
      before(async () => {
        source = await newAttachment(sourceBytes);
        await zfs.uploadFile(request(source));
      });

      it('follows the 302 and delivers file with matching md5 and mtime', async () => {
        const local = await c.harness.newAttachment({ libraryID: 1, key: source.key, version: source.version }, Buffer.alloc(0), 0);
        await rm(local.filePath); // so the download has to fetch it
        const result = await zfs.downloadFile(request(local));
        assert.equal(result.localChanges, true);
        const got = await readFile(local.filePath);
        assert.equal(md5(got), md5(sourceBytes));
        assert.equal(Math.floor((await stat(local.filePath)).mtimeMs), MTIME);
        const processed = c.harness.calls.processed.at(-1);
        assert.equal(processed.expectedMd5, processed.md5);
        assert.equal(processed.compressed, false);
      });

      it('skips the transfer when the local mtime already matches', async () => {
        const before = c.harness.calls.processed.length;
        const local = await c.harness.newAttachment({ libraryID: 1, key: source.key, version: source.version }, randomBytes(10), MTIME);
        const result = await zfs.downloadFile(request(local));
        assert.equal(result.localChanges, true);
        assert.equal(local.attachmentSyncState, 'in_sync');
        assert.equal(c.harness.calls.processed.length, before, 'no body should have been fetched');
      });

      it('skips the transfer and adopts the hash when only the mtime differs', async () => {
        const before = c.harness.calls.processed.length;
        const content = sourceBytes;
        const local = await c.harness.newAttachment({ libraryID: 1, key: source.key, version: source.version }, content, MTIME + 777);
        const result = await zfs.downloadFile(request(local));
        assert.equal(result.localChanges, true);
        assert.equal(local.attachmentSyncedHash, md5(content));
        assert.equal(c.harness.calls.processed.length, before);
      });

      it('treats an attachment without a stored file as in sync (404)', async () => {
        const empty = await newAttachment(randomBytes(10));
        const local = await c.harness.newAttachment({ libraryID: 1, key: empty.key, version: empty.version }, randomBytes(10), MTIME);
        await rm(local.filePath);
        const result = await zfs.downloadFile(request(local));
        assert.equal(result.localChanges, false);
        assert.equal(local.attachmentSyncState, 'in_sync');
      });
    });

    // Raw wire checks for the download redirect, independent of client logic.
    describe('wire format', { todo: zfsTodo }, () => {
      it('GET items/{key}/file answers 302 with the Zotero-File-* headers and an absolute Location', async () => {
        const item = await newAttachment(randomBytes(500));
        await zfs.uploadFile(request(item));
        const res = await fetch(`${BASE_URL}users/${user.userID}/items/${item.key}/file`, {
          redirect: 'manual',
          headers: { 'Zotero-API-Key': user.key, 'Zotero-API-Version': '3' },
        });
        assert.equal(res.status, 302);
        assert.match(res.headers.get('location'), /^https?:\/\//);
        assert.equal(res.headers.get('zotero-file-md5'), md5(await readFile(item.filePath)));
        assert.equal(res.headers.get('zotero-file-modification-time'), String(MTIME));
        assert.match(res.headers.get('zotero-file-compressed'), /^(Yes|No)$/);
        assert.match(res.headers.get('cache-control') || '', /no-store/);
        // The redirect target must work with no credentials at all.
        const file = await fetch(res.headers.get('location'));
        assert.equal(file.status, 200);
      });

      // Raw authorize/upload/register steps, so a test can replay one of them the way
      // a client does after losing the response (the real client retries blindly).
      const fileURL = (key) => `${BASE_URL}users/${user.userID}/items/${key}/file`;
      const apiHeaders = (extra = {}) => ({
        'Zotero-API-Key': user.key,
        'Zotero-API-Version': '3',
        'Content-Type': 'application/x-www-form-urlencoded',
        ...extra,
      });
      async function authorize(key, content, precondition) {
        const res = await fetch(fileURL(key), {
          method: 'POST',
          headers: apiHeaders(precondition),
          body: `md5=${md5(content)}&filename=paper.pdf&filesize=${content.length}&mtime=${MTIME}`,
        });
        assert.equal(res.status, 200, await res.clone().text());
        return res.json();
      }
      const sendBlob = (auth, content) =>
        fetch(auth.url, {
          method: 'POST',
          headers: { 'Content-Type': auth.contentType },
          body: Buffer.concat([Buffer.from(auth.prefix), content, Buffer.from(auth.suffix)]),
        });
      const register = (key, auth, precondition) =>
        fetch(fileURL(key), {
          method: 'POST',
          headers: apiHeaders(precondition),
          body: `upload=${auth.uploadKey}`,
        });

      it('a repeated upload POST to the same upload URL answers 201 again', async () => {
        const item = await newAttachment(randomBytes(300));
        const content = randomBytes(300);
        const auth = await authorize(item.key, content, { 'If-None-Match': '*' });
        assert.ok(auth.uploadKey, 'expected an upload authorization');
        assert.equal((await sendBlob(auth, content)).status, 201);
        assert.equal((await sendBlob(auth, content)).status, 201, 'a client that lost the first 201 retries the upload');
      });

      it('a repeated register after a lost 204 succeeds with If-None-Match: *', async () => {
        const item = await newAttachment(randomBytes(300));
        const content = randomBytes(300);
        const auth = await authorize(item.key, content, { 'If-None-Match': '*' });
        assert.equal((await sendBlob(auth, content)).status, 201);
        const first = await register(item.key, auth, { 'If-None-Match': '*' });
        assert.equal(first.status, 204);
        const again = await register(item.key, auth, { 'If-None-Match': '*' });
        assert.equal(again.status, 204, await again.text());
        assert.equal(again.headers.get('last-modified-version'), first.headers.get('last-modified-version'));
        assert.equal((await serverItem(item.key)).data.md5, md5(content));
      });

      it('a repeated register after a lost 204 succeeds with If-Match on the old hash', async () => {
        const item = await newAttachment(randomBytes(300));
        const v1 = randomBytes(300);
        const a1 = await authorize(item.key, v1, { 'If-None-Match': '*' });
        await sendBlob(a1, v1);
        assert.equal((await register(item.key, a1, { 'If-None-Match': '*' })).status, 204);

        const v2 = randomBytes(310);
        const a2 = await authorize(item.key, v2, { 'If-Match': md5(v1) });
        assert.ok(a2.uploadKey);
        assert.equal((await sendBlob(a2, v2)).status, 201);
        const first = await register(item.key, a2, { 'If-Match': md5(v1) });
        assert.equal(first.status, 204);
        // The stored hash is now v2's, so the original If-Match no longer matches;
        // the retry has to be recognised as a replay of the same upload.
        const again = await register(item.key, a2, { 'If-Match': md5(v1) });
        assert.equal(again.status, 204, await again.text());
        assert.equal((await serverItem(item.key)).data.md5, md5(v2));
      });

      it('POST items/{key}/file without If-Match or If-None-Match is 428', async () => {
        const item = await newAttachment(randomBytes(50));
        const res = await fetch(`${BASE_URL}users/${user.userID}/items/${item.key}/file`, {
          method: 'POST',
          headers: {
            'Zotero-API-Key': user.key,
            'Zotero-API-Version': '3',
            'Content-Type': 'application/x-www-form-urlencoded',
          },
          body: `md5=${md5('x')}&filename=a.pdf&filesize=1&mtime=${MTIME}`,
        });
        assert.equal(res.status, 428);
      });
    });
  });
}
