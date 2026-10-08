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
import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { createInstance } from 'i18next'
import { I18nextProvider, initReactI18next } from 'react-i18next'
import { beforeEach, expect, it, vi } from 'vitest'

import { CreditBalanceSummary } from '@/features/credits/components/credit-balance-summary'
import { CreditLedger } from '@/features/credits/components/credit-ledger'
import type { CreditAccount } from '@/features/credits/types'
import { SubscriptionRightsPanel } from '@/features/subscriptions/components/subscription-rights-panel'
import { BillingHistoryDialog } from '@/features/wallet/components/dialogs/billing-history-dialog'
import en from '@/i18n/locales/en.json'
import fr from '@/i18n/locales/fr.json'
import ja from '@/i18n/locales/ja.json'
import ru from '@/i18n/locales/ru.json'
import viLocale from '@/i18n/locales/vi.json'
import zhTW from '@/i18n/locales/zh-TW.json'
import zh from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'

import { RechargeFormCard } from '../components/recharge-form-card'
import { SubscriptionPlansCard } from '../components/subscription-plans-card'
import { WalletStatsCard } from '../components/wallet-stats-card'

const i18n = createInstance()

beforeEach(async () => {
  await i18n.use(initReactI18next).init({
    lng: 'zh',
    fallbackLng: false,
    nsSeparator: false,
    resources: { en, zh },
    interpolation: { escapeValue: false },
  })
})

it('translates billing history statuses and updates them when the language changes', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        total: 3,
        items: ['success', 'pending', 'expired'].map((status, id) => ({
          id,
          status,
          amount: 10,
          money: 10,
          trade_no: `order-${id}`,
          payment_method: 'stripe',
          create_time: 1,
        })),
      },
    },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <BillingHistoryDialog open onOpenChange={vi.fn()} />
    </I18nextProvider>
  )
  await screen.findByText('order-0')
  for (const label of ['成功', '待确认', '已过期']) {
    expect(screen.getByText(label)).toBeVisible()
  }
  await act(() => i18n.changeLanguage('en'))
  for (const label of ['Success', 'Pending', 'Expired']) {
    expect(screen.getByText(label)).toBeVisible()
  }
})

it.each([
  { priceRatio: 7.3, paid: '584', saved: '146', full: '146' },
  { priceRatio: 0.000123, paid: '0.0098', saved: '0.0025', full: '0.0025' },
])(
  'translates recharge presets and minimum amount without changing precision at price ratio $priceRatio',
  async ({ priceRatio, paid, saved, full }) => {
    render(
      <I18nextProvider i18n={i18n}>
        <RechargeFormCard
          topupInfo={{
            enable_online_topup: true,
            enable_stripe_topup: false,
            pay_methods: [],
            min_topup: 1,
            stripe_min_topup: 1,
            amount_options: [100, 20],
            discount: {},
          }}
          presetAmounts={[{ value: 100, discount: 0.8 }, { value: 20 }]}
          selectedPreset={null}
          topupAmount={1}
          paymentAmount={priceRatio}
          priceRatio={priceRatio}
          calculating={false}
          paymentLoading={null}
          redemptionCode=''
          redeeming={false}
          onSelectPreset={vi.fn()}
          onTopupAmountChange={vi.fn()}
          onPaymentMethodSelect={vi.fn()}
          onRedemptionCodeChange={vi.fn()}
          onRedeem={vi.fn()}
        />
      </I18nextProvider>
    )
    expect(screen.getByRole('button', { name: /^100 / })).toHaveTextContent(
      `优惠 20%支付 ${paid} • 节省 ${saved}`
    )
    expect(screen.getByRole('button', { name: /^20 / })).toHaveTextContent(
      `支付 ${full}`
    )
    expect(screen.getByPlaceholderText('最低 1')).toBeVisible()
    await act(() => i18n.changeLanguage('en'))
    expect(screen.getByRole('button', { name: /^100 / })).toHaveTextContent(
      `20% OFFPay ${paid} • Save ${saved}`
    )
    expect(screen.getByRole('button', { name: /^20 / })).toHaveTextContent(
      `Pay ${full}`
    )
    expect(screen.getByPlaceholderText('Minimum 1')).toBeVisible()
  }
)

it('shows expiry-ordered packs, purpose-specific available funds and platform-covered bill amounts', async () => {
  await i18n.changeLanguage('en')
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (String(url).endsWith('/account')) {
      return {
        data: {
          success: true,
          data: {
            accounting_version: 1,
            server_time: 100,
            api_available: 25,
            subscription_available: 50,
            held: 15,
            total: 2,
            page_size: 10,
            packs: [
              {
                id: 1,
                source_type: 'checkin',
                issued: 20,
                available: 0,
                held: 0,
                spent: 0,
                expired: 20,
                revoked: 0,
                starts_at: 1,
                expires_at: 99,
                use_mask: 1,
                state: 'expired',
              },
              {
                id: 2,
                source_type: 'topup',
                issued: 50,
                available: 50,
                held: 0,
                spent: 0,
                expired: 0,
                revoked: 0,
                starts_at: 1,
                expires_at: 200,
                use_mask: 2,
                state: 'active',
              },
            ],
          },
        },
      }
    }
    if (String(url).endsWith('/bills')) {
      return {
        data: {
          success: true,
          data: {
            items: [
              {
                id: 7,
                request_id: 'owned-bill',
                model_name: 'model',
                protocol: 'text',
                state: 'settled',
                funding_source: 'credit_packs',
                current: {
                  reference_quota: 100,
                  charged: 75,
                  uncollected: 25,
                },
                created_at: 100,
              },
            ],
            total: 1,
            page_size: 10,
          },
        },
      }
    }
    return {
      data: {
        success: true,
        data: {
          request_id: 'owned-bill',
          model_name: 'model',
          funding_source: 'credit_packs',
          original: { reference_quota: 100, charged: 75 },
          current: { reference_quota: 100, charged: 75, uncollected: 25 },
          manually_confirmed: false,
          revisions: [],
          total: 0,
          page_size: 10,
          usage: {
            version: 'estimated-v1',
            facts: [
              {
                field: 'completion_tokens',
                quantity: null,
                unit: 'token',
                source: 'unknown',
              },
            ],
          },
        },
      },
    }
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <I18nextProvider i18n={i18n}>
        <CreditLedger />
      </I18nextProvider>
    </QueryClientProvider>
  )
  const first = await screen.findByText('#1 · Check-in')
  const row = first.closest('tr')
  expect(row).not.toBeNull()
  if (!row) throw new Error('Missing credit pack row')
  const expired = within(row)
  expect(expired.getByText('Expired')).toBeVisible()
  expect(expired.getByText('API consumption')).toBeVisible()
  expect(screen.getByText('Available for subscription purchase')).toBeVisible()
  fireEvent.click(await screen.findByRole('button', { name: 'owned-bill' }))
  expect(await screen.findByText(/estimated-v1/)).toBeVisible()
  expect(screen.getAllByText('Unknown')).toHaveLength(3)
  expect(screen.getAllByText('Platform-covered excess').length).toBeGreaterThan(
    0
  )
})

it('keeps a failed business response visible as an error instead of showing an empty successful wallet', async () => {
  await i18n.changeLanguage('en')
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: false, message: 'Account unavailable' },
  })
  render(
    <QueryClientProvider
      client={
        new QueryClient({ defaultOptions: { queries: { retry: false } } })
      }
    >
      <I18nextProvider i18n={i18n}>
        <CreditLedger />
      </I18nextProvider>
    </QueryClientProvider>
  )
  expect(await screen.findByRole('button', { name: 'Retry' })).toBeVisible()
  expect(screen.queryByText('Time-limited credits')).not.toBeInTheDocument()
})

it('uses server time for rights and removes expired tags even when refreshing fails', async () => {
  await i18n.changeLanguage('en')
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2040-01-01T00:00:00Z'))
  try {
    vi.spyOn(api, 'get')
      .mockResolvedValueOnce({
        data: {
          success: true,
          data: {
            server_time: 100,
            subscriptions: [],
            all_subscriptions: [],
            current_rights: [
              {
                id: 8,
                plan_version_id: 9,
                status: 'active',
                start_time: 90,
                end_time: 102,
                entitlement_tags: { resource: 'private-resource-tag' },
              },
            ],
            windows: [
              {
                subscription_id: 8,
                rule_id: 'five-hours',
                state: 'unstarted',
                limit: 60,
                held: 0,
                used: 0,
                reference_used: 0,
                available: 60,
                starts_at: 0,
                ends_at: 0,
              },
            ],
          },
        },
      })
      .mockRejectedValue(new Error('refresh unavailable'))
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    await act(async () => {
      render(
        <QueryClientProvider client={client}>
          <I18nextProvider i18n={i18n}>
            <SubscriptionRightsPanel />
          </I18nextProvider>
        </QueryClientProvider>
      )
      await vi.advanceTimersByTimeAsync(100)
    })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    expect(screen.getByText('private-resource-tag')).toBeVisible()
    expect(screen.getAllByText('Starts on first use').length).toBeGreaterThan(0)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2100)
    })
    expect(screen.queryByText('private-resource-tag')).not.toBeInTheDocument()
    client.clear()
  } finally {
    vi.useRealTimers()
  }
})

it('shows purpose-specific credits instead of the stale legacy balance in wallet statistics', async () => {
  await i18n.changeLanguage('en')
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        accounting_version: 1,
        api_available: 500000,
        subscription_available: 1000000,
        held: 0,
        server_time: 100,
        packs: [],
      },
    },
  })
  render(
    <QueryClientProvider
      client={
        new QueryClient({ defaultOptions: { queries: { retry: false } } })
      }
    >
      <I18nextProvider i18n={i18n}>
        <WalletStatsCard
          user={{
            id: 1,
            username: 'buyer',
            quota: 0,
            used_quota: 0,
            request_count: 0,
            aff_quota: 0,
            aff_history_quota: 0,
            aff_count: 0,
            group: 'default',
          }}
        />
      </I18nextProvider>
    </QueryClientProvider>
  )
  expect(await screen.findByText('Available for API')).toBeVisible()
  expect(screen.queryByText('Current Balance')).not.toBeInTheDocument()
  expect(screen.getByText('Available for subscription purchase')).toBeVisible()
})

it('clears the displayed expired short-window generation while awaiting its refreshed state', async () => {
  await i18n.changeLanguage('en')
  vi.useFakeTimers()
  try {
    vi.spyOn(api, 'get')
      .mockResolvedValueOnce({
        data: {
          success: true,
          data: {
            server_time: 100,
            subscriptions: [],
            all_subscriptions: [],
            current_rights: [
              {
                id: 8,
                plan_version_id: 9,
                status: 'active',
                start_time: 90,
                end_time: 500,
                entitlement_tags: {},
              },
            ],
            windows: [
              {
                subscription_id: 8,
                rule_id: 'five-hours',
                state: 'active',
                limit: 500000,
                held: 50000,
                used: 100000,
                reference_used: 150000,
                available: 350000,
                starts_at: 99,
                ends_at: 102,
              },
            ],
          },
        },
      })
      .mockImplementation(() => new Promise(() => {}))
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    await act(async () => {
      render(
        <QueryClientProvider client={client}>
          <I18nextProvider i18n={i18n}>
            <SubscriptionRightsPanel />
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
      await vi.advanceTimersByTimeAsync(2100)
    })
    const row = screen.getByText('five-hours').closest('tr')
    if (!row) throw new Error('Missing short window')
    expect(within(row).getAllByText('Starts on first use')).toHaveLength(3)
    expect(within(row).queryByText('$0.1')).not.toBeInTheDocument()
    expect(within(row).getAllByText('$0')).toHaveLength(3)
    client.clear()
  } finally {
    vi.useRealTimers()
  }
})

it('updates the credit balance labels in all seven interface languages with an English fallback', async () => {
  const multilingual = createInstance()
  const resources = { en, zhCN: zh, zhTW, fr, ja, ru, vi: viLocale }
  await multilingual.use(initReactI18next).init({
    lng: 'en',
    fallbackLng: 'en',
    nsSeparator: false,
    resources,
    interpolation: { escapeValue: false },
  })
  const account: CreditAccount = {
    user_id: 2,
    accounting_version: 1,
    server_time: 100,
    api_available: 1,
    subscription_available: 0,
    held: 0,
    packs: [],
    total: 0,
    page: 1,
    page_size: 10,
  }
  render(
    <I18nextProvider i18n={multilingual}>
      <CreditBalanceSummary account={account} />
    </I18nextProvider>
  )
  for (const [language, resource] of Object.entries(resources)) {
    await act(() => multilingual.changeLanguage(language))
    expect(
      screen.getByText(resource.translation['Available for API'])
    ).toBeVisible()
    expect(screen.getByText('$0.000002')).toBeVisible()
  }
  await act(() => multilingual.changeLanguage('invalid-language'))
  expect(screen.getByText('Available for API')).toBeVisible()
})

it('keeps cancelled subscription history without presenting its unused quota as available or its term end as the cancellation time', async () => {
  await i18n.changeLanguage('en')
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data: String(url).endsWith('/plans')
        ? []
        : {
            server_time: 100,
            billing_preference: 'subscription_first',
            subscriptions: [],
            all_subscriptions: [
              {
                subscription: {
                  id: 8,
                  plan_id: 7,
                  plan_version_id: 9,
                  status: 'cancelled',
                  start_time: 90,
                  end_time: 500,
                  amount_total: 500000,
                  amount_used: 50000,
                },
              },
            ],
            windows: [],
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
        <SubscriptionPlansCard topupInfo={null} />
      </I18nextProvider>
    </QueryClientProvider>
  )
  expect(await screen.findByText('Cancelled')).toBeVisible()
  expect(screen.queryByText(/Remaining \$0.9/)).not.toBeInTheDocument()
  expect(screen.queryByText(/Cancelled at/)).not.toBeInTheDocument()
})
