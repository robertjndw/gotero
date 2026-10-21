import {
  keepPreviousData,
  queryOptions,
  useMutation,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query'
import { api, ApiError, setCsrfToken } from './api'
import type {
  AdminSettings,
  ApiKey,
  ApiKeyAccess,
  CreatedApiKey,
  GcResult,
  Group,
  GroupInput,
  GroupMember,
  GroupRole,
  ImportJob,
  Invite,
  LoginSession,
  Me,
  MyGroup,
  Page,
  Setup,
  Stats,
  User,
  UserLookup,
} from './types'

export const qk = {
  me: ['me'] as const,
  setup: ['setup'] as const,
  keys: ['me', 'keys'] as const,
  myGroups: ['me', 'groups'] as const,
  stats: ['admin', 'stats'] as const,
  users: (q: string) => ['admin', 'users', q] as const,
  invites: ['admin', 'invites'] as const,
  settings: ['admin', 'settings'] as const,
  groups: ['groups'] as const,
  group: (id: number) => ['groups', id] as const,
  members: (id: number) => ['groups', id, 'members'] as const,
  lookup: (q: string) => ['lookup', q] as const,
  imports: ['imports'] as const,
  loginSession: (t: string) => ['login-session', t] as const,
  invite: (t: string) => ['invite', t] as const,
}

export const meQuery = queryOptions({
  queryKey: qk.me,
  queryFn: () => api<Me>('/me'),
  retry: false,
  staleTime: 60_000,
})
export const setupQuery = queryOptions({
  queryKey: qk.setup,
  queryFn: () => api<Setup>('/setup'),
  staleTime: 5 * 60_000,
})

export const useMe = () => useQuery(meQuery)
export const useSetup = () => useQuery(setupQuery)

export function useLogin() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (b: { username: string; password: string }) =>
      api<Me>('/login', { method: 'POST', body: b }),
    onSuccess: (me) => qc.setQueryData(qk.me, me),
  })
}
export function useLogout() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => api<void>('/logout', { method: 'POST' }),
    onSettled: () => {
      setCsrfToken(null)
      qc.clear()
    },
  })
}
export function useRegister() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (b: {
      username: string
      password: string
      email: string
      displayName: string
      inviteToken?: string
    }) => api<Me>('/register', { method: 'POST', body: b }),
    onSuccess: (me) => qc.setQueryData(qk.me, me),
  })
}
export const useInvite = (token: string | undefined) =>
  useQuery({
    queryKey: qk.invite(token ?? ''),
    queryFn: () =>
      api<{ valid: boolean; email: string | null }>(`/invites/${encodeURIComponent(token!)}`),
    enabled: !!token,
    retry: false,
  })

export function useUpdateProfile() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (b: { displayName?: string; email?: string }) =>
      api<Me>('/me', { method: 'PATCH', body: b }),
    onSuccess: (me) => qc.setQueryData(qk.me, me),
  })
}
export const useChangePassword = () =>
  useMutation({
    mutationFn: (b: { currentPassword: string; newPassword: string }) =>
      api<void>('/me/password', { method: 'POST', body: b }),
  })

export const useKeys = () =>
  useQuery({
    queryKey: qk.keys,
    queryFn: () => api<{ items: ApiKey[] }>('/me/keys').then((r) => r.items),
  })
export function useCreateKey() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (b: { name: string; access: ApiKeyAccess }) =>
      api<CreatedApiKey>('/me/keys', { method: 'POST', body: b }),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.keys }),
  })
}
export function useRevokeKey() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: number) => api<void>(`/me/keys/${id}`, { method: 'DELETE' }),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.keys }),
  })
}
export const useMyGroups = () =>
  useQuery({
    queryKey: qk.myGroups,
    queryFn: () => api<{ items: MyGroup[] }>('/me/groups').then((r) => r.items),
  })

// Login sessions (Zotero client approval)
export const useLoginSession = (token: string | undefined, enabled: boolean) =>
  useQuery({
    queryKey: qk.loginSession(token ?? ''),
    queryFn: () => api<LoginSession>(`/login-sessions/${encodeURIComponent(token!)}`),
    enabled: !!token && enabled,
    retry: false,
  })
export function useResolveLoginSession(token: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (action: 'approve' | 'cancel') =>
      api<LoginSession>(`/login-sessions/${encodeURIComponent(token)}/${action}`, {
        method: 'POST',
      }),
    onSuccess: (s) => qc.setQueryData(qk.loginSession(token), s),
  })
}

// Admin
export const useStats = (enabled = true) =>
  useQuery({ queryKey: qk.stats, queryFn: () => api<Stats>('/admin/stats'), enabled })
export const useAdminUsers = (q: string) =>
  useQuery({
    queryKey: qk.users(q),
    queryFn: () => api<Page<User>>('/admin/users', { query: { q, limit: 100 } }),
    placeholderData: keepPreviousData,
  })
export type UserCreate = {
  username: string
  password: string
  email: string
  displayName: string
  isAdmin: boolean
  quotaBytes: number | null
}
export function useCreateUser() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (b: UserCreate) => api<User>('/admin/users', { method: 'POST', body: b }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['admin', 'users'] }),
  })
}
export type UserPatch = Partial<
  Pick<User, 'displayName' | 'email' | 'isAdmin' | 'disabled' | 'quotaBytes'> & { password: string }
>
export function useUpdateUser() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, ...b }: UserPatch & { id: number }) =>
      api<User>(`/admin/users/${id}`, { method: 'PATCH', body: b }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['admin', 'users'] }),
  })
}
export function useDeleteUser() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: number) => api<void>(`/admin/users/${id}`, { method: 'DELETE' }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['admin', 'users'] }),
  })
}
export const useInvites = () =>
  useQuery({
    queryKey: qk.invites,
    queryFn: () => api<Page<Invite>>('/admin/invites', { query: { limit: 100 } }),
  })
export function useCreateInvite() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (b: { email?: string; isAdmin: boolean; expiresInHours: number }) =>
      api<Invite>('/admin/invites', { method: 'POST', body: b }),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.invites }),
  })
}
export function useRevokeInvite() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: number) => api<void>(`/admin/invites/${id}`, { method: 'DELETE' }),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.invites }),
  })
}
export const useAdminSettings = () =>
  useQuery({ queryKey: qk.settings, queryFn: () => api<AdminSettings>('/admin/settings') })
export function useUpdateSettings() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (b: Partial<AdminSettings>) =>
      api<AdminSettings>('/admin/settings', { method: 'PATCH', body: b }),
    onSuccess: (s) => {
      qc.setQueryData(qk.settings, s)
      qc.invalidateQueries({ queryKey: qk.setup })
    },
  })
}
export const useGc = () =>
  useMutation({
    mutationFn: (dryRun: boolean) =>
      api<GcResult>('/admin/gc', { method: 'POST', body: { dryRun } }),
  })

// Groups
export const useGroups = () =>
  useQuery({
    queryKey: qk.groups,
    queryFn: () => api<Page<Group>>('/groups', { query: { limit: 100 } }),
  })
export const useGroup = (id: number) =>
  useQuery({ queryKey: qk.group(id), queryFn: () => api<Group>(`/groups/${id}`) })
export function useCreateGroup() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (b: GroupInput) => api<Group>('/groups', { method: 'POST', body: b }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.groups })
      qc.invalidateQueries({ queryKey: qk.myGroups })
    },
  })
}
export function useUpdateGroup(id: number) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (b: Partial<GroupInput>) =>
      api<Group>(`/groups/${id}`, { method: 'PATCH', body: b }),
    onSuccess: (g) => {
      qc.setQueryData(qk.group(id), g)
      qc.invalidateQueries({ queryKey: qk.groups, exact: true })
      qc.invalidateQueries({ queryKey: qk.myGroups })
    },
  })
}
export function useDeleteGroup() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: number) => api<void>(`/groups/${id}`, { method: 'DELETE' }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.groups })
      qc.invalidateQueries({ queryKey: qk.myGroups })
    },
  })
}
export const useMembers = (id: number) =>
  useQuery({
    queryKey: qk.members(id),
    queryFn: () => api<{ items: GroupMember[] }>(`/groups/${id}/members`).then((r) => r.items),
  })
export function useSetMember(id: number) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (b: { userId: number; role: GroupRole }) =>
      api<GroupMember>(`/groups/${id}/members/${b.userId}`, {
        method: 'PUT',
        body: { role: b.role },
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.members(id) })
      qc.invalidateQueries({ queryKey: qk.group(id), exact: true })
    },
  })
}
export function useRemoveMember(id: number) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (userId: number) =>
      api<void>(`/groups/${id}/members/${userId}`, { method: 'DELETE' }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.members(id) })
      qc.invalidateQueries({ queryKey: qk.group(id), exact: true })
    },
  })
}
export const useUserLookup = (q: string) =>
  useQuery({
    queryKey: qk.lookup(q),
    queryFn: () =>
      api<{ items: UserLookup[] }>('/users/lookup', { query: { q } }).then((r) => r.items),
    enabled: q.trim().length >= 2,
  })

// Imports
export const useImports = () =>
  useQuery({
    queryKey: qk.imports,
    queryFn: () => api<{ items: ImportJob[] }>('/imports').then((r) => r.items),
    // Poll only while something is in flight so an idle page stays quiet.
    refetchInterval: (q) =>
      q.state.data?.some((j) => j.status === 'queued' || j.status === 'running') ? 2000 : false,
  })
export function useStartImport() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (b: { apiKey: string; includeGroups: boolean; includeFiles: boolean }) =>
      api<ImportJob>('/imports', { method: 'POST', body: b }),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.imports }),
  })
}
export function useCancelImport() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: number) => api<ImportJob>(`/imports/${id}/cancel`, { method: 'POST' }),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.imports }),
  })
}

export const errorMessage = (e: unknown) =>
  e instanceof ApiError ? e.message : 'Something went wrong.'
