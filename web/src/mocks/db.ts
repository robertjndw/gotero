import type {
  ApiKey,
  Group,
  GroupMember,
  Identity,
  ImportJob,
  Invite,
  LoginSession,
  SsoProvider,
  User,
} from '@/lib/types'

const GB = 1024 ** 3
const MB = 1024 ** 2
const ago = (ms: number) => new Date(Date.now() - ms).toISOString()
const ahead = (ms: number) => new Date(Date.now() + ms).toISOString()
const HOUR = 3600_000
const DAY = 24 * HOUR

export type DbState = {
  sessionUserId: number | null
  csrf: string
  passwords: Record<number, string>
  identities: Record<number, Identity[]>
  providers: SsoProvider[]
  localLogin: boolean
  users: User[]
  keys: Record<number, ApiKey[]>
  groups: Group[]
  members: Record<number, GroupMember[]>
  invites: Invite[]
  loginSessions: LoginSession[]
  imports: ImportJob[]
  settings: { openRegistration: boolean; defaultQuotaBytes: number | null }
  publicUrl: string
  nextId: number
}

const keycloakIdentity: Identity = {
  provider: 'keycloak',
  providerName: 'Keycloak',
  kind: 'oidc',
  email: '',
  createdAt: ago(20 * DAY),
  lastLoginAt: ago(2 * DAY),
}

function seed(): DbState {
  const users: User[] = [
    {
      id: 1,
      username: 'admin',
      displayName: 'Ada Admin',
      email: 'ada@example.org',
      isAdmin: true,
      disabled: false,
      quotaBytes: null,
      usedBytes: 2.4 * GB,
      createdAt: ago(90 * DAY),
      lastSyncAt: ago(5 * 60_000),
      hasPassword: true,
      providers: [],
    },
    {
      id: 2,
      username: 'alice',
      displayName: 'Alice Archer',
      email: 'alice@example.org',
      isAdmin: false,
      disabled: false,
      quotaBytes: 5 * GB,
      usedBytes: 1.2 * GB,
      createdAt: ago(40 * DAY),
      lastSyncAt: ago(3 * HOUR),
      hasPassword: true,
      providers: ['keycloak'],
    },
    {
      id: 3,
      username: 'bob',
      displayName: 'Bob Bishop',
      email: 'bob@example.org',
      isAdmin: false,
      disabled: false,
      quotaBytes: 2 * GB,
      usedBytes: 310 * MB,
      createdAt: ago(12 * DAY),
      lastSyncAt: null,
      hasPassword: false,
      providers: ['keycloak'],
    },
  ]
  const groups: Group[] = [
    {
      id: 10,
      name: 'Reading Group',
      description: 'Weekly paper discussions',
      owner: { id: 1, username: 'admin' },
      libraryReading: 'members',
      libraryEditing: 'members',
      fileEditing: 'members',
      memberCount: 3,
      usedBytes: 420 * MB,
      version: 12,
      createdAt: ago(30 * DAY),
    },
    {
      id: 11,
      name: 'Thesis Sources',
      description: '',
      owner: { id: 2, username: 'alice' },
      libraryReading: 'all',
      libraryEditing: 'admins',
      fileEditing: 'admins',
      memberCount: 1,
      usedBytes: 12 * MB,
      version: 3,
      createdAt: ago(8 * DAY),
    },
  ]
  return {
    sessionUserId: null,
    csrf: 'csrf-mock-token',
    // Bob is SSO-only: no entry here and hasPassword false.
    passwords: { 1: 'password', 2: 'password' },
    identities: {
      1: [],
      2: [{ ...keycloakIdentity, email: 'alice@example.org' }],
      3: [{ ...keycloakIdentity, email: 'bob@example.org' }],
    },
    providers: [
      {
        id: 'keycloak',
        kind: 'oidc',
        displayName: 'Keycloak',
        startUrl: '/auth/oidc/keycloak/start',
      },
      {
        id: 'corp-saml',
        kind: 'saml',
        displayName: 'Corp SAML',
        startUrl: '/auth/saml/corp-saml/start',
      },
    ],
    localLogin: true,
    users,
    keys: {
      1: [
        {
          id: 1,
          name: 'Laptop',
          prefix: 'aB3d',
          access: { library: true, notes: true, write: true, files: true, groups: 'all' },
          createdAt: ago(20 * DAY),
          lastUsedAt: ago(2 * HOUR),
        },
      ],
      2: [],
      3: [],
    },
    groups,
    members: {
      10: [
        { userId: 1, username: 'admin', displayName: 'Ada Admin', role: 'owner' },
        { userId: 2, username: 'alice', displayName: 'Alice Archer', role: 'admin' },
        { userId: 3, username: 'bob', displayName: 'Bob Bishop', role: 'member' },
      ],
      11: [{ userId: 2, username: 'alice', displayName: 'Alice Archer', role: 'owner' }],
    },
    invites: [
      {
        id: 1,
        url: 'https://zotero.example.org/register?invite=inv_abc123',
        email: 'carol@example.org',
        isAdmin: false,
        createdBy: 'admin',
        createdAt: ago(DAY),
        expiresAt: ahead(6 * DAY),
        usedBy: null,
      },
      {
        id: 2,
        url: 'https://zotero.example.org/register?invite=inv_used99',
        email: null,
        isAdmin: false,
        createdBy: 'admin',
        createdAt: ago(10 * DAY),
        expiresAt: ago(3 * DAY),
        usedBy: 'bob',
      },
    ],
    loginSessions: [
      {
        token: 'good',
        status: 'pending',
        requiredUserId: null,
        clientName: "Zotero 7.1 on Ada's MacBook",
        createdAt: ago(60_000),
        expiresAt: ahead(10 * 60_000),
      },
      {
        token: 'bound',
        status: 'pending',
        requiredUserId: 2,
        clientName: 'Zotero 7.1 on Linux',
        createdAt: ago(60_000),
        expiresAt: ahead(10 * 60_000),
      },
      {
        token: 'old',
        status: 'expired',
        requiredUserId: null,
        clientName: 'Zotero 7.1',
        createdAt: ago(DAY),
        expiresAt: ago(DAY - 600_000),
      },
    ],
    imports: [],
    settings: { openRegistration: false, defaultQuotaBytes: 5 * GB },
    publicUrl:
      typeof window !== 'undefined' ? window.location.origin : 'https://zotero.example.org',
    nextId: 100,
  }
}

export const db: DbState = seed()

export function resetDb() {
  Object.assign(db, seed())
  try {
    sessionStorage.removeItem('gotero-mock-session')
  } catch {
    // no storage in this environment
  }
}

const SESSION_KEY = 'gotero-mock-session'
// Dev only: survives page reloads so the mock behaves like a cookie.
export function restoreSession() {
  try {
    const v = sessionStorage.getItem(SESSION_KEY)
    if (v) db.sessionUserId = Number(v)
  } catch {
    // ignore
  }
}
export function persistSession() {
  try {
    if (db.sessionUserId) sessionStorage.setItem(SESSION_KEY, String(db.sessionUserId))
    else sessionStorage.removeItem(SESSION_KEY)
  } catch {
    // ignore
  }
}
