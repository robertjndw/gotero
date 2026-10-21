import { after, before, describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { clientFor, collection, noteItem, objectKey, pinnedRefs, plain, provisionUser, skipReason } from './_support.mjs';

const skip = await skipReason();
const num = Number;

for (const ref of await pinnedRefs()) {
  describe(`[${ref}] object upload, versions and download`, { skip }, () => {
    let user, c, T, ID;
    // Library version as the sync engine tracks it between steps.
    let libVersion = 0;
    const post = async (type, objects, version = libVersion) => {
      const r = await c.api.uploadObjects(T, ID, 'POST', version, type, objects);
      libVersion = num(r.libraryVersion);
      return plain(r);
    };

    before(async () => {
      user = await provisionUser('objects');
      c = await clientFor(ref, user);
      T = 'user';
      ID = user.userID;
    });
    after(() => user?.cleanup());

    describe('uploadObjects', () => {
      const col = collection('contract collection');

      it('creates objects and keys the response by array index', async () => {
        const r = await post('collection', [col]);
        assert.equal(r.libraryVersion, String(libVersion));
        assert.deepEqual(r.results.success, { 0: col.key });
        assert.deepEqual(r.results.unchanged, {});
        assert.deepEqual(r.results.failed, {});
        const saved = r.results.successful[0];
        assert.equal(saved.key, col.key);
        assert.equal(saved.version, libVersion);
        assert.equal(saved.data.version, libVersion, 'data.version must match the new library version');
        assert.equal(saved.data.name, col.name);
        col.version = saved.version;
      });

      it('reports an identical re-upload as unchanged and keeps the library version', async () => {
        const before = libVersion;
        const r = await post('collection', [{ ...col }]);
        assert.deepEqual(r.results.unchanged, { 0: col.key });
        assert.deepEqual(r.results.success, {});
        assert.equal(libVersion, before);
      });

      it('mixes success, unchanged and failed in one response', async () => {
        const fresh = noteItem();
        const orphan = noteItem({ parentItem: 'ZZZZZZZZ' });
        const r = await post('item', [fresh, orphan]);
        assert.deepEqual(r.results.success, { 0: fresh.key });
        assert.equal(r.results.failed[1].key, orphan.key);
        assert.equal(r.results.failed[1].code, 409, '409 is a permanent error for the client, not a reset');
        assert.equal(typeof r.results.failed[1].message, 'string');
      });

      it('fails an object with an outdated version as 412', async () => {
        const stale = { ...col, version: col.version - 1 || 99, name: 'renamed' };
        // Move the stored version forward first so "outdated" is unambiguous.
        const bumped = await post('collection', [{ ...col, name: 'bumped' }]);
        const newVersion = bumped.results.successful[0].version;
        assert.ok(newVersion > col.version);
        const r = await post('collection', [{ ...stale, version: col.version }]);
        assert.equal(r.results.failed[0].code, 412);
        col.version = newVersion;
      });

      it('fails an update to a missing object as 404', async () => {
        const r = await post('item', [noteItem({ version: 5 })]);
        assert.equal(r.results.failed[0].code, 404);
      });

      it('sends gzip bodies of 1000+ bytes and the server accepts them', async () => {
        const big = noteItem({ note: `<p>${'lorem ipsum '.repeat(300)}</p>` });
        const r = await post('item', [big]);
        assert.deepEqual(r.results.success, { 0: big.key });
        assert.equal(r.results.successful[0].data.note, big.note);
      });

      it('rejects a stale library version with a 412 exposing Last-Modified-Version', async () => {
        await assert.rejects(
          c.api.uploadObjects(T, ID, 'POST', 0, 'collection', [collection('late')]),
          (e) => {
            assert.ok(e instanceof c.Zotero.HTTP.UnexpectedStatusException);
            assert.equal(e.status, 412);
            assert.equal(num(e.xmlhttp.getResponseHeader('Last-Modified-Version')), libVersion);
            return true;
          },
        );
      });

      it('refuses PUT-style methods before touching the network', async () => {
        await assert.rejects(c.api.uploadObjects(T, ID, 'PUT', libVersion, 'item', []), /Invalid method/);
      });
    });

    describe('getVersions', () => {
      const search = {
        key: objectKey(),
        version: 0,
        name: 'contract search',
        conditions: [{ condition: 'title', operator: 'contains', value: 'x' }],
      };
      const parent = noteItem();
      const child = noteItem({ parentItem: parent.key });
      const trashed = noteItem({ deleted: 1 });
      let beforeFirst, beforeLast;

      it('uploads fixtures', async () => {
        beforeFirst = libVersion;
        const s = await post('search', [search]);
        assert.deepEqual(s.results.success, { 0: search.key });
        beforeLast = libVersion;
        const p = await post('item', [parent, trashed]);
        assert.deepEqual(p.results.failed, {});
        const k = await post('item', [child]);
        assert.deepEqual(k.results.failed, {});
      });

      it('lists collections as {key: version} with the library version', async () => {
        const r = plain(await c.api.getVersions(T, ID, 'collection'));
        assert.equal(r.libraryVersion, String(libVersion));
        assert.ok(Object.keys(r.versions).length >= 1);
        for (const v of Object.values(r.versions)) assert.equal(typeof v, 'number');
      });

      it('lists searches', async () => {
        const r = plain(await c.api.getVersions(T, ID, 'search'));
        assert.equal(r.versions[search.key], beforeLast);
      });

      it('items include trashed objects, because the client always asks for them', async () => {
        const r = plain(await c.api.getVersions(T, ID, 'item'));
        assert.ok(parent.key in r.versions);
        assert.ok(child.key in r.versions);
        assert.ok(trashed.key in r.versions, 'includeTrashed=1 must be honoured');
      });

      it('top-level listing omits child items', async () => {
        const r = plain(await c.api.getVersions(T, ID, 'item', { top: true }));
        assert.ok(parent.key in r.versions);
        assert.ok(!(child.key in r.versions));
      });

      it('since filters to objects modified after that version', async () => {
        const r = plain(await c.api.getVersions(T, ID, 'item', { since: beforeLast }));
        assert.ok(parent.key in r.versions);
        for (const v of Object.values(r.versions)) assert.ok(v > beforeLast);
        const none = plain(await c.api.getVersions(T, ID, 'search', { since: beforeFirst }));
        assert.ok(search.key in none.versions);
      });

      // getVersions sends no If-Modified-Since-Version header, so an up to date
      // library is a 200 with an empty map. The client also copes with a 304
      // (it returns false), which the server may legitimately send.
      it('since at the current version yields nothing new', async () => {
        for (const type of ['item', 'collection', 'search']) {
          const r = plain(await c.api.getVersions(T, ID, type, { since: libVersion }));
          if (r === false) continue;
          assert.equal(r.libraryVersion, String(libVersion));
          assert.deepEqual(r.versions, {});
        }
      });
    });

    describe('downloadObjects', () => {
      const keys = [];

      it('fetches a single object by key', async () => {
        const k = noteItem();
        await post('item', [k]);
        const [promise, ...rest] = c.api.downloadObjects(T, ID, 'item', [k.key]);
        assert.equal(rest.length, 0);
        const r = plain(await promise);
        assert.deepEqual(r.keys, [k.key]);
        assert.equal(r.error, undefined);
        assert.equal(r.json.length, 1);
        assert.equal(r.json[0].key, k.key);
        assert.equal(r.json[0].data.itemType, 'note');
        assert.equal(r.json[0].data.version, r.json[0].version);
      });

      it('returns no promises for an empty key list', () => {
        assert.deepEqual(plain(c.api.downloadObjects(T, ID, 'item', [])), []);
      });

      it('splits more than 100 keys into batches of 100', async () => {
        // The server caps one write at 50 objects, so seed in chunks.
        for (let i = 0; i < 5; i++) {
          const batch = Array.from({ length: 50 }, () => noteItem());
          batch.forEach((b) => keys.push(b.key));
          const r = await post('item', batch);
          assert.deepEqual(r.results.failed, {});
        }
        const promises = c.api.downloadObjects(T, ID, 'item', keys);
        assert.equal(promises.length, 3);
        const results = plain(await Promise.all(promises));
        assert.deepEqual(results.map((r) => r.keys.length), [100, 100, 50]);
        const got = results.flatMap((r) => r.json.map((o) => o.key));
        assert.deepEqual([...got].sort(), [...keys].sort());
        for (const r of results) assert.equal(r.error, undefined);
      });

      it('silently omits keys that do not exist', async () => {
        const known = keys[0];
        const [p] = c.api.downloadObjects(T, ID, 'item', [known, 'AAAAAAAA']);
        const r = plain(await p);
        assert.deepEqual(r.json.map((o) => o.key), [known]);
      });

      it('returns collections and searches in the expected shape', async () => {
        const col = collection('dl');
        const r1 = await post('collection', [col]);
        const [p] = c.api.downloadObjects(T, ID, 'collection', [col.key]);
        const r = plain(await p);
        assert.equal(r.json[0].data.name, 'dl');
        assert.equal(r.json[0].version, r1.results.successful[0].version);
        assert.equal(r.json[0].data.parentCollection, false);
      });

      it('serializes attachment fields with linkMode before filename', async () => {
        const att = {
          key: objectKey(),
          version: 0,
          itemType: 'attachment',
          linkMode: 'imported_file',
          title: 'a.pdf',
          contentType: 'application/pdf',
          filename: 'a.pdf',
          tags: [],
          collections: [],
          relations: {},
        };
        const up = await post('item', [att]);
        assert.deepEqual(up.results.failed, {});
        const [p] = c.api.downloadObjects(T, ID, 'item', [att.key]);
        const data = (await p).json[0].data;
        const order = Object.keys(data);
        assert.ok(order.indexOf('linkMode') < order.indexOf('filename'), `field order was ${order.join(',')}`);
        assert.equal(data.md5, null);
        assert.equal(data.mtime, null);
      });
    });

    describe('uploadDeletions and getDeleted', () => {
      it('deletes objects and lists them in the deleted log', async () => {
        const col = collection('doomed');
        const item = noteItem();
        const s = { key: objectKey(), version: 0, name: 'doomed', conditions: [{ condition: 'title', operator: 'contains', value: 'z' }] };
        await post('collection', [col]);
        await post('item', [item]);
        await post('search', [s]);
        const base = libVersion;

        const v1 = await c.api.uploadDeletions(T, ID, libVersion, 'collection', [col.key]);
        assert.ok(num(v1) > base);
        const v2 = await c.api.uploadDeletions(T, ID, v1, 'item', [item.key]);
        const v3 = await c.api.uploadDeletions(T, ID, v2, 'search', [s.key]);
        libVersion = num(v3);

        const r = plain(await c.api.getDeleted(T, ID, base));
        assert.equal(r.libraryVersion, String(libVersion));
        assert.deepEqual(r.deleted.collections, [col.key]);
        assert.deepEqual(r.deleted.items, [item.key]);
        assert.deepEqual(r.deleted.searches, [s.key]);
        assert.deepEqual(r.deleted.tags, []);
        assert.deepEqual(r.deleted.settings, []);

        const [p] = c.api.downloadObjects(T, ID, 'item', [item.key]);
        assert.deepEqual(plain((await p).json), []);
      });

      it('deleting several keys in one request works and missing keys are not an error', async () => {
        const items = [noteItem(), noteItem()];
        await post('item', items);
        const v = await c.api.uploadDeletions(T, ID, libVersion, 'item', [...items.map((i) => i.key), 'AAAAAAAA']);
        libVersion = num(v);
        const r = plain(await c.api.getDeleted(T, ID, libVersion - 1));
        for (const i of items) assert.ok(r.deleted.items.includes(i.key));
      });

      it('uploadDeletions with a stale library version throws 412', async () => {
        await assert.rejects(
          c.api.uploadDeletions(T, ID, 0, 'item', [objectKey()]),
          (e) => e instanceof c.Zotero.HTTP.UnexpectedStatusException && e.status === 412,
        );
      });

      it('getDeleted(since = 0) works for a full sync', async () => {
        const r = plain(await c.api.getDeleted(T, ID, 0));
        assert.equal(r.libraryVersion, String(libVersion));
        assert.ok(r.deleted.items.length >= 3);
      });

      it('getDeleted at the current version is an empty log, not an error', async () => {
        const r = plain(await c.api.getDeleted(T, ID, libVersion));
        assert.deepEqual(r.deleted, { collections: [], searches: [], items: [], tags: [], settings: [] });
      });
    });
  });
}
