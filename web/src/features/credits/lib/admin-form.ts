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
import type { TFunction } from 'i18next'
import { z } from 'zod'

export function creditAdminFormSchema(t: TFunction) {
  return z
    .object({
      action: z.enum(['grant', 'freeze', 'cash', 'adjust', 'review', 'retry']),
      amount: z.coerce.number().finite().min(0),
      zero_confirmed: z.boolean(),
      target_id: z.coerce.number().int().min(0).max(Number.MAX_SAFE_INTEGER),
      starts_at: z.string(),
      expires_at: z.string(),
      use_mask: z.enum(['1', '2', '3']),
      reason: z.string().trim().min(1, t('Reason is required')).max(1024),
      reference: z.string().max(256),
      cash_state: z.enum(['confirmed', 'rejected']),
      recovery_kind: z.enum(['request_id', 'outbox_id', 'adjustment_id']),
      facts: z
        .array(
          z.object({
            field: z.string().trim().min(1).max(128),
            unit: z.enum(['token', 'count', 'second', 'credit', 'request']),
            quantity: z.coerce.number().finite().min(0).max(2147483647),
          })
        )
        .max(64),
    })
    .superRefine((values, ctx) => {
      const invalid = (path: string, message: string) =>
        ctx.addIssue({ code: 'custom', path: [path], message: t(message) })
      if (values.action === 'grant') {
        if (values.amount <= 0) invalid('amount', 'Please enter amount')
        const end = Date.parse(values.expires_at)
        const start = values.starts_at ? Date.parse(values.starts_at) : 0
        if (!Number.isFinite(end) || end <= start) {
          invalid('expires_at', 'Expiry must follow the start time')
        }
        if (values.starts_at && !Number.isFinite(start)) {
          invalid('starts_at', 'Invalid start time')
        }
      } else if (values.target_id <= 0) invalid('target_id', 'Select a record')
      if (values.action === 'review' && values.amount === 0) {
        if (!values.zero_confirmed) {
          invalid(
            'zero_confirmed',
            'Explicitly confirm the verified zero charge'
          )
        }
      }
      if (['adjust', 'review'].includes(values.action)) {
        if (!values.facts.length) {
          invalid('facts', 'Verified quantities are required')
        }
        if (
          new Set(values.facts.map((fact) => fact.field)).size !==
          values.facts.length
        ) {
          invalid('facts', 'Quantity names must be unique')
        }
      }
      if (
        ['cash', 'review'].includes(values.action) &&
        !values.reference.trim()
      ) {
        invalid('reference', 'External reference is required')
      }
    })
}

export type CreditAdminForm = z.infer<ReturnType<typeof creditAdminFormSchema>>
export const CREDIT_ADMIN_DEFAULTS: CreditAdminForm = {
  action: 'grant',
  amount: 0,
  zero_confirmed: false,
  target_id: 0,
  starts_at: '',
  expires_at: '',
  use_mask: '1',
  reason: '',
  reference: '',
  cash_state: 'confirmed',
  recovery_kind: 'request_id',
  facts: [],
}
