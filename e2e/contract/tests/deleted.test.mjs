// How the client reacts to a 409 from GET deleted?since=V, which is gotero's way
// of saying "your since is older than the pruned part of the delete log".
//
// The API client turns that 409 into a `false` return value. What the sync
// engine does with `false` is outside what this suite executes (see the note at
// the bottom), so only the API-client half is asserted here.
//
// Forcing a 409 needs a raised delete_log_floor, which only the prune job sets
// and which has no API. Point GOTERO_FLOOR_SQL_CMD at a command that runs one SQL
// statement against gotero's database, for example:
//   GOTERO_FLOOR_SQL_CMD='docker compose -p gotero-a exec -T postgres psql -U gotero -d gotero -c'
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { after, before, describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { clientFor, collection, pinnedRefs, plain, provisionUser, skipReason } from './_support.mjs';

const run = promisify(execFile);
const sqlCmd = (process.env.GOTERO_FLOOR_SQL_CMD || '').split(/\s+/).filter(Boolean);
const skip = (await skipReason()) || (sqlCmd.length ? false : 'GOTERO_FLOOR_SQL_CMD is not set, cannot raise the delete log floor');

const sql = async (statement) => {
  const [cmd, ...args] = sqlCmd;
  await run(cmd, [...args, statement]);
};

for (const ref of await pinnedRefs()) {
  describe(`[${ref}] getDeleted below the delete log floor`, { skip }, () => {
    let user, c, libVersion;

    before(async () => {
      user = await provisionUser('deleted');
      c = await clientFor(ref, user);
      const col = collection('doomed');
      const created = await c.api.uploadObjects('user', user.userID, 'POST', 0, 'collection', [col]);
      libVersion = Number(
        await c.api.uploadDeletions('user', user.userID, Number(created.libraryVersion), 'collection', [col.key]),
      );
      // Pretend the prune job dropped log entries up to the latest deletion.
      await sql(
        `UPDATE libraries SET delete_log_floor = ${libVersion} WHERE id = (SELECT library_id FROM users WHERE id = ${user.userID})`,
      );
    });
    after(() => user?.cleanup());

    it('returns false for a since below the floor', async () => {
      assert.equal(await c.api.getDeleted('user', user.userID, 1), false);
    });

    it('still answers since = 0 (full sync) and since at the floor normally', async () => {
      const full = plain(await c.api.getDeleted('user', user.userID, 0));
      assert.equal(full.libraryVersion, String(libVersion));
      const atFloor = plain(await c.api.getDeleted('user', user.userID, libVersion));
      assert.deepEqual(atFloor.deleted.collections, []);
    });

    // The sync engine's deletion step (syncEngine.js, not fetched by this suite)
    // does not check for false: it reads libraryVersion off the result and
    // treats the mismatch as a remote change, restarting the download with the
    // same since until its retry delays run out. It never falls back to a full
    // sync, so a 409 here strands the client rather than resyncing it.
  });
}
