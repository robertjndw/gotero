import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from '@tanstack/react-router'
import { Toaster } from '@/components/ui/sonner'
import { ThemeProvider } from '@/components/theme'
import { ApiError } from '@/lib/api'
import { createAppRouter } from '@/router'
import './index.css'

// Gated on DEV so the MSW code is dead-stripped from the production bundle that the server embeds.
const mock =
  import.meta.env.DEV &&
  (import.meta.env.VITE_MOCK === '1' ||
    (import.meta.env.VITE_MOCK !== '0' && !import.meta.env.VITE_API_URL))

async function boot() {
  if (mock) {
    const { startMocking } = await import('@/mocks/browser')
    await startMocking()
  }
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: {
        refetchOnWindowFocus: false,
        // Retrying a 4xx never helps and only delays the error state.
        retry: (n, e) => !(e instanceof ApiError && e.status >= 400 && e.status < 500) && n < 2,
      },
    },
  })
  const router = createAppRouter(queryClient)
  createRoot(document.getElementById('root')!).render(
    <StrictMode>
      <ThemeProvider>
        <QueryClientProvider client={queryClient}>
          <RouterProvider router={router} />
          <Toaster richColors closeButton />
        </QueryClientProvider>
      </ThemeProvider>
    </StrictMode>,
  )
}
void boot()
