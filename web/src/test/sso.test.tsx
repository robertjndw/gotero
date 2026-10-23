import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { renderApp } from './utils'
import { db } from '@/mocks/db'

describe('login with SSO', () => {
  it('renders one plain link per provider', async () => {
    renderApp('/login')
    const oidc = await screen.findByRole('link', { name: 'Log in with Keycloak' })
    expect(oidc).toHaveAttribute('href', '/auth/oidc/keycloak/start')
    expect(screen.getByRole('link', { name: 'Log in with Corp SAML' })).toHaveAttribute(
      'href',
      '/auth/saml/corp-saml/start',
    )
    expect(screen.getByLabelText('Username')).toBeInTheDocument()
  })

  it('passes the Zotero session and redirect through to the provider', async () => {
    renderApp('/login?session=abc123&redirect=%2Fgroups')
    const link = await screen.findByRole('link', { name: 'Log in with Keycloak' })
    const url = new URL(link.getAttribute('href')!, 'http://localhost')
    expect(url.pathname).toBe('/auth/oidc/keycloak/start')
    expect(url.searchParams.get('session')).toBe('abc123')
    expect(url.searchParams.get('redirect')).toBe('/groups')
  })

  it('hides the password form and register link when local login is off', async () => {
    db.localLogin = false
    db.settings.openRegistration = true
    const user = userEvent.setup()
    renderApp('/login')
    await screen.findByRole('link', { name: 'Log in with Keycloak' })
    const toggle = await screen.findByRole('button', { name: 'Administrator sign-in' })
    expect(screen.queryByLabelText('Username')).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'Register' })).not.toBeInTheDocument()

    await user.click(toggle)
    await user.type(await screen.findByLabelText('Username'), 'admin')
    await user.type(screen.getByLabelText('Password'), 'password')
    await user.click(screen.getByRole('button', { name: 'Log in' }))
    expect(await screen.findByRole('heading', { name: /welcome/i })).toBeInTheDocument()
  })

  it.each([
    ['sso_failed', 'Single sign-on failed. Please try again.'],
    [
      'sso_state',
      'The sign-in attempt expired or was started in another browser. Please try again.',
    ],
    ['unknown_provider', 'That sign-in provider does not exist.'],
    ['no_account', 'No account is linked to that sign-in. Ask an administrator for an invite.'],
    ['disabled', 'This account is disabled.'],
    ['invalid_invite', 'This invitation is expired, already used or unknown.'],
    ['invite_email_mismatch', 'The email address of that sign-in does not match the invitation.'],
    [
      'link_required',
      'An account with this email already exists. Log in to it and connect the provider from your account page.',
    ],
    ['identity_in_use', 'That sign-in is already linked to another account.'],
  ])('maps the %s error code to a friendly message', async (code, message) => {
    renderApp(`/login?error=${code}`)
    expect(await screen.findByRole('alert')).toHaveTextContent(message)
  })

  it('never renders an unknown error string from the URL', async () => {
    renderApp('/login?error=%3Cb%3Eevil%3C%2Fb%3E')
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('Sign-in failed. Please try again.')
    expect(screen.queryByText(/evil/)).not.toBeInTheDocument()
  })
})

describe('register with SSO', () => {
  it('offers sign up buttons that carry the invite token', async () => {
    renderApp('/register?invite=inv_abc123')
    const link = await screen.findByRole('link', { name: 'Sign up with Keycloak' })
    const url = new URL(link.getAttribute('href')!, 'http://localhost')
    expect(url.searchParams.get('invite')).toBe('inv_abc123')
    expect(screen.getByRole('link', { name: 'Sign up with Corp SAML' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Create account' })).toBeInTheDocument()
  })

  it('hides the password form when local login is off', async () => {
    db.localLogin = false
    renderApp('/register?invite=inv_abc123')
    expect(await screen.findByRole('link', { name: 'Sign up with Keycloak' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Create account' })).not.toBeInTheDocument()
  })

  it('shows the error alert', async () => {
    renderApp('/register?invite=inv_abc123&error=invite_email_mismatch')
    expect(await screen.findByRole('alert')).toHaveTextContent(/does not match the invitation/)
  })

  it('does not offer SSO signup without an invite', async () => {
    db.settings.openRegistration = true
    renderApp('/register')
    expect(await screen.findByRole('button', { name: 'Create account' })).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /Sign up with/ })).not.toBeInTheDocument()
  })
})

describe('account sign-in methods', () => {
  it('lists identities and offers to connect providers that are not linked', async () => {
    renderApp('/account', { loginAs: 2 })
    await screen.findByText('Sign-in methods')
    await screen.findByRole('link', { name: 'Connect Corp SAML' })
    const scope = screen
    expect(scope.getByText('Keycloak')).toBeInTheDocument()
    expect(scope.getByText(/alice@example\.org/)).toBeInTheDocument()
    expect(scope.getByRole('link', { name: 'Connect Corp SAML' })).toHaveAttribute(
      'href',
      '/auth/saml/corp-saml/start?link=1',
    )
    expect(scope.queryByRole('link', { name: 'Connect Keycloak' })).not.toBeInTheDocument()
  })

  it('unlinks an identity after confirmation', async () => {
    const user = userEvent.setup()
    renderApp('/account', { loginAs: 2 })
    await user.click(await screen.findByRole('button', { name: /Unlink/ }))
    const confirm = await screen.findByRole('alertdialog')
    await user.click(within(confirm).getByRole('button', { name: 'Unlink' }))
    expect(await screen.findByRole('link', { name: 'Connect Keycloak' })).toBeInTheDocument()
    expect(db.identities[2]).toHaveLength(0)
  })

  it('surfaces the server message when it is the last way to sign in', async () => {
    const user = userEvent.setup()
    renderApp('/account', { loginAs: 3 })
    await user.click(await screen.findByRole('button', { name: /Unlink/ }))
    const confirm = await screen.findByRole('alertdialog')
    await user.click(within(confirm).getByRole('button', { name: 'Unlink' }))
    expect(
      await screen.findByText('You cannot remove your only way to sign in.'),
    ).toBeInTheDocument()
    expect(db.identities[3]).toHaveLength(1)
  })

  it('shows the error alert from a failed connect attempt', async () => {
    renderApp('/account?error=identity_in_use', { loginAs: 2 })
    expect(await screen.findByRole('alert')).toHaveTextContent(/already linked to another account/)
  })

  it('lets an SSO-only user set a password without a current one', async () => {
    const user = userEvent.setup()
    renderApp('/account', { loginAs: 3 })
    await screen.findByRole('button', { name: 'Set password' })
    expect(screen.queryByLabelText('Current password')).not.toBeInTheDocument()
    await user.type(screen.getByLabelText('New password'), 'longenough1')
    await user.type(screen.getByLabelText('Confirm new password'), 'longenough1')
    await user.click(screen.getByRole('button', { name: 'Set password' }))
    await waitFor(() => expect(db.passwords[3]).toBe('longenough1'))
    expect(await screen.findByRole('button', { name: 'Change password' })).toBeInTheDocument()
    expect(screen.getByLabelText('Current password')).toBeInTheDocument()
  })

  it('hides the password card for non-admins when local login is off', async () => {
    db.localLogin = false
    renderApp('/account', { loginAs: 2 })
    await screen.findByText('Sign-in methods')
    expect(screen.queryByText('Password')).not.toBeInTheDocument()
  })

  it('keeps the password card for admins when local login is off', async () => {
    db.localLogin = false
    renderApp('/account', { loginAs: 1 })
    expect(await screen.findByRole('button', { name: 'Change password' })).toBeInTheDocument()
  })
})

describe('admin users with SSO', () => {
  it('shows provider badges and unlinks from the edit sheet', async () => {
    const user = userEvent.setup()
    renderApp('/admin/users', { loginAs: 1 })
    await screen.findByText('Alice Archer')
    expect(screen.getAllByText('Keycloak').length).toBeGreaterThanOrEqual(2)
    await user.click(screen.getByRole('button', { name: 'Edit alice' }))
    const sheet = await screen.findByRole('dialog')
    await user.click(within(sheet).getByRole('button', { name: 'Unlink Keycloak' }))
    await waitFor(() => expect(db.users.find((u) => u.id === 2)!.providers).toEqual([]))
    await waitFor(() =>
      expect(
        within(sheet).queryByRole('button', { name: 'Unlink Keycloak' }),
      ).not.toBeInTheDocument(),
    )
  })
})
