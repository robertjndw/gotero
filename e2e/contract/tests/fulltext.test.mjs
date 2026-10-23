import { after, before, describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { clientFor, objectKey, pinnedRefs, plain, provisionUser, skipReason } from './_support.mjs';

const skip = await skipReason();

for (const ref of await pinnedRefs()) {
  describe(`[${ref}] full text`, { skip }, () => {
    let user, c, key, libVersion;

    before(async () => {
      user = await provisionUser('ft');
      c = await clientFor(ref, user);
      key = objectKey();
      const r = await c.api.uploadObjects('user', user.userID, 'POST', 0, 'item', [
        {
          key,
          version: 0,
          itemType: 'attachment',
          linkMode: 'imported_file',
          title: 'doc.pdf',
          contentType: 'application/pdf',
          filename: 'doc.pdf',
          tags: [],
          collections: [],
          relations: {},
        },
      ]);
      libVersion = Number(r.libraryVersion);
    });
    after(() => user?.cleanup());

    it('getFullTextVersions on a library without full text is empty but versioned', async () => {
      const r = plain(await c.api.getFullTextVersions('user', user.userID, 0));
      assert.deepEqual(r.versions, {});
      assert.equal(r.libraryVersion, String(libVersion));
    });

    it('getFullTextForItem returns false (404) before anything is stored', async () => {
      assert.equal(await c.api.getFullTextForItem('user', user.userID, key), false);
    });

    it('setFullTextForItems stores content and answers in the write-response format', async () => {
      const r = plain(
        await c.api.setFullTextForItems('user', user.userID, libVersion, [
          { key, content: 'the quick brown fox', indexedChars: 19, totalChars: 19 },
        ]),
      );
      assert.ok(Number(r.libraryVersion) > libVersion);
      assert.deepEqual(r.results.success, { 0: key });
      assert.deepEqual(r.results.failed, {});
      libVersion = Number(r.libraryVersion);
    });

    it('getFullTextVersions maps item key to a version', async () => {
      const r = plain(await c.api.getFullTextVersions('user', user.userID, 0));
      assert.deepEqual(Object.keys(r.versions), [key]);
      assert.equal(typeof r.versions[key], 'number');
      assert.equal(r.versions[key], libVersion);
    });

    it('getFullTextVersions honours since', async () => {
      const none = plain(await c.api.getFullTextVersions('user', user.userID, libVersion));
      assert.deepEqual(none.versions, {});
      const some = plain(await c.api.getFullTextVersions('user', user.userID, libVersion - 1));
      assert.ok(key in some.versions);
    });

    it('getFullTextForItem returns {version, data} with content and counts', async () => {
      const r = plain(await c.api.getFullTextForItem('user', user.userID, key));
      assert.equal(r.version, String(libVersion));
      assert.equal(r.data.content, 'the quick brown fox');
      assert.equal(r.data.indexedChars, 19);
      assert.equal(r.data.totalChars, 19);
    });

    it('stores page-based counts too', async () => {
      const r = await c.api.setFullTextForItems('user', user.userID, libVersion, [
        { key, content: 'page text', indexedPages: 2, totalPages: 3 },
      ]);
      libVersion = Number(r.libraryVersion);
      const got = plain(await c.api.getFullTextForItem('user', user.userID, key));
      assert.equal(got.data.indexedPages, 2);
      assert.equal(got.data.totalPages, 3);
    });

    it('setFullTextForItems with a stale library version throws 412', async () => {
      await assert.rejects(
        c.api.setFullTextForItems('user', user.userID, 0, [{ key, content: 'x', indexedChars: 1, totalChars: 1 }]),
        (e) => e instanceof c.Zotero.HTTP.UnexpectedStatusException && e.status === 412,
      );
    });

    // The desktop's full-text upload step reads `.key` from every entry of both
    // `successful` and `unchanged` to mark items as synced. A bare key string
    // there yields undefined, so the item would never be marked synced and
    // would be re-uploaded on every sync.
    it('reports resent identical content in unchanged as objects carrying a key', async () => {
      const body = { key, content: 'steady content', indexedChars: 14, totalChars: 14 };
      const first = plain(await c.api.setFullTextForItems('user', user.userID, libVersion, [body]));
      libVersion = Number(first.libraryVersion);
      assert.equal(first.results.successful[0].key, key, 'successful entries must carry a key');

      const again = plain(await c.api.setFullTextForItems('user', user.userID, libVersion, [body]));
      assert.equal(again.libraryVersion, String(libVersion), 'nothing changed, so the version must not move');
      assert.equal(again.results.unchanged[0]?.key, key, 'unchanged entries must be objects with a key');
      assert.deepEqual(again.results.failed, {});
    });

    it('full text for an unknown item key reports failure per item or false on read', async () => {
      assert.equal(await c.api.getFullTextForItem('user', user.userID, 'AAAAAAAA'), false);
    });
  });
}
