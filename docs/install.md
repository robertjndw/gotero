# Installation

gotero needs PostgreSQL (version 14 or newer) and somewhere to keep files. The Docker Compose setup brings both.

## Docker Compose (recommended)

```sh
git clone https://github.com/robertjndw/gotero && cd gotero
cp deploy/.env.example deploy/.env
```

Edit `deploy/.env`:

```sh
GOTERO_SECRET=<output of: openssl rand -hex 32>
GOTERO_PUBLIC_URL=https://zotero.example.org
GOTERO_ADMIN_USERNAME=admin
GOTERO_ADMIN_PASSWORD=<a strong password>
POSTGRES_PASSWORD=<another strong password>
```

Start it:

```sh
docker compose -f deploy/compose.yaml up -d
docker compose -f deploy/compose.yaml logs -f gotero   # wait for "listening"
```

The compose file builds the image from source the first time. To use the published image instead, run `docker compose -f deploy/compose.yaml pull` first; set `GOTERO_TAG` in `.env` to pin a version.

To try the S3 or WebDAV backends locally, add `--profile s3` or `--profile webdav` and set `GOTERO_STORAGE_BACKEND` (see [File storage](storage.md)).

## Plain Docker

```sh
docker run -d --name gotero -p 8080:8080 \
  -e GOTERO_DATABASE_URL='postgres://gotero:secret@db-host:5432/gotero' \
  -e GOTERO_SECRET="$(openssl rand -hex 32)" \
  -e GOTERO_PUBLIC_URL=https://zotero.example.org \
  -e GOTERO_ADMIN_USERNAME=admin -e GOTERO_ADMIN_PASSWORD='change me please' \
  -v gotero-files:/data \
  ghcr.io/robertjndw/gotero:latest
```

The image is distroless, runs as a non-root user and is available for `linux/amd64` and `linux/arm64`. Migrations run automatically on startup.

## HTTPS and reverse proxies

Zotero only accepts certificates it trusts, so run gotero behind a reverse proxy with a real certificate, such as one from Let's Encrypt. Set `GOTERO_PUBLIC_URL` to the public `https://` address, and set `GOTERO_TRUSTED_PROXIES` to the proxy's address so that login rate limits see each user's real IP. The proxy must:

- pass WebSocket upgrades on `/stream`;
- allow large request bodies on `/files/upload/` (attachments), or have no limit;
- not buffer uploads or downloads, so large files stream straight through.

### Caddy

```caddy
zotero.example.org {
	reverse_proxy gotero:8080
}
```

Caddy handles certificates, WebSockets and large bodies out of the box.

### nginx

```nginx
server {
    listen 443 ssl;
    server_name zotero.example.org;
    # ssl_certificate ...;

    client_max_body_size 0;          # attachments can be large
    proxy_request_buffering off;     # stream uploads straight through
    proxy_buffering off;             # stream downloads straight through

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_read_timeout 1h;       # keeps /stream connections open
    }
}

map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}
```

## Upgrades

```sh
docker compose -f deploy/compose.yaml pull     # or: git pull && docker compose ... build
docker compose -f deploy/compose.yaml up -d
```

Database migrations run on startup. Read the release notes before jumping several versions.

## Backups

Back up both of these, the database first:

1. **The database:**
   ```sh
   docker compose -f deploy/compose.yaml exec postgres pg_dump -U gotero gotero > gotero.sql
   ```
2. **The files:** the `gotero-files` volume for the local backend, or your bucket or WebDAV folder. For the volume:
   ```sh
   docker run --rm -v gotero_gotero-files:/data -v "$PWD":/backup alpine tar czf /backup/files.tgz -C /data .
   ```

Both can be taken while gotero is running, as long as the files are backed up after the database and within 24 hours of it. Files are never changed in place, and gotero deletes unused ones only after a 24-hour grace period, so such a file backup holds every file the database backup refers to. A file backup taken before the database backup can miss files uploaded in between. With S3 or WebDAV, schedule the bucket or folder backup to run after the database dump.

### Restoring

1. Stop gotero and bring up only the database:
   ```sh
   docker compose -f deploy/compose.yaml stop gotero
   docker compose -f deploy/compose.yaml up -d --wait postgres
   ```
2. Replace the database with the backup:
   ```sh
   docker compose -f deploy/compose.yaml exec -T postgres psql -U gotero -d postgres \
     -c 'DROP DATABASE gotero WITH (FORCE)' -c 'CREATE DATABASE gotero OWNER gotero'
   docker compose -f deploy/compose.yaml exec -T postgres psql -q -v ON_ERROR_STOP=1 -U gotero gotero < gotero.sql
   ```
3. Put the files back (the image runs as user 65532):
   ```sh
   docker run --rm -v gotero_gotero-files:/data -v "$PWD":/backup alpine \
     sh -c 'tar xzf /backup/files.tgz -C /data && chown -R 65532:65532 /data'
   ```
4. Start gotero: `docker compose -f deploy/compose.yaml up -d`.

`scripts/ops-test.sh` tests these backups: it restores them into new containers after a total loss and checks that simulated Zotero clients carry on without noticing.

Restoring a database that is *older* than what clients have already seen makes those clients refuse to sync. In that case, have them reset their sync history (**Settings → Sync → Reset**).
