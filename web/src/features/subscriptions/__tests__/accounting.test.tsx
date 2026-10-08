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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  flexRender,
  getCoreRowModel,
  useReactTable,
} from '@tanstack/react-table'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { createInstance, t } from 'i18next'
import { I18nextProvider, initReactI18next } from 'react-i18next'
import { expect, it, vi } from 'vitest'

import { Button } from '@/components/ui/button'
import { formatAccountingQuota } from '@/features/credits/lib/format'
import { toIntlLocale } from '@/i18n/languages'
import { api } from '@/lib/api'
import { formatContractCurrencyAmount } from '@/lib/currency'
import { useAuthStore } from '@/stores/auth-store'

import { getSelfSubscriptionFull } from '../api'
import { PlanVersionsDialog } from '../components/dialogs/plan-versions-dialog'
import { SubscriptionPurchaseDialog } from '../components/dialogs/subscription-purchase-dialog'
import { UserSubscriptionsDialog } from '../components/dialogs/user-subscriptions-dialog'
import { SubscriptionOrders } from '../components/subscription-orders'
import { useSubscriptionsColumns } from '../components/subscriptions-columns'
import {
  SubscriptionsProvider,
  useSubscriptions,
} from '../components/subscriptions-provider'
import {
  getPlanFormSchema,
  formValuesToPlanPayload,
  planToFormValues,
} from '../lib/plan-form'
import type { PlanRecord } from '../types'

const plan = {
  plan: {
    id: 7,
    title: 'Monthly',
    price_amount: 1,
    currency: 'USD',
    duration_unit: 'month',
    duration_value: 1,
    quota_reset_period: 'never',
    enabled: true,
    sort_order: 0,
    allow_balance_pay: true,
    allow_wallet_overflow: true,
    max_purchase_per_user: 0,
    total_amount: 500000,
    window_rules: [
      { id: 'five-hours', duration_seconds: 18000, limit: 500000 },
    ],
    entitlement_tags: { resource: 'premium' },
  },
  version_id: 9,
} as PlanRecord

it('normalizes current rights while preserving server windows and purchased tags', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        server_time: 100,
        billing_preference: 'subscription_first',
        subscriptions: [
          {
            id: 8,
            plan_id: 7,
            plan_version_id: 9,
            entitlement_tags: { resource: 'premium' },
          },
        ],
        all_subscriptions: [{ subscription: { id: 8 } }],
        windows: [{ rule_id: 'five-hours', state: 'unstarted' }],
      },
    },
  })
  const result = await getSelfSubscriptionFull()
  expect(result.data?.subscriptions[0]?.subscription).toMatchObject({
    id: 8,
    plan_version_id: 9,
  })
  expect(result.data).toMatchObject({
    server_time: 100,
    windows: [{ state: 'unstarted' }],
  })
})

it('keeps short-window quota and tag snapshots when editing a plan', () => {
  const values = planToFormValues(plan.plan)
  expect(values).toMatchObject({
    window_rules: [{ id: 'five-hours', duration_seconds: 18000, limit: 1 }],
    entitlement_tags: [{ key: 'resource', value: 'premium' }],
  })
  expect(formValuesToPlanPayload(values).plan).toMatchObject({
    window_rules: plan.plan.window_rules,
    entitlement_tags: plan.plan.entitlement_tags,
  })
})

it('locks purchase to the displayed version and reuses the event on an uncertain response', async () => {
  const i18n = createInstance()
  await i18n.use(initReactI18next).init({
    lng: 'en',
    resources: { en: { translation: {} } },
    interpolation: { escapeValue: false },
  })
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: { accounting_version: 1, subscription_available: 5000000 },
    },
  })
  const post = vi
    .spyOn(api, 'post')
    .mockRejectedValueOnce(new Error('connection lost'))
    .mockResolvedValueOnce({ data: { success: true } })
  const changed = vi.fn()
  render(
    <QueryClientProvider
      client={
        new QueryClient({ defaultOptions: { queries: { retry: false } } })
      }
    >
      <I18nextProvider i18n={i18n}>
        <SubscriptionPurchaseDialog
          open
          onOpenChange={changed}
          plan={plan}
          userQuota={5000000}
        />
      </I18nextProvider>
    </QueryClientProvider>
  )
  await waitFor(() =>
    expect(
      screen.getByRole('button', { name: /Pay with Balance/ })
    ).toBeEnabled()
  )
  fireEvent.click(screen.getByRole('button', { name: /Pay with Balance/ }))
  await waitFor(() => expect(post).toHaveBeenCalledTimes(1))
  expect(changed).not.toHaveBeenCalled()
  await waitFor(() =>
    expect(
      screen.getByRole('button', { name: /Pay with Balance/ })
    ).toBeEnabled()
  )
  fireEvent.click(screen.getByRole('button', { name: /Pay with Balance/ }))
  await waitFor(() => expect(post).toHaveBeenCalledTimes(2))
  const first = post.mock.calls[0]?.[1]
  expect(first).toMatchObject({
    plan_id: 7,
    version_id: 9,
    event_id: expect.any(String),
  })
  expect(post.mock.calls[1]?.[1]).toEqual(first)
})

it('uses subscription-eligible packs rather than the legacy wallet balance for a versioned purchase', async () => {
  const i18n = createInstance()
  await i18n.use(initReactI18next).init({
    lng: 'en',
    resources: { en: { translation: {} } },
    interpolation: { escapeValue: false },
  })
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: { accounting_version: 1, subscription_available: 500000 },
    },
  })
  render(
    <QueryClientProvider
      client={
        new QueryClient({ defaultOptions: { queries: { retry: false } } })
      }
    >
      <I18nextProvider i18n={i18n}>
        <SubscriptionPurchaseDialog
          open
          onOpenChange={vi.fn()}
          plan={plan}
          userQuota={0}
        />
      </I18nextProvider>
    </QueryClientProvider>
  )
  await waitFor(() =>
    expect(
      screen.getByRole('button', { name: 'Pay with Balance' })
    ).toBeEnabled()
  )
})

it('displays the locked payment price to six decimals instead of rounding a micro-price to zero', async () => {
  const i18n = createInstance()
  await i18n.use(initReactI18next).init({
    lng: 'en',
    resources: { en: { translation: {} } },
    interpolation: { escapeValue: false },
  })
  render(
    <QueryClientProvider client={new QueryClient()}>
      <I18nextProvider i18n={i18n}>
        <SubscriptionPurchaseDialog
          open
          onOpenChange={vi.fn()}
          plan={{ plan: { ...plan.plan, price_amount: 0.000001 } }}
          userQuota={0}
        />
      </I18nextProvider>
    </QueryClientProvider>
  )
  expect(screen.getByText('Amount Due').parentElement).toHaveTextContent(
    '$0.000001'
  )
})

it('starts a new purchase only after resolving the original paid order, even when its checkout response was lost', async () => {
  useAuthStore.setState({
    auth: {
      ...useAuthStore.getState().auth,
      user: { id: 11, username: 'buyer', role: 1 },
    },
  })
  const i18n = createInstance()
  await i18n.use(initReactI18next).init({
    lng: 'en',
    resources: { en: { translation: {} } },
    interpolation: { escapeValue: false },
  })
  const key = 'subscription-intent:11:7:9:stripe'
  sessionStorage.setItem(key, 'original-uncertain-intent')
  sessionStorage.setItem(
    'subscription-intent:12:7:9:stripe',
    'another-user-intent'
  )
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data:
        url === '/api/subscription/orders'
          ? {
              total: 1,
              items: [
                {
                  id: 18,
                  event_id: 'original-uncertain-intent',
                  plan_id: 7,
                  version_id: 9,
                  provider: 'stripe',
                  price_micros: 1000000,
                  currency: 'USD',
                  payment_state: 'verified',
                  needs_review: false,
                  paid_at: 99,
                  expires_at: 200,
                  rights_cancelled_at: 0,
                },
              ],
            }
          : {
              server_time: 100,
              subscriptions: [],
              all_subscriptions: [],
              current_rights: [],
            },
    },
  }))
  render(
    <QueryClientProvider
      client={
        new QueryClient({ defaultOptions: { queries: { retry: false } } })
      }
    >
      <I18nextProvider i18n={i18n}>
        <SubscriptionOrders />
      </I18nextProvider>
    </QueryClientProvider>
  )
  fireEvent.click(
    await screen.findByRole('button', { name: 'Start another purchase' })
  )
  expect(sessionStorage.getItem(key)).toBeNull()
  expect(sessionStorage.getItem('subscription-intent:12:7:9:stripe')).toBe(
    'another-user-intent'
  )
})

it('rejects a tag whose encoded value exceeds the server byte limit', () => {
  const values = planToFormValues(plan.plan)
  values.entitlement_tags = [{ key: 'resource', value: '中'.repeat(700) }]
  expect(getPlanFormSchema(t).safeParse(values).success).toBe(false)
})

it('retains the purchased currency when editing a CNY plan draft', () => {
  const values = planToFormValues({ ...plan.plan, currency: 'CNY' })
  expect(formValuesToPlanPayload(values).plan.currency).toBe('CNY')
})

it.each([
  { currency: 'USD', price: 1 },
  { currency: 'CNY', price: 0.000001 },
])(
  'does not offer versioned Epay checkout for unsupported contract $currency/$price',
  async ({ currency, price }) => {
    const i18n = createInstance()
    await i18n.use(initReactI18next).init({
      lng: 'en',
      resources: { en: { translation: {} } },
      interpolation: { escapeValue: false },
    })
    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: { accounting_version: 1, subscription_available: 0 },
      },
    })
    render(
      <QueryClientProvider
        client={
          new QueryClient({ defaultOptions: { queries: { retry: false } } })
        }
      >
        <I18nextProvider i18n={i18n}>
          <SubscriptionPurchaseDialog
            open
            onOpenChange={vi.fn()}
            plan={{
              ...plan,
              plan: { ...plan.plan, currency, price_amount: price },
            }}
            enableOnlineTopUp
            epayMethods={[{ name: 'Alipay', type: 'alipay' }]}
          />
        </I18nextProvider>
      </QueryClientProvider>
    )
    expect(
      screen.queryByRole('button', { name: /^Pay$/ })
    ).not.toBeInTheDocument()
  }
)

it.each(['zhCN', 'zhTW', 'en', 'fr', 'ru', 'ja', 'vi', 'invalid-language'])(
  'preserves a micro-price and the smallest credit unit in interface language %s',
  (language) => {
    const price = formatContractCurrencyAmount(
      0.000001,
      'USD',
      toIntlLocale(language)
    )
    expect(price).toMatch(/0[,.]000001/)
    expect(formatAccountingQuota(1)).toContain('0.000002')
  }
)

function PublishFixture() {
  const context = useSubscriptions()
  return (
    <>
      <Button
        type='button'
        onClick={() => {
          context.setCurrentRow(plan)
          context.setOpen('versions')
        }}
      >
        Review contract
      </Button>
      <PlanVersionsDialog />
    </>
  )
}

it('publishes the explicitly reviewed draft revision and digest and retains its event when the response is uncertain', async () => {
  const i18n = createInstance()
  await i18n.use(initReactI18next).init({
    lng: 'en',
    resources: { en: { translation: {} } },
    interpolation: { escapeValue: false },
  })
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data: String(url).includes('/versions')
        ? {
            draft: { plan: plan.plan, digest: 'reviewed-draft-digest' },
            latest_revision: 4,
            versions: [],
            total: 0,
          }
        : [],
    },
  }))
  const post = vi
    .spyOn(api, 'post')
    .mockRejectedValueOnce(new Error('response lost'))
    .mockResolvedValueOnce({ data: { success: true } })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <I18nextProvider i18n={i18n}>
        <SubscriptionsProvider>
          <PublishFixture />
        </SubscriptionsProvider>
      </I18nextProvider>
    </QueryClientProvider>
  )
  fireEvent.click(screen.getByRole('button', { name: 'Review contract' }))
  const publish = await screen.findByRole('button', {
    name: 'Publish reviewed draft',
  })
  expect(publish).toBeEnabled()
  fireEvent.click(publish)
  await waitFor(() => expect(post).toHaveBeenCalledTimes(1))
  await waitFor(() => expect(publish).toBeEnabled())
  fireEvent.click(publish)
  await waitFor(() => expect(post).toHaveBeenCalledTimes(2))
  expect(post.mock.calls[0]?.[1]).toMatchObject({
    expected_revision: 4,
    expected_plan_digest: 'reviewed-draft-digest',
    event_id: expect.any(String),
  })
  expect(post.mock.calls[1]?.[1]).toEqual(post.mock.calls[0]?.[1])
  client.clear()
})

it('explains and disables publication of an incompatible legacy reset schedule', async () => {
  const i18n = createInstance()
  await i18n.use(initReactI18next).init({
    lng: 'en',
    resources: { en: { translation: {} } },
    interpolation: { escapeValue: false },
  })
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data: String(url).includes('/versions')
        ? {
            draft: {
              plan: { ...plan.plan, quota_reset_period: 'daily' },
              digest: 'legacy-draft',
            },
            latest_revision: 0,
            versions: [],
            total: 0,
          }
        : [],
    },
  }))
  render(
    <QueryClientProvider
      client={
        new QueryClient({ defaultOptions: { queries: { retry: false } } })
      }
    >
      <I18nextProvider i18n={i18n}>
        <SubscriptionsProvider>
          <PublishFixture />
        </SubscriptionsProvider>
      </I18nextProvider>
    </QueryClientProvider>
  )
  fireEvent.click(screen.getByRole('button', { name: 'Review contract' }))
  expect(
    await screen.findByRole('button', { name: 'Publish reviewed draft' })
  ).toBeDisabled()
  expect(
    screen.getByText(/Publishing requires a 30-day monthly term/)
  ).toBeVisible()
})

it('uses the account server clock when an administrator reviews active purchased rights', async () => {
  const i18n = createInstance()
  await i18n.use(initReactI18next).init({
    lng: 'en',
    resources: { en: { translation: {} } },
    interpolation: { escapeValue: false },
  })
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2040-01-01T00:00:00Z'))
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  try {
    vi.spyOn(api, 'get').mockImplementation(async (url) => {
      const path = String(url)
      let data: unknown = { items: [], total: 0 }
      if (path.endsWith('/account')) {
        data = { accounting_version: 1, server_time: 100, packs: [] }
      } else if (path.endsWith('/plans')) data = [plan]
      else if (path.endsWith('/subscriptions')) {
        data = [
          {
            subscription: {
              id: 8,
              plan_id: 7,
              plan_version_id: 9,
              status: 'active',
              start_time: 90,
              end_time: 500,
              amount_total: 500000,
              amount_used: 0,
            },
          },
        ]
      }
      return { data: { success: true, data } }
    })
    await act(async () => {
      render(
        <QueryClientProvider client={client}>
          <I18nextProvider i18n={i18n}>
            <UserSubscriptionsDialog
              open
              onOpenChange={vi.fn()}
              user={{ id: 2 }}
            />
          </I18nextProvider>
        </QueryClientProvider>
      )
      await vi.advanceTimersByTimeAsync(100)
    })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    expect(screen.getByText('Active')).toBeVisible()
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Actions' }))
      await vi.advanceTimersByTimeAsync(50)
    })
    expect(
      screen.getByRole('menuitem', { name: 'Reset quota' })
    ).toHaveAttribute('aria-disabled', 'true')
  } finally {
    client.clear()
    vi.useRealTimers()
  }
})

function PlanPriceFixture() {
  const columns = useSubscriptionsColumns()
  const table = useReactTable({
    columns,
    data: [
      {
        ...plan,
        plan: { ...plan.plan, currency: 'CNY', price_amount: 0.000001 },
      },
    ],
    getCoreRowModel: getCoreRowModel(),
  })
  return (
    <table>
      <tbody>
        {table.getRowModel().rows.map((row) => (
          <tr key={row.id}>
            {row
              .getVisibleCells()
              .filter((cell) => cell.column.id === 'price')
              .map((cell) => (
                <td key={cell.id}>
                  {flexRender(cell.column.columnDef.cell, cell.getContext())}
                </td>
              ))}
          </tr>
        ))}
      </tbody>
    </table>
  )
}

it('shows the admin catalog price in its contract currency without rounding away a micro-price', async () => {
  const i18n = createInstance()
  await i18n.use(initReactI18next).init({
    lng: 'en',
    resources: { en: { translation: {} } },
    interpolation: { escapeValue: false },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <PlanPriceFixture />
    </I18nextProvider>
  )
  expect(screen.getByRole('cell')).toHaveTextContent('¥0.000001')
  expect(screen.getByRole('cell')).not.toHaveTextContent('$')
})
