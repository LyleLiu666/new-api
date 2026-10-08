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
import { Ban, Plus, RotateCcw, Trash2 } from 'lucide-react'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import {
  DataTableRowActionMenu,
  StaticDataTable,
} from '@/components/data-table'
import {
  sideDrawerContentClassName,
  sideDrawerFormClassName,
  sideDrawerHeaderClassName,
} from '@/components/drawer-layout'
import { ErrorState } from '@/components/error-state'
import { StatusBadge } from '@/components/status-badge'
import { TableId } from '@/components/table-id'
import { Button } from '@/components/ui/button'
import { Combobox } from '@/components/ui/combobox'
import {
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuShortcut,
} from '@/components/ui/dropdown-menu'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription,
} from '@/components/ui/sheet'
import { Switch } from '@/components/ui/switch'
import { getCreditAccount } from '@/features/credits/api'
import { useServerClock } from '@/hooks/use-server-clock'
import { formatQuota } from '@/lib/format'
import { handleServerError } from '@/lib/handle-server-error'

import {
  getAdminPlans,
  getUserSubscriptions,
  createUserSubscription,
  invalidateUserSubscription,
  deleteUserSubscription,
  resetUserSubscriptionsByPlan,
} from '../../api'
import { formatTimestamp } from '../../lib'
import type { PlanRecord, UserSubscriptionRecord } from '../../types'
import { SubscriptionOrders } from '../subscription-orders'

interface Props {
  open: boolean
  onOpenChange: (open: boolean) => void
  user: { id: number; username?: string } | null
  onSuccess?: () => void
}

function SubscriptionStatusBadge(props: {
  sub: UserSubscriptionRecord['subscription']
  t: (key: string) => string
  now: number
}) {
  const now = props.now
  const isExpired = (props.sub.end_time || 0) > 0 && props.sub.end_time <= now
  const isActive = props.sub.status === 'active' && !isExpired
  if (isActive) {
    return (
      <StatusBadge
        label={props.t('Active')}
        variant='success'
        copyable={false}
      />
    )
  }
  if (props.sub.status === 'cancelled') {
    return (
      <StatusBadge
        label={props.t('Invalidated')}
        variant='neutral'
        copyable={false}
      />
    )
  }
  return (
    <StatusBadge
      label={props.t('Expired')}
      variant='neutral'
      copyable={false}
    />
  )
}

export function UserSubscriptionsDialog(props: Props) {
  const { t } = useTranslation()
  const [loading, setLoading] = useState(false)
  const account = useQuery({
    queryKey: ['credits', props.user?.id, 'subscription-admin-account'],
    enabled: props.open && !!props.user?.id,
    queryFn: () => getCreditAccount(1, props.user?.id ?? 0),
  })
  const versionedAccount = account.data?.accounting_version === 1
  const serverNow = useServerClock(
    account.data?.server_time,
    account.dataUpdatedAt
  )
  const [creating, setCreating] = useState(false)
  const [plans, setPlans] = useState<PlanRecord[]>([])
  const [subs, setSubs] = useState<UserSubscriptionRecord[]>([])
  const [selectedPlanId, setSelectedPlanId] = useState<string>('')
  const [resetting, setResetting] = useState(false)
  const [advanceResetTime, setAdvanceResetTime] = useState(true)
  const [resetAction, setResetAction] = useState<{
    planId: number
    planTitle: string
  } | null>(null)
  const [cancellationReason, setCancellationReason] = useState('')
  const [cancelling, setCancelling] = useState(false)
  const [confirmAction, setConfirmAction] = useState<{
    type: 'invalidate' | 'delete'
    subId: number
  } | null>(null)

  const planTitleMap = useMemo(() => {
    const map = new Map<number, string>()
    plans.forEach((p) => {
      if (p.plan.id) map.set(p.plan.id, p.plan.title || `#${p.plan.id}`)
    })
    return map
  }, [plans])

  const loadData = useCallback(async () => {
    if (!props.user?.id) return
    setLoading(true)
    try {
      const [plansRes, subsRes] = await Promise.all([
        getAdminPlans(),
        getUserSubscriptions(props.user.id),
      ])
      if (plansRes.success) {
        setPlans(plansRes.data || [])
      } else {
        handleServerError(plansRes)
      }
      if (subsRes.success) {
        setSubs(subsRes.data || [])
      } else {
        handleServerError(subsRes)
      }
    } catch (error) {
      handleServerError(error, t('Loading failed'))
    } finally {
      setLoading(false)
    }
  }, [props.user?.id, t])

  useEffect(() => {
    if (props.open && props.user?.id) {
      setSelectedPlanId('')
      loadData()
    }
  }, [props.open, props.user?.id, loadData])

  const handleCreate = async () => {
    if (!props.user?.id || !selectedPlanId) {
      toast.error(t('Please select a subscription plan'))
      return
    }
    setCreating(true)
    try {
      const res = await createUserSubscription(props.user.id, {
        plan_id: Number(selectedPlanId),
      })
      if (res.success) {
        toast.success(res.data?.message || t('Added successfully'))
        setSelectedPlanId('')
        await loadData()
        props.onSuccess?.()
      } else {
        handleServerError(res)
      }
    } catch (error) {
      handleServerError(error, t('Request failed'))
    } finally {
      setCreating(false)
    }
  }

  const handleConfirmAction = async () => {
    if (!confirmAction) return
    setCancelling(true)
    try {
      if (confirmAction.type === 'invalidate') {
        const subscription = subs.find(
          (row) => row.subscription.id === confirmAction.subId
        )?.subscription
        const versioned = (subscription?.plan_version_id || 0) > 0
        const intentKey = `subscription-cancel:${props.user?.id}:${confirmAction.subId}`
        const eventId = sessionStorage.getItem(intentKey) || crypto.randomUUID()
        if (versioned) sessionStorage.setItem(intentKey, eventId)
        const res = await invalidateUserSubscription(
          confirmAction.subId,
          versioned
            ? { event_id: eventId, reason: cancellationReason }
            : undefined
        )
        if (res.success) {
          sessionStorage.removeItem(intentKey)
          setCancellationReason('')
          toast.success(res.data?.message || t('Has been invalidated'))
          setConfirmAction(null)
          await loadData()
          props.onSuccess?.()
        } else {
          handleServerError(res)
        }
      } else {
        const res = await deleteUserSubscription(confirmAction.subId)
        if (res.success) {
          toast.success(t('Deleted'))
          setConfirmAction(null)
          await loadData()
          props.onSuccess?.()
        } else {
          handleServerError(res)
        }
      }
    } catch (error) {
      handleServerError(error, t('Operation failed'))
    } finally {
      setCancelling(false)
    }
  }

  const handleResetConfirm = async () => {
    if (!props.user?.id || !resetAction) return
    setResetting(true)
    try {
      const res = await resetUserSubscriptionsByPlan(props.user.id, {
        plan_id: resetAction.planId,
        advance_reset_time: advanceResetTime,
      })
      if (res.success) {
        toast.success(
          t('Reset {{count}} active subscriptions', {
            count: res.data?.reset_count || 0,
          })
        )
        await loadData()
        props.onSuccess?.()
      } else {
        handleServerError(res)
      }
    } catch (error) {
      handleServerError(error, t('Operation failed'))
    } finally {
      setResetting(false)
      setResetAction(null)
    }
  }

  return (
    <>
      <Sheet open={props.open} onOpenChange={props.onOpenChange}>
        <SheetContent className={sideDrawerContentClassName('sm:max-w-2xl')}>
          <SheetHeader className={sideDrawerHeaderClassName()}>
            <SheetTitle>{t('User Subscription Management')}</SheetTitle>
            <SheetDescription>
              {props.user?.username || '-'} (ID: {props.user?.id || '-'})
            </SheetDescription>
          </SheetHeader>

          <div className={sideDrawerFormClassName()}>
            {versionedAccount && props.user && (
              <SubscriptionOrders userId={props.user.id} />
            )}
            {versionedAccount && (
              <p className='text-muted-foreground text-sm'>
                {t(
                  'Versioned subscriptions are purchased from published plans. Cancelling rights does not automatically refund money.'
                )}
              </p>
            )}
            <div className='flex gap-2'>
              <Combobox
                options={plans.map((p) => ({
                  value: String(p.plan.id),
                  label: `${p.plan.title} ($${Number(p.plan.price_amount || 0).toFixed(2)})`,
                }))}
                value={selectedPlanId}
                onValueChange={(v) => v !== null && setSelectedPlanId(v)}
                className='flex-1'
                placeholder={t('Select subscription plan')}
              />
              <Button
                onClick={handleCreate}
                disabled={
                  creating ||
                  !selectedPlanId ||
                  !account.data ||
                  versionedAccount
                }
              >
                <Plus className='mr-1 h-4 w-4' />
                {t('Add subscription')}
              </Button>
            </div>

            {account.isError && (
              <ErrorState onRetry={() => void account.refetch()} />
            )}
            <StaticDataTable
              data={loading ? [] : subs}
              getRowKey={(record) => record.subscription.id}
              emptyClassName={loading ? 'py-8' : 'text-muted-foreground py-8'}
              emptyContent={
                loading ? t('Loading...') : t('No subscription records')
              }
              columns={[
                {
                  id: 'id',
                  header: t('ID'),
                  cell: (record) => <TableId value={record.subscription.id} />,
                },
                {
                  id: 'plan',
                  header: t('Plan'),
                  cell: (record) => {
                    const sub = record.subscription

                    return (
                      <div>
                        <div className='font-medium'>
                          {planTitleMap.get(sub.plan_id) || `#${sub.plan_id}`}
                        </div>
                        <div className='text-muted-foreground text-sm'>
                          {t('Source')}: {sub.source || '-'}
                        </div>
                      </div>
                    )
                  },
                },
                {
                  id: 'status',
                  header: t('Status'),
                  cell: (record) => (
                    <SubscriptionStatusBadge
                      sub={record.subscription}
                      t={t}
                      now={serverNow}
                    />
                  ),
                },
                {
                  id: 'validity',
                  header: t('Validity'),
                  cell: (record) => {
                    const sub = record.subscription

                    return (
                      <div className='text-sm'>
                        <div>
                          {t('Start')}: {formatTimestamp(sub.start_time)}
                        </div>
                        <div>
                          {t('End')}: {formatTimestamp(sub.end_time)}
                        </div>
                      </div>
                    )
                  },
                },
                {
                  id: 'quota',
                  header: t('Total Quota'),
                  cell: (record) => {
                    const sub = record.subscription
                    const total = Number(sub.amount_total || 0)
                    const used = Number(sub.amount_used || 0)
                    return total > 0
                      ? `${formatQuota(used)}/${formatQuota(total)}`
                      : t('Unlimited')
                  },
                },
                {
                  id: 'actions',
                  header: t('Actions'),
                  className: 'text-right',
                  cellClassName: 'text-right',
                  cell: (record) => {
                    const sub = record.subscription
                    const now = serverNow
                    const isExpired =
                      (sub.end_time || 0) > 0 && sub.end_time <= now
                    const isActive = sub.status === 'active' && !isExpired

                    return (
                      <DataTableRowActionMenu ariaLabel={t('Actions')}>
                        <DropdownMenuItem
                          disabled={!isActive || (sub.plan_version_id || 0) > 0}
                          onClick={() => {
                            setAdvanceResetTime(true)
                            setResetAction({
                              planId: sub.plan_id,
                              planTitle:
                                planTitleMap.get(sub.plan_id) ||
                                `#${sub.plan_id}`,
                            })
                          }}
                        >
                          {t('Reset quota')}
                          <DropdownMenuShortcut>
                            <RotateCcw size={16} />
                          </DropdownMenuShortcut>
                        </DropdownMenuItem>
                        <DropdownMenuItem
                          disabled={!isActive}
                          onClick={() =>
                            setConfirmAction({
                              type: 'invalidate',
                              subId: sub.id,
                            })
                          }
                        >
                          {t('Invalidate')}
                          <DropdownMenuShortcut>
                            <Ban size={16} />
                          </DropdownMenuShortcut>
                        </DropdownMenuItem>
                        <DropdownMenuSeparator />
                        <DropdownMenuItem
                          variant='destructive'
                          disabled={(sub.plan_version_id || 0) > 0}
                          onClick={() =>
                            setConfirmAction({
                              type: 'delete',
                              subId: sub.id,
                            })
                          }
                        >
                          {t('Delete')}
                          <DropdownMenuShortcut>
                            <Trash2 size={16} />
                          </DropdownMenuShortcut>
                        </DropdownMenuItem>
                      </DataTableRowActionMenu>
                    )
                  },
                },
              ]}
            />
          </div>
        </SheetContent>
      </Sheet>

      {confirmAction && (
        <ConfirmDialog
          open
          onOpenChange={(v) => !v && setConfirmAction(null)}
          title={
            confirmAction.type === 'invalidate'
              ? t('Confirm invalidate')
              : t('Confirm delete')
          }
          desc={
            confirmAction.type === 'invalidate'
              ? t(
                  'After invalidating, this subscription will be immediately deactivated. Historical records are not affected. Continue?'
                )
              : t(
                  'Deleting will permanently remove this subscription record (including benefit details). Continue?'
                )
          }
          handleConfirm={handleConfirmAction}
          destructive={confirmAction.type === 'delete'}
          disabled={
            cancelling ||
            (confirmAction.type === 'invalidate' &&
              (subs.find((row) => row.subscription.id === confirmAction.subId)
                ?.subscription.plan_version_id || 0) > 0 &&
              !cancellationReason.trim())
          }
          isLoading={cancelling}
        >
          {confirmAction.type === 'invalidate' && (
            <div className='space-y-2'>
              <Label htmlFor='cancellation-reason'>
                {t('Cancellation reason')}
              </Label>
              <Input
                id='cancellation-reason'
                value={cancellationReason}
                onChange={(event) => setCancellationReason(event.target.value)}
              />
              <p className='text-muted-foreground text-sm'>
                {t(
                  'This cancels rights only. Cash refunds are handled outside this system.'
                )}
              </p>
            </div>
          )}
        </ConfirmDialog>
      )}

      {resetAction && (
        <ConfirmDialog
          open
          onOpenChange={(v) => !v && setResetAction(null)}
          title={t('Reset subscription quota')}
          desc={t('Reset active {{plan}} subscriptions for this user?', {
            plan: resetAction.planTitle,
          })}
          confirmText={t('Reset quota')}
          handleConfirm={handleResetConfirm}
          isLoading={resetting}
        >
          <label className='flex items-center justify-between gap-3 rounded-md border px-3 py-2 text-sm'>
            <span>{t('Advance next reset time')}</span>
            <Switch
              checked={advanceResetTime}
              onCheckedChange={(checked) => setAdvanceResetTime(!!checked)}
              aria-label={t('Advance next reset time')}
            />
          </label>
        </ConfirmDialog>
      )}
    </>
  )
}
