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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { StaticDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import { toIntlLocale } from '@/i18n/languages'
import { formatContractCurrencyAmount } from '@/lib/currency'
import { formatQuota } from '@/lib/format'
import { requireServerSuccess } from '@/lib/server-error-message'

import { getPlanVersions, publishPlanVersion } from '../../api'
import { useSubscriptions } from '../subscriptions-provider'

export function PlanVersionsDialog() {
  const { t, i18n } = useTranslation()
  const { open, setOpen, currentRow, triggerRefresh } = useSubscriptions()
  const client = useQueryClient()
  const [page, setPage] = useState(1)
  const planId = currentRow?.plan.id ?? 0
  const versions = useQuery({
    queryKey: ['subscriptions', 'versions', planId, page],
    enabled: open === 'versions' && planId > 0,
    refetchOnWindowFocus: false,
    queryFn: async () => {
      const response = requireServerSuccess(await getPlanVersions(planId, page))
      if (!response.data) throw new Error(t('Invalid response'))
      return response.data
    },
  })
  const publish = useMutation({
    mutationFn: async () => {
      const draft = versions.data
      if (!draft || versions.isFetching) throw new Error(t('Invalid response'))
      const key = `subscription-publish:${planId}:${draft.latest_revision}:${draft.draft.digest}`
      const event = sessionStorage.getItem(key) || crypto.randomUUID()
      sessionStorage.setItem(key, event)
      return requireServerSuccess(
        await publishPlanVersion(planId, {
          expected_revision: draft.latest_revision,
          expected_plan_digest: draft.draft.digest,
          event_id: event,
        })
      )
    },
    onSuccess: async () => {
      toast.success(t('Version published'))
      triggerRefresh()
      await client.invalidateQueries({
        queryKey: ['subscriptions', 'versions', planId],
      })
    },
    retry: false,
  })
  const data = versions.data
  const publishableTerm =
    !!data &&
    data.draft.plan.duration_unit === 'month' &&
    data.draft.plan.duration_value === 1 &&
    (!data.draft.plan.quota_reset_period ||
      data.draft.plan.quota_reset_period === 'never')
  return (
    <Dialog
      open={open === 'versions'}
      onOpenChange={(value) => {
        if (!value) {
          setOpen(null)
          setPage(1)
        }
      }}
      title={t('Published plan versions')}
      description={t(
        'Publishing affects new purchases. Existing rights keep their purchased version.'
      )}
    >
      {versions.isPending && <LoadingState />}
      {versions.isError && (
        <ErrorState onRetry={() => void versions.refetch()} />
      )}
      {data && (
        <div className='space-y-4'>
          <div className='space-y-2 rounded-md border p-3'>
            <p>{data.draft.plan.title}</p>
            <p>
              {t('Locked price')}:{' '}
              {formatContractCurrencyAmount(
                data.draft.plan.price_amount,
                data.draft.plan.currency,
                toIntlLocale(i18n.resolvedLanguage || i18n.language)
              )}
            </p>
            <p>
              {t('Validity')}:{' '}
              {publishableTerm
                ? t('30 days from confirmed payment')
                : t(
                    'Publishing requires a 30-day monthly term and independent first-use windows.'
                  )}
            </p>
            <p>
              {t('Credit-pack purchase')}:{' '}
              {data.draft.plan.allow_balance_pay ? t('Enabled') : t('Disabled')}
            </p>
            <p>
              {t('Add-on fallback')}:{' '}
              {data.draft.plan.allow_wallet_overflow
                ? t('Enabled')
                : t('Disabled')}
            </p>
            <p>
              {t('Purchase limit')}:{' '}
              {data.draft.plan.max_purchase_per_user || t('Unlimited')}
            </p>
            <p>
              {t('Upgrade group')}: {data.draft.plan.upgrade_group || '—'}
            </p>
            <p>
              {t('Plan Quota')}: {formatQuota(data.draft.plan.total_amount)}
            </p>
            {(data.draft.plan.window_rules || []).map((rule) => (
              <p key={rule.id}>
                {rule.id}: {rule.duration_seconds} {t('seconds')} ·{' '}
                {formatQuota(rule.limit)}
              </p>
            ))}
            {Object.entries(data.draft.plan.entitlement_tags || {}).map(
              ([key, value]) => (
                <p key={key}>
                  {key}: {value}
                </p>
              )
            )}
            <Button
              disabled={
                publish.isPending || versions.isFetching || !publishableTerm
              }
              onClick={() => publish.mutate()}
            >
              {t('Publish reviewed draft')}
            </Button>
          </div>
          <StaticDataTable
            data={data.versions}
            getRowKey={(row) => row.id}
            columns={[
              {
                id: 'revision',
                header: t('Version'),
                cell: (row) => row.revision,
              },
              {
                id: 'price',
                header: t('Plan Price'),
                cell: (row) =>
                  formatContractCurrencyAmount(
                    row.price_micros / 1000000,
                    row.currency,
                    toIntlLocale(i18n.resolvedLanguage || i18n.language)
                  ),
              },
              {
                id: 'duration',
                header: t('Validity'),
                cell: (row) => `${row.duration_seconds} ${t('seconds')}`,
              },
            ]}
          />
          <div className='flex gap-2'>
            <Button
              variant='outline'
              disabled={page <= 1 || versions.isFetching}
              onClick={() => setPage(page - 1)}
            >
              {t('Previous')}
            </Button>
            <Button
              variant='outline'
              disabled={page * 10 >= data.total || versions.isFetching}
              onClick={() => setPage(page + 1)}
            >
              {t('Next')}
            </Button>
          </div>
        </div>
      )}
    </Dialog>
  )
}
