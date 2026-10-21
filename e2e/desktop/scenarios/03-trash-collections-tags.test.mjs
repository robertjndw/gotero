import { suite, assert, assertSameLibrary, userLibrary, itemByKey } from '../lib/harness.mjs';

const lib = async (c) => userLibrary(await c.dump());

suite('trash, collections and tags', { clients: 2 }, (s) => {
  s.test('trash, restore and empty trash propagate', async ({ A, B }) => {
    const [k1, k2] = await A.createItems([
      { itemType: 'book', fields: { title: 'one' } },
      { itemType: 'book', fields: { title: 'two' } },
    ]);
    await A.syncOk();
    await B.syncOk();

    await A.trashItems([k1, k2]);
    await A.syncOk();
    await B.syncOk();
    assert.equal(itemByKey(await lib(B), k1).deleted, true);

    await A.restoreItems([k1]);
    await A.syncOk();
    await B.syncOk();
    assert.equal(itemByKey(await lib(B), k1).deleted, false);
    assert.equal(itemByKey(await lib(B), k2).deleted, true);

    await A.emptyTrash();
    await A.syncOk();
    await B.syncOk();
    assert.equal(itemByKey(await lib(B), k2), undefined);
    assertSameLibrary(await lib(A), await lib(B));
  });

  s.test('tag rename and delete', async ({ A, B }) => {
    const key = await A.createItem({ itemType: 'book', fields: { title: 'tagged' }, tags: ['old', 'doomed'] });
    await A.syncOk();
    await B.syncOk();

    await A.call('renameTag', { oldName: 'old', newName: 'new' });
    await A.call('deleteTag', { name: 'doomed' });
    await A.syncOk();
    await B.syncOk();
    const tags = itemByKey(await lib(B), key).tags.map((t) => t.tag).sort();
    assert.deepEqual(tags, ['new']);
  });
  s.test('collection tree moves and deletes', async ({ A, B }) => {
    const root1 = await A.createCollection({ name: 'Root 1' });
    const root2 = await A.createCollection({ name: 'Root 2' });
    const leaf = await A.createCollection({ name: 'Leaf', parentKey: root1 });
    await A.syncOk();
    await B.syncOk();

    await A.call('moveCollection', { key: leaf, parentKey: root2 });
    await A.call('renameCollection', { key: root1, name: 'Root 1 renamed' });
    await A.syncOk();
    await B.syncOk();
    const moved = (await lib(B)).collections.find((c) => c.key === leaf);
    assert.equal(moved.parentCollection, root2);

    await A.call('deleteCollection', { keys: [leaf] });
    await A.syncOk();
    await B.syncOk();
    assert.equal((await lib(B)).collections.some((c) => c.key === leaf), false);
  });

});

// Separate suite so a failing collection trash cannot leave unsynced state that
// breaks the other scenarios.
suite('collection trash', { clients: 2 }, (s) => {
  s.test('trashed collection propagates', async ({ A, B }) => {
    const root = await A.createCollection({ name: 'To trash' });
    await A.syncOk();
    await B.syncOk();
    await A.call('trashCollection', { keys: [root] });
    await A.syncOk();
    await B.syncOk();
    assert.equal((await lib(B)).collections.find((c) => c.key === root).deleted, true);
  });
});
