import { expect, test } from '@playwright/test'
import { admin, AdminApi, baseURL, login, logout } from './support'

test('admin logs in and sees the dashboard', async ({ page }) => {
  await page.goto('/')
  await expect(page).toHaveURL(/\/login\?redirect=/)

  await login(page, admin)
  await expect(page).toHaveURL(new URL('/', baseURL).href)

  // Admin-only parts come from /admin/stats and /me.isAdmin; if either
  // changes shape the tiles or the nav section quietly disappear.
  const stats = page.getByRole('region', { name: 'Server statistics' })
  await expect(stats.getByText('Users', { exact: true })).toBeVisible()
  await expect(stats.getByText(/^Storage \(\w+\)$/)).toBeVisible()
  const users = stats.getByText('Users', { exact: true }).locator('xpath=following-sibling::p')
  expect(Number(await users.textContent())).toBeGreaterThanOrEqual(1)
  for (const item of ['Users', 'Invites', 'Settings']) {
    await expect(
      page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: item }),
    ).toBeVisible()
  }

  // The connect card renders /setup.publicUrl; a missing field would show "undefined/api/".
  await expect(page.getByLabel('API URL', { exact: true })).toHaveText(/^https?:\/\/[^/]+\/api\/$/)
})

test('wrong password shows an error and does not log in', async ({ page }) => {
  await page.goto('/login')
  await page.getByLabel('Username').fill(admin.username)
  await page.getByLabel('Password', { exact: true }).fill(`${admin.password}-wrong`)
  await page.getByRole('button', { name: 'Log in' }).click()

  await expect(page.getByRole('alert')).toHaveText('Invalid username or password.')
  await expect(page).toHaveURL(/\/login$/)
  expect((await page.request.get('/admin/api/me')).status()).toBe(401)
})

test('logout ends the session and protected pages send you to login', async ({ page }) => {
  const api = await AdminApi.login()
  const user = await api.createUser('logout')
  await api.dispose()

  await login(page, user)
  await page.goto('/account')
  await expect(page.getByRole('heading', { name: 'Account' })).toBeVisible()

  await logout(page)
  expect((await page.request.get('/admin/api/me')).status()).toBe(401)

  for (const path of ['/', '/account', '/groups']) {
    await page.goto(path)
    await expect(page).toHaveURL(new RegExp(`/login\\?redirect=${encodeURIComponent(path)}$`))
    await expect(page.getByRole('button', { name: 'Log in' })).toBeVisible()
  }

  // Logging back in from the redirect lands on the page that was asked for.
  await page.getByLabel('Username').fill(user.username)
  await page.getByLabel('Password', { exact: true }).fill(user.password)
  await page.getByRole('button', { name: 'Log in' }).click()
  await expect(page).toHaveURL(/\/groups$/)
  await expect(page.getByRole('heading', { name: 'Groups' })).toBeVisible()
})
