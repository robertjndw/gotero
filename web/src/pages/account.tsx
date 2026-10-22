import { useState } from 'react'
import { Link, useSearch } from '@tanstack/react-router'
import { useForm, Controller } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { toast } from 'sonner'
import { AlertTriangle, KeyRound, Plus, Trash2, Unlink } from 'lucide-react'
import {
  errorMessage,
  useChangePassword,
  useCreateKey,
  useKeys,
  useMe,
  useMyGroups,
  useRevokeKey,
  useSetup,
  useUnlinkIdentity,
  useUpdateProfile,
} from '@/lib/queries'
import type { ApiKey, ApiKeyAccess, CreatedApiKey, Identity } from '@/lib/types'
import { formatDate, relativeTime } from '@/lib/format'
import { applyServerError } from '@/lib/forms'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import {
  CodeLine,
  ConfirmDialog,
  EmptyState,
  ErrorState,
  Field,
  ListSkeleton,
  PageHeader,
} from '@/components/common'
import { SsoErrorAlert, ssoHref } from '@/components/sso'

const profileSchema = z.object({
  displayName: z.string().min(1, 'Enter a display name'),
  email: z.email('Enter a valid email address'),
})
type ProfileValues = z.infer<typeof profileSchema>

function ProfileCard() {
  const { data: me } = useMe()
  const update = useUpdateProfile()
  const {
    register,
    handleSubmit,
    setError,
    formState: { errors, isDirty },
    reset,
  } = useForm<ProfileValues>({
    resolver: zodResolver(profileSchema),
    values: { displayName: me?.displayName ?? '', email: me?.email ?? '' },
  })
  return (
    <Card>
      <CardHeader>
        <CardTitle>Profile</CardTitle>
        <CardDescription>Signed in as @{me?.username}.</CardDescription>
      </CardHeader>
      <form
        noValidate
        onSubmit={handleSubmit((v) =>
          update.mutate(v, {
            onSuccess: (m) => {
              toast.success('Profile updated')
              reset({ displayName: m.displayName, email: m.email })
            },
            onError: (e) => applyServerError(e, setError),
          }),
        )}
      >
        <CardContent className="grid gap-4 sm:grid-cols-2">
          <Field label="Display name" htmlFor="displayName" error={errors.displayName?.message}>
            <Input
              id="displayName"
              aria-invalid={!!errors.displayName}
              {...register('displayName')}
            />
          </Field>
          <Field label="Email" htmlFor="email" error={errors.email?.message}>
            <Input id="email" type="email" aria-invalid={!!errors.email} {...register('email')} />
          </Field>
        </CardContent>
        <CardFooter className="mt-6 justify-end">
          <Button type="submit" disabled={!isDirty || update.isPending}>
            Save changes
          </Button>
        </CardFooter>
      </form>
    </Card>
  )
}

const pwSchema = z
  .object({
    currentPassword: z.string().optional(),
    newPassword: z.string().min(8, 'At least 8 characters'),
    confirm: z.string(),
  })
  .refine((v) => v.newPassword === v.confirm, {
    path: ['confirm'],
    message: 'Passwords do not match',
  })
type PwValues = z.infer<typeof pwSchema>

function PasswordCard({ hasPassword }: { hasPassword: boolean }) {
  const change = useChangePassword()
  const {
    register,
    handleSubmit,
    setError,
    reset,
    formState: { errors },
  } = useForm<PwValues>({ resolver: zodResolver(pwSchema) })
  return (
    <Card>
      <CardHeader>
        <CardTitle>Password</CardTitle>
        <CardDescription>
          {hasPassword
            ? 'Choose a new password for this account.'
            : 'This account signs in through single sign-on. Set a password to also log in without it.'}
        </CardDescription>
      </CardHeader>
      <form
        noValidate
        onSubmit={handleSubmit((v) => {
          if (hasPassword && !v.currentPassword) {
            setError('currentPassword', { message: 'Enter your current password' })
            return
          }
          change.mutate(
            {
              ...(hasPassword ? { currentPassword: v.currentPassword } : {}),
              newPassword: v.newPassword,
            },
            {
              onSuccess: () => {
                toast.success(hasPassword ? 'Password changed' : 'Password set')
                reset()
              },
              onError: (e) => applyServerError(e, setError),
            },
          )
        })}
      >
        <CardContent className={`grid gap-4 ${hasPassword ? 'sm:grid-cols-3' : 'sm:grid-cols-2'}`}>
          {hasPassword && (
            <Field
              label="Current password"
              htmlFor="currentPassword"
              error={errors.currentPassword?.message}
            >
              <Input
                id="currentPassword"
                type="password"
                autoComplete="current-password"
                aria-invalid={!!errors.currentPassword}
                {...register('currentPassword')}
              />
            </Field>
          )}
          <Field label="New password" htmlFor="newPassword" error={errors.newPassword?.message}>
            <Input
              id="newPassword"
              type="password"
              autoComplete="new-password"
              aria-invalid={!!errors.newPassword}
              {...register('newPassword')}
            />
          </Field>
          <Field label="Confirm new password" htmlFor="confirm" error={errors.confirm?.message}>
            <Input
              id="confirm"
              type="password"
              autoComplete="new-password"
              aria-invalid={!!errors.confirm}
              {...register('confirm')}
            />
          </Field>
        </CardContent>
        <CardFooter className="mt-6 justify-end">
          <Button type="submit" disabled={change.isPending}>
            {hasPassword ? 'Change password' : 'Set password'}
          </Button>
        </CardFooter>
      </form>
    </Card>
  )
}

function IdentitiesCard() {
  const { data: me } = useMe()
  const setup = useSetup()
  const unlink = useUnlinkIdentity()
  const [toUnlink, setToUnlink] = useState<Identity | null>(null)
  const identities = me?.identities ?? []
  const linked = new Set(identities.map((i) => i.provider))
  const connectable = (setup.data?.providers ?? []).filter((p) => !linked.has(p.id))
  // Nothing to show or offer, so skip the card instead of rendering an empty one.
  if (identities.length === 0 && connectable.length === 0) return null
  return (
    <Card>
      <CardHeader>
        <CardTitle>Sign-in methods</CardTitle>
        <CardDescription>Single sign-on accounts connected to this gotero account.</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        {identities.length === 0 ? (
          <p className="text-sm text-muted-foreground">No providers connected yet.</p>
        ) : (
          <ul className="divide-y rounded-md border">
            {identities.map((i) => (
              <li key={i.provider} className="flex items-center gap-3 p-3">
                <div className="min-w-0 flex-1">
                  <p className="truncate text-sm font-medium">
                    {i.providerName}{' '}
                    <Badge variant="secondary" className="ml-1 uppercase">
                      {i.kind}
                    </Badge>
                  </p>
                  <p className="truncate text-xs text-muted-foreground">
                    {i.email} - last login {relativeTime(i.lastLoginAt)}
                  </p>
                </div>
                <Button variant="outline" size="sm" onClick={() => setToUnlink(i)}>
                  <Unlink /> Unlink
                </Button>
              </li>
            ))}
          </ul>
        )}
        {connectable.length > 0 && (
          <div className="flex flex-wrap gap-2">
            {connectable.map((p) => (
              <Button key={p.id} asChild variant="outline" size="sm">
                <a href={ssoHref(p, { link: '1' })}>Connect {p.displayName}</a>
              </Button>
            ))}
          </div>
        )}
      </CardContent>
      <ConfirmDialog
        open={!!toUnlink}
        onOpenChange={(o) => !o && setToUnlink(null)}
        title="Unlink sign-in method?"
        description={
          <>
            You will no longer be able to log in with <strong>{toUnlink?.providerName}</strong>.
          </>
        }
        confirmLabel="Unlink"
        pending={unlink.isPending}
        onConfirm={() =>
          toUnlink &&
          unlink.mutate(toUnlink.provider, {
            onSuccess: () => {
              toast.success('Sign-in method unlinked')
              setToUnlink(null)
            },
            onError: (e) => {
              // Typically 409 last_login_method; the server message explains why.
              toast.error(errorMessage(e))
              setToUnlink(null)
            },
          })
        }
      />
    </Card>
  )
}

const keySchema = z.object({
  name: z.string().min(1, 'Give the key a name'),
  library: z.boolean(),
  notes: z.boolean(),
  files: z.boolean(),
  write: z.boolean(),
  groups: z.enum(['all', 'read', 'none']),
})
type KeyValues = z.infer<typeof keySchema>

function CreateKeyDialog({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  onCreated: (k: CreatedApiKey) => void
}) {
  const create = useCreateKey()
  const {
    register,
    control,
    handleSubmit,
    reset,
    formState: { errors },
  } = useForm<KeyValues>({
    resolver: zodResolver(keySchema),
    defaultValues: {
      name: '',
      library: true,
      notes: true,
      files: true,
      write: false,
      groups: 'none',
    },
  })
  const checks = [
    ['library', 'Read library'],
    ['notes', 'Read notes'],
    ['files', 'Access files'],
    ['write', 'Write access'],
  ] as const
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        onOpenChange(o)
        if (!o) reset()
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Create API key</DialogTitle>
          <DialogDescription>
            Keys let scripts and other tools use the Zotero API on your behalf.
          </DialogDescription>
        </DialogHeader>
        <form
          id="create-key"
          noValidate
          className="grid gap-4"
          onSubmit={handleSubmit(({ name, groups, ...rest }) => {
            const access: ApiKeyAccess = { ...rest, groups }
            create.mutate(
              { name, access },
              {
                onSuccess: (k) => {
                  reset()
                  onCreated(k)
                },
                onError: (e) => toast.error(errorMessage(e)),
              },
            )
          })}
        >
          <Field label="Name" htmlFor="key-name" error={errors.name?.message}>
            <Input
              id="key-name"
              placeholder="e.g. Home laptop"
              aria-invalid={!!errors.name}
              {...register('name')}
            />
          </Field>
          <fieldset className="grid gap-3">
            <legend className="mb-1 text-sm font-medium">Personal library access</legend>
            {checks.map(([name, label]) => (
              <Controller
                key={name}
                control={control}
                name={name}
                render={({ field }) => (
                  <div className="flex items-center gap-2">
                    <Checkbox
                      id={`key-${name}`}
                      checked={field.value}
                      onCheckedChange={(c) => field.onChange(c === true)}
                    />
                    <Label htmlFor={`key-${name}`} className="font-normal">
                      {label}
                    </Label>
                  </div>
                )}
              />
            ))}
          </fieldset>
          <Field label="Group access" htmlFor="key-groups">
            <Controller
              control={control}
              name="groups"
              render={({ field }) => (
                <Select value={field.value} onValueChange={field.onChange}>
                  <SelectTrigger id="key-groups">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="none">No groups</SelectItem>
                    <SelectItem value="read">Read-only access to all groups</SelectItem>
                    <SelectItem value="all">Read and write access to all groups</SelectItem>
                  </SelectContent>
                </Select>
              )}
            />
          </Field>
        </form>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button type="submit" form="create-key" disabled={create.isPending}>
            Create key
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function accessSummary(a: ApiKeyAccess) {
  const parts = [
    a.library && 'library',
    a.notes && 'notes',
    a.files && 'files',
    a.write && 'write',
  ].filter(Boolean) as string[]
  if (a.groups !== 'none') parts.push(`groups (${a.groups === 'all' ? 'read/write' : 'read'})`)
  return parts.length ? parts.join(', ') : 'no access'
}

function KeysCard() {
  const keys = useKeys()
  const revoke = useRevokeKey()
  const [creating, setCreating] = useState(false)
  const [created, setCreated] = useState<CreatedApiKey | null>(null)
  const [toRevoke, setToRevoke] = useState<ApiKey | null>(null)
  return (
    <Card>
      <CardHeader className="flex-row items-start justify-between gap-4">
        <div className="grid gap-1.5">
          <CardTitle>API keys</CardTitle>
          <CardDescription>
            Keys for the Zotero desktop app are created automatically when you log in.
          </CardDescription>
        </div>
        <Button size="sm" onClick={() => setCreating(true)}>
          <Plus /> Create key
        </Button>
      </CardHeader>
      <CardContent>
        {keys.isLoading ? (
          <ListSkeleton rows={2} />
        ) : keys.error ? (
          <ErrorState error={keys.error} retry={() => keys.refetch()} />
        ) : keys.data?.length === 0 ? (
          <EmptyState title="No API keys" description="Create one to use the API from scripts." />
        ) : (
          <ul className="divide-y rounded-md border">
            {keys.data?.map((k) => (
              <li key={k.id} className="flex items-center gap-3 p-3">
                <KeyRound className="size-4 shrink-0 text-muted-foreground" />
                <div className="min-w-0 flex-1">
                  <p className="truncate text-sm font-medium">
                    {k.name}{' '}
                    <span className="font-mono text-xs font-normal text-muted-foreground">
                      {k.prefix}...
                    </span>
                  </p>
                  <p className="truncate text-xs text-muted-foreground">
                    {accessSummary(k.access)} - created {relativeTime(k.createdAt)} - last used{' '}
                    {relativeTime(k.lastUsedAt)}
                  </p>
                </div>
                <Button
                  variant="ghost"
                  size="icon"
                  aria-label={`Revoke ${k.name}`}
                  onClick={() => setToRevoke(k)}
                >
                  <Trash2 />
                </Button>
              </li>
            ))}
          </ul>
        )}
      </CardContent>

      <CreateKeyDialog
        open={creating}
        onOpenChange={setCreating}
        onCreated={(k) => {
          setCreating(false)
          setCreated(k)
        }}
      />
      <Dialog open={!!created} onOpenChange={(o) => !o && setCreated(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Your new API key</DialogTitle>
            <DialogDescription>The full key is only shown once.</DialogDescription>
          </DialogHeader>
          <Alert variant="warning">
            <AlertTriangle />
            <AlertTitle>Copy it now</AlertTitle>
            <AlertDescription>If you lose it, revoke it and create a new one.</AlertDescription>
          </Alert>
          {created && <CodeLine label="API key" value={created.key} />}
          <DialogFooter>
            <Button onClick={() => setCreated(null)}>Done</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <ConfirmDialog
        open={!!toRevoke}
        onOpenChange={(o) => !o && setToRevoke(null)}
        title="Revoke API key?"
        description={
          <>
            Anything using <strong>{toRevoke?.name}</strong> will stop working immediately.
          </>
        }
        confirmLabel="Revoke key"
        pending={revoke.isPending}
        onConfirm={() =>
          toRevoke &&
          revoke.mutate(toRevoke.id, {
            onSuccess: () => {
              toast.success('API key revoked')
              setToRevoke(null)
            },
            onError: (e) => toast.error(errorMessage(e)),
          })
        }
      />
    </Card>
  )
}

function GroupsCard() {
  const groups = useMyGroups()
  return (
    <Card>
      <CardHeader>
        <CardTitle>My groups</CardTitle>
      </CardHeader>
      <CardContent>
        {groups.isLoading ? (
          <ListSkeleton rows={2} />
        ) : groups.error ? (
          <ErrorState error={groups.error} />
        ) : groups.data?.length === 0 ? (
          <EmptyState
            title="Not in any groups"
            description="Groups you create or join will show up here."
          />
        ) : (
          <ul className="divide-y rounded-md border">
            {groups.data?.map((g) => (
              <li key={g.id} className="flex items-center justify-between gap-3 p-3">
                <Link
                  to="/groups/$id"
                  params={{ id: String(g.id) }}
                  className="truncate text-sm font-medium hover:underline"
                >
                  {g.name}
                </Link>
                <Badge variant="secondary" className="capitalize">
                  {g.role}
                </Badge>
              </li>
            ))}
          </ul>
        )}
      </CardContent>
    </Card>
  )
}

export default function AccountPage() {
  const { data: me } = useMe()
  const setup = useSetup()
  const { error } = useSearch({ from: '/app/account' })
  // Wait for /setup so the card does not flash in for non-admins when local login is off.
  const showPassword = !!me && !!setup.data && (setup.data.localLogin || me.isAdmin)
  return (
    <>
      <PageHeader
        title="Account"
        description={me ? `Member since ${formatDate(me.createdAt)}` : undefined}
      />
      <div className="grid gap-6">
        <SsoErrorAlert code={error} />
        <ProfileCard />
        <IdentitiesCard />
        {showPassword && <PasswordCard hasPassword={me.hasPassword} />}
        <KeysCard />
        <GroupsCard />
      </div>
    </>
  )
}
