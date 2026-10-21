import { suite, assert, eventually, userLibrary, itemByKey } from '../lib/harness.mjs';

suite('stream-triggered sync', { clients: 2 }, (s) => {
  s.test('B autosyncs within 15 s of an edit on A', async ({ A, B }) => {
    const key = await A.createItem({ itemType: 'book', fields: { title: 'stream v1' } });
    await A.syncOk();
    await B.syncOk();

    const mark = B.logText().length;
    await B.setPref('sync.autoSync', true);
    assert.ok(await B.waitForLog(/WebSocket connection opened/, { from: mark, timeoutMs: 20000 }),
      'B should open the stream websocket (is streaming.url pointing at gotero /stream?)');
    // Enabling autosync can itself trigger a sync; let it settle first.
    await new Promise((r) => setTimeout(r, 3000));
    const before = (await B.syncStatus()).finished;

    await A.editItem(key, { fields: { title: 'stream v2' } });
    await A.syncOk();

    await eventually(async () => (await B.syncStatus()).finished > before, {
      timeoutMs: 15000, message: 'B to run a stream-triggered sync',
    });
    const title = itemByKey(userLibrary(await B.dump()), key).title;
    assert.equal(title, 'stream v2');
  });
});
