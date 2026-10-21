// Our own stand-in for Zotero.HTTP, built on Node's fetch.
//
// It only reproduces the parts of the desktop's contract that the sync client
// leans on: successCodes handling, the exception shape callers inspect, gzip
// request bodies, response types and manual redirect handling.
import { gzipSync } from 'node:zlib';
import { mkdir, writeFile } from 'node:fs/promises';
import { dirname } from 'node:path';

export class UnexpectedStatusException extends Error {
  constructor(xmlhttp, msg) {
    super(msg || `Unexpected status ${xmlhttp.status} from ${xmlhttp.responseURL}`);
    this.name = 'UnexpectedStatusException';
    this.xmlhttp = xmlhttp;
    this.status = xmlhttp.status;
  }
  is4xx() {
    return this.status >= 400 && this.status < 500;
  }
  is5xx() {
    return this.status >= 500 && this.status < 600;
  }
}

export class BrowserOfflineException extends Error {
  constructor(msg = 'Browser is offline') {
    super(msg);
    this.name = 'BrowserOfflineException';
  }
}

const WRITE_METHODS = new Set(['POST', 'PUT', 'PATCH', 'DELETE']);
export const isWriteMethod = (m) => WRITE_METHODS.has(String(m).toUpperCase());

// Mimics the slice of XMLHttpRequest the client reads. getResponseHeader
// returns null for missing headers, like the real thing.
function xhrLike(res, bodyBytes, responseType) {
  const text = () => Buffer.from(bodyBytes).toString('utf8');
  const x = {
    status: res.status,
    statusText: res.statusText,
    responseURL: res.url,
    getResponseHeader: (name) => res.headers.get(name),
    getAllResponseHeaders: () =>
      [...res.headers].map(([k, v]) => `${k}: ${v}`).join('\r\n') + '\r\n',
    channel: {},
  };
  Object.defineProperty(x, 'responseText', { get: text, enumerable: true });
  Object.defineProperty(x, 'response', {
    get() {
      switch (responseType) {
        case 'json':
          return text() === '' ? null : JSON.parse(text());
        case 'arraybuffer':
          return bodyBytes.buffer.slice(bodyBytes.byteOffset, bodyBytes.byteOffset + bodyBytes.byteLength);
        case 'blob':
          return new Blob([bodyBytes], { type: res.headers.get('content-type') || '' });
        default:
          return text();
      }
    },
    enumerable: true,
  });
  return x;
}

function networkFailure(url, err) {
  const x = {
    status: 0,
    statusText: '',
    responseURL: url,
    responseText: '',
    response: null,
    getResponseHeader: () => null,
    getAllResponseHeaders: () => '',
    channel: {},
  };
  return new UnexpectedStatusException(x, `Network error for ${url}: ${err?.cause?.code || err?.message}`);
}

function checkStatus(xhr, options) {
  const codes = options.successCodes;
  if (codes === false) return;
  const ok = Array.isArray(codes) ? codes.includes(xhr.status) : xhr.status >= 200 && xhr.status < 300;
  if (!ok) throw new UnexpectedStatusException(xhr);
}

async function prepareBody(options, headers) {
  let body = options.body;
  if (body === undefined || body === null || body === '') return undefined;
  if (options.compressBody) {
    body = gzipSync(typeof body === 'string' ? Buffer.from(body, 'utf8') : Buffer.from(await new Blob([body]).arrayBuffer()));
    headers['Content-Encoding'] = 'gzip';
  }
  return body;
}

export async function request(method, url, options = {}) {
  // The desktop also accepts a bare string as the body; keep that working.
  if (typeof options === 'string') options = { body: options };
  const headers = { ...(options.headers || {}) };
  if (options.noCache) headers['Cache-Control'] = 'no-cache';
  method = method.toUpperCase();
  const body = await prepareBody(options, headers);
  const init = {
    method,
    headers,
    body,
    // Only an explicit followRedirects:false opts out; ZFS download relies on
    // seeing the 302 and its Zotero-File-* headers itself.
    redirect: options.followRedirects === false ? 'manual' : 'follow',
  };
  if (options.timeout) init.signal = AbortSignal.timeout(options.timeout);
  let res;
  let bytes;
  try {
    res = await fetch(url, init);
    bytes = new Uint8Array(await res.arrayBuffer());
  } catch (e) {
    throw networkFailure(url, e);
  }
  const xhr = xhrLike(res, bytes, options.responseType || 'text');
  checkStatus(xhr, options);
  return xhr;
}

// Fake nsIHttpChannel, handed to the redirect observer that older clients use
// to peek at the 302 before deciding whether to follow it.
function redirectChannels(res, from, to) {
  const oldChannel = {
    QueryInterface() {
      return this;
    },
    responseStatus: res.status,
    URI: { spec: from },
    getResponseHeader(name) {
      const v = res.headers.get(name);
      if (v === null) throw new Error(`NS_ERROR_NOT_AVAILABLE: ${name}`);
      return v;
    },
  };
  return { oldChannel, newChannel: { URI: { spec: to } } };
}

export const NS_OK = 0;
export const NS_ERROR_ABORT = 0x80004004;

async function saveBody(res, dest) {
  await mkdir(dirname(dest), { recursive: true });
  await writeFile(dest, new Uint8Array(await res.arrayBuffer()));
}

// Stand-in for Zotero.HTTP.download. Two client generations call it:
//  - newer: a plain URL fetched without any credentials (redirect target of
//    the file endpoint),
//  - older: the API URL plus a notificationCallbacks observer that is told
//    about the redirect and may abort it.
export async function download(url, dest, options = {}) {
  const observer = options.notificationCallbacks;
  const hasObserver = typeof observer?.asyncOnChannelRedirect === 'function';
  const init = {
    headers: { ...(options.headers || {}) },
    redirect: hasObserver ? 'manual' : 'follow',
  };
  let res;
  try {
    res = await fetch(url, init);
    if (hasObserver && res.status >= 300 && res.status < 400 && res.headers.get('location')) {
      const target = new URL(res.headers.get('location'), url).toString();
      const { oldChannel, newChannel } = redirectChannels(res, url, target);
      const verdict = await new Promise((resolve) => {
        observer.asyncOnChannelRedirect(oldChannel, newChannel, 0, {
          onRedirectVerifyCallback: resolve,
        });
      });
      if (verdict !== NS_OK) {
        // Observer aborted the redirect, so the caller sees the 302 itself.
        return xhrLike(res, new Uint8Array(), 'text');
      }
      // The follow-up request deliberately carries no API headers.
      res = await fetch(target);
    }
  } catch (e) {
    throw networkFailure(url, e);
  }
  const probe = xhrLike(res, new Uint8Array(), 'text');
  checkStatus(probe, options.successCodes ? options : { ...options, successCodes: [200] });
  if (res.status === 200) await saveBody(res, dest);
  return probe;
}
