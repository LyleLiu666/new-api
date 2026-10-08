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
import { useTranslation } from 'react-i18next'

import { formatAccountingQuota } from '../lib/format'
import type { CreditAccount } from '../types'

export function CreditBalanceSummary(props: { account: CreditAccount }) {
  const { t } = useTranslation()
  return (
    <div className='space-y-3 rounded-lg border p-4'>
      <p className='text-sm font-medium'>{t('Time-limited credits')}</p>
      <dl className='space-y-2'>
        <div>
          <dt>{t('Available for API')}</dt>
          <dd>{formatAccountingQuota(props.account.api_available)}</dd>
        </div>
        <div>
          <dt>{t('Available for subscription purchase')}</dt>
          <dd>{formatAccountingQuota(props.account.subscription_available)}</dd>
        </div>
        <div>
          <dt>{t('Reserved')}</dt>
          <dd>{formatAccountingQuota(props.account.held)}</dd>
        </div>
      </dl>
      <p className='text-muted-foreground text-xs'>
        {t('Subscription windows and rights')}
      </p>
    </div>
  )
}
