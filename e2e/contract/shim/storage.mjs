// A tiny in-memory model of the Zotero item/file layer, just deep enough for
// the real ZFS upload and download paths to run against actual files on disk.
//
// None of this mirrors desktop code; it only answers the questions ZFS asks
// ("what is the md5 of the local file?", "where is it?", "save this state").
import { createHash } from 'node:crypto';
import { mkdir, readFile, rm, stat, utimes, writeFile, copyFile } from 'node:fs/promises';
import { existsSync, mkdirSync, mkdtempSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';
import { tmpdir } from 'node:os';

export const md5File = async (p) => createHash('md5').update(await readFile(p)).digest('hex');

export class FakeAttachment {
  static nextID = 1;

  constructor({ harness, libraryID, key, version, filePath, contentType = 'application/pdf' }) {
    this.harness = harness;
    this.id = FakeAttachment.nextID++;
    this.libraryID = libraryID;
    this.key = key;
    this.version = version;
    this.filePath = filePath;
    this.attachmentContentType = contentType;
    this.attachmentSyncedHash = null;
    this.attachmentSyncedModificationTime = null;
    this.attachmentSyncState = 'to_upload';
    this.saves = 0;
    harness.itemsByID.set(this.id, this);
  }

  get libraryKey() {
    return `${this.libraryID}/${this.key}`;
  }
  get library() {
    return this.harness.libraries.get(this.libraryID);
  }
  get attachmentFilename() {
    return basename(this.filePath);
  }
  get attachmentHash() {
    return md5File(this.filePath);
  }
  get attachmentModificationTime() {
    return stat(this.filePath).then((s) => Math.floor(s.mtimeMs));
  }
  getFilePath() {
    return this.filePath;
  }
  getFile() {
    return { path: this.filePath };
  }
  // Text attachments would be zipped by the desktop; we stay on the
  // single-file path so the zip builder is not needed.
  isImportedAttachment() {
    return true;
  }
  async saveTx() {
    this.saves++;
  }
  async save() {
    this.saves++;
  }
}

// Builds the per-test environment object that shim/zotero.mjs plugs into the VM.
export function createStorageHarness() {
  const harness = {
    tmpDir: mkdtempSync(join(tmpdir(), 'gotero-contract-')),
    libraries: new Map(),
    itemsByID: new Map(),
    calls: { markUnsynced: [], syncQueue: [], cacheSaved: [], processed: [] },
    storageRemaining: new Map(),
  };

  harness.addLibrary = (libraryID, libraryType, libraryTypeID) =>
    harness.libraries.set(libraryID, { libraryID, libraryType, libraryTypeID });

  harness.newAttachment = async (opts, bytes, mtimeMs) => {
    const filePath = join(harness.tmpDir, 'files', opts.key, opts.filename || 'file.pdf');
    await mkdir(dirname(filePath), { recursive: true });
    await writeFile(filePath, bytes);
    if (mtimeMs) await utimes(filePath, new Date(mtimeMs), new Date(mtimeMs));
    return new FakeAttachment({ ...opts, harness, filePath });
  };

  class Result {
    constructor(opts = {}) {
      this.localChanges = !!opts.localChanges;
      this.remoteChanges = !!opts.remoteChanges;
      this.syncRequired = !!opts.syncRequired;
      this.fileSyncRequired = !!opts.fileSyncRequired;
    }
  }

  class ZoteroError extends Error {
    constructor(message, error, data = {}) {
      super(message);
      this.error = error;
      Object.assign(this, data);
    }
  }
  ZoteroError.ERROR_ZFS_OVER_QUOTA = 'ZFS_OVER_QUOTA';

  const tempDir = () => ({
    path: join(harness.tmpDir, 'tmp'),
  });
  mkdirSync(join(harness.tmpDir, 'tmp'), { recursive: true });

  // Pieces the VM context receives. Keys are the names the client code uses.
  harness.env = {
    OS: {
      Path: { join: (...p) => join(...p) },
      File: {
        stat: async (p) => ({ size: (await stat(p)).size }),
        setDates: async (p, _atime, date) => utimes(p, date, date),
        remove: (p) => rm(p, { force: true }),
      },
    },
    IOUtils: {
      write: async (p, bytes) => {
        await mkdir(dirname(p), { recursive: true });
        await writeFile(p, bytes);
      },
      exists: async (p) => existsSync(p),
    },
    PathUtils: { filename: (p) => basename(p) },
    Components: { utils: { importGlobalProperties() {} }, interfaces: { nsIHttpChannel: {} } },
    // Desktop exposes File.createFromFileName; Node's File does not.
    File: {
      createFromFileName: async (p) => new File([await readFile(p)], basename(p)),
    },
    zotero: {
      Error: ZoteroError,
      getString: (name) => name,
      getTempDirectory: tempDir,
      Libraries: { userLibraryID: 1, get: (id) => harness.libraries.get(id) },
      Groups: {
        getGroupIDFromLibraryID: (id) => harness.libraries.get(id)?.libraryTypeID,
        getByLibraryID: (id) => ({ name: `group ${harness.libraries.get(id)?.libraryTypeID}` }),
      },
      Attachments: { hasMultipleFiles: () => false },
      File: { checkFileAccessError(e) { throw e; } },
      Items: {
        updateVersion: async (ids, version) => {
          for (const id of ids) harness.itemsByID.get(id).version = version;
        },
      },
      DB: { executeTransaction: (fn) => fn() },
      Utilities: {
        Internal: { md5Async: md5File },
        numberFormat: (n, d) => Number(n).toFixed(d),
      },
      Sync: {
        Storage: {
          defaultError: 'A file sync error occurred.',
          Result,
          Utilities: { getItemFromRequest: (req) => req.item },
          Local: {
            STORAGE_REMAINING_MINIMUM: 10,
            storageRemainingForLibrary: harness.storageRemaining,
            // The desktop verifies the download and moves it into place; we do
            // the same two steps so tests can check md5 and mtime end to end.
            processDownload: async (data) => {
              const tmp = join(harness.tmpDir, 'tmp', `${data.item.key}.tmp`);
              const md5 = await md5File(tmp);
              harness.calls.processed.push({ md5, expectedMd5: data.md5, mtime: data.mtime, compressed: data.compressed });
              if (md5 !== data.md5) throw new Error(`downloaded md5 ${md5} != Zotero-File-MD5 ${data.md5}`);
              await mkdir(dirname(data.item.filePath), { recursive: true });
              await copyFile(tmp, data.item.filePath);
              await utimes(data.item.filePath, new Date(data.mtime), new Date(data.mtime));
              return new Result({ localChanges: true });
            },
          },
        },
        Data: {
          Local: {
            markObjectAsUnsynced: async (item) => harness.calls.markUnsynced.push(item.key),
            addObjectsToSyncQueue: async (type, lib, keys) => harness.calls.syncQueue.push({ type, lib, keys }),
            saveCacheObjects: async (type, lib, json) => harness.calls.cacheSaved.push({ type, lib, json }),
            saveCacheObject: async () => {},
            getCacheObject: async () => null,
          },
        },
      },
    },
  };

  harness.cleanup = () => rm(harness.tmpDir, { recursive: true, force: true });
  return harness;
}
