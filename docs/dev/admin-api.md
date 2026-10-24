# Admin API contract

This is the API the web UI uses. It is separate from the Zotero API and is mounted at `/admin/api`. The server (`internal/adminapi`) and the UI (`web/`) both implement against this file, so change it first when either needs something new.

## Conventions
- All bodies are JSON, and timestamps are RFC 3339 UTC strings.
- **Authentication:**
  - Logging in sets an HttpOnly session cookie `gotero_session`. It uses `SameSite=Lax`, and `Secure` when the public URL is https.
  - Every response for a logged-in user carries an `X-CSRF-Token` header, which is also returned by `GET /me`.
  - Every non-GET request must echo it back in an `X-CSRF-Token` request header and send an `Origin` matching the public URL. Otherwise it gets 403.
- **Errors** look like `{"error": {"code": "string_code", "message": "Human readable"}}`.
  - 401 means not logged in, and 403 means not allowed or a CSRF failure.
  - 404, 409 (conflict, e.g. a username that is already taken) and 422 (validation, may include `"fields": {"name": "message"}`).
- **Pagination:** list endpoints accept `?limit=50&cursor=...` and return `{"items": [...], "nextCursor": "..." | null}`.
- **Sizes** are bytes (int64), and `null` quota means unlimited.
- **Token values:**
  - Invite links have the form `{publicUrl}/register?invite=TOKEN`, and `TOKEN` is the value accepted by `GET /invites/{token}` and `POST /register`.
  - Login-session tokens are the `session` query value from the Zotero client's `loginURL`.
- **Expiry is decided by the server:** a pending login session past `expiresAt` is reported with status `expired`.
- **Local login:** when `GOTERO_LOCAL_LOGIN=false`, password sign-in only works for instance admins. For everyone else a correct password gets the same 401 as a wrong one, so the response never confirms a password. Password signup and password changes for non-admins get 403 `local_login_disabled`.
- **Self-protection:** an instance admin cannot delete, disable or demote themselves, and the last remaining admin cannot be removed. Both get 409 `last_admin` or `self_modification`.

## Types
```ts
type User = {
  id: number; username: string; displayName: string; email: string;
  emailVerified: boolean;                            // set by an admin, the CLI, an invite or an IdP; only these link by email
  isAdmin: boolean; disabled: boolean;
  quotaBytes: number | null; usedBytes: number;
  createdAt: string; lastSyncAt: string | null;
  hasPassword: boolean;                              // false for SSO-only accounts
  providers: string[];                               // ids of linked SSO providers
};
type Identity = {                                    // a linked SSO login
  provider: string; providerName: string; kind: "oidc" | "saml";
  email: string; createdAt: string; lastLoginAt: string | null;
};
type SsoProvider = { id: string; kind: "oidc" | "saml"; displayName: string; startUrl: string };
type Me = User & { csrfToken: string; identities: Identity[] };
type ApiKey = {
  id: number; name: string; prefix: string;           // first 4 chars, for recognition
  access: { library: boolean; notes: boolean; write: boolean; files: boolean; groups: "all" | "none" | "read" };
  createdAt: string; lastUsedAt: string | null;
};
type Invite = {
  id: number; url: string; email: string | null; isAdmin: boolean;
  createdBy: string; createdAt: string; expiresAt: string; usedBy: string | null;
};
type GroupRole = "owner" | "admin" | "member";
type Group = {
  id: number; name: string; description: string; owner: { id: number; username: string };
  libraryReading: "all" | "members"; libraryEditing: "admins" | "members"; fileEditing: "none" | "admins" | "members";
  memberCount: number; usedBytes: number; version: number; createdAt: string;
};
type GroupMember = { userId: number; username: string; displayName: string; role: GroupRole };
type LoginSession = {                               // a Zotero client asking for a key
  token: string; status: "pending" | "completed" | "cancelled" | "expired";
  requiredUserId: number | null;                    // set when the client is already linked to an account
  clientName: string; createdAt: string; expiresAt: string;
};
type Stats = {
  users: number; groups: number; items: number; attachments: number;
  storage: { backend: "local" | "s3" | "webdav"; usedBytes: number; blobCount: number; orphanBytes: number };
  version: string;
};
type ImportJob = {
  id: number; status: "queued" | "running" | "completed" | "failed" | "cancelled";
  source: "zotero.org"; libraries: { name: string; items: number; files: number; done: boolean; conflicts: number; error?: string }[];
  progress: { objects: number; objectsTotal: number; files: number; filesTotal: number; bytes: number };
  error: string | null; createdAt: string; finishedAt: string | null;
};
```

## Endpoints

### Session
| Method | Path | Body | Response |
|---|---|---|---|
| POST | `/login` | `{username, password}` | `Me` (sets the cookie). 401 on bad credentials. |
| POST | `/logout` | | 204 |
| GET | `/me` | | `Me` |
| GET | `/setup` | (public) | `{openRegistration: boolean, publicUrl: string, version: string, localLogin: boolean, providers: SsoProvider[]}` |
| POST | `/register` | `{username, password, email, displayName, inviteToken?}` (public) | `Me`. Needs an invite token unless open registration is on. 403 `local_login_disabled` when local login is off. |
| GET | `/invites/{token}` | (public) | `{valid: boolean, email: string \| null}` |

### Account (current user)
| Method | Path | Body | Response |
|---|---|---|---|
| PATCH | `/me` | `{displayName?, email?}` | `Me` |
| POST | `/me/password` | `{currentPassword?, newPassword}` | 204. `currentPassword` may be omitted when `hasPassword` is false. 403 `local_login_disabled` for non-admins when local login is off. |
| DELETE | `/me/identities/{provider}` | | 204. 409 `last_login_method` if this would leave no way to sign in. |
| GET | `/me/keys` | | `{items: ApiKey[]}` |
| POST | `/me/keys` | `{name, access}` | `ApiKey & {key: string}` (the full key is shown once) |
| DELETE | `/me/keys/{id}` | | 204 |
| GET | `/me/groups` | | `{items: (Group & {role: GroupRole})[]}` |

### Client login approval (browser login flow of newer Zotero clients)
| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/login-sessions/{token}` | | `LoginSession` |
| POST | `/login-sessions/{token}/approve` | | `LoginSession`. 403 `user_mismatch` if `requiredUserId` is a different user. |
| POST | `/login-sessions/{token}/cancel` | | `LoginSession` |

The Zotero client opens `{publicUrl}/login?session=TOKEN`, a page served by the SPA. If needed the page logs the user in, then shows "Allow *clientName* to access your library?" with Approve and Deny.

### Administration (admin only)
| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/admin/stats` | | `Stats` |
| GET | `/admin/users` | `?q=` | page of `User` |
| POST | `/admin/users` | `{username, password, email, displayName, isAdmin, quotaBytes}` | `User` |
| PATCH | `/admin/users/{id}` | `{displayName?, email?, isAdmin?, disabled?, quotaBytes?, password?}` | `User` |
| DELETE | `/admin/users/{id}` | | 204 (deletes the user's library and files) |
| DELETE | `/admin/users/{id}/identities/{provider}` | | 204 |
| GET | `/admin/invites` | | page of `Invite` |
| POST | `/admin/invites` | `{email?, isAdmin, expiresInHours}` | `Invite` |
| DELETE | `/admin/invites/{id}` | | 204 |
| GET | `/admin/settings` | | `{openRegistration: boolean, defaultQuotaBytes: number \| null}` |
| PATCH | `/admin/settings` | same, partial | same |
| POST | `/admin/gc` | `{dryRun: boolean}` | `{orphanBlobs: number, orphanBytes: number, deleted: boolean}` |

### Groups (owner or admin of the group; instance admins can manage every group)
| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/groups` | | page of `Group` (instance admin: all groups; others: their own) |
| POST | `/groups` | `{name, description, libraryReading, libraryEditing, fileEditing}` | `Group` (the caller becomes owner) |
| GET | `/groups/{id}` | | `Group` |
| PATCH | `/groups/{id}` | partial of the create body, plus `ownerId?` | `Group` |
| DELETE | `/groups/{id}` | | 204 (owner or instance admin only) |
| GET | `/groups/{id}/members` | | `{items: GroupMember[]}` |
| PUT | `/groups/{id}/members/{userId}` | `{role}` | `GroupMember` |
| DELETE | `/groups/{id}/members/{userId}` | | 204 |
| GET | `/users/lookup` | `?q=` (any logged-in user, min 2 chars) | `{items: {id, username, displayName}[]}` (max 10) |

### Import from zotero.org
| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/imports` | | `{items: ImportJob[]}` (own jobs) |
| POST | `/imports` | `{apiKey, includeGroups: boolean, includeFiles: boolean}` | `ImportJob`. 422 if zotero.org rejects the key. |
| GET | `/imports/{id}` | | `ImportJob` |
| POST | `/imports/{id}/cancel` | | `ImportJob` |

## Single sign-on (outside `/admin/api`)

SSO endpoints live under `/auth` because their URLs are registered at the identity provider and they answer with browser redirects, not JSON. The SPA never calls them with `fetch`; it links to `startUrl` from `/setup` with a full page navigation.

| Method | Path | Notes |
|---|---|---|
| GET | `/auth/oidc/{provider}/start` | Redirects to the IdP. |
| GET | `/auth/oidc/{provider}/callback` | Redirect URI to register at the IdP. |
| POST | `/auth/oidc/{provider}/start` | Connects the provider to the logged-in account, then redirects to the IdP. |
| GET | `/auth/saml/{provider}/start` | Redirects to the IdP (HTTP-Redirect binding). |
| POST | `/auth/saml/{provider}/start` | Same as the OIDC POST. |
| POST | `/auth/saml/{provider}/acs` | Assertion consumer service (HTTP-POST binding). |
| GET | `/auth/saml/{provider}/finish` | Internal second hop after the ACS. |
| GET | `/auth/saml/{provider}/metadata` | SP metadata XML for the IdP. |

`GET start` accepts these query parameters:
- `invite=TOKEN`: sign up through an invite. The invite's email, if any, must match the IdP's verified email.
- `session=TOKEN`: a Zotero client login session; on success the browser lands on `/login?session=TOKEN` to approve it.
- `redirect=/path`: a same-origin path to land on after login. Anything not starting with a single `/` is ignored.

`POST start` connects the provider to the account that is currently logged in and lands on `/account`. It is a plain HTML form submission (`application/x-www-form-urlencoded`) that must come from this origin and carry the session's CSRF token in a `csrf` field. It cannot be a GET because the session cookie is `SameSite=Lax`, so a cross-site link would carry it. A `GET start` with `link` set is refused with `sso_state`.

On failure the browser is redirected to `/login?error=CODE` (`/register?invite=TOKEN&error=CODE` for invite signups, `/account?error=CODE` for `POST start`). Details are only logged server-side. Codes:

| Code | Meaning |
|---|---|
| `sso_failed` | The IdP returned an error or its response failed validation. |
| `sso_state` | The login attempt expired, was replayed, or was started in another browser. |
| `unknown_provider` | No provider with that id. |
| `no_account` | No linked account and the provider doesn't create accounts. |
| `disabled` | The account is disabled. |
| `invalid_invite` | The invite is unknown, used or expired. |
| `invite_email_mismatch` | The IdP's email doesn't match the invite. |
| `link_required` | An account with this email already exists; sign in to it and connect the provider from the account page. |
| `identity_in_use` | That IdP account is already linked to another user, or this user already has one for the provider. |

SSO never grants or removes instance admin rights. New accounts created through SSO are regular users unless they come from an admin invite, and email-based linking never attaches an IdP identity to an existing admin; admins connect providers explicitly with `POST start`.
