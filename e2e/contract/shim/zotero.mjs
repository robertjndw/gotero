// Builds a vm context holding our own minimal `Zotero` global, then runs
// Zotero desktop's real sync client files inside it, unmodified.
import vm from 'node:vm';
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { ensureRef, loadRefs } from '../fetch.mjs';
import * as http from './http.mjs';
import { createStorageHarness } from './storage.mjs';

// Roughly what the bundled schema reports. The server hides fields newer than
// this, so it must not be absurdly low.
export const SCHEMA_VERSION = 45;

const PLURALS = { search: 'searches' };
const plural = (t) => PLURALS[t] || `${t}s`;

// Same contract as the desktop's ConcurrentCaller as far as the API client
// cares: run the function, and remember when asked to pause.
export class Caller {
  constructor() {
    this.pauses = [];
  }
  start(fn) {
    return fn();
  }
  pause(ms) {
    this.pauses.push(ms);
  }
}

export async function loadClient(ref, { debug = !!process.env.ZOTERO_DEBUG } = {}) {
  const dir = await ensureRef(ref);
  const cfg = await loadRefs();
  const harness = createStorageHarness();
  const logs = [];
  const log = (...a) => {
    logs.push(a.map(String).join(' '));
    if (debug) console.error('[zotero]', ...a);
  };

  const prefs = { 'sync.server.compressData': true, 'sync.debugUploadPolicy': 0 };
  const Zotero = {
    debug: (m) => log(typeof m === 'object' ? JSON.stringify(m) : m),
    logError: (e) => log('ERROR', e?.message ?? e),
    Prefs: { get: (k) => prefs[k] },
    Schema: { globalSchemaVersion: SCHEMA_VERSION },
    DataObjectUtilities: { getObjectTypePlural: plural },
    Promise: { delay: (ms) => new Promise((r) => setTimeout(r, ms)) },
    HTTP: {
      request: http.request,
      download: http.download,
      isWriteMethod: http.isWriteMethod,
      UnexpectedStatusException: http.UnexpectedStatusException,
      BrowserOfflineException: http.BrowserOfflineException,
    },
    ...harness.env.zotero,
    Sync: harness.env.zotero.Sync,
  };

  const sandbox = {
    Zotero,
    Blob,
    URLSearchParams,
    TextDecoder,
    OS: harness.env.OS,
    IOUtils: harness.env.IOUtils,
    PathUtils: harness.env.PathUtils,
    Components: harness.env.Components,
    File: harness.env.File,
    Cr: { NS_OK: http.NS_OK, NS_ERROR_ABORT: http.NS_ERROR_ABORT },
  };
  const context = vm.createContext(sandbox);
  for (const file of cfg.files) {
    const source = await readFile(join(dir, file), 'utf8');
    new vm.Script(source, { filename: `zotero@${ref}/${file}` }).runInContext(context);
  }

  const caller = new Caller();
  const makeApiClient = ({ baseURL, apiKey, schemaVersion = SCHEMA_VERSION }) =>
    new Zotero.Sync.APIClient({ baseURL, apiVersion: 3, apiKey, schemaVersion, caller });

  return { Zotero, context, harness, caller, logs, makeApiClient };
}
