import { suite, assert, assertSameLibrary, userLibrary, itemByKey } from '../lib/harness.mjs';

const lib = async (c) => userLibrary(await c.dump());

// Everything a client logs after this call, to check what one sync did.
const logMark = (c) => {
  const from = c.logText().length;
  return () => c.logText().slice(from);
};

const assertAllSame = async (clients) => {
  const [first, ...rest] = await Promise.all(clients.map(lib));
  for (const other of rest) assertSameLibrary(first, other);
};

suite('three clients', { clients: 3 }, (s) => {
  s.test('edits to different fields of one item merge on every client', async ({ A, B, C }) => {
    const key = await A.createItem({ itemType: 'book', fields: { title: 'base' } });
    await A.syncOk();
    await B.syncOk();
    await C.syncOk();

    await A.editItem(key, { fields: { title: 'title from A' } });
    await B.editItem(key, { addTags: ['tag from B'] });
    await C.editItem(key, { fields: { extra: 'extra from C' } });
    // Each one uploads against a library the others already moved.
    await A.syncOk();
    await B.syncOk();
    await C.syncOk();
    await A.syncOk();
    await B.syncOk();

    for (const c of [A, B, C]) {
      const item = itemByKey(await lib(c), key);
      assert.equal(item.title, 'title from A', c.name);
      assert.equal(item.extra, 'extra from C', c.name);
      assert.deepEqual(item.tags.map((t) => t.tag), ['tag from B'], c.name);
    }
    await assertAllSame([A, B, C]);
  });

  s.test('simultaneous syncs converge', async ({ A, B, C }) => {
    const keys = {};
    for (const c of [A, B, C]) {
      keys[c.name] = await c.createItems(
        Array.from({ length: 15 }, (_, i) => ({ itemType: 'book', fields: { title: `${c.name} ${i}` } })),
      );
    }
    // All three hit the library lock and each other's 412s at once.
    await Promise.all([A.syncOk(), B.syncOk(), C.syncOk()]);
    await Promise.all([A.syncOk(), B.syncOk(), C.syncOk()]);
    await A.syncOk();
    await B.syncOk();
    await C.syncOk();

    const items = (await lib(A)).items;
    for (const [name, ks] of Object.entries(keys)) {
      for (const k of ks) assert.ok(itemByKey({ items }, k), `${name}'s item ${k} reached A`);
    }
    await assertAllSame([A, B, C]);
  });

  // Regression: the server used to give items a new version when a
  // collection they were in was deleted. The deleting client adopts the
  // DELETE's library version without downloading those items, so its next
  // edit of one failed with 412 and forced a full sync.
  s.test('deleting a collection does not force a full sync on the next edit', async ({ A, B, C }) => {
    const coll = await A.createCollection({ name: 'short-lived' });
    const key = await A.createItem({ itemType: 'book', fields: { title: 'in a collection' }, collections: [coll] });
    await A.syncOk();
    await B.syncOk();
    await C.syncOk();

    await A.call('deleteCollection', { keys: [coll] });
    await A.syncOk();
    await A.editItem(key, { fields: { title: 'edited after the delete' } });
    const after = logMark(A);
    await A.syncOk();
    assert.doesNotMatch(after(), /Performing a full sync/, 'the edit went up without a library reset');

    await B.syncOk();
    await C.syncOk();
    for (const c of [A, B, C]) {
      const l = await lib(c);
      assert.equal(l.collections.find((x) => x.key === coll), undefined, c.name);
      const item = itemByKey(l, key);
      assert.equal(item.title, 'edited after the delete', c.name);
      assert.deepEqual(item.collections, [], c.name);
    }
    await assertAllSame([A, B, C]);
  });
});
