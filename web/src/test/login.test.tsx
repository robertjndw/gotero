import { describe, expect, it } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { renderApp } from './utils'
import { db } from '@/mocks/db'

describe('login', () => {
  it('logs in and lands on the dashboard', async () => {
    const user = userEvent.setup()
    const { router } = renderApp('/login')
    await user.type(await screen.findByLabelText('Username'), 'alice')
    await user.type(screen.getByLabelText('Password'), 'password')
    await user.click(screen.getByRole('button', { name: 'Log in' }))
    expect(
      await screen.findByRole('heading', { name: /welcome, alice archer/i }),
    ).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/')
  })

  it('shows an error for bad credentials and stays on the page', async () => {
    const user = userEvent.setup()
    const { router } = renderApp('/login')
    await user.type(await screen.findByLabelText('Username'), 'alice')
    await user.type(screen.getByLabelText('Password'), 'nope')
    await user.click(screen.getByRole('button', { name: 'Log in' }))
    expect(await screen.findByText('Invalid username or password.')).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/login')
  })

  it('validates empty fields client side', async () => {
    const user = userEvent.setup()
    renderApp('/login')
    await user.click(await screen.findByRole('button', { name: 'Log in' }))
    expect(await screen.findByText('Enter your username')).toBeInTheDocument()
  })

  it('redirects anonymous visitors from protected pages to /login', async () => {
    const { router } = renderApp('/account')
    await waitFor(() => expect(router.state.location.pathname).toBe('/login'))
    expect(await screen.findByRole('button', { name: 'Log in' })).toBeInTheDocument()
  })

  it('redirects to /login when the session expires mid-use (401)', async () => {
    const { router, queryClient } = renderApp('/account', { loginAs: 2 })
    expect(await screen.findByRole('heading', { name: 'Account' })).toBeInTheDocument()
    db.sessionUserId = null
    await queryClient.invalidateQueries({ queryKey: ['me', 'keys'] })
    await waitFor(() => expect(router.state.location.pathname).toBe('/login'))
  })

  it('hides admin navigation from non-admins and blocks admin routes', async () => {
    const { router } = renderApp('/admin/users', { loginAs: 2 })
    await waitFor(() => expect(router.state.location.pathname).toBe('/'))
    expect(screen.queryByRole('link', { name: 'Users' })).not.toBeInTheDocument()
  })
})
