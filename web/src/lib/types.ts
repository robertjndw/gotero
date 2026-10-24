// Mirrors docs/dev/admin-api.md. Change the contract first.

export type User = {
  id: number
  username: string
  displayName: string
  email: string
  emailVerified: boolean
  isAdmin: boolean
  disabled: boolean
  quotaBytes: number | null
  usedBytes: number
  createdAt: string
  lastSyncAt: string | null
  hasPassword: boolean
  providers: string[]
}
export type Identity = {
  provider: string
  providerName: string
  // Empty for a provider that was removed from the config.
  kind: 'oidc' | 'saml' | ''
  email: string
  createdAt: string
  lastLoginAt: string | null
}
export type SsoProvider = {
  id: string
  kind: 'oidc' | 'saml'
  displayName: string
  startUrl: string
}
export type Me = User & { csrfToken: string; identities: Identity[] }

export type ApiKeyAccess = {
  library: boolean
  notes: boolean
  write: boolean
  files: boolean
  groups: 'all' | 'none' | 'read'
}
export type ApiKey = {
  id: number
  name: string
  prefix: string
  access: ApiKeyAccess
  createdAt: string
  lastUsedAt: string | null
}
export type CreatedApiKey = ApiKey & { key: string }

export type Invite = {
  id: number
  url: string
  email: string | null
  isAdmin: boolean
  createdBy: string
  createdAt: string
  expiresAt: string
  usedBy: string | null
}

export type GroupRole = 'owner' | 'admin' | 'member'
export type LibraryReading = 'all' | 'members'
export type LibraryEditing = 'admins' | 'members'
export type FileEditing = 'none' | 'admins' | 'members'
export type Group = {
  id: number
  name: string
  description: string
  owner: { id: number; username: string }
  libraryReading: LibraryReading
  libraryEditing: LibraryEditing
  fileEditing: FileEditing
  memberCount: number
  usedBytes: number
  version: number
  createdAt: string
}
export type MyGroup = Group & { role: GroupRole }
export type GroupInput = {
  name: string
  description: string
  libraryReading: LibraryReading
  libraryEditing: LibraryEditing
  fileEditing: FileEditing
}
export type GroupMember = { userId: number; username: string; displayName: string; role: GroupRole }
export type UserLookup = { id: number; username: string; displayName: string }

export type LoginSession = {
  token: string
  status: 'pending' | 'completed' | 'cancelled' | 'expired'
  requiredUserId: number | null
  clientName: string
  createdAt: string
  expiresAt: string
}

export type Stats = {
  users: number
  groups: number
  items: number
  attachments: number
  storage: {
    backend: 'local' | 's3' | 'webdav'
    usedBytes: number
    blobCount: number
    orphanBytes: number
  }
  version: string
}

export type ImportJob = {
  id: number
  status: 'queued' | 'running' | 'completed' | 'failed' | 'cancelled'
  source: 'zotero.org'
  libraries: {
    name: string
    items: number
    files: number
    done: boolean
    conflicts?: number
    error?: string
  }[]
  progress: {
    objects: number
    objectsTotal: number
    files: number
    filesTotal: number
    bytes: number
  }
  error: string | null
  createdAt: string
  finishedAt: string | null
}

export type Setup = {
  openRegistration: boolean
  publicUrl: string
  version: string
  localLogin: boolean
  providers: SsoProvider[]
}
export type AdminSettings = { openRegistration: boolean; defaultQuotaBytes: number | null }
export type GcResult = { orphanBlobs: number; orphanBytes: number; deleted: boolean }
export type Page<T> = { items: T[]; nextCursor: string | null }
