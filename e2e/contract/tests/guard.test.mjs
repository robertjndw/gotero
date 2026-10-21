// Licensing guard: Zotero's client files are AGPL and must only ever live in
// the gitignored cache. Fails if git tracks anything there, or if anything
// outside the cache looks like a copy of a pinned file.
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, '..');

function git(...args) {
  try {
    return execFileSync('git', args, { cwd: root, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
  } catch (e) {
    return null;
  }
}

describe('AGPL cache guard', () => {
  it('no file under e2e/contract/.cache is tracked by git', (t) => {
    const out = git('ls-files', '--', '.cache');
    if (out === null) return t.skip('not inside a git work tree');
    assert.equal(out.trim(), '', `tracked files under .cache:\n${out}`);
  });

  it('.cache is ignored by git', (t) => {
    const probe = git('check-ignore', '-q', '--', '.cache/probe');
    if (probe === null && git('rev-parse', '--is-inside-work-tree') === null) return t.skip('not inside a git work tree');
    assert.notEqual(probe, null, '.cache/ must be listed in .gitignore');
  });

  it('no tracked file is byte-identical to a pinned Zotero file', (t) => {
    const tracked = git('ls-files', '-z', '--', '.');
    if (tracked === null) return t.skip('not inside a git work tree');
    const cfg = JSON.parse(readFileSync(join(root, 'zotero-ref.json'), 'utf8'));
    const banned = new Set(Object.values(cfg.refs).flatMap((r) => Object.values(r.sha256)));
    for (const f of tracked.split('\0').filter(Boolean)) {
      const h = createHash('sha256').update(readFileSync(join(root, f))).digest('hex');
      assert.ok(!banned.has(h), `${f} is a verbatim copy of a Zotero source file`);
    }
  });
});
