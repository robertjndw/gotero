import { AlertCircle } from 'lucide-react'
import type { SsoProvider } from '@/lib/types'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { CardContent } from '@/components/ui/card'
import { cn } from '@/lib/utils'

// Only these strings are ever rendered for ?error=, never the raw value from the URL.
const SSO_ERRORS: Record<string, string> = {
  sso_failed: 'Single sign-on failed. Please try again.',
  sso_state: 'The sign-in attempt expired or was started in another browser. Please try again.',
  unknown_provider: 'That sign-in provider does not exist.',
  no_account: 'No account is linked to that sign-in. Ask an administrator for an invite.',
  disabled: 'This account is disabled.',
  invalid_invite: 'This invitation is expired, already used or unknown.',
  invite_email_mismatch: 'The email address of that sign-in does not match the invitation.',
  link_required:
    'An account with this email already exists. Log in to it and connect the provider from your account page.',
  identity_in_use: 'That sign-in is already linked to another account.',
}
const SSO_ERROR_FALLBACK = 'Sign-in failed. Please try again.'

export function SsoErrorAlert({ code }: { code: string | undefined }) {
  if (!code) return null
  const message = Object.hasOwn(SSO_ERRORS, code) ? SSO_ERRORS[code] : SSO_ERROR_FALLBACK
  return (
    <Alert variant="destructive">
      <AlertCircle />
      <AlertDescription>{message}</AlertDescription>
    </Alert>
  )
}

// startUrl comes from the server and may already carry a query string.
export function ssoHref(provider: SsoProvider, params: Record<string, string | undefined>) {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) if (v) q.set(k, v)
  const qs = q.toString()
  if (!qs) return provider.startUrl
  return provider.startUrl + (provider.startUrl.includes('?') ? '&' : '?') + qs
}

// Error banner plus one button per provider. `divider` is shown under the
// buttons when a password form follows; omit it when there is none.
export function SsoButtons({
  providers,
  error,
  verb,
  params,
  divider,
  className,
}: {
  providers: SsoProvider[]
  error: string | undefined
  verb: string
  params: Record<string, string | undefined>
  divider?: string
  className?: string
}) {
  if (!error && providers.length === 0) return null
  return (
    <CardContent className={cn('grid gap-3', className)}>
      <SsoErrorAlert code={error} />
      {providers.map((p) => (
        <Button key={p.id} asChild variant="outline">
          {/* Plain anchor: the SSO flow needs a full page navigation, not a router transition. */}
          <a href={ssoHref(p, params)}>
            {verb} {p.displayName}
          </a>
        </Button>
      ))}
      {providers.length > 0 && divider && (
        <p className="text-center text-xs text-muted-foreground">{divider}</p>
      )}
    </CardContent>
  )
}
