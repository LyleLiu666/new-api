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
import { useForm, type Resolver } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { z } from 'zod'

import { StaticDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
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
import { Textarea } from '@/components/ui/textarea'
import { TitledCard } from '@/components/ui/titled-card'
import { useServerClock } from '@/hooks/use-server-clock'
import { toIntlLocale } from '@/i18n/languages'
import { api } from '@/lib/api'
import { formatContractCurrencyAmount } from '@/lib/currency'
import { requireServerSuccess } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import { getSelfSubscriptionFull } from '../api'

interface Order {
  id: number
  event_id: string
  plan_id: number
  version_id: number
  provider: string
  price_micros: number
  currency: string
  created_at: number
  expires_at: number
  paid_at: number
  payment_state: string
  needs_review: boolean
  last_review_reason: string
  rights_cancelled_at: number
  checkout_state?: string
}
interface OrderPage {
  items: Order[]
  total: number
}
interface ReviewDetails {
  order: Order
  latest_fact_id: number
  facts: {
    id: number
    kind: string
    amount_micros: number | null
    currency: string
    paid_at: number | null
  }[]
}
const schema = z.object({
  amount: z.coerce.number().finite().min(0).max(9999),
  paid_at: z
    .string()
    .min(1)
    .refine((value) => Number.isFinite(Date.parse(value))),
  reference_id: z.string().trim().min(1).max(128),
  evidence_reference: z.string().trim().min(1).max(1024),
  reason: z.string().trim().min(1).max(1024),
})
type ReviewValues = z.infer<typeof schema>

export function SubscriptionOrders(props: {
  userId?: number
  refreshKey?: number
}) {
  const { t, i18n } = useTranslation()
  const viewer = useAuthStore((state) => state.auth.user?.id ?? 0)
  const client = useQueryClient()
  const [page, setPage] = useState(1)
  const [selected, setSelected] = useState<number | null>(null)
  const [factPage, setFactPage] = useState(1)
  const admin = props.userId !== undefined
  const orders = useQuery({
    queryKey: [
      'subscriptions',
      admin ? 'payment-reviews' : 'orders',
      props.userId ?? viewer,
      page,
      props.refreshKey,
    ],
    queryFn: async () =>
      requireServerSuccess(
        (
          await api.get(
            admin
              ? '/api/subscription/admin/payment-reviews'
              : '/api/subscription/orders',
            { params: { user_id: props.userId, p: page, page_size: 10 } }
          )
        ).data
      ).data as OrderPage,
    refetchInterval: 30000,
  })
  const rights = useQuery({
    queryKey: ['subscriptions', 'self-rights', viewer, props.refreshKey],
    enabled: !admin,
    queryFn: async () => {
      const response = requireServerSuccess(await getSelfSubscriptionFull())
      if (!response.data) throw new Error(t('Invalid response'))
      return response.data
    },
  })
  const now = useServerClock(rights.data?.server_time, rights.dataUpdatedAt)
  const details = useQuery({
    queryKey: ['subscriptions', 'payment-review', selected, factPage, viewer],
    enabled: admin && selected !== null,
    refetchOnWindowFocus: false,
    queryFn: async () =>
      requireServerSuccess(
        (
          await api.get(`/api/subscription/admin/orders/${selected}`, {
            params: { p: factPage, page_size: 10 },
          })
        ).data
      ).data as ReviewDetails,
  })
  const form = useForm<ReviewValues>({
    resolver: zodResolver(schema) as Resolver<ReviewValues>,
    defaultValues: {
      amount: 0,
      paid_at: '',
      reference_id: '',
      evidence_reference: '',
      reason: '',
    },
  })
  const review = useMutation({
    mutationFn: async (values: ReviewValues) => {
      const current = details.data
      if (!current || details.isFetching || current.order.id !== selected) {
        throw new Error(t('Review the selected order before submitting'))
      }
      const body = {
        ...values,
        amount_micros: Math.round(values.amount * 1000000),
        paid_at: Math.floor(Date.parse(values.paid_at) / 1000),
        expected_fact_id: current.latest_fact_id,
        currency: current.order.currency,
      }
      const digest = await crypto.subtle.digest(
        'SHA-256',
        new TextEncoder().encode(JSON.stringify(body))
      )
      const key = `subscription-review:${selected}:${Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, '0')).join('')}`
      const eventId = sessionStorage.getItem(key) || crypto.randomUUID()
      sessionStorage.setItem(key, eventId)
      const result = requireServerSuccess(
        (
          await api.post(
            `/api/subscription/admin/orders/${selected}/reconcile`,
            { ...body, event_id: eventId }
          )
        ).data
      )
      sessionStorage.removeItem(key)
      return result
    },
    onSuccess: async () => {
      toast.success(t('Payment review recorded'))
      setSelected(null)
      form.reset()
      await client.invalidateQueries({ queryKey: ['subscriptions'] })
    },
    retry: false,
  })
  const date = (seconds: number) =>
    seconds > 0
      ? new Date(seconds * 1000).toLocaleString(
          toIntlLocale(i18n.resolvedLanguage || i18n.language)
        )
      : '—'
  return (
    <TitledCard
      title={
        admin
          ? t('Subscription payment reviews')
          : t('Subscription purchase orders')
      }
      description={t(
        'Payment and rights status are separate. This system does not automatically refund cash.'
      )}
    >
      {orders.isPending && <LoadingState />}
      {orders.isError && <ErrorState onRetry={() => void orders.refetch()} />}
      {orders.data && (
        <div className='space-y-3'>
          <StaticDataTable
            data={orders.data.items}
            getRowKey={(order) => order.id}
            columns={[
              {
                id: 'id',
                header: t('Order ID'),
                cell: (order) =>
                  admin ? (
                    <Button
                      variant='link'
                      onClick={() => {
                        setSelected(order.id)
                        setFactPage(1)
                        form.reset()
                      }}
                    >
                      #{order.id}
                    </Button>
                  ) : (
                    `#${order.id}`
                  ),
              },
              {
                id: 'version',
                header: t('Plan / version'),
                cell: (order) => `${order.plan_id} / ${order.version_id}`,
              },
              {
                id: 'price',
                header: t('Locked price'),
                cell: (order) =>
                  formatContractCurrencyAmount(
                    order.price_micros / 1000000,
                    order.currency,
                    toIntlLocale(i18n.language)
                  ),
              },
              {
                id: 'status',
                header: t('Status'),
                cell: (order) => {
                  if (order.needs_review) return t('Needs review')
                  if (order.payment_state === 'verified') return t('Paid')
                  if (
                    order.expires_at <= now &&
                    rights.data?.server_time !== undefined
                  ) {
                    return t('Expired')
                  }
                  return t('Pending')
                },
              },
              {
                id: 'paid',
                header: t('Paid at'),
                cell: (order) => date(order.paid_at),
              },
              {
                id: 'rights',
                header: t('Rights'),
                cell: (order) => {
                  if (order.rights_cancelled_at) return t('Cancelled')
                  if (order.payment_state === 'verified') return t('Granted')
                  return t('Awaiting payment')
                },
              },
              {
                id: 'expires',
                header: t('Checkout expires at'),
                cell: (order) => date(order.expires_at),
              },
              {
                id: 'next',
                header: t('Actions'),
                cell: (order) =>
                  !admin &&
                  !order.needs_review &&
                  (order.payment_state === 'verified' ||
                    (rights.data?.server_time !== undefined &&
                      order.expires_at <= now)) ? (
                    <Button
                      variant='outline'
                      onClick={() => {
                        const prefix = `subscription-intent:${viewer}:${order.plan_id}:${order.version_id}:`
                        const matching = Object.keys(sessionStorage).filter(
                          (key) =>
                            key.startsWith(prefix) &&
                            (sessionStorage.getItem(key) === order.event_id ||
                              (key.endsWith(':order') &&
                                sessionStorage.getItem(key) ===
                                  String(order.id)))
                        )
                        for (const key of matching) {
                          const intentKey = key.endsWith(':order')
                            ? key.slice(0, -6)
                            : key
                          sessionStorage.removeItem(intentKey)
                          sessionStorage.removeItem(`${intentKey}:order`)
                        }
                        toast.success(
                          t('Select the plan to start a new purchase')
                        )
                      }}
                    >
                      {t('Start another purchase')}
                    </Button>
                  ) : null,
              },
            ]}
          />
          <div className='flex gap-2'>
            <Button
              variant='outline'
              disabled={page <= 1 || orders.isFetching}
              onClick={() => setPage(page - 1)}
            >
              {t('Previous')}
            </Button>
            <Button
              variant='outline'
              disabled={page * 10 >= orders.data.total || orders.isFetching}
              onClick={() => setPage(page + 1)}
            >
              {t('Next')}
            </Button>
            <Button
              variant='outline'
              disabled={orders.isFetching}
              onClick={() => void orders.refetch()}
            >
              {t('Refresh')}
            </Button>
          </div>
        </div>
      )}
      {admin && (
        <Dialog
          open={selected !== null}
          onOpenChange={(open) => {
            if (!open && !review.isPending) {
              setSelected(null)
              form.reset()
            }
          }}
          title={t('Review subscription payment')}
        >
          {details.isPending && <LoadingState />}
          {details.isError && (
            <ErrorState onRetry={() => void details.refetch()} />
          )}
          {details.data && (
            <div className='space-y-4'>
              <p>
                {t('Locked price')}:{' '}
                {formatContractCurrencyAmount(
                  details.data.order.price_micros / 1000000,
                  details.data.order.currency,
                  toIntlLocale(i18n.language)
                )}
              </p>
              <p>
                {t('Last review reason')}:{' '}
                {details.data.order.last_review_reason || '—'}
              </p>
              <StaticDataTable
                data={details.data.facts}
                getRowKey={(fact) => fact.id}
                columns={[
                  {
                    id: 'id',
                    header: t('Evidence ID'),
                    cell: (fact) => fact.id,
                  },
                  {
                    id: 'amount',
                    header: t('Amount'),
                    cell: (fact) =>
                      fact.amount_micros === null
                        ? t('Unknown')
                        : formatContractCurrencyAmount(
                            fact.amount_micros / 1000000,
                            fact.currency,
                            toIntlLocale(i18n.language)
                          ),
                  },
                  {
                    id: 'paid',
                    header: t('Paid at'),
                    cell: (fact) =>
                      fact.paid_at === null ? t('Unknown') : date(fact.paid_at),
                  },
                ]}
              />
              <div className='flex gap-2'>
                <Button
                  variant='outline'
                  disabled={factPage <= 1 || details.isFetching}
                  onClick={() => setFactPage(factPage - 1)}
                >
                  {t('Previous')}
                </Button>
                <Button
                  variant='outline'
                  disabled={
                    details.data.facts.length < 10 || details.isFetching
                  }
                  onClick={() => setFactPage(factPage + 1)}
                >
                  {t('Next')}
                </Button>
                <Button
                  variant='outline'
                  disabled={details.isFetching || review.isPending}
                  onClick={() => void details.refetch()}
                >
                  {t('Refresh reviewed order')}
                </Button>
              </div>
              <Form {...form}>
                <form
                  onSubmit={form.handleSubmit((values) =>
                    review.mutate(values)
                  )}
                  className='space-y-3'
                >
                  <FormField
                    control={form.control}
                    name='amount'
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>
                          {t('Verified payment amount')} (
                          {details.data?.order.currency})
                        </FormLabel>
                        <FormControl>
                          <Input
                            {...field}
                            type='number'
                            min={0}
                            step='0.000001'
                          />
                        </FormControl>
                        <FormMessage />
                      </FormItem>
                    )}
                  />
                  <FormField
                    control={form.control}
                    name='paid_at'
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>{t('Verified payment time')}</FormLabel>
                        <FormControl>
                          <Input {...field} type='datetime-local' />
                        </FormControl>
                        <FormMessage />
                      </FormItem>
                    )}
                  />
                  <FormField
                    control={form.control}
                    name='reference_id'
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>{t('Payment receipt reference')}</FormLabel>
                        <FormControl>
                          <Input {...field} />
                        </FormControl>
                        <FormMessage />
                      </FormItem>
                    )}
                  />
                  <FormField
                    control={form.control}
                    name='evidence_reference'
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>
                          {t('External evidence reference')}
                        </FormLabel>
                        <FormControl>
                          <Input {...field} />
                        </FormControl>
                        <FormMessage />
                      </FormItem>
                    )}
                  />
                  <FormField
                    control={form.control}
                    name='reason'
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>{t('Reason')}</FormLabel>
                        <FormControl>
                          <Textarea {...field} />
                        </FormControl>
                        <FormMessage />
                      </FormItem>
                    )}
                  />
                  <Button
                    type='submit'
                    disabled={review.isPending || details.isFetching}
                  >
                    {t('Confirm verified payment')}
                  </Button>
                </form>
              </Form>
            </div>
          )}
        </Dialog>
      )}
    </TitledCard>
  )
}
