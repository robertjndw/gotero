import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useForm, Controller } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { toast } from 'sonner'
import { AlertTriangle, KeyRound, Plus, Trash2 } from 'lucide-react'
import {
  errorMessage,
  useChangePassword,
  useCreateKey,
  useKeys,
  useMe,
  useMyGroups,
  useRevokeKey,
  useUpdateProfile,
} from '@/lib/queries'
import type { ApiKey, ApiKeyAccess, CreatedApiKey } from '@/lib/types'
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
    currentPassword: z.string().min(1, 'Enter your current password'),
    newPassword: z.string().min(8, 'At least 8 characters'),
    confirm: z.string(),
  })
  .refine((v) => v.newPassword === v.confirm, {
    path: ['confirm'],
    message: 'Passwords do not match',
  })
type PwValues = z.infer<typeof pwSchema>

function PasswordCard() {
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
        <CardDescription>Choose a new password for this account.</CardDescription>
      </CardHeader>
      <form
        noValidate
        onSubmit={handleSubmit((v) =>
          change.mutate(
            { currentPassword: v.currentPassword, newPassword: v.newPassword },
            {
              onSuccess: () => {
                toast.success('Password changed')
                reset()
              },
              onError: (e) => applyServerError(e, setError),
            },
          ),
        )}
      >
        <CardContent className="grid gap-4 sm:grid-cols-3">
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
            Change password
          </Button>
        </CardFooter>
      </form>
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
  return (
    <>
      <PageHeader
        title="Account"
        description={me ? `Member since ${formatDate(me.createdAt)}` : undefined}
      />
      <div className="grid gap-6">
        <ProfileCard />
        <PasswordCard />
        <KeysCard />
        <GroupsCard />
      </div>
    </>
  )
}
