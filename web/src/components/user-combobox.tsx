import { useEffect, useState } from 'react'
import { ChevronsUpDown } from 'lucide-react'
import { useUserLookup } from '@/lib/queries'
import type { UserLookup } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'

function useDebounced<T>(value: T, ms = 250) {
  const [v, setV] = useState(value)
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms)
    return () => clearTimeout(t)
  }, [value, ms])
  return v
}

export function UserCombobox({
  value,
  onChange,
  id,
}: {
  value: UserLookup | null
  onChange: (u: UserLookup) => void
  id?: string
}) {
  const [open, setOpen] = useState(false)
  const [q, setQ] = useState('')
  const debounced = useDebounced(q)
  const { data, isFetching } = useUserLookup(debounced)
  const tooShort = q.trim().length < 2
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button
          id={id}
          type="button"
          variant="outline"
          role="combobox"
          aria-expanded={open}
          className="w-full justify-between font-normal"
        >
          <span className={value ? '' : 'text-muted-foreground'}>
            {value
              ? `${value.displayName || value.username} (@${value.username})`
              : 'Search for a user...'}
          </span>
          <ChevronsUpDown className="opacity-50" />
        </Button>
      </PopoverTrigger>
      <PopoverContent className="w-[var(--radix-popover-trigger-width)] p-2">
        <Input
          aria-label="Search users"
          autoFocus
          placeholder="Type at least 2 characters"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
        <ul role="listbox" aria-label="Matching users" className="mt-2 max-h-56 overflow-y-auto">
          {tooShort ? (
            <li className="px-2 py-3 text-center text-sm text-muted-foreground">
              Start typing to search.
            </li>
          ) : isFetching && !data ? (
            <li className="px-2 py-3 text-center text-sm text-muted-foreground">Searching...</li>
          ) : data?.length === 0 ? (
            <li className="px-2 py-3 text-center text-sm text-muted-foreground">No users found.</li>
          ) : (
            data?.map((u) => (
              <li key={u.id} role="option" aria-selected={value?.id === u.id}>
                <button
                  type="button"
                  className="flex w-full flex-col items-start rounded-sm px-2 py-1.5 text-left text-sm outline-none hover:bg-accent focus-visible:bg-accent"
                  onClick={() => {
                    onChange(u)
                    setOpen(false)
                  }}
                >
                  <span>{u.displayName || u.username}</span>
                  <span className="text-xs text-muted-foreground">@{u.username}</span>
                </button>
              </li>
            ))
          )}
        </ul>
      </PopoverContent>
    </Popover>
  )
}
