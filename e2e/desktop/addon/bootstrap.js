/*
 * Test-only driver for the gotero desktop e2e harness. It adds HTTP endpoints
 * under /gotero-test/ to Zotero's built-in connector server so the Node
 * harness can create data, trigger syncs and dump state without any UI.
 *
 * Written from scratch against Zotero's public-ish JS objects; nothing here is
 * copied from Zotero itself. Keep it tolerant: the same file has to run on
 * 7.0, 9.x and 10.x, so anything that moved between releases is feature-tested
 * instead of version-tested.
 */

var ADDON_VERSION = '1.0.0';
var PREFIX = '/gotero-test/';

var registered = [];
var state = {
  conflictMode: 'local', // what the stubbed merge dialogs answer
  conflictLog: [],
  syncFinishCount: 0,
  syncStartCount: 0,
  lastSyncFinishAt: null,
  origMergeWindow: null,
  origFileResolve: null,
};
var observerID = null;

// Zotero hands bootstrap plugins a global `Zotero`, but only once it has
// finished initialising, so every entry point waits for that first.
async function startup({ version }) {
  await Zotero.initializationPromise;
  ADDON_VERSION = version || ADDON_VERSION;
  for (let [name, spec] of Object.entries(commands)) {
    registerEndpoint(name, spec);
  }
  observerID = Zotero.Notifier.registerObserver({
    notify(event, type) {
      if (type !== 'sync') return;
      if (event === 'start') state.syncStartCount++;
      if (event === 'finish') {
        state.syncFinishCount++;
        state.lastSyncFinishAt = Date.now();
      }
    },
  }, ['sync'], 'gotero-e2e');
  Zotero.debug('[gotero-e2e] test driver ready, endpoints under ' + PREFIX);
}

function shutdown() {
  for (let path of registered) delete Zotero.Server.Endpoints[path];
  registered = [];
  if (observerID) Zotero.Notifier.unregisterObserver(observerID);
  uninstallConflictStubs();
}

function install() {}
function uninstall() {}

function registerEndpoint(name, spec) {
  let path = PREFIX + name;
  let Endpoint = function () {};
  // init() must declare exactly one parameter: the server picks the calling
  // convention (request object in, [status, type, body] out) by arity.
  Endpoint.prototype = {
    supportedMethods: ['GET', 'POST'],
    supportedDataTypes: ['application/json'],
    init: async function (req) {
      try {
        let result = await spec.run(req.data || {}, req);
        return [200, 'application/json', JSON.stringify(result ?? { ok: true })];
      }
      catch (e) {
        Zotero.logError(e);
        return [500, 'application/json', JSON.stringify({ error: serializeError(e) })];
      }
    },
  };
  Zotero.Server.Endpoints[path] = Endpoint;
  registered.push(path);
}

function serializeError(e) {
  if (!e) return { message: 'unknown error' };
  let out = {
    name: e.name || (e.constructor && e.constructor.name),
    message: String(e.message ?? e),
    stack: e.stack ? String(e.stack).split('\n').slice(0, 8).join('\n') : undefined,
  };
  // Zotero.Error carries a numeric code, HTTP exceptions an xmlhttp with status
  if (e.error !== undefined) out.code = e.error;
  if (e.status !== undefined) out.status = e.status;
  if (e.xmlhttp && e.xmlhttp.status !== undefined) out.status = e.xmlhttp.status;
  if (e.xmlhttp && typeof e.xmlhttp.responseText === 'string') {
    out.responseText = e.xmlhttp.responseText.slice(0, 500);
  }
  if (e.data) {
    try { out.data = JSON.parse(JSON.stringify(e.data)); } catch (_) { /* not serialisable */ }
  }
  return out;
}

//
// Helpers
//
function libraryIDOf(args) {
  return args.libraryID || Zotero.Libraries.userLibraryID;
}

async function getItem(libraryID, key) {
  let item = await Zotero.Items.getByLibraryAndKeyAsync(libraryID, key);
  if (!item) throw new Error(`item ${key} not found in library ${libraryID}`);
  return item;
}

async function getCollection(libraryID, key) {
  let c = await Zotero.Collections.getByLibraryAndKeyAsync(libraryID, key);
  if (!c) throw new Error(`collection ${key} not found in library ${libraryID}`);
  return c;
}

function normaliseTags(tags) {
  return (tags || []).map((t) => (typeof t === 'string' ? { tag: t } : t));
}

// Accept both {itemType, fields: {...}} and flat Zotero JSON.
function toItemJSON(spec) {
  let json = { ...spec };
  if (spec.fields) {
    delete json.fields;
    Object.assign(json, spec.fields);
  }
  if (json.tags) json.tags = normaliseTags(json.tags);
  return json;
}

async function createItemFromSpec(libraryID, spec) {
  let json = toItemJSON(spec);
  if (!json.itemType) throw new Error('itemType missing');
  let item = new Zotero.Item(json.itemType);
  item.libraryID = libraryID;
  // fromJSON understands tags, collections, creators and parentItem keys
  item.fromJSON(json);
  await item.saveTx();
  return item.key;
}

async function md5Of(path) {
  if (Zotero.Utilities.Internal.md5Async) {
    return Zotero.Utilities.Internal.md5Async(path);
  }
  return Zotero.Utilities.Internal.md5(Zotero.File.pathToFile(path));
}

function decodeBase64(b64) {
  let bin = atob(b64);
  let bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes;
}

function payloadBytes(args) {
  if (args.base64 !== undefined) return decodeBase64(args.base64);
  if (args.text !== undefined) return new TextEncoder().encode(args.text);
  return null;
}

async function fileInfo(item) {
  if (!item.isAttachment() || !item.isStoredFileAttachment()) return null;
  let path = await item.getFilePathAsync();
  if (!path) return { exists: false };
  let { size } = await IOUtils.stat(path);
  return { exists: true, size, md5: await md5Of(path), name: PathUtils.filename(path) };
}

function libraryInfo(lib) {
  let info = {
    libraryID: lib.libraryID,
    type: lib.libraryType,
    name: lib.name,
    version: lib.libraryVersion,
    editable: lib.editable,
  };
  if (lib.libraryType === 'group') info.groupID = lib.groupID;
  return info;
}

function syncableLibraries() {
  return Zotero.Libraries.getAll().filter((l) => l.libraryType === 'user' || l.libraryType === 'group');
}

async function dumpLibrary(lib) {
  let libraryID = lib.libraryID;
  let itemIDs = await Zotero.DB.columnQueryAsync('SELECT itemID FROM items WHERE libraryID=?', [libraryID]);
  let items = itemIDs.length ? await Zotero.Items.getAsync(itemIDs) : [];
  let out = [];
  for (let item of items) {
    let json = item.toJSON();
    json.deleted = !!item.deleted;
    json.synced = item.synced;
    let file = await fileInfo(item);
    if (file) json.file = file;
    out.push(json);
  }
  out.sort((a, b) => (a.key < b.key ? -1 : 1));

  let collIDs = await Zotero.DB.columnQueryAsync('SELECT collectionID FROM collections WHERE libraryID=?', [libraryID]);
  let collections = collIDs.length ? await Zotero.Collections.getAsync(collIDs) : [];
  let colls = collections.map((c) => ({ ...c.toJSON(), deleted: !!c.deleted }));
  colls.sort((a, b) => (a.key < b.key ? -1 : 1));

  let tags = (await Zotero.Tags.getAll(libraryID)).map((t) => ({ tag: t.tag, type: t.type || 0 }));
  tags.sort((a, b) => (a.tag < b.tag ? -1 : 1));

  return { ...libraryInfo(lib), items: out, collections: colls, tags };
}

//
// Conflict stubs. The real dialogs are modal windows nobody can click in CI,
// so both are replaced by functions that answer from state.conflictMode.
//
function installConflictStubs() {
  let Local = Zotero.Sync.Data.Local;
  if (!state.origMergeWindow) state.origMergeWindow = Local.showConflictResolutionWindow;
  Local.showConflictResolutionWindow = function (conflicts) {
    let mode = state.conflictMode;
    state.conflictLog.push({
      kind: 'object',
      mode,
      conflicts: conflicts.map((c) => ({
        key: (c.left && c.left.key) || (c.right && c.right.key),
        localDeleted: !!(c.left && c.left.deleted),
        remoteDeleted: !!(c.right && c.right.deleted),
        fields: (c.conflicts || []).map((pair) => (pair && pair[0] && pair[0].field) || null),
      })),
    });
    if (mode === 'cancel') return null;
    // Same shape the merge window produces: one {selected, data} per conflict.
    return conflicts.map((c) => {
      let side = mode === 'remote' ? 'right' : 'left';
      let data = JSON.parse(JSON.stringify(c[side]));
      data.key = (c.left && c.left.key) || (c.right && c.right.key);
      data.version = c[side].version;
      return { selected: side, data };
    });
  };

  let StorageLocal = Zotero.Sync.Storage && Zotero.Sync.Storage.Local;
  if (StorageLocal && StorageLocal.resolveConflicts) {
    if (!state.origFileResolve) state.origFileResolve = StorageLocal.resolveConflicts;
    StorageLocal.resolveConflicts = async function (libraryID) {
      let conflicts = await this.getConflicts(libraryID);
      if (!conflicts.length) return false;
      let mode = state.conflictMode;
      state.conflictLog.push({
        kind: 'file',
        mode,
        conflicts: conflicts.map((c) => ({ key: c.left.key })),
      });
      if (mode === 'cancel') return false;
      for (let c of conflicts) {
        let item = Zotero.Items.getByLibraryAndKey(libraryID, c.left.key);
        if (mode === 'remote') {
          item.attachmentSyncState = this.SYNC_STATE_FORCE_DOWNLOAD;
        }
        else {
          // Pretend the remote state is what we last synced, otherwise the
          // forced upload is rejected as a stale write.
          if (c.right.mtime) item.attachmentSyncedModificationTime = c.right.mtime;
          if (c.right.md5) item.attachmentSyncedHash = c.right.md5;
          item.attachmentSyncState = this.SYNC_STATE_FORCE_UPLOAD;
        }
        await item.saveTx({ skipAll: true });
      }
      return true;
    };
  }
}

function uninstallConflictStubs() {
  if (state.origMergeWindow) Zotero.Sync.Data.Local.showConflictResolutionWindow = state.origMergeWindow;
  if (state.origFileResolve) Zotero.Sync.Storage.Local.resolveConflicts = state.origFileResolve;
}

async function writeKey(key) {
  let Local = Zotero.Sync.Data.Local;
  // 10.x encrypts keys with the OS keystore, which does not exist in a bare
  // container, and its fallback is a modal prompt. Writing the login directly
  // keeps the run unattended. Older releases have no such split.
  if (Local._writeAPIKey) {
    await Local._writeAPIKey(key);
    Local._reencryptedAPIKey = true;
    Local._mirroredAPIKey = true;
    Zotero.Notifier.trigger('modify', 'api-key', []);
  }
  else {
    await Local.setAPIKey(key);
  }
}

//
// Commands. Each is POST /gotero-test/<name> with a JSON body.
//
var commands = {
  ping: {
    run: async () => ({
      ok: true,
      addonVersion: ADDON_VERSION,
      zoteroVersion: Zotero.version,
      appName: Zotero.appName,
      platform: Zotero.platform,
      uiReady: !!Zotero.getMainWindow(),
      dataDir: Zotero.DataDirectory.dir,
      apiURL: Zotero.Sync.Runner.baseURL,
      streamingURL: Zotero.Prefs.get('streaming.url'),
      userLibraryID: Zotero.Libraries.userLibraryID,
      userID: Zotero.Users.getCurrentUserID() || null,
    }),
  },

  eval: {
    // Escape hatch for scenarios that need one-off access. Loopback only and
    // only present in test profiles.
    run: async (args) => {
      let fn = new (Object.getPrototypeOf(async function () {}).constructor)('args', args.code);
      return { value: await fn(args.args || {}) };
    },
  },

  setPref: {
    run: async (args) => {
      Zotero.Prefs.set(args.name, args.value, !!args.global);
      return { ok: true };
    },
  },

  setAPIKey: {
    run: async (args) => {
      if (!args.key) throw new Error('key missing');
      await writeKey(args.key);
      // Without a stored userID the first sync prompts about an empty
      // library, which would hang a headless run.
      if (args.userID) {
        await Zotero.Users.setCurrentUserID(args.userID);
        await Zotero.Users.setCurrentUsername(args.username || `user${args.userID}`);
        if (Zotero.Users.setCurrentName) {
          await Zotero.Users.setCurrentName(args.displayName || args.username || '');
        }
      }
      return { ok: true, hasKey: !!(await Zotero.Sync.Data.Local.getAPIKey()) };
    },
  },

  clearAPIKey: {
    run: async () => {
      await Zotero.Sync.Data.Local.setAPIKey('');
      return { ok: true };
    },
  },

  // Pre-keys login (password sent to POST /keys on the first sync). The login
  // manager entry mirrors what old preference panes wrote.
  setLegacyLogin: {
    run: async (args) => {
      Zotero.Prefs.set('sync.server.username', args.username);
      let LoginInfo = new Components.Constructor(
        '@mozilla.org/login-manager/loginInfo;1', Components.interfaces.nsILoginInfo, 'init'
      );
      let info = new LoginInfo('chrome://zotero', null, 'Zotero Sync Server', args.username, args.password, '', '');
      await Services.logins.addLoginAsync(info);
      return { ok: true };
    },
  },

  libraries: {
    run: async () => ({ libraries: syncableLibraries().map(libraryInfo) }),
  },

  createCollection: {
    run: async (args) => {
      let c = new Zotero.Collection();
      c.libraryID = libraryIDOf(args);
      c.name = args.name;
      if (args.parentKey) c.parentKey = args.parentKey;
      await c.saveTx();
      return { key: c.key };
    },
  },

  renameCollection: {
    run: async (args) => {
      let c = await getCollection(libraryIDOf(args), args.key);
      c.name = args.name;
      await c.saveTx();
      return { ok: true };
    },
  },

  moveCollection: {
    run: async (args) => {
      let c = await getCollection(libraryIDOf(args), args.key);
      c.parentKey = args.parentKey || false;
      await c.saveTx();
      return { ok: true };
    },
  },

  trashCollection: {
    run: async (args) => {
      let libraryID = libraryIDOf(args);
      for (let key of args.keys) {
        let c = await getCollection(libraryID, key);
        c.deleted = true;
        await c.saveTx();
      }
      return { ok: true };
    },
  },

  restoreCollection: {
    run: async (args) => {
      let libraryID = libraryIDOf(args);
      for (let key of args.keys) {
        let c = await getCollection(libraryID, key);
        c.deleted = false;
        await c.saveTx();
      }
      return { ok: true };
    },
  },

  deleteCollection: {
    run: async (args) => {
      let libraryID = libraryIDOf(args);
      for (let key of args.keys) {
        let c = await getCollection(libraryID, key);
        await c.eraseTx({ deleteItems: !!args.deleteItems });
      }
      return { ok: true };
    },
  },

  createItem: {
    run: async (args) => {
      let key = await createItemFromSpec(libraryIDOf(args), args.item);
      return { key };
    },
  },

  createItems: {
    run: async (args) => {
      let libraryID = libraryIDOf(args);
      let keys = [];
      for (let spec of args.items) keys.push(await createItemFromSpec(libraryID, spec));
      return { keys };
    },
  },

  editItem: {
    run: async (args) => {
      let item = await getItem(libraryIDOf(args), args.key);
      for (let [k, v] of Object.entries(args.fields || {})) item.setField(k, v);
      if (args.note !== undefined) item.setNote(args.note);
      if (args.tags) item.setTags(normaliseTags(args.tags));
      if (args.addTags) for (let t of normaliseTags(args.addTags)) item.addTag(t.tag, t.type);
      if (args.removeTags) for (let t of args.removeTags) item.removeTag(t);
      if (args.collections) item.setCollections(args.collections.map((k) => {
        return Zotero.Collections.getByLibraryAndKey(item.libraryID, k).id;
      }));
      if (args.addToCollection) item.addToCollection(args.addToCollection);
      if (args.removeFromCollection) item.removeFromCollection(args.removeFromCollection);
      await item.saveTx();
      return { ok: true };
    },
  },

  trashItem: {
    run: async (args) => {
      let libraryID = libraryIDOf(args);
      let ids = [];
      for (let key of args.keys) ids.push((await getItem(libraryID, key)).id);
      await Zotero.Items.trashTx(ids);
      return { ok: true };
    },
  },

  restoreItem: {
    run: async (args) => {
      let libraryID = libraryIDOf(args);
      for (let key of args.keys) {
        let item = await getItem(libraryID, key);
        item.deleted = false;
        await item.saveTx();
      }
      return { ok: true };
    },
  },

  // Permanent delete of specific items, skipping the trash.
  deleteItem: {
    run: async (args) => {
      let libraryID = libraryIDOf(args);
      for (let key of args.keys) await (await getItem(libraryID, key)).eraseTx();
      return { ok: true };
    },
  },

  emptyTrash: {
    run: async (args) => {
      await Zotero.Items.emptyTrash(libraryIDOf(args));
      return { ok: true };
    },
  },

  renameTag: {
    run: async (args) => {
      await Zotero.Tags.rename(libraryIDOf(args), args.oldName, args.newName);
      return { ok: true };
    },
  },

  deleteTag: {
    run: async (args) => {
      let libraryID = libraryIDOf(args);
      let id = Zotero.Tags.getID(args.name);
      if (!id) throw new Error(`tag ${args.name} not found`);
      await Zotero.Tags.removeFromLibrary(libraryID, [id]);
      return { ok: true };
    },
  },

  // Stores a file attachment. The bytes come from `path` (inside the
  // container), `base64` or `text`.
  importFileAttachment: {
    run: async (args) => {
      let libraryID = libraryIDOf(args);
      let path = args.path;
      if (!path) {
        let bytes = payloadBytes(args);
        if (!bytes) throw new Error('path, base64 or text required');
        let name = args.filename || 'attachment.txt';
        let dir = PathUtils.join(Zotero.getTempDirectory().path, 'gotero-e2e-' + Date.now());
        await IOUtils.makeDirectory(dir);
        path = PathUtils.join(dir, name);
        await IOUtils.write(path, bytes);
      }
      let opts = { file: path, libraryID };
      if (args.parentKey) opts.parentItemID = (await getItem(libraryID, args.parentKey)).id;
      else if (args.collections) {
        opts.collections = args.collections.map((k) => Zotero.Collections.getByLibraryAndKey(libraryID, k).id);
      }
      if (args.title) opts.title = args.title;
      if (args.contentType) opts.contentType = args.contentType;
      let item = await Zotero.Attachments.importFromFile(opts);
      return { key: item.key, file: await fileInfo(item) };
    },
  },

  // Overwrites the stored file of an existing attachment, as if the user had
  // edited the PDF. The file is rescanned right away so the change is registered.
  modifyFile: {
    run: async (args) => {
      let item = await getItem(libraryIDOf(args), args.key);
      let path = await item.getFilePathAsync();
      if (!path) throw new Error('attachment has no local file');
      let bytes = payloadBytes(args);
      if (!bytes) throw new Error('base64 or text required');
      await IOUtils.write(path, bytes);
      // File sync ignores mtimes within one second of each other, so two
      // clients editing in the same second would look unchanged. Scenarios
      // pass distinct mtimeMs values for the sides of a conflict.
      await IOUtils.setModificationTime(path, args.mtimeMs || Date.now() + 2000);
      // Real clients notice the edit long before the next sync (file watcher or
      // the previous scan). Without this the sync would pull the remote
      // metadata first and then see no local change, hiding the conflict.
      if (Zotero.Sync.Storage.Local.checkForUpdatedFiles) {
        await Zotero.Sync.Storage.Local.checkForUpdatedFiles(item.libraryID, [item.id]);
      }
      return { ok: true, file: await fileInfo(item) };
    },
  },

  // Remote-driven download of a not yet fetched file (download-as-needed mode).
  downloadFile: {
    run: async (args) => {
      let item = await getItem(libraryIDOf(args), args.key);
      await Zotero.Sync.Runner.downloadFile(item);
      return { file: await fileInfo(item) };
    },
  },

  setConflictResolution: {
    run: async (args) => {
      if (!['local', 'remote', 'cancel'].includes(args.mode)) throw new Error('mode must be local, remote or cancel');
      state.conflictMode = args.mode;
      installConflictStubs();
      return { ok: true, mode: state.conflictMode };
    },
  },

  conflictLog: {
    run: async (args) => {
      let log = state.conflictLog;
      if (args.clear) state.conflictLog = [];
      return { log };
    },
  },

  // Runs a full foreground sync and resolves when it is completely done.
  // Passing onError means failures come back here instead of popping up
  // dialogs, which makes them assertable.
  sync: {
    run: async (args) => {
      let errors = [];
      let started = Date.now();
      let options = {
        background: !!args.background,
        onError: (e) => errors.push(serializeError(e)),
      };
      if (args.libraries) options.libraries = args.libraries;
      let timeoutMs = args.timeoutMs || 120000;
      let timedOut = false;
      let syncPromise = Zotero.Sync.Runner.sync(options);
      let timeout = Zotero.Promise.delay(timeoutMs).then(() => { timedOut = true; });
      await Promise.race([syncPromise, timeout]);
      if (timedOut) {
        errors.push({ name: 'Timeout', message: `sync did not finish within ${timeoutMs} ms` });
        try { Zotero.Sync.Runner.stop(); } catch (_) { /* nothing to stop */ }
      }
      return { ok: errors.length === 0, errors, durationMs: Date.now() - started, conflicts: state.conflictLog.length };
    },
  },

  syncStatus: {
    run: async () => ({
      started: state.syncStartCount,
      finished: state.syncFinishCount,
      lastFinishAt: state.lastSyncFinishAt,
      autoSync: Zotero.Prefs.get('sync.autoSync'),
      streamingEnabled: Zotero.Prefs.get('streaming.enabled'),
    }),
  },

  // For stream-triggered syncs: blocks until the finished counter passes
  // `after`, i.e. until a sync nobody asked for here has completed.
  waitForStreamingSync: {
    run: async (args) => {
      let after = args.after ?? state.syncFinishCount;
      let deadline = Date.now() + (args.timeoutMs || 20000);
      while (Date.now() < deadline) {
        if (state.syncFinishCount > after) return { ok: true, finished: state.syncFinishCount };
        await Zotero.Promise.delay(250);
      }
      return { ok: false, finished: state.syncFinishCount };
    },
  },

  dump: {
    run: async (args) => {
      let libs = syncableLibraries();
      if (args.libraryID) libs = libs.filter((l) => l.libraryID === args.libraryID);
      let libraries = [];
      for (let lib of libs) libraries.push(await dumpLibrary(lib));
      return { libraries };
    },
  },

  quit: {
    run: async () => {
      Zotero.Promise.delay(200).then(() => {
        Services.startup.quit(Components.interfaces.nsIAppStartup.eForceQuit);
      });
      return { ok: true };
    },
  },
};
