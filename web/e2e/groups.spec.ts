import { expect, test } from '@playwright/test'
import { AdminApi, login, unique } from './support'

test('a user creates a group, adds and removes a member, then deletes it', async ({
  page,
  browser,
}) => {
  const api = await AdminApi.login()
  const owner = await api.createUser('owner')
  const member = await api.createUser('member')
  await api.dispose()
  const name = unique('group')

  await login(page, owner)
  await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Groups' }).click()
  await page.getByRole('button', { name: 'Create group' }).first().click()
  const dialog = page.getByRole('dialog', { name: 'Create group' })
  await dialog.getByLabel('Name').fill(name)
  await dialog.getByLabel('Description').fill('made by the e2e suite')
  await dialog.getByRole('button', { name: 'Create group' }).click()

  await expect(page).toHaveURL(/\/groups\/\d+$/)
  const groupPath = new URL(page.url()).pathname
  await expect(page.getByRole('heading', { name })).toBeVisible()
  await expect(page.getByText(`Owned by @${owner.username}`)).toBeVisible()
  await expect(page.getByLabel('Name')).toHaveValue(name)
  await expect(page.getByLabel('Description')).toHaveValue('made by the e2e suite')
  const ownerRow = page.getByRole('row').filter({ hasText: `@${owner.username}` })
  await expect(ownerRow.getByText('owner', { exact: true })).toBeVisible()

  await page.getByRole('combobox').filter({ hasText: 'Search for a user...' }).click()
  await page.getByLabel('Search users').fill(member.username)
  await page
    .getByRole('option')
    .filter({ hasText: `@${member.username}` })
    .click()
  await page.getByRole('button', { name: 'Add member' }).click()
  await expect(page.getByText(`Added @${member.username}`)).toBeVisible()
  const memberRow = page.getByRole('row').filter({ hasText: `@${member.username}` })
  await expect(memberRow.getByRole('combobox', { name: `Role of ${member.username}` })).toHaveText(
    'Member',
  )

  // The member sees the group, read-only.
  const memberCtx = await browser.newContext()
  try {
    const m = await memberCtx.newPage()
    await login(m, member)
    await m.goto('/groups')
    await m.getByRole('link', { name: new RegExp(name) }).click()
    await expect(m).toHaveURL(new RegExp(`${groupPath}$`))
    await expect(m.getByText('Only group admins can change these.')).toBeVisible()
    await expect(m.getByLabel('Name')).toBeDisabled()
    await expect(m.getByRole('button', { name: 'Add member' })).toHaveCount(0)
    await expect(m.getByRole('button', { name: 'Delete this group' })).toHaveCount(0)
    await m.goto('/account')
    // "My groups" on the account page comes from /me/groups, a separate endpoint.
    const mine = m.getByRole('listitem').filter({ has: m.getByRole('link', { name }) })
    await expect(mine).toContainText('member')

    await memberRow.getByRole('button', { name: `Remove ${member.username}` }).click()
    await page
      .getByRole('alertdialog', { name: 'Remove member?' })
      .getByRole('button', { name: 'Remove' })
      .click()
    await expect(page.getByText('Member removed')).toBeVisible()
    await expect(memberRow).toHaveCount(0)

    await m.reload()
    await expect(m.getByText('Not in any groups')).toBeVisible()
  } finally {
    await memberCtx.close()
  }

  await page.getByRole('button', { name: 'Delete this group' }).click()
  const confirm = page.getByRole('alertdialog', { name: 'Delete this group?' })
  const del = confirm.getByRole('button', { name: 'Delete group' })
  await expect(del).toBeDisabled()
  await confirm.getByLabel(/to confirm/).fill(name)
  await del.click()
  await expect(page).toHaveURL(/\/groups$/)
  await expect(page.getByText('Group deleted')).toBeVisible()
  await expect(page.getByRole('link', { name: new RegExp(name) })).toHaveCount(0)
})
