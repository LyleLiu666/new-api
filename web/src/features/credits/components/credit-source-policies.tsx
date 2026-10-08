/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm, type Resolver } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { z } from 'zod'

import { StaticDataTable } from '@/components/data-table'
import { ErrorState } from '@/components/error-state'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { toIntlLocale } from '@/i18n/languages'
import { api } from '@/lib/api'
import { formatNumber } from '@/lib/format'
import { ROLE } from '@/lib/roles'
import { requireServerSuccess } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import { CREDIT_SOURCE_LABELS } from '../constants'

const sources = [
  'topup',
  'redemption',
  'checkin',
  'signup',
  'invitee',
  'inviter',
  'invite_transfer',
  'admin',
  'promotion',
  'compensation',
] as const
const schema = z.object({
  source_type: z.enum(sources),
  hours: z.coerce
    .number()
    .min(1 / 3600)
    .max(Number.MAX_SAFE_INTEGER / 3600),
  use_mask: z.enum(['1', '2', '3']),
  expected_revision: z.number().int().nonnegative(),
})
type Values = z.infer<typeof schema>
interface Policy {
  source_type: (typeof sources)[number]
  duration_seconds: number
  use_mask: number
  revision: number
}

export function CreditSourcePolicies() {
  const { t, i18n } = useTranslation()
  const user = useAuthStore((state) => state.auth.user)
  const client = useQueryClient()
  const root = user?.role === ROLE.SUPER_ADMIN
  const query = useQuery({
    queryKey: ['credits', 'policies', user?.id],
    enabled: root,
    queryFn: async () =>
      requireServerSuccess((await api.get('/api/credit/admin/policies')).data)
        .data as Policy[],
  })
  const form = useForm<Values>({
    resolver: zodResolver(schema) as Resolver<Values>,
    defaultValues: {
      source_type: 'topup',
      hours: 720,
      use_mask: '1',
      expected_revision: 0,
    },
  })
  const mutation = useMutation({
    mutationFn: async (values: Values) =>
      requireServerSuccess(
        (
          await api.put('/api/credit/admin/policies', {
            source_type: values.source_type,
            duration_seconds: Math.round(values.hours * 3600),
            use_mask: Number(values.use_mask),
            expected_revision: values.expected_revision,
          })
        ).data
      ),
    onSuccess: async () => {
      toast.success(t('Operation successful'))
      await client.invalidateQueries({ queryKey: ['credits', 'policies'] })
    },
    retry: false,
  })
  if (!root) return null
  return (
    <section className='space-y-3 rounded-md border p-4'>
      <h3 className='font-medium'>{t('Credit validity policies')}</h3>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Policies apply to newly issued packs. Existing pack expiry and purposes stay unchanged.'
        )}
      </p>
      {query.isError && <ErrorState onRetry={() => void query.refetch()} />}
      {query.data && (
        <StaticDataTable
          data={query.data}
          getRowKey={(row) => row.source_type}
          columns={[
            {
              id: 'source',
              header: t('Source'),
              cell: (row) => t(CREDIT_SOURCE_LABELS[row.source_type]),
            },
            {
              id: 'duration',
              header: t('Validity in hours'),
              cell: (row) =>
                formatNumber(
                  row.duration_seconds / 3600,
                  toIntlLocale(i18n.resolvedLanguage || i18n.language)
                ),
            },
            {
              id: 'revision',
              header: t('Version'),
              cell: (row) => row.revision,
            },
            {
              id: 'edit',
              header: t('Actions'),
              cell: (row) => (
                <Button
                  variant='outline'
                  onClick={() =>
                    form.reset({
                      source_type: row.source_type,
                      hours: row.duration_seconds / 3600,
                      use_mask: String(row.use_mask) as Values['use_mask'],
                      expected_revision: row.revision,
                    })
                  }
                >
                  {t('Edit')}
                </Button>
              ),
            },
          ]}
        />
      )}
      <p className='text-muted-foreground text-sm'>
        {t(
          'Select Edit for an existing policy, or choose an unconfigured source to create one.'
        )}
      </p>
      <Form {...form}>
        <form
          onSubmit={form.handleSubmit((values) => mutation.mutate(values))}
          className='grid gap-3 sm:grid-cols-4'
        >
          <FormField
            control={form.control}
            name='source_type'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Source')}</FormLabel>
                <FormControl>
                  <NativeSelect
                    {...field}
                    onChange={(event) => {
                      const source = event.target.value as Values['source_type']
                      const policy = query.data?.find(
                        (row) => row.source_type === source
                      )
                      form.reset({
                        source_type: source,
                        hours: policy ? policy.duration_seconds / 3600 : 720,
                        use_mask: String(
                          policy?.use_mask || 1
                        ) as Values['use_mask'],
                        expected_revision: policy?.revision || 0,
                      })
                    }}
                  >
                    {sources.map((source) => (
                      <NativeSelectOption key={source} value={source}>
                        {t(CREDIT_SOURCE_LABELS[source])}
                      </NativeSelectOption>
                    ))}
                  </NativeSelect>
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='hours'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Validity in hours')}</FormLabel>
                <FormControl>
                  <Input {...field} type='number' min={0} step='any' />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='use_mask'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Purpose')}</FormLabel>
                <FormControl>
                  <NativeSelect {...field}>
                    <NativeSelectOption value='1'>
                      {t('API consumption')}
                    </NativeSelectOption>
                    <NativeSelectOption value='2'>
                      {t('Subscription purchase')}
                    </NativeSelectOption>
                    <NativeSelectOption value='3'>
                      {t('API and subscriptions')}
                    </NativeSelectOption>
                  </NativeSelect>
                </FormControl>
              </FormItem>
            )}
          />
          <Button
            type='submit'
            className='self-end'
            disabled={
              mutation.isPending ||
              !query.data ||
              (query.data.find(
                (row) => row.source_type === form.watch('source_type')
              )?.revision || 0) !== form.watch('expected_revision')
            }
          >
            {t('Save policy')}
          </Button>
        </form>
      </Form>
    </section>
  )
}
