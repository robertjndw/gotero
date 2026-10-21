import { useState } from 'react'
import { toast } from 'sonner'
import { Trash2 } from 'lucide-react'
import { errorMessage, useAdminSettings, useGc, useUpdateSettings } from '@/lib/queries'
import type { GcResult } from '@/lib/types'
import { formatBytes } from '@/lib/format'
import { Button } from '@/components/ui/button'
import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { ConfirmDialog, ErrorState, ListSkeleton, PageHeader } from '@/components/common'
import { QuotaInput } from '@/components/quota-input'

function QuotaCard({ initial }: { initial: number | null }) {
  const update = useUpdateSettings()
  const [value, setValue] = useState(initial)
  return (
    <Card>
      <CardHeader>
        <CardTitle>Default storage quota</CardTitle>
        <CardDescription>
          Applied to newly registered users. Existing users keep their quota.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <div className="max-w-sm">
          <QuotaInput id="default-quota" value={value} onChange={setValue} />
        </div>
      </CardContent>
      <CardFooter className="justify-end">
        <Button
          disabled={value === initial || update.isPending}
          onClick={() =>
            update.mutate(
              { defaultQuotaBytes: value },
              {
                onSuccess: () => toast.success('Default quota saved'),
                onError: (e) => toast.error(errorMessage(e)),
              },
            )
          }
        >
          Save
        </Button>
      </CardFooter>
    </Card>
  )
}

function GcCard() {
  const gc = useGc()
  const [preview, setPreview] = useState<GcResult | null>(null)
  return (
    <Card>
      <CardHeader>
        <CardTitle>Garbage collection</CardTitle>
        <CardDescription>
          Find and delete stored files that no library references any more.
        </CardDescription>
      </CardHeader>
      <CardContent>{gc.error && <ErrorState error={gc.error} />}</CardContent>
      <CardFooter>
        <Button
          variant="outline"
          disabled={gc.isPending}
          onClick={() => gc.mutate(true, { onSuccess: setPreview })}
        >
          <Trash2 /> Run garbage collection
        </Button>
      </CardFooter>
      <ConfirmDialog
        open={!!preview}
        onOpenChange={(o) => !o && setPreview(null)}
        title={
          preview && preview.orphanBlobs === 0 ? 'Nothing to clean up' : 'Delete orphaned files?'
        }
        description={
          preview && preview.orphanBlobs === 0 ? (
            'No orphaned files were found.'
          ) : (
            <>
              Dry run found <strong>{preview?.orphanBlobs} files</strong> using{' '}
              <strong>{formatBytes(preview?.orphanBytes ?? 0)}</strong>. Deleting them cannot be
              undone.
            </>
          )
        }
        confirmLabel="Delete files"
        pending={gc.isPending}
        onConfirm={() => {
          if (!preview || preview.orphanBlobs === 0) return setPreview(null)
          gc.mutate(false, {
            onSuccess: (r) => {
              toast.success(`Deleted ${r.orphanBlobs} files (${formatBytes(r.orphanBytes)})`)
              setPreview(null)
            },
            onError: (e) => toast.error(errorMessage(e)),
          })
        }}
      />
    </Card>
  )
}

export default function SettingsPage() {
  const settings = useAdminSettings()
  const update = useUpdateSettings()
  return (
    <>
      <PageHeader title="Settings" description="Server-wide options." />
      {settings.isLoading ? (
        <ListSkeleton rows={2} />
      ) : settings.error || !settings.data ? (
        <ErrorState error={settings.error} retry={() => settings.refetch()} />
      ) : (
        <div className="grid gap-6">
          <Card>
            <CardContent className="flex items-center justify-between gap-6">
              <div className="grid gap-1">
                <Label htmlFor="open-reg" className="text-base">
                  Open registration
                </Label>
                <p className="text-sm text-muted-foreground">
                  Let anyone with the server address create an account. Leave off to use invites
                  only.
                </p>
              </div>
              <Switch
                id="open-reg"
                checked={settings.data.openRegistration}
                disabled={update.isPending}
                onCheckedChange={(c) =>
                  update.mutate(
                    { openRegistration: c },
                    {
                      onSuccess: () =>
                        toast.success(
                          c ? 'Open registration enabled' : 'Open registration disabled',
                        ),
                      onError: (e) => toast.error(errorMessage(e)),
                    },
                  )
                }
              />
            </CardContent>
          </Card>
          <QuotaCard initial={settings.data.defaultQuotaBytes} />
          <GcCard />
        </div>
      )}
    </>
  )
}
