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
import { useFieldArray, type UseFormReturn } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { SideDrawerSection } from '@/components/drawer-layout'
import { Button } from '@/components/ui/button'
import {
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { getCurrencyDisplay, getCurrencyLabel } from '@/lib/currency'

import type { PlanFormValues } from '../lib/plan-form'

export function PlanAccountingFields(props: {
  form: UseFormReturn<PlanFormValues>
}) {
  const { t } = useTranslation()
  const windows = useFieldArray({
    control: props.form.control,
    name: 'window_rules',
  })
  const tags = useFieldArray({
    control: props.form.control,
    name: 'entitlement_tags',
  })
  return (
    <SideDrawerSection>
      <h3 className='text-sm font-medium'>
        {t('Subscription windows and tags')}
      </h3>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Published monthly plans last 30 days from payment. Short windows start on first use and are shared by all API keys.'
        )}
      </p>
      {windows.fields.map((row, index) => (
        <div key={row.id} className='space-y-2 rounded-md border p-3'>
          <FormField
            control={props.form.control}
            name={`window_rules.${index}.id`}
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Window ID')}</FormLabel>
                <FormControl>
                  <Input {...field} />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
          <FormField
            control={props.form.control}
            name={`window_rules.${index}.duration_seconds`}
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Window duration in seconds')}</FormLabel>
                <FormControl>
                  <Input {...field} type='number' min={1} max={2592000} />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
          <FormField
            control={props.form.control}
            name={`window_rules.${index}.limit`}
            render={({ field }) => (
              <FormItem>
                <FormLabel>
                  {t('Window quota ({{unit}})', { unit: getCurrencyLabel() })}
                </FormLabel>
                <FormControl>
                  <Input
                    {...field}
                    type='number'
                    min={0}
                    step={
                      getCurrencyDisplay().meta.kind === 'tokens' ? 1 : 0.000001
                    }
                  />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
          <Button
            type='button'
            variant='outline'
            onClick={() => windows.remove(index)}
          >
            {t('Remove window')}
          </Button>
        </div>
      ))}
      <FormMessage>
        {props.form.formState.errors.window_rules?.root?.message}
      </FormMessage>
      <Button
        type='button'
        variant='outline'
        disabled={windows.fields.length >= 8}
        onClick={() =>
          windows.append({ id: '', duration_seconds: 18000, limit: 1 })
        }
      >
        {t('Add window')}
      </Button>
      {tags.fields.map((row, index) => (
        <div key={row.id} className='space-y-2 rounded-md border p-3'>
          <FormField
            control={props.form.control}
            name={`entitlement_tags.${index}.key`}
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Tag key')}</FormLabel>
                <FormControl>
                  <Input {...field} />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
          <FormField
            control={props.form.control}
            name={`entitlement_tags.${index}.value`}
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Tag value')}</FormLabel>
                <FormControl>
                  <Input {...field} />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
          <Button
            type='button'
            variant='outline'
            onClick={() => tags.remove(index)}
          >
            {t('Remove tag')}
          </Button>
        </div>
      ))}
      <FormMessage>
        {props.form.formState.errors.entitlement_tags?.root?.message}
      </FormMessage>
      <Button
        type='button'
        variant='outline'
        disabled={tags.fields.length >= 32}
        onClick={() => tags.append({ key: '', value: '' })}
      >
        {t('Add tag')}
      </Button>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Saving changes the draft. Publish a version to offer it to new buyers. Purchased rights keep their original version.'
        )}
      </p>
    </SideDrawerSection>
  )
}
