import { useState } from 'react'
import { GB, MB } from '@/lib/format'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

type Unit = 'MB' | 'GB'

function split(bytes: number | null): { amount: string; unit: Unit } {
  if (bytes === null) return { amount: '', unit: 'GB' }
  if (bytes >= GB && bytes % GB === 0) return { amount: String(bytes / GB), unit: 'GB' }
  return { amount: String(Math.round((bytes / MB) * 100) / 100), unit: 'MB' }
}

// Owns its text state so half-typed values ("1.") are not clobbered by the parent's byte value.
export function QuotaInput({
  id,
  value,
  onChange,
}: {
  id: string
  value: number | null
  onChange: (bytes: number | null) => void
}) {
  const [state, setState] = useState(() => split(value))
  const unlimited = value === null

  const emit = (amount: string, unit: Unit) => {
    const n = Number(amount)
    if (amount === '' || !Number.isFinite(n) || n < 0) return
    onChange(Math.round(n * (unit === 'GB' ? GB : MB)))
  }

  return (
    <div className="grid gap-2">
      <div className="flex gap-2">
        <Input
          id={id}
          type="number"
          min={0}
          step="any"
          inputMode="decimal"
          aria-label="Quota amount"
          disabled={unlimited}
          value={unlimited ? '' : state.amount}
          placeholder={unlimited ? 'Unlimited' : '0'}
          onChange={(e) => {
            setState((s) => ({ ...s, amount: e.target.value }))
            emit(e.target.value, state.unit)
          }}
        />
        <Select
          value={state.unit}
          disabled={unlimited}
          onValueChange={(u) => {
            setState((s) => ({ ...s, unit: u as Unit }))
            emit(state.amount, u as Unit)
          }}
        >
          <SelectTrigger aria-label="Quota unit" className="w-24">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="MB">MB</SelectItem>
            <SelectItem value="GB">GB</SelectItem>
          </SelectContent>
        </Select>
      </div>
      <div className="flex items-center gap-2">
        <Checkbox
          id={`${id}-unlimited`}
          checked={unlimited}
          onCheckedChange={(c) => {
            if (c === true) onChange(null)
            else {
              const next = state.amount === '' ? '5' : state.amount
              setState((s) => ({ ...s, amount: next }))
              emit(next, state.unit)
            }
          }}
        />
        <Label htmlFor={`${id}-unlimited`} className="font-normal">
          Unlimited
        </Label>
      </div>
    </div>
  )
}
