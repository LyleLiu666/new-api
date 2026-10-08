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
export interface CreditPack {
  id: number
  source_type: string
  issued: number
  available: number
  held: number
  spent: number
  expired: number
  revoked: number
  starts_at: number
  expires_at: number
  use_mask: number
  state: string
}

export interface CreditAccount {
  user_id: number
  accounting_version: number
  server_time: number
  api_available: number
  subscription_available: number
  held: number
  total: number
  page: number
  page_size: number
  packs: CreditPack[]
}

export interface CreditBillBalance {
  revision: number
  reference_quota: number
  charged: number
  uncollected: number
}

export interface CreditBill {
  id: number
  request_id: string
  model_name: string
  protocol: string
  state: string
  funding_source: string
  created_at: number
  current: CreditBillBalance
}

export interface CreditUsageFact {
  field: string
  quantity: number | null
  unit: string
  source: string
  algorithm?: string
  partial?: boolean
}

export interface CreditBillDetail extends CreditBill {
  original: CreditBillBalance
  usage_evidence_id?: number
  manually_confirmed: boolean
  usage?: { version: string; facts: CreditUsageFact[] }
  revisions: (CreditBillBalance & {
    id: number
    refunded: number
    created_at: number
    reason?: string
  })[]
  total: number
  page: number
  page_size: number
}

export interface CreditPage<T> {
  items: T[]
  total: number
  page: number
  page_size: number
  server_time: number
}
