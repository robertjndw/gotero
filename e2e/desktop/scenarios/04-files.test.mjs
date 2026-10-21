import { suite, assert, userLibrary, itemByKey } from '../lib/harness.mjs';
import { createHash, randomBytes } from 'node:crypto';

const md5 = (buf) => createHash('md5').update(buf).digest('hex');
const fileOf = async (c, key) => itemByKey(userLibrary(await c.dump()), key).file;

suite('file attachments', { clients: 2 }, (s) => {
  s.test('upload on A, download on B with matching md5', async ({ A, B }) => {
    const bytes = randomBytes(200 * 1024);
    const parent = await A.createItem({ itemType: 'book', fields: { title: 'with file' } });
    const { key, file } = await A.attach({
      base64: bytes.toString('base64'), filename: 'blob.bin', parentKey: parent, contentType: 'application/octet-stream',
    });
    assert.equal(file.md5, md5(bytes));

    await A.syncOk();
    await B.syncOk();
    const onB = await fileOf(B, key);
    assert.equal(onB.exists, true);
    assert.equal(onB.md5, md5(bytes));
  });

  s.test('file modified on one side propagates', async ({ A, B }) => {
    const { key } = await A.attach({ text: 'version one', filename: 'note.txt', contentType: 'text/plain' });
    await A.syncOk();
    await B.syncOk();

    await A.modifyFile(key, { text: 'version two, edited on A' });
    await A.syncOk();
    await B.syncOk();
    assert.equal((await fileOf(B, key)).md5, md5('version two, edited on A'));
  });

  for (const mode of ['local', 'remote']) {
    s.test(`file modified on both sides, ${mode} wins`, async ({ A, B }) => {
      const { key } = await A.attach({ text: `base ${mode}`, filename: `c-${mode}.txt`, contentType: 'text/plain' });
      await A.syncOk();
      await B.syncOk();

      await A.modifyFile(key, { text: `A edit ${mode}` });
      await A.syncOk();
      await B.modifyFile(key, { text: `B edit ${mode}`, mtimeMs: Date.now() + 6000 });
      await B.setConflictResolution(mode);
      await B.syncOk();
      await A.syncOk();
      await B.syncOk();

      const winner = md5(mode === 'local' ? `B edit ${mode}` : `A edit ${mode}`);
      assert.equal((await fileOf(A, key)).md5, winner);
      assert.equal((await fileOf(B, key)).md5, winner);
      const log = await B.conflictLog(true);
      assert.ok(log.some((l) => l.kind === 'file'), 'expected a file conflict on B');
    }, {
      // Still unexplained: on B the second sync reports "mod time matches remote"
      // and neither uploads nor raises a file conflict, so the edit made on B is
      // never registered as a local change. Needs a look at how 10.x tracks
      // local file edits (FileChangeWatcher) inside a container.
      todo: 'file conflict is not triggered by the add-on-driven edit yet',
    });
  }
});
