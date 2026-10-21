import { describe, expect, it } from 'vitest'
import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { renderApp } from './utils'
import { db } from '@/mocks/db'

describe('admin users', () => {
  it('lists users and filters by search', async () => {
    const user = userEvent.setup()
    renderApp('/admin/users', { loginAs: 1 })
    expect(await screen.findByText('Alice Archer')).toBeInTheDocument()
    expect(screen.getByText('Bob Bishop')).toBeInTheDocument()
    await user.type(screen.getByLabelText('Search users'), 'bob')
    expect(await screen.findByText('Bob Bishop')).toBeInTheDocument()
    await screen.findByText('Bob Bishop')
    await new Promise((r) => setTimeout(r, 400))
    expect(screen.queryByText('Alice Archer')).not.toBeInTheDocument()
  })

  it('creates a user through the dialog', async () => {
    const user = userEvent.setup()
    renderApp('/admin/users', { loginAs: 1 })
    await user.click(await screen.findByRole('button', { name: /create user/i }))
    const dialog = await screen.findByRole('dialog')
    await user.type(within(dialog).getByLabelText('Username'), 'dana')
    await user.type(within(dialog).getByLabelText('Display name'), 'Dana Dunn')
    await user.type(within(dialog).getByLabelText('Email'), 'dana@example.org')
    await user.type(within(dialog).getByLabelText('Initial password'), 'supersecret')
    await user.click(within(dialog).getByRole('button', { name: 'Create user' }))
    expect(await screen.findByText('Dana Dunn')).toBeInTheDocument()
    expect(db.users.some((u) => u.username === 'dana')).toBe(true)
  })

  it('shows a server field error for a duplicate username', async () => {
    const user = userEvent.setup()
    renderApp('/admin/users', { loginAs: 1 })
    await user.click(await screen.findByRole('button', { name: /create user/i }))
    const dialog = await screen.findByRole('dialog')
    await user.type(within(dialog).getByLabelText('Username'), 'alice')
    await user.type(within(dialog).getByLabelText('Display name'), 'Other Alice')
    await user.type(within(dialog).getByLabelText('Email'), 'a2@example.org')
    await user.type(within(dialog).getByLabelText('Initial password'), 'supersecret')
    await user.click(within(dialog).getByRole('button', { name: 'Create user' }))
    expect(await within(dialog).findByText('Already taken.')).toBeInTheDocument()
  })

  it('edits a user from the sheet', async () => {
    const user = userEvent.setup()
    renderApp('/admin/users', { loginAs: 1 })
    await user.click(await screen.findByRole('button', { name: 'Edit bob' }))
    const sheet = await screen.findByRole('dialog')
    const name = within(sheet).getByLabelText('Display name')
    await user.clear(name)
    await user.type(name, 'Robert Bishop')
    await user.click(within(sheet).getByRole('switch', { name: /disabled/i }))
    await user.click(within(sheet).getByRole('button', { name: 'Save changes' }))
    expect(await screen.findByText('Robert Bishop')).toBeInTheDocument()
    expect(db.users.find((u) => u.id === 3)).toMatchObject({
      displayName: 'Robert Bishop',
      disabled: true,
    })
  })

  it('requires typing the username to delete a user', async () => {
    const user = userEvent.setup()
    renderApp('/admin/users', { loginAs: 1 })
    await user.click(await screen.findByRole('button', { name: 'Delete bob' }))
    const confirm = await screen.findByRole('alertdialog')
    const button = within(confirm).getByRole('button', { name: 'Delete user' })
    expect(button).toBeDisabled()
    await user.type(within(confirm).getByLabelText(/to confirm/i), 'bob')
    expect(button).toBeEnabled()
    await user.click(button)
    await within(document.body).findByText('User deleted')
    expect(db.users.some((u) => u.username === 'bob')).toBe(false)
  })
})
