import { suite, assert, assertSameLibrary, userLibrary, itemByKey } from '../lib/harness.mjs';

const title = async (c, key) => itemByKey(userLibrary(await c.dump()), key).title;

suite('edits and conflicts', { clients: 2 }, (s) => {
  s.test('an edit on A reaches B', async ({ A, B }) => {
    const key = await A.createItem({ itemType: 'book', fields: { title: 'v1' } });
    await A.syncOk();
    await B.syncOk();
    await A.editItem(key, { fields: { title: 'v2' }, addTags: ['edited'] });
    await A.syncOk();
    await B.syncOk();
    assert.equal(await title(B, key), 'v2');
    assertSameLibrary(userLibrary(await A.dump()), userLibrary(await B.dump()));
  });

  s.test('concurrent edit, local wins', async ({ A, B }) => {
    const key = await A.createItem({ itemType: 'book', fields: { title: 'base' } });
    await A.syncOk();
    await B.syncOk();

    await A.editItem(key, { fields: { title: 'from A' } });
    await A.syncOk();
    await B.editItem(key, { fields: { title: 'from B' } });
    await B.setConflictResolution('local');
    await B.syncOk();
    await A.syncOk();

    const log = await B.conflictLog(true);
    assert.equal(log.filter((l) => l.kind === 'object').length, 1, 'B should have hit exactly one conflict');
    assert.equal(await title(B, key), 'from B');
    assert.equal(await title(A, key), 'from B');
  });

  s.test('concurrent edit, remote wins', async ({ A, B }) => {
    const key = await A.createItem({ itemType: 'book', fields: { title: 'base2' } });
    await A.syncOk();
    await B.syncOk();

    await A.editItem(key, { fields: { title: 'A wins' } });
    await A.syncOk();
    await B.editItem(key, { fields: { title: 'B loses' } });
    await B.setConflictResolution('remote');
    await B.syncOk();
    await A.syncOk();

    assert.equal(await title(B, key), 'A wins');
    assert.equal(await title(A, key), 'A wins');
  });

  s.test('deleted on A, modified on B, keep the modification', async ({ A, B }) => {
    const key = await A.createItem({ itemType: 'book', fields: { title: 'doomed' } });
    await A.syncOk();
    await B.syncOk();

    await A.deleteItems([key]);
    await A.syncOk();
    await B.editItem(key, { fields: { title: 'rescued' } });
    await B.setConflictResolution('local');
    await B.syncOk();
    await A.syncOk();

    assert.equal(await title(A, key), 'rescued');
  });

  s.test('deleted on A, modified on B, accept the deletion', async ({ A, B }) => {
    const key = await A.createItem({ itemType: 'book', fields: { title: 'doomed too' } });
    await A.syncOk();
    await B.syncOk();

    await A.deleteItems([key]);
    await A.syncOk();
    await B.editItem(key, { fields: { title: 'late edit' } });
    await B.setConflictResolution('remote');
    await B.syncOk();

    const lib = userLibrary(await B.dump());
    assert.equal(itemByKey(lib, key), undefined);
  });
});
