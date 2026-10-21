import { suite, assert } from '../lib/harness.mjs';

suite('revoked key', { clients: 1 }, (s) => {
  s.test('sync fails cleanly once the key is revoked', async ({ A, user }) => {
    await A.createItem({ itemType: 'book', fields: { title: 'before revoke' } });
    await A.syncOk();

    await user.revokeKey();
    const r = await A.sync();
    assert.equal(r.ok, false);
    // A revoked key answers 403 on keys/current; the client reports an invalid key.
    const text = JSON.stringify(r.errors);
    assert.match(text, /403|invalid|key|authoriz/i);
  });
});

suite('quota exceeded', { clients: 1, quotaBytes: 100 * 1024 }, (s) => {
  s.test('uploading past the quota reports an error and keeps the data', async ({ A }) => {
    const big = Buffer.alloc(400 * 1024, 7);
    const { key } = await A.attach({ base64: big.toString('base64'), filename: 'big.bin', contentType: 'application/octet-stream' });
    const r = await A.sync();
    assert.equal(r.ok, false, 'file sync should fail when over quota');
    assert.match(JSON.stringify(r.errors), /quota|413|storage|space/i);
    // The item metadata itself is still intact locally
    const lib = (await A.dump()).libraries.find((l) => l.type === 'user');
    assert.ok(lib.items.some((i) => i.key === key));
  });
});
