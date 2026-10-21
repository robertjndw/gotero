import { Link } from '@tanstack/react-router'
import { ArrowDownToLine, KeyRound, UsersRound } from 'lucide-react'
import { useMe, useSetup, useStats } from '@/lib/queries'
import { formatBytes, streamUrl } from '@/lib/format'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { CodeLine, PageHeader, UsageMeter } from '@/components/common'

function Tile({ label, value }: { label: string; value: string }) {
  return (
    <Card className="gap-1 py-4">
      <CardContent>
        <p className="text-xs text-muted-foreground">{label}</p>
        <p className="mt-1 text-2xl font-semibold tabular-nums">{value}</p>
      </CardContent>
    </Card>
  )
}

function ConnectCard() {
  const { data: setup, isLoading } = useSetup()
  return (
    <Card>
      <CardHeader>
        <CardTitle>Connect your Zotero</CardTitle>
        <CardDescription>
          Point the Zotero desktop app at this server instead of zotero.org.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-5">
        <ol className="grid list-decimal gap-2 pl-5 text-sm marker:text-muted-foreground">
          <li>
            In Zotero, open <strong>Settings &gt; Advanced &gt; Config Editor</strong> and accept
            the warning.
          </li>
          <li>Search for each preference below and set it to the value shown.</li>
          <li>
            Restart Zotero, then log in under <strong>Settings &gt; Sync</strong> using your gotero
            account.
          </li>
        </ol>
        {isLoading || !setup ? (
          <Skeleton className="h-24 w-full" />
        ) : (
          <div className="grid gap-4">
            <div className="grid gap-1.5">
              <p className="font-mono text-xs text-muted-foreground">extensions.zotero.api.url</p>
              <CodeLine label="API URL" value={`${setup.publicUrl.replace(/\/$/, '')}/api/`} />
            </div>
            <div className="grid gap-1.5">
              <p className="font-mono text-xs text-muted-foreground">
                extensions.zotero.streaming.url
              </p>
              <CodeLine label="streaming URL" value={streamUrl(setup.publicUrl)} />
            </div>
          </div>
        )}
        <p className="text-xs text-muted-foreground">
          Needs a recent Zotero 7 release. Over https the server must present a certificate your
          computer trusts.
        </p>
      </CardContent>
    </Card>
  )
}

export default function DashboardPage() {
  const { data: me } = useMe()
  const stats = useStats(!!me?.isAdmin)
  if (!me) return null

  return (
    <>
      <PageHeader title={`Welcome, ${me.displayName || me.username}`} />
      <div className="grid gap-6">
        {me.isAdmin && (
          <section aria-label="Server statistics" className="grid grid-cols-2 gap-4 lg:grid-cols-4">
            {stats.data ? (
              <>
                <Tile label="Users" value={String(stats.data.users)} />
                <Tile label="Groups" value={String(stats.data.groups)} />
                <Tile label="Items" value={stats.data.items.toLocaleString()} />
                <Tile
                  label={`Storage (${stats.data.storage.backend})`}
                  value={formatBytes(stats.data.storage.usedBytes)}
                />
              </>
            ) : (
              Array.from({ length: 4 }, (_, i) => <Skeleton key={i} className="h-20" />)
            )}
          </section>
        )}

        <ConnectCard />

        <Card>
          <CardHeader>
            <CardTitle>Your storage</CardTitle>
            <CardDescription>
              {me.quotaBytes === null
                ? `${formatBytes(me.usedBytes)} used, no quota`
                : `${formatBytes(me.usedBytes)} of ${formatBytes(me.quotaBytes)} used`}
            </CardDescription>
          </CardHeader>
          <CardContent>
            <UsageMeter used={me.usedBytes} quota={me.quotaBytes} />
          </CardContent>
        </Card>

        <section aria-label="Quick links" className="grid gap-4 sm:grid-cols-3">
          {[
            {
              to: '/account',
              icon: KeyRound,
              title: 'API keys',
              text: 'Create keys for scripts and other tools.',
            },
            {
              to: '/groups',
              icon: UsersRound,
              title: 'Groups',
              text: 'Share libraries with other users.',
            },
            {
              to: '/import',
              icon: ArrowDownToLine,
              title: 'Import',
              text: 'Bring your library over from zotero.org.',
            },
          ].map((l) => (
            <Link
              key={l.to}
              to={l.to}
              className="group rounded-xl border bg-card p-4 transition-colors outline-none hover:bg-accent focus-visible:ring-[3px] focus-visible:ring-ring/50"
            >
              <l.icon className="mb-3 size-5 text-primary" />
              <p className="font-medium">{l.title}</p>
              <p className="mt-1 text-sm text-muted-foreground">{l.text}</p>
            </Link>
          ))}
        </section>
      </div>
    </>
  )
}
