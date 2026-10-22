import { useEffect, useState } from 'react'
import { useNavigate, useSearch } from '@tanstack/react-router'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { CheckCircle2, Clock, ShieldAlert, XCircle } from 'lucide-react'
import { ApiError } from '@/lib/api'
import {
  errorMessage,
  useLogin,
  useLoginSession,
  useLogout,
  useMe,
  useResolveLoginSession,
  useSetup,
} from '@/lib/queries'
import type { LoginSession, Me } from '@/lib/types'
import { Link } from '@tanstack/react-router'
import { AuthLayout } from '@/components/layout/auth-layout'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Skeleton } from '@/components/ui/skeleton'
import { Field, usePageTitle } from '@/components/common'
import { SsoButtons } from '@/components/sso'
import { toast } from 'sonner'

const schema = z.object({
  username: z.string().min(1, 'Enter your username'),
  password: z.string().min(1, 'Enter your password'),
})
type Values = z.infer<typeof schema>

function LoginForm({
  session,
  redirect,
  error,
}: {
  session?: string
  redirect?: string
  error?: string
}) {
  const login = useLogin()
  const setup = useSetup()
  const providers = setup.data?.providers ?? []
  // Assume local login is on until /setup says otherwise so the form does not flicker away.
  const localLogin = setup.data?.localLogin ?? true
  // Admins can still use passwords when local login is off, so the form stays reachable.
  const [adminOpen, setAdminOpen] = useState(false)
  const showForm = localLogin || adminOpen
  const {
    register,
    handleSubmit,
    formState: { errors },
  } = useForm<Values>({ resolver: zodResolver(schema) })

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-xl">Log in</CardTitle>
        <CardDescription>Use your gotero account.</CardDescription>
      </CardHeader>
      <SsoButtons
        providers={providers}
        error={error}
        verb="Log in with"
        params={{ session, redirect }}
        divider={showForm ? 'or use a password' : undefined}
      />
      {!showForm ? (
        <CardFooter className="mt-3 justify-center">
          <Button type="button" variant="link" size="sm" onClick={() => setAdminOpen(true)}>
            Administrator sign-in
          </Button>
        </CardFooter>
      ) : (
        <form onSubmit={handleSubmit((v) => login.mutate(v))} noValidate>
          <CardContent className={`grid gap-4${error || providers.length > 0 ? ' mt-4' : ''}`}>
            {login.error && (
              <Alert variant="destructive">
                <AlertDescription>
                  {login.error instanceof ApiError && login.error.status === 401
                    ? 'Invalid username or password.'
                    : errorMessage(login.error)}
                </AlertDescription>
              </Alert>
            )}
            <Field label="Username" htmlFor="username" error={errors.username?.message}>
              <Input
                id="username"
                autoComplete="username"
                autoFocus
                aria-invalid={!!errors.username}
                {...register('username')}
              />
            </Field>
            <Field label="Password" htmlFor="password" error={errors.password?.message}>
              <Input
                id="password"
                type="password"
                autoComplete="current-password"
                aria-invalid={!!errors.password}
                {...register('password')}
              />
            </Field>
          </CardContent>
          <CardFooter className="mt-6 flex-col items-stretch gap-3">
            <Button type="submit" disabled={login.isPending}>
              {login.isPending ? 'Logging in...' : 'Log in'}
            </Button>
            {localLogin && setup.data?.openRegistration && (
              <p className="text-center text-sm text-muted-foreground">
                No account yet?{' '}
                <Link
                  to="/register"
                  search={{}}
                  className="text-primary underline-offset-4 hover:underline"
                >
                  Register
                </Link>
              </p>
            )}
          </CardFooter>
        </form>
      )}
    </Card>
  )
}

function StatusCard({
  icon,
  title,
  children,
}: {
  icon: React.ReactNode
  title: string
  children: React.ReactNode
}) {
  return (
    <Card>
      <CardContent className="flex flex-col items-center gap-3 text-center">
        {icon}
        <h2 className="text-lg font-semibold">{title}</h2>
        <div className="text-sm text-muted-foreground">{children}</div>
      </CardContent>
    </Card>
  )
}

// Closing is a best effort: browsers only allow it for windows a script opened.
function useAutoClose(active: boolean) {
  useEffect(() => {
    if (!active) return
    const t = setTimeout(() => window.close(), 4000)
    return () => clearTimeout(t)
  }, [active])
}

function ApprovalPanel({ token, me }: { token: string; me: Me }) {
  const { data: session, error, isLoading } = useLoginSession(token, true)
  const resolve = useResolveLoginSession(token)
  const logout = useLogout()
  const navigate = useNavigate()
  const isPending = resolve.isPending
  useAutoClose(session?.status === 'completed')

  if (isLoading) return <Skeleton className="h-64 w-full" />
  if (error instanceof ApiError && error.status === 404) {
    return (
      <StatusCard
        icon={<XCircle className="size-10 text-muted-foreground" />}
        title="Request not found"
      >
        This link is not valid. Start the login again from Zotero.
      </StatusCard>
    )
  }
  if (error || !session) {
    return (
      <StatusCard
        icon={<XCircle className="size-10 text-destructive" />}
        title="Could not load request"
      >
        {errorMessage(error)}
      </StatusCard>
    )
  }
  return (
    <SessionView
      session={session}
      me={me}
      isPending={isPending}
      onResolve={(a) => resolve.mutate(a, { onError: (e) => toast.error(errorMessage(e)) })}
      onSwitch={async () => {
        await logout.mutateAsync().catch(() => undefined)
        navigate({ to: '/login', search: { session: token } })
      }}
    />
  )
}

function SessionView({
  session,
  me,
  isPending,
  onResolve,
  onSwitch,
}: {
  session: LoginSession
  me: Me
  isPending: boolean
  onResolve: (a: 'approve' | 'cancel') => void
  onSwitch: () => void
}) {
  if (session.status === 'completed') {
    return (
      <StatusCard
        icon={<CheckCircle2 className="size-10 text-success" />}
        title="Zotero is connected"
      >
        You can return to Zotero now. This tab will try to close itself.
      </StatusCard>
    )
  }
  if (session.status === 'cancelled') {
    return (
      <StatusCard
        icon={<XCircle className="size-10 text-muted-foreground" />}
        title="Request denied"
      >
        Nothing was shared with {session.clientName}. You can close this tab.
      </StatusCard>
    )
  }
  if (session.status === 'expired' || new Date(session.expiresAt) < new Date()) {
    return (
      <StatusCard
        icon={<Clock className="size-10 text-muted-foreground" />}
        title="Request expired"
      >
        This request is too old. Start the login again from Zotero.
      </StatusCard>
    )
  }
  const mismatch = session.requiredUserId !== null && session.requiredUserId !== me.id
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-xl">Connect Zotero</CardTitle>
        <CardDescription>
          Allow <strong className="text-foreground">{session.clientName}</strong> to access your
          library?
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        <div className="rounded-md border bg-muted/40 p-3 text-sm">
          <p className="text-xs text-muted-foreground">It will be linked to</p>
          <p className="font-medium">{me.displayName || me.username}</p>
          <p className="text-muted-foreground">@{me.username}</p>
        </div>
        {mismatch && (
          <Alert variant="warning">
            <ShieldAlert />
            <AlertTitle>Wrong account</AlertTitle>
            <AlertDescription>
              This Zotero profile is already linked to a different gotero account. Log out and sign
              in as that user to approve.
            </AlertDescription>
          </Alert>
        )}
      </CardContent>
      <CardFooter className="mt-1 flex-col items-stretch gap-2 sm:flex-row sm:justify-end">
        {mismatch ? (
          <Button onClick={onSwitch}>Log in as a different user</Button>
        ) : (
          <Button onClick={() => onResolve('approve')} disabled={isPending}>
            Approve
          </Button>
        )}
        <Button variant="outline" onClick={() => onResolve('cancel')} disabled={isPending}>
          Deny
        </Button>
      </CardFooter>
    </Card>
  )
}

export default function LoginPage() {
  usePageTitle('Log in')
  const { session, redirect, error } = useSearch({ from: '/login' })
  const navigate = useNavigate()
  const { data: me, isLoading } = useMe()

  useEffect(() => {
    if (me && !session) {
      // Only follow same-origin paths so the redirect param cannot be used for open redirects.
      const target =
        redirect && redirect.startsWith('/') && !redirect.startsWith('//') ? redirect : '/'
      void navigate({ to: target, replace: true })
    }
  }, [me, session, redirect, navigate])

  return (
    <AuthLayout>
      {isLoading ? (
        <Skeleton className="h-72 w-full" />
      ) : me && session ? (
        <ApprovalPanel token={session} me={me} />
      ) : me ? null : (
        <>
          {session && (
            <p className="mb-4 text-center text-sm text-muted-foreground">
              Log in to connect your Zotero app.
            </p>
          )}
          <LoginForm session={session} redirect={redirect} error={error} />
        </>
      )}
    </AuthLayout>
  )
}
