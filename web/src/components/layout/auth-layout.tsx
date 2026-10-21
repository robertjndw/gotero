import type { ReactNode } from 'react'
import { Wordmark } from './wordmark'
import { Footer, ThemeToggle } from './app-shell'

export function AuthLayout({ children }: { children: ReactNode }) {
  return (
    <div className="flex min-h-dvh flex-col">
      <div className="flex justify-end p-4">
        <ThemeToggle />
      </div>
      <main className="flex flex-1 flex-col items-center justify-center gap-8 px-4 pb-16">
        <Wordmark className="text-2xl" />
        <div className="w-full max-w-sm">{children}</div>
      </main>
      <footer className="px-4 py-4 text-center">
        <Footer />
      </footer>
    </div>
  )
}
