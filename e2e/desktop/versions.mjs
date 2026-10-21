#!/usr/bin/env node
// Resolves which Zotero desktop builds the e2e matrix should run against.
//
// Endpoints relied on (all official, no auth):
//   - https://www.zotero.org/download/client/dl?channel=release&platform=linux-x86_64
//     302s to the newest stable tarball; the version is parsed out of the
//     Location header. This is what the zotero.org download page itself uses,
//     so it is the closest thing to "current stable" we can get.
//   - The same URL with &version=X answers 302 for a published build and an
//     error otherwise. We use it to confirm a candidate really has a Linux
//     build, because a git tag alone does not guarantee that (7.0.26 has none).
//   - https://api.github.com/repos/zotero/zotero/git/matching-refs/tags/<major>.
//     lists candidate patch releases of an older major. GITHUB_TOKEN is used if
//     set. When the API is unavailable we fall back to probing the dl endpoint
//     patch by patch.
//
// Output: JSON on stdout, e.g.
//   {"include":[{"label":"current","version":"10.0.6"},{"label":"previous","version":"9.0.6"}, ...]}
// Flags: --legacy adds the newest 7.0.x (legacy POST /keys login check).
//        --versions-only prints just a JSON array of version strings.

const DL = 'https://www.zotero.org/download/client/dl';
const UA = 'gotero-e2e-versions';

function dlUrl(version, platform = 'linux-x86_64') {
  const u = new URL(DL);
  u.searchParams.set('channel', 'release');
  u.searchParams.set('platform', platform);
  if (version) u.searchParams.set('version', version);
  return u.toString();
}

async function redirectTarget(url) {
  const res = await fetch(url, { method: 'HEAD', redirect: 'manual', headers: { 'user-agent': UA } });
  if (res.status >= 300 && res.status < 400) return res.headers.get('location');
  return null;
}

function versionFromLocation(loc) {
  const m = /\/release\/(\d+\.\d+(?:\.\d+)*)\//.exec(loc ?? '');
  return m ? m[1] : null;
}

async function hasLinuxBuild(version) {
  return versionFromLocation(await redirectTarget(dlUrl(version))) === version;
}

function cmp(a, b) {
  const pa = a.split('.').map(Number);
  const pb = b.split('.').map(Number);
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const d = (pa[i] ?? 0) - (pb[i] ?? 0);
    if (d) return d;
  }
  return 0;
}

async function candidatesFromGitHub(prefix) {
  const headers = { 'user-agent': UA, accept: 'application/vnd.github+json' };
  if (process.env.GITHUB_TOKEN) headers.authorization = `Bearer ${process.env.GITHUB_TOKEN}`;
  const res = await fetch(`https://api.github.com/repos/zotero/zotero/git/matching-refs/tags/${prefix}.`, { headers });
  if (!res.ok) throw new Error(`github ${res.status}`);
  const refs = await res.json();
  return refs
    .map((r) => r.ref.replace('refs/tags/', ''))
    .filter((t) => /^\d+\.\d+\.\d+$/.test(t) && t.startsWith(`${prefix}.`))
    .sort(cmp)
    .reverse();
}

// Tags have gaps, so tolerate a few consecutive misses before giving up.
async function candidatesByProbing(prefix) {
  const found = [];
  let misses = 0;
  for (let patch = 0; misses < 6 && patch < 200; patch++) {
    const v = `${prefix}.${patch}`;
    if (await hasLinuxBuild(v)) {
      found.push(v);
      misses = 0;
    } else {
      misses++;
    }
  }
  return found.reverse();
}

async function newestWithBuild(prefix) {
  let list;
  try {
    list = await candidatesFromGitHub(prefix);
  } catch {
    list = await candidatesByProbing(prefix);
  }
  for (const v of list) if (await hasLinuxBuild(v)) return v;
  throw new Error(`no downloadable Linux build found for ${prefix}.x`);
}

export async function resolve({ legacy = false } = {}) {
  const current = versionFromLocation(await redirectTarget(dlUrl()));
  if (!current) throw new Error('could not resolve the current stable Zotero version');
  const major = Number(current.split('.')[0]);
  const include = [{ label: 'current', version: current }];
  include.push({ label: 'previous', version: await newestWithBuild(String(major - 1)) });
  if (legacy) include.push({ label: 'legacy', version: await newestWithBuild('7.0') });
  return { include };
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const args = new Set(process.argv.slice(2));
  const out = await resolve({ legacy: args.has('--legacy') });
  console.log(JSON.stringify(args.has('--versions-only') ? out.include.map((i) => i.version) : out));
}
