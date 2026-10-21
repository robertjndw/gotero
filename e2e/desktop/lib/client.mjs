// One real Zotero desktop client in a Docker container, controlled over the
// add-on's /gotero-test/ endpoints.

import { execFileSync } from 'node:child_process';
import { mkdirSync, readFileSync, existsSync, cpSync, rmSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { createProfile } from './profile.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
// One image per CPU architecture, built by run.sh (docker build --platform).
export const imageFor = (arch) => process.env.E2E_IMAGE || `gotero-e2e-runner:${arch}`;
const RUN_ID = process.env.E2E_RUN_ID || `${Date.now().toString(36)}-${process.pid}`;
export const RUNS_DIR = process.env.E2E_RUNS_DIR || path.join(here, '..', '.cache', 'run', RUN_ID);

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function docker(args, opts = {}) {
  return execFileSync('docker', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], ...opts }).trim();
}

export class ZoteroClient {
  /**
   * @param {object} o
   * @param {string} o.name         label used for the container and artifact folder
   * @param {object} o.zotero       result of fetchZotero()
   * @param {string} o.apiURL       api.url as seen from inside the container
   * @param {string} o.streamURL
   * @param {number} [o.index]      picks a distinct connector port per client
   */
  constructor({ name, zotero, apiURL, streamURL, index = 0, extraPrefs, scope = '' }) {
    this.name = name;
    this.zotero = zotero;
    this.apiURL = apiURL;
    this.streamURL = streamURL;
    // Ports differ per client even though each lives in its own container, so
    // a mix-up between clients shows up as a failed ping instead of silently
    // controlling the wrong one.
    this.port = 23119 + index * 10;
    this.bridgePort = 33119 + index * 10;
    // Every suite logs in as a fresh user, so it needs a fresh profile too.
    // A reused profile keeps the previous user's library version, asks the
    // server for changes "since" a version the new library never reached,
    // and silently downloads nothing.
    const id = scope ? `${scope}-${name}` : name;
    this.workDir = path.join(RUNS_DIR, id);
    this.container = `gotero-e2e-${RUN_ID}-${id}`.replace(/[^a-zA-Z0-9_.-]/g, '-').slice(0, 120);
    this.extraPrefs = extraPrefs;
    this.hostPort = null;
  }

  get logPath() { return path.join(this.workDir, 'zotero.log'); }

  async start({ timeoutMs = Number(process.env.E2E_START_TIMEOUT_MS) || 240000 } = {}) {
    rmSync(this.workDir, { recursive: true, force: true });
    mkdirSync(this.workDir, { recursive: true });
    createProfile(this.workDir, {
      apiURL: this.apiURL, streamURL: this.streamURL, port: this.port, extraPrefs: this.extraPrefs,
    });
    const uid = typeof process.getuid === 'function';
    const args = [
      'run', '-d', '--name', this.container,
      '--platform', this.zotero.platform,
      ...(uid ? ['-e', `E2E_UID=${process.getuid()}`, '-e', `E2E_GID=${process.getgid()}`] : []),
      // Lets the client reach a gotero server published on the host
      '--add-host', 'host.docker.internal:host-gateway',
      '-p', `127.0.0.1::${this.bridgePort}`,
      '-e', `ZOTERO_PORT=${this.port}`, '-e', `BRIDGE_PORT=${this.bridgePort}`,
      '-v', `${this.workDir}:/work`,
      '-v', `${this.zotero.dir}:/zotero:ro`,
      '--shm-size=1g',
      imageFor(this.zotero.arch),
    ];
    docker(args);
    const mapping = docker(['port', this.container, `${this.bridgePort}/tcp`]).split('\n')[0];
    this.hostPort = Number(mapping.split(':').pop());

    const deadline = Date.now() + timeoutMs;
    let lastErr;
    while (Date.now() < deadline) {
      if (!this.running()) {
        throw new Error(`container for ${this.name} exited during startup\n${this.logTail(60)}`);
      }
      try {
        this.info = await this.call('ping', {}, { timeoutMs: 4000 });
        return this.info;
      }
      catch (e) { lastErr = e; }
      await sleep(1500);
    }
    throw new Error(`${this.name}: no ping within ${timeoutMs} ms (${lastErr?.message})\n${this.logTail(60)}`);
  }

  running() {
    try { return docker(['inspect', '-f', '{{.State.Running}}', this.container]) === 'true'; }
    catch { return false; }
  }

  logTail(n = 40) {
    if (!existsSync(this.logPath)) return '(no zotero.log)';
    return readFileSync(this.logPath, 'utf8').split('\n').slice(-n).join('\n');
  }

  async call(command, args = {}, { timeoutMs = 180000 } = {}) {
    const res = await fetch(`http://127.0.0.1:${this.hostPort}/gotero-test/${command}`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(args),
      signal: AbortSignal.timeout(timeoutMs),
    });
    const text = await res.text();
    let body;
    try { body = JSON.parse(text); } catch { body = { raw: text }; }
    if (!res.ok) {
      const err = new Error(`${this.name}.${command} failed: ${res.status} ${body?.error?.message ?? text}`);
      err.detail = body;
      throw err;
    }
    return body;
  }

  // Convenience wrappers used by scenarios
  ping() { return this.call('ping'); }
  setAPIKey(o) { return this.call('setAPIKey', o); }
  setPref(name, value, global = false) { return this.call('setPref', { name, value, global }); }
  createCollection(o) { return this.call('createCollection', o).then((r) => r.key); }
  createItem(item, o = {}) { return this.call('createItem', { item, ...o }).then((r) => r.key); }
  createItems(items, o = {}) { return this.call('createItems', { items, ...o }).then((r) => r.keys); }
  editItem(key, patch, o = {}) { return this.call('editItem', { key, ...patch, ...o }); }
  trashItems(keys, o = {}) { return this.call('trashItem', { keys, ...o }); }
  restoreItems(keys, o = {}) { return this.call('restoreItem', { keys, ...o }); }
  deleteItems(keys, o = {}) { return this.call('deleteItem', { keys, ...o }); }
  emptyTrash(o = {}) { return this.call('emptyTrash', o); }
  attach(o) { return this.call('importFileAttachment', o); }
  modifyFile(key, o) { return this.call('modifyFile', { key, ...o }); }
  setConflictResolution(mode) { return this.call('setConflictResolution', { mode }); }
  conflictLog(clear = false) { return this.call('conflictLog', { clear }).then((r) => r.log); }
  libraries() { return this.call('libraries').then((r) => r.libraries); }
  dump(o = {}) { return this.call('dump', o); }
  sync(o = {}) { return this.call('sync', o); }
  syncStatus() { return this.call('syncStatus'); }

  logText() { return existsSync(this.logPath) ? readFileSync(this.logPath, 'utf8') : ''; }

  async waitForLog(re, { timeoutMs = 20000, from = 0 } = {}) {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      if (re.test(this.logText().slice(from))) return true;
      await sleep(300);
    }
    return false;
  }

  /** Runs a sync and throws with the collected errors if it was not clean. */
  async syncOk(o = {}) {
    const r = await this.sync(o);
    if (!r.ok) throw new Error(`${this.name} sync failed: ${JSON.stringify(r.errors)}`);
    return r;
  }

  /** Copies logs, a state dump and container info into `dir`. Never throws. */
  async collectArtifacts(dir) {
    const out = path.join(dir, this.name);
    mkdirSync(out, { recursive: true });
    const safe = async (fn) => { try { await fn(); } catch { /* best effort */ } };
    await safe(() => cpSync(this.logPath, path.join(out, 'zotero.log')));
    await safe(() => cpSync(path.join(this.workDir, 'profile', 'user.js'), path.join(out, 'user.js')));
    await safe(async () => {
      const d = await this.dump();
      (await import('node:fs')).writeFileSync(path.join(out, 'dump.json'), JSON.stringify(d, null, 2));
    });
    await safe(() => (
      import('node:fs').then((fs) => fs.writeFileSync(path.join(out, 'container.log'), docker(['logs', this.container])))
    ));
  }

  async stop() {
    try { await this.call('quit', {}, { timeoutMs: 5000 }); } catch { /* already gone */ }
    for (let i = 0; i < 20 && this.running(); i++) await sleep(250);
    try { docker(['rm', '-f', this.container]); } catch { /* ignore */ }
  }
}
