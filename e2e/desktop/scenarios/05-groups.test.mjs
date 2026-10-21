import { suite, assert, assertSameLibrary, groupLibrary, itemByKey } from '../lib/harness.mjs';

suite('group library', { clients: 2, sameUser: false }, (s) => {
  s.test('items created by one member reach the other', async ({ A, B, users, server }) => {
    const group = await server.createGroup(users, { name: 'e2e group' });
    // Group membership shows up on the next sync, which also creates the library locally.
    await A.syncOk();
    await B.syncOk();

    const libA = (await A.libraries()).find((l) => l.groupID === group.id);
    assert.ok(libA, 'A should know the group library after syncing');
    const key = await A.createItem(
      { itemType: 'book', fields: { title: 'shared' }, tags: ['g'] },
      { libraryID: libA.libraryID },
    );
    await A.syncOk();
    await B.syncOk();

    const a = groupLibrary(await A.dump(), group.id);
    const b = groupLibrary(await B.dump(), group.id);
    assert.equal(itemByKey(b, key).title, 'shared');
    assertSameLibrary(a, b);

    // and back the other way
    const libB = (await B.libraries()).find((l) => l.groupID === group.id);
    const key2 = await B.createItem({ itemType: 'book', fields: { title: 'reply' } }, { libraryID: libB.libraryID });
    await B.syncOk();
    await A.syncOk();
    assert.equal(itemByKey(groupLibrary(await A.dump(), group.id), key2).title, 'reply');
  });
});
