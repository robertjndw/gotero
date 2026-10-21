// Needs no gotero server: proves the harness can launch a real Zotero, drive
// it through the add-on, and that the api.url pref is honoured (a sync against
// a dead URL must fail cleanly and the log must show the request going there).
import { test, describe, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { ZoteroClient } from '../lib/client.mjs';
import { fetchZotero } from '../fetch-zotero.mjs';
import { resolve } from '../versions.mjs';

const DUMMY = 'http://host.docker.internal:9/api/';
const version = process.env.ZOTERO_VERSION || (await resolve()).include[0].version;
const zotero = await fetchZotero(version);

describe(`smoke: Zotero ${version} (${zotero.arch})`, () => {
  let client;
  before(async () => {
    client = new ZoteroClient({
      name: 'smoke', zotero, apiURL: DUMMY, streamURL: 'ws://host.docker.internal:9/stream',
    });
    await client.start();
  });
  after(async () => { await client?.stop(); });

  test('ping reports version and the configured api.url', async () => {
    const info = await client.ping();
    console.log('ping:', JSON.stringify(info));
    assert.ok(info.zoteroVersion.startsWith(version.split('.')[0]));
    assert.equal(info.apiURL, DUMMY);
  });

  test('items can be created and dumped', async () => {
    const key = await client.createItem({
      itemType: 'book', fields: { title: 'Smoke test' }, tags: ['t1'],
    });
    const lib = (await client.dump()).libraries.find((l) => l.type === 'user');
    assert.ok(lib.items.some((i) => i.key === key && i.title === 'Smoke test'));
  });

  test('local commands behave (no server needed)', async () => {
    const lib = async () => (await client.dump()).libraries.find((l) => l.type === 'user');
    const col = await client.createCollection({ name: 'Root' });
    const child = await client.createCollection({ name: 'Child', parentKey: col });
    const book = await client.createItem({
      itemType: 'book', fields: { title: 'B' }, tags: ['old', 'gone'], collections: [child],
    });
    await client.createItem({ itemType: 'note', note: '<p>n</p>', parentItem: book });
    await client.editItem(book, { fields: { title: 'B2' }, addTags: ['extra'] });
    await client.call('renameTag', { oldName: 'old', newName: 'new' });
    await client.call('deleteTag', { name: 'gone' });
    await client.call('moveCollection', { key: child, parentKey: false });

    const att = await client.attach({ text: 'hello', filename: 'h.txt', parentKey: book, contentType: 'text/plain' });
    assert.equal(att.file.md5, '5d41402abc4b2a76b9719d911017c592');
    const mod = await client.modifyFile(att.key, { text: 'hello2' });
    assert.notEqual(mod.file.md5, att.file.md5);

    let l = await lib();
    const item = l.items.find((i) => i.key === book);
    assert.equal(item.title, 'B2');
    assert.deepEqual(item.tags.map((t) => t.tag).sort(), ['extra', 'new']);
    assert.equal(l.collections.find((c) => c.key === child).parentCollection, false);

    await client.trashItems([book]);
    assert.equal((await lib()).items.find((i) => i.key === book).deleted, true);
    await client.restoreItems([book]);
    await client.trashItems([book]);
    await client.emptyTrash();
    assert.equal((await lib()).items.find((i) => i.key === book), undefined);

    assert.deepEqual((await client.setConflictResolution('remote')).mode, 'remote');
    assert.deepEqual(await client.conflictLog(), []);
  });

  test('sync against a dead server fails cleanly and targets api.url', async () => {
    await client.setAPIKey({ key: 'x'.repeat(24), userID: 1, username: 'smoke' });
    const r = await client.sync({ timeoutMs: 90000 });
    console.log('sync result:', JSON.stringify(r));
    assert.equal(r.ok, false);
    assert.ok(r.errors.length > 0);
    const log = readFileSync(client.logPath, 'utf8');
    const lines = log.split('\n').filter((l) => /^zotero\(\d\)\(.*HTTP \w+ http:\/\/host\.docker\.internal:9\/api\//.test(l));
    console.log('log excerpt:\n' + lines.slice(0, 3).join('\n'));
    assert.ok(lines.length > 0, 'debug log should mention the configured api.url');
  });
});
