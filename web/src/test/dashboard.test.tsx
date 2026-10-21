import { describe, expect, it } from 'vitest'
import { screen } from '@testing-library/react'
import { renderApp } from './utils'

describe('dashboard', () => {
  it('shows the config values derived from the public url', async () => {
    renderApp('/', { loginAs: 2 })
    const api = await screen.findByLabelText('API URL')
    expect(api.textContent).toBe(`${window.location.origin}/api/`)
    expect(screen.getByLabelText('streaming URL').textContent).toMatch(/^ws:\/\/.+\/stream$/)
  })

  it('shows server stats only to admins', async () => {
    const { unmount } = renderApp('/', { loginAs: 1 })
    expect(await screen.findByLabelText('Server statistics')).toBeInTheDocument()
    unmount()
    renderApp('/', { loginAs: 2 })
    await screen.findByLabelText('API URL')
    expect(screen.queryByLabelText('Server statistics')).not.toBeInTheDocument()
  })
})
