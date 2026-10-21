import { useForm, Controller } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { toast } from 'sonner'
import { CheckCircle2, ExternalLink, Loader2, XCircle } from 'lucide-react'
import { useCancelImport, useImports, useStartImport, errorMessage } from '@/lib/queries'
import type { ImportJob } from '@/lib/types'
import { formatBytes, relativeTime } from '@/lib/format'
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
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Progress } from '@/components/ui/progress'
import { EmptyState, ErrorState, Field, ListSkeleton, PageHeader } from '@/components/common'

const schema = z.object({
  apiKey: z.string().trim().min(1, 'Paste your zotero.org API key'),
  includeGroups: z.boolean(),
  includeFiles: z.boolean(),
})
type Values = z.infer<typeof schema>

function StartCard() {
  const start = useStartImport()
  const {
    register,
    control,
    handleSubmit,
    setError,
    reset,
    formState: { errors },
  } = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: { apiKey: '', includeGroups: true, includeFiles: true },
  })
  return (
    <Card>
      <CardHeader>
        <CardTitle>Import from zotero.org</CardTitle>
        <CardDescription>
          Create a key with read access on{' '}
          <a
            href="https://www.zotero.org/settings/keys/new"
            target="_blank"
            rel="noreferrer"
            className="inline-flex items-center gap-1 text-primary underline-offset-4 hover:underline"
          >
            zotero.org/settings/keys <ExternalLink className="size-3" />
          </a>
          , allowing library and file access, then paste it below. The key is only used for this
          import and is not stored.
        </CardDescription>
      </CardHeader>
      <form
        noValidate
        onSubmit={handleSubmit((v) =>
          start.mutate(v, {
            onSuccess: () => {
              toast.success('Import started')
              reset({ apiKey: '', includeGroups: v.includeGroups, includeFiles: v.includeFiles })
            },
            onError: (e) => applyServerError(e, setError),
          }),
        )}
      >
        <CardContent className="grid gap-4">
          <Field label="zotero.org API key" htmlFor="apiKey" error={errors.apiKey?.message}>
            <Input
              id="apiKey"
              type="password"
              autoComplete="off"
              spellCheck={false}
              aria-invalid={!!errors.apiKey}
              {...register('apiKey')}
            />
          </Field>
          <Controller
            control={control}
            name="includeGroups"
            render={({ field }) => (
              <div className="flex items-center gap-2">
                <Checkbox
                  id="includeGroups"
                  checked={field.value}
                  onCheckedChange={(c) => field.onChange(c === true)}
                />
                <Label htmlFor="includeGroups" className="font-normal">
                  Include group libraries
                </Label>
              </div>
            )}
          />
          <Controller
            control={control}
            name="includeFiles"
            render={({ field }) => (
              <div className="flex items-center gap-2">
                <Checkbox
                  id="includeFiles"
                  checked={field.value}
                  onCheckedChange={(c) => field.onChange(c === true)}
                />
                <Label htmlFor="includeFiles" className="font-normal">
                  Include attachment files (uses more storage and time)
                </Label>
              </div>
            )}
          />
        </CardContent>
        <CardFooter className="mt-6 justify-end">
          <Button type="submit" disabled={start.isPending}>
            {start.isPending ? 'Starting...' : 'Start import'}
          </Button>
        </CardFooter>
      </form>
    </Card>
  )
}

const statusBadge: Record<
  ImportJob['status'],
  { label: string; variant: 'secondary' | 'success' | 'destructive' | 'warning' | 'default' }
> = {
  queued: { label: 'Queued', variant: 'secondary' },
  running: { label: 'Running', variant: 'default' },
  completed: { label: 'Completed', variant: 'success' },
  failed: { label: 'Failed', variant: 'destructive' },
  cancelled: { label: 'Cancelled', variant: 'warning' },
}

function JobCard({ job }: { job: ImportJob }) {
  const cancel = useCancelImport()
  const p = job.progress
  const active = job.status === 'queued' || job.status === 'running'
  const total = p.objectsTotal + p.filesTotal
  const pct =
    total > 0
      ? Math.round(((p.objects + p.files) / total) * 100)
      : job.status === 'completed'
        ? 100
        : 0
  const s = statusBadge[job.status]
  return (
    <Card className="gap-4">
      <CardHeader className="flex-row items-center justify-between gap-4">
        <div className="flex items-center gap-3">
          {active ? (
            <Loader2 className="size-4 animate-spin text-primary" />
          ) : job.status === 'completed' ? (
            <CheckCircle2 className="size-4 text-success" />
          ) : (
            <XCircle className="size-4 text-muted-foreground" />
          )}
          <div>
            <CardTitle className="text-base">Import #{job.id}</CardTitle>
            <CardDescription>Started {relativeTime(job.createdAt)}</CardDescription>
          </div>
        </div>
        <div className="flex items-center gap-2">
          <Badge variant={s.variant}>{s.label}</Badge>
          {active && (
            <Button
              variant="outline"
              size="sm"
              disabled={cancel.isPending}
              onClick={() =>
                cancel.mutate(job.id, { onError: (e) => toast.error(errorMessage(e)) })
              }
            >
              Cancel
            </Button>
          )}
        </div>
      </CardHeader>
      <CardContent className="grid gap-3">
        <Progress value={pct} aria-label={`Import ${job.id} progress`} />
        <p className="text-xs text-muted-foreground">
          {p.objects.toLocaleString()} of {p.objectsTotal.toLocaleString()} items
          {p.filesTotal > 0 && (
            <>
              {' '}
              - {p.files.toLocaleString()} of {p.filesTotal.toLocaleString()} files
            </>
          )}
          {' - '}
          {formatBytes(p.bytes)} downloaded
        </p>
        {job.libraries.length > 0 && (
          <ul className="grid gap-1 text-sm">
            {job.libraries.map((l) => (
              <li key={l.name} className="grid gap-0.5">
                <div className="flex items-center justify-between">
                  <span>{l.name}</span>
                  <span className="text-muted-foreground">
                    {l.items.toLocaleString()} items
                    {l.files > 0 && `, ${l.files.toLocaleString()} files`} {l.done && '- done'}
                  </span>
                </div>
                {!!l.conflicts && (
                  <p className="text-xs text-muted-foreground">
                    {l.conflicts.toLocaleString()} kept as they are, because they were changed in
                    gotero after the last import.
                  </p>
                )}
                {l.error && <p className="text-xs text-destructive">{l.error}</p>}
              </li>
            ))}
          </ul>
        )}
        {job.error && (
          <p role="alert" className="text-sm text-destructive">
            {job.error}
          </p>
        )}
      </CardContent>
    </Card>
  )
}

export default function ImportPage() {
  const jobs = useImports()
  return (
    <>
      <PageHeader title="Import" description="Bring your existing library over from zotero.org." />
      <div className="grid gap-6">
        <StartCard />
        <section aria-label="Import jobs" className="grid gap-4">
          <h2 className="text-lg font-semibold">History</h2>
          {jobs.isLoading ? (
            <ListSkeleton rows={2} />
          ) : jobs.error ? (
            <ErrorState error={jobs.error} retry={() => jobs.refetch()} />
          ) : jobs.data?.length === 0 ? (
            <EmptyState
              title="No imports yet"
              description="Imports you start will appear here with live progress."
            />
          ) : (
            jobs.data?.map((j) => <JobCard key={j.id} job={j} />)
          )}
        </section>
      </div>
    </>
  )
}
