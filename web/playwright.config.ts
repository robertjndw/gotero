import { defineConfig, devices } from '@playwright/test'

// Smoke tests against a real gotero + Postgres, never the MSW mocks. Start a
// server first (`make dev`, or deploy/compose.yaml like CI does) and point
// GOTERO_URL at it.
const baseURL = process.env.GOTERO_URL ?? 'http://localhost:8080'
const ci = !!process.env.CI

export default defineConfig({
  testDir: './e2e',
  fullyParallel: true,
  forbidOnly: ci,
  // No retries: a flaky pass would hide exactly the UI/API drift these
  // tests exist to catch.
  retries: 0,
  // Every login runs an argon2 hash on the server, so a handful of workers
  // is plenty and keeps a small CI runner from timing out.
  workers: ci ? 2 : 4,
  timeout: 60_000,
  expect: { timeout: 10_000 },
  reporter: ci ? [['list'], ['html', { open: 'never' }]] : [['list']],
  use: {
    baseURL,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
})
