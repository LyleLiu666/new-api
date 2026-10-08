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
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { t, createInstance } from 'i18next'
import { I18nextProvider, initReactI18next } from 'react-i18next'
import { expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { CreditAdminDialog } from '../components/credit-admin-dialog'
import { CREDIT_ADMIN_DEFAULTS, creditAdminFormSchema } from '../lib/admin-form'

it('requires explicit verified zero quantities before approving a zero-cost unknown bill', () => {
  const schema = creditAdminFormSchema(t)
  const values = {
    ...CREDIT_ADMIN_DEFAULTS,
    action: 'review',
    target_id: 18,
    amount: 0,
    reason: 'Reviewed original provider receipt',
    reference: 'receipt-18',
    facts: [{ field: 'completion_tokens', unit: 'token', quantity: 0 }],
  }
  expect(schema.safeParse(values).success).toBe(false)
  expect(schema.safeParse({ ...values, zero_confirmed: true }).success).toBe(
    true
  )
  expect(
    schema.safeParse({
      ...values,
      zero_confirmed: true,
      facts: [{ field: 'completion_tokens', unit: 'token', quantity: 5 }],
    }).success
  ).toBe(false)
})

it('requires an explicit expiry after the start when issuing promotional credits', () => {
  const schema = creditAdminFormSchema(t)
  const values = {
    ...CREDIT_ADMIN_DEFAULTS,
    amount: 1,
    reason: 'Promotion',
    starts_at: '2026-10-09T12:00',
    expires_at: '2026-11-09T12:00',
  }
  expect(schema.safeParse(values).success).toBe(true)
  expect(schema.safeParse({ ...values, expires_at: '' }).success).toBe(false)
  expect(
    schema.safeParse({ ...values, expires_at: values.starts_at }).success
  ).toBe(false)
})

it('retries an uncertain credit grant with the original event and server start time without storing private evidence', async () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'admin', role: 10 })
  const i18n = createInstance()
  await i18n.use(initReactI18next).init({
    lng: 'en',
    resources: { en: { translation: {} } },
    interpolation: { escapeValue: false },
  })
  let serverTime = 100
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    let data: unknown
    if (String(url).endsWith('/account')) {
      data = {
        accounting_version: 1,
        server_time: serverTime++,
        api_available: 0,
        subscription_available: 0,
        held: 0,
        packs: [],
        total: 0,
        page_size: 10,
      }
    } else if (String(url).endsWith('/work')) {
      data = {
        requests: [],
        total: 0,
        logs: [],
        logs_total: 0,
        adjustment_logs: [],
        adjustment_logs_total: 0,
      }
    } else data = { items: [], total: 0, page_size: 10 }
    return { data: { success: true, data } }
  })
  const post = vi
    .spyOn(api, 'post')
    .mockRejectedValueOnce(new Error('response lost'))
    .mockResolvedValueOnce({ data: { success: true } })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <I18nextProvider i18n={i18n}>
        <CreditAdminDialog
          open
          onOpenChange={vi.fn()}
          user={{ id: 2, username: 'buyer' }}
        />
      </I18nextProvider>
    </QueryClientProvider>
  )
  await screen.findByText('Time-limited credits')
  fireEvent.change(screen.getByLabelText(/Credit amount/), {
    target: { value: '1' },
  })
  fireEvent.change(screen.getByLabelText('Expires at', { exact: true }), {
    target: { value: '2030-01-01T12:00' },
  })
  fireEvent.change(screen.getByLabelText('Reason / evidence'), {
    target: { value: 'private verified promotion evidence' },
  })
  fireEvent.click(screen.getByRole('button', { name: 'Submit operation' }))
  await waitFor(() => expect(post).toHaveBeenCalledTimes(1))
  await waitFor(() =>
    expect(
      screen.getByRole('button', { name: 'Submit operation' })
    ).toBeEnabled()
  )
  fireEvent.click(screen.getByRole('button', { name: 'Submit operation' }))
  await waitFor(() => expect(post).toHaveBeenCalledTimes(2))
  expect(post.mock.calls[0]?.[1]).toMatchObject({
    user_id: 2,
    amount: 500000,
    starts_at: 101,
    use_mask: 1,
    event_id: expect.any(String),
  })
  expect(post.mock.calls[1]?.[1]).toEqual(post.mock.calls[0]?.[1])
  expect(Object.values(sessionStorage).join(' ')).not.toContain(
    'private verified promotion evidence'
  )
  client.clear()
})
