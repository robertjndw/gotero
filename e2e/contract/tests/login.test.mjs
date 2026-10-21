import { after, before, describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { BASE_URL, clientFor, pinnedRefs, plain, provisionUser, skipReason } from './_support.mjs';

const skip = await skipReason();

for (const ref of await pinnedRefs()) {
  describe(`[${ref}] login flows`, { skip }, () => {
    let user, c;
    before(async () => {
      user = await provisionUser('login');
      c = await clientFor(ref, user);
    });
    after(() => user?.cleanup());

    it('createAPIKeyFromCredentials returns a usable key (legacy login)', async () => {
      const anon = c.makeApiClient({ baseURL: BASE_URL, apiKey: undefined });
      const json = plain(await anon.createAPIKeyFromCredentials(user.username, user.password));
      assert.ok(json.key);
      assert.equal(json.userID, user.userID);
    });

    it('createAPIKeyFromCredentials returns false for bad credentials (403)', async () => {
      const anon = c.makeApiClient({ baseURL: BASE_URL, apiKey: undefined });
      assert.equal(await anon.createAPIKeyFromCredentials(user.username, 'definitely-wrong'), false);
    });

    it('browser login session: create, poll pending, approve, poll completed', async () => {
      const anon = c.makeApiClient({ baseURL: BASE_URL, apiKey: undefined });
      const s = plain(await anon.createLoginSession());
      assert.ok(s.sessionToken);
      assert.match(s.loginURL, /\?/, 'loginURL must already contain a query string');
      assert.deepEqual(plain(await anon.checkLoginSession(s.sessionToken)).status, 'pending');

      await user.session.call('POST', `/login-sessions/${s.sessionToken}/approve`, {});
      const done = plain(await anon.checkLoginSession(s.sessionToken));
      assert.equal(done.status, 'completed');
      assert.equal(done.userID, user.userID);
      assert.ok(done.apiKey);

      const withNewKey = c.makeApiClient({ baseURL: BASE_URL, apiKey: done.apiKey });
      assert.equal(plain(await withNewKey.getKeyInfo()).userID, user.userID);
      await withNewKey.deleteAPIKey();
      assert.equal(await withNewKey.getKeyInfo(), false);
    });

    it('cancelLoginSession ends a pending session', async () => {
      const anon = c.makeApiClient({ baseURL: BASE_URL, apiKey: undefined });
      const s = plain(await anon.createLoginSession());
      await anon.cancelLoginSession(s.sessionToken);
      assert.equal(plain(await anon.checkLoginSession(s.sessionToken)).status, 'cancelled');
    });

    it('checkLoginSession throws for an unknown token (404)', async () => {
      const anon = c.makeApiClient({ baseURL: BASE_URL, apiKey: undefined });
      await assert.rejects(anon.checkLoginSession('nope'), /not found/);
    });
  });
}
