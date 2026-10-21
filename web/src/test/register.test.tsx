import { describe, expect, it } from 'vitest'
import { screen } from '@testing-library/react'
import { renderApp } from './utils'
import { db } from '@/mocks/db'

describe('register', () => {
  it('explains that registration is closed without an invite', async () => {
    renderApp('/register')
    expect(await screen.findByText('Registration is closed')).toBeInTheDocument()
  })

  it('shows the form for a valid invite and prefills the invited email', async () => {
    renderApp('/register?invite=inv_abc123')
    expect(await screen.findByRole('button', { name: 'Create account' })).toBeInTheDocument()
    expect(await screen.findByDisplayValue('carol@example.org')).toBeInTheDocument()
  })

  it('rejects an already used invite', async () => {
    renderApp('/register?invite=inv_used99')
    expect(await screen.findByText('Invitation not valid')).toBeInTheDocument()
  })

  it('allows registration without an invite when it is open', async () => {
    db.settings.openRegistration = true
    renderApp('/register')
    expect(await screen.findByRole('button', { name: 'Create account' })).toBeInTheDocument()
  })
})
