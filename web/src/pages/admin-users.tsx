import { useEffect, useState } from 'react'
import { Controller, useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { toast } from 'sonner'
import { Pencil, Plus, Search, Trash2, Unlink } from 'lucide-react'
import {
  errorMessage,
  useAdminUnlinkIdentity,
  useAdminUsers,
  useCreateUser,
  useDeleteUser,
  useMe,
  useUpdateUser,
} from '@/lib/queries'
import type { User } from '@/lib/types'
import { formatBytes, relativeTime } from '@/lib/format'
import { applyServerError } from '@/lib/forms'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
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
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { Switch } from '@/components/ui/switch'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import {
  ConfirmDialog,
  EmptyState,
  ErrorState,
  Field,
  ListSkeleton,
  PageHeader,
} from '@/components/common'
import { QuotaInput } from '@/components/quota-input'
import { useProviderName } from '@/components/sso'

const createSchema = z.object({
  username: z
    .string()
    .min(3, 'At least 3 characters')
    .regex(/^[A-Za-z0-9._-]+$/, 'Letters, numbers, dots, dashes and underscores only'),
  displayName: z.string().min(1, 'Enter a display name'),
  email: z.email('Enter a valid email address'),
  password: z.string().min(8, 'At least 8 characters'),
  isAdmin: z.boolean(),
  quotaBytes: z.number().nullable(),
})
type CreateValues = z.infer<typeof createSchema>

function CreateUserDialog({
  open,
  onOpenChange,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
}) {
  const create = useCreateUser()
  const defaults: CreateValues = {
    username: '',
    displayName: '',
    email: '',
    password: '',
    isAdmin: false,
    quotaBytes: null,
  }
  const {
    register,
    control,
    handleSubmit,
    setError,
    reset,
    formState: { errors },
  } = useForm<CreateValues>({
    resolver: zodResolver(createSchema),
    defaultValues: defaults,
  })
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        onOpenChange(o)
        if (!o) reset(defaults)
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Create user</DialogTitle>
          <DialogDescription>
            The user can change their password after logging in.
          </DialogDescription>
        </DialogHeader>
        <form
          id="create-user"
          noValidate
          className="grid gap-4"
          onSubmit={handleSubmit((v) =>
            create.mutate(v, {
              onSuccess: (u) => {
                toast.success(`User @${u.username} created`)
                onOpenChange(false)
                reset(defaults)
              },
              onError: (e) => applyServerError(e, setError),
            }),
          )}
        >
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="Username" htmlFor="c-username" error={errors.username?.message}>
              <Input
                id="c-username"
                autoComplete="off"
                aria-invalid={!!errors.username}
                {...register('username')}
              />
            </Field>
            <Field label="Display name" htmlFor="c-displayName" error={errors.displayName?.message}>
              <Input
                id="c-displayName"
                autoComplete="off"
                aria-invalid={!!errors.displayName}
                {...register('displayName')}
              />
            </Field>
          </div>
          <Field label="Email" htmlFor="c-email" error={errors.email?.message}>
            <Input
              id="c-email"
              type="email"
              autoComplete="off"
              aria-invalid={!!errors.email}
              {...register('email')}
            />
          </Field>
          <Field label="Initial password" htmlFor="c-password" error={errors.password?.message}>
            <Input
              id="c-password"
              type="password"
              autoComplete="new-password"
              aria-invalid={!!errors.password}
              {...register('password')}
            />
          </Field>
          <Field label="Storage quota" htmlFor="c-quota">
            <Controller
              control={control}
              name="quotaBytes"
              render={({ field }) => (
                <QuotaInput id="c-quota" value={field.value} onChange={field.onChange} />
              )}
            />
          </Field>
          <Controller
            control={control}
            name="isAdmin"
            render={({ field }) => (
              <div className="flex items-center gap-2">
                <Checkbox
                  id="c-admin"
                  checked={field.value}
                  onCheckedChange={(c) => field.onChange(c === true)}
                />
                <Label htmlFor="c-admin" className="font-normal">
                  Administrator
                </Label>
              </div>
            )}
          />
        </form>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button type="submit" form="create-user" disabled={create.isPending}>
            Create user
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

const editSchema = z.object({
  displayName: z.string().min(1, 'Enter a display name'),
  email: z.email('Enter a valid email address'),
  isAdmin: z.boolean(),
  disabled: z.boolean(),
  quotaBytes: z.number().nullable(),
  password: z.string().refine((v) => v === '' || v.length >= 8, 'At least 8 characters'),
})
type EditValues = z.infer<typeof editSchema>

function EditUserSheet({
  user,
  onClose,
  isSelf,
}: {
  user: User | null
  onClose: () => void
  isSelf: boolean
}) {
  const update = useUpdateUser()
  return (
    <Sheet open={!!user} onOpenChange={(o) => !o && onClose()}>
      <SheetContent>
        {user && (
          <EditForm key={user.id} user={user} isSelf={isSelf} onClose={onClose} update={update} />
        )}
      </SheetContent>
    </Sheet>
  )
}

function ProviderList({ user }: { user: User }) {
  const unlink = useAdminUnlinkIdentity()
  const name = useProviderName()
  return (
    <div className="grid gap-2">
      <p className="text-sm font-medium">Single sign-on</p>
      <ul className="divide-y rounded-md border">
        {user.providers.map((id) => (
          <li key={id} className="flex items-center justify-between gap-3 p-2 pl-3">
            <span className="truncate text-sm">{name(id)}</span>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              aria-label={`Unlink ${name(id)}`}
              disabled={unlink.isPending}
              onClick={() =>
                unlink.mutate(
                  { userId: user.id, provider: id },
                  {
                    onSuccess: () => toast.success(`${name(id)} unlinked from @${user.username}`),
                    onError: (e) => toast.error(errorMessage(e)),
                  },
                )
              }
            >
              <Unlink /> Unlink
            </Button>
          </li>
        ))}
      </ul>
      <p className="text-xs text-muted-foreground">
        Signing in with SSO never changes admin rights.
      </p>
    </div>
  )
}

function EditForm({
  user,
  isSelf,
  onClose,
  update,
}: {
  user: User
  isSelf: boolean
  onClose: () => void
  update: ReturnType<typeof useUpdateUser>
}) {
  const {
    register,
    control,
    handleSubmit,
    setError,
    formState: { errors, dirtyFields },
  } = useForm<EditValues>({
    resolver: zodResolver(editSchema),
    defaultValues: {
      displayName: user.displayName,
      email: user.email,
      isAdmin: user.isAdmin,
      disabled: user.disabled,
      quotaBytes: user.quotaBytes,
      password: '',
    },
  })
  return (
    <form
      noValidate
      className="flex min-h-0 flex-1 flex-col gap-4"
      onSubmit={handleSubmit((v) => {
        const { password, ...rest } = v
        update.mutate(
          { id: user.id, ...rest, ...(password ? { password } : {}) },
          {
            onSuccess: () => {
              toast.success(`@${user.username} updated`)
              onClose()
            },
            onError: (e) => applyServerError(e, setError),
          },
        )
      })}
    >
      <SheetHeader>
        <SheetTitle>Edit @{user.username}</SheetTitle>
        <SheetDescription>
          {formatBytes(user.usedBytes)} used - last sync {relativeTime(user.lastSyncAt)}
        </SheetDescription>
      </SheetHeader>
      <div className="grid gap-4 px-6">
        <Field label="Display name" htmlFor="e-displayName" error={errors.displayName?.message}>
          <Input id="e-displayName" {...register('displayName')} />
        </Field>
        <Field label="Email" htmlFor="e-email" error={errors.email?.message}>
          <Input id="e-email" type="email" {...register('email')} />
        </Field>
        <Field label="Storage quota" htmlFor="e-quota">
          <Controller
            control={control}
            name="quotaBytes"
            render={({ field }) => (
              <QuotaInput id="e-quota" value={field.value} onChange={field.onChange} />
            )}
          />
        </Field>
        <Controller
          control={control}
          name="isAdmin"
          render={({ field }) => (
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="e-admin" className="flex-col items-start gap-1">
                Administrator
                {isSelf && (
                  <span className="text-xs font-normal text-muted-foreground">
                    You cannot change your own role.
                  </span>
                )}
              </Label>
              <Switch
                id="e-admin"
                checked={field.value}
                disabled={isSelf}
                onCheckedChange={field.onChange}
              />
            </div>
          )}
        />
        <Controller
          control={control}
          name="disabled"
          render={({ field }) => (
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="e-disabled" className="flex-col items-start gap-1">
                Disabled
                <span className="text-xs font-normal text-muted-foreground">
                  {isSelf ? 'You cannot disable yourself.' : 'Blocks login and sync.'}
                </span>
              </Label>
              <Switch
                id="e-disabled"
                checked={field.value}
                disabled={isSelf}
                onCheckedChange={field.onChange}
              />
            </div>
          )}
        />
        {user.providers.length > 0 && <ProviderList user={user} />}
        <Field
          label="Reset password"
          htmlFor="e-password"
          error={errors.password?.message}
          hint="Leave empty to keep the current password."
        >
          <Input
            id="e-password"
            type="password"
            autoComplete="new-password"
            {...register('password')}
          />
        </Field>
      </div>
      <SheetFooter className="flex-row justify-end">
        <Button type="button" variant="outline" onClick={onClose}>
          Cancel
        </Button>
        <Button type="submit" disabled={update.isPending || Object.keys(dirtyFields).length === 0}>
          Save changes
        </Button>
      </SheetFooter>
    </form>
  )
}

export default function UsersPage() {
  const { data: me } = useMe()
  const [q, setQ] = useState('')
  const [debounced, setDebounced] = useState('')
  useEffect(() => {
    const t = setTimeout(() => setDebounced(q.trim()), 250)
    return () => clearTimeout(t)
  }, [q])
  const users = useAdminUsers(debounced)
  const providerName = useProviderName()
  const del = useDeleteUser()
  const [creating, setCreating] = useState(false)
  const [editing, setEditing] = useState<User | null>(null)
  const [deleting, setDeleting] = useState<User | null>(null)

  return (
    <>
      <PageHeader
        title="Users"
        description="Everyone who can sync with this server."
        actions={
          <Button onClick={() => setCreating(true)}>
            <Plus /> Create user
          </Button>
        }
      />
      <div className="relative mb-4 max-w-sm">
        <Search className="pointer-events-none absolute top-2.5 left-3 size-4 text-muted-foreground" />
        <Input
          aria-label="Search users"
          placeholder="Search by name, username or email"
          className="pl-9"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
      </div>
      {users.isLoading ? (
        <ListSkeleton />
      ) : users.error ? (
        <ErrorState error={users.error} retry={() => users.refetch()} />
      ) : users.data?.items.length === 0 ? (
        <EmptyState
          title={debounced ? 'No matching users' : 'No users'}
          description={debounced ? 'Try a different search.' : undefined}
        />
      ) : (
        <div className="rounded-xl border bg-card">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>User</TableHead>
                <TableHead>Storage</TableHead>
                <TableHead className="hidden md:table-cell">Last sync</TableHead>
                <TableHead className="w-24">
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {users.data?.items.map((u) => (
                <TableRow key={u.id}>
                  <TableCell>
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="font-medium">{u.displayName || u.username}</span>
                      {u.isAdmin && <Badge>Admin</Badge>}
                      {u.disabled && <Badge variant="destructive">Disabled</Badge>}
                      {u.providers.map((id) => (
                        <Badge key={id} variant="outline">
                          {providerName(id)}
                        </Badge>
                      ))}
                    </div>
                    <p className="text-xs text-muted-foreground">
                      @{u.username} - {u.email}
                    </p>
                  </TableCell>
                  <TableCell className="whitespace-nowrap text-muted-foreground">
                    {formatBytes(u.usedBytes)} /{' '}
                    {u.quotaBytes === null ? 'unlimited' : formatBytes(u.quotaBytes)}
                  </TableCell>
                  <TableCell className="hidden text-muted-foreground md:table-cell">
                    {relativeTime(u.lastSyncAt)}
                  </TableCell>
                  <TableCell>
                    <div className="flex justify-end">
                      <Button
                        variant="ghost"
                        size="icon"
                        aria-label={`Edit ${u.username}`}
                        onClick={() => setEditing(u)}
                      >
                        <Pencil />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon"
                        aria-label={`Delete ${u.username}`}
                        disabled={u.id === me?.id}
                        onClick={() => setDeleting(u)}
                      >
                        <Trash2 />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}
      <CreateUserDialog open={creating} onOpenChange={setCreating} />
      <EditUserSheet
        // Read from the list so an unlink shows up without reopening the sheet.
        user={users.data?.items.find((u) => u.id === editing?.id) ?? editing}
        isSelf={editing?.id === me?.id}
        onClose={() => setEditing(null)}
      />
      <ConfirmDialog
        open={!!deleting}
        onOpenChange={(o) => !o && setDeleting(null)}
        title="Delete user?"
        description={
          <>
            This permanently deletes <strong>@{deleting?.username}</strong> together with their
            library and all stored files.
          </>
        }
        requireText={deleting?.username}
        confirmLabel="Delete user"
        pending={del.isPending}
        onConfirm={() =>
          deleting &&
          del.mutate(deleting.id, {
            onSuccess: () => {
              toast.success('User deleted')
              setDeleting(null)
            },
            onError: (e) => toast.error(errorMessage(e)),
          })
        }
      />
    </>
  )
}
