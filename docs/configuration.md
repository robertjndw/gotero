# Configuration

gotero is configured entirely with environment variables, all starting with `GOTERO_`. In Docker Compose, put them in `deploy/.env` (start from `deploy/.env.example`).

## Required

| Variable | Description |
|---|---|
| `GOTERO_SECRET` | Random string of 32+ characters that signs file download links. Generate it with `openssl rand -hex 32`. Keep it stable: changing it breaks download links that are in flight. |
| `GOTERO_DATABASE_URL` | PostgreSQL connection URL. Default: `postgres://gotero:gotero@localhost:5432/gotero?sslmode=disable`. The compose file sets it for you. |

## Server

| Variable | Default | Description |
|---|---|---|
| `GOTERO_LISTEN` | `:8080` | Address and port to listen on. |
| `GOTERO_PUBLIC_URL` | (derived) | The URL clients use to reach gotero, e.g. `https://zotero.example.org`. **Set this behind a reverse proxy.** It is used for upload URLs, download redirects and the browser login link. |
| `GOTERO_TRUSTED_PROXIES` | (none) | Comma-separated IPs or CIDRs of your reverse proxies, e.g. `172.16.0.0/12` for a proxy in Docker. Only from these are `X-Forwarded-For` (client address for rate limits) and `X-Forwarded-Proto`/`-Host` (when `GOTERO_PUBLIC_URL` is unset) honoured. **Set this behind a proxy**, or every user shares the proxy's login rate limit. |
| `GOTERO_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `GOTERO_LOG_FORMAT` | `json` | `json` or `text`. |
| `GOTERO_SHUTDOWN_TIMEOUT` | `30s` | How long to wait for running requests on shutdown. |
| `GOTERO_MAX_REQUEST_BYTES` | `134217728` (128 MiB) | Size limit for API request bodies after decompression. Full-text uploads are the largest. |
| `GOTERO_MAX_UPLOAD_BYTES` | `0` (unlimited) | Size limit for a single attachment file. |

## Accounts

| Variable | Default | Description |
|---|---|---|
| `GOTERO_ADMIN_USERNAME` | (none) | If set together with `GOTERO_ADMIN_PASSWORD` and no admin exists yet, this admin account is created on startup. Nothing changes if an admin already exists. |
| `GOTERO_ADMIN_PASSWORD` | (none) | Password for the bootstrap admin (at least 10 characters). |
| `GOTERO_ADMIN_EMAIL` | (none) | Optional email for the bootstrap admin. |
| `GOTERO_OPEN_REGISTRATION` | `false` | Allow anyone to sign up without an invite. It can also be toggled in the web UI, and the UI setting wins once it has been changed there. |
| `GOTERO_DEFAULT_QUOTA_MB` | `0` (unlimited) | File storage quota for users without an individual quota. |

## File storage

`GOTERO_STORAGE_BACKEND` selects where attachment files are kept: `local` (default), `s3` or `webdav`. Files always stream through gotero, so the storage does not need to be reachable by Zotero clients.

### Local directory

| Variable | Default | Description |
|---|---|---|
| `GOTERO_STORAGE_LOCAL_PATH` | `/data/files` | Directory for files. In Docker it lives on the `gotero-files` volume. |

### S3-compatible

Works with AWS S3, Cloudflare R2, Backblaze B2, Garage, SeaweedFS, MinIO and other S3 implementations.

| Variable | Default | Description |
|---|---|---|
| `GOTERO_STORAGE_S3_ENDPOINT` | (required) | Host and port, without scheme, e.g. `s3.eu-central-1.amazonaws.com` or `garage:3900`. |
| `GOTERO_STORAGE_S3_BUCKET` | (required) | Bucket name. It is created if missing. |
| `GOTERO_STORAGE_S3_REGION` | `us-east-1` | Region. |
| `GOTERO_STORAGE_S3_ACCESS_KEY` | | Access key ID. |
| `GOTERO_STORAGE_S3_SECRET_KEY` | | Secret access key. |
| `GOTERO_STORAGE_S3_PREFIX` | (none) | Optional key prefix, for sharing a bucket. |
| `GOTERO_STORAGE_S3_USE_SSL` | `true` | Use HTTPS to reach the endpoint. |

### WebDAV

gotero acts as a WebDAV *client* and stores files on your WebDAV server (Nextcloud, ownCloud, Apache, rclone, ...). This is different from Zotero's own WebDAV sync mode; your Zotero clients keep using gotero for files.

| Variable | Default | Description |
|---|---|---|
| `GOTERO_STORAGE_WEBDAV_URL` | (required) | Base URL of a folder, e.g. `https://cloud.example.org/remote.php/dav/files/me/zotero/`. |
| `GOTERO_STORAGE_WEBDAV_USERNAME` | | Username. |
| `GOTERO_STORAGE_WEBDAV_PASSWORD` | | Password or app token. |
