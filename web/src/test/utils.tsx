import { render } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from '@tanstack/react-router'
import { Toaster } from '@/components/ui/sonner'
import { ThemeProvider } from '@/components/theme'
import { createAppRouter, createMemoryHistory } from '@/router'
import { db } from '@/mocks/db'

export function renderApp(path: string, opts: { loginAs?: number } = {}) {
  if (opts.loginAs) db.sessionUserId = opts.loginAs
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  })
  const router = createAppRouter(queryClient, createMemoryHistory({ initialEntries: [path] }))
  const utils = render(
    <ThemeProvider>
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router} />
        <Toaster />
      </QueryClientProvider>
    </ThemeProvider>,
  )
  return { ...utils, router, queryClient }
}
