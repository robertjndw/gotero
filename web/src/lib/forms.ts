import { z } from 'zod'
import type { FieldValues, Path, UseFormSetError } from 'react-hook-form'
import { toast } from 'sonner'
import { ApiError } from './api'

// Mirrors auth.MinPasswordLength on the server. Keep them in sync, or users
// pass the form check and then get bounced by a 422.
export const MIN_PASSWORD_LENGTH = 10
export const passwordHint = `At least ${MIN_PASSWORD_LENGTH} characters`

// Mirrors usernameRe in internal/accounts. A stricter form check rejects
// names the server would take, a looser one turns into a 422.
export const usernameSchema = z
  .string()
  .min(2, 'At least 2 characters')
  .max(64, 'At most 64 characters')
  .regex(/^[A-Za-z0-9._-]+$/, 'Letters, numbers, dots, dashes and underscores only')

// Maps server field errors onto the form; falls back to a toast for everything else.
export function applyServerError<T extends FieldValues>(e: unknown, setError: UseFormSetError<T>) {
  if (e instanceof ApiError && Object.keys(e.fields).length > 0) {
    for (const [k, msg] of Object.entries(e.fields)) setError(k as Path<T>, { message: msg })
    return
  }
  toast.error(e instanceof ApiError ? e.message : 'Something went wrong.')
}
