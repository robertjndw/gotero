import { useState } from 'react'
import { Link, useNavigate } from '@tanstack/react-router'
import { Plus, UsersRound } from 'lucide-react'
import { toast } from 'sonner'
import { useCreateGroup, useGroups } from '@/lib/queries'
import { formatBytes } from '@/lib/format'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { EmptyState, ErrorState, ListSkeleton, PageHeader } from '@/components/common'
import { GroupFields, groupDefaults, useGroupForm } from '@/components/group-form'
import { applyServerError } from '@/lib/forms'

function CreateGroupDialog({
  open,
  onOpenChange,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
}) {
  const create = useCreateGroup()
  const navigate = useNavigate()
  const form = useGroupForm(groupDefaults)
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        onOpenChange(o)
        if (!o) form.reset(groupDefaults)
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Create group</DialogTitle>
          <DialogDescription>
            You will become the owner and can add members afterwards.
          </DialogDescription>
        </DialogHeader>
        <form
          id="create-group"
          noValidate
          onSubmit={form.handleSubmit((v) =>
            create.mutate(v, {
              onSuccess: (g) => {
                toast.success(`Group "${g.name}" created`)
                onOpenChange(false)
                form.reset(groupDefaults)
                void navigate({ to: '/groups/$id', params: { id: String(g.id) } })
              },
              onError: (e) => applyServerError(e, form.setError),
            }),
          )}
        >
          <GroupFields form={form} />
        </form>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button type="submit" form="create-group" disabled={create.isPending}>
            Create group
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

export default function GroupsPage() {
  const groups = useGroups()
  const [open, setOpen] = useState(false)
  return (
    <>
      <PageHeader
        title="Groups"
        description="Libraries shared between users."
        actions={
          <Button onClick={() => setOpen(true)}>
            <Plus /> Create group
          </Button>
        }
      />
      {groups.isLoading ? (
        <ListSkeleton />
      ) : groups.error ? (
        <ErrorState error={groups.error} retry={() => groups.refetch()} />
      ) : groups.data?.items.length === 0 ? (
        <EmptyState
          title="No groups yet"
          description="Create a group to share a library with colleagues."
          action={
            <Button onClick={() => setOpen(true)}>
              <Plus /> Create group
            </Button>
          }
        />
      ) : (
        <Card className="gap-0 py-0">
          <ul className="divide-y">
            {groups.data?.items.map((g) => (
              <li key={g.id}>
                <Link
                  to="/groups/$id"
                  params={{ id: String(g.id) }}
                  className="flex items-center gap-4 p-4 outline-none transition-colors hover:bg-accent/50 focus-visible:bg-accent/50"
                >
                  <div className="rounded-md bg-primary/10 p-2 text-primary">
                    <UsersRound className="size-4" />
                  </div>
                  <div className="min-w-0 flex-1">
                    <p className="truncate font-medium">{g.name}</p>
                    <p className="truncate text-sm text-muted-foreground">
                      {g.description || `Owned by @${g.owner.username}`}
                    </p>
                  </div>
                  <div className="text-right text-sm text-muted-foreground">
                    <p>
                      {g.memberCount} {g.memberCount === 1 ? 'member' : 'members'}
                    </p>
                    <p>{formatBytes(g.usedBytes)}</p>
                  </div>
                </Link>
              </li>
            ))}
          </ul>
        </Card>
      )}
      <CreateGroupDialog open={open} onOpenChange={setOpen} />
    </>
  )
}
