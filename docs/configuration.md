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
| `GOTERO_LOCAL_LOGIN` | `true` | Allow username/password sign-in. With `false`, only instance admins can still sign in with a password (so you can't lock yourself out); everyone else uses single sign-on, and password signup is off. Needs at least one SSO provider. |

## Single sign-on (OIDC and SAML)

Users can sign in through OpenID Connect or SAML 2.0 identity providers in addition to, or instead of, passwords. You can configure several providers of either kind. Each provider gets a short name (lowercase letters and digits) that appears in its variable names and in its callback URLs, so don't rename it once users have linked accounts.

`GOTERO_PUBLIC_URL` is required as soon as a provider is configured, because the callback URLs you register at the provider are built from it.

**How sign-in maps to accounts.** On each SSO sign-in, gotero looks for an account in this order:
1. An account already linked to this provider account.
2. With `LINK_BY_EMAIL=true`: an existing account with the same, verified email. It gets linked. Admin accounts are never linked this way.
3. An invite link the user started from. The invite's email, if set, must match.
4. With `AUTO_PROVISION=true`: a new account. The username comes from the provider (or the email), with a number appended if it's taken.

Otherwise sign-in is refused. Users can also connect a provider to their existing account from the account page.

**Admin rights never come from the identity provider.** SSO never makes anyone an admin or removes admin rights. Accounts created through SSO are regular users unless they come from an admin invite. Admins are managed as before: the bootstrap admin above, the admin UI, `gotero user` and admin invites. An admin who wants to use SSO signs in with their password and connects the provider on the account page.

| Variable | Default | Description |
|---|---|---|
| `GOTERO_OIDC_PROVIDERS` | (none) | Comma-separated names of OIDC providers, e.g. `keycloak,google`. |
| `GOTERO_SAML_PROVIDERS` | (none) | Comma-separated names of SAML providers. Names must be unique across both lists. |

### OIDC

Register gotero as a confidential (or public) client with the redirect URI `{GOTERO_PUBLIC_URL}/auth/oidc/<name>/callback`. gotero always uses the authorization code flow with PKCE. Variables, with `<NAME>` being the provider name in upper case:

| Variable | Default | Description |
|---|---|---|
| `GOTERO_OIDC_<NAME>_ISSUER` | (required) | Issuer URL, e.g. `https://keycloak.example.org/realms/main`. Discovery is fetched from it on first use. |
| `GOTERO_OIDC_<NAME>_CLIENT_ID` | (required) | Client ID. |
| `GOTERO_OIDC_<NAME>_CLIENT_SECRET` | (none) | Client secret. Leave empty for a public client. |
| `GOTERO_OIDC_<NAME>_DISPLAY_NAME` | the name | Button label on the login page. |
| `GOTERO_OIDC_<NAME>_SCOPES` | `openid,email,profile` | Requested scopes. `openid` is always added. |
| `GOTERO_OIDC_<NAME>_USERNAME_CLAIM` | `preferred_username` | Claim used to suggest a username for new accounts. |
| `GOTERO_OIDC_<NAME>_AUTO_PROVISION` | `false` | Create accounts for unknown users. |
| `GOTERO_OIDC_<NAME>_LINK_BY_EMAIL` | `false` | Link to an existing non-admin account with the same verified email. Only enable this if the provider verifies email addresses. |
| `GOTERO_OIDC_<NAME>_TRUST_EMAIL` | `false` | Treat the email claim as verified even without `email_verified` (Microsoft Entra ID never sends it). |

Example for Keycloak:

```sh
GOTERO_PUBLIC_URL=https://zotero.example.org
GOTERO_OIDC_PROVIDERS=keycloak
GOTERO_OIDC_KEYCLOAK_ISSUER=https://keycloak.example.org/realms/main
GOTERO_OIDC_KEYCLOAK_CLIENT_ID=gotero
GOTERO_OIDC_KEYCLOAK_CLIENT_SECRET=...
GOTERO_OIDC_KEYCLOAK_DISPLAY_NAME=Company login
GOTERO_OIDC_KEYCLOAK_AUTO_PROVISION=true
```

### SAML

gotero is a SAML service provider. Its metadata is served at `{GOTERO_PUBLIC_URL}/auth/saml/<name>/metadata`; give that URL (or the downloaded file) to your identity provider. The assertion consumer service is `{GOTERO_PUBLIC_URL}/auth/saml/<name>/acs` (HTTP-POST binding). Only SP-initiated sign-in is supported.

gotero needs one key pair, shared by all SAML providers. Its certificate is published in the metadata, and providers that encrypt assertions encrypt them to it. Create one with:

```sh
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj "/CN=gotero" -keyout sp.key -out sp.crt
```

| Variable | Default | Description |
|---|---|---|
| `GOTERO_SAML_SP_CERT_FILE` | (required with SAML) | Path to the SP certificate (PEM). |
| `GOTERO_SAML_SP_KEY_FILE` | (required with SAML) | Path to the SP private key (PEM). |
| `GOTERO_SAML_<NAME>_IDP_METADATA_URL` | | URL of the identity provider's metadata. Refreshed daily. |
| `GOTERO_SAML_<NAME>_IDP_METADATA_FILE` | | Path to the metadata instead. Set exactly one of the two. |
| `GOTERO_SAML_<NAME>_ENTITY_ID` | the metadata URL | SP entity ID, if your provider needs a specific one. |
| `GOTERO_SAML_<NAME>_DISPLAY_NAME` | the name | Button label on the login page. |
| `GOTERO_SAML_<NAME>_SUBJECT_ATTRIBUTE` | (NameID) | Attribute that identifies the user. By default the persistent NameID is used; transient NameIDs are rejected unless this is set. |
| `GOTERO_SAML_<NAME>_EMAIL_ATTRIBUTE` | `email` | Email attribute. The standard `mail` OID and the Microsoft claim URI are tried as fallbacks. |
| `GOTERO_SAML_<NAME>_USERNAME_ATTRIBUTE` | `uid` | Attribute used to suggest a username. |
| `GOTERO_SAML_<NAME>_NAME_ATTRIBUTE` | `displayName` | Attribute used for the display name. |
| `GOTERO_SAML_<NAME>_AUTO_PROVISION` | `false` | Create accounts for unknown users. |
| `GOTERO_SAML_<NAME>_LINK_BY_EMAIL` | `false` | Link to an existing non-admin account with the same email. SAML has no "verified" flag, so enabling this means you trust the provider's email addresses. |

### Zotero clients and SSO

The Zotero desktop app's browser login works with SSO: the login page it opens offers the SSO buttons. The older username/password login inside Zotero only works for accounts that have a password, and with `GOTERO_LOCAL_LOGIN=false` only for admins.

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
