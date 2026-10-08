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
import { Crown, CalendarClock, Package } from 'lucide-react'
import { useState, useEffect } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { GroupBadge } from '@/components/group-badge'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Combobox } from '@/components/ui/combobox'
import { Separator } from '@/components/ui/separator'
import { creditQueryKeys, getCreditAccount } from '@/features/credits/api'
import { useSystemConfig } from '@/hooks/use-system-config'
import { toIntlLocale } from '@/i18n/languages'
import { formatContractCurrencyAmount } from '@/lib/currency'
import { formatQuota } from '@/lib/format'
import { handleServerError } from '@/lib/handle-server-error'
import { useAuthStore } from '@/stores/auth-store'
import { DEFAULT_CURRENCY_CONFIG } from '@/stores/system-config-store'

import {
  paySubscriptionStripe,
  paySubscriptionCreem,
  paySubscriptionEpay,
  paySubscriptionWaffoPancake,
  paySubscriptionBalance,
} from '../../api'
import { formatDuration, formatResetPeriod } from '../../lib'
import type { PlanRecord } from '../../types'

interface PaymentMethod {
  type: string
  name?: string
}

interface Props {
  open: boolean
  onOpenChange: (open: boolean) => void
  plan: PlanRecord | null
  enableStripe?: boolean
  enableCreem?: boolean
  enableWaffoPancake?: boolean
  enableOnlineTopUp?: boolean
  epayMethods?: PaymentMethod[]
  purchaseLimit?: number
  purchaseCount?: number
  userQuota?: number
  onPurchaseSuccess?: () => void | Promise<void>
}

export function SubscriptionPurchaseDialog(props: Props) {
  const { t, i18n } = useTranslation()
  const { currency } = useSystemConfig()
  const userId = useAuthStore((state) => state.auth.user?.id ?? 0)
  const account = useQuery({
    queryKey: [...creditQueryKeys.account(undefined, 1), userId],
    queryFn: () => getCreditAccount(),
    enabled: props.open && (props.plan?.version_id || 0) > 0,
    retry: false,
  })
  const [paying, setPaying] = useState(false)
  const [selectedEpayMethod, setSelectedEpayMethod] = useState('')

  useEffect(() => {
    if (props.open && props.epayMethods && props.epayMethods.length > 0) {
      setSelectedEpayMethod(props.epayMethods[0].type)
    } else if (!props.open) {
      setSelectedEpayMethod('')
    }
  }, [props.open, props.epayMethods])

  const plan = props.plan?.plan
  if (!plan) return null

  const hasStripe =
    props.enableStripe &&
    (props.plan?.payment_methods?.includes('stripe') || !!plan.stripe_price_id)
  const hasCreem =
    props.enableCreem &&
    (props.plan?.payment_methods?.includes('creem') || !!plan.creem_product_id)
  const hasWaffoPancake =
    props.enableWaffoPancake &&
    (props.plan?.payment_methods?.includes('waffo-pancake') ||
      !!plan.waffo_pancake_product_id)
  const hasEpay =
    props.enableOnlineTopUp &&
    (props.epayMethods || []).length > 0 &&
    (!props.plan?.version_id ||
      (plan.currency === 'CNY' &&
        plan.price_amount >= 0.01 &&
        Number.isInteger(Math.round(plan.price_amount * 1000000) / 10000)))
  const hasAnyPayment = hasStripe || hasCreem || hasWaffoPancake || hasEpay
  const totalAmount = Number(plan.total_amount || 0)
  const price = formatContractCurrencyAmount(
    Number(plan.price_amount || 0),
    plan.currency || 'USD',
    toIntlLocale(i18n.resolvedLanguage || i18n.language)
  )
  const quotaPerUnit =
    currency?.quotaPerUnit && currency.quotaPerUnit > 0
      ? currency.quotaPerUnit
      : DEFAULT_CURRENCY_CONFIG.quotaPerUnit
  const balanceCost = Math.max(
    0,
    Math.ceil(Number(plan.price_amount || 0) * quotaPerUnit)
  )
  const userQuota = props.plan?.version_id
    ? (account.data?.subscription_available ?? 0)
    : Math.max(0, Number(props.userQuota || 0))
  const allowBalancePay =
    plan.allow_balance_pay !== false &&
    (!props.plan?.version_id || plan.currency === 'USD')
  const insufficientBalance = userQuota < balanceCost
  const limitReached =
    (props.purchaseLimit || 0) > 0 &&
    (props.purchaseCount || 0) >= (props.purchaseLimit || 0)

  // Persist only an opaque purchase intent, scoped to the account and displayed
  // contract. An uncertain response must reuse it after closing or reloading.
  const purchaseInput = (method: string) => {
    if (!props.plan?.version_id) return { plan_id: plan.id }
    const storageKey = `subscription-intent:${userId}:${plan.id}:${props.plan.version_id}:${method}`
    let eventId = sessionStorage.getItem(storageKey)
    if (!eventId) {
      eventId = crypto.randomUUID()
      sessionStorage.setItem(storageKey, eventId)
    }
    return {
      plan_id: plan.id,
      version_id: props.plan.version_id,
      event_id: eventId,
    }
  }

  const handlePayStripe = async () => {
    setPaying(true)
    try {
      const res = await paySubscriptionStripe(purchaseInput('stripe'))
      if (res.message === 'success' && res.data?.pay_link) {
        if (props.plan?.version_id && res.data.order_id) {
          sessionStorage.setItem(
            `subscription-intent:${userId}:${plan.id}:${props.plan.version_id}:stripe:order`,
            String(res.data.order_id)
          )
        }
        window.open(res.data.pay_link, '_blank')
        toast.success(t('Payment page opened'))
        props.onOpenChange(false)
      } else {
        handleServerError(res, t('Payment request failed'))
      }
    } catch (error) {
      handleServerError(error, t('Payment request failed'))
    } finally {
      setPaying(false)
    }
  }

  const handlePayCreem = async () => {
    setPaying(true)
    try {
      const res = await paySubscriptionCreem(purchaseInput('creem'))
      if (res.message === 'success' && res.data?.checkout_url) {
        if (props.plan?.version_id && res.data.order_id) {
          sessionStorage.setItem(
            `subscription-intent:${userId}:${plan.id}:${props.plan.version_id}:creem:order`,
            String(res.data.order_id)
          )
        }
        window.open(res.data.checkout_url, '_blank')
        toast.success(t('Payment page opened'))
        props.onOpenChange(false)
      } else {
        handleServerError(res, t('Payment request failed'))
      }
    } catch (error) {
      handleServerError(error, t('Payment request failed'))
    } finally {
      setPaying(false)
    }
  }

  // In-tab redirect (not window.open) — user-gesture context is lost
  // across the await, so a popup would be blocked. Same as the wallet hook.
  const handlePayWaffoPancake = async () => {
    setPaying(true)
    try {
      const res = await paySubscriptionWaffoPancake(
        purchaseInput('waffo-pancake')
      )
      if (res.message === 'success' && res.data?.checkout_url) {
        if (props.plan?.version_id && res.data.order_id) {
          sessionStorage.setItem(
            `subscription-intent:${userId}:${plan.id}:${props.plan.version_id}:waffo-pancake:order`,
            String(res.data.order_id)
          )
        }
        toast.success(t('Redirecting to payment page...'))
        window.location.href = res.data.checkout_url
      } else {
        handleServerError(res, t('Payment request failed'))
      }
    } catch (error) {
      handleServerError(error, t('Payment request failed'))
    } finally {
      setPaying(false)
    }
  }

  const isSafari =
    typeof navigator !== 'undefined' &&
    /^((?!chrome|android).)*safari/i.test(navigator.userAgent)

  const handlePayEpay = async () => {
    if (!selectedEpayMethod) {
      toast.error(t('Please select a payment method'))
      return
    }
    setPaying(true)
    try {
      const res = await paySubscriptionEpay({
        ...purchaseInput(`epay:${selectedEpayMethod}`),
        payment_method: selectedEpayMethod,
      })
      if (res.message === 'success' && res.url) {
        if (props.plan?.version_id && res.order_id) {
          sessionStorage.setItem(
            `subscription-intent:${userId}:${plan.id}:${props.plan.version_id}:epay:${selectedEpayMethod}:order`,
            String(res.order_id)
          )
        }
        const form = document.createElement('form')
        form.action = res.url
        form.method = 'POST'
        if (!isSafari) {
          form.target = '_blank'
        }
        Object.entries(res.data || {}).forEach(([key, value]) => {
          const input = document.createElement('input')
          input.type = 'hidden'
          input.name = key
          input.value = String(value)
          form.appendChild(input)
        })
        document.body.appendChild(form)
        form.submit()
        document.body.removeChild(form)
        toast.success(t('Payment initiated'))
        props.onOpenChange(false)
      } else {
        handleServerError(res, t('Payment request failed'))
      }
    } catch (error) {
      handleServerError(error, t('Payment request failed'))
    } finally {
      setPaying(false)
    }
  }

  const handlePayBalance = async () => {
    if (!allowBalancePay) {
      toast.error(t('This plan does not allow balance redemption'))
      return
    }
    setPaying(true)
    try {
      const res = await paySubscriptionBalance(purchaseInput('balance'))
      if (res.success) {
        if (props.plan?.version_id) {
          sessionStorage.removeItem(
            `subscription-intent:${userId}:${plan.id}:${props.plan.version_id}:balance`
          )
        }
        toast.success(t('Subscription purchased successfully'))
        void props.onPurchaseSuccess?.()
        props.onOpenChange(false)
      } else {
        handleServerError(res, t('Payment request failed'))
      }
    } catch (error) {
      handleServerError(error, t('Payment request failed'))
    } finally {
      setPaying(false)
    }
  }

  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={
        <>
          <Crown className='h-5 w-5' />
          {t('Purchase Subscription')}
        </>
      }
      contentClassName='max-sm:w-[calc(100vw-1.5rem)] sm:max-w-md'
      titleClassName='flex items-center gap-2'
      contentHeight='auto'
      bodyClassName='space-y-4'
    >
      <div className='space-y-3 sm:space-y-4'>
        <div className='bg-muted/50 space-y-2.5 rounded-lg border p-3 sm:space-y-3 sm:p-4'>
          <div className='flex justify-between'>
            <span className='text-muted-foreground text-sm'>
              {t('Plan Name')}
            </span>
            <span className='max-w-[200px] truncate text-sm font-medium'>
              {plan.title}
            </span>
          </div>
          <div className='flex items-center justify-between'>
            <span className='text-muted-foreground text-sm'>
              {t('Validity Period')}
            </span>
            <span className='flex items-center gap-1 text-sm'>
              <CalendarClock className='h-3.5 w-3.5' />
              {props.plan?.version_id && plan.duration_unit === 'month'
                ? t('30 days from payment')
                : formatDuration(plan, t)}
            </span>
          </div>
          {formatResetPeriod(plan, t) !== t('No Reset') && (
            <div className='flex justify-between'>
              <span className='text-muted-foreground text-sm'>
                {t('Reset Period')}
              </span>
              <span className='text-sm'>{formatResetPeriod(plan, t)}</span>
            </div>
          )}
          <div className='flex items-center justify-between'>
            <span className='text-muted-foreground text-sm'>
              {t('Plan Quota')}
            </span>
            <span className='flex items-center gap-1 text-sm'>
              <Package className='h-3.5 w-3.5' />
              {totalAmount > 0 ? formatQuota(totalAmount) : t('Unlimited')}
            </span>
          </div>
          {plan.upgrade_group && (
            <div className='flex items-center justify-between'>
              <span className='text-muted-foreground text-sm'>
                {t('Upgrade Group')}
              </span>
              <GroupBadge group={plan.upgrade_group} />
            </div>
          )}
          <Separator />
          <div className='flex items-center justify-between'>
            <span className='text-sm font-medium'>{t('Amount Due')}</span>
            <span className='text-primary text-lg font-bold'>{price}</span>
          </div>
        </div>

        {limitReached && (
          <Alert variant='destructive'>
            <AlertDescription>
              {t('Purchase limit reached')} ({props.purchaseCount}/
              {props.purchaseLimit})
            </AlertDescription>
          </Alert>
        )}

        {props.plan?.version_id && account.isError && (
          <ErrorState onRetry={() => void account.refetch()} />
        )}
        <div className='flex flex-col gap-2 rounded-md border p-3'>
          <div className='flex items-center justify-between gap-2 text-xs'>
            <span className='text-muted-foreground'>{t('Required')}</span>
            <span>{formatQuota(balanceCost)}</span>
          </div>
          <div className='flex items-center justify-between gap-2 text-xs'>
            <span className='text-muted-foreground'>{t('Available')}</span>
            <span>{formatQuota(userQuota)}</span>
          </div>
          {!allowBalancePay ? (
            <Alert variant='destructive'>
              <AlertDescription>
                {t('This plan does not allow balance redemption')}
              </AlertDescription>
            </Alert>
          ) : (
            insufficientBalance && (
              <Alert variant='destructive'>
                <AlertDescription>{t('Insufficient balance')}</AlertDescription>
              </Alert>
            )
          )}
          <Button
            variant='outline'
            onClick={handlePayBalance}
            disabled={
              paying || limitReached || !allowBalancePay || insufficientBalance
            }
          >
            {t('Pay with Balance')}
          </Button>
        </div>

        {hasAnyPayment && (
          <div className='space-y-3'>
            <p className='text-muted-foreground text-xs'>
              {t('Select payment method')}
            </p>
            {(hasStripe || hasCreem || hasWaffoPancake) && (
              <div className='grid grid-cols-2 gap-2 sm:flex'>
                {hasStripe && (
                  <Button
                    variant='outline'
                    className='flex-1'
                    onClick={handlePayStripe}
                    disabled={paying || limitReached}
                  >
                    Stripe
                  </Button>
                )}
                {hasCreem && (
                  <Button
                    variant='outline'
                    className='flex-1'
                    onClick={handlePayCreem}
                    disabled={paying || limitReached}
                  >
                    Creem
                  </Button>
                )}
                {hasWaffoPancake && (
                  <Button
                    variant='outline'
                    className='flex-1'
                    onClick={handlePayWaffoPancake}
                    disabled={paying || limitReached}
                  >
                    Waffo Pancake
                  </Button>
                )}
              </div>
            )}
            {hasEpay && (
              <div className='grid grid-cols-[minmax(0,1fr)_auto] gap-2'>
                <Combobox
                  options={(props.epayMethods || []).map((m) => ({
                    value: m.type,
                    label: m.name || m.type,
                  }))}
                  value={selectedEpayMethod}
                  onValueChange={(v) => v !== null && setSelectedEpayMethod(v)}
                  disabled={limitReached}
                  className='flex-1'
                />
                <Button
                  onClick={handlePayEpay}
                  disabled={paying || !selectedEpayMethod || limitReached}
                >
                  {t('Pay')}
                </Button>
              </div>
            )}
          </div>
        )}
      </div>
    </Dialog>
  )
}
