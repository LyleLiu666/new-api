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
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm, type Resolver } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { z } from 'zod'

import { CopyButton } from '@/components/copy-button'
import { StaticDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import {
  ADMIN_PERMISSION_ACTIONS,
  ADMIN_PERMISSION_RESOURCES,
  hasPermission,
} from '@/lib/admin-permissions'
import { api } from '@/lib/api'
import { handleServerError } from '@/lib/handle-server-error'
import {
  getServerErrorMessage,
  requireServerSuccess,
} from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import { channelsQueryKeys } from '../../lib'

interface Account {
  id: string
  channel_id: number
  credential_version: number
  retired_at: number
}
const schema = z.object({
  account_id: z.string().min(1),
  credential_version: z.number().int().positive(),
  key: z
    .string()
    .min(1)
    .max(65535)
    .refine((value) => !/[\r\n]/.test(value)),
})
type Values = z.infer<typeof schema>
const defaults: Values = { account_id: '', credential_version: 0, key: '' }

export function ChannelAccountsDialog(props: {
  channelId: number
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const { t } = useTranslation()
  const client = useQueryClient()
  const user = useAuthStore((state) => state.auth.user)
  const canRotate = hasPermission(
    user,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.SENSITIVE_WRITE
  )
  const form = useForm<Values>({
    resolver: zodResolver(schema) as Resolver<Values>,
    defaultValues: defaults,
  })
  const accounts = useQuery({
    queryKey: ['channel-accounts', props.channelId, user?.id],
    enabled: props.open,
    queryFn: async () =>
      requireServerSuccess(
        (await api.get(`/api/channel/${props.channelId}/accounts`)).data
      ).data as Account[],
  })
  const rotation = useMutation({
    // Query's mutation cache must never retain a credential, including one
    // embedded in an Axios failure's request config. Read the form locally.
    mutationFn: async () => {
      const values = form.getValues()
      try {
        return requireServerSuccess(
          (
            await api.put(
              `/api/channel/${props.channelId}/accounts/${values.account_id}/credential`,
              { credential_version: values.credential_version, key: values.key }
            )
          ).data
        )
      } catch (error) {
        throw new Error(getServerErrorMessage(error))
      }
    },
    onError: (error) => handleServerError(error),
    meta: { errorToast: false },
    gcTime: 0,
    onSuccess: async () => {
      form.reset(defaults)
      toast.success(t('Credential updated'))
      await Promise.all([
        accounts.refetch(),
        client.invalidateQueries({ queryKey: channelsQueryKeys.all }),
      ])
    },
    retry: false,
  })
  const selected = form.watch('account_id')
  return (
    <Dialog
      open={props.open}
      onOpenChange={(open) => {
        if (!rotation.isPending) {
          form.reset(defaults)
          props.onOpenChange(open)
        }
      }}
      title={t('Upstream accounts')}
      description={t(
        'Rotating credentials preserves account identity and existing session bindings. Replacing a key through ordinary editing creates a new identity.'
      )}
    >
      {accounts.isPending && <LoadingState />}
      {accounts.isError && (
        <ErrorState onRetry={() => void accounts.refetch()} />
      )}
      {accounts.data && (
        <div className='space-y-4'>
          <StaticDataTable
            data={accounts.data}
            getRowKey={(account) => account.id}
            columns={[
              {
                id: 'id',
                header: t('Account ID'),
                cell: (account) => (
                  <div className='flex items-center gap-2'>
                    <span className='font-mono text-xs'>{account.id}</span>
                    <CopyButton value={account.id} />
                  </div>
                ),
              },
              {
                id: 'version',
                header: t('Credential version'),
                cell: (account) => account.credential_version,
              },
              {
                id: 'state',
                header: t('Status'),
                cell: (account) =>
                  account.retired_at ? t('Retired') : t('Active'),
              },
              {
                id: 'rotate',
                header: t('Actions'),
                cell: (account) =>
                  canRotate && !account.retired_at ? (
                    <Button
                      variant='outline'
                      disabled={rotation.isPending}
                      onClick={() =>
                        form.reset({
                          account_id: account.id,
                          credential_version: account.credential_version,
                          key: '',
                        })
                      }
                    >
                      {t('Rotate credential')}
                    </Button>
                  ) : null,
              },
            ]}
          />
          {selected && canRotate && (
            <Form {...form}>
              <form
                className='space-y-3'
                onSubmit={form.handleSubmit(() => rotation.mutate())}
              >
                <p className='font-mono text-xs'>
                  {selected} · {t('Credential version')}{' '}
                  {form.watch('credential_version')}
                </p>
                <FormField
                  control={form.control}
                  name='key'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('New credential')}</FormLabel>
                      <FormControl>
                        <Input
                          {...field}
                          type='password'
                          autoComplete='new-password'
                        />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
                <p className='text-muted-foreground text-sm'>
                  {t(
                    'On a version conflict, reload accounts and select the account again. Credentials are cleared when this dialog closes.'
                  )}
                </p>
                <div className='flex gap-2'>
                  <Button type='submit' disabled={rotation.isPending}>
                    {t('Confirm rotation')}
                  </Button>
                  <Button
                    type='button'
                    variant='outline'
                    disabled={rotation.isPending}
                    onClick={() => void accounts.refetch()}
                  >
                    {t('Reload accounts')}
                  </Button>
                </div>
              </form>
            </Form>
          )}
        </div>
      )}
    </Dialog>
  )
}
