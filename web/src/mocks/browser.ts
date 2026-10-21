import { setupWorker } from 'msw/browser'
import { handlers } from './handlers'
import { restoreSession } from './db'

export async function startMocking() {
  restoreSession()
  await setupWorker(...handlers).start({ onUnhandledFrame: 'bypass', quiet: true })
}
