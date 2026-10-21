# File storage

Zotero sends attachment files (PDFs, snapshots, ...) to gotero, and gotero keeps them in one of three backends. Clients never talk to the storage directly: every byte streams through gotero, so the storage can stay on a private network.

Files are stored by their MD5 checksum (`blobs/ab/abcdef...`). The same file is stored once, even if it is attached in several libraries.

## Choosing a backend

| Backend | Good for | Set `GOTERO_STORAGE_BACKEND=` |
|---|---|---|
| Local directory | Single server, simplest setup | `local` (default) |
| S3-compatible | Large libraries, cheap durable storage, off-site | `s3` |
| WebDAV | Reusing Nextcloud, ownCloud or a NAS | `webdav` |

All settings are listed in [Configuration](configuration.md#file-storage).

### Local directory

The default. With Docker the files live on the `gotero-files` volume (`/data/files` in the container). Back up that volume along with the database.

### S3

```sh
GOTERO_STORAGE_BACKEND=s3
GOTERO_STORAGE_S3_ENDPOINT=s3.eu-central-1.amazonaws.com
GOTERO_STORAGE_S3_BUCKET=my-zotero-files
GOTERO_STORAGE_S3_ACCESS_KEY=...
GOTERO_STORAGE_S3_SECRET_KEY=...
```

gotero creates the bucket if it doesn't exist. The bucket doesn't need to be public. Files smaller than 16 MiB are written in a single request. Larger ones are streamed as multipart uploads, which needs about 16 MiB of memory per upload in progress.

To try it locally with the bundled S3 gateway:

```sh
docker compose -f deploy/compose.yaml -f deploy/compose.s3.yaml --profile s3 up -d
```

### WebDAV

```sh
GOTERO_STORAGE_BACKEND=webdav
GOTERO_STORAGE_WEBDAV_URL=https://cloud.example.org/remote.php/dav/files/me/zotero/
GOTERO_STORAGE_WEBDAV_USERNAME=me
GOTERO_STORAGE_WEBDAV_PASSWORD=<app password>
```

Point the URL at an empty folder. With Nextcloud, create an app password under **Settings → Security**.

To try it locally with the bundled WebDAV server:

```sh
docker compose -f deploy/compose.yaml -f deploy/compose.webdav.yaml --profile webdav up -d
```

> This is gotero storing its files *on* a WebDAV server. Your Zotero clients still use **Zotero** as the file sync method in their settings, not WebDAV.

## Quotas

- An admin sets a quota per user in the web UI (**Users → Edit**), and a default quota for everyone else under **Settings**, or with `GOTERO_DEFAULT_QUOTA_MB`.
- A user's quota covers their own library plus the group libraries they own.
- When an upload would exceed the quota, Zotero shows its usual "storage quota exceeded" message.

## Cleaning up

Deleted attachments leave their files behind for 24 hours, then the garbage collector removes them. It runs every few hours. An admin can also run it from **Settings → Run garbage collection**, with a dry run first. Abandoned partial uploads are cleaned up the same way.

## Moving to another backend

There is no built-in migration between backends yet. Because files are stored under the same keys in every backend, you can copy them across with a tool such as [rclone](https://rclone.org/) and then switch `GOTERO_STORAGE_BACKEND`:

```sh
rclone copy /var/lib/docker/volumes/gotero_gotero-files/_data/files/blobs s3remote:my-zotero-files/blobs
```

Stop gotero while copying, so that no new uploads are missed.
