import { useEffect, useState, type ReactNode } from 'react'
import { Check, Copy, Inbox } from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { AlertCircle } from 'lucide-react'
import { errorMessage } from '@/lib/queries'

export function usePageTitle(title: string) {
  useEffect(() => {
    document.title = `${title} - gotero`
  }, [title])
}

export function PageHeader({
  title,
  description,
  actions,
}: {
  title: string
  description?: string
  actions?: ReactNode
}) {
  usePageTitle(title)
  return (
    <div className="mb-8 flex flex-wrap items-start justify-between gap-4">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
        {description && <p className="mt-1 text-sm text-muted-foreground">{description}</p>}
      </div>
      {actions && <div className="flex items-center gap-2">{actions}</div>}
    </div>
  )
}

export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    return false
  }
}

export function CopyButton({
  value,
  label = 'Copy',
  variant = 'outline',
}: {
  value: string
  label?: string
  variant?: 'outline' | 'ghost'
}) {
  const [done, setDone] = useState(false)
  return (
    <Button
      type="button"
      variant={variant}
      size="sm"
      onClick={async () => {
        if (await copyText(value)) {
          setDone(true)
          toast.success('Copied to clipboard')
          setTimeout(() => setDone(false), 1500)
        } else toast.error('Could not copy. Select the text and copy it manually.')
      }}
    >
      {done ? <Check /> : <Copy />}
      {done ? 'Copied' : label}
    </Button>
  )
}

export function CodeLine({ value, label }: { value: string; label: string }) {
  return (
    <div className="flex items-center gap-2 rounded-md border bg-muted/40 py-1 pr-1 pl-3">
      <code aria-label={label} className="min-w-0 flex-1 truncate font-mono text-xs sm:text-sm">
        {value}
      </code>
      <CopyButton value={value} label={`Copy ${label}`} variant="ghost" />
    </div>
  )
}

export function EmptyState({
  title,
  description,
  action,
}: {
  title: string
  description?: string
  action?: ReactNode
}) {
  return (
    <div className="flex flex-col items-center gap-3 rounded-xl border border-dashed px-6 py-12 text-center">
      <div className="rounded-full bg-muted p-3 text-muted-foreground">
        <Inbox className="size-5" />
      </div>
      <div>
        <p className="font-medium">{title}</p>
        {description && <p className="mt-1 text-sm text-muted-foreground">{description}</p>}
      </div>
      {action}
    </div>
  )
}

export function ErrorState({ error, retry }: { error: unknown; retry?: () => void }) {
  return (
    <Alert variant="destructive">
      <AlertCircle />
      <AlertTitle>Something went wrong</AlertTitle>
      <AlertDescription>
        <p>{errorMessage(error)}</p>
        {retry && (
          <Button variant="outline" size="sm" onClick={retry}>
            Try again
          </Button>
        )}
      </AlertDescription>
    </Alert>
  )
}

export function ListSkeleton({ rows = 4 }: { rows?: number }) {
  return (
    <div className="space-y-3" role="status" aria-label="Loading">
      {Array.from({ length: rows }, (_, i) => (
        <Skeleton key={i} className="h-12 w-full" />
      ))}
    </div>
  )
}

export function Field({
  label,
  htmlFor,
  error,
  hint,
  children,
}: {
  label: string
  htmlFor: string
  error?: string
  hint?: string
  children: ReactNode
}) {
  return (
    <div className="grid gap-2">
      <Label htmlFor={htmlFor}>{label}</Label>
      {children}
      {hint && !error && <p className="text-xs text-muted-foreground">{hint}</p>}
      {error && (
        <p role="alert" className="text-xs text-destructive">
          {error}
        </p>
      )}
    </div>
  )
}

type ConfirmProps = {
  open: boolean
  onOpenChange: (o: boolean) => void
  title: string
  description: ReactNode
  confirmLabel: string
  onConfirm: () => void
  pending?: boolean
  destructive?: boolean
  // When set, the user must type this exact text before confirming.
  requireText?: string
}

// The content unmounts when the dialog closes, so the typed text resets for free.
function ConfirmBody({
  title,
  description,
  confirmLabel,
  onConfirm,
  pending,
  destructive = true,
  requireText,
}: Omit<ConfirmProps, 'open' | 'onOpenChange'>) {
  const [typed, setTyped] = useState('')
  const blocked = !!requireText && typed !== requireText
  return (
    <AlertDialogContent>
      <AlertDialogHeader>
        <AlertDialogTitle>{title}</AlertDialogTitle>
        <AlertDialogDescription asChild>
          <div>{description}</div>
        </AlertDialogDescription>
      </AlertDialogHeader>
      {requireText && (
        <div className="grid gap-2">
          <Label htmlFor="confirm-text">
            Type <span className="font-mono font-semibold">{requireText}</span> to confirm
          </Label>
          <Input
            id="confirm-text"
            autoComplete="off"
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
          />
        </div>
      )}
      <AlertDialogFooter>
        <AlertDialogCancel>Cancel</AlertDialogCancel>
        <AlertDialogAction
          disabled={blocked || pending}
          className={destructive ? 'bg-destructive text-white hover:bg-destructive/90' : undefined}
          onClick={(e) => {
            // Keep the dialog open until the mutation settles so errors stay in context.
            e.preventDefault()
            onConfirm()
          }}
        >
          {pending ? 'Working...' : confirmLabel}
        </AlertDialogAction>
      </AlertDialogFooter>
    </AlertDialogContent>
  )
}

export function ConfirmDialog({ open, onOpenChange, ...rest }: ConfirmProps) {
  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <ConfirmBody {...rest} />
    </AlertDialog>
  )
}

export function UsageMeter({ used, quota }: { used: number; quota: number | null }) {
  const pct = quota ? Math.min(100, (used / quota) * 100) : 0
  return (
    <div
      role="meter"
      aria-label="Storage used"
      aria-valuemin={0}
      aria-valuemax={quota ?? undefined}
      aria-valuenow={used}
      className="h-2 w-full overflow-hidden rounded-full bg-muted"
    >
      <div
        className={pct > 90 ? 'h-full bg-destructive' : 'h-full bg-primary'}
        style={{ width: quota ? `${pct}%` : '8%' }}
      />
    </div>
  )
}
