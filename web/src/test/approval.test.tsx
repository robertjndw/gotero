import { describe, expect, it } from 'vitest'
import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { renderApp } from './utils'
import { db } from '@/mocks/db'

describe('Zotero client approval', () => {
  it('approves a pending request for a logged-in user', async () => {
    const user = userEvent.setup()
    renderApp('/login?session=good', { loginAs: 1 })
    expect(await screen.findByText(/Allow/)).toBeInTheDocument()
    expect(screen.getByText('@admin')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Approve' }))
    expect(await screen.findByText(/You can return to Zotero now/)).toBeInTheDocument()
    expect(db.loginSessions.find((s) => s.token === 'good')?.status).toBe('completed')
  })

  it('asks anonymous users to log in first, then shows the approval screen', async () => {
    const user = userEvent.setup()
    renderApp('/login?session=good')
    await user.type(await screen.findByLabelText('Username'), 'alice')
    await user.type(screen.getByLabelText('Password'), 'password')
    await user.click(screen.getByRole('button', { name: 'Log in' }))
    expect(await screen.findByRole('button', { name: 'Approve' })).toBeInTheDocument()
    expect(screen.getByText('@alice')).toBeInTheDocument()
  })

  it('warns and disables approval when logged in as the wrong user', async () => {
    renderApp('/login?session=bound', { loginAs: 1 })
    expect(await screen.findByText('Wrong account')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /log in as a different user/i })).toBeInTheDocument()
  })

  it('allows the bound user to approve', async () => {
    renderApp('/login?session=bound', { loginAs: 2 })
    expect(await screen.findByRole('button', { name: 'Approve' })).toBeEnabled()
    expect(screen.queryByText('Wrong account')).not.toBeInTheDocument()
  })

  it('denies a request', async () => {
    const user = userEvent.setup()
    renderApp('/login?session=good', { loginAs: 1 })
    await user.click(await screen.findByRole('button', { name: 'Deny' }))
    expect(await screen.findByText('Request denied')).toBeInTheDocument()
  })

  it('shows an expired state', async () => {
    renderApp('/login?session=old', { loginAs: 1 })
    expect(await screen.findByText('Request expired')).toBeInTheDocument()
  })

  it('handles unknown sessions gracefully', async () => {
    renderApp('/login?session=missing', { loginAs: 1 })
    expect(await screen.findByText('Request not found')).toBeInTheDocument()
  })
})
