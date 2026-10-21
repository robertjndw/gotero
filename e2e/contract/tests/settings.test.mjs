import { after, before, describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { clientFor, pinnedRefs, plain, provisionUser, skipReason } from './_support.mjs';

const skip = await skipReason();

for (const ref of await pinnedRefs()) {
  describe(`[${ref}] settings`, { skip }, () => {
    let user, c;
    const colors = [{ name: 'important', color: '#FF6666' }];
    let afterUpload;

    before(async () => {
      user = await provisionUser('settings');
      c = await clientFor(ref, user);
    });
    after(() => user?.cleanup());

    it('getSettings on an empty library reports version 0 and no settings', async () => {
      const r = plain(await c.api.getSettings('user', user.userID));
      assert.deepEqual(r, { libraryVersion: '0', settings: {} });
    });

    it('uploadSettings stores a setting and bumps the library version', async () => {
      const r = await c.api.uploadSettings('user', user.userID, 0, {
        tagColors: { value: colors, version: 0 },
      });
      afterUpload = Number(r.libraryVersion);
      assert.ok(afterUpload > 0);
      // 204 means no body; a 200 body is also acceptable to the client.
      assert.ok(r.results === null || typeof r.results === 'object');
    });

    it('getSettings returns what was stored, versioned with the library', async () => {
      const r = plain(await c.api.getSettings('user', user.userID));
      assert.equal(Number(r.libraryVersion), afterUpload);
      assert.deepEqual(r.settings.tagColors.value, colors);
      assert.equal(r.settings.tagColors.version, afterUpload);
    });

    it('getSettings(since = current version) answers 304 and the client returns false', async () => {
      assert.equal(await c.api.getSettings('user', user.userID, afterUpload), false);
      assert.equal(await c.api.getSettings('user', user.userID, afterUpload + 5), false);
    });

    it('getSettings(since < current version) returns the settings again', async () => {
      const r = plain(await c.api.getSettings('user', user.userID, afterUpload - 1));
      assert.ok(r.settings.tagColors);
    });

    it('re-uploading an identical value does not move the library version', async () => {
      const before = (await c.api.getSettings('user', user.userID)).libraryVersion;
      const r = await c.api.uploadSettings('user', user.userID, before, {
        tagColors: { value: colors, version: afterUpload },
      });
      assert.equal(r.libraryVersion, before);
    });

    it('uploadSettings with a stale library version throws a 412 carrying Last-Modified-Version', async () => {
      await assert.rejects(
        c.api.uploadSettings('user', user.userID, 0, { tagColors: { value: [], version: 0 } }),
        (e) => {
          assert.ok(e instanceof c.Zotero.HTTP.UnexpectedStatusException);
          assert.equal(e.status, 412);
          assert.equal(Number(e.xmlhttp.getResponseHeader('Last-Modified-Version')), afterUpload);
          return true;
        },
      );
    });

    it('deleteSettings removes the key and logs it in the deleted list', async () => {
      const v = (await c.api.getSettings('user', user.userID)).libraryVersion;
      const newVersion = await c.api.deleteSettings('user', user.userID, v, ['tagColors']);
      assert.ok(Number(newVersion) > Number(v));
      const after = plain(await c.api.getSettings('user', user.userID));
      assert.deepEqual(after.settings, {});
      const deleted = plain(await c.api.getDeleted('user', user.userID, v));
      assert.deepEqual(deleted.deleted.settings, ['tagColors']);
    });
  });
}
