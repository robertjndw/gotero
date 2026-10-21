import { Controller, useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import type { GroupInput } from '@/lib/types'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Field } from '@/components/common'

export const groupSchema = z.object({
  name: z.string().trim().min(1, 'Give the group a name').max(100),
  description: z.string().max(1000),
  libraryReading: z.enum(['all', 'members']),
  libraryEditing: z.enum(['admins', 'members']),
  fileEditing: z.enum(['none', 'admins', 'members']),
})

export const groupDefaults: GroupInput = {
  name: '',
  description: '',
  libraryReading: 'members',
  libraryEditing: 'members',
  fileEditing: 'members',
}

export function useGroupForm(values: GroupInput) {
  return useForm<GroupInput>({ resolver: zodResolver(groupSchema), defaultValues: values, values })
}

export function GroupFields({ form }: { form: ReturnType<typeof useGroupForm> }) {
  const {
    register,
    control,
    formState: { errors },
  } = form
  return (
    <div className="grid gap-4">
      <Field label="Name" htmlFor="g-name" error={errors.name?.message}>
        <Input id="g-name" aria-invalid={!!errors.name} {...register('name')} />
      </Field>
      <Field label="Description" htmlFor="g-desc" error={errors.description?.message}>
        <Input id="g-desc" {...register('description')} />
      </Field>
      <Field label="Who can view the library" htmlFor="g-read">
        <Controller
          control={control}
          name="libraryReading"
          render={({ field }) => (
            <Select value={field.value} onValueChange={field.onChange}>
              <SelectTrigger id="g-read">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="members">Members only</SelectItem>
                <SelectItem value="all">Anyone</SelectItem>
              </SelectContent>
            </Select>
          )}
        />
      </Field>
      <Field label="Who can edit the library" htmlFor="g-edit">
        <Controller
          control={control}
          name="libraryEditing"
          render={({ field }) => (
            <Select value={field.value} onValueChange={field.onChange}>
              <SelectTrigger id="g-edit">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="members">All members</SelectItem>
                <SelectItem value="admins">Admins only</SelectItem>
              </SelectContent>
            </Select>
          )}
        />
      </Field>
      <Field label="Who can edit files" htmlFor="g-files">
        <Controller
          control={control}
          name="fileEditing"
          render={({ field }) => (
            <Select value={field.value} onValueChange={field.onChange}>
              <SelectTrigger id="g-files">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="members">All members</SelectItem>
                <SelectItem value="admins">Admins only</SelectItem>
                <SelectItem value="none">Nobody</SelectItem>
              </SelectContent>
            </Select>
          )}
        />
      </Field>
    </div>
  )
}
