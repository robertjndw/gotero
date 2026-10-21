import { after, before, describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { clientFor, createGroup, pinnedRefs, plain, provisionUser, skipReason, BASE_URL } from './_support.mjs';

const skip = await skipReason();

for (const ref of await pinnedRefs()) {
  describe(`[${ref}] keys and groups`, { skip }, () => {
    let user, c, group;

    before(async () => {
      user = await provisionUser('keys');
      c = await clientFor(ref, user);
      group = await createGroup(user, `contract-${user.username}`);
    });
    after(() => user?.cleanup());

    it('getKeyInfo returns the key description without the secret', async () => {
      const info = plain(await c.api.getKeyInfo());
      assert.equal(info.userID, user.userID);
      assert.equal(info.username, user.username);
      assert.equal(info.key, undefined, 'client strips key; server must not rely on that');
      assert.equal(info.access.user.library, true);
      assert.equal(info.access.user.write, true);
      assert.equal(info.access.user.files, true);
      assert.ok(info.access.groups.all, 'groups.all must be present or the client treats the key as group-less');
    });

    it('getKeyInfo with includeEmails still parses', async () => {
      const info = plain(await c.api.getKeyInfo({ includeEmails: true }));
      assert.equal(info.userID, user.userID);
    });

    it('getKeyInfo returns false for a revoked or unknown key (403)', async () => {
      const bad = c.makeApiClient({ baseURL: BASE_URL, apiKey: 'x'.repeat(24) });
      assert.equal(await bad.getKeyInfo(), false);
    });

    it('getGroupVersions maps group id to version', async () => {
      const versions = plain(await c.api.getGroupVersions(user.userID));
      assert.ok(String(group.id) in versions, `group ${group.id} missing from ${JSON.stringify(versions)}`);
      assert.equal(typeof versions[group.id], 'number');
    });

    it('getGroup returns data with admins and members arrays', async () => {
      const g = plain(await c.api.getGroup(group.id));
      assert.equal(g.id, group.id);
      assert.equal(g.data.id, group.id);
      assert.equal(typeof g.version, 'number');
      assert.equal(g.data.owner, user.userID);
      assert.ok(Array.isArray(g.data.admins));
      assert.ok(Array.isArray(g.data.members));
      assert.ok(['members', 'admins'].includes(g.data.libraryEditing));
      assert.ok(['all', 'members'].includes(g.data.libraryReading));
    });

    it('getGroup returns false for an unknown group (404)', async () => {
      assert.equal(await c.api.getGroup(2147480000), false);
    });

    // The client walks the Link header unconditionally after each page and
    // throws a TypeError when there is none, so single-page lists need one too.
    it('getGroups pages through the list via the Link header', async () => {
      const groups = plain(await c.api.getGroups(user.userID));
      assert.ok(Array.isArray(groups));
      assert.ok(groups.some((g) => g.id === group.id));
    });
  });
}
