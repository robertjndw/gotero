import type { FieldValues, Path, UseFormSetError } from 'react-hook-form'
import { toast } from 'sonner'
import { ApiError } from './api'

// Maps server field errors onto the form; falls back to a toast for everything else.
export function applyServerError<T extends FieldValues>(e: unknown, setError: UseFormSetError<T>) {
  if (e instanceof ApiError && Object.keys(e.fields).length > 0) {
    for (const [k, msg] of Object.entries(e.fields)) setError(k as Path<T>, { message: msg })
    return
  }
  toast.error(e instanceof ApiError ? e.message : 'Something went wrong.')
}
