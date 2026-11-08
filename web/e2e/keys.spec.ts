import { expect, test } from '@playwright/test'
import { AdminApi, keyInfo, login } from './support'

test('an API key is shown once, grants what was ticked, and dies when revoked', async ({
  page,
}) => {
  const api = await AdminApi.login()
  const user = await api.createUser('keys')
  await api.dispose()

  await login(page, user)
  await page.goto('/account')
  await page.getByRole('button', { name: 'Create key' }).click()

  const dialog = page.getByRole('dialog', { name: 'Create API key' })
  await dialog.getByLabel('Name').fill('e2e script')
  // Defaults are library, notes and files on, write off, no groups. Flip
  // some of each so a dropped or renamed field cannot pass by accident.
  await dialog.getByLabel('Read notes').uncheck()
  await dialog.getByLabel('Write access').check()
  await dialog.getByLabel('Group access').click()
  await page.getByRole('option', { name: 'Read-only access to all groups' }).click()
  await dialog.getByRole('button', { name: 'Create key' }).click()

  const shown = page.getByRole('dialog', { name: 'Your new API key' })
  await expect(shown.getByText('The full key is only shown once.')).toBeVisible()
  const key = (await shown.getByLabel('API key', { exact: true }).textContent()) ?? ''
  expect(key).toMatch(/^[A-Za-z0-9]{24}$/)
  await shown.getByRole('button', { name: 'Done' }).click()
  await expect(shown).toBeHidden()

  const row = page.getByRole('listitem').filter({ hasText: 'e2e script' })
  await expect(row).toContainText(`${key.slice(0, 4)}...`)
  await expect(row).toContainText('library, files, write, groups (read)')
  await page.reload()
  await expect(row).toBeVisible()
  await expect(page.locator('body')).not.toContainText(key)

  // The key has to work against the real Zotero API, with exactly the
  // access the dialog promised.
  const info = await keyInfo(key)
  expect(info.status).toBe(200)
  expect(info.body).toEqual({
    key,
    userID: user.id,
    username: user.username,
    displayName: user.displayName,
    access: {
      user: { library: true, files: true, write: true },
      groups: { all: { library: true } },
    },
  })

  await row.getByRole('button', { name: 'Revoke e2e script' }).click()
  const confirm = page.getByRole('alertdialog', { name: 'Revoke API key?' })
  await confirm.getByRole('button', { name: 'Revoke key' }).click()
  await expect(page.getByText('API key revoked')).toBeVisible()
  await expect(confirm).toBeHidden()
  await expect(page.getByText('No API keys')).toBeVisible()

  expect((await keyInfo(key)).status).toBe(403)
})
