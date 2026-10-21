import { useState } from 'react'
import { Link, Outlet, useNavigate } from '@tanstack/react-router'
import {
  ArrowDownToLine,
  LayoutDashboard,
  LogOut,
  Mail,
  Menu,
  Monitor,
  Moon,
  Settings,
  Sun,
  UserCircle,
  Users,
  UsersRound,
} from 'lucide-react'
import { useMe, useLogout } from '@/lib/queries'
import { initials } from '@/lib/format'
import { cn } from '@/lib/utils'
import { Button } from '@/components/ui/button'
import { Sheet, SheetContent, SheetDescription, SheetTitle } from '@/components/ui/sheet'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { useTheme, type Theme } from '@/components/theme'
import { Wordmark } from './wordmark'

const nav = [
  { to: '/', label: 'Dashboard', icon: LayoutDashboard },
  { to: '/groups', label: 'Groups', icon: UsersRound },
  { to: '/import', label: 'Import', icon: ArrowDownToLine },
  { to: '/account', label: 'Account', icon: UserCircle },
] as const
const adminNav = [
  { to: '/admin/users', label: 'Users', icon: Users },
  { to: '/admin/invites', label: 'Invites', icon: Mail },
  { to: '/admin/settings', label: 'Settings', icon: Settings },
] as const

function NavList({ isAdmin, onNavigate }: { isAdmin: boolean; onNavigate?: () => void }) {
  const link = (item: { to: string; label: string; icon: typeof Users }) => (
    <Link
      key={item.to}
      to={item.to}
      onClick={onNavigate}
      activeOptions={{ exact: item.to === '/' }}
      className="flex items-center gap-3 rounded-md px-3 py-2 text-sm text-muted-foreground outline-none transition-colors hover:bg-accent hover:text-accent-foreground focus-visible:ring-[3px] focus-visible:ring-ring/50"
      activeProps={{
        className: 'bg-accent font-medium text-accent-foreground',
        'aria-current': 'page',
      }}
    >
      <item.icon className="size-4" />
      {item.label}
    </Link>
  )
  return (
    <nav aria-label="Main" className="flex flex-col gap-1">
      {nav.map(link)}
      {isAdmin && (
        <>
          <p className="mt-6 mb-1 px-3 text-xs font-medium tracking-wide text-muted-foreground uppercase">
            Administration
          </p>
          {adminNav.map(link)}
        </>
      )}
    </nav>
  )
}

const themes: { value: Theme; label: string; icon: typeof Sun }[] = [
  { value: 'light', label: 'Light', icon: Sun },
  { value: 'dark', label: 'Dark', icon: Moon },
  { value: 'system', label: 'System', icon: Monitor },
]

export function ThemeToggle() {
  const { theme, setTheme } = useTheme()
  const Icon = themes.find((t) => t.value === theme)?.icon ?? Monitor
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="icon" aria-label="Change theme">
          <Icon />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        {themes.map((t) => (
          <DropdownMenuItem
            key={t.value}
            onSelect={() => setTheme(t.value)}
            className={cn(theme === t.value && 'font-medium')}
          >
            <t.icon /> {t.label}
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

export function Footer() {
  return (
    <p className="text-xs text-muted-foreground">
      Compatible with Zotero. Not affiliated with the Zotero project.
    </p>
  )
}

export function AppShell() {
  const { data: me } = useMe()
  const logout = useLogout()
  const navigate = useNavigate()
  const [open, setOpen] = useState(false)
  if (!me) return null

  return (
    <div className="min-h-dvh md:grid md:grid-cols-[15rem_1fr]">
      <aside className="sticky top-0 hidden h-dvh flex-col gap-8 border-r bg-card/50 p-4 md:flex">
        <Wordmark className="px-3 pt-2" />
        <div className="flex-1 overflow-y-auto">
          <NavList isAdmin={me.isAdmin} />
        </div>
      </aside>

      <Sheet open={open} onOpenChange={setOpen}>
        <SheetContent side="left" className="w-64 p-4 sm:max-w-64">
          <SheetTitle className="sr-only">Navigation</SheetTitle>
          <SheetDescription className="sr-only">Site navigation</SheetDescription>
          <Wordmark className="px-3 pt-2 pb-4" />
          <NavList isAdmin={me.isAdmin} onNavigate={() => setOpen(false)} />
        </SheetContent>
      </Sheet>

      <div className="flex min-w-0 flex-col">
        <header className="sticky top-0 z-30 flex h-14 items-center gap-2 border-b bg-background/80 px-4 backdrop-blur md:justify-end">
          <Button
            variant="ghost"
            size="icon"
            className="md:hidden"
            aria-label="Open navigation"
            onClick={() => setOpen(true)}
          >
            <Menu />
          </Button>
          <Wordmark className="md:hidden" />
          <div className="ml-auto flex items-center gap-1 md:ml-0">
            <ThemeToggle />
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="ghost" className="gap-2 px-2" aria-label="User menu">
                  <span className="flex size-7 items-center justify-center rounded-full bg-primary/10 text-xs font-semibold text-primary">
                    {initials(me.displayName || me.username)}
                  </span>
                  <span className="hidden max-w-32 truncate text-sm sm:inline">
                    {me.displayName || me.username}
                  </span>
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="w-56">
                <DropdownMenuLabel>
                  <div className="truncate">{me.displayName || me.username}</div>
                  <div className="truncate text-xs font-normal text-muted-foreground">
                    @{me.username}
                  </div>
                </DropdownMenuLabel>
                <DropdownMenuSeparator />
                <DropdownMenuItem onSelect={() => navigate({ to: '/account' })}>
                  <UserCircle /> Account
                </DropdownMenuItem>
                <DropdownMenuItem
                  onSelect={async () => {
                    await logout.mutateAsync().catch(() => undefined)
                    navigate({ to: '/login', search: {} })
                  }}
                >
                  <LogOut /> Log out
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
        </header>
        <main className="mx-auto w-full max-w-5xl flex-1 px-4 py-8 sm:px-8">
          <Outlet />
        </main>
        <footer className="border-t px-4 py-4 text-center sm:px-8">
          <Footer />
        </footer>
      </div>
    </div>
  )
}
