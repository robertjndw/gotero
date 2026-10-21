import { useEffect } from 'react'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { useInvite, useRegister, useSetup } from '@/lib/queries'
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
import { Skeleton } from '@/components/ui/skeleton'
import { Field, usePageTitle } from '@/components/common'
import { applyServerError } from '@/lib/forms'

const schema = z.object({
  username: z
    .string()
    .min(3, 'At least 3 characters')
    .regex(/^[A-Za-z0-9._-]+$/, 'Letters, numbers, dots, dashes and underscores only'),
  displayName: z.string().min(1, 'Enter a display name'),
  email: z.email('Enter a valid email address'),
  password: z.string().min(8, 'At least 8 characters'),
})
type Values = z.infer<typeof schema>

function Closed({ title, text }: { title: string; text: string }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-xl">{title}</CardTitle>
        <CardDescription>{text}</CardDescription>
      </CardHeader>
      <CardFooter>
        <Button asChild variant="outline">
          <Link to="/login" search={{}}>
            Back to login
          </Link>
        </Button>
      </CardFooter>
    </Card>
  )
}

export default function RegisterPage() {
  usePageTitle('Register')
  const { invite } = useSearch({ from: '/register' })
  const navigate = useNavigate()
  const setup = useSetup()
  const inv = useInvite(invite)
  const reg = useRegister()
  const {
    register,
    handleSubmit,
    setError,
    setValue,
    formState: { errors },
  } = useForm<Values>({ resolver: zodResolver(schema) })

  const inviteEmail = inv.data?.valid ? inv.data.email : null
  useEffect(() => {
    if (inviteEmail) setValue('email', inviteEmail)
  }, [inviteEmail, setValue])

  const loading = setup.isLoading || (!!invite && inv.isLoading)
  const inviteOk = !!invite && inv.data?.valid
  const allowed = inviteOk || (!invite && setup.data?.openRegistration)

  let body
  if (loading) body = <Skeleton className="h-96 w-full" />
  else if (invite && !inviteOk) {
    body = (
      <Closed
        title="Invitation not valid"
        text="This invitation link is expired, already used or unknown. Ask an administrator for a new one."
      />
    )
  } else if (!allowed) {
    body = (
      <Closed
        title="Registration is closed"
        text="This server accepts new users by invitation only. Ask an administrator for an invite link."
      />
    )
  } else {
    body = (
      <Card>
        <CardHeader>
          <CardTitle className="text-xl">Create your account</CardTitle>
          <CardDescription>
            {inviteOk
              ? 'You have been invited to this server.'
              : 'Registration is open on this server.'}
          </CardDescription>
        </CardHeader>
        <form
          noValidate
          onSubmit={handleSubmit((v) =>
            reg.mutate(
              { ...v, inviteToken: invite },
              {
                onSuccess: () => navigate({ to: '/' }),
                onError: (e) => applyServerError(e, setError),
              },
            ),
          )}
        >
          <CardContent className="grid gap-4">
            <Field label="Username" htmlFor="username" error={errors.username?.message}>
              <Input
                id="username"
                autoComplete="username"
                aria-invalid={!!errors.username}
                {...register('username')}
              />
            </Field>
            <Field label="Display name" htmlFor="displayName" error={errors.displayName?.message}>
              <Input
                id="displayName"
                autoComplete="name"
                aria-invalid={!!errors.displayName}
                {...register('displayName')}
              />
            </Field>
            <Field label="Email" htmlFor="email" error={errors.email?.message}>
              <Input
                id="email"
                type="email"
                autoComplete="email"
                readOnly={!!inviteEmail}
                aria-invalid={!!errors.email}
                {...register('email')}
              />
            </Field>
            <Field
              label="Password"
              htmlFor="password"
              error={errors.password?.message}
              hint="At least 8 characters."
            >
              <Input
                id="password"
                type="password"
                autoComplete="new-password"
                aria-invalid={!!errors.password}
                {...register('password')}
              />
            </Field>
          </CardContent>
          <CardFooter className="mt-6">
            <Button type="submit" className="w-full" disabled={reg.isPending}>
              {reg.isPending ? 'Creating account...' : 'Create account'}
            </Button>
          </CardFooter>
        </form>
      </Card>
    )
  }
  return <AuthLayout>{body}</AuthLayout>
}
