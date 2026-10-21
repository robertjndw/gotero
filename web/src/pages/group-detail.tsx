import { useState } from 'react'
import { Link, useNavigate, useParams } from '@tanstack/react-router'
import { ArrowLeft, Trash2, UserPlus } from 'lucide-react'
import { toast } from 'sonner'
import {
  errorMessage,
  useDeleteGroup,
  useGroup,
  useMe,
  useMembers,
  useRemoveMember,
  useSetMember,
  useUpdateGroup,
} from '@/lib/queries'
import type { GroupInput, GroupRole, UserLookup } from '@/lib/types'
import { formatBytes } from '@/lib/format'
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
import { ConfirmDialog, ErrorState, ListSkeleton, PageHeader } from '@/components/common'
import { GroupFields, useGroupForm } from '@/components/group-form'
import { UserCombobox } from '@/components/user-combobox'
import { Skeleton } from '@/components/ui/skeleton'

function SettingsCard({ id, value, canEdit }: { id: number; value: GroupInput; canEdit: boolean }) {
  const update = useUpdateGroup(id)
  const form = useGroupForm(value)
  return (
    <Card>
      <CardHeader>
        <CardTitle>Settings</CardTitle>
        <CardDescription>
          {canEdit ? 'Permissions apply to every member.' : 'Only group admins can change these.'}
        </CardDescription>
      </CardHeader>
      <form
        noValidate
        onSubmit={form.handleSubmit((v) =>
          update.mutate(v, {
            onSuccess: () => toast.success('Group updated'),
            onError: (e) => applyServerError(e, form.setError),
          }),
        )}
      >
        <CardContent>
          <fieldset disabled={!canEdit}>
            <GroupFields form={form} />
          </fieldset>
        </CardContent>
        {canEdit && (
          <CardFooter className="mt-6 justify-end">
            <Button type="submit" disabled={!form.formState.isDirty || update.isPending}>
              Save changes
            </Button>
          </CardFooter>
        )}
      </form>
    </Card>
  )
}

function MembersCard({ id, canEdit, ownerId }: { id: number; canEdit: boolean; ownerId: number }) {
  const members = useMembers(id)
  const setMember = useSetMember(id)
  const remove = useRemoveMember(id)
  const [picked, setPicked] = useState<UserLookup | null>(null)
  const [toRemove, setToRemove] = useState<{ userId: number; username: string } | null>(null)

  const add = () => {
    if (!picked) return
    setMember.mutate(
      { userId: picked.id, role: 'member' },
      {
        onSuccess: () => {
          toast.success(`Added @${picked.username}`)
          setPicked(null)
        },
        onError: (e) => toast.error(errorMessage(e)),
      },
    )
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle>Members</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-4">
        {canEdit && (
          <div className="flex flex-col gap-2 sm:flex-row">
            <div className="flex-1">
              <UserCombobox id="add-member" value={picked} onChange={setPicked} />
            </div>
            <Button onClick={add} disabled={!picked || setMember.isPending}>
              <UserPlus /> Add member
            </Button>
          </div>
        )}
        {members.isLoading ? (
          <ListSkeleton rows={3} />
        ) : members.error ? (
          <ErrorState error={members.error} retry={() => members.refetch()} />
        ) : (
          <div className="rounded-md border">
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>User</TableHead>
                  <TableHead className="w-40">Role</TableHead>
                  <TableHead className="w-12">
                    <span className="sr-only">Actions</span>
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {members.data?.map((m) => (
                  <TableRow key={m.userId}>
                    <TableCell>
                      <p className="font-medium">{m.displayName || m.username}</p>
                      <p className="text-xs text-muted-foreground">@{m.username}</p>
                    </TableCell>
                    <TableCell>
                      {canEdit && m.role !== 'owner' && m.userId !== ownerId ? (
                        <Select
                          value={m.role}
                          onValueChange={(role) =>
                            setMember.mutate(
                              { userId: m.userId, role: role as GroupRole },
                              {
                                onSuccess: () => toast.success('Role updated'),
                                onError: (e) => toast.error(errorMessage(e)),
                              },
                            )
                          }
                        >
                          <SelectTrigger aria-label={`Role of ${m.username}`} className="h-8">
                            <SelectValue />
                          </SelectTrigger>
                          <SelectContent>
                            <SelectItem value="admin">Admin</SelectItem>
                            <SelectItem value="member">Member</SelectItem>
                          </SelectContent>
                        </Select>
                      ) : (
                        <Badge
                          variant={m.role === 'owner' ? 'default' : 'secondary'}
                          className="capitalize"
                        >
                          {m.role}
                        </Badge>
                      )}
                    </TableCell>
                    <TableCell>
                      {canEdit && m.role !== 'owner' && (
                        <Button
                          variant="ghost"
                          size="icon"
                          aria-label={`Remove ${m.username}`}
                          onClick={() => setToRemove(m)}
                        >
                          <Trash2 />
                        </Button>
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </CardContent>
      <ConfirmDialog
        open={!!toRemove}
        onOpenChange={(o) => !o && setToRemove(null)}
        title="Remove member?"
        description={<>@{toRemove?.username} will lose access to this group library.</>}
        confirmLabel="Remove"
        pending={remove.isPending}
        onConfirm={() =>
          toRemove &&
          remove.mutate(toRemove.userId, {
            onSuccess: () => {
              toast.success('Member removed')
              setToRemove(null)
            },
            onError: (e) => toast.error(errorMessage(e)),
          })
        }
      />
    </Card>
  )
}

export default function GroupDetailPage() {
  const { id: raw } = useParams({ from: '/app/groups/$id' })
  const id = Number(raw)
  const { data: me } = useMe()
  const group = useGroup(id)
  const members = useMembers(id)
  const del = useDeleteGroup()
  const navigate = useNavigate()
  const [confirmDelete, setConfirmDelete] = useState(false)

  if (group.isLoading) return <Skeleton className="h-64 w-full" />
  if (group.error || !group.data) {
    return (
      <>
        <PageHeader title="Group not found" />
        <ErrorState error={group.error} />
      </>
    )
  }
  const g = group.data
  const myRole = members.data?.find((m) => m.userId === me?.id)?.role
  const canEdit = !!me?.isAdmin || myRole === 'owner' || myRole === 'admin'
  const canDelete = !!me?.isAdmin || g.owner.id === me?.id

  return (
    <>
      <Link
        to="/groups"
        className="mb-4 inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
      >
        <ArrowLeft className="size-4" /> All groups
      </Link>
      <PageHeader
        title={g.name}
        description={`Owned by @${g.owner.username} - ${formatBytes(g.usedBytes)} used`}
      />
      <div className="grid gap-6">
        <SettingsCard
          id={id}
          canEdit={canEdit}
          value={{
            name: g.name,
            description: g.description,
            libraryReading: g.libraryReading,
            libraryEditing: g.libraryEditing,
            fileEditing: g.fileEditing,
          }}
        />
        <MembersCard id={id} canEdit={canEdit} ownerId={g.owner.id} />
        {canDelete && (
          <Card className="border-destructive/30">
            <CardHeader>
              <CardTitle>Delete group</CardTitle>
              <CardDescription>
                Permanently removes the group library and its files for every member.
              </CardDescription>
            </CardHeader>
            <CardFooter>
              <Button variant="destructive" onClick={() => setConfirmDelete(true)}>
                Delete this group
              </Button>
            </CardFooter>
          </Card>
        )}
      </div>
      <ConfirmDialog
        open={confirmDelete}
        onOpenChange={setConfirmDelete}
        title="Delete this group?"
        description="This cannot be undone. All items and files in the group library will be deleted."
        requireText={g.name}
        confirmLabel="Delete group"
        pending={del.isPending}
        onConfirm={() =>
          del.mutate(id, {
            onSuccess: () => {
              toast.success('Group deleted')
              void navigate({ to: '/groups' })
            },
            onError: (e) => toast.error(errorMessage(e)),
          })
        }
      />
    </>
  )
}
