import { describe, expect, it } from 'vitest'
import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { renderApp } from './utils'
import { db } from '@/mocks/db'

describe('API keys', () => {
  it('creates a key, shows it once, and never again', async () => {
    const user = userEvent.setup()
    renderApp('/account', { loginAs: 2 })
    await user.click(await screen.findByRole('button', { name: /create key/i }))
    const dialog = await screen.findByRole('dialog')
    await user.type(within(dialog).getByLabelText('Name'), 'Backup script')
    await user.click(within(dialog).getByRole('button', { name: 'Create key' }))

    const shown = await screen.findByLabelText('API key')
    const secret = db.keys[2]![0]!
    expect(shown.textContent).toHaveLength(24)
    expect(shown.textContent?.startsWith(secret.prefix)).toBe(true)

    await user.click(screen.getByRole('button', { name: 'Done' }))
    expect(screen.queryByLabelText('API key')).not.toBeInTheDocument()
    // The list only exposes the prefix.
    expect(await screen.findByText('Backup script')).toBeInTheDocument()
    expect(screen.queryByText(shown.textContent!)).not.toBeInTheDocument()
  })

  it('revokes a key after confirmation', async () => {
    const user = userEvent.setup()
    renderApp('/account', { loginAs: 1 })
    await user.click(await screen.findByRole('button', { name: 'Revoke Laptop' }))
    const confirm = await screen.findByRole('alertdialog')
    await user.click(within(confirm).getByRole('button', { name: 'Revoke key' }))
    expect(await screen.findByText('No API keys')).toBeInTheDocument()
  })

  it('requires matching passwords when changing the password', async () => {
    const user = userEvent.setup()
    renderApp('/account', { loginAs: 2 })
    await user.type(await screen.findByLabelText('Current password'), 'password')
    await user.type(screen.getByLabelText('New password'), 'longenough1')
    await user.type(screen.getByLabelText('Confirm new password'), 'different1')
    await user.click(screen.getByRole('button', { name: 'Change password' }))
    expect(await screen.findByText('Passwords do not match')).toBeInTheDocument()
  })
})
