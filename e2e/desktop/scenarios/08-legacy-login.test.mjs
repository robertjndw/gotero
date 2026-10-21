import { suite, assert, userLibrary } from '../lib/harness.mjs';

// Zotero 7.0 still logs in with username and password via POST /keys.
suite('legacy password login', { clients: 1, login: false, onlyVersions: (v) => v.startsWith('7.') }, (s) => {
  s.test('first sync creates a key from the stored password', async ({ A, user }) => {
    // A non-empty library avoids the "library is empty" confirmation dialog
    // that appears when no user ID is stored yet.
    await A.createItem({ itemType: 'book', fields: { title: 'legacy' } });
    await A.call('setLegacyLogin', { username: user.username, password: user.password });
    await A.syncOk();
    const lib = userLibrary(await A.dump());
    assert.ok(lib.version > 0, 'library version should advance after a successful sync');
    const keys = await user.session.request('GET', '/me/keys');
    assert.ok(keys.items.length >= 2, 'POST /keys should have minted an additional API key');
  });
});
