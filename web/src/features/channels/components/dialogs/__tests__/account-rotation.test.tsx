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
import { createInstance } from 'i18next'
import { I18nextProvider, initReactI18next } from 'react-i18next'
import { expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { ChannelAccountsDialog } from '../channel-accounts-dialog'

it('rotates the reviewed account version and excludes credentials from mutation cache', async () => {
  useAuthStore.setState({
    auth: {
      ...useAuthStore.getState().auth,
      user: { id: 1, username: 'admin', role: 100 },
    },
  })
  const i18n = createInstance()
  await i18n.use(initReactI18next).init({
    lng: 'en',
    resources: { en: { translation: {} } },
    interpolation: { escapeValue: false },
  })
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: [
        {
          id: 'stable-account',
          channel_id: 7,
          credential_version: 3,
          retired_at: 0,
        },
      ],
    },
  })
  const secret = 'private-upstream-credential'
  const put = vi.spyOn(api, 'put').mockRejectedValue({
    message: 'Network Error',
    config: { data: JSON.stringify({ key: secret }) },
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  const changed = vi.fn()
  render(
    <QueryClientProvider client={client}>
      <I18nextProvider i18n={i18n}>
        <ChannelAccountsDialog channelId={7} open onOpenChange={changed} />
      </I18nextProvider>
    </QueryClientProvider>
  )
  fireEvent.click(
    await screen.findByRole('button', { name: 'Rotate credential' })
  )
  fireEvent.change(screen.getByLabelText('New credential'), {
    target: { value: secret },
  })
  fireEvent.click(screen.getByRole('button', { name: 'Confirm rotation' }))
  await waitFor(() => expect(put).toHaveBeenCalledTimes(1))
  await waitFor(() =>
    expect(
      screen.getByRole('button', { name: 'Confirm rotation' })
    ).toBeEnabled()
  )
  expect(put.mock.calls[0]).toEqual([
    '/api/channel/7/accounts/stable-account/credential',
    { credential_version: 3, key: secret },
  ])
  expect(
    JSON.stringify(
      client
        .getMutationCache()
        .getAll()
        .map((mutation) => mutation.state)
    )
  ).not.toContain(secret)
  expect(screen.queryByText(secret)).not.toBeInTheDocument()
  fireEvent.keyDown(document, { key: 'Escape' })
  await waitFor(() => expect(changed).toHaveBeenCalledWith(false))
  expect(screen.queryByLabelText('New credential')).not.toBeInTheDocument()
})
