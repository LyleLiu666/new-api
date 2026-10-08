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
import { useQuery } from '@tanstack/react-query'
import { useEffect } from 'react'
import { useTranslation } from 'react-i18next'

import { StaticDataTable } from '@/components/data-table'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { TitledCard } from '@/components/ui/titled-card'
import { formatAccountingQuota as formatQuota } from '@/features/credits/lib/format'
import { useServerClock } from '@/hooks/use-server-clock'
import { toIntlLocale } from '@/i18n/languages'
import { requireServerSuccess } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import { getSelfSubscriptionFull } from '../api'

export function SubscriptionRightsPanel(props: { refreshKey?: number }) {
  const { t, i18n } = useTranslation()
  const viewer = useAuthStore((state) => state.auth.user?.id)
  const query = useQuery({
    queryKey: ['subscriptions', 'self-rights', viewer, props.refreshKey],
    queryFn: async () => {
      const response = requireServerSuccess(await getSelfSubscriptionFull())
      if (!response.data) throw new Error(t('Invalid response'))
      return response.data
    },
    refetchInterval: 30000,
  })
  const now = useServerClock(query.data?.server_time, query.dataUpdatedAt)
  const rights = (query.data?.current_rights || []).filter(
    (right) =>
      right.status === 'active' &&
      right.start_time <= now &&
      right.end_time > now
  )
  const windows = (query.data?.windows || [])
    .filter((window) =>
      rights.some((right) => right.id === window.subscription_id)
    )
    .map((window) => {
      if (
        window.rule_id === 'term' ||
        window.ends_at === 0 ||
        window.ends_at > now
      ) {
        return window
      }
      return {
        ...window,
        state: 'unstarted',
        starts_at: 0,
        ends_at: 0,
        held: 0,
        used: 0,
        reference_used: 0,
        available: window.limit,
      }
    })
  const date = (seconds: number) =>
    seconds > 0
      ? new Date(seconds * 1000).toLocaleString(
          toIntlLocale(i18n.resolvedLanguage || i18n.language)
        )
      : t('Starts on first use')
  const {
    data: rightsData,
    refetch: refreshRights,
    dataUpdatedAt: rightsUpdatedAt,
  } = query
  useEffect(() => {
    if (rightsData?.server_time === undefined) return
    const serverTime = rightsData.server_time
    const deadlines = [
      ...(rightsData.current_rights || []).flatMap((right) => [
        right.start_time,
        right.end_time,
      ]),
      ...(rightsData.windows || []).map((window) => window.ends_at),
    ].filter((end) => end > serverTime)
    if (!deadlines.length) return
    const timer = setTimeout(
      () => void refreshRights(),
      Math.min(
        2147483647,
        Math.max(0, Math.min(...deadlines) * 1000 - serverTime * 1000)
      )
    )
    return () => clearTimeout(timer)
  }, [rightsUpdatedAt, rightsData, refreshRights])
  if (query.isPending) return <LoadingState />
  if (query.isError) return <ErrorState onRetry={() => void query.refetch()} />
  if (query.data?.server_time === undefined) return null
  return (
    <TitledCard
      title={t('Subscription windows and rights')}
      description={t(
        'All applicable windows limit the same consumption. Add-on credits have their own balance and expiry.'
      )}
    >
      <div className='space-y-4'>
        {rights.map((right) => (
          <div key={right.id} className='space-y-1 rounded-md border p-3'>
            <p>
              {t('Subscription')} #{right.id} · {t('Version')} #
              {right.plan_version_id}
            </p>
            <p>
              {t('Expires at')}: {date(right.end_time)}
            </p>
            <dl className='flex flex-wrap gap-3'>
              {Object.entries(right.entitlement_tags || {}).map(
                ([key, value]) => (
                  <div key={key}>
                    <dt className='text-muted-foreground text-xs'>{key}</dt>
                    <dd>{value}</dd>
                  </div>
                )
              )}
            </dl>
          </div>
        ))}
        {!rights.length && <p>{t('No active rights')}</p>}
        <StaticDataTable
          data={windows}
          getRowKey={(window) => `${window.subscription_id}:${window.rule_id}`}
          columns={[
            {
              id: 'subscription',
              header: t('Subscription'),
              cell: (window) => `#${window.subscription_id}`,
            },
            {
              id: 'window',
              header: t('Window'),
              cell: (window) =>
                window.rule_id === 'term'
                  ? t('Subscription term')
                  : window.rule_id,
            },
            {
              id: 'state',
              header: t('Status'),
              cell: (window) => {
                if (
                  window.state === 'unstarted' ||
                  (window.ends_at > 0 && window.ends_at <= now)
                ) {
                  return t('Starts on first use')
                }
                if (window.state === 'pending') return t('Awaiting first use')
                return t('Active')
              },
            },
            {
              id: 'available',
              header: t('Available'),
              cell: (window) =>
                window.unlimited
                  ? t('Unlimited')
                  : formatQuota(
                      window.ends_at > 0 && window.ends_at <= now
                        ? window.limit
                        : window.available
                    ),
            },
            {
              id: 'held',
              header: t('Reserved'),
              cell: (window) => formatQuota(window.held),
            },
            {
              id: 'used',
              header: t('Used'),
              cell: (window) => formatQuota(window.used),
            },
            {
              id: 'reference',
              header: t('Reference usage'),
              cell: (window) => formatQuota(window.reference_used),
            },
            {
              id: 'start',
              header: t('Started at'),
              cell: (window) => date(window.starts_at),
            },
            {
              id: 'end',
              header: t('Window ends at'),
              cell: (window) => date(window.ends_at),
            },
          ]}
        />
      </div>
    </TitledCard>
  )
}
