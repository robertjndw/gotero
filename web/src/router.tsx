import {
  createRootRouteWithContext,
  createRoute,
  createRouter,
  createMemoryHistory,
  Outlet,
  redirect,
  type RouterHistory,
} from '@tanstack/react-router'
import type { QueryClient } from '@tanstack/react-query'
import { z } from 'zod'
import { ApiError, setUnauthorizedHandler } from '@/lib/api'
import { meQuery } from '@/lib/queries'
import { AppShell } from '@/components/layout/app-shell'
import LoginPage from '@/pages/login'
import RegisterPage from '@/pages/register'
import DashboardPage from '@/pages/dashboard'
import AccountPage from '@/pages/account'
import GroupsPage from '@/pages/groups'
import GroupDetailPage from '@/pages/group-detail'
import UsersPage from '@/pages/admin-users'
import InvitesPage from '@/pages/admin-invites'
import SettingsPage from '@/pages/admin-settings'
import ImportPage from '@/pages/import'
import { NotFound } from '@/pages/not-found'

type Ctx = { queryClient: QueryClient }

const rootRoute = createRootRouteWithContext<Ctx>()({
  component: Outlet,
  notFoundComponent: NotFound,
})

const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/login',
  validateSearch: z.object({
    session: z.string().optional(),
    redirect: z.string().optional(),
    error: z.string().optional(),
  }),
  component: LoginPage,
})
const registerRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/register',
  validateSearch: z.object({ invite: z.string().optional(), error: z.string().optional() }),
  component: RegisterPage,
})

const appRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: 'app',
  component: AppShell,
  beforeLoad: async ({ context, location }) => {
    try {
      await context.queryClient.ensureQueryData(meQuery)
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        throw redirect({ to: '/login', search: { redirect: location.href } })
      }
      throw e
    }
  },
})

const adminOnly = {
  beforeLoad: async ({ context }: { context: Ctx }) => {
    const me = await context.queryClient.ensureQueryData(meQuery)
    if (!me.isAdmin) throw redirect({ to: '/' })
  },
}

const indexRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/',
  component: DashboardPage,
})
const accountRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/account',
  validateSearch: z.object({ error: z.string().optional() }),
  component: AccountPage,
})
const groupsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/groups',
  component: GroupsPage,
})
const groupDetailRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/groups/$id',
  component: GroupDetailPage,
})
const importRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/import',
  component: ImportPage,
})
const usersRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/admin/users',
  component: UsersPage,
  ...adminOnly,
})
const invitesRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/admin/invites',
  component: InvitesPage,
  ...adminOnly,
})
const settingsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/admin/settings',
  component: SettingsPage,
  ...adminOnly,
})

const routeTree = rootRoute.addChildren([
  loginRoute,
  registerRoute,
  appRoute.addChildren([
    indexRoute,
    accountRoute,
    groupsRoute,
    groupDetailRoute,
    importRoute,
    usersRoute,
    invitesRoute,
    settingsRoute,
  ]),
])

export function createAppRouter(queryClient: QueryClient, history?: RouterHistory) {
  const router = createRouter({
    routeTree,
    context: { queryClient },
    history,
    defaultPreload: 'intent',
  })
  setUnauthorizedHandler(() => {
    const path = router.state.location.pathname
    if (path === '/login' || path === '/register') return
    queryClient.clear()
    void router.navigate({ to: '/login', search: { redirect: router.state.location.href } })
  })
  return router
}

export { createMemoryHistory }

declare module '@tanstack/react-router' {
  interface Register {
    router: ReturnType<typeof createAppRouter>
  }
}
