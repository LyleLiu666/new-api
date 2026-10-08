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
import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { StaticDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { TitledCard } from '@/components/ui/titled-card'
import { toIntlLocale } from '@/i18n/languages'
import { useAuthStore } from '@/stores/auth-store'

import {
  creditQueryKeys,
  getCreditAccount,
  getCreditBill,
  getCreditBills,
} from '../api'
import { CREDIT_SOURCE_LABELS } from '../constants'
import { formatAccountingQuota as formatQuota } from '../lib/format'
import type { CreditPack } from '../types'

const stateLabels: Record<string, string> = {
  active: 'Active',
  expired: 'Expired',
  blocked: 'Frozen',
  scheduled: 'Not started',
  exhausted: 'Exhausted',
  executing: 'In progress',
  review: 'Needs review',
  settled: 'Settled',
  refunded: 'Released',
}

function AccountingPager(props: {
  page: number
  total: number
  pageSize: number
  disabled?: boolean
  onPage: (page: number) => void
}) {
  const { t } = useTranslation()
  return (
    <div className='mt-3 flex items-center justify-end gap-2'>
      <Button
        variant='outline'
        size='sm'
        disabled={props.page <= 1 || props.disabled}
        onClick={() => props.onPage(props.page - 1)}
      >
        {t('Previous')}
      </Button>
      <span className='text-muted-foreground text-sm'>
        {props.page} / {Math.max(1, Math.ceil(props.total / props.pageSize))}
      </span>
      <Button
        variant='outline'
        size='sm'
        disabled={props.page * props.pageSize >= props.total || props.disabled}
        onClick={() => props.onPage(props.page + 1)}
      >
        {t('Next')}
      </Button>
    </div>
  )
}

export function CreditLedger(props: { userId?: number; refreshKey?: number }) {
  const { t, i18n } = useTranslation()
  const viewerId = useAuthStore((state) => state.auth.user?.id)
  const [packPage, setPackPage] = useState(1)
  const [billPage, setBillPage] = useState(1)
  const [billId, setBillId] = useState<number | null>(null)
  const [revisionPage, setRevisionPage] = useState(1)
  const account = useQuery({
    queryKey: [
      ...creditQueryKeys.account(props.userId, packPage),
      viewerId,
      props.refreshKey,
    ],
    queryFn: () => getCreditAccount(packPage, props.userId),
    refetchInterval: 30000,
  })
  const bills = useQuery({
    queryKey: [
      ...creditQueryKeys.bills(props.userId, billPage),
      viewerId,
      props.refreshKey,
    ],
    queryFn: () => getCreditBills(billPage, props.userId),
    enabled: account.data?.accounting_version === 1,
    refetchInterval: 30000,
  })
  const detail = useQuery({
    queryKey: [
      ...creditQueryKeys.bill(props.userId, billId ?? 0, revisionPage),
      viewerId,
    ],
    queryFn: () => getCreditBill(billId ?? 0, revisionPage, props.userId),
    enabled: billId !== null,
  })
  const date = (seconds: number) =>
    seconds > 0
      ? new Date(seconds * 1000).toLocaleString(
          toIntlLocale(i18n.resolvedLanguage || i18n.language)
        )
      : '—'
  const {
    data: accountData,
    refetch: refreshAccount,
    dataUpdatedAt: accountUpdatedAt,
  } = account
  const { refetch: refreshBills } = bills
  useEffect(() => {
    if (!accountData) return
    const next = accountData.packs
      .flatMap((pack) => [pack.starts_at, pack.expires_at])
      .filter((time) => time > accountData.server_time)
    if (!next.length) return
    const timer = setTimeout(
      () => {
        void refreshAccount()
        void refreshBills()
      },
      Math.min(
        2147483647,
        Math.max(0, Math.min(...next) * 1000 - accountData.server_time * 1000)
      )
    )
    return () => clearTimeout(timer)
  }, [accountUpdatedAt, accountData, refreshAccount, refreshBills])
  if (account.isPending) return <LoadingState />
  if (account.isError) {
    return <ErrorState onRetry={() => void account.refetch()} />
  }
  if (!account.data || account.data.accounting_version !== 1) return null
  const data = account.data
  const packColumns = [
    {
      id: 'id',
      header: t('Credit pack'),
      cell: (pack: CreditPack) =>
        `#${pack.id} · ${t(CREDIT_SOURCE_LABELS[pack.source_type] || 'Unknown')}`,
    },
    {
      id: 'state',
      header: t('Status'),
      cell: (pack: CreditPack) => (
        <StatusBadge
          label={t(stateLabels[pack.state] || 'Unknown')}
          variant={pack.state === 'active' ? 'success' : 'neutral'}
          copyable={false}
        />
      ),
    },
    {
      id: 'available',
      header: t('Available'),
      cell: (pack: CreditPack) => formatQuota(pack.available),
    },
    {
      id: 'held',
      header: t('Reserved'),
      cell: (pack: CreditPack) => formatQuota(pack.held),
    },
    {
      id: 'spent',
      header: t('Used'),
      cell: (pack: CreditPack) => formatQuota(pack.spent),
    },
    {
      id: 'expired',
      header: t('Expired / revoked'),
      cell: (pack: CreditPack) =>
        `${formatQuota(pack.expired)} / ${formatQuota(pack.revoked)}`,
    },
    {
      id: 'purpose',
      header: t('Purpose'),
      cell: (pack: CreditPack) => {
        if (pack.use_mask === 1) return t('API consumption')
        if (pack.use_mask === 2) return t('Subscription purchase')
        return t('API and subscriptions')
      },
    },
    {
      id: 'starts',
      header: t('Valid from'),
      cell: (pack: CreditPack) => date(pack.starts_at),
    },
    {
      id: 'expires',
      header: t('Expires at'),
      cell: (pack: CreditPack) => date(pack.expires_at),
    },
  ]
  return (
    <div className='space-y-4'>
      <TitledCard
        title={t('Time-limited credits')}
        description={t(
          'Credits are consumed in expiry order. Expired and future credits are excluded from available balances.'
        )}
        action={
          <Button
            variant='outline'
            onClick={() => {
              void account.refetch()
              void bills.refetch()
            }}
          >
            {t('Refresh')}
          </Button>
        }
      >
        <div className='space-y-4'>
          <dl className='grid gap-3 sm:grid-cols-3'>
            <div>
              <dt>{t('Available for API')}</dt>
              <dd>
                {account.isFetching
                  ? t('Updating...')
                  : formatQuota(data.api_available)}
              </dd>
            </div>
            <div>
              <dt>{t('Available for subscription purchase')}</dt>
              <dd>
                {account.isFetching
                  ? t('Updating...')
                  : formatQuota(data.subscription_available)}
              </dd>
            </div>
            <div>
              <dt>{t('Reserved')}</dt>
              <dd>{formatQuota(data.held)}</dd>
            </div>
          </dl>
          <StaticDataTable
            data={data.packs}
            columns={packColumns}
            getRowKey={(pack) => pack.id}
            emptyContent={<EmptyState />}
          />
          <AccountingPager
            page={packPage}
            total={data.total}
            pageSize={data.page_size}
            disabled={account.isFetching}
            onPage={setPackPage}
          />
        </div>
      </TitledCard>
      <TitledCard
        title={t('Consumption bills')}
        description={t(
          'Reference cost, actual deduction and platform-covered excess are recorded separately. No user debt is created.'
        )}
      >
        {bills.isPending && <LoadingState />}
        {bills.isError && <ErrorState onRetry={() => void bills.refetch()} />}
        {bills.data && (
          <>
            <StaticDataTable
              data={bills.data.items}
              getRowKey={(bill) => bill.id}
              emptyContent={<EmptyState />}
              columns={[
                {
                  id: 'request',
                  header: t('Request'),
                  cell: (bill) => (
                    <Button
                      variant='link'
                      onClick={() => {
                        setRevisionPage(1)
                        setBillId(bill.id)
                      }}
                    >
                      {bill.request_id}
                    </Button>
                  ),
                },
                {
                  id: 'model',
                  header: t('Model'),
                  cell: (bill) => bill.model_name,
                },
                {
                  id: 'state',
                  header: t('Status'),
                  cell: (bill) => t(stateLabels[bill.state] || 'Unknown'),
                },
                {
                  id: 'reference',
                  header: t('Reference cost'),
                  cell: (bill) => formatQuota(bill.current.reference_quota),
                },
                {
                  id: 'charged',
                  header: t('Actual deduction'),
                  cell: (bill) => formatQuota(bill.current.charged),
                },
                {
                  id: 'uncollected',
                  header: t('Platform-covered excess'),
                  cell: (bill) => formatQuota(bill.current.uncollected),
                },
                {
                  id: 'date',
                  header: t('Time'),
                  cell: (bill) => date(bill.created_at),
                },
              ]}
            />
            <AccountingPager
              page={billPage}
              total={bills.data.total}
              pageSize={bills.data.page_size}
              disabled={bills.isFetching}
              onPage={setBillPage}
            />
          </>
        )}
      </TitledCard>
      <Dialog
        open={billId !== null}
        onOpenChange={(open) => {
          if (!open) setBillId(null)
        }}
        title={t('Bill details')}
      >
        {detail.isPending && <LoadingState />}
        {detail.isError && <ErrorState onRetry={() => void detail.refetch()} />}
        {detail.data && (
          <div className='space-y-4'>
            <p>
              {detail.data.request_id} · {detail.data.model_name}
            </p>
            <p>
              {t('Payment source')}:{' '}
              {detail.data.funding_source === 'subscription_windows'
                ? t('Subscription windows')
                : t('Credit packs')}
            </p>
            <p>
              {t('Original reference / deduction')}:{' '}
              {formatQuota(detail.data.original.reference_quota)} /{' '}
              {formatQuota(detail.data.original.charged)}
            </p>
            <p>
              {t('Current reference / deduction')}:{' '}
              {formatQuota(detail.data.current.reference_quota)} /{' '}
              {formatQuota(detail.data.current.charged)}
            </p>
            <p>
              {t('Platform-covered excess')}:{' '}
              {formatQuota(detail.data.current.uncollected)}
            </p>
            <p>
              {detail.data.manually_confirmed
                ? t('Usage manually confirmed')
                : t('Usage evidence')}
            </p>
            {detail.data.usage ? (
              <>
                <p>
                  {t('Evidence version')}: {detail.data.usage.version}
                </p>
                <StaticDataTable
                  data={detail.data.usage.facts}
                  getRowKey={(fact) => fact.field}
                  columns={[
                    {
                      id: 'field',
                      header: t('Quantity'),
                      cell: (fact) => fact.field,
                    },
                    {
                      id: 'value',
                      header: t('Value'),
                      cell: (fact) =>
                        fact.quantity === null
                          ? t('Unknown')
                          : new Intl.NumberFormat(
                              toIntlLocale(
                                i18n.resolvedLanguage || i18n.language
                              ),
                              { maximumFractionDigits: 20 }
                            ).format(fact.quantity),
                    },
                    {
                      id: 'unit',
                      header: t('Unit'),
                      cell: (fact) => fact.unit,
                    },
                    {
                      id: 'source',
                      header: t('Source'),
                      cell: (fact) => {
                        if (fact.source === 'upstream') {
                          return t('Upstream receipt')
                        }
                        if (fact.source === 'estimate') return t('Estimated')
                        if (fact.source === 'adaptor') {
                          return t('Adapter-derived')
                        }
                        return t('Unknown')
                      },
                    },
                    {
                      id: 'partial',
                      header: t('Evidence completeness'),
                      cell: (fact) => {
                        if (
                          fact.source === 'unknown' ||
                          fact.quantity === null
                        ) {
                          return t('Unknown')
                        }
                        return fact.partial ? t('Partial') : t('Complete')
                      },
                    },
                    {
                      id: 'algorithm',
                      header: t('Estimation method'),
                      cell: (fact) => fact.algorithm || '—',
                    },
                  ]}
                />
              </>
            ) : (
              <p className='text-muted-foreground'>
                {t('No reliable usage evidence')}
              </p>
            )}
            <StaticDataTable
              data={detail.data.revisions}
              getRowKey={(revision) => revision.id}
              columns={[
                {
                  id: 'revision',
                  header: t('Version'),
                  cell: (row) => row.revision,
                },
                {
                  id: 'reference',
                  header: t('Reference cost'),
                  cell: (row) => formatQuota(row.reference_quota),
                },
                {
                  id: 'charged',
                  header: t('Actual deduction'),
                  cell: (row) => formatQuota(row.charged),
                },
                {
                  id: 'refund',
                  header: t('Returned credits'),
                  cell: (row) => formatQuota(row.refunded),
                },
              ]}
            />
            <AccountingPager
              page={revisionPage}
              total={detail.data.total}
              pageSize={detail.data.page_size}
              disabled={detail.isFetching}
              onPage={setRevisionPage}
            />
          </div>
        )}
      </Dialog>
    </div>
  )
}
