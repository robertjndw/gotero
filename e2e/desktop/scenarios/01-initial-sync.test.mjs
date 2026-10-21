import { suite, assert, assertSameLibrary, userLibrary, itemByKey } from '../lib/harness.mjs';

suite('initial upload and fresh download', { clients: 2 }, (s) => {
  s.test('B gets an identical library after A uploads', async ({ A, B }) => {
    const top = await A.createCollection({ name: 'Top' });
    const child = await A.createCollection({ name: 'Child', parentKey: top });
    const book = await A.createItem({
      itemType: 'book',
      fields: { title: 'A Book', date: '2020', publisher: 'Pub' },
      creators: [{ creatorType: 'author', firstName: 'Ada', lastName: 'Lovelace' }],
      tags: ['alpha', { tag: 'auto', type: 1 }],
      collections: [child],
    });
    const article = await A.createItem({
      itemType: 'journalArticle', fields: { title: 'An Article', publicationTitle: 'J' }, tags: ['beta'],
    });
    await A.createItem({ itemType: 'note', note: '<p>child note</p>', parentItem: book });
    await A.createItem({ itemType: 'note', note: '<p>standalone</p>' });

    await A.syncOk();
    await B.syncOk();

    const a = userLibrary(await A.dump());
    const b = userLibrary(await B.dump());
    assert.equal(b.items.length, 4);
    assert.equal(b.collections.length, 2);
    assert.equal(itemByKey(b, article).title, 'An Article');
    assertSameLibrary(a, b);
  });

  s.test('a second sync without changes is a no-op', async ({ A }) => {
    const before = userLibrary(await A.dump()).version;
    await A.syncOk();
    assert.equal(userLibrary(await A.dump()).version, before);
  });
});
