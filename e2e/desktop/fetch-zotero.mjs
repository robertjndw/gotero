#!/usr/bin/env node
// Downloads an official Zotero Linux build into e2e/desktop/.cache and unpacks
// it. Nothing from Zotero is committed; the cache directory is gitignored.
//
// usage: node fetch-zotero.mjs <version> [--arch amd64|arm64]
// stdout: {"version","arch","platform","dir"}  (dir is what run.sh mounts at /zotero)
//
// We prefer a native build for the host (arm64 on Apple silicon runs without
// emulation) and fall back to x86_64 when no arm64 build exists, e.g. 7.0.x.

import { createWriteStream, existsSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { Readable } from 'node:stream';
import { pipeline } from 'node:stream/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
export const cacheDir = path.join(here, '.cache');

const PLATFORMS = { amd64: 'linux-x86_64', arm64: 'linux-arm64' };

function dl(version, platform) {
  return `https://www.zotero.org/download/client/dl?channel=release&platform=${platform}&version=${version}`;
}

async function buildExists(version, platform) {
  // The dl redirect also points at arm64 files that were never built (7.0.x),
  // so follow it and require the tarball itself to exist.
  const res = await fetch(dl(version, platform), { method: 'HEAD', redirect: 'follow' });
  return res.ok && res.url.includes(`/release/${version}/`);
}

export async function pickArch(version, wanted) {
  const hostArch = process.arch === 'arm64' ? 'arm64' : 'amd64';
  const want = wanted || process.env.E2E_ARCH || hostArch;
  if (await buildExists(version, PLATFORMS[want])) return want;
  if (want !== 'amd64' && (await buildExists(version, PLATFORMS.amd64))) return 'amd64';
  throw new Error(`no Linux build of Zotero ${version} for ${want}`);
}

export async function fetchZotero(version, wantedArch) {
  const arch = await pickArch(version, wantedArch);
  const platform = PLATFORMS[arch];
  const dest = path.join(cacheDir, 'zotero', `${version}-${arch}`);
  const info = { version, arch, platform: `linux/${arch}`, dir: dest };
  if (existsSync(path.join(dest, '.complete'))) return info;

  const tarDir = path.join(cacheDir, 'tarballs');
  mkdirSync(tarDir, { recursive: true });
  const tarball = path.join(tarDir, `Zotero-${version}_${platform}.archive`);
  if (!existsSync(`${tarball}.done`)) {
    console.error(`downloading Zotero ${version} (${platform})`);
    const res = await fetch(dl(version, platform), { redirect: 'follow' });
    if (!res.ok) throw new Error(`download failed: ${res.status}`);
    await pipeline(Readable.fromWeb(res.body), createWriteStream(tarball));
    writeFileSync(`${tarball}.done`, '');
  }

  rmSync(dest, { recursive: true, force: true });
  mkdirSync(dest, { recursive: true });
  // tar sniffs xz vs bz2 itself; the tarball has a single top-level directory
  execFileSync('tar', ['-xf', tarball, '-C', dest, '--strip-components=1'], { stdio: 'inherit' });
  writeFileSync(path.join(dest, '.complete'), '');
  return info;
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const [version] = process.argv.slice(2).filter((a) => !a.startsWith('--'));
  const archIdx = process.argv.indexOf('--arch');
  if (!version) {
    console.error('usage: fetch-zotero.mjs <version> [--arch amd64|arm64]');
    process.exit(2);
  }
  console.log(JSON.stringify(await fetchZotero(version, archIdx > 0 ? process.argv[archIdx + 1] : undefined)));
}
