import { expect, test } from '@playwright/test'
import { AdminApi, login } from './support'

test('changing the password logs out every other session', async ({ page, browser }) => {
  const api = await AdminApi.login()
  const user = await api.createUser('passwd')
  await api.dispose()
  const newPassword = `${user.password}-new`

  const other = await browser.newContext()
  try {
    const elsewhere = await other.newPage()
    await login(elsewhere, user)
    await login(page, user)

    await page.goto('/account')
    const card = page.locator('form').filter({ has: page.getByLabel('Current password') })
    await card.getByLabel('Current password').fill('not-the-password')
    await card.getByLabel('New password', { exact: true }).fill(newPassword)
    await card.getByLabel('Confirm new password').fill(newPassword)
    await card.getByRole('button', { name: 'Change password' }).click()
    // The server reports this as a field error; it has to land on the field.
    await expect(card.getByRole('alert')).toHaveText('Current password is incorrect.')

    await card.getByLabel('Current password').fill(user.password)
    await card.getByRole('button', { name: 'Change password' }).click()
    await expect(page.getByText('Password changed')).toBeVisible()

    // This session survives, and not just for reads: the server swapped the
    // session (and its CSRF token) underneath the page, so prove a write works.
    await page.reload()
    await expect(page.getByRole('heading', { name: 'Account' })).toBeVisible()
    await page.getByLabel('Display name').fill('Renamed After Password Change')
    await page.getByRole('button', { name: 'Save changes' }).click()
    await expect(page.getByText('Profile updated')).toBeVisible()

    // The other browser is out.
    await elsewhere.goto('/account')
    await expect(elsewhere).toHaveURL(/\/login\?redirect=/)
    expect((await elsewhere.request.get('/admin/api/me')).status()).toBe(401)

    await elsewhere.getByLabel('Username').fill(user.username)
    await elsewhere.getByLabel('Password', { exact: true }).fill(user.password)
    await elsewhere.getByRole('button', { name: 'Log in' }).click()
    await expect(elsewhere.getByRole('alert')).toHaveText('Invalid username or password.')

    await elsewhere.getByLabel('Password', { exact: true }).fill(newPassword)
    await elsewhere.getByRole('button', { name: 'Log in' }).click()
    await expect(elsewhere).toHaveURL(/\/account$/)
    await expect(elsewhere.getByRole('heading', { name: 'Account' })).toBeVisible()
  } finally {
    await other.close()
  }
})

test('a password change without a reload keeps the page usable', async ({ page }) => {
  const api = await AdminApi.login()
  const user = await api.createUser('passwd')
  await api.dispose()
  const newPassword = `${user.password}-new`

  await login(page, user)
  await page.goto('/account')
  await page.getByLabel('Current password').fill(user.password)
  await page.getByLabel('New password', { exact: true }).fill(newPassword)
  await page.getByLabel('Confirm new password').fill(newPassword)
  await page.getByRole('button', { name: 'Change password' }).click()
  await expect(page.getByText('Password changed')).toBeVisible()

  // Same page, no reload: the next write must carry whatever CSRF token the
  // new session expects.
  await page.getByRole('button', { name: 'Create key' }).click()
  const dialog = page.getByRole('dialog', { name: 'Create API key' })
  await dialog.getByLabel('Name').fill('after password change')
  await dialog.getByRole('button', { name: 'Create key' }).click()
  await expect(page.getByRole('dialog', { name: 'Your new API key' })).toBeVisible()
})
