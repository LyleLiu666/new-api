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
import { useState } from 'react'
import { useFieldArray, useForm, type Resolver } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { StaticDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
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
import { Textarea } from '@/components/ui/textarea'
import { api } from '@/lib/api'
import { getCurrencyDisplay, getCurrencyLabel } from '@/lib/currency'
import { formatQuota, parseQuotaFromDollars } from '@/lib/format'
import { requireServerSuccess } from '@/lib/server-error-message'

import { creditQueryKeys, getCreditAccount, getCreditBill } from '../api'
import {
  CREDIT_ADMIN_DEFAULTS,
  creditAdminFormSchema,
  type CreditAdminForm,
} from '../lib/admin-form'
import { CreditLedger } from './credit-ledger'
import { CreditSourcePolicies } from './credit-source-policies'

interface WorkItem {
  request_id: number
  logical_request_id: string
  state: string
  recovery_attempts: number
  last_error: string
}
interface LogItem {
  id: number
  request_id: number
  state: string
  attempts: number
  last_error: string
}
interface WorkData {
  requests: WorkItem[]
  total: number
  logs: LogItem[]
  logs_total: number
  adjustment_logs: LogItem[]
  adjustment_logs_total: number
}
interface ReviewItem {
  case_id: number
  pack_id: number
  reason: string
  cash_state: string
}

export function CreditAdminDialog(props: {
  open: boolean
  onOpenChange: (open: boolean) => void
  user: { id: number; username?: string }
}) {
  const { t } = useTranslation()
  const client = useQueryClient()
  const [page, setPage] = useState(1)
  const form = useForm<CreditAdminForm>({
    resolver: zodResolver(
      creditAdminFormSchema(t)
    ) as Resolver<CreditAdminForm>,
    defaultValues: CREDIT_ADMIN_DEFAULTS,
  })
  const facts = useFieldArray({ control: form.control, name: 'facts' })
  const action = form.watch('action')
  const targetLabel = {
    freeze: 'Credit pack ID',
    cash: 'Review case ID',
    grant: 'Bill or work ID',
    adjust: 'Bill or work ID',
    review: 'Bill or work ID',
    retry: 'Bill or work ID',
  }[action]
  const targetId = Number(form.watch('target_id'))
  const selectedBill = useQuery({
    queryKey: ['credits', props.user.id, 'reviewed-bill', targetId],
    refetchOnWindowFocus: false,
    enabled:
      props.open && ['adjust', 'review'].includes(action) && targetId > 0,
    queryFn: () => getCreditBill(targetId, 1, props.user.id),
  })
  const work = useQuery({
    queryKey: ['credits', props.user.id, 'work', page],
    enabled: props.open,
    queryFn: async () =>
      requireServerSuccess(
        (
          await api.get('/api/credit/admin/work', {
            params: { user_id: props.user.id, p: page, page_size: 10 },
          })
        ).data
      ).data as WorkData,
  })
  const reviews = useQuery({
    queryKey: ['credits', props.user.id, 'reviews', page],
    enabled: props.open,
    queryFn: async () =>
      requireServerSuccess(
        (
          await api.get('/api/credit/admin/reviews', {
            params: { user_id: props.user.id, p: page, page_size: 10 },
          })
        ).data
      ).data as { items: ReviewItem[]; total: number },
  })
  const reconcile = useMutation({
    mutationFn: async () =>
      requireServerSuccess(
        (
          await api.get('/api/credit/admin/reconcile', {
            params: { user_id: props.user.id },
          })
        ).data
      ).data as {
        consistent: boolean
        differences: {
          kind: string
          id: number
          expected: number
          actual: number
        }[]
      },
    retry: false,
  })
  const operation = useMutation({
    mutationFn: async (values: CreditAdminForm) => {
      const quota = parseQuotaFromDollars(values.amount)
      let path: string
      let body: Record<string, unknown> = {
        user_id: props.user.id,
        reason: values.reason,
      }
      if (values.action === 'grant') {
        path = '/api/credit/admin/grants'
        body = {
          ...body,
          amount: quota,
          starts_at: values.starts_at
            ? Math.floor(Date.parse(values.starts_at) / 1000)
            : 0,
          expires_at: Math.floor(Date.parse(values.expires_at) / 1000),
          use_mask: Number(values.use_mask),
        }
      } else if (values.action === 'freeze') {
        path = '/api/credit/admin/reviews'
        body.pack_id = values.target_id
      } else if (values.action === 'cash') {
        path = '/api/credit/admin/reviews/cash-outcome'
        body = {
          user_id: props.user.id,
          case_id: values.target_id,
          state: values.cash_state,
          reference: values.reference,
          evidence: values.reason,
        }
      } else if (values.action === 'retry') {
        path = '/api/credit/admin/work/retry'
        body[values.recovery_kind] = values.target_id
      } else {
        const bill = selectedBill.data
        if (!bill || bill.id !== values.target_id || selectedBill.isFetching) {
          throw new Error(t('Review the selected bill before submitting'))
        }
        const quantities = values.facts.map((fact) => ({
          ...fact,
          source: 'upstream',
        }))
        path =
          values.action === 'adjust'
            ? '/api/credit/admin/bills/adjustments'
            : '/api/credit/admin/bills/review'
        body = {
          ...body,
          request_id: values.target_id,
          facts: quantities,
          evidence_version: 'admin-verified-usage-v1',
        }
        if (values.action === 'adjust') {
          body.expected_revision = bill.current.revision
          body.reference_quota = quota
        } else {
          body.expected_evidence_id = bill.usage_evidence_id ?? 0
          body.external_reference = values.reference
          body.consume = {
            reference_quota: quota,
            zero_charge_established: quota === 0 && values.zero_confirmed,
            other: '{}',
          }
        }
      }
      // Opaque intent IDs survive an uncertain response. Private explanations
      // and quantity evidence are never stored in browser storage.
      const digest = await crypto.subtle.digest(
        'SHA-256',
        new TextEncoder().encode(JSON.stringify({ path, body }))
      )
      const digestKey = Array.from(new Uint8Array(digest), (byte) =>
        byte.toString(16).padStart(2, '0')
      ).join('')
      const storageKey = `credit-intent:${props.user.id}:${digestKey}`
      const saved = sessionStorage.getItem(storageKey)
      const intent = saved
        ? (JSON.parse(saved) as { event_id: string; starts_at: number })
        : { event_id: crypto.randomUUID(), starts_at: 0 }
      if (values.action === 'grant' && !body.starts_at) {
        if (!intent.starts_at) {
          intent.starts_at = (
            await getCreditAccount(1, props.user.id)
          ).server_time
        }
        body.starts_at = intent.starts_at
      }
      body.event_id = intent.event_id
      sessionStorage.setItem(storageKey, JSON.stringify(intent))
      const result = requireServerSuccess((await api.post(path, body)).data)
      sessionStorage.removeItem(storageKey)
      return result
    },
    onSuccess: async () => {
      toast.success(t('Operation successful'))
      form.reset(CREDIT_ADMIN_DEFAULTS)
      await client.invalidateQueries({ queryKey: creditQueryKeys.all })
    },
    retry: false,
  })
  const labels: Record<CreditAdminForm['action'], string> = {
    grant: 'Grant expiring credits',
    freeze: 'Freeze a credit pack for review',
    cash: 'Record external cash outcome',
    adjust: 'Correct a settled bill',
    review: 'Confirm unknown usage',
    retry: 'Resume accounting work',
  }
  return (
    <Dialog
      open={props.open}
      onOpenChange={(open) => {
        if (!operation.isPending) props.onOpenChange(open)
      }}
      title={t('Credit management')}
      description={props.user.username}
      contentClassName='max-w-6xl'
      contentHeight='min(80vh, 900px)'
    >
      <div className='space-y-5'>
        <CreditLedger userId={props.user.id} />
        <CreditSourcePolicies />
        <Form {...form}>
          <form
            onSubmit={form.handleSubmit((values) => operation.mutate(values))}
            className='space-y-3 rounded-md border p-4'
          >
            <FormField
              control={form.control}
              name='action'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Operation')}</FormLabel>
                  <FormControl>
                    <NativeSelect {...field} disabled={operation.isPending}>
                      {Object.entries(labels).map(([value, label]) => (
                        <NativeSelectOption key={value} value={value}>
                          {t(label)}
                        </NativeSelectOption>
                      ))}
                    </NativeSelect>
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <p className='text-muted-foreground text-sm'>
              {t(
                'Cash refunds are handled outside this system. Confirmation records evidence and does not send money.'
              )}
            </p>
            {action !== 'grant' && (
              <FormField
                control={form.control}
                name='target_id'
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>{t(targetLabel)}</FormLabel>
                    <FormControl>
                      <Input {...field} type='number' min={1} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
            )}
            {['grant', 'adjust', 'review'].includes(action) && (
              <FormField
                control={form.control}
                name='amount'
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>
                      {action === 'grant'
                        ? t('Credit amount ({{unit}})', {
                            unit: getCurrencyLabel(),
                          })
                        : t('Verified reference cost ({{unit}})', {
                            unit: getCurrencyLabel(),
                          })}
                    </FormLabel>
                    <FormControl>
                      <Input
                        {...field}
                        type='number'
                        min={0}
                        step={
                          getCurrencyDisplay().meta.kind === 'tokens'
                            ? 1
                            : 0.000001
                        }
                      />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
            )}
            {action === 'grant' && (
              <>
                <FormField
                  control={form.control}
                  name='starts_at'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>
                        {t('Valid from (leave blank for server time)')}
                      </FormLabel>
                      <FormControl>
                        <Input {...field} type='datetime-local' />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
                <FormField
                  control={form.control}
                  name='expires_at'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Expires at')}</FormLabel>
                      <FormControl>
                        <Input {...field} type='datetime-local' required />
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
              </>
            )}
            {action === 'cash' && (
              <FormField
                control={form.control}
                name='cash_state'
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>{t('Cash outcome')}</FormLabel>
                    <FormControl>
                      <NativeSelect {...field}>
                        <NativeSelectOption value='confirmed'>
                          {t('Confirmed')}
                        </NativeSelectOption>
                        <NativeSelectOption value='rejected'>
                          {t('Declined')}
                        </NativeSelectOption>
                      </NativeSelect>
                    </FormControl>
                  </FormItem>
                )}
              />
            )}
            {action === 'retry' && (
              <FormField
                control={form.control}
                name='recovery_kind'
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>{t('Work type')}</FormLabel>
                    <FormControl>
                      <NativeSelect {...field}>
                        <NativeSelectOption value='request_id'>
                          {t('Bill settlement')}
                        </NativeSelectOption>
                        <NativeSelectOption value='outbox_id'>
                          {t('Original bill log')}
                        </NativeSelectOption>
                        <NativeSelectOption value='adjustment_id'>
                          {t('Correction log')}
                        </NativeSelectOption>
                      </NativeSelect>
                    </FormControl>
                  </FormItem>
                )}
              />
            )}
            {action === 'review' && (
              <FormField
                control={form.control}
                name='zero_confirmed'
                render={({ field }) => (
                  <FormItem className='flex items-center gap-2'>
                    <FormControl>
                      <Checkbox
                        checked={field.value}
                        onCheckedChange={field.onChange}
                      />
                    </FormControl>
                    <FormLabel>
                      {t('I verified that this request has zero charge')}
                    </FormLabel>
                    <FormMessage />
                  </FormItem>
                )}
              />
            )}
            {['review', 'cash'].includes(action) && (
              <FormField
                control={form.control}
                name='reference'
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>{t('External reference')}</FormLabel>
                    <FormControl>
                      <Input {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
            )}
            {['adjust', 'review'].includes(action) && (
              <>
                {selectedBill.isError && (
                  <ErrorState onRetry={() => void selectedBill.refetch()} />
                )}
                {selectedBill.data && (
                  <div className='rounded-md border p-3'>
                    <p>
                      {selectedBill.data.request_id} · {t('Version')}{' '}
                      {selectedBill.data.current.revision}
                    </p>
                    <p>
                      {t('Current reference / deduction')}:{' '}
                      {formatQuota(selectedBill.data.current.reference_quota)} /{' '}
                      {formatQuota(selectedBill.data.current.charged)}
                    </p>
                    <Button
                      type='button'
                      variant='outline'
                      onClick={() => void selectedBill.refetch()}
                    >
                      {t('Refresh reviewed bill')}
                    </Button>
                  </div>
                )}
                <p>
                  {t(
                    'Enter verified upstream quantities. Confirming zero requires verified zero quantities.'
                  )}
                </p>
                {facts.fields.map((row, index) => (
                  <div
                    key={row.id}
                    className='grid gap-2 rounded-md border p-3 sm:grid-cols-4'
                  >
                    <FormField
                      control={form.control}
                      name={`facts.${index}.field`}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('Quantity name')}</FormLabel>
                          <FormControl>
                            <Input {...field} />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`facts.${index}.quantity`}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('Verified quantity')}</FormLabel>
                          <FormControl>
                            <Input
                              {...field}
                              type='number'
                              min={0}
                              step='any'
                              onChange={(event) =>
                                field.onChange(
                                  event.target.value === ''
                                    ? Number.NaN
                                    : Number(event.target.value)
                                )
                              }
                            />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`facts.${index}.unit`}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('Unit')}</FormLabel>
                          <FormControl>
                            <NativeSelect {...field}>
                              {[
                                'token',
                                'count',
                                'second',
                                'credit',
                                'request',
                              ].map((unit) => (
                                <NativeSelectOption key={unit} value={unit}>
                                  {unit}
                                </NativeSelectOption>
                              ))}
                            </NativeSelect>
                          </FormControl>
                        </FormItem>
                      )}
                    />
                    <Button
                      type='button'
                      variant='outline'
                      onClick={() => facts.remove(index)}
                    >
                      {t('Remove')}
                    </Button>
                  </div>
                ))}
                <FormMessage>
                  {form.formState.errors.facts?.message}
                </FormMessage>
                <Button
                  type='button'
                  variant='outline'
                  disabled={facts.fields.length >= 64}
                  onClick={() =>
                    facts.append({ field: '', quantity: 0, unit: 'token' })
                  }
                >
                  {t('Add verified quantity')}
                </Button>
              </>
            )}
            <FormField
              control={form.control}
              name='reason'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Reason / evidence')}</FormLabel>
                  <FormControl>
                    <Textarea {...field} />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <Button
              type='submit'
              disabled={
                operation.isPending ||
                (['adjust', 'review'].includes(action) &&
                  (!selectedBill.data || selectedBill.isFetching))
              }
            >
              {operation.isPending ? t('Processing...') : t('Submit operation')}
            </Button>
          </form>
        </Form>
        <section className='space-y-3'>
          <h3 className='font-medium'>{t('Pending accounting work')}</h3>
          {work.isError && <ErrorState onRetry={() => void work.refetch()} />}
          {work.data && (
            <>
              <StaticDataTable
                data={work.data.requests}
                getRowKey={(row) => row.request_id}
                columns={[
                  {
                    id: 'id',
                    header: t('Bill ID'),
                    cell: (row) => row.request_id,
                  },
                  {
                    id: 'request',
                    header: t('Request'),
                    cell: (row) => row.logical_request_id,
                  },
                  {
                    id: 'state',
                    header: t('Status'),
                    cell: (row) => row.state,
                  },
                  {
                    id: 'error',
                    header: t('Last error'),
                    cell: (row) => row.last_error || '—',
                  },
                ]}
              />
              {[
                { label: 'Original bill log', rows: work.data.logs },
                { label: 'Correction log', rows: work.data.adjustment_logs },
              ].map((group) => (
                <div key={group.label}>
                  <h4>{t(group.label)}</h4>
                  <StaticDataTable
                    data={group.rows}
                    getRowKey={(row) => row.id}
                    columns={[
                      { id: 'id', header: t('Work ID'), cell: (row) => row.id },
                      {
                        id: 'bill',
                        header: t('Bill ID'),
                        cell: (row) => row.request_id,
                      },
                      {
                        id: 'error',
                        header: t('Last error'),
                        cell: (row) => row.last_error || '—',
                      },
                    ]}
                  />
                </div>
              ))}
            </>
          )}
          <h3 className='font-medium'>{t('Credit pack reviews')}</h3>
          {reviews.isError && (
            <ErrorState onRetry={() => void reviews.refetch()} />
          )}
          {reviews.data && (
            <StaticDataTable
              data={reviews.data.items}
              getRowKey={(row) => row.case_id}
              columns={[
                {
                  id: 'case',
                  header: t('Review case ID'),
                  cell: (row) => row.case_id,
                },
                {
                  id: 'pack',
                  header: t('Credit pack ID'),
                  cell: (row) => row.pack_id,
                },
                {
                  id: 'state',
                  header: t('Cash outcome'),
                  cell: (row) => row.cash_state,
                },
                {
                  id: 'reason',
                  header: t('Reason'),
                  cell: (row) => row.reason,
                },
              ]}
            />
          )}
          <div className='flex gap-2'>
            <Button
              variant='outline'
              disabled={page <= 1 || work.isFetching || reviews.isFetching}
              onClick={() => setPage(page - 1)}
            >
              {t('Previous')}
            </Button>
            <Button
              variant='outline'
              disabled={
                page * 10 >=
                  Math.max(
                    work.data?.total || 0,
                    work.data?.logs_total || 0,
                    work.data?.adjustment_logs_total || 0,
                    reviews.data?.total || 0
                  ) ||
                work.isFetching ||
                reviews.isFetching
              }
              onClick={() => setPage(page + 1)}
            >
              {t('Next')}
            </Button>
          </div>
          <Button
            variant='outline'
            disabled={reconcile.isPending}
            onClick={() => reconcile.mutate()}
          >
            {t('Reconcile account')}
          </Button>
          {reconcile.data && (
            <p role='status'>
              {reconcile.data.consistent
                ? t('Account quantities are consistent')
                : t('Account differences require review')}
            </p>
          )}
        </section>
      </div>
    </Dialog>
  )
}
