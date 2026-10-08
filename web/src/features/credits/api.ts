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
import { api } from '@/lib/api'
import { requireServerSuccess } from '@/lib/server-error-message'

import type {
  CreditAccount,
  CreditBill,
  CreditBillDetail,
  CreditPage,
} from './types'

export const creditQueryKeys = {
  all: ['credits'] as const,
  account: (userId: number | undefined, page: number) =>
    ['credits', userId ?? 'self', 'account', page] as const,
  bills: (userId: number | undefined, page: number) =>
    ['credits', userId ?? 'self', 'bills', page] as const,
  bill: (userId: number | undefined, id: number, page: number) =>
    ['credits', userId ?? 'self', 'bill', id, page] as const,
}

export async function getCreditAccount(
  page = 1,
  userId?: number
): Promise<CreditAccount> {
  const path =
    userId === undefined ? '/api/credit/account' : '/api/credit/admin/account'
  return requireServerSuccess(
    (
      await api.get(path, {
        params: { user_id: userId, p: page, page_size: 10 },
      })
    ).data
  ).data
}

export async function getCreditBills(
  page = 1,
  userId?: number
): Promise<CreditPage<CreditBill>> {
  const path =
    userId === undefined ? '/api/credit/bills' : '/api/credit/admin/bills'
  return requireServerSuccess(
    (
      await api.get(path, {
        params: { user_id: userId, p: page, page_size: 10 },
      })
    ).data
  ).data
}

export async function getCreditBill(
  id: number,
  page = 1,
  userId?: number
): Promise<CreditBillDetail> {
  const path =
    userId === undefined
      ? `/api/credit/bills/${id}`
      : `/api/credit/admin/bills/${id}`
  return requireServerSuccess(
    (
      await api.get(path, {
        params: { user_id: userId, p: page, page_size: 10 },
      })
    ).data
  ).data
}
