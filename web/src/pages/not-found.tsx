import { Link } from '@tanstack/react-router'
import { Button } from '@/components/ui/button'
import { usePageTitle } from '@/components/common'

export function NotFound() {
  usePageTitle('Not found')
  return (
    <div className="flex min-h-[50dvh] flex-col items-center justify-center gap-4 text-center">
      <p className="text-5xl font-semibold tracking-tight">404</p>
      <p className="text-muted-foreground">This page does not exist.</p>
      <Button asChild>
        <Link to="/">Back to dashboard</Link>
      </Button>
    </div>
  )
}
