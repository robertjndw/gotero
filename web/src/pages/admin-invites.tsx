import { useState } from 'react'
import { Controller, useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { toast } from 'sonner'
import { Plus, Trash2 } from 'lucide-react'
import { errorMessage, useCreateInvite, useInvites, useRevokeInvite } from '@/lib/queries'
import type { Invite } from '@/lib/types'
import { formatDate, relativeTime } from '@/lib/format'
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
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import {
  CodeLine,
  ConfirmDialog,
  CopyButton,
  EmptyState,
  ErrorState,
  Field,
  ListSkeleton,
  PageHeader,
} from '@/components/common'

const schema = z.object({
  email: z.union([z.literal(''), z.email('Enter a valid email address')]),
  isAdmin: z.boolean(),
  expiresInHours: z.enum(['24', '72', '168', '720']),
})
type Values = z.infer<typeof schema>
const defaults: Values = { email: '', isAdmin: false, expiresInHours: '168' }

function status(i: Invite): { label: string; variant: 'success' | 'secondary' | 'warning' } {
  if (i.usedBy) return { label: `Used by @${i.usedBy}`, variant: 'secondary' }
  if (new Date(i.expiresAt) < new Date()) return { label: 'Expired', variant: 'warning' }
  return { label: 'Active', variant: 'success' }
}

function CreateInviteDialog({
  open,
  onOpenChange,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
}) {
  const create = useCreateInvite()
  const [created, setCreated] = useState<Invite | null>(null)
  const {
    register,
    control,
    handleSubmit,
    reset,
    setError,
    formState: { errors },
  } = useForm<Values>({ resolver: zodResolver(schema), defaultValues: defaults })
  const close = (o: boolean) => {
    onOpenChange(o)
    if (!o) {
      reset(defaults)
      setCreated(null)
    }
  }
  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent>
        {created ? (
          <>
            <DialogHeader>
              <DialogTitle>Invite created</DialogTitle>
              <DialogDescription>
                Send this link to the person you want to invite.
              </DialogDescription>
            </DialogHeader>
            <CodeLine label="invite link" value={created.url} />
            <DialogFooter>
              <Button onClick={() => close(false)}>Done</Button>
            </DialogFooter>
          </>
        ) : (
          <>
            <DialogHeader>
              <DialogTitle>Create invite</DialogTitle>
              <DialogDescription>Generates a one-time registration link.</DialogDescription>
            </DialogHeader>
            <form
              id="create-invite"
              noValidate
              className="grid gap-4"
              onSubmit={handleSubmit((v) =>
                create.mutate(
                  {
                    email: v.email || undefined,
                    isAdmin: v.isAdmin,
                    expiresInHours: Number(v.expiresInHours),
                  },
                  { onSuccess: setCreated, onError: (e) => applyServerError(e, setError) },
                ),
              )}
            >
              <Field
                label="Email (optional)"
                htmlFor="i-email"
                error={errors.email?.message}
                hint="Locks the invite to this address."
              >
                <Input
                  id="i-email"
                  type="email"
                  aria-invalid={!!errors.email}
                  {...register('email')}
                />
              </Field>
              <Field label="Expires after" htmlFor="i-expiry">
                <Controller
                  control={control}
                  name="expiresInHours"
                  render={({ field }) => (
                    <Select value={field.value} onValueChange={field.onChange}>
                      <SelectTrigger id="i-expiry">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="24">1 day</SelectItem>
                        <SelectItem value="72">3 days</SelectItem>
                        <SelectItem value="168">7 days</SelectItem>
                        <SelectItem value="720">30 days</SelectItem>
                      </SelectContent>
                    </Select>
                  )}
                />
              </Field>
              <Controller
                control={control}
                name="isAdmin"
                render={({ field }) => (
                  <div className="flex items-center gap-2">
                    <Checkbox
                      id="i-admin"
                      checked={field.value}
                      onCheckedChange={(c) => field.onChange(c === true)}
                    />
                    <Label htmlFor="i-admin" className="font-normal">
                      Grant administrator rights
                    </Label>
                  </div>
                )}
              />
            </form>
            <DialogFooter>
              <Button variant="outline" onClick={() => close(false)}>
                Cancel
              </Button>
              <Button type="submit" form="create-invite" disabled={create.isPending}>
                Create invite
              </Button>
            </DialogFooter>
          </>
        )}
      </DialogContent>
    </Dialog>
  )
}

export default function InvitesPage() {
  const invites = useInvites()
  const revoke = useRevokeInvite()
  const [creating, setCreating] = useState(false)
  const [toRevoke, setToRevoke] = useState<Invite | null>(null)
  return (
    <>
      <PageHeader
        title="Invites"
        description="Invite people to register, even while open registration is off."
        actions={
          <Button onClick={() => setCreating(true)}>
            <Plus /> Create invite
          </Button>
        }
      />
      {invites.isLoading ? (
        <ListSkeleton />
      ) : invites.error ? (
        <ErrorState error={invites.error} retry={() => invites.refetch()} />
      ) : invites.data?.items.length === 0 ? (
        <EmptyState title="No invites" description="Create an invite link to add someone." />
      ) : (
        <div className="rounded-xl border bg-card">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Invite</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="hidden md:table-cell">Expires</TableHead>
                <TableHead className="w-40">
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {invites.data?.items.map((i) => {
                const s = status(i)
                return (
                  <TableRow key={i.id}>
                    <TableCell>
                      <div className="flex items-center gap-2">
                        <span className="font-medium">{i.email ?? 'Any email'}</span>
                        {i.isAdmin && <Badge>Admin</Badge>}
                      </div>
                      <p className="text-xs text-muted-foreground">
                        by @{i.createdBy} {relativeTime(i.createdAt)}
                      </p>
                    </TableCell>
                    <TableCell>
                      <Badge variant={s.variant}>{s.label}</Badge>
                    </TableCell>
                    <TableCell className="hidden text-muted-foreground md:table-cell">
                      {formatDate(i.expiresAt)}
                    </TableCell>
                    <TableCell>
                      <div className="flex justify-end gap-1">
                        {!i.usedBy && (
                          <CopyButton value={i.url} label="Copy link" variant="ghost" />
                        )}
                        <Button
                          variant="ghost"
                          size="icon"
                          aria-label={`Revoke invite for ${i.email ?? 'anyone'}`}
                          onClick={() => setToRevoke(i)}
                        >
                          <Trash2 />
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        </div>
      )}
      <CreateInviteDialog open={creating} onOpenChange={setCreating} />
      <ConfirmDialog
        open={!!toRevoke}
        onOpenChange={(o) => !o && setToRevoke(null)}
        title="Revoke invite?"
        description="The link will stop working immediately."
        confirmLabel="Revoke invite"
        pending={revoke.isPending}
        onConfirm={() =>
          toRevoke &&
          revoke.mutate(toRevoke.id, {
            onSuccess: () => {
              toast.success('Invite revoked')
              setToRevoke(null)
            },
            onError: (e) => toast.error(errorMessage(e)),
          })
        }
      />
    </>
  )
}
