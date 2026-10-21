#!/usr/bin/env node
// Downloads Zotero's own sync client files at a pinned tag into .cache/<ref>/.
//
// Those files are AGPL, so they are only ever fetched at test time into a
// gitignored directory. Nothing here (or anywhere else in this repo) may copy
// them into tracked files.
//
// Usage: node fetch.mjs [ref ...]   (no args = every ref in zotero-ref.json)
import { createHash } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
export const cacheRoot = join(here, '.cache');

export async function loadRefs() {
  return JSON.parse(await readFile(join(here, 'zotero-ref.json'), 'utf8'));
}

const sha256 = (buf) => createHash('sha256').update(buf).digest('hex');

// Returns the directory holding the verified files for `ref`. A cached copy is
// re-hashed every time, so a tampered or stale cache is refetched rather than
// trusted.
export async function ensureRef(ref) {
  const cfg = await loadRefs();
  const entry = cfg.refs[ref];
  if (!entry) throw new Error(`unknown ref ${ref}; known: ${Object.keys(cfg.refs).join(', ')}`);
  const dir = join(cacheRoot, ref);
  for (const file of cfg.files) {
    const dest = join(dir, file);
    const want = entry.sha256[file];
    let buf = await readFile(dest).catch(() => null);
    if (buf && sha256(buf) !== want) buf = null;
    if (!buf) {
      const url = `https://raw.githubusercontent.com/${cfg.repo}/${ref}/${cfg.baseDir}${file}`;
      const res = await fetch(url);
      if (!res.ok) throw new Error(`GET ${url} -> ${res.status}`);
      buf = Buffer.from(await res.arrayBuffer());
      const got = sha256(buf);
      if (got !== want) {
        throw new Error(
          `sha256 mismatch for ${ref}:${file}\n  expected ${want}\n  got      ${got}\n` +
            'upstream may have moved the tag; review the change before updating zotero-ref.json',
        );
      }
      await mkdir(dirname(dest), { recursive: true });
      await writeFile(dest, buf);
    }
  }
  return dir;
}

// Helper for bumping refs: prints the hashes to paste into zotero-ref.json.
export async function hashesFor(ref) {
  const cfg = await loadRefs();
  const out = {};
  for (const file of cfg.files) {
    const url = `https://raw.githubusercontent.com/${cfg.repo}/${ref}/${cfg.baseDir}${file}`;
    const res = await fetch(url);
    if (!res.ok) throw new Error(`GET ${url} -> ${res.status}`);
    out[file] = sha256(Buffer.from(await res.arrayBuffer()));
  }
  return out;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const args = process.argv.slice(2);
  if (args[0] === '--hash') {
    console.log(JSON.stringify(await hashesFor(args[1]), null, 2));
  } else {
    const refs = args.length ? args : Object.keys((await loadRefs()).refs);
    for (const ref of refs) console.log(`${ref}: ${await ensureRef(ref)}`);
  }
}
