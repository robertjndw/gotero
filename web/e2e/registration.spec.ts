import { expect, test } from '@playwright/test'
import { admin, login, logout, unique } from './support'

test('the register page follows the server registration setting', async ({ page }) => {
  const setup = (await (await page.request.get('/admin/api/setup')).json()) as {
    openRegistration: boolean
  }
  await page.goto('/register')
  if (setup.openRegistration) {
    await expect(page.getByText('Registration is open on this server.')).toBeVisible()
  } else {
    await expect(page.getByText('Registration is closed')).toBeVisible()
    await expect(page.getByLabel('Username')).toHaveCount(0)
  }
})

// gotero has no approval queue: closed servers admit people by invite, so
// that is the flow a new user goes through.
test('admin invites a user, who registers and logs in', async ({ page, browser }) => {
  const username = unique('invitee')
  const email = `${username}@example.org`
  const password = `pw-${username}`

  await login(page, admin)
  await page
    .getByRole('navigation', { name: 'Main' })
    .getByRole('link', { name: 'Invites' })
    .click()
  await page.getByRole('button', { name: 'Create invite' }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('Email (optional)').fill(email)
  await dialog.getByRole('button', { name: 'Create invite' }).click()
  await expect(dialog.getByRole('heading', { name: 'Invite created' })).toBeVisible()
  const link = (await dialog.getByLabel('invite link', { exact: true }).textContent()) ?? ''
  expect(link).toMatch(/\/register\?invite=[^&\s]+$/)
  await dialog.getByRole('button', { name: 'Done' }).click()

  // The link carries the server's public URL; follow its path on whatever
  // address this run talks to.
  const invitePath = link.slice(link.indexOf('/register'))
  const guest = await browser.newContext()
  try {
    const p = await guest.newPage()
    await p.goto(invitePath)
    await expect(p.getByText('You have been invited to this server.')).toBeVisible()
    await expect(p.getByLabel('Email', { exact: true })).toHaveValue(email)
    await expect(p.getByLabel('Email', { exact: true })).toHaveAttribute('readonly', '')

    // 10 is auth.MinPasswordLength on the server, which /setup does not expose.
    await expect(p.getByText('At least 10 characters.')).toBeVisible()

    await p.getByLabel('Username').fill(username)
    await p.getByLabel('Display name').fill('Invited Person')
    // One short of the server's minimum: whichever side rejects it, the user
    // must see the same rule the hint promises.
    await p.getByLabel('Password', { exact: true }).fill('123456789')
    await p.getByRole('button', { name: 'Create account' }).click()
    await expect(p.getByRole('alert')).toHaveText(/at least 10 characters/i)

    await p.getByLabel('Password', { exact: true }).fill(password)
    await p.getByRole('button', { name: 'Create account' }).click()
    await expect(p.getByRole('heading', { name: 'Welcome, Invited Person' })).toBeVisible()
    await expect(p.getByText('Administration')).toHaveCount(0)

    await logout(p)
    await login(p, { username, password })

    // Invites are single use.
    await logout(p)
    await p.goto(invitePath)
    await expect(p.getByText('Invitation not valid')).toBeVisible()
  } finally {
    await guest.close()
  }

  await page.reload()
  const row = page.getByRole('row').filter({ hasText: email })
  await expect(row.getByText(`Used by @${username}`)).toBeVisible()
})
